package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadExpandsAndNormalizes(t *testing.T) {
	d := t.TempDir()
	t.Setenv("LINEAR_API_KEY", "secret")
	p := filepath.Join(d, "WORKFLOW.md")
	if err := os.WriteFile(p, []byte("---\ntracker:\n  kind: linear\n  provider: {api_key: $LINEAR_API_KEY}\n  active_states: [Todo]\n  terminal_states: [Done]\nworkspace: {root: work}\n---\nHello {{.Issue.Identifier}}"), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := Load(p, "")
	if err != nil {
		t.Fatal(err)
	}
	if w.Config.Workspace.Root != filepath.Join(d, "work") {
		t.Fatalf("root=%s", w.Config.Workspace.Root)
	}
	if w.Config.Tracker.Provider["api_key"] != "secret" {
		t.Fatal("env not resolved")
	}
}
func TestInvalidReloadKeepsLastValid(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "WORKFLOW.md")
	good := []byte("---\ntracker: {kind: linear, active_states: [Todo], terminal_states: [Done]}\n---\nok")
	if err := os.WriteFile(p, good, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := NewStore(p, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("---\nbroken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Reload(); err == nil {
		t.Fatal("expected reload failure")
	}
	if s.Current().Prompt != "ok" {
		t.Fatal("last good workflow lost")
	}
}
