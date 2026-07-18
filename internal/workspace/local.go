// Package workspace implements local, bounded workspace lifecycle operations.
package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
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

const stateDirectory = ".symphony-state"

var unsafe = regexp.MustCompile(`[^A-Za-z0-9._-]`)

type Local struct{ settings func() config.Settings }

// workspaceState is deliberately stored below workspace.root rather than in a
// process-local directory. It is the durable handoff record used after a
// service restart and includes the initial detached-worktree commit so cleanup
// can refuse to delete work that was changed or committed locally.
type workspaceState struct {
	IssueID            string     `json:"issue_id"`
	Identifier         string     `json:"identifier"`
	BaseCommit         string     `json:"base_commit,omitempty"`
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

// ShouldRun suppresses a completed issue only when Linear still reports the
// exact version that completed. Any update, including a reopen or new comment,
// makes the issue eligible again.
func (l *Local) ShouldRun(ctx context.Context, issue domain.Issue) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	state, found, err := l.loadState(issue)
	if err != nil || !found {
		return true, err
	}
	if state.IssueID != issue.ID || state.CompletedUpdatedAt == nil {
		return true, nil
	}
	return !sameTime(state.CompletedUpdatedAt, issue.UpdatedAt), nil
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
	if err := l.ensureState(ctx, ws, issue); err != nil {
		return domain.Workspace{}, err
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

// MarkCompleted records the exact Linear issue version that reached a normal
// handoff. It intentionally does not alter the worktree: a later terminal
// cleanup is subject to the same safety checks as every other removal.
func (l *Local) MarkCompleted(ctx context.Context, ws domain.Workspace, issue domain.Issue) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	expected, err := l.workspacePath(issue)
	if err != nil {
		return err
	}
	if filepath.Clean(ws.Path) != expected {
		return fmt.Errorf("completion workspace does not match issue workspace")
	}
	state, found, err := l.loadState(issue)
	if err != nil {
		return err
	}
	if !found {
		state = workspaceState{IssueID: issue.ID, Identifier: issue.Identifier}
		if git, err := isGitWorkspace(ws.Path); err != nil {
			return err
		} else if git {
			state.BaseCommit, err = gitHead(ctx, ws.Path)
			if err != nil {
				return err
			}
		}
	}
	if state.IssueID != "" && state.IssueID != issue.ID {
		return fmt.Errorf("workspace state belongs to issue %q, not %q", state.IssueID, issue.ID)
	}
	state.IssueID = issue.ID
	state.Identifier = issue.Identifier
	state.CompletedUpdatedAt = cloneTime(issue.UpdatedAt)
	return l.writeState(issue, state)
}

func (l *Local) Cleanup(ctx context.Context, issue domain.Issue) error {
	settings := l.settings()
	path, err := l.workspacePath(issue)
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return l.removeState(issue)
	} else if err != nil {
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
		state, found, err := l.loadState(issue)
		if err != nil {
			return err
		}
		if !found || state.BaseCommit == "" {
			return errors.New("refusing to remove Git workspace without a recorded base commit")
		}
		if state.IssueID != issue.ID {
			return fmt.Errorf("refusing to remove Git workspace owned by issue %q", state.IssueID)
		}
		if err := ensureGitWorkspaceUnchanged(ctx, path, state.BaseCommit); err != nil {
			return err
		}
		if settings.Workspace.SourceRoot != "" {
			if err := removeWorktree(ctx, settings.Workspace.SourceRoot, path); err != nil {
				return err
			}
		} else if err := os.RemoveAll(path); err != nil {
			return err
		}
	} else if err := os.RemoveAll(path); err != nil {
		return err
	}
	// Do not discard the completion record until the workspace was removed.
	return l.removeState(issue)
}
func (l *Local) Execute(ctx context.Context, ws domain.Workspace, command string, args []string) ([]byte, error) {
	if !below(l.settings().Workspace.Root, ws.Path) {
		return nil, fmt.Errorf("workspace execution path escapes root")
	}
	c := exec.CommandContext(ctx, command, args...)
	c.Dir = ws.Path
	return c.CombinedOutput()
}

func (l *Local) workspacePath(issue domain.Issue) (string, error) {
	root := l.settings().Workspace.Root
	path := filepath.Join(root, Key(issue.Identifier))
	if !below(root, path) {
		return "", fmt.Errorf("workspace path escapes root")
	}
	return filepath.Clean(path), nil
}

func (l *Local) statePath(issue domain.Issue) (string, error) {
	root := l.settings().Workspace.Root
	dir := filepath.Join(root, stateDirectory)
	if !below(root, dir) {
		return "", fmt.Errorf("workspace state path escapes root")
	}
	keySum := sha256.Sum256([]byte(Key(issue.Identifier)))
	path := filepath.Join(dir, fmt.Sprintf("%x.json", keySum[:]))
	if !below(root, path) {
		return "", fmt.Errorf("workspace state path escapes root")
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
		return workspaceState{}, false, fmt.Errorf("read workspace state: %w", err)
	}
	var state workspaceState
	if err := json.Unmarshal(b, &state); err != nil {
		return workspaceState{}, false, fmt.Errorf("decode workspace state: %w", err)
	}
	return state, true, nil
}

func (l *Local) writeState(issue domain.Issue, state workspaceState) error {
	path, err := l.statePath(issue)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create workspace state directory: %w", err)
	}
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
	if found && state.IssueID != "" && state.IssueID != issue.ID {
		return fmt.Errorf("workspace state belongs to issue %q, not %q", state.IssueID, issue.ID)
	}
	if !found {
		state = workspaceState{IssueID: issue.ID, Identifier: issue.Identifier}
	}
	git, err := isGitWorkspace(ws.Path)
	if err != nil {
		return err
	}
	if git && state.BaseCommit == "" {
		state.BaseCommit, err = gitHead(ctx, ws.Path)
		if err != nil {
			return err
		}
	}
	state.IssueID = issue.ID
	state.Identifier = issue.Identifier
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
	cmd := exec.CommandContext(ctx, "git", "-C", path, "rev-parse", "--verify", "HEAD")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("read Git workspace HEAD: %w: %s", err, strings.TrimSpace(string(out)))
	}
	head := strings.TrimSpace(string(out))
	if head == "" {
		return "", errors.New("read Git workspace HEAD: empty commit")
	}
	return head, nil
}

func ensureGitWorkspaceUnchanged(ctx context.Context, path, baseCommit string) error {
	status := exec.CommandContext(ctx, "git", "-C", path, "status", "--porcelain=v1", "--untracked-files=all")
	out, err := status.CombinedOutput()
	if err != nil {
		return fmt.Errorf("inspect Git workspace changes: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if strings.TrimSpace(string(out)) != "" {
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

func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

func cloneTime(v *time.Time) *time.Time {
	if v == nil {
		return nil
	}
	c := *v
	return &c
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
	cmd := exec.CommandContext(ctx, "git", "-C", sourceRoot, "worktree", "remove", path)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("remove workspace worktree: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
