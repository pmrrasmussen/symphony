package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/domain"
)

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
