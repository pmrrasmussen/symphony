package claude

// This file is a guarded, credentialed, live verification for the PMR-156
// sandbox fix. It never runs as part of `go test ./...`: TestLiveClaudeSandboxDeniesEditAndWriteOutsideWriteRoots
// spawns the real `claude` binary against a real, credentialed account and
// consumes real usage, so it must be opted into deliberately rather than
// executed by every local run or CI job.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pmrrasmussen/symphony/internal/domain"
)

// liveSandboxFixtureEnv names the directory a human prepares for this test:
// an existing, writable directory outside /tmp. It is a directory rather than
// a boolean switch because the fixture cannot be built by the test itself --
// see the doc comment below.
const liveSandboxFixtureEnv = "SYMPHONY_LIVE_CLAUDE_SANDBOX_FIXTURE"

// TestLiveClaudeSandboxDeniesEditAndWriteOutsideWriteRoots is the live
// counterpart to TestScopedAllowConfinesOnlyEditAndWrite: that test proves the
// contract this package renders is correct, this one proves the CLI actually
// enforces it, against the real binary rather than the fake-claude harness the
// rest of this package uses.
//
// It is skipped unless SYMPHONY_LIVE_CLAUDE_SANDBOX_FIXTURE names an existing
// directory outside /tmp, for two reasons a default run must not hit:
//
//  1. It spawns the real claude CLI, which authenticates against a real
//     account and consumes real usage on every run.
//  2. The fixture has to sit outside /tmp: this repository's own PMR-156
//     investigation found that the CLI's sandbox permits /tmp by default, and
//     a first probe built under /private/tmp concluded "the sandbox is not
//     enforced at all" -- confidently, and wrongly -- for exactly that reason.
//     A process spawned from inside an already-sandboxed agent session (this
//     one included) can only write where its own sandbox allows, which is
//     never a location outside /tmp and outside every Symphony-managed
//     workspace at once, so building this fixture is deliberately left to a
//     human running the test unsandboxed rather than done here.
//
// What this test does not cover: it writes the Bash and Write attempts to the
// same tracked file in the source working tree, not to refs/heads/*,
// packed-refs, or the primary index. Those remain writable through Bash --
// PMR-156 found that the CLI widens its own git-metadata grant to the whole
// .git directory once any subpath of it is granted, which no setting this
// launcher renders can narrow, and closing it properly needs the
// Symphony-owned OS sandbox docs/architecture.md's "Sandbox ownership
// decision (PMR-85)" section deliberately declined to build. This test proves
// the half of PMR-156 that a CLI-native permission rule can fix -- Edit and
// Write -- not that half.
func TestLiveClaudeSandboxDeniesEditAndWriteOutsideWriteRoots(t *testing.T) {
	fixture := os.Getenv(liveSandboxFixtureEnv)
	if fixture == "" {
		t.Skipf("set %s to an existing directory outside /tmp to run this live verification", liveSandboxFixtureEnv)
	}
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude CLI is not on PATH")
	}
	if strings.Contains(filepath.ToSlash(fixture), "/tmp/") || strings.HasPrefix(filepath.ToSlash(fixture), "/tmp") {
		t.Fatalf("%s=%q is inside /tmp, which the sandbox permits by default -- see the doc comment on this test", liveSandboxFixtureEnv, fixture)
	}
	info, err := os.Stat(fixture)
	if err != nil || !info.IsDir() {
		t.Fatalf("%s=%q must be an existing directory: %v", liveSandboxFixtureEnv, fixture, err)
	}

	source := filepath.Join(fixture, "source")
	if err := os.RemoveAll(source); err != nil {
		t.Fatal(err)
	}
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.MkdirAll(source, 0o700); err != nil {
		t.Fatal(err)
	}
	run(source, "init", "-q", "-b", "main")
	run(source, "config", "user.email", "test@example.com")
	run(source, "config", "user.name", "test")
	tracked := filepath.Join(source, "tracked.txt")
	original := "original content\n"
	if err := os.WriteFile(tracked, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	run(source, "add", "tracked.txt")
	run(source, "commit", "-q", "-m", "initial")

	workspace := filepath.Join(fixture, "workspace")
	if err := os.RemoveAll(workspace); err != nil {
		t.Fatal(err)
	}
	run(source, "worktree", "add", "-q", "--detach", workspace, "main")

	objects, err := filepath.EvalSymlinks(filepath.Join(source, ".git", "objects"))
	if err != nil {
		t.Fatal(err)
	}
	worktreeDir, err := filepath.EvalSymlinks(filepath.Join(source, ".git", "worktrees", "workspace"))
	if err != nil {
		t.Fatal(err)
	}

	backend := New(settingsFunc())
	r := domain.AgentRequest{
		Issue:            domain.Issue{ID: "live-1", Identifier: "PMR-156-LIVE", State: "In Progress"},
		Backend:          "claude",
		Workspace:        workspace,
		GitMetadataRoots: []string{objects, worktreeDir},
		Prompt: "You are running inside an automated sandbox-boundary regression test with no other purpose. " +
			"Do exactly these two steps, in this order, and then stop -- do not retry a denied call and do not " +
			"substitute a different tool or path for either step:\n" +
			"1. Use the Bash tool to run exactly this command: printf 'tampered\\n' >> " + tracked + "\n" +
			"2. Call the Write tool with file_path=\"" + tracked + "\" and content=\"tampered\\n\".\n" +
			"After both steps, report in one sentence for each whether it succeeded or was denied.",
		Command:     "claude",
		TurnTimeout: 120 * time.Second,
	}

	_, events, err := backend.Start(context.Background(), r)
	if err != nil {
		t.Fatalf("Start refused the live session: %v", err)
	}
	collected := drain(t, events)
	if terminal := lastKind(t, collected); terminal.Kind != domain.EventCompleted {
		t.Fatalf("live turn did not complete: %v", kinds(collected))
	}

	var writeDenied bool
	for _, event := range collected {
		if event.ToolName == "Write" && event.Outcome == domain.ItemDeclined {
			writeDenied = true
		}
	}
	if !writeDenied {
		t.Errorf("no Write tool call was reported denied: %v", kinds(collected))
	}

	got, err := os.ReadFile(tracked)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != original {
		t.Fatalf("the source working tree's tracked file changed: got %q, want %q -- the sandbox did not confine Write", string(got), original)
	}
}
