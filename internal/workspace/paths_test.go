package workspace

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/domain"
)

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
