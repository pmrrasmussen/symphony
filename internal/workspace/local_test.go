package workspace

import (
	"context"
	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/domain"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestKeyAndWorkspaceRemainBelowRoot(t *testing.T) {
	root := t.TempDir()
	s := config.Settings{Workspace: config.Workspace{Root: root}, Hooks: config.Hooks{}}
	l := New(func() config.Settings { return s })
	i := domain.Issue{ID: "1", Identifier: "../unsafe/name"}
	ws, err := l.Prepare(context.Background(), i)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ws.Path, root+string(filepath.Separator)) {
		t.Fatalf("escaped root: %s", ws.Path)
	}
	if !strings.Contains(ws.Key, "--") {
		t.Fatal("sanitized key needs hash")
	}
}

func TestKeyNeverResolvesToWorkspaceRoot(t *testing.T) {
	for _, identifier := range []string{"", ".", "..", stateDirectory} {
		if key := Key(identifier); key == "" || key == "." || key == ".." {
			t.Fatalf("unsafe key for %q: %q", identifier, key)
		}
		if key := Key(identifier); key == stateDirectory {
			t.Fatalf("workspace key conflicts with durable state directory for %q", identifier)
		}
	}
}

func TestWorkspaceOperationsRejectSymlinkedWorkspace(t *testing.T) {
	root := t.TempDir()
	target := t.TempDir()
	s := config.Settings{Workspace: config.Workspace{Root: root}, Hooks: config.Hooks{BeforeRun: "true"}}
	l := New(func() config.Settings { return s })
	issue := domain.Issue{ID: "1", Identifier: "PMR-1"}
	path := filepath.Join(root, Key(issue.Identifier))
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}

	if _, err := l.Prepare(context.Background(), issue); err == nil || !strings.Contains(err.Error(), "must not be a symlink") {
		t.Fatalf("Prepare error = %v, want symlink rejection", err)
	}
	ws := domain.Workspace{Path: path, Key: Key(issue.Identifier)}
	if _, err := l.Execute(context.Background(), ws, "true", nil); err == nil || !strings.Contains(err.Error(), "must not be a symlink") {
		t.Fatalf("Execute error = %v, want symlink rejection", err)
	}
	if err := l.BeforeRun(context.Background(), ws, issue); err == nil || !strings.Contains(err.Error(), "must not be a symlink") {
		t.Fatalf("BeforeRun error = %v, want symlink rejection", err)
	}
	if err := l.MarkCompleted(context.Background(), ws, issue); err == nil || !strings.Contains(err.Error(), "must not be a symlink") {
		t.Fatalf("MarkCompleted error = %v, want symlink rejection", err)
	}
	if err := l.Cleanup(context.Background(), issue); err == nil || !strings.Contains(err.Error(), "must not be a symlink") {
		t.Fatalf("Cleanup error = %v, want symlink rejection", err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("symlink target must not be removed: %v", err)
	}
}

func TestStatePathsRejectSymlinkedDirectoryAndMarker(t *testing.T) {
	root := t.TempDir()
	target := t.TempDir()
	s := config.Settings{Workspace: config.Workspace{Root: root}, Hooks: config.Hooks{}}
	l := New(func() config.Settings { return s })
	issue := domain.Issue{ID: "1", Identifier: "PMR-1"}
	stateDir := filepath.Join(root, stateDirectory)
	if err := os.Symlink(target, stateDir); err != nil {
		t.Fatal(err)
	}
	if _, err := l.ShouldRun(context.Background(), issue); err == nil || !strings.Contains(err.Error(), "workspace state directory path must not be a symlink") {
		t.Fatalf("ShouldRun directory error = %v, want state symlink rejection", err)
	}
	if entries, err := os.ReadDir(target); err != nil || len(entries) != 0 {
		t.Fatalf("state target must remain untouched: entries=%v err=%v", entries, err)
	}
	if err := os.Remove(stateDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	marker, err := l.statePath(issue)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(target, "marker.json"), marker); err != nil {
		t.Fatal(err)
	}
	if _, err := l.ShouldRun(context.Background(), issue); err == nil || !strings.Contains(err.Error(), "workspace state marker path must not be a symlink") {
		t.Fatalf("ShouldRun marker error = %v, want state marker symlink rejection", err)
	}
}

func TestPrepareCanonicalizesSymlinkedConfiguredRoot(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(t.TempDir(), "workspaces")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	configured := filepath.Join(base, "configured-root")
	if err := os.Symlink(target, configured); err != nil {
		t.Fatal(err)
	}
	s := config.Settings{Workspace: config.Workspace{Root: configured}, Hooks: config.Hooks{}}
	l := New(func() config.Settings { return s })
	issue := domain.Issue{ID: "1", Identifier: "PMR-1"}
	ws, err := l.Prepare(context.Background(), issue)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(target, Key(issue.Identifier))
	if ws.Path != want {
		t.Fatalf("workspace path = %q, want canonical root path %q", ws.Path, want)
	}
	if _, err := os.Stat(filepath.Join(target, stateDirectory)); err != nil {
		t.Fatalf("state directory should use canonical root: %v", err)
	}
}

func TestPrepareUsesDetachedGitWorktree(t *testing.T) {
	source := t.TempDir()
	runGit(t, source, "init")
	runGit(t, source, "config", "user.email", "test@example.invalid")
	runGit(t, source, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("source\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, source, "add", "README.md")
	runGit(t, source, "commit", "-m", "initial")

	root := filepath.Join(t.TempDir(), "workspaces")
	s := config.Settings{Workspace: config.Workspace{Root: root, SourceRoot: source}, Hooks: config.Hooks{}}
	l := New(func() config.Settings { return s })
	issue := domain.Issue{ID: "1", Identifier: "PMR-1"}
	ws, err := l.Prepare(context.Background(), issue)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(ws.Path, "README.md")); err != nil {
		t.Fatalf("worktree lacks source file: %v", err)
	}
	branch := exec.Command("git", "-C", ws.Path, "branch", "--show-current")
	if out, err := branch.Output(); err != nil || strings.TrimSpace(string(out)) != "" {
		t.Fatalf("workspace must be detached, branch=%q err=%v", out, err)
	}
	state, found, err := l.loadState(issue)
	if err != nil || !found || state.BaseCommit == "" {
		t.Fatalf("detached worktree base commit was not recorded: state=%+v found=%t err=%v", state, found, err)
	}
	if err := l.Cleanup(context.Background(), issue); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ws.Path); !os.IsNotExist(err) {
		t.Fatalf("worktree was not removed: %v", err)
	}
	if _, found, err := l.loadState(issue); err != nil || found {
		t.Fatalf("workspace state should be removed only after successful cleanup: found=%t err=%v", found, err)
	}
}

func TestCompletedMarkerSkipsOnlyUnchangedIssue(t *testing.T) {
	root := t.TempDir()
	s := config.Settings{Workspace: config.Workspace{Root: root}, Hooks: config.Hooks{}}
	l := New(func() config.Settings { return s })
	updated := time.Date(2026, time.July, 18, 12, 0, 0, 0, time.UTC)
	issue := domain.Issue{ID: "1", Identifier: "PMR-1", UpdatedAt: &updated}

	shouldRun, err := l.ShouldRun(context.Background(), issue)
	if err != nil || !shouldRun {
		t.Fatalf("new issue should run: shouldRun=%t err=%v", shouldRun, err)
	}
	ws, err := l.Prepare(context.Background(), issue)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.MarkCompleted(context.Background(), ws, issue); err != nil {
		t.Fatal(err)
	}
	shouldRun, err = l.ShouldRun(context.Background(), issue)
	if err != nil || shouldRun {
		t.Fatalf("unchanged completed issue should not run: shouldRun=%t err=%v", shouldRun, err)
	}

	changed := issue
	changedAt := updated.Add(time.Second)
	changed.UpdatedAt = &changedAt
	shouldRun, err = l.ShouldRun(context.Background(), changed)
	if err != nil || !shouldRun {
		t.Fatalf("changed issue should run: shouldRun=%t err=%v", shouldRun, err)
	}
	if err := l.Cleanup(context.Background(), issue); err != nil {
		t.Fatal(err)
	}
	shouldRun, err = l.ShouldRun(context.Background(), issue)
	if err != nil || !shouldRun {
		t.Fatalf("terminal cleanup should remove completion state: shouldRun=%t err=%v", shouldRun, err)
	}
}

func TestCleanupPreservesChangedGitWorktrees(t *testing.T) {
	source := newGitRepository(t)
	root := filepath.Join(t.TempDir(), "workspaces")
	s := config.Settings{Workspace: config.Workspace{Root: root, SourceRoot: source}, Hooks: config.Hooks{}}
	l := New(func() config.Settings { return s })
	issue := domain.Issue{ID: "1", Identifier: "PMR-1"}
	ws, err := l.Prepare(context.Background(), issue)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { runGit(t, source, "worktree", "remove", "--force", ws.Path) })

	if err := os.WriteFile(filepath.Join(ws.Path, "untracked.txt"), []byte("keep me\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err = l.Cleanup(context.Background(), issue)
	if err == nil || !strings.Contains(err.Error(), "uncommitted or untracked") {
		t.Fatalf("dirty worktree cleanup error = %v, want clear preservation error", err)
	}
	if _, statErr := os.Stat(ws.Path); statErr != nil {
		t.Fatalf("dirty worktree must be preserved: %v", statErr)
	}
	if _, found, stateErr := l.loadState(issue); stateErr != nil || !found {
		t.Fatalf("state must remain until safe cleanup: found=%t err=%v", found, stateErr)
	}
	if err := os.Remove(filepath.Join(ws.Path, "untracked.txt")); err != nil {
		t.Fatal(err)
	}
	runGit(t, ws.Path, "commit", "--allow-empty", "-m", "new detached commit")
	err = l.Cleanup(context.Background(), issue)
	if err == nil || !strings.Contains(err.Error(), "differs from recorded base commit") {
		t.Fatalf("changed HEAD cleanup error = %v, want clear preservation error", err)
	}
	if _, statErr := os.Stat(ws.Path); statErr != nil {
		t.Fatalf("changed-HEAD worktree must be preserved: %v", statErr)
	}
}

func newGitRepository(t *testing.T) string {
	t.Helper()
	source := t.TempDir()
	runGit(t, source, "init")
	runGit(t, source, "config", "user.email", "test@example.invalid")
	runGit(t, source, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("source\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, source, "add", "README.md")
	runGit(t, source, "commit", "-m", "initial")
	return source
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
}
