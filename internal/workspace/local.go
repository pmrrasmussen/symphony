// Package workspace implements local, bounded workspace lifecycle operations.
package workspace

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/domain"
)

const maxHookOutput = 16 << 10

const stateDirectory = ".symphony-state"

const (
	legacyWorkspaceStateSchema = "symphony.workspace-state/v1"
	workspaceStateSchema       = "symphony.workspace-state/v2"
)

const (
	preparationCreating    = "creating"
	preparationHookPending = "hook_pending"
	preparationReady       = "ready"
)

var unsafe = regexp.MustCompile(`[^A-Za-z0-9._-]`)

type Local struct{ settings func() config.Settings }

// workspaceState is deliberately stored below workspace.root rather than in a
// process-local directory. It is the durable handoff record used after a
// service restart and includes the initial detached-worktree commit so cleanup
// can refuse to delete work that was changed or committed locally.
type workspaceState struct {
	Schema          string `json:"schema,omitempty"`
	IssueID         string `json:"issue_id"`
	Identifier      string `json:"identifier"`
	BaseCommit      string `json:"base_commit,omitempty"`
	Preparation     string `json:"preparation,omitempty"`
	SourceRoot      string `json:"source_root,omitempty"`
	GitCommonDir    string `json:"git_common_dir,omitempty"`
	GitWorktreeDir  string `json:"git_worktree_dir,omitempty"`
	GitCommonDevice uint64 `json:"git_common_device,omitempty"`
	GitCommonInode  uint64 `json:"git_common_inode,omitempty"`
	// CompletedUpdatedAt is decoded only for compatibility with state written by
	// older releases. Completion timestamps no longer affect dispatch and are
	// removed whenever Symphony next writes this state.
	CompletedUpdatedAt *time.Time `json:"completed_updated_at,omitempty"`
}

func New(settings func() config.Settings) *Local { return &Local{settings: settings} }
func Key(identifier string) string {
	clean := unsafe.ReplaceAllString(identifier, "_")
	if clean == identifier && clean != "" && clean != "." && clean != ".." && clean != stateDirectory {
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
	root, err := l.ensureWorkspaceRoot()
	if err != nil {
		return domain.Workspace{}, err
	}
	key := Key(issue.Identifier)
	path, err := workspacePath(root, key)
	if err != nil {
		return domain.Workspace{}, err
	}
	state, found, err := l.loadState(issue)
	if err != nil {
		return domain.Workspace{}, err
	}
	if found {
		if err := validateStateOwner(state, issue); err != nil {
			return domain.Workspace{}, err
		}
		if state.Preparation != "" && state.Preparation != preparationReady {
			if err := l.recoverPreparation(ctx, issue, path, state); err != nil {
				return domain.Workspace{}, err
			}
			state, found = workspaceState{}, false
		}
	}
	info, err := os.Stat(path)
	created := os.IsNotExist(err)
	if err == nil && !info.IsDir() {
		return domain.Workspace{}, fmt.Errorf("workspace exists but is not a directory: %s", path)
	}
	if err != nil && !os.IsNotExist(err) {
		return domain.Workspace{}, err
	}
	if !found && !created {
		return domain.Workspace{}, fmt.Errorf("workspace state is missing for existing workspace %q; manual recovery is required", path)
	}
	settings := l.settings()
	if created {
		if settings.Workspace.SourceRoot != "" {
			identity, err := sourceIdentity(ctx, settings.Workspace.SourceRoot)
			if err != nil {
				return domain.Workspace{}, err
			}
			state = workspaceState{Schema: workspaceStateSchema, IssueID: issue.ID, Identifier: issue.Identifier, Preparation: preparationCreating, SourceRoot: identity.sourceRoot, GitCommonDir: identity.commonDir, GitCommonDevice: identity.commonDevice, GitCommonInode: identity.commonInode}
			if err := l.writeState(issue, state); err != nil {
				return domain.Workspace{}, err
			}
			if err := addWorktree(ctx, identity.sourceRoot, path); err != nil {
				return domain.Workspace{}, err
			}
			worktreeDir, err := worktreeIdentity(ctx, path, identity.commonDir)
			if err != nil {
				return domain.Workspace{}, err
			}
			state.GitWorktreeDir = worktreeDir
			state.BaseCommit, err = gitHead(ctx, path)
			if err != nil {
				return domain.Workspace{}, err
			}
		} else {
			state = workspaceState{Schema: workspaceStateSchema, IssueID: issue.ID, Identifier: issue.Identifier, Preparation: preparationCreating}
			if err := l.writeState(issue, state); err != nil {
				return domain.Workspace{}, err
			}
			if err := os.MkdirAll(path, 0o755); err != nil {
				return domain.Workspace{}, err
			}
		}
		state.Preparation = preparationHookPending
		if err := l.writeState(issue, state); err != nil {
			return domain.Workspace{}, err
		}
	}
	// Re-check after creating the directory or invoking Git. A trusted local
	// process can still race this check with a rename or symlink replacement;
	// see docs/architecture.md for that unavoidable filesystem limitation.
	if path, err = workspacePath(root, key); err != nil {
		return domain.Workspace{}, err
	}
	ws := domain.Workspace{Path: path, Key: key, CreatedNow: created}
	if created && settings.Hooks.AfterCreate != "" {
		if err := l.hook(ctx, ws, issue, "after_create", settings.Hooks.AfterCreate); err != nil {
			recoveryErr := l.recoverPreparation(ctx, issue, path, state)
			if recoveryErr != nil {
				return domain.Workspace{}, fmt.Errorf("%w; partial workspace recovery also failed: %v; retry cleanup or preserve the workspace outside the managed root for manual recovery", err, recoveryErr)
			}
			return domain.Workspace{}, err
		}
	}
	if created {
		if state.SourceRoot == "" {
			git, gitErr := isGitWorkspace(path)
			if gitErr != nil {
				return domain.Workspace{}, gitErr
			}
			if git {
				recoveryErr := l.recoverPreparation(ctx, issue, path, state)
				if recoveryErr != nil {
					return domain.Workspace{}, fmt.Errorf("after_create created an unclassifiable plain Git repository; recovery failed: %v; manual recovery is required", recoveryErr)
				}
				return domain.Workspace{}, errors.New("after_create created a Git repository without a source-worktree identity; the partial workspace was removed and must be configured with workspace.source_root")
			}
		}
		state.Preparation = preparationReady
		if err := l.writeState(issue, state); err != nil {
			return domain.Workspace{}, err
		}
	}
	if err := l.ensureState(ctx, ws, issue); err != nil {
		return domain.Workspace{}, err
	}
	if err := l.setGitMetadataRoot(ctx, &ws, issue); err != nil {
		return domain.Workspace{}, err
	}
	return ws, nil
}

// setGitMetadataRoot grants the agent only the Git boundary a detached-HEAD
// commit in a linked worktree needs: the source repository's shared object
// store and this worktree's own per-worktree metadata directory. Git stores a
// linked worktree's HEAD, index, and reflog under the per-worktree directory and
// its objects in the shared store, so those two roots are sufficient to add and
// commit. It deliberately excludes the rest of the source common directory --
// refs/heads (including the primary branch), the primary working tree's index,
// packed-refs, and other worktrees' directories -- so a misbehaving agent cannot
// write the source repository's branches or primary index (PMR-65). It also
// records an integrity baseline so AfterRun can detect any drift that slips past
// this narrowed grant.
func (l *Local) setGitMetadataRoot(ctx context.Context, ws *domain.Workspace, issue domain.Issue) error {
	state, found, err := l.loadState(issue)
	if err != nil {
		return err
	}
	if !found || state.SourceRoot == "" {
		return nil
	}
	if err := validateStateOwner(state, issue); err != nil {
		return err
	}
	if err := validateWorktreeIdentity(ws.Path, state); err != nil {
		return fmt.Errorf("validate Git workspace for local commits: %w", err)
	}
	available, err := gitRepositoryAvailable(ctx, state)
	if err != nil {
		return fmt.Errorf("validate Git workspace repository: %w", err)
	}
	if !available {
		return errors.New("recorded Git workspace repository is unavailable; cannot grant local commit access")
	}
	objects, err := canonicalExistingDirectory(filepath.Join(state.GitCommonDir, "objects"))
	if err != nil {
		return fmt.Errorf("resolve Git object store for local commits: %w", err)
	}
	ws.GitMetadataRoots = []string{objects, filepath.Clean(state.GitWorktreeDir)}
	// The integrity baseline is a best-effort backstop: an unexpected failure to
	// fingerprint the source must not fail an otherwise valid workspace. An empty
	// baseline simply skips the post-run assertion.
	if baseline, err := sourceIntegrityDigest(ctx, state.SourceRoot, state.GitCommonDir); err == nil {
		ws.GitIntegrityBaseline = baseline
	}
	return nil
}
func (l *Local) BeforeRun(ctx context.Context, ws domain.Workspace, issue domain.Issue) error {
	return l.hook(ctx, ws, issue, "before_run", l.settings().Hooks.BeforeRun)
}
func (l *Local) AfterRun(ctx context.Context, ws domain.Workspace, issue domain.Issue) {
	if err := l.hook(ctx, ws, issue, "after_run", l.settings().Hooks.AfterRun); err != nil {
		fmt.Fprintf(os.Stderr, "symphony after_run hook error issue=%s: %v\n", issue.Identifier, err)
	}
	l.assertSourceIntegrity(ctx, ws, issue)
}

// assertSourceIntegrity is the PMR-65 defense-in-depth backstop. Even though the
// narrowed sandbox grant should make it impossible, it re-checks that the run
// left the source repository's branches (other than the symphony/* publish
// branches Symphony itself creates) and primary working tree index exactly as
// they were when the workspace was prepared, and alerts if not. It deliberately
// only detects and alerts; it never rewrites the operator's refs or index,
// because it cannot distinguish an agent breach from a legitimate concurrent
// operator change and a destructive "repair" could lose real work.
func (l *Local) assertSourceIntegrity(ctx context.Context, ws domain.Workspace, issue domain.Issue) {
	if ws.GitIntegrityBaseline == "" {
		return
	}
	state, found, err := l.loadState(issue)
	if err != nil || !found || state.SourceRoot == "" {
		return
	}
	current, err := sourceIntegrityDigest(ctx, state.SourceRoot, state.GitCommonDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "symphony source integrity check error issue=%s: %v\n", issue.Identifier, err)
		return
	}
	if current != ws.GitIntegrityBaseline {
		fmt.Fprintf(os.Stderr, "symphony source integrity alert issue=%s: source branches or primary index changed during the run; an isolated worktree must never modify them; inspect %s\n", issue.Identifier, state.SourceRoot)
	}
}

// sourceIntegrityDigest fingerprints the source repository state an isolated
// detached worktree must never modify: every branch head except the symphony/*
// publish branches Symphony itself creates, plus the primary working tree's
// index. It ignores unrelated churn (object packing, other worktrees) so a
// post-run comparison flags exactly a source branch or primary-index write.
func sourceIntegrityDigest(ctx context.Context, sourceRoot, commonDir string) (string, error) {
	refs, err := gitMetadataAllowEmpty(ctx, sourceRoot, "for-each-ref", "--format=%(refname) %(objectname)", "refs/heads/")
	if err != nil {
		return "", fmt.Errorf("read source branch heads: %w", err)
	}
	kept := make([]string, 0)
	for _, line := range strings.Split(refs, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "refs/heads/symphony/") {
			continue
		}
		kept = append(kept, line)
	}
	sort.Strings(kept)
	digest := sha256.New()
	for _, line := range kept {
		digest.Write([]byte(line))
		digest.Write([]byte{0})
	}
	index, err := os.ReadFile(filepath.Join(commonDir, "index"))
	if err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("read primary index: %w", err)
	}
	indexSum := sha256.Sum256(index)
	digest.Write(indexSum[:])
	return fmt.Sprintf("%x", digest.Sum(nil)), nil
}

func (l *Local) Cleanup(ctx context.Context, issue domain.Issue) error {
	path, err := l.workspacePath(issue)
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return l.removeState(issue)
	} else if err != nil {
		return err
	}
	state, found, err := l.loadState(issue)
	if err != nil {
		return err
	}
	if !found {
		return errors.New("refusing to remove workspace without durable ownership state; preserve it outside the managed root for manual recovery")
	}
	if err := validateStateOwner(state, issue); err != nil {
		return err
	}
	ws := domain.Workspace{Path: path, Key: Key(issue.Identifier)}
	if err := l.hook(ctx, ws, issue, "before_remove", l.settings().Hooks.BeforeRemove); err != nil {
		fmt.Fprintf(os.Stderr, "symphony before_remove hook error issue=%s: %v\n", issue.Identifier, err)
	}
	git, err := isGitWorkspace(path)
	if err != nil {
		return err
	}
	if git {
		if state.BaseCommit == "" {
			return errors.New("refusing to remove Git workspace without a recorded base commit")
		}
		if state.SourceRoot != "" {
			if err := validateWorktreeIdentity(path, state); err != nil {
				return err
			}
			available, availableErr := gitRepositoryAvailable(ctx, state)
			if availableErr != nil {
				return availableErr
			}
			if available {
				if err := ensureGitWorkspaceUnchanged(ctx, path, state.BaseCommit); err != nil {
					return err
				}
				if err := removeRecordedWorktree(ctx, state, path, false); err != nil {
					return err
				}
				if err := pruneRecordedWorktrees(ctx, state); err != nil {
					return err
				}
			} else {
				return errors.New("recorded source and Git common directory are unavailable; refusing to remove a worktree whose local changes cannot be verified; preserve it outside the managed root for manual recovery")
			}
		} else {
			return errors.New("refusing to remove legacy Git workspace without recorded source-worktree identity; preserve it outside the managed root for manual recovery")
		}
	} else {
		if state.SourceRoot != "" || state.GitCommonDir != "" || state.GitWorktreeDir != "" || state.BaseCommit != "" {
			return errors.New("recorded Git workspace no longer has its worktree identity; refusing cleanup because local changes cannot be verified; manual recovery is required")
		}
		if err := os.RemoveAll(path); err != nil {
			return err
		}
	}
	// Do not discard the completion record until the workspace was removed.
	return l.removeState(issue)
}
func (l *Local) Execute(ctx context.Context, ws domain.Workspace, command string, args []string) ([]byte, error) {
	path, err := l.managedWorkspacePath(ws.Path)
	if err != nil {
		return nil, err
	}
	c := exec.CommandContext(ctx, command, args...)
	c.Dir = path
	return c.CombinedOutput()
}

func (l *Local) workspacePath(issue domain.Issue) (string, error) {
	root, err := l.workspaceRoot()
	if err != nil {
		return "", err
	}
	return workspacePath(root, Key(issue.Identifier))
}

func (l *Local) statePath(issue domain.Issue) (string, error) {
	root, err := l.workspaceRoot()
	if err != nil {
		return "", err
	}
	dir, err := statePath(root)
	if err != nil {
		return "", err
	}
	keySum := sha256.Sum256([]byte(Key(issue.Identifier)))
	path := filepath.Join(dir, fmt.Sprintf("%x.json", keySum[:]))
	if err := regularManagedPath(root, path, "workspace state marker"); err != nil {
		return "", err
	}
	return path, nil
}

func (l *Local) loadState(issue domain.Issue) (workspaceState, bool, error) {
	path, err := l.statePath(issue)
	if err != nil {
		return workspaceState{}, false, err
	}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return workspaceState{}, false, nil
	}
	if err != nil {
		return workspaceState{}, false, fmt.Errorf("read workspace state %q: %w", path, err)
	}
	var state workspaceState
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil {
		return workspaceState{}, false, fmt.Errorf("decode workspace state %q: %w", path, err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return workspaceState{}, false, fmt.Errorf("decode workspace state %q: %w", path, err)
	}
	if state.Schema != "" && state.Schema != legacyWorkspaceStateSchema && state.Schema != workspaceStateSchema {
		return workspaceState{}, false, fmt.Errorf("workspace state %q uses unsupported schema %q; manual recovery is required", path, state.Schema)
	}
	if strings.TrimSpace(state.IssueID) == "" || strings.TrimSpace(state.Identifier) == "" {
		return workspaceState{}, false, fmt.Errorf("workspace state %q is missing required ownership fields; manual recovery is required", path)
	}
	if state.Preparation != "" && state.Preparation != preparationCreating && state.Preparation != preparationHookPending && state.Preparation != preparationReady {
		return workspaceState{}, false, fmt.Errorf("workspace state %q has invalid preparation phase %q; manual recovery is required", path, state.Preparation)
	}
	if state.Schema == legacyWorkspaceStateSchema && (state.Preparation != "" || state.SourceRoot != "" || state.GitCommonDir != "" || state.GitWorktreeDir != "" || state.GitCommonDevice != 0 || state.GitCommonInode != 0) {
		return workspaceState{}, false, fmt.Errorf("workspace state %q mixes v2 recovery fields into the v1 schema; manual recovery is required", path)
	}
	return state, true, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err == io.EOF {
		return nil
	} else if err != nil {
		return err
	}
	return errors.New("workspace state contains multiple JSON values")
}

func validateStateOwner(state workspaceState, issue domain.Issue) error {
	if state.IssueID != issue.ID {
		return fmt.Errorf("workspace state belongs to issue %q, not %q; manual recovery is required", state.IssueID, issue.ID)
	}
	if state.Identifier != issue.Identifier {
		return fmt.Errorf("workspace state identifier is %q, not %q; manual recovery is required", state.Identifier, issue.Identifier)
	}
	return nil
}

func (l *Local) writeState(issue domain.Issue, state workspaceState) error {
	if _, err := l.ensureWorkspaceRoot(); err != nil {
		return err
	}
	path, err := l.statePath(issue)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create workspace state directory: %w", err)
	}
	state.Schema = workspaceStateSchema
	state.CompletedUpdatedAt = nil
	b, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode workspace state: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".state-*")
	if err != nil {
		return fmt.Errorf("create workspace state: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("set workspace state permissions: %w", err)
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("write workspace state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close workspace state: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace workspace state: %w", err)
	}
	return nil
}

func (l *Local) removeState(issue domain.Issue) error {
	path, err := l.statePath(issue)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove workspace state: %w", err)
	}
	return nil
}

func (l *Local) ensureState(ctx context.Context, ws domain.Workspace, issue domain.Issue) error {
	state, found, err := l.loadState(issue)
	if err != nil {
		return err
	}
	if found {
		if err := validateStateOwner(state, issue); err != nil {
			return err
		}
	}
	if !found {
		if !ws.CreatedNow {
			return errors.New("workspace state is missing for an existing workspace; manual recovery is required")
		}
		state = workspaceState{Schema: workspaceStateSchema, IssueID: issue.ID, Identifier: issue.Identifier}
	}
	git, err := isGitWorkspace(ws.Path)
	if err != nil {
		return err
	}
	if git && state.BaseCommit == "" {
		if state.SourceRoot == "" {
			return errors.New("workspace became a Git repository without recorded source-worktree identity; manual recovery is required")
		}
		state.BaseCommit, err = gitHead(ctx, ws.Path)
		if err != nil {
			return err
		}
	}
	state.IssueID = issue.ID
	state.Identifier = issue.Identifier
	if state.Preparation == "" {
		state.Preparation = preparationReady
	}
	return l.writeState(issue, state)
}

func isGitWorkspace(path string) (bool, error) {
	_, err := os.Lstat(filepath.Join(path, ".git"))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect Git workspace: %w", err)
	}
	return true, nil
}

func gitHead(ctx context.Context, path string) (string, error) {
	head, err := gitMetadata(ctx, path, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return "", fmt.Errorf("read Git workspace HEAD: %w", err)
	}
	if head == "" {
		return "", errors.New("read Git workspace HEAD: empty commit")
	}
	return head, nil
}

func ensureGitWorkspaceUnchanged(ctx context.Context, path, baseCommit string) error {
	status, err := gitMetadataAllowEmpty(ctx, path, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return fmt.Errorf("inspect Git workspace changes: %w", err)
	}
	if strings.TrimSpace(status) != "" {
		return errors.New("refusing to remove Git workspace with uncommitted or untracked changes")
	}
	head, err := gitHead(ctx, path)
	if err != nil {
		return err
	}
	if head != baseCommit {
		return fmt.Errorf("refusing to remove Git workspace whose HEAD %s differs from recorded base commit %s", head, baseCommit)
	}
	return nil
}

func (l *Local) hook(ctx context.Context, ws domain.Workspace, issue domain.Issue, name, script string) error {
	if script == "" {
		return nil
	}
	path, err := l.managedWorkspacePath(ws.Path)
	if err != nil {
		return err
	}
	timeout := l.settings().Hooks.Timeout
	if timeout <= 0 {
		timeout = time.Minute
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "sh", "-lc", script)
	cmd.Dir = path
	cmd.Env = append(os.Environ(), "SYMPHONY_ISSUE_ID="+issue.ID, "SYMPHONY_ISSUE_IDENTIFIER="+issue.Identifier)
	var stdout, stderr boundedBuffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	if err != nil {
		diagnostics := strings.TrimSpace(strings.Join([]string{stdout.String(), stderr.String()}, "\n"))
		return fmt.Errorf("%s hook failed: %w: %s", name, err, diagnostics)
	}
	return nil
}

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

type gitSourceIdentity struct {
	sourceRoot   string
	commonDir    string
	commonDevice uint64
	commonInode  uint64
}

func sourceIdentity(ctx context.Context, sourceRoot string) (gitSourceIdentity, error) {
	top, err := gitMetadata(ctx, sourceRoot, "rev-parse", "--path-format=absolute", "--show-toplevel")
	if err != nil {
		return gitSourceIdentity{}, fmt.Errorf("classify workspace source repository: %w", err)
	}
	common, err := gitMetadata(ctx, sourceRoot, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return gitSourceIdentity{}, fmt.Errorf("classify workspace source Git directory: %w", err)
	}
	top, err = canonicalExistingDirectory(top)
	if err != nil {
		return gitSourceIdentity{}, fmt.Errorf("resolve workspace source repository: %w", err)
	}
	common, err = canonicalExistingDirectory(common)
	if err != nil {
		return gitSourceIdentity{}, fmt.Errorf("resolve workspace source Git directory: %w", err)
	}
	device, inode, err := directoryIdentity(common)
	if err != nil {
		return gitSourceIdentity{}, fmt.Errorf("identify workspace source Git directory: %w", err)
	}
	return gitSourceIdentity{sourceRoot: top, commonDir: common, commonDevice: device, commonInode: inode}, nil
}

func directoryIdentity(path string) (uint64, uint64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, 0, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, errors.New("filesystem does not expose stable directory identity")
	}
	return uint64(stat.Dev), uint64(stat.Ino), nil
}

func worktreeIdentity(ctx context.Context, path, expectedCommon string) (string, error) {
	common, err := gitMetadata(ctx, path, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", fmt.Errorf("classify created worktree common directory: %w", err)
	}
	common, err = canonicalExistingDirectory(common)
	if err != nil {
		return "", fmt.Errorf("resolve created worktree common directory: %w", err)
	}
	if common != expectedCommon {
		return "", fmt.Errorf("created worktree belongs to Git directory %q, expected %q; manual recovery is required", common, expectedCommon)
	}
	gitDir, err := gitMetadata(ctx, path, "rev-parse", "--path-format=absolute", "--absolute-git-dir")
	if err != nil {
		return "", fmt.Errorf("classify created worktree Git directory: %w", err)
	}
	gitDir, err = canonicalExistingDirectory(gitDir)
	if err != nil {
		return "", fmt.Errorf("resolve created worktree Git directory: %w", err)
	}
	return gitDir, nil
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

func validateWorktreeIdentity(path string, state workspaceState) error {
	if state.GitWorktreeDir == "" || state.GitCommonDir == "" || state.GitCommonDevice == 0 || state.GitCommonInode == 0 {
		return errors.New("refusing to remove Git workspace with incomplete source-worktree identity; preserve it outside the managed root for manual recovery")
	}
	b, err := os.ReadFile(filepath.Join(path, ".git"))
	if err != nil {
		return fmt.Errorf("read owned worktree identity: %w", err)
	}
	line := strings.TrimSpace(string(b))
	if !strings.HasPrefix(line, "gitdir:") {
		return errors.New("owned worktree has an unrecognized .git identity; manual recovery is required")
	}
	gitDir := strings.TrimSpace(strings.TrimPrefix(line, "gitdir:"))
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(path, gitDir)
	}
	abs, err := filepath.Abs(filepath.Clean(gitDir))
	if err != nil {
		return fmt.Errorf("resolve owned worktree identity: %w", err)
	}
	if abs != filepath.Clean(state.GitWorktreeDir) {
		return fmt.Errorf("refusing to remove worktree whose Git directory is %q, not recorded identity %q; manual recovery is required", abs, state.GitWorktreeDir)
	}
	if !below(filepath.Clean(state.GitCommonDir), abs) {
		return errors.New("recorded worktree Git directory is outside its recorded Git common directory; manual recovery is required")
	}
	return nil
}

func (l *Local) recoverPreparation(ctx context.Context, issue domain.Issue, path string, state workspaceState) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if state.Preparation != preparationCreating && state.Preparation != preparationHookPending {
		return fmt.Errorf("workspace preparation phase %q cannot be recovered automatically; manual recovery is required", state.Preparation)
	}
	managedPath, err := l.workspacePath(issue)
	if err != nil {
		return err
	}
	if filepath.Clean(path) != managedPath {
		return errors.New("partial workspace is outside its owned managed path")
	}
	_, statErr := os.Stat(path)
	if statErr == nil {
		git, err := isGitWorkspace(path)
		if err != nil {
			return err
		}
		if git && state.SourceRoot != "" {
			if state.GitWorktreeDir == "" {
				if _, sourceErr := os.Stat(state.SourceRoot); sourceErr != nil {
					return errors.New("partial worktree identity is incomplete and its source repository is unavailable; manual recovery is required")
				}
				state.GitWorktreeDir, err = worktreeIdentity(ctx, path, state.GitCommonDir)
				if err != nil {
					return err
				}
				if err := l.writeState(issue, state); err != nil {
					return err
				}
			}
			if err := validateWorktreeIdentity(path, state); err != nil {
				return err
			}
		}
		if state.SourceRoot != "" {
			available, availableErr := gitRepositoryAvailable(ctx, state)
			if availableErr != nil {
				return availableErr
			}
			if available {
				// Failure is recoverable below: removing the owned path followed by
				// prune reconciles a stale registration left by Git.
				_ = removeRecordedWorktree(ctx, state, path, true)
			}
		}
		if _, err := os.Stat(path); err == nil {
			if err := os.RemoveAll(path); err != nil {
				return fmt.Errorf("remove partial owned workspace: %w", err)
			}
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("inspect partial owned workspace: %w", err)
		}
	} else if !os.IsNotExist(statErr) {
		return fmt.Errorf("inspect partial owned workspace: %w", statErr)
	}
	if state.SourceRoot != "" {
		available, availableErr := gitRepositoryAvailable(ctx, state)
		if availableErr != nil {
			return availableErr
		}
		if available {
			if err := pruneRecordedWorktrees(ctx, state); err != nil {
				return err
			}
		}
	}
	return l.removeState(issue)
}

type boundedBuffer struct {
	b []byte
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	remaining := maxHookOutput - len(b.b)
	if remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		b.b = append(b.b, p...)
	}
	return n, nil
}

func (b *boundedBuffer) String() string {
	value := strings.TrimSpace(string(b.b))
	if len(b.b) == maxHookOutput {
		value += "...[truncated]"
	}
	return value
}

func gitMetadata(ctx context.Context, dir string, args ...string) (string, error) {
	value, err := gitMetadataAllowEmpty(ctx, dir, args...)
	if err != nil {
		return "", err
	}
	if value == "" {
		return "", fmt.Errorf("git %s returned empty metadata", strings.Join(args, " "))
	}
	return value, nil
}

func gitMetadataAllowEmpty(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	var stdout, stderr boundedBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, stderr.String())
	}
	value := stdout.String()
	return value, nil
}

func addWorktree(ctx context.Context, sourceRoot, path string) error {
	if err := gitMutation(ctx, sourceRoot, "fetch", "--no-tags", "origin", "+refs/heads/main:refs/remotes/origin/main"); err != nil {
		return fmt.Errorf("refresh origin/main before creating workspace: %w", err)
	}
	baseCommit, err := gitMetadata(ctx, sourceRoot, "rev-parse", "--verify", "refs/remotes/origin/main^{commit}")
	if err != nil {
		return fmt.Errorf("resolve refreshed origin/main commit: %w", err)
	}
	if err := gitMutation(ctx, sourceRoot, "worktree", "add", "--detach", path, baseCommit); err != nil {
		return fmt.Errorf("create workspace worktree: %w", err)
	}
	return nil
}

func gitRepositoryAvailable(ctx context.Context, state workspaceState) (bool, error) {
	if _, err := os.Stat(state.SourceRoot); err == nil {
		identity, identityErr := sourceIdentity(ctx, state.SourceRoot)
		if identityErr != nil {
			return false, fmt.Errorf("validate recorded source repository: %w; manual recovery is required", identityErr)
		}
		if identity.sourceRoot != filepath.Clean(state.SourceRoot) || identity.commonDir != filepath.Clean(state.GitCommonDir) || identity.commonDevice != state.GitCommonDevice || identity.commonInode != state.GitCommonInode {
			return false, errors.New("recorded source path now identifies a different Git repository; manual recovery is required")
		}
		return true, nil
	} else if !os.IsNotExist(err) {
		return false, fmt.Errorf("inspect recorded source repository path: %w", err)
	}
	if common, err := canonicalExistingDirectory(state.GitCommonDir); err == nil {
		device, inode, identityErr := directoryIdentity(common)
		if identityErr != nil {
			return false, identityErr
		}
		if device != state.GitCommonDevice || inode != state.GitCommonInode {
			return false, errors.New("recorded Git common directory now identifies a different repository; manual recovery is required")
		}
		return true, nil
	} else if !os.IsNotExist(err) {
		return false, fmt.Errorf("inspect recorded Git common directory: %w", err)
	}
	return false, nil
}

func removeRecordedWorktree(ctx context.Context, state workspaceState, path string, force bool) error {
	args := []string{"worktree", "remove"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, path)
	if err := gitRecordedMutation(ctx, state, args...); err != nil {
		return fmt.Errorf("remove workspace worktree: %w", err)
	}
	return nil
}

func pruneRecordedWorktrees(ctx context.Context, state workspaceState) error {
	if err := gitRecordedMutation(ctx, state, "worktree", "prune"); err != nil {
		return fmt.Errorf("prune workspace worktrees: %w", err)
	}
	return nil
}

func gitRecordedMutation(ctx context.Context, state workspaceState, args ...string) error {
	if _, err := os.Stat(state.SourceRoot); err == nil {
		identity, identityErr := sourceIdentity(ctx, state.SourceRoot)
		if identityErr != nil || identity.sourceRoot != filepath.Clean(state.SourceRoot) || identity.commonDir != filepath.Clean(state.GitCommonDir) || identity.commonDevice != state.GitCommonDevice || identity.commonInode != state.GitCommonInode {
			return errors.New("recorded source path no longer identifies the expected Git repository; manual recovery is required")
		}
		return gitMutation(ctx, state.SourceRoot, args...)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect recorded source repository: %w", err)
	}
	if _, err := os.Stat(state.GitCommonDir); err != nil {
		return fmt.Errorf("inspect recorded Git common directory: %w", err)
	}
	device, inode, err := directoryIdentity(state.GitCommonDir)
	if err != nil || device != state.GitCommonDevice || inode != state.GitCommonInode {
		return errors.New("recorded Git common directory no longer identifies the expected repository; manual recovery is required")
	}
	cmd := exec.CommandContext(ctx, "git", append([]string{"--git-dir=" + state.GitCommonDir}, args...)...)
	var stdout, stderr boundedBuffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, stderr.String())
	}
	return nil
}

func gitMutation(ctx context.Context, sourceRoot string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", sourceRoot}, args...)...)
	var stdout, stderr boundedBuffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, stderr.String())
	}
	return nil
}
