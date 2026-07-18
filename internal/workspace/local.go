// Package workspace implements local, bounded workspace lifecycle operations.
package workspace

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/domain"
)

const maxHookOutput = 16 << 10

var unsafe = regexp.MustCompile(`[^A-Za-z0-9._-]`)

type Local struct{ settings func() config.Settings }

func New(settings func() config.Settings) *Local { return &Local{settings: settings} }
func Key(identifier string) string {
	clean := unsafe.ReplaceAllString(identifier, "_")
	if clean == identifier && clean != "" && clean != "." && clean != ".." {
		return clean
	}
	if clean == "" || clean == "." || clean == ".." {
		clean = "issue"
	}
	h := sha256.Sum256([]byte(identifier))
	return fmt.Sprintf("%s--%x", clean, h[:8])
}
func (l *Local) Prepare(ctx context.Context, issue domain.Issue) (domain.Workspace, error) {
	if err := ctx.Err(); err != nil {
		return domain.Workspace{}, err
	}
	root := l.settings().Workspace.Root
	key := Key(issue.Identifier)
	path := filepath.Join(root, key)
	if !below(root, path) {
		return domain.Workspace{}, fmt.Errorf("workspace path escapes root")
	}
	info, err := os.Stat(path)
	created := os.IsNotExist(err)
	if err == nil && !info.IsDir() {
		return domain.Workspace{}, fmt.Errorf("workspace exists but is not a directory: %s", path)
	}
	if err != nil && !os.IsNotExist(err) {
		return domain.Workspace{}, err
	}
	settings := l.settings()
	if created {
		if settings.Workspace.SourceRoot != "" {
			if err := os.MkdirAll(root, 0o755); err != nil {
				return domain.Workspace{}, err
			}
			if err := addWorktree(ctx, settings.Workspace.SourceRoot, path); err != nil {
				return domain.Workspace{}, err
			}
		} else if err := os.MkdirAll(path, 0o755); err != nil {
			return domain.Workspace{}, err
		}
	}
	ws := domain.Workspace{Path: path, Key: key, CreatedNow: created}
	if created && settings.Hooks.AfterCreate != "" {
		if err := l.hook(ctx, ws, issue, "after_create", settings.Hooks.AfterCreate); err != nil {
			return domain.Workspace{}, err
		}
	}
	return ws, nil
}
func (l *Local) BeforeRun(ctx context.Context, ws domain.Workspace, issue domain.Issue) error {
	return l.hook(ctx, ws, issue, "before_run", l.settings().Hooks.BeforeRun)
}
func (l *Local) AfterRun(ctx context.Context, ws domain.Workspace, issue domain.Issue) {
	if err := l.hook(ctx, ws, issue, "after_run", l.settings().Hooks.AfterRun); err != nil {
		fmt.Fprintf(os.Stderr, "symphony after_run hook error issue=%s: %v\n", issue.Identifier, err)
	}
}
func (l *Local) Cleanup(ctx context.Context, issue domain.Issue) error {
	settings := l.settings()
	root := settings.Workspace.Root
	path := filepath.Join(root, Key(issue.Identifier))
	if !below(root, path) {
		return fmt.Errorf("workspace cleanup path escapes root")
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	}
	ws := domain.Workspace{Path: path, Key: Key(issue.Identifier)}
	if err := l.hook(ctx, ws, issue, "before_remove", l.settings().Hooks.BeforeRemove); err != nil {
		fmt.Fprintf(os.Stderr, "symphony before_remove hook error issue=%s: %v\n", issue.Identifier, err)
	}
	if settings.Workspace.SourceRoot != "" {
		if err := removeWorktree(ctx, settings.Workspace.SourceRoot, path); err != nil {
			return err
		}
		return nil
	}
	return os.RemoveAll(path)
}
func (l *Local) Execute(ctx context.Context, ws domain.Workspace, command string, args []string) ([]byte, error) {
	if !below(l.settings().Workspace.Root, ws.Path) {
		return nil, fmt.Errorf("workspace execution path escapes root")
	}
	c := exec.CommandContext(ctx, command, args...)
	c.Dir = ws.Path
	return c.CombinedOutput()
}
func (l *Local) hook(ctx context.Context, ws domain.Workspace, issue domain.Issue, name, script string) error {
	if script == "" {
		return nil
	}
	if !below(l.settings().Workspace.Root, ws.Path) {
		return fmt.Errorf("hook workspace path escapes root")
	}
	timeout := l.settings().Hooks.Timeout
	if timeout <= 0 {
		timeout = time.Minute
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "sh", "-lc", script)
	cmd.Dir = ws.Path
	cmd.Env = append(os.Environ(), "SYMPHONY_ISSUE_ID="+issue.ID, "SYMPHONY_ISSUE_IDENTIFIER="+issue.Identifier)
	out, err := cmd.CombinedOutput()
	if len(out) > maxHookOutput {
		out = append(out[:maxHookOutput], []byte("...[truncated]")...)
	}
	if err != nil {
		return fmt.Errorf("%s hook failed: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	return nil
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

func addWorktree(ctx context.Context, sourceRoot, path string) error {
	cmd := exec.CommandContext(ctx, "git", "-C", sourceRoot, "worktree", "add", "--detach", path, "HEAD")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("create workspace worktree: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func removeWorktree(ctx context.Context, sourceRoot, path string) error {
	cmd := exec.CommandContext(ctx, "git", "-C", sourceRoot, "worktree", "remove", "--force", path)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("remove workspace worktree: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
