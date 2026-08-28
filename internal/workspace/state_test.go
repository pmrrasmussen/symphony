package workspace

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/domain"
)

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
