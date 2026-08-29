package workspace

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/domain"
)

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

// TestPrepareSerializesConcurrentBaseRefFetches asserts two workspaces
// created concurrently from one source repository never fetch the shared
// refs/remotes/origin/<base> at the same time, and that both preparations
// succeed. Without serialization, refs/remotes/origin/<base> and packed-refs
// live in the shared Git common directory rather than in either workspace, so
// two concurrent fetches can race that repository-wide ref and fail with
// "cannot lock ref ...: is at ... but expected ..." -- mirroring
// internal/github's TestRefreshBaseRefSerializesConcurrentFetches for the
// same invariant (PMR-162).
func TestPrepareSerializesConcurrentBaseRefFetches(t *testing.T) {
	source := newGitRepository(t)
	markDir := t.TempDir()
	installOverlapTrackingGit(t, markDir)

	root := filepath.Join(t.TempDir(), "workspaces")
	s := config.Settings{Workspace: config.Workspace{Root: root, SourceRoot: source}}
	l := New(func() config.Settings { return s })

	issues := []domain.Issue{{ID: "issue-1", Identifier: "PMR-1"}, {ID: "issue-2", Identifier: "PMR-2"}}
	errs := make([]error, len(issues))
	var wg sync.WaitGroup
	wg.Add(len(issues))
	for i, issue := range issues {
		i, issue := i, issue
		go func() {
			defer wg.Done()
			_, errs[i] = l.Prepare(context.Background(), issue)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("Prepare(%d) = %v, want both concurrent preparations to succeed", i, err)
		}
	}
	if _, err := os.Stat(filepath.Join(markDir, "overlap")); !os.IsNotExist(err) {
		t.Fatal("two base-ref fetches overlapped; addWorktree must serialize them")
	}
}

// installOverlapTrackingGit prepends a fake "git" to PATH that, for every
// invocation whose arguments include the "fetch" subcommand, holds a marker
// open for a short, deliberate window before delegating to the real git
// binary, then records to markDir/overlap if it ever saw another fetch's
// marker still open. Two goroutines calling addWorktree at once are actually
// likely to overlap in that window absent serialization.
func installOverlapTrackingGit(t *testing.T, markDir string) {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PMR162_MARK_DIR", markDir)
	t.Setenv("PMR162_REAL_GIT", realGit)
	binDir := t.TempDir()
	script := `#!/bin/sh
is_fetch=0
for a in "$@"; do
	if [ "$a" = "fetch" ]; then
		is_fetch=1
		break
	fi
done
if [ "$is_fetch" = "1" ]; then
	marker="$PMR162_MARK_DIR/active-$$"
	mkdir "$marker"
	count=$(ls "$PMR162_MARK_DIR" | grep -c '^active-')
	if [ "$count" -gt 1 ]; then
		: > "$PMR162_MARK_DIR/overlap"
	fi
	sleep 0.3
	rmdir "$marker"
fi
exec "$PMR162_REAL_GIT" "$@"
`
	path := filepath.Join(binDir, "git")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestNoHostCredentialReachesAWorkspaceGitChild is the git runners' share of the
// guarantee config.ReservedSecretEnvNames documents, and the counterpart of
// TestNoHostCredentialReachesAHook. Git is the fourth kind of child Symphony
// spawns and, until PMR-175, the one that ran with the daemon's environment
// whole: most of these runners are `git -C <agent worktree>` over state the
// agent wrote, and git reads repository configuration the agent can reach --
// core.fsmonitor in a per-worktree config, say -- so a host credential inherited
// here is one an agent-influenced subprocess can be made to carry.
//
// It runs a real Prepare, AfterRun, and Cleanup rather than calling a runner
// directly, because what is proven here is that the exec sites reach the filter
// at all; what the filter then removes is proven once, over hostenv.Filter.
// Between them those three reach every exec site in this package but one:
// gitRecordedMutation's `--git-dir` fallback runs only once the recorded source
// root is gone, and it sets cmd.Env from the same gitEnvironment as the sibling
// branch three lines above it.
//
// The reserved names are written out rather than read from
// config.ReservedSecretEnvNames, deliberately: a test that iterates the list
// asserts nothing about its contents, and dropping an entry would leave it
// green.
func TestNoHostCredentialReachesAWorkspaceGitChild(t *testing.T) {
	source := newGitRepository(t)
	root := filepath.Join(t.TempDir(), "workspaces")
	// Installed after the fixture repository is built, so the recording only ever
	// holds the environment of a child this package spawned.
	environment := recordGitChildEnvironments(t)

	// Filter 1: the reserved names, whatever the workflow configures. Each value
	// is unique and is matched by no other filter, so only the name can remove it.
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
	// Filter 2: a configured name, including the credential *file path* form no
	// value filter can see. Filter 3: a configured credential inside the value of
	// a variable no list mentions. Filter 4 has no case here: a git runner has no
	// session to build a matcher from, which is the documented shape of this
	// caller rather than a gap in the test.
	t.Setenv("PMR175_CONFIGURED_NAME", "configured-name-value")
	t.Setenv("PMR175_CONFIGURED_FILE", "/private/configured-key-path")
	t.Setenv("PMR175_INHERITED_CONFIGURED", "Bearer configured-secret-value")
	t.Setenv("PMR175_KEPT", "ordinary-value")

	s := config.Settings{
		Workspace:          config.Workspace{Root: root, SourceRoot: source},
		HostSecretEnvNames: []string{"PMR175_CONFIGURED_NAME", "PMR175_CONFIGURED_FILE"},
		HostSecretValues:   []string{"configured-secret-value"},
	}
	l := New(func() config.Settings { return s })
	issue := domain.Issue{ID: "issue-175", Identifier: "PMR-175"}
	ws, err := l.Prepare(context.Background(), issue)
	if err != nil {
		t.Fatal(err)
	}
	// Move a source branch so AfterRun's integrity classifier has a change to
	// classify: that is what reaches isAncestor and reachableFromRemote, the two
	// runners a source repository nobody touched never invokes. The move itself
	// runs git with an environment of its own, so this fixture step cannot put a
	// reserved variable into the recording and mask a leak.
	runGitWithoutHostEnvironment(t, source, "commit", "--allow-empty", "-m", "concurrent operator commit")
	runGitWithoutHostEnvironment(t, source, "push", "origin", "main")
	// AfterRun runs the integrity classifier over the source repository, and
	// Cleanup the worktree inspection and removal.
	l.AfterRun(context.Background(), ws, issue)
	if _, err := l.Cleanup(context.Background(), issue); err != nil {
		t.Fatal(err)
	}
	// Read before anything else in this test runs git again: from here on the
	// recording would also hold this test process's own unfiltered environment.
	data, err := os.ReadFile(environment)
	if err != nil {
		t.Fatal(err)
	}
	children := string(data)
	for name, value := range reserved {
		if slices.Contains(hookEnvironmentNames(children), name) {
			t.Fatalf("git child environment retained reserved variable %s", name)
		}
		if strings.Contains(children, value) {
			t.Fatalf("git child environment retained the value of reserved variable %s", name)
		}
	}
	for _, leaked := range []string{"configured-name-value", "/private/configured-key-path", "configured-secret-value"} {
		if strings.Contains(children, leaked) {
			t.Fatalf("git child environment retained %q", leaked)
		}
	}
	// Without this the filter could pass by handing every git child an empty
	// environment -- or by never being reached, if no child was recorded at all.
	if !strings.Contains(children, "PMR175_KEPT=ordinary-value") {
		t.Fatal("no git child inherited an unrelated variable; the recording proves nothing")
	}
}

// recordGitChildEnvironments prepends a fake "git" to PATH that appends its own
// environment to one file and then delegates to the real git binary, and returns
// that file's path. The paths are written into the script rather than passed
// through the environment, because the environment is what is under test: a
// variable the filter removed cannot also be the shim's way of finding the real
// git.
func recordGitChildEnvironments(t *testing.T) string {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	environment := filepath.Join(t.TempDir(), "git-environments")
	binDir := t.TempDir()
	script := fmt.Sprintf("#!/bin/sh\n/usr/bin/env >> %q\nexec %q \"$@\"\n", environment, realGit)
	if err := os.WriteFile(filepath.Join(binDir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return environment
}

// runGitWithoutHostEnvironment runs one fixture git command with only the
// variables git needs to run at all, so a test recording git child environments
// can distinguish what this package's runners passed from what the test process
// itself holds.
func runGitWithoutHostEnvironment(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
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
