package workspace

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/domain"
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
