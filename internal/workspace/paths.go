package workspace

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// workspaceRoot resolves all existing path components before workspace paths
// are constructed. For a root that does not exist yet, it resolves the
// deepest existing ancestor and appends the missing components. This prevents
// a lexical path check from overlooking a pre-existing symlink.
func (l *Local) workspaceRoot() (string, error) {
	root := l.settings().Workspace.Root
	if strings.TrimSpace(root) == "" {
		return "", errors.New("workspace root is empty")
	}
	return resolveExistingAncestors(root)
}

func (l *Local) ensureWorkspaceRoot() (string, error) {
	root, err := l.workspaceRoot()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", fmt.Errorf("create workspace root: %w", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		return "", fmt.Errorf("inspect workspace root: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("workspace root is not a directory: %s", root)
	}
	return resolveExistingAncestors(root)
}

func (l *Local) managedWorkspacePath(path string) (string, error) {
	root, err := l.workspaceRoot()
	if err != nil {
		return "", err
	}
	path, err = filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", fmt.Errorf("resolve workspace path: %w", err)
	}
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("workspace path must not be a symlink: %s", path)
	} else if err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("inspect workspace path: %w", err)
	}
	resolved, err := resolveExistingAncestors(path)
	if err != nil {
		return "", fmt.Errorf("resolve workspace path: %w", err)
	}
	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == "." || filepath.Dir(rel) != "." || rel == stateDirectory {
		return "", fmt.Errorf("workspace path is not a direct workspace below the configured root")
	}
	if err := regularManagedPath(root, resolved, "workspace"); err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("inspect workspace path: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("workspace path is not a directory: %s", resolved)
	}
	if !below(root, resolved) {
		return "", fmt.Errorf("workspace path escapes root")
	}
	return resolved, nil
}

func workspacePath(root, key string) (string, error) {
	path := filepath.Join(root, key)
	if err := regularManagedPath(root, path, "workspace"); err != nil {
		return "", err
	}
	return path, nil
}

func statePath(root string) (string, error) {
	path := filepath.Join(root, stateDirectory)
	if err := regularManagedPath(root, path, "workspace state directory"); err != nil {
		return "", err
	}
	return path, nil
}

// regularManagedPath rejects a symlink at the service-owned path itself, then
// resolves existing ancestors to prove the target remains below the canonical
// workspace root. Service-owned workspace, state-directory, and marker paths
// do not need symlinks, so rejecting them also avoids surprising rename/remove
// semantics.
func regularManagedPath(root, path, kind string) error {
	if !below(root, path) {
		return fmt.Errorf("%s path escapes workspace root", kind)
	}
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s path must not be a symlink: %s", kind, path)
	} else if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("inspect %s path: %w", kind, err)
	}
	resolved, err := resolveExistingAncestors(path)
	if err != nil {
		return fmt.Errorf("resolve %s path: %w", kind, err)
	}
	if !below(root, resolved) {
		return fmt.Errorf("%s path escapes workspace root through a symlink", kind)
	}
	return nil
}

// resolveExistingAncestors returns an absolute path whose existing ancestors
// have been resolved with EvalSymlinks. It deliberately supports a final path
// that does not yet exist, which is required before creating a workspace or
// state directory.
func resolveExistingAncestors(path string) (string, error) {
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	missing := make([]string, 0)
	for {
		_, err := os.Lstat(abs)
		if err == nil {
			resolved, err := filepath.EvalSymlinks(abs)
			if err != nil {
				return "", err
			}
			info, err := os.Stat(resolved)
			if err != nil {
				return "", err
			}
			if !info.IsDir() && len(missing) > 0 {
				return "", fmt.Errorf("existing ancestor is not a directory: %s", abs)
			}
			return filepath.Join(append([]string{resolved}, missing...)...), nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return "", fmt.Errorf("no existing ancestor for %s", path)
		}
		missing = append([]string{filepath.Base(abs)}, missing...)
		abs = parent
	}
}

func below(root, path string) bool {
	r, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	p, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(r, p)
	return err == nil && rel != "." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != ".."
}

func canonicalExistingDirectory(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(strings.TrimSpace(path))
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(resolved)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("not a directory: %s", abs)
	}
	return filepath.Clean(abs), nil
}
