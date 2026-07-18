package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pmrrasmussen/symphony/internal/domain"
)

func TestLoadExpandsAndNormalizes(t *testing.T) {
	d := t.TempDir()
	t.Setenv("LINEAR_API_KEY", "secret")
	p := filepath.Join(d, "WORKFLOW.md")
	if err := os.WriteFile(p, []byte("---\ntracker:\n  kind: linear\n  provider: {api_key: $LINEAR_API_KEY}\n  active_states: [Todo]\n  terminal_states: [Done]\nworkspace: {root: work}\n---\nHello {{.issue.identifier}}"), 0o600); err != nil {
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
	if got, err := w.Config.Render(domain.Issue{Identifier: "PMR-7"}, 0); err != nil || got != "Hello PMR-7" {
		t.Fatalf("render=%q err=%v", got, err)
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

func TestLoadRejectsMalformedKnownFieldsButPreservesExtensions(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "WORKFLOW.md")
	if err := os.WriteFile(p, []byte("---\nextension: {enabled: true}\ntracker: {kind: linear, provider: {future_key: [one, two]}, active_states: [Todo], terminal_states: [Done]}\nworkspace: {root: work}\n---\n{{.issue.identifier}}"), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := Load(p, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := w.Raw["extension"]; !ok {
		t.Fatal("unknown top-level extension was not preserved")
	}
	if got := w.Config.Tracker.Provider["future_key"]; got == nil {
		t.Fatal("unknown provider key was not preserved")
	}
	for _, malformed := range []string{
		"tracker: []\n",
		"tracker: {kind: linear, active_states: Todo, terminal_states: [Done]}\n",
		"tracker: {kind: linear, active_states: [Todo], terminal_states: [Done]}\npolling: {interval_ms: 1.5}\n",
		"tracker: {kind: linear, active_states: [Todo], terminal_states: [Done]}\nworkspace: {root: 42}\n",
		"tracker: {kind: linear, active_states: [Todo], terminal_states: [Done]}\nagent: {max_concurrent_agents_by_state: 3}\n",
	} {
		if err := os.WriteFile(p, []byte("---\n"+malformed+"---\nok"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(p, ""); err == nil {
			t.Fatalf("Load accepted malformed configuration: %q", malformed)
		}
	}
}

func TestParseRejectsNonMapFrontMatter(t *testing.T) {
	for _, document := range []string{"---\n- not\n- a-map\n---\nprompt", "---\ntrue\n---\nprompt"} {
		_, _, err := parse([]byte(document))
		if !strings.Contains(err.Error(), "workflow_front_matter_not_a_map") {
			t.Fatalf("parse(%q) error=%v", document, err)
		}
		if err == nil {
			t.Fatalf("parse(%q) unexpectedly succeeded", document)
		}
	}
}

func TestDefaultsAndPathReferencesAreDeterministic(t *testing.T) {
	d := t.TempDir()
	t.Setenv("SYMPHONY_TEST_ROOT", "custom-root")
	t.Setenv("SYMPHONY_TEST_SECRET", "from-env")
	p := filepath.Join(d, "WORKFLOW.md")
	content := "---\ntracker: {kind: linear, provider: {api_key: $SYMPHONY_TEST_SECRET, literal: prefix-$SYMPHONY_TEST_SECRET}, active_states: [ Todo ], terminal_states: [Done]}\nworkspace: {root: $SYMPHONY_TEST_ROOT, source_root: \"~\"}\n---\n"
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := Load(p, "relative-logs")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(d, "custom-root"); w.Config.Workspace.Root != want {
		t.Fatalf("workspace root=%q want %q", w.Config.Workspace.Root, want)
	}
	if !filepath.IsAbs(w.Config.Workspace.SourceRoot) {
		t.Fatalf("home path not normalized: %q", w.Config.Workspace.SourceRoot)
	}
	if w.Config.Tracker.Provider["api_key"] != "from-env" || w.Config.Tracker.Provider["literal"] != "prefix-$SYMPHONY_TEST_SECRET" {
		t.Fatalf("unexpected provider environment handling: %#v", w.Config.Tracker.Provider)
	}
	if want := filepath.Join(d, "relative-logs"); w.Config.LogRoot != want {
		t.Fatalf("log root=%q want %q", w.Config.LogRoot, want)
	}
	if err := os.WriteFile(p, []byte("---\ntracker: {kind: linear, active_states: [Todo], terminal_states: [Done]}\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err = Load(p, "")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(os.TempDir(), "symphony_workspaces"); w.Config.Workspace.Root != want {
		t.Fatalf("default root=%q want %q", w.Config.Workspace.Root, want)
	}
}

func TestEmptyRequiredPathReferenceIsRejected(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "WORKFLOW.md")
	content := "---\ntracker: {kind: linear, active_states: [Todo], terminal_states: [Done]}\nworkspace: {root: $UNSET_SYMPHONY_TEST_ROOT}\n---\nprompt"
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p, ""); err == nil || !strings.Contains(err.Error(), "root must not be empty") {
		t.Fatalf("empty workspace root error=%v", err)
	}
}

func TestEmptyAPIKeyReferenceRejectsReloadAndPreservesLastGoodWorkflow(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "WORKFLOW.md")
	t.Setenv("SYMPHONY_TEST_GOOD_KEY", "test-secret")
	good := "---\ntracker: {kind: linear, provider: {api_key: $SYMPHONY_TEST_GOOD_KEY}, active_states: [Todo], terminal_states: [Done]}\n---\ngood"
	if err := os.WriteFile(p, []byte(good), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := NewStore(p, "")
	if err != nil {
		t.Fatal(err)
	}
	invalid := "---\ntracker: {kind: linear, provider: {api_key: $UNSET_SYMPHONY_TEST_KEY}, active_states: [Todo], terminal_states: [Done]}\n---\nbad"
	if err := os.WriteFile(p, []byte(invalid), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Reload(); err == nil || !strings.Contains(err.Error(), "resolved secret is empty") {
		t.Fatalf("empty key reload error=%v", err)
	}
	current := s.Current()
	if current.Prompt != "good" || current.Config.Tracker.Provider["api_key"] != "test-secret" {
		t.Fatal("invalid reload replaced the last known good workflow")
	}
}

func TestAPIKeyFileTakesPrecedenceOverInlineReference(t *testing.T) {
	d := t.TempDir()
	secret := filepath.Join(d, "linear-key")
	if err := os.WriteFile(secret, []byte("file-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(d, "WORKFLOW.md")
	content := "---\ntracker: {kind: linear, provider: {api_key: $UNSET_SYMPHONY_TEST_KEY, api_key_file: linear-key}, active_states: [Todo], terminal_states: [Done]}\n---\nprompt"
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := Load(p, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := w.Config.Tracker.Provider["api_key"]; got != "file-secret" {
		t.Fatalf("file key precedence=%q", got)
	}
}

func TestTemplateErrorsAreDeferredAndPromptContractIsLowercase(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "WORKFLOW.md")
	base := "---\ntracker: {kind: linear, active_states: [Todo], terminal_states: [Done]}\n---\n"
	if err := os.WriteFile(p, []byte(base+"{{.issue.identifier}} retry={{.attempt}}"), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := Load(p, "")
	if err != nil {
		t.Fatal(err)
	}
	first, err := w.Config.Render(domain.Issue{Identifier: "PMR-7"}, 0)
	if err != nil || first != "PMR-7 retry=<no value>" {
		t.Fatalf("first render=%q err=%v", first, err)
	}
	retry, err := w.Config.Render(domain.Issue{Identifier: "PMR-7"}, 1)
	if err != nil || retry != "PMR-7 retry=1" {
		t.Fatalf("retry render=%q err=%v", retry, err)
	}
	if err := os.WriteFile(p, []byte(base+"{{.issue.unknown}}"), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err = Load(p, "")
	if err != nil {
		t.Fatalf("template parse/render errors must not fail Load: %v", err)
	}
	if _, err := w.Config.Render(domain.Issue{}, 0); err == nil || !strings.Contains(err.Error(), "template_render_error") {
		t.Fatalf("unknown variable error=%v", err)
	}
	if err := os.WriteFile(p, []byte(base+"{{.Issue.Identifier}}"), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err = Load(p, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Config.Render(domain.Issue{Identifier: "PMR-7"}, 0); err == nil || !strings.Contains(err.Error(), "template_render_error") {
		t.Fatalf("legacy uppercase contract was accepted: %v", err)
	}
	if err := os.WriteFile(p, []byte(base+"{{.issue.identifier"), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err = Load(p, "")
	if err != nil {
		t.Fatalf("template parse errors must not fail Load: %v", err)
	}
	if _, err := w.Config.Render(domain.Issue{}, 0); err == nil || !strings.Contains(err.Error(), "template_parse_error") {
		t.Fatalf("invalid template error=%v", err)
	}
}

func TestEmptyPromptUsesFallback(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "WORKFLOW.md")
	if err := os.WriteFile(p, []byte("---\ntracker: {kind: linear, active_states: [Todo], terminal_states: [Done]}\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := Load(p, "")
	if err != nil {
		t.Fatal(err)
	}
	got, err := w.Config.Render(domain.Issue{Identifier: "PMR-7", Title: "Workflow", Description: "Contract"}, 0)
	if err != nil || got != "Work on PMR-7: Workflow\n\nContract" {
		t.Fatalf("fallback render=%q err=%v", got, err)
	}
}

func TestReloadAppliesValidChangesAndRetainsLastValidWorkflow(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "WORKFLOW.md")
	good := "---\ntracker: {kind: linear, active_states: [Todo], terminal_states: [Done]}\npolling: {interval_ms: 100}\n---\n{{.issue.identifier}}"
	if err := os.WriteFile(p, []byte(good), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := NewStore(p, "")
	if err != nil {
		t.Fatal(err)
	}
	updated := "---\ntracker: {kind: linear, active_states: [Todo], terminal_states: [Done]}\npolling: {interval_ms: 200}\n---\nupdated {{.issue.title}}"
	if err := os.WriteFile(p, []byte(updated), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Reload(); err != nil {
		t.Fatal(err)
	}
	if got := s.Current().Config.Polling.Interval; got != 200*time.Millisecond {
		t.Fatalf("reload interval=%v", got)
	}
	if err := os.WriteFile(p, []byte("---\ntracker: []\n---\nbad"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Reload(); err == nil {
		t.Fatal("invalid reload succeeded")
	}
	if got := s.Current().Prompt; got != "updated {{.issue.title}}" {
		t.Fatalf("last valid prompt=%q", got)
	}
}

func TestHandoffPolicyIsOptInAndInvalidReloadRetainsLastKnownGood(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "WORKFLOW.md")
	good := "---\ntracker: {kind: linear, provider: {handoff_state: \"In Review\", handoff_comment_template: \"Ready: {{.issue.identifier}}\"}, active_states: [Todo, In Progress], terminal_states: [Done]}\n---\nprompt"
	if err := os.WriteFile(p, []byte(good), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := NewStore(p, "")
	if err != nil {
		t.Fatal(err)
	}
	current := s.Current().Config.Tracker
	if current.HandoffState != "In Review" || current.HandoffCommentTemplate == "" {
		t.Fatalf("handoff policy=%+v", current)
	}
	for _, invalid := range []string{
		"tracker: {kind: linear, provider: {handoff_state: Todo}, active_states: [Todo], terminal_states: [Done]}",
		"tracker: {kind: linear, provider: {handoff_state: Done}, active_states: [Todo], terminal_states: [Done]}",
		"tracker: {kind: linear, provider: {handoff_comment_template: comment}, active_states: [Todo], terminal_states: [Done]}",
		"tracker: {kind: linear, provider: {handoff_state: \"In Review\", handoff_comment_template: \"{{.issue\"}, active_states: [Todo], terminal_states: [Done]}",
	} {
		if err := os.WriteFile(p, []byte("---\n"+invalid+"\n---\nprompt"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := s.Reload(); err == nil {
			t.Fatalf("invalid handoff policy reloaded: %s", invalid)
		}
		if got := s.Current().Config.Tracker.HandoffState; got != "In Review" {
			t.Fatalf("invalid reload replaced handoff policy: %q", got)
		}
	}
}

func TestRenderHandoffCommentUsesOnlyRepositoryPolicy(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "WORKFLOW.md")
	content := "---\ntracker: {kind: linear, provider: {handoff_state: \"In Review\", handoff_comment_template: \"Handoff {{.issue.identifier}}: {{.issue.title}}\"}, active_states: [Todo], terminal_states: [Done]}\n---\nprompt"
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := Load(p, "")
	if err != nil {
		t.Fatal(err)
	}
	got, err := w.Config.RenderHandoffComment(domain.Issue{Identifier: "PMR-5", Title: "Handoff"})
	if err != nil || got != "Handoff PMR-5: Handoff" {
		t.Fatalf("comment=%q err=%v", got, err)
	}
}
