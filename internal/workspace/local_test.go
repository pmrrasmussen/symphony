package workspace

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/domain"
	"github.com/pmrrasmussen/symphony/internal/observability"
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
	effectiveRoot, err := resolveExistingAncestors(root)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ws.Path, effectiveRoot+string(filepath.Separator)) {
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
	if _, err := l.Cleanup(context.Background(), issue); err == nil || !strings.Contains(err.Error(), "must not be a symlink") {
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
	if _, err := l.Prepare(context.Background(), issue); err == nil || !strings.Contains(err.Error(), "workspace state directory path must not be a symlink") {
		t.Fatalf("Prepare directory error = %v, want state symlink rejection", err)
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
	if _, err := l.Prepare(context.Background(), issue); err == nil || !strings.Contains(err.Error(), "workspace state marker path must not be a symlink") {
		t.Fatalf("Prepare marker error = %v, want state marker symlink rejection", err)
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
	effectiveTarget, err := resolveExistingAncestors(target)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(effectiveTarget, Key(issue.Identifier))
	if ws.Path != want {
		t.Fatalf("workspace path = %q, want canonical root path %q", ws.Path, want)
	}
	if _, err := os.Stat(filepath.Join(target, stateDirectory)); err != nil {
		t.Fatalf("state directory should use canonical root: %v", err)
	}
}

func TestPrepareUsesDetachedGitWorktree(t *testing.T) {
	source := newGitRepository(t)

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
	wantObjects, err := canonicalExistingDirectory(filepath.Join(state.GitCommonDir, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	if len(ws.GitMetadataRoots) != 2 || ws.GitMetadataRoots[0] != wantObjects || ws.GitMetadataRoots[1] != filepath.Clean(state.GitWorktreeDir) {
		t.Fatalf("Git metadata roots=%q, want object store %q and worktree dir %q", ws.GitMetadataRoots, wantObjects, state.GitWorktreeDir)
	}
	// The narrowed grant must never hand the agent the whole common directory,
	// its branch refs, or the primary index (PMR-65).
	for _, forbidden := range []string{state.GitCommonDir, filepath.Join(state.GitCommonDir, "refs", "heads"), filepath.Join(state.GitCommonDir, "index")} {
		for _, granted := range ws.GitMetadataRoots {
			if granted == forbidden {
				t.Fatalf("Git metadata roots leaked source-repo path %q: %q", forbidden, ws.GitMetadataRoots)
			}
		}
	}
	if _, err := l.Cleanup(context.Background(), issue); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ws.Path); !os.IsNotExist(err) {
		t.Fatalf("worktree was not removed: %v", err)
	}
	if _, found, err := l.loadState(issue); err != nil || found {
		t.Fatalf("workspace state should be removed only after successful cleanup: found=%t err=%v", found, err)
	}
}

func TestPrepareUsesRefreshedOriginMainWithoutChangingSourceCheckout(t *testing.T) {
	source := newGitRepository(t)
	publisher := cloneRepository(t, source)
	if err := os.WriteFile(filepath.Join(publisher, "remote.txt"), []byte("remote main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, publisher, "add", "remote.txt")
	runGit(t, publisher, "commit", "-m", "advance remote main")
	runGit(t, publisher, "push", "origin", "main")
	remoteMain := gitShow(t, publisher, "HEAD")

	staleLocalMain := gitShow(t, source, "main")
	if got := gitShow(t, source, "origin/main"); got != staleLocalMain {
		t.Fatalf("fixture origin/main = %s, want stale local main %s", got, staleLocalMain)
	}
	runGit(t, source, "switch", "-c", "feature")
	if err := os.WriteFile(filepath.Join(source, "feature.txt"), []byte("feature commit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, source, "add", "feature.txt")
	runGit(t, source, "commit", "-m", "feature work")
	if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("dirty source\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "untracked.txt"), []byte("untracked source\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sourceHead := gitShow(t, source, "HEAD")
	sourceStatus := gitOutput(t, source, "status", "--porcelain=v1", "--untracked-files=all")

	root := filepath.Join(t.TempDir(), "workspaces")
	s := config.Settings{Workspace: config.Workspace{Root: root, SourceRoot: source}}
	l := New(func() config.Settings { return s })
	issue := domain.Issue{ID: "issue-70", Identifier: "PMR-70"}
	ws, err := l.Prepare(context.Background(), issue)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = l.Cleanup(context.Background(), issue) })

	if got := gitShow(t, ws.Path, "HEAD"); got != remoteMain {
		t.Fatalf("workspace HEAD = %s, want refreshed origin/main %s", got, remoteMain)
	}
	if got := gitShow(t, source, "origin/main"); got != remoteMain {
		t.Fatalf("source origin/main = %s, want refreshed remote commit %s", got, remoteMain)
	}
	if got := gitShow(t, source, "main"); got != staleLocalMain {
		t.Fatalf("local main moved from %s to %s", staleLocalMain, got)
	}
	if got := gitShow(t, source, "HEAD"); got != sourceHead {
		t.Fatalf("source HEAD moved from %s to %s", sourceHead, got)
	}
	if got := gitOutput(t, source, "branch", "--show-current"); got != "feature" {
		t.Fatalf("source branch = %q, want feature", got)
	}
	if got := gitOutput(t, source, "status", "--porcelain=v1", "--untracked-files=all"); got != sourceStatus {
		t.Fatalf("source working state changed\nbefore=%q\nafter=%q", sourceStatus, got)
	}
	state, found, err := l.loadState(issue)
	if err != nil || !found || state.BaseCommit != remoteMain {
		t.Fatalf("workspace state=%+v found=%t err=%v, want base commit %s", state, found, err, remoteMain)
	}
}

// TestPrepareFetchesConfiguredBaseBranchNotMain asserts a non-"main"
// github.base_branch is the ref addWorktree fetches and creates the worktree
// from, rather than the literal "main" -- which the publish gate (reading
// refs/remotes/origin/<base>) does not check (PMR-135).
func TestPrepareFetchesConfiguredBaseBranchNotMain(t *testing.T) {
	remote := filepath.Join(t.TempDir(), "remote.git")
	runGit(t, filepath.Dir(remote), "init", "--bare", remote)
	source := filepath.Join(t.TempDir(), "source")
	if err := os.Mkdir(source, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, source, "init")
	runGit(t, source, "config", "user.email", "test@example.invalid")
	runGit(t, source, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("source\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, source, "add", "README.md")
	runGit(t, source, "commit", "-m", "initial")
	runGit(t, source, "branch", "-M", "develop")
	runGit(t, source, "remote", "add", "origin", remote)
	runGit(t, source, "push", "-u", "origin", "develop")
	runGit(t, remote, "symbolic-ref", "HEAD", "refs/heads/develop")

	publisher := cloneRepository(t, source)
	if err := os.WriteFile(filepath.Join(publisher, "remote.txt"), []byte("remote develop\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, publisher, "add", "remote.txt")
	runGit(t, publisher, "commit", "-m", "advance remote develop")
	runGit(t, publisher, "push", "origin", "develop")
	remoteDevelop := gitShow(t, publisher, "HEAD")

	root := filepath.Join(t.TempDir(), "workspaces")
	s := config.Settings{Workspace: config.Workspace{Root: root, SourceRoot: source}, GitHub: config.GitHub{BaseBranch: "develop"}}
	l := New(func() config.Settings { return s })
	issue := domain.Issue{ID: "issue-135", Identifier: "PMR-135"}
	ws, err := l.Prepare(context.Background(), issue)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = l.Cleanup(context.Background(), issue) })

	if got := gitShow(t, ws.Path, "HEAD"); got != remoteDevelop {
		t.Fatalf("workspace HEAD = %s, want refreshed origin/develop %s", got, remoteDevelop)
	}
	if got := gitShow(t, source, "origin/develop"); got != remoteDevelop {
		t.Fatalf("source origin/develop = %s, want refreshed remote commit %s", got, remoteDevelop)
	}
	if out, err := exec.Command("git", "-C", source, "rev-parse", "--verify", "refs/remotes/origin/main").CombinedOutput(); err == nil {
		t.Fatalf("addWorktree unexpectedly created refs/remotes/origin/main for a non-main base branch: %s", out)
	}
}

func TestPrepareExistingWorkspaceDoesNotRefreshOrReset(t *testing.T) {
	source := newGitRepository(t)
	publisher := cloneRepository(t, source)
	root := filepath.Join(t.TempDir(), "workspaces")
	s := config.Settings{Workspace: config.Workspace{Root: root, SourceRoot: source}}
	l := New(func() config.Settings { return s })
	issue := domain.Issue{ID: "issue-70", Identifier: "PMR-70"}
	ws, err := l.Prepare(context.Background(), issue)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { runGit(t, source, "worktree", "remove", "--force", ws.Path) })
	runGit(t, ws.Path, "commit", "--allow-empty", "-m", "task history")
	workspaceHead := gitShow(t, ws.Path, "HEAD")
	trackedRemoteMain := gitShow(t, source, "origin/main")

	if err := os.WriteFile(filepath.Join(publisher, "later.txt"), []byte("later remote main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, publisher, "add", "later.txt")
	runGit(t, publisher, "commit", "-m", "advance remote after workspace creation")
	runGit(t, publisher, "push", "origin", "main")
	if remoteMain := gitShow(t, publisher, "HEAD"); remoteMain == trackedRemoteMain {
		t.Fatal("fixture did not advance remote main")
	}

	redispatched, err := l.Prepare(context.Background(), issue)
	if err != nil {
		t.Fatal(err)
	}
	if redispatched.CreatedNow {
		t.Fatal("existing workspace was reported as newly created")
	}
	if redispatched.Path != ws.Path {
		t.Fatalf("redispatched path = %q, want %q", redispatched.Path, ws.Path)
	}
	if got := gitShow(t, ws.Path, "HEAD"); got != workspaceHead {
		t.Fatalf("existing workspace HEAD changed from %s to %s", workspaceHead, got)
	}
	if got := gitShow(t, source, "origin/main"); got != trackedRemoteMain {
		t.Fatalf("existing workspace dispatch unexpectedly refreshed origin/main from %s to %s", trackedRemoteMain, got)
	}
}

func TestPrepareFailsWhenOriginMainCannotBeRefreshed(t *testing.T) {
	tests := []struct {
		name        string
		configure   func(*testing.T, string)
		wantDetails string
	}{
		{name: "missing origin", wantDetails: "does not appear to be a git repository"},
		{
			name: "missing remote main",
			configure: func(t *testing.T, source string) {
				remote := filepath.Join(t.TempDir(), "remote.git")
				runGit(t, filepath.Dir(remote), "init", "--bare", remote)
				runGit(t, source, "remote", "add", "origin", remote)
			},
			wantDetails: "couldn't find remote ref refs/heads/main",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := t.TempDir()
			runGit(t, source, "init")
			runGit(t, source, "config", "user.email", "test@example.invalid")
			runGit(t, source, "config", "user.name", "Test")
			if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("source\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			runGit(t, source, "add", "README.md")
			runGit(t, source, "commit", "-m", "initial")
			if test.configure != nil {
				test.configure(t, source)
			}

			root := filepath.Join(t.TempDir(), "workspaces")
			s := config.Settings{Workspace: config.Workspace{Root: root, SourceRoot: source}}
			l := New(func() config.Settings { return s })
			issue := domain.Issue{ID: "issue-70", Identifier: "PMR-70"}
			_, err := l.Prepare(context.Background(), issue)
			if err == nil || !strings.Contains(err.Error(), "refresh origin/main before creating workspace") || !strings.Contains(err.Error(), test.wantDetails) {
				t.Fatalf("Prepare error = %v, want actionable origin/main refresh error containing %q", err, test.wantDetails)
			}
			if _, statErr := os.Stat(filepath.Join(root, Key(issue.Identifier))); !os.IsNotExist(statErr) {
				t.Fatalf("workspace created despite refresh failure: %v", statErr)
			}
		})
	}
}

func TestPrepareRejectsInvalidOwnershipState(t *testing.T) {
	root := t.TempDir()
	s := config.Settings{Workspace: config.Workspace{Root: root}, Hooks: config.Hooks{}}
	l := New(func() config.Settings { return s })
	updated := time.Date(2026, time.July, 18, 12, 0, 0, 0, time.UTC)
	issue := domain.Issue{ID: "issue-1", Identifier: "PMR-1", UpdatedAt: &updated}
	marker, err := l.statePath(issue)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(marker), 0o755); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "corrupt", body: `{`, want: "decode workspace state"},
		{name: "unknown schema", body: `{"schema":"symphony.workspace-state/v3","issue_id":"issue-1","identifier":"PMR-1"}`, want: "unsupported schema"},
		{name: "unknown field", body: `{"schema":"symphony.workspace-state/v1","issue_id":"issue-1","identifier":"PMR-1","surprise":true}`, want: "unknown field"},
		{name: "missing owner", body: `{"schema":"symphony.workspace-state/v1","identifier":"PMR-1"}`, want: "required ownership fields"},
		{name: "wrong owner", body: `{"schema":"symphony.workspace-state/v1","issue_id":"issue-2","identifier":"PMR-1"}`, want: "belongs to issue"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := os.WriteFile(marker, []byte(test.body), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := l.Prepare(context.Background(), issue)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Prepare error=%v, want fail-closed error containing %q", err, test.want)
			}
		})
	}
}

func TestMissingStateForExistingWorkspaceRequiresManualRecovery(t *testing.T) {
	root := t.TempDir()
	s := config.Settings{Workspace: config.Workspace{Root: root}, Hooks: config.Hooks{}}
	l := New(func() config.Settings { return s })
	updated := time.Date(2026, time.July, 18, 12, 0, 0, 0, time.UTC)
	issue := domain.Issue{ID: "issue-1", Identifier: "PMR-1", UpdatedAt: &updated}
	path := filepath.Join(root, Key(issue.Identifier))
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Prepare(context.Background(), issue); err == nil || !strings.Contains(err.Error(), "manual recovery") {
		t.Fatalf("Prepare existing workspace without marker error=%v", err)
	}

	// The documented recovery is deliberate and external: after Symphony is
	// stopped, the operator preserves the workspace outside the managed root.
	quarantine := filepath.Join(t.TempDir(), "PMR-1")
	if err := os.Rename(path, quarantine); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Prepare(context.Background(), issue); err != nil {
		t.Fatalf("recovered issue Prepare error=%v", err)
	}
	if _, err := os.Stat(quarantine); err != nil {
		t.Fatalf("quarantined workspace was not preserved: %v", err)
	}
}

func TestLegacyWorkspaceStateIsRecognizedAndUpgraded(t *testing.T) {
	root := t.TempDir()
	s := config.Settings{Workspace: config.Workspace{Root: root}, Hooks: config.Hooks{}}
	l := New(func() config.Settings { return s })
	updated := time.Date(2026, time.July, 18, 12, 0, 0, 0, time.UTC)
	issue := domain.Issue{ID: "issue-1", Identifier: "PMR-1", UpdatedAt: &updated}
	path := filepath.Join(root, Key(issue.Identifier))
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	marker, err := l.statePath(issue)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(marker), 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := `{"issue_id":"issue-1","identifier":"PMR-1","completed_updated_at":"2026-07-18T12:00:00Z"}`
	if err := os.WriteFile(marker, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Prepare(context.Background(), issue); err != nil {
		t.Fatal(err)
	}
	state, found, err := l.loadState(issue)
	if err != nil || !found || state.Schema != workspaceStateSchema || state.CompletedUpdatedAt != nil {
		t.Fatalf("upgraded state=%+v found=%t err=%v", state, found, err)
	}
	contents, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(contents), "completed_updated_at") {
		t.Fatalf("rewritten state retained legacy completion timestamp: %s", contents)
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
	_, err = l.Cleanup(context.Background(), issue)
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
	_, err = l.Cleanup(context.Background(), issue)
	if err == nil || !strings.Contains(err.Error(), "differs from recorded base commit") {
		t.Fatalf("changed HEAD cleanup error = %v, want clear preservation error", err)
	}
	if _, statErr := os.Stat(ws.Path); statErr != nil {
		t.Fatalf("changed-HEAD worktree must be preserved: %v", statErr)
	}
}

// landingCall records exactly what terminal cleanup asked the host verifier,
// so the tests can assert cleanup never verifies a commit it did not read from
// the worktree it is about to remove.
type landingCall struct{ issueID, identifier, commit string }

type stubLandingVerifier struct {
	landed bool
	err    error
	calls  []landingCall
}

func (s *stubLandingVerifier) VerifyLanded(_ context.Context, issue domain.Issue, commit string) (bool, error) {
	s.calls = append(s.calls, landingCall{issueID: issue.ID, identifier: issue.Identifier, commit: commit})
	return s.landed, s.err
}

// landedWorktree prepares an owned worktree and adds one local commit, the
// shape a completed Symphony run leaves behind: clean tree, HEAD past the
// recorded base commit.
func landedWorktree(t *testing.T, l *Local, source string, issue domain.Issue) (domain.Workspace, string) {
	t.Helper()
	ws, err := l.Prepare(context.Background(), issue)
	if err != nil {
		t.Fatal(err)
	}
	runGit(t, ws.Path, "commit", "--allow-empty", "-m", "landed work")
	return ws, gitShow(t, ws.Path, "HEAD")
}

func TestCleanupRemovesVerifiedLandedGitWorktree(t *testing.T) {
	source := newGitRepository(t)
	root := filepath.Join(t.TempDir(), "workspaces")
	s := config.Settings{Workspace: config.Workspace{Root: root, SourceRoot: source}}
	l := New(func() config.Settings { return s })
	issue := domain.Issue{ID: "issue-79", Identifier: "PMR-79"}
	ws, head := landedWorktree(t, l, source, issue)
	verifier := &stubLandingVerifier{landed: true}
	l.SetLandingVerifier(verifier)

	outcome, err := l.Cleanup(context.Background(), issue)
	if err != nil {
		t.Fatalf("verified landed cleanup error=%v", err)
	}
	if outcome != domain.CleanupLanded {
		t.Fatalf("outcome=%q, want %q", outcome, domain.CleanupLanded)
	}
	if want := []landingCall{{issueID: issue.ID, identifier: issue.Identifier, commit: head}}; !reflect.DeepEqual(verifier.calls, want) {
		t.Fatalf("verification calls=%v, want %v", verifier.calls, want)
	}
	if _, statErr := os.Stat(ws.Path); !os.IsNotExist(statErr) {
		t.Fatalf("verified landed worktree remains: %v", statErr)
	}
	if _, found, err := l.loadState(issue); err != nil || found {
		t.Fatalf("state must be discarded after removal: found=%t err=%v", found, err)
	}
	out, err := exec.Command("git", "-C", source, "worktree", "list", "--porcelain").CombinedOutput()
	if err != nil || strings.Contains(string(out), ws.Path) {
		t.Fatalf("stale worktree registration output=%q err=%v", out, err)
	}
}

// A restart rediscovers terminal issues with no in-process landing memory, so
// cleanup must reach the same verified outcome from durable state alone.
func TestCleanupAfterRestartRemovesVerifiedLandedGitWorktree(t *testing.T) {
	source := newGitRepository(t)
	root := filepath.Join(t.TempDir(), "workspaces")
	settings := func() config.Settings {
		return config.Settings{Workspace: config.Workspace{Root: root, SourceRoot: source}}
	}
	issue := domain.Issue{ID: "issue-79", Identifier: "PMR-79"}
	ws, head := landedWorktree(t, New(settings), source, issue)

	restarted := New(settings)
	verifier := &stubLandingVerifier{landed: true}
	restarted.SetLandingVerifier(verifier)
	outcome, err := restarted.Cleanup(context.Background(), issue)
	if err != nil || outcome != domain.CleanupLanded {
		t.Fatalf("restart cleanup outcome=%q err=%v", outcome, err)
	}
	if len(verifier.calls) != 1 || verifier.calls[0].commit != head {
		t.Fatalf("restart verification calls=%v, want the worktree HEAD %s", verifier.calls, head)
	}
	if _, statErr := os.Stat(ws.Path); !os.IsNotExist(statErr) {
		t.Fatalf("verified landed worktree remains after restart: %v", statErr)
	}
}

func TestCleanupPreservesUnverifiedAndDirtyWorktrees(t *testing.T) {
	tests := []struct {
		name     string
		verifier *stubLandingVerifier
		dirty    bool
		wantErr  string
		wantCall bool
	}{
		{name: "unpublished commit", verifier: &stubLandingVerifier{}, wantErr: "differs from recorded base commit", wantCall: true},
		{name: "unverifiable landing", verifier: &stubLandingVerifier{err: errors.New("github unavailable")}, wantErr: "merged landing could not be verified", wantCall: true},
		{name: "no verifier configured", wantErr: "differs from recorded base commit"},
		{name: "uncommitted changes outrank a verified landing", verifier: &stubLandingVerifier{landed: true}, dirty: true, wantErr: "uncommitted or untracked changes"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := newGitRepository(t)
			root := filepath.Join(t.TempDir(), "workspaces")
			s := config.Settings{Workspace: config.Workspace{Root: root, SourceRoot: source}}
			l := New(func() config.Settings { return s })
			issue := domain.Issue{ID: "issue-79", Identifier: "PMR-79"}
			ws, _ := landedWorktree(t, l, source, issue)
			t.Cleanup(func() { runGit(t, source, "worktree", "remove", "--force", ws.Path) })
			if test.verifier != nil {
				l.SetLandingVerifier(test.verifier)
			}
			if test.dirty {
				if err := os.WriteFile(filepath.Join(ws.Path, "untracked.txt"), []byte("keep me\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			outcome, err := l.Cleanup(context.Background(), issue)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("cleanup error=%v, want %q", err, test.wantErr)
			}
			if outcome == domain.CleanupLanded {
				t.Fatalf("refused cleanup reported outcome %q", outcome)
			}
			if test.verifier != nil && (len(test.verifier.calls) > 0) != test.wantCall {
				t.Fatalf("verification calls=%v, want consulted=%t", test.verifier.calls, test.wantCall)
			}
			if _, statErr := os.Stat(filepath.Join(ws.Path, ".git")); statErr != nil {
				t.Fatalf("preserved worktree is no longer intact: %v", statErr)
			}
			if _, found, stateErr := l.loadState(issue); stateErr != nil || !found {
				t.Fatalf("state must remain for manual recovery: found=%t err=%v", found, stateErr)
			}
		})
	}
}

// An unchanged worktree is still removed as an ordinary clean workspace, and a
// configured verifier is never consulted for work that was never committed.
func TestCleanupReportsCleanRemovalWithoutVerifyingUnchangedWorktree(t *testing.T) {
	source := newGitRepository(t)
	root := filepath.Join(t.TempDir(), "workspaces")
	s := config.Settings{Workspace: config.Workspace{Root: root, SourceRoot: source}}
	l := New(func() config.Settings { return s })
	verifier := &stubLandingVerifier{landed: true}
	l.SetLandingVerifier(verifier)
	issue := domain.Issue{ID: "issue-79", Identifier: "PMR-79"}
	ws, err := l.Prepare(context.Background(), issue)
	if err != nil {
		t.Fatal(err)
	}

	outcome, err := l.Cleanup(context.Background(), issue)
	if err != nil || outcome != domain.CleanupClean {
		t.Fatalf("unchanged worktree outcome=%q err=%v", outcome, err)
	}
	if len(verifier.calls) != 0 {
		t.Fatalf("unchanged worktree consulted the verifier: %v", verifier.calls)
	}
	if _, statErr := os.Stat(ws.Path); !os.IsNotExist(statErr) {
		t.Fatalf("unchanged worktree remains: %v", statErr)
	}
}

func TestCleanupPreservesWorktreeWhenEntireSourceRepositoryIsRemoved(t *testing.T) {
	source := newGitRepository(t)
	root := filepath.Join(t.TempDir(), "workspaces")
	s := config.Settings{Workspace: config.Workspace{Root: root, SourceRoot: source}}
	l := New(func() config.Settings { return s })
	issue := domain.Issue{ID: "issue-17", Identifier: "PMR-17"}
	ws, err := l.Prepare(context.Background(), issue)
	if err != nil {
		t.Fatal(err)
	}
	state, found, err := l.loadState(issue)
	if err != nil || !found || state.SourceRoot == "" || state.GitCommonDir == "" || state.GitWorktreeDir == "" {
		t.Fatalf("persisted identity=%+v found=%t err=%v", state, found, err)
	}
	if err := os.RemoveAll(source); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws.Path, "local.txt"), []byte("must survive"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Cleanup(context.Background(), issue); err == nil || !strings.Contains(err.Error(), "local changes cannot be verified") {
		t.Fatalf("Cleanup error=%v", err)
	}
	if b, err := os.ReadFile(filepath.Join(ws.Path, "local.txt")); err != nil || string(b) != "must survive" {
		t.Fatalf("local work was not preserved: %q err=%v", b, err)
	}
	if _, found, err := l.loadState(issue); err != nil || !found {
		t.Fatalf("state must remain for manual recovery: found=%t err=%v", found, err)
	}
}

func TestCleanupUsesCommonDirWhenLinkedSourceWorktreeIsRemoved(t *testing.T) {
	commonSource := newGitRepository(t)
	linkedSource := filepath.Join(t.TempDir(), "linked-source")
	runGit(t, commonSource, "worktree", "add", "--detach", linkedSource, "HEAD")
	root := filepath.Join(t.TempDir(), "workspaces")
	s := config.Settings{Workspace: config.Workspace{Root: root, SourceRoot: linkedSource}}
	l := New(func() config.Settings { return s })
	issue := domain.Issue{ID: "issue-17", Identifier: "PMR-17"}
	ws, err := l.Prepare(context.Background(), issue)
	if err != nil {
		t.Fatal(err)
	}
	runGit(t, commonSource, "worktree", "remove", "--force", linkedSource)
	if _, err := l.Cleanup(context.Background(), issue); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ws.Path); !os.IsNotExist(err) {
		t.Fatalf("worktree remains after common-dir cleanup: %v", err)
	}
	cmd := exec.Command("git", "-C", commonSource, "worktree", "list", "--porcelain")
	out, err := cmd.Output()
	if err != nil || strings.Contains(string(out), ws.Path) {
		t.Fatalf("stale worktree registration output=%q err=%v", out, err)
	}
}

func TestCleanupLegacyGitStateFailsClosedWithoutUsingCurrentSource(t *testing.T) {
	source := newGitRepository(t)
	root := filepath.Join(t.TempDir(), "workspaces")
	s := config.Settings{Workspace: config.Workspace{Root: root, SourceRoot: source}}
	l := New(func() config.Settings { return s })
	issue := domain.Issue{ID: "issue-17", Identifier: "PMR-17"}
	ws, err := l.Prepare(context.Background(), issue)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { runGit(t, source, "worktree", "remove", "--force", ws.Path) })
	state, _, err := l.loadState(issue)
	if err != nil {
		t.Fatal(err)
	}
	state.Schema = legacyWorkspaceStateSchema
	state.Preparation = ""
	state.SourceRoot, state.GitCommonDir, state.GitWorktreeDir = "", "", ""
	state.GitCommonDevice, state.GitCommonInode = 0, 0
	b, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	marker, err := l.statePath(issue)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Cleanup(context.Background(), issue); err == nil || !strings.Contains(err.Error(), "legacy Git workspace") {
		t.Fatalf("Cleanup error=%v", err)
	}
	if _, err := os.Stat(ws.Path); err != nil {
		t.Fatalf("legacy/unowned worktree was removed: %v", err)
	}
}

func TestAfterCreateFailureRemovesPartialWorkspaceBeforeRetry(t *testing.T) {
	source := newGitRepository(t)
	root := filepath.Join(t.TempDir(), "workspaces")
	s := config.Settings{Workspace: config.Workspace{Root: root, SourceRoot: source}, Hooks: config.Hooks{AfterCreate: "printf partial > partial.txt; exit 7"}}
	l := New(func() config.Settings { return s })
	issue := domain.Issue{ID: "issue-17", Identifier: "PMR-17"}
	path := filepath.Join(root, Key(issue.Identifier))
	if _, err := l.Prepare(context.Background(), issue); err == nil || !strings.Contains(err.Error(), "after_create hook failed") {
		t.Fatalf("Prepare error=%v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("failed-hook workspace was reused/preserved: %v", err)
	}
	if _, found, err := l.loadState(issue); err != nil || found {
		t.Fatalf("failed-hook state remains: found=%t err=%v", found, err)
	}
	s.Hooks.AfterCreate = "test ! -e partial.txt"
	ws, err := l.Prepare(context.Background(), issue)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = l.Cleanup(context.Background(), issue) })
	if !ws.CreatedNow {
		t.Fatal("retry must create a fresh workspace")
	}
}

func TestInterruptedPreparationMarkerIsReconciled(t *testing.T) {
	source := newGitRepository(t)
	root := filepath.Join(t.TempDir(), "workspaces")
	s := config.Settings{Workspace: config.Workspace{Root: root, SourceRoot: source}}
	l := New(func() config.Settings { return s })
	issue := domain.Issue{ID: "issue-17", Identifier: "PMR-17"}
	identity, err := sourceIdentity(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.writeState(issue, workspaceState{Schema: workspaceStateSchema, IssueID: issue.ID, Identifier: issue.Identifier, Preparation: preparationCreating, SourceRoot: identity.sourceRoot, GitCommonDir: identity.commonDir, GitCommonDevice: identity.commonDevice, GitCommonInode: identity.commonInode}); err != nil {
		t.Fatal(err)
	}
	ws, err := l.Prepare(context.Background(), issue)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = l.Cleanup(context.Background(), issue) })
	state, found, err := l.loadState(issue)
	if err != nil || !found || state.Preparation != preparationReady {
		t.Fatalf("reconciled state=%+v found=%t err=%v", state, found, err)
	}
	if _, err := os.Stat(ws.Path); err != nil {
		t.Fatalf("recreated workspace: %v", err)
	}
}

func TestCleanupRetryPreservesMarkerUntilWorkspaceIsSafe(t *testing.T) {
	source := newGitRepository(t)
	root := filepath.Join(t.TempDir(), "workspaces")
	s := config.Settings{Workspace: config.Workspace{Root: root, SourceRoot: source}}
	l := New(func() config.Settings { return s })
	issue := domain.Issue{ID: "issue-17", Identifier: "PMR-17"}
	ws, err := l.Prepare(context.Background(), issue)
	if err != nil {
		t.Fatal(err)
	}
	dirty := filepath.Join(ws.Path, "retry.txt")
	if err := os.WriteFile(dirty, []byte("operator work"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Cleanup(context.Background(), issue); err == nil || !strings.Contains(err.Error(), "uncommitted or untracked") {
		t.Fatalf("first Cleanup error=%v", err)
	}
	if _, found, err := l.loadState(issue); err != nil || !found {
		t.Fatalf("retry state found=%t err=%v", found, err)
	}
	if err := os.Remove(dirty); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Cleanup(context.Background(), issue); err != nil {
		t.Fatal(err)
	}
}

func TestRestartReconcilesHookPendingWorkspaceAndRerunsHook(t *testing.T) {
	source := newGitRepository(t)
	root := filepath.Join(t.TempDir(), "workspaces")
	counter := filepath.Join(t.TempDir(), "counter")
	t.Setenv("PMR17_HOOK_COUNTER", counter)
	s := config.Settings{Workspace: config.Workspace{Root: root, SourceRoot: source}, Hooks: config.Hooks{AfterCreate: `printf x >> "$PMR17_HOOK_COUNTER"`}}
	issue := domain.Issue{ID: "issue-17", Identifier: "PMR-17"}
	first := New(func() config.Settings { return s })
	if _, err := first.Prepare(context.Background(), issue); err != nil {
		t.Fatal(err)
	}
	state, _, err := first.loadState(issue)
	if err != nil {
		t.Fatal(err)
	}
	state.Preparation = preparationHookPending
	if err := first.writeState(issue, state); err != nil {
		t.Fatal(err)
	}
	restarted := New(func() config.Settings { return s })
	if _, err := restarted.Prepare(context.Background(), issue); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = restarted.Cleanup(context.Background(), issue) })
	b, err := os.ReadFile(counter)
	if err != nil || string(b) != "xx" {
		t.Fatalf("hook counter=%q err=%v, want two fresh executions", b, err)
	}
}

func TestCleanupRejectsUnownedWorktreeIdentity(t *testing.T) {
	source := newGitRepository(t)
	root := filepath.Join(t.TempDir(), "workspaces")
	s := config.Settings{Workspace: config.Workspace{Root: root, SourceRoot: source}}
	l := New(func() config.Settings { return s })
	issue := domain.Issue{ID: "issue-17", Identifier: "PMR-17"}
	ws, err := l.Prepare(context.Background(), issue)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { runGit(t, source, "worktree", "remove", "--force", ws.Path) })
	state, _, err := l.loadState(issue)
	if err != nil {
		t.Fatal(err)
	}
	state.GitWorktreeDir = filepath.Join(t.TempDir(), "unowned")
	if err := l.writeState(issue, state); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Cleanup(context.Background(), issue); err == nil || !strings.Contains(err.Error(), "recorded identity") {
		t.Fatalf("Cleanup error=%v", err)
	}
	if _, err := os.Stat(ws.Path); err != nil {
		t.Fatalf("unowned workspace was removed: %v", err)
	}
}

func TestCleanupRejectsReplacedSourceRepository(t *testing.T) {
	source := newGitRepository(t)
	originalParent := filepath.Dir(source)
	root := filepath.Join(t.TempDir(), "workspaces")
	s := config.Settings{Workspace: config.Workspace{Root: root, SourceRoot: source}}
	l := New(func() config.Settings { return s })
	issue := domain.Issue{ID: "issue-17", Identifier: "PMR-17"}
	ws, err := l.Prepare(context.Background(), issue)
	if err != nil {
		t.Fatal(err)
	}
	preserved := filepath.Join(originalParent, "preserved-source")
	if err := os.Rename(source, preserved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(source, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, source, "init")
	if _, err := l.Cleanup(context.Background(), issue); err == nil || !strings.Contains(err.Error(), "different Git repository") {
		t.Fatalf("Cleanup error=%v", err)
	}
	if _, err := os.Stat(ws.Path); err != nil {
		t.Fatalf("workspace was removed through replaced source: %v", err)
	}
	if _, err := os.Stat(filepath.Join(source, ".git")); err != nil {
		t.Fatalf("replacement repository was mutated: %v", err)
	}
}

func TestAfterCreateDiagnosticsAreBounded(t *testing.T) {
	root := filepath.Join(t.TempDir(), "workspaces")
	script := `(yes stdout | head -c 40000); (yes stderr | head -c 40000 >&2); exit 1`
	s := config.Settings{Workspace: config.Workspace{Root: root}, Hooks: config.Hooks{AfterCreate: script}}
	l := New(func() config.Settings { return s })
	issue := domain.Issue{ID: "issue-17", Identifier: "PMR-17"}
	_, err := l.Prepare(context.Background(), issue)
	if err == nil {
		t.Fatal("Prepare unexpectedly succeeded")
	}
	if len(err.Error()) > 2*maxHookOutput+1024 {
		t.Fatalf("hook error is not bounded: %d bytes", len(err.Error()))
	}
	if !strings.Contains(err.Error(), "[truncated]") {
		t.Fatalf("hook error lacks truncation marker: %v", err)
	}
}

// TestNoHostCredentialReachesAHook is the hook's share of the guarantee
// config.ReservedSecretEnvNames documents, and the counterpart of
// TestNoHostCredentialReachesTheChildEnvironment in internal/codex and
// TestHostSecretsNeverReachTheChild in internal/claude. A hook is the third
// child Symphony spawns, and until PMR-113 it was the one that ran with the
// daemon's environment whole -- in the agent's own worktree, where it can invoke
// anything the agent committed.
//
// It runs a real after_create hook through Prepare rather than calling hook
// directly, because what is proven here is that the launcher reaches the filter
// at all; what the filter then removes is proven once, over hostenv.Filter.
//
// The reserved names are written out rather than read from
// config.ReservedSecretEnvNames, deliberately: a test that iterates the list
// asserts nothing about its contents, and dropping an entry would leave it
// green.
func TestNoHostCredentialReachesAHook(t *testing.T) {
	// Filter 1: the reserved names, whatever the workflow configures. Each value
	// is unique and is matched by no other filter, so only the name can remove
	// it.
	reserved := map[string]string{
		"LINEAR_API_KEY":               "reserved-linear-key-value",
		"SYMPHONY_LINEAR_API_KEY_FILE": "/private/reserved-linear-key-path",
		"GITHUB_TOKEN":                 "reserved-forge-token-value",
		"SYMPHONY_GITHUB_TOKEN":        "reserved-symphony-forge-token-value",
		"SYMPHONY_GITHUB_TOKEN_FILE":   "/private/reserved-forge-token-path",
	}
	for name, value := range reserved {
		t.Setenv(name, value)
	}
	// Filter 2: configured names, including the credential *file path* form no
	// value filter can see. Filter 3: a configured credential inside the value
	// of a variable no list mentions. Filter 4 has no case here: a hook has no
	// session to build a matcher from, which is the documented shape of this
	// caller rather than a gap in the test.
	t.Setenv("PMR113_CONFIGURED_NAME", "configured-name-value")
	t.Setenv("PMR113_PADDED_NAME", "padded-name-value")
	t.Setenv("PMR113_CONFIGURED_FILE", "/private/configured-key-path")
	t.Setenv("PMR113_INHERITED_CONFIGURED", "Bearer configured-secret-value")
	t.Setenv("PMR113_KEPT", "ordinary-value")
	// Inherited variables under the two names Symphony sets itself: the hook
	// must be told about this run's issue, never about whatever the host
	// exported under the same name.
	t.Setenv(issueIDEnvName, "inherited-issue-id")
	t.Setenv(issueIdentifierEnvName, "inherited-identifier")

	root := filepath.Join(t.TempDir(), "workspaces")
	environment := filepath.Join(t.TempDir(), "environment")
	s := config.Settings{
		Workspace: config.Workspace{Root: root},
		Hooks:     config.Hooks{AfterCreate: "env > " + environment},
		// The padded and blank names are hostenv.Filter's, and are here because
		// this test runs the real hook: a Settings that never went through
		// config.Load can carry either, and the launcher must reach the filter
		// that handles them.
		HostSecretEnvNames: []string{"PMR113_CONFIGURED_NAME", "  PMR113_PADDED_NAME  ", "   ", "PMR113_CONFIGURED_FILE"},
		HostSecretValues:   []string{"configured-secret-value"},
	}
	l := New(func() config.Settings { return s })
	issue := domain.Issue{ID: "issue-113", Identifier: "PMR-113"}
	if _, err := l.Prepare(context.Background(), issue); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(environment)
	if err != nil {
		t.Fatal(err)
	}
	child := string(data)
	names := hookEnvironmentNames(child)
	for name, value := range reserved {
		if slices.Contains(names, name) {
			t.Fatalf("hook environment retained reserved variable %s", name)
		}
		if strings.Contains(child, value) {
			t.Fatalf("hook environment retained the value of reserved variable %s", name)
		}
	}
	for _, leaked := range []string{"configured-name-value", "padded-name-value",
		"/private/configured-key-path", "configured-secret-value",
		"inherited-issue-id", "inherited-identifier"} {
		if strings.Contains(child, leaked) {
			t.Fatalf("hook environment retained %q", leaked)
		}
	}
	if !strings.Contains(child, "ordinary-value") {
		t.Fatal("the host credential filter removed unrelated variables")
	}
	// The hook still gets what it is for. Without this the filter could pass by
	// handing every hook an empty environment.
	for _, want := range []string{issueIDEnvName + "=" + issue.ID, issueIdentifierEnvName + "=" + issue.Identifier} {
		if !slices.Contains(strings.Split(child, "\n"), want) {
			t.Fatalf("hook environment lacks %q", want)
		}
	}
}

// hookEnvironmentNames reads the variable names out of `env` output, so an
// absence assertion on one name cannot be satisfied by another name that merely
// ends with it -- GITHUB_TOKEN inside SYMPHONY_GITHUB_TOKEN, for example.
//
// Only a line whose prefix is shaped like a variable name counts, because a
// developer or CI machine can hold a multi-line value whose continuation lines
// would otherwise read as names and produce a confusing failure.
func hookEnvironmentNames(child string) []string {
	var names []string
	for _, line := range strings.Split(child, "\n") {
		if name, _, found := strings.Cut(line, "="); found && environmentName(name) {
			names = append(names, name)
		}
	}
	return names
}

func environmentName(candidate string) bool {
	if candidate == "" {
		return false
	}
	for i, r := range candidate {
		digit := r >= '0' && r <= '9'
		if !(r == '_' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || digit && i > 0) {
			return false
		}
	}
	return true
}

func TestAfterCreateCannotCreateUnclassifiableGitRepository(t *testing.T) {
	root := filepath.Join(t.TempDir(), "workspaces")
	s := config.Settings{Workspace: config.Workspace{Root: root}, Hooks: config.Hooks{AfterCreate: "git init -q"}}
	l := New(func() config.Settings { return s })
	issue := domain.Issue{ID: "issue-17", Identifier: "PMR-17"}
	if _, err := l.Prepare(context.Background(), issue); err == nil || !strings.Contains(err.Error(), "without a source-worktree identity") {
		t.Fatalf("Prepare error=%v", err)
	}
	if _, err := os.Stat(filepath.Join(root, Key(issue.Identifier))); !os.IsNotExist(err) {
		t.Fatalf("unclassifiable Git repository remains: %v", err)
	}
}

// TestNarrowedGrantSufficesForDetachedCommitAndProtectsSource proves the two
// halves of the PMR-65 fix on a real repository: a detached-HEAD add+commit in
// the linked worktree needs only the shared object store and the worktree's own
// metadata directory (it succeeds and moves the worktree HEAD), and it writes
// nothing outside those two granted roots -- in particular it leaves every
// source branch head and the primary index byte-for-byte unchanged.
func TestNarrowedGrantSufficesForDetachedCommitAndProtectsSource(t *testing.T) {
	source := newGitRepository(t)
	root := filepath.Join(t.TempDir(), "workspaces")
	s := config.Settings{Workspace: config.Workspace{Root: root, SourceRoot: source}}
	l := New(func() config.Settings { return s })
	issue := domain.Issue{ID: "1", Identifier: "PMR-1"}
	ws, err := l.Prepare(context.Background(), issue)
	if err != nil {
		t.Fatal(err)
	}
	state, _, err := l.loadState(issue)
	if err != nil {
		t.Fatal(err)
	}
	objects, err := canonicalExistingDirectory(filepath.Join(state.GitCommonDir, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	// Fingerprint everything under the common directory except the two granted
	// roots (the object store and this worktree's own metadata directory). A
	// detached-HEAD commit that only needs those roots must leave this set of
	// files unchanged.
	granted := []string{objects, filepath.Clean(state.GitWorktreeDir)}
	before := snapshotCommonDirExcept(t, state.GitCommonDir, granted)
	baseHead := gitShow(t, ws.Path, "HEAD")

	if err := os.WriteFile(filepath.Join(ws.Path, "worktree.txt"), []byte("agent change\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, ws.Path, "add", "worktree.txt")
	runGit(t, ws.Path, "commit", "-m", "agent commit on detached HEAD")

	newHead := gitShow(t, ws.Path, "HEAD")
	if newHead == baseHead {
		t.Fatalf("detached-HEAD commit did not advance the worktree HEAD (%s)", newHead)
	}
	if out, err := exec.Command("git", "-C", ws.Path, "symbolic-ref", "-q", "HEAD").CombinedOutput(); err == nil {
		t.Fatalf("worktree left detached state and is on branch %q", strings.TrimSpace(string(out)))
	}
	after := snapshotCommonDirExcept(t, state.GitCommonDir, granted)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("detached-HEAD commit wrote outside the granted roots\nbefore=%v\nafter=%v", before, after)
	}
	if want := gitShow(t, source, "HEAD"); want != baseHead {
		t.Fatalf("source HEAD moved from %s to %s during a detached worktree commit", baseHead, want)
	}
}

// TestSourceIntegritySnapshotIgnoresSymphonyBranches unit-tests the PMR-65
// backstop's capture primitive: the snapshot excludes the symphony/* publish
// branches Symphony itself creates, but records any other branch head.
func TestSourceIntegritySnapshotIgnoresSymphonyBranches(t *testing.T) {
	source := newGitRepository(t)
	ctx := context.Background()
	base, err := captureSourceIntegrity(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	runGit(t, source, "branch", "symphony/pmr-1")
	afterSymphony, err := captureSourceIntegrity(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(afterSymphony.Refs, base.Refs) {
		t.Fatalf("symphony/* branch changed the snapshot: got=%v base=%v", afterSymphony.Refs, base.Refs)
	}
	runGit(t, source, "branch", "feature")
	afterFeature, err := captureSourceIntegrity(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	if reflect.DeepEqual(afterFeature.Refs, base.Refs) {
		t.Fatalf("a new non-symphony branch head was not recorded: %v", afterFeature.Refs)
	}
}

// TestDiffSourceRefsExplainsOperatorFastForwardPulls proves the PMR-145 fix:
// an operator fast-forwarding a branch to a commit reachable from its
// remote-tracking ref -- the ordinary `git pull --ff-only` workflow -- is
// explained rather than flagged, while an arbitrary local write that does not
// land a commit any remote knows about still alerts.
func TestDiffSourceRefsExplainsOperatorFastForwardPulls(t *testing.T) {
	source := newGitRepository(t)
	ctx := context.Background()

	baseline, err := captureSourceIntegrity(ctx, source)
	if err != nil {
		t.Fatal(err)
	}

	// Simulate a second operator pushing to the remote, then this checkout
	// running `git pull --ff-only`: refs/heads/main moves forward to a commit
	// reachable from refs/remotes/origin/main.
	publisher := cloneRepository(t, source)
	if err := os.WriteFile(filepath.Join(publisher, "upstream.txt"), []byte("landed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, publisher, "add", "upstream.txt")
	runGit(t, publisher, "commit", "-m", "operator landed a PR")
	runGit(t, publisher, "push", "origin", "main")
	runGit(t, source, "pull", "--ff-only")

	pulled, err := captureSourceIntegrity(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	alerts, explained := diffSourceRefs(ctx, source, baseline.Refs, pulled.Refs)
	if len(alerts) != 0 {
		t.Fatalf("operator fast-forward pull was flagged as an alert: %+v", alerts)
	}
	if len(explained) != 1 || explained[0].Name != "refs/heads/main" {
		t.Fatalf("operator fast-forward pull was not explained: %+v", explained)
	}

	// Now simulate the breach the backstop exists to catch: main advances to a
	// commit no remote-tracking ref has ever heard of.
	if err := os.WriteFile(filepath.Join(source, "escape.txt"), []byte("agent write\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, source, "add", "escape.txt")
	runGit(t, source, "commit", "-m", "agent wrote the source repository")

	written, err := captureSourceIntegrity(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	alerts, explained = diffSourceRefs(ctx, source, pulled.Refs, written.Refs)
	if len(explained) != 0 {
		t.Fatalf("an unreachable local write was explained away: %+v", explained)
	}
	if len(alerts) != 1 || alerts[0].Name != "refs/heads/main" {
		t.Fatalf("an unreachable local write was not flagged: %+v", alerts)
	}
	if alerts[0].Before != pulled.Refs["refs/heads/main"] || alerts[0].After != written.Refs["refs/heads/main"] {
		t.Fatalf("alert did not name the before/after values: %+v", alerts[0])
	}
}

// TestDiffSourceRefsFailsClosedOnClassificationFailure proves the PMR-147 fix:
// when classifying a genuinely changed ref cannot be completed because a git
// subprocess call fails for a reason other than a negative ancestry answer --
// here, a baseline value that names no object in the repository, the same
// shape of failure a pruned or missing object would produce in production --
// the ref is still reported as an alert, naming the classification failure,
// rather than dropped the way an unrelated Warn would drop it.
func TestDiffSourceRefsFailsClosedOnClassificationFailure(t *testing.T) {
	source := newGitRepository(t)
	ctx := context.Background()

	current, err := captureSourceIntegrity(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	head := current.Refs["refs/heads/main"]
	if head == "" {
		t.Fatalf("expected refs/heads/main in snapshot: %+v", current.Refs)
	}
	baseline := map[string]string{"refs/heads/main": "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"}

	alerts, explained := diffSourceRefs(ctx, source, baseline, current.Refs)
	if len(explained) != 0 {
		t.Fatalf("an unclassifiable ref change was explained away: %+v", explained)
	}
	if len(alerts) != 1 || alerts[0].Name != "refs/heads/main" {
		t.Fatalf("an unclassifiable ref change was not flagged: %+v", alerts)
	}
	if alerts[0].Reason == "" {
		t.Fatalf("alert did not name the classification failure: %+v", alerts[0])
	}
	if alerts[0].Before != baseline["refs/heads/main"] || alerts[0].After != head {
		t.Fatalf("alert did not name the before/after values: %+v", alerts[0])
	}
}

// TestDiffSourceRefsTreatsNotAncestorAsNegativeAnswer proves that
// merge-base --is-ancestor's exit code 1 -- a legitimate negative answer, not
// a subprocess failure -- still alerts (the ref moved to a commit that is not
// a fast-forward) but without being mistaken for a classification failure: no
// Reason is set.
func TestDiffSourceRefsTreatsNotAncestorAsNegativeAnswer(t *testing.T) {
	source := newGitRepository(t)
	ctx := context.Background()

	baseline, err := captureSourceIntegrity(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	before := baseline.Refs["refs/heads/main"]

	// Move main sideways to a commit that shares no ancestry with before: a
	// hard reset onto an orphan commit is neither a fast-forward from before
	// nor an ancestor of it, so isAncestor(before, after) returns false, err
	// nil -- a genuine negative answer, not a failure.
	runGit(t, source, "checkout", "--orphan", "orphan")
	if err := os.WriteFile(filepath.Join(source, "orphan.txt"), []byte("orphan\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, source, "add", "orphan.txt")
	runGit(t, source, "commit", "-m", "orphan commit")
	runGit(t, source, "branch", "-f", "main", "orphan")
	runGit(t, source, "checkout", "main")
	runGit(t, source, "branch", "-D", "orphan")

	current, err := captureSourceIntegrity(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	after := current.Refs["refs/heads/main"]
	if after == before {
		t.Fatalf("main did not move: %s", after)
	}

	alerts, explained := diffSourceRefs(ctx, source, baseline.Refs, current.Refs)
	if len(explained) != 0 {
		t.Fatalf("an orphaned reset was explained away: %+v", explained)
	}
	if len(alerts) != 1 || alerts[0].Name != "refs/heads/main" {
		t.Fatalf("an orphaned reset was not flagged: %+v", alerts)
	}
	if alerts[0].Reason != "" {
		t.Fatalf("a legitimate not-an-ancestor answer was reported as a classification failure: %+v", alerts[0])
	}
}

// TestDiffSourceRefsAlertsOnDeletedRef proves a deleted source branch head
// always alerts: diffSourceRefs skips the fast-forward branch entirely when
// the ref no longer exists on the current side, so there is no path by which
// a deletion could be explained away.
func TestDiffSourceRefsAlertsOnDeletedRef(t *testing.T) {
	source := newGitRepository(t)
	ctx := context.Background()
	baseline := map[string]string{"refs/heads/main": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	current := map[string]string{}

	alerts, explained := diffSourceRefs(ctx, source, baseline, current)
	if len(explained) != 0 {
		t.Fatalf("a deleted ref was explained away: %+v", explained)
	}
	if len(alerts) != 1 || alerts[0].Name != "refs/heads/main" {
		t.Fatalf("a deleted ref was not flagged: %+v", alerts)
	}
	if alerts[0].Before != baseline["refs/heads/main"] || alerts[0].After != "" {
		t.Fatalf("alert did not name the before/after values: %+v", alerts[0])
	}
}

// TestDiffSourceRefsAlertsOnNewRef proves a brand-new source branch head
// always alerts: diffSourceRefs skips the fast-forward branch entirely when
// the ref did not exist on the baseline side, so there is no path by which a
// new ref could be explained away.
func TestDiffSourceRefsAlertsOnNewRef(t *testing.T) {
	source := newGitRepository(t)
	ctx := context.Background()
	baseline := map[string]string{}
	current := map[string]string{"refs/heads/feature": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}

	alerts, explained := diffSourceRefs(ctx, source, baseline, current)
	if len(explained) != 0 {
		t.Fatalf("a new ref was explained away: %+v", explained)
	}
	if len(alerts) != 1 || alerts[0].Name != "refs/heads/feature" {
		t.Fatalf("a new ref was not flagged: %+v", alerts)
	}
	if alerts[0].Before != "" || alerts[0].After != current["refs/heads/feature"] {
		t.Fatalf("alert did not name the before/after values: %+v", alerts[0])
	}
}

// TestSourceIntegrityAlertFiresOnClassificationFailure proves the PMR-147 fix
// end to end: when AfterRun's ref classification cannot be completed for a
// changed ref, the integrity alert still fires at Error and names the
// classification failure, rather than degrading to the Warn that used to
// drop the ref change entirely.
func TestSourceIntegrityAlertFiresOnClassificationFailure(t *testing.T) {
	source := newGitRepository(t)
	root := filepath.Join(t.TempDir(), "workspaces")
	s := config.Settings{Workspace: config.Workspace{Root: root, SourceRoot: source}}
	l := New(func() config.Settings { return s })
	var logs bytes.Buffer
	l.SetLogger(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	issue := domain.Issue{ID: "issue-147", Identifier: "PMR-147"}
	ws, err := l.Prepare(context.Background(), issue)
	if err != nil {
		t.Fatal(err)
	}
	if ws.GitIntegrityBaseline == "" {
		t.Fatal("expected Prepare to record an integrity baseline")
	}

	// Corrupt the recorded baseline so main's classification against the
	// unmodified current ref value cannot resolve the before commit: this
	// stands in for a pruned or missing object surfacing mid-run.
	var baseline sourceIntegritySnapshot
	if err := json.Unmarshal([]byte(ws.GitIntegrityBaseline), &baseline); err != nil {
		t.Fatal(err)
	}
	if _, ok := baseline.Refs["refs/heads/main"]; !ok {
		t.Fatalf("expected refs/heads/main in baseline: %+v", baseline.Refs)
	}
	baseline.Refs["refs/heads/main"] = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	corrupted, err := json.Marshal(baseline)
	if err != nil {
		t.Fatal(err)
	}
	ws.GitIntegrityBaseline = string(corrupted)

	// Move main so the ref is genuinely "changed" and classification is
	// attempted, rather than skipped as unchanged.
	if err := os.WriteFile(filepath.Join(source, "change.txt"), []byte("change\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, source, "add", "change.txt")
	runGit(t, source, "commit", "-m", "advance main")

	l.AfterRun(context.Background(), ws, issue)

	var record struct {
		Level       string `json:"level"`
		Msg         string `json:"msg"`
		ChangedRefs string `json:"changed_refs"`
	}
	found := false
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if line == "" {
			continue
		}
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		if record.Msg == "workspace source integrity alert" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("classification failure did not fire the integrity alert: %s", logs.String())
	}
	if record.Level != "ERROR" {
		t.Fatalf("integrity alert logged at %q, want ERROR", record.Level)
	}
	if !strings.Contains(record.ChangedRefs, "refs/heads/main") || !strings.Contains(record.ChangedRefs, "classification_failed=") {
		t.Fatalf("integrity alert did not name the classification failure: %q", record.ChangedRefs)
	}
}

// TestSourceIntegrityAlertIsStructuredAndNeverReachesStderr proves the PMR-65
// backstop alert lands in the operator log -- with the dedicated Operation and
// the issue attributes an operator needs to query for it -- instead of
// launchd's stderr file, which is not queryable and carries no issue
// attribution.
func TestSourceIntegrityAlertIsStructuredAndNeverReachesStderr(t *testing.T) {
	source := newGitRepository(t)
	root := filepath.Join(t.TempDir(), "workspaces")
	s := config.Settings{Workspace: config.Workspace{Root: root, SourceRoot: source}}
	l := New(func() config.Settings { return s })
	var logs bytes.Buffer
	l.SetLogger(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	issue := domain.Issue{ID: "issue-65", Identifier: "PMR-65"}
	ws, err := l.Prepare(context.Background(), issue)
	if err != nil {
		t.Fatal(err)
	}
	if ws.GitIntegrityBaseline == "" {
		t.Fatal("expected Prepare to record an integrity baseline")
	}
	// Simulate the breach the backstop exists to catch: a source branch head
	// moves during the run, despite the narrowed sandbox grant.
	runGit(t, source, "branch", "unexpected")

	stderrRead, stderrWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	origStderr := os.Stderr
	os.Stderr = stderrWrite
	l.AfterRun(context.Background(), ws, issue)
	os.Stderr = origStderr
	if err := stderrWrite.Close(); err != nil {
		t.Fatal(err)
	}
	leaked, err := io.ReadAll(stderrRead)
	if err != nil {
		t.Fatal(err)
	}
	if len(leaked) != 0 {
		t.Fatalf("integrity alert wrote to os.Stderr: %q", leaked)
	}

	var record struct {
		Level           string `json:"level"`
		Msg             string `json:"msg"`
		Operation       string `json:"operation"`
		IssueID         string `json:"issue_id"`
		IssueIdentifier string `json:"issue_identifier"`
		SourceRoot      string `json:"source_root"`
		ChangedRefs     string `json:"changed_refs"`
	}
	found := false
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if line == "" {
			continue
		}
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		if record.Msg == "workspace source integrity alert" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("integrity alert was not logged as a structured record: %s", logs.String())
	}
	if record.Level != "ERROR" {
		t.Fatalf("integrity alert logged at %q, want ERROR", record.Level)
	}
	if record.Operation != string(observability.OperationSourceIntegrityAlert) {
		t.Fatalf("integrity alert operation=%q, want %q", record.Operation, observability.OperationSourceIntegrityAlert)
	}
	if record.IssueID != issue.ID || record.IssueIdentifier != issue.Identifier {
		t.Fatalf("integrity alert missing issue attributes: %+v", record)
	}
	if record.SourceRoot == "" {
		t.Fatal("integrity alert missing source_root")
	}
	if !strings.Contains(record.ChangedRefs, "refs/heads/unexpected") || !strings.Contains(record.ChangedRefs, "(none)") {
		t.Fatalf("integrity alert did not name the changed ref with its before/after values: %q", record.ChangedRefs)
	}
}

// TestSourceIntegrityAlertIsSilentForOperatorFastForwardPulls proves the
// PMR-145 fix end to end: an operator running `git pull --ff-only` in the
// source checkout while a run is in flight -- the documented operator
// workflow, and the scenario this issue observed firing on every run -- does
// not trip the PMR-65 backstop.
func TestSourceIntegrityAlertIsSilentForOperatorFastForwardPulls(t *testing.T) {
	source := newGitRepository(t)
	root := filepath.Join(t.TempDir(), "workspaces")
	s := config.Settings{Workspace: config.Workspace{Root: root, SourceRoot: source}}
	l := New(func() config.Settings { return s })
	var logs bytes.Buffer
	l.SetLogger(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	issue := domain.Issue{ID: "issue-145", Identifier: "PMR-145"}
	ws, err := l.Prepare(context.Background(), issue)
	if err != nil {
		t.Fatal(err)
	}
	if ws.GitIntegrityBaseline == "" {
		t.Fatal("expected Prepare to record an integrity baseline")
	}

	// A second operator merges a PR while the run is in flight, and this
	// checkout runs its ordinary `git pull --ff-only`.
	publisher := cloneRepository(t, source)
	if err := os.WriteFile(filepath.Join(publisher, "landed.txt"), []byte("landed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, publisher, "add", "landed.txt")
	runGit(t, publisher, "commit", "-m", "operator landed a PR")
	runGit(t, publisher, "push", "origin", "main")
	runGit(t, source, "pull", "--ff-only")

	l.AfterRun(context.Background(), ws, issue)

	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if line == "" {
			continue
		}
		var record struct {
			Level string `json:"level"`
			Msg   string `json:"msg"`
		}
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		if record.Msg == "workspace source integrity alert" {
			t.Fatalf("operator fast-forward pull was reported as an integrity alert: %s", logs.String())
		}
	}
}

// TestHookDiagnosticsAreBoundedAndMaskedOnBothPaths proves observability.Text
// is applied once, at the source in hook, so a token-shaped credential in hook
// output is masked and the diagnostics are bounded to
// observability.MaxDiagnosticBytes on both the returned-error path
// (BeforeRun, which the coordinator logs) and the logged path (AfterRun,
// which this package logs directly).
func TestHookDiagnosticsAreBoundedAndMaskedOnBothPaths(t *testing.T) {
	root := filepath.Join(t.TempDir(), "workspaces")
	const secret = "token=super-secret-hook-value"
	script := fmt.Sprintf(`echo %s; (yes filler | head -c 4000); exit 1`, secret)
	s := config.Settings{Workspace: config.Workspace{Root: root}, Hooks: config.Hooks{BeforeRun: script, AfterRun: script}}
	l := New(func() config.Settings { return s })
	var logs bytes.Buffer
	l.SetLogger(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	issue := domain.Issue{ID: "issue-114", Identifier: "PMR-114"}
	ws, err := l.Prepare(context.Background(), issue)
	if err != nil {
		t.Fatal(err)
	}

	// The returned-error path: what the coordinator's BeforeRun failure carries.
	beforeRunErr := l.BeforeRun(context.Background(), ws, issue)
	if beforeRunErr == nil {
		t.Fatal("BeforeRun unexpectedly succeeded")
	}
	if strings.Contains(beforeRunErr.Error(), secret) {
		t.Fatalf("hook secret leaked through the returned error: %v", beforeRunErr)
	}
	if !strings.Contains(beforeRunErr.Error(), "[REDACTED]") {
		t.Fatalf("returned hook error was not masked: %v", beforeRunErr)
	}
	if got := len(beforeRunErr.Error()); got > observability.MaxDiagnosticBytes+256 {
		t.Fatalf("returned hook error is not bounded: %d bytes: %v", got, beforeRunErr)
	}

	// The logged path: AfterRun's hook failure never returns to a caller, it is
	// logged directly.
	l.AfterRun(context.Background(), ws, issue)
	if strings.Contains(logs.String(), secret) {
		t.Fatalf("hook secret leaked into the log: %s", logs.String())
	}
	if !strings.Contains(logs.String(), "[REDACTED]") {
		t.Fatalf("logged hook diagnostics were not masked: %s", logs.String())
	}
}

// snapshotCommonDirExcept fingerprints the contents of every file under root
// except those inside the excluded directories, so a test can assert that an
// operation wrote nothing outside the excluded (granted) roots.
func snapshotCommonDirExcept(t *testing.T, root string, excluded []string) map[string]string {
	t.Helper()
	root = filepath.Clean(root)
	snapshot := map[string]string{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		for _, dir := range excluded {
			if path == dir || strings.HasPrefix(path, filepath.Clean(dir)+string(filepath.Separator)) {
				if info.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		}
		if info.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		snapshot[rel] = fmt.Sprintf("%x", sum)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func gitShow(t *testing.T, dir string, rev string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "rev-parse", rev).CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse %s: %v: %s", rev, err, out)
	}
	return strings.TrimSpace(string(out))
}

func newGitRepository(t *testing.T) string {
	t.Helper()
	remote := filepath.Join(t.TempDir(), "remote.git")
	runGit(t, filepath.Dir(remote), "init", "--bare", remote)
	source := filepath.Join(t.TempDir(), "source")
	if err := os.Mkdir(source, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, source, "init")
	runGit(t, source, "config", "user.email", "test@example.invalid")
	runGit(t, source, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("source\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, source, "add", "README.md")
	runGit(t, source, "commit", "-m", "initial")
	runGit(t, source, "branch", "-M", "main")
	runGit(t, source, "remote", "add", "origin", remote)
	runGit(t, source, "push", "-u", "origin", "main")
	runGit(t, remote, "symbolic-ref", "HEAD", "refs/heads/main")
	return source
}

func cloneRepository(t *testing.T, source string) string {
	t.Helper()
	clone := filepath.Join(t.TempDir(), "clone")
	remote := gitOutput(t, source, "remote", "get-url", "origin")
	runGit(t, filepath.Dir(clone), "clone", remote, clone)
	runGit(t, clone, "config", "user.email", "test@example.invalid")
	runGit(t, clone, "config", "user.name", "Test")
	return clone
}

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
}
