package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
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

func TestLoadPreservesLinearStateFilterSpelling(t *testing.T) {
	path := filepath.Join(t.TempDir(), "WORKFLOW.md")
	content := "---\ntracker: {kind: linear, active_states: [ Todo, In Progress ], terminal_states: [ Done ]}\n---\nprompt"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	workflow, err := Load(path, "")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := workflow.Config.Tracker.ActiveStates, []string{"Todo", "In Progress"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("active states=%v want %v", got, want)
	}
	if got, want := workflow.Config.Tracker.TerminalStates, []string{"Done"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("terminal states=%v want %v", got, want)
	}
}

func TestRequiredLabelsPreserveNormalizedBlankValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "WORKFLOW.md")
	content := "---\ntracker: {kind: linear, required_labels: [ Ready, '  ' ], active_states: [Todo], terminal_states: [Done]}\n---\nprompt"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	workflow, err := Load(path, "")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := workflow.Config.Tracker.RequiredLabels, []string{"ready", ""}; !reflect.DeepEqual(got, want) {
		t.Fatalf("required labels=%v want %v", got, want)
	}
}

func TestHostSecretEnvNamesIncludeCredentialReferencesEvenWhenGitHubIsDisabled(t *testing.T) {
	d := t.TempDir()
	linearFile := filepath.Join(d, "linear-key")
	githubFile := filepath.Join(d, "github-token")
	for path, value := range map[string]string{linearFile: "linear-file-secret", githubFile: "github-file-secret"} {
		if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PMR29_LINEAR_KEY", "linear-env-secret")
	t.Setenv("PMR29_LINEAR_FILE", linearFile)
	t.Setenv("PMR29_GITHUB_TOKEN", "github-env-secret")
	t.Setenv("PMR29_GITHUB_FILE", githubFile)
	workflow := filepath.Join(d, "WORKFLOW.md")
	content := "---\ntracker: {kind: linear, provider: {project_slug_id: project, api_key: $PMR29_LINEAR_KEY, api_key_file: $PMR29_LINEAR_FILE}, active_states: [Todo], terminal_states: [Done]}\ngithub: {token: $PMR29_GITHUB_TOKEN, token_file: $PMR29_GITHUB_FILE}\n---\nprompt"
	if err := os.WriteFile(workflow, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(workflow, "")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Config.GitHub.Enabled {
		t.Fatal("incomplete optional GitHub configuration was enabled")
	}
	want := []string{"PMR29_GITHUB_FILE", "PMR29_GITHUB_TOKEN", "PMR29_LINEAR_FILE", "PMR29_LINEAR_KEY"}
	if got := loaded.Config.HostSecretEnvNames; !reflect.DeepEqual(got, want) {
		t.Fatalf("secret environment names=%v want %v", got, want)
	}
	wantValues := []string{"github-file-secret", "linear-file-secret"}
	if got := loaded.Config.HostSecretValues; !reflect.DeepEqual(got, wantValues) {
		t.Fatalf("secret values=%v want %v", got, wantValues)
	}
	for _, value := range []string{"linear-env-secret", "linear-file-secret", "github-env-secret", "github-file-secret"} {
		if strings.Contains(strings.Join(loaded.Config.HostSecretEnvNames, ","), value) {
			t.Fatalf("secret metadata exposed resolved value %q", value)
		}
	}
}

func TestDifferentWorkflowFilesKeepProjectAndAssigneeSettings(t *testing.T) {
	d := t.TempDir()
	first := filepath.Join(d, "first.md")
	second := filepath.Join(d, "second.md")
	for path, content := range map[string]string{
		first:  "---\ntracker: {kind: linear, provider: {project_slug_id: first-project, api_key: first-secret}, active_states: [Todo], terminal_states: [Done]}\n---\nprompt",
		second: "---\ntracker: {kind: linear, provider: {project_slug_id: second-project, assignee: second-assignee, api_key: second-secret}, active_states: [Todo], terminal_states: [Done]}\n---\nprompt",
	} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	firstWorkflow, err := Load(first, "")
	if err != nil {
		t.Fatal(err)
	}
	secondWorkflow, err := Load(second, "")
	if err != nil {
		t.Fatal(err)
	}
	if firstWorkflow.Config.Tracker.Provider["project_slug_id"] != "first-project" || firstWorkflow.Config.Tracker.Provider["assignee"] != nil {
		t.Fatalf("first provider=%#v", firstWorkflow.Config.Tracker.Provider)
	}
	if secondWorkflow.Config.Tracker.Provider["project_slug_id"] != "second-project" || secondWorkflow.Config.Tracker.Provider["assignee"] != "second-assignee" {
		t.Fatalf("second provider=%#v", secondWorkflow.Config.Tracker.Provider)
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

func TestProjectSlugMigrationNormalizesAndWarnsWithoutValues(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "WORKFLOW.md")
	legacyValue := "private-project-value"
	content := "---\ntracker: {kind: linear, provider: {project_slug: " + legacyValue + ", api_key: test-key}, active_states: [Todo], terminal_states: [Done]}\n---\nprompt"
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := Load(p, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := w.Config.Tracker.Provider["project_slug_id"]; got != legacyValue {
		t.Fatalf("normalized project_slug_id=%q", got)
	}
	if _, exists := w.Config.Tracker.Provider["project_slug"]; exists {
		t.Fatal("deprecated project_slug remained in normalized provider")
	}
	if len(w.Config.Warnings) != 1 || w.Config.Warnings[0] != legacyProjectSlugWarning {
		t.Fatalf("migration warnings=%q", w.Config.Warnings)
	}
	if strings.Contains(w.Config.Warnings[0], legacyValue) {
		t.Fatalf("migration warning exposed configured project value: %q", w.Config.Warnings[0])
	}
}

func TestCanonicalAndLegacyProjectSlugAreAmbiguousAndReloadIsTransactional(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "WORKFLOW.md")
	canonical := "---\ntracker: {kind: linear, provider: {project_slug_id: project-one, api_key: test-key}, active_states: [Todo], terminal_states: [Done]}\n---\ngood"
	if err := os.WriteFile(p, []byte(canonical), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(p, "")
	if err != nil {
		t.Fatal(err)
	}
	ambiguous := "---\ntracker: {kind: linear, provider: {project_slug_id: project-two, project_slug: legacy-project, api_key: test-key}, active_states: [Todo], terminal_states: [Done]}\n---\nbad"
	if err := os.WriteFile(p, []byte(ambiguous), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.Reload(); err == nil || !strings.Contains(err.Error(), "must not both be set") {
		t.Fatalf("ambiguous reload error=%v", err)
	}
	current := store.Current()
	if current.Prompt != "good" || current.Config.Tracker.Provider["project_slug_id"] != "project-one" {
		t.Fatalf("ambiguous reload replaced last valid workflow: %+v", current)
	}
}

func TestAPIKeyFileReadErrorDoesNotExposeConfiguredPath(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "WORKFLOW.md")
	secretPath := filepath.Join(d, "private-secret-location")
	t.Setenv("SYMPHONY_LINEAR_API_KEY_FILE", secretPath)
	content := "---\ntracker: {kind: linear, provider: {project_slug_id: project, api_key_file: $SYMPHONY_LINEAR_API_KEY_FILE}, active_states: [Todo], terminal_states: [Done]}\n---\nprompt"
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(p, "")
	if err == nil || strings.Contains(err.Error(), secretPath) {
		t.Fatalf("secret file error=%v", err)
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

func TestDeliveryInstructionsReportExactAvailableMode(t *testing.T) {
	manual := (Settings{}).DeliveryInstructions()
	if !strings.Contains(manual, "Delivery mode: manual") || !strings.Contains(manual, "github.owner") {
		t.Fatalf("manual instructions=%q", manual)
	}
	host := (Settings{GitHub: GitHub{Enabled: true}, Tracker: Tracker{HandoffState: "In Review"}}).DeliveryInstructions()
	if !strings.Contains(host, "host-side publish") || !strings.Contains(host, "github_publish_pr") || strings.Contains(host, "manual") {
		t.Fatalf("host instructions=%q", host)
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

func TestAgentTransitionPolicyIsExactAndReloadSafe(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "WORKFLOW.md")
	good := "---\ntracker: {kind: linear, provider: {agent_transitions: {Todo: \"In Progress\", Merging: \"In Review\"}}, active_states: [Todo, In Progress], terminal_states: [Done]}\n---\nprompt"
	if err := os.WriteFile(p, []byte(good), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := NewStore(p, "")
	if err != nil {
		t.Fatal(err)
	}
	policy := s.Current().Config.Tracker.AgentTransitions
	if len(policy) != 2 || policy["Todo"] != "In Progress" || policy["Merging"] != "In Review" {
		t.Fatalf("agent transition policy=%#v", policy)
	}
	for _, invalid := range []string{
		"tracker: {kind: linear, provider: {agent_transitions: []}, active_states: [Todo], terminal_states: [Done]}",
		"tracker: {kind: linear, provider: {agent_transitions: {Todo: Todo}}, active_states: [Todo], terminal_states: [Done]}",
		"tracker: {kind: linear, provider: {agent_transitions: {Done: \"In Review\"}}, active_states: [Todo], terminal_states: [Done]}",
		"tracker: {kind: linear, provider: {agent_transitions: {Todo: Done}}, active_states: [Todo], terminal_states: [Done]}",
	} {
		if err := os.WriteFile(p, []byte("---\n"+invalid+"\n---\nprompt"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := s.Reload(); err == nil {
			t.Fatalf("invalid agent transition policy reloaded: %s", invalid)
		}
		if got := s.Current().Config.Tracker.AgentTransitions["Todo"]; got != "In Progress" {
			t.Fatalf("invalid reload replaced agent transition policy: %#v", s.Current().Config.Tracker.AgentTransitions)
		}
	}
}

func TestGitHubConfigurationIsOptionalScopedAndSecretBacked(t *testing.T) {
	d := t.TempDir()
	secret := filepath.Join(d, "github-token")
	if err := os.WriteFile(secret, []byte("  github-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	workflow := filepath.Join(d, "WORKFLOW.md")
	content := "---\ntracker: {kind: linear, active_states: [Todo], terminal_states: [Done]}\ngithub: {owner: pmrrasmussen, repository: symphony, base_branch: main, token_file: github-token, poll_interval_ms: 123}\n---\nprompt"
	if err := os.WriteFile(workflow, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := Load(workflow, "")
	if err != nil {
		t.Fatal(err)
	}
	got := w.Config.GitHub
	if !got.Enabled || got.Owner != "pmrrasmussen" || got.Repository != "symphony" || got.BaseBranch != "main" || got.Token != "github-secret" || got.PollInterval != 123*time.Millisecond {
		t.Fatalf("github settings=%+v", got)
	}
	if raw := w.Raw["github"].(map[string]any); raw["token_file"] != "github-token" {
		t.Fatalf("raw config unexpectedly contains resolved secret: %#v", raw)
	}
	t.Setenv("PMR27_GITHUB_TOKEN", "environment-secret")
	content = "---\ntracker: {kind: linear, active_states: [Todo], terminal_states: [Done]}\ngithub: {owner: pmrrasmussen, repository: symphony, token: $PMR27_GITHUB_TOKEN}\n---\nprompt"
	if err := os.WriteFile(workflow, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err = Load(workflow, "")
	if err != nil || !w.Config.GitHub.Enabled || w.Config.GitHub.Token != "environment-secret" {
		t.Fatalf("environment-backed github=%+v err=%v", w.Config.GitHub, err)
	}
}

func TestInvalidGitHubConfigurationStaysDisabledWithoutAffectingWorkflow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "WORKFLOW.md")
	for _, githubConfig := range []string{
		"github: []",
		"github: {owner: owner, repository: repo, token: $UNSET_PMR27_TOKEN}",
		"github: {owner: '../owner', repository: repo, token: secret}",
		"github: {owner: owner, repository: repo, token: secret, endpoint: 'http://example.com'}",
	} {
		content := "---\ntracker: {kind: linear, active_states: [Todo], terminal_states: [Done]}\n" + githubConfig + "\n---\nmanual"
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		workflow, err := Load(path, "")
		if err != nil {
			t.Fatalf("optional invalid config affected workflow: %v", err)
		}
		if workflow.Config.GitHub.Enabled || workflow.Prompt != "manual" {
			t.Fatalf("github=%+v prompt=%q", workflow.Config.GitHub, workflow.Prompt)
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

func TestReloadTracksEnvironmentAndSecretFileDependencies(t *testing.T) {
	d := t.TempDir()
	firstSource := filepath.Join(d, "source-one")
	secondSource := filepath.Join(d, "source-two")
	for _, directory := range []string{firstSource, secondSource} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	secret := filepath.Join(d, "linear-key")
	if err := os.WriteFile(secret, []byte("  first-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PMR16_SECRET_FILE", secret)
	t.Setenv("PMR16_SOURCE_ROOT", firstSource)
	workflow := filepath.Join(d, "WORKFLOW.md")
	content := "---\ntracker: {kind: linear, provider: {api_key_file: $PMR16_SECRET_FILE}, active_states: [Todo], terminal_states: [Done]}\nworkspace: {root: work, source_root: $PMR16_SOURCE_ROOT}\n---\nprompt"
	if err := os.WriteFile(workflow, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(workflow, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := store.Current().Config.Tracker.Provider["api_key"]; got != "first-secret" {
		t.Fatalf("trimmed initial file secret=%q", got)
	}

	if err := os.WriteFile(secret, []byte("second-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	changed, err := store.ReloadIfChanged()
	if err != nil || !changed {
		t.Fatalf("secret-only reload changed=%t err=%v", changed, err)
	}
	if got := store.Current().Config.Tracker.Provider["api_key"]; got != "second-secret" {
		t.Fatalf("reloaded file secret=%q", got)
	}

	t.Setenv("PMR16_SOURCE_ROOT", secondSource)
	changed, err = store.ReloadIfChanged()
	if err != nil || !changed {
		t.Fatalf("environment-only reload changed=%t err=%v", changed, err)
	}
	if got := store.Current().Config.Workspace.SourceRoot; got != secondSource {
		t.Fatalf("reloaded source root=%q", got)
	}

	if err := os.WriteFile(secret, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	changed, err = store.ReloadIfChanged()
	if err == nil || changed {
		t.Fatalf("empty secret reload changed=%t err=%v", changed, err)
	}
	if strings.Contains(err.Error(), "second-secret") {
		t.Fatalf("reload error exposed secret: %v", err)
	}
	if got := store.Current().Config.Tracker.Provider["api_key"]; got != "second-secret" {
		t.Fatalf("invalid reload replaced last valid secret=%q", got)
	}
	if changed, err = store.ReloadIfChanged(); err != nil || changed {
		t.Fatalf("unchanged rejected dependency changed=%t err=%v", changed, err)
	}
	if err := os.WriteFile(secret, []byte("recovered-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if changed, err = store.ReloadIfChanged(); err != nil || !changed {
		t.Fatalf("recovered dependency changed=%t err=%v", changed, err)
	}
}

func TestEnvironmentReferencesAreExactAndRequiredSourceRootMustResolve(t *testing.T) {
	d := t.TempDir()
	workflow := filepath.Join(d, "WORKFLOW.md")
	for _, test := range []struct {
		name    string
		setting string
		want    string
	}{
		{name: "braced root", setting: "workspace: {root: '${PMR16_ROOT}'}", want: "exact $VARNAME"},
		{name: "compound root", setting: "workspace: {root: '$PMR16_ROOT/child'}", want: "exact $VARNAME"},
		{name: "ambiguous secret", setting: "tracker: {kind: linear, provider: {api_key: '$PMR16_KEY-extra'}, active_states: [Todo], terminal_states: [Done]}", want: "exact $VARNAME"},
		{name: "unresolved source", setting: "workspace: {source_root: $PMR16_UNSET_SOURCE}", want: "environment reference is unresolved"},
	} {
		t.Run(test.name, func(t *testing.T) {
			frontMatter := "tracker: {kind: linear, active_states: [Todo], terminal_states: [Done]}\n" + test.setting
			if strings.HasPrefix(test.setting, "tracker:") {
				frontMatter = test.setting
			}
			if err := os.WriteFile(workflow, []byte("---\n"+frontMatter+"\n---\nprompt"), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(workflow, ""); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Load error=%v, want %q", err, test.want)
			}
		})
	}
}

func TestReloadPublishesEveryDynamicSettingAsOneSnapshot(t *testing.T) {
	d := t.TempDir()
	firstSource := filepath.Join(d, "source-one")
	secondSource := filepath.Join(d, "source-two")
	for _, directory := range []string{firstSource, secondSource} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	workflow := filepath.Join(d, "WORKFLOW.md")
	initial := "---\ntracker: {kind: linear, provider: {api_key: ' first-key '}, active_states: [Todo], terminal_states: [Done]}\npolling: {interval_ms: 100}\nworkspace: {root: work-one, source_root: " + firstSource + "}\nhooks: {after_create: one, before_run: one, after_run: one, before_remove: one, timeout_ms: 101}\nagent: {max_concurrent_agents: 1, max_turns: 2, max_retry_backoff_ms: 102, max_concurrent_agents_by_state: {Todo: 1}}\ncodex: {command: codex-one, approval_policy: never, thread_sandbox: workspace-write, turn_sandbox_policy: {type: one}, turn_timeout_ms: 103, read_timeout_ms: 104, stall_timeout_ms: 105}\n---\nfirst"
	if err := os.WriteFile(workflow, []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(workflow, "logs")
	if err != nil {
		t.Fatal(err)
	}
	updated := "---\ntracker: {kind: linear, provider: {api_key: ' second-key '}, required_labels: [Ready], active_states: [Backlog, Started], terminal_states: [Closed]}\npolling: {interval_ms: 200}\nworkspace: {root: work-two, source_root: " + secondSource + "}\nhooks: {after_create: two-create, before_run: two-before, after_run: two-after, before_remove: two-remove, timeout_ms: 201}\nagent: {max_concurrent_agents: 3, max_turns: 4, max_retry_backoff_ms: 202, max_concurrent_agents_by_state: {Started: 2}}\ncodex: {command: codex-two, approval_policy: on-request, thread_sandbox: danger-full-access, turn_sandbox_policy: {type: two}, turn_timeout_ms: 203, read_timeout_ms: 204, stall_timeout_ms: 205}\n---\nsecond"
	if err := os.WriteFile(workflow, []byte(updated), 0o600); err != nil {
		t.Fatal(err)
	}
	changed, err := store.ReloadIfChanged()
	if err != nil || !changed {
		t.Fatalf("reload changed=%t err=%v", changed, err)
	}
	got := store.Current()
	settings := got.Config
	if got.Prompt != "second" || settings.Tracker.Provider["api_key"] != "second-key" || strings.Join(settings.Tracker.RequiredLabels, ",") != "ready" || strings.Join(settings.Tracker.ActiveStates, ",") != "Backlog,Started" || strings.Join(settings.Tracker.TerminalStates, ",") != "Closed" {
		t.Fatalf("tracker snapshot=%+v prompt=%q", settings.Tracker, got.Prompt)
	}
	if settings.Polling.Interval != 200*time.Millisecond || settings.Workspace.Root != filepath.Join(d, "work-two") || settings.Workspace.SourceRoot != secondSource || settings.LogRoot != filepath.Join(d, "logs") {
		t.Fatalf("operational paths/polling=%+v %+v log=%q", settings.Polling, settings.Workspace, settings.LogRoot)
	}
	if settings.Hooks != (Hooks{AfterCreate: "two-create", BeforeRun: "two-before", AfterRun: "two-after", BeforeRemove: "two-remove", Timeout: 201 * time.Millisecond}) {
		t.Fatalf("hooks=%+v", settings.Hooks)
	}
	if settings.Agent.MaxConcurrent != 3 || settings.Agent.MaxTurns != 4 || settings.Agent.MaxRetryBackoff != 202*time.Millisecond || settings.Agent.ByState["started"] != 2 {
		t.Fatalf("agent=%+v", settings.Agent)
	}
	policy, ok := settings.Codex.TurnSandboxPolicy.(map[string]any)
	if !ok || policy["type"] != "two" || settings.Codex.Command != "codex-two" || settings.Codex.ApprovalPolicy != "on-request" || settings.Codex.ThreadSandbox != "danger-full-access" || settings.Codex.TurnTimeout != 203*time.Millisecond || settings.Codex.ReadTimeout != 204*time.Millisecond || settings.Codex.StallTimeout != 205*time.Millisecond {
		t.Fatalf("codex=%+v", settings.Codex)
	}
}

func TestReloadAtomicallyReplacesTrackerScopeAndSecretMetadata(t *testing.T) {
	d := t.TempDir()
	t.Setenv("PMR29_FIRST_LINEAR", "first-linear-secret")
	t.Setenv("PMR29_FIRST_GITHUB", "first-github-secret")
	t.Setenv("PMR29_SECOND_LINEAR", "second-linear-secret")
	t.Setenv("PMR29_SECOND_GITHUB", "second-github-secret")
	workflow := filepath.Join(d, "WORKFLOW.md")
	initial := "---\ntracker: {kind: linear, provider: {project_slug_id: first-project, assignee: first-assignee, api_key: $PMR29_FIRST_LINEAR}, active_states: [Todo], terminal_states: [Done]}\ngithub: {token: $PMR29_FIRST_GITHUB}\n---\nfirst"
	if err := os.WriteFile(workflow, []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(workflow, "")
	if err != nil {
		t.Fatal(err)
	}
	updated := "---\ntracker: {kind: linear, provider: {project_slug_id: second-project, assignee: second-assignee, api_key: $PMR29_SECOND_LINEAR}, active_states: [In Progress], terminal_states: [Closed]}\ngithub: {token: $PMR29_SECOND_GITHUB}\n---\nsecond"
	if err := os.WriteFile(workflow, []byte(updated), 0o600); err != nil {
		t.Fatal(err)
	}
	changed, err := store.ReloadIfChanged()
	if err != nil || !changed {
		t.Fatalf("reload changed=%t err=%v", changed, err)
	}
	current := store.Current()
	provider := current.Config.Tracker.Provider
	if current.Prompt != "second" || provider["project_slug_id"] != "second-project" || provider["assignee"] != "second-assignee" || provider["api_key"] != "second-linear-secret" {
		t.Fatalf("tracker scope snapshot=%#v prompt=%q", provider, current.Prompt)
	}
	if got, want := current.Config.HostSecretEnvNames, []string{"PMR29_SECOND_GITHUB", "PMR29_SECOND_LINEAR"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("secret metadata=%v want %v", got, want)
	}
}

func TestCurrentReturnsAnImmutableSnapshotCopy(t *testing.T) {
	d := t.TempDir()
	workflow := filepath.Join(d, "WORKFLOW.md")
	t.Setenv("PMR29_IMMUTABLE_SECRET", "secret")
	content := "---\nextension: {nested: [original]}\ntracker: {kind: linear, provider: {project_slug: project, api_key: $PMR29_IMMUTABLE_SECRET, nested: {value: original}}, active_states: [Todo], terminal_states: [Done]}\nagent: {max_concurrent_agents_by_state: {Todo: 1}}\ncodex: {turn_sandbox_policy: {type: original}}\n---\nprompt"
	if err := os.WriteFile(workflow, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(workflow, "")
	if err != nil {
		t.Fatal(err)
	}
	copy := store.Current()
	copy.Raw["extension"].(map[string]any)["nested"].([]any)[0] = "mutated"
	copy.Config.Tracker.Provider["nested"].(map[string]any)["value"] = "mutated"
	copy.Config.Tracker.ActiveStates[0] = "mutated"
	copy.Config.Agent.ByState["todo"] = 99
	copy.Config.Codex.TurnSandboxPolicy.(map[string]any)["type"] = "mutated"
	copy.Config.HostSecretEnvNames[0] = "mutated"
	copy.Config.Warnings[0] = "mutated"

	current := store.Current()
	if current.Raw["extension"].(map[string]any)["nested"].([]any)[0] != "original" || current.Config.Tracker.Provider["nested"].(map[string]any)["value"] != "original" || current.Config.Tracker.ActiveStates[0] != "Todo" || current.Config.Agent.ByState["todo"] != 1 || current.Config.Codex.TurnSandboxPolicy.(map[string]any)["type"] != "original" || current.Config.HostSecretEnvNames[0] != "PMR29_IMMUTABLE_SECRET" || current.Config.Warnings[0] != legacyProjectSlugWarning {
		t.Fatalf("published workflow was mutated through Current: %+v", current)
	}
}

func TestConcurrentReadersNeverObserveMixedSnapshots(t *testing.T) {
	d := t.TempDir()
	workflow := filepath.Join(d, "WORKFLOW.md")
	versions := []string{
		"---\ntracker: {kind: linear, active_states: [One], terminal_states: [Done]}\npolling: {interval_ms: 1}\nagent: {max_concurrent_agents: 1}\ncodex: {command: one}\n---\none",
		"---\ntracker: {kind: linear, active_states: [Two], terminal_states: [Done]}\npolling: {interval_ms: 2}\nagent: {max_concurrent_agents: 2}\ncodex: {command: two}\n---\ntwo",
	}
	if err := os.WriteFile(workflow, []byte(versions[0]), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(workflow, "")
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	errors := make(chan string, 4)
	var readers sync.WaitGroup
	for range 4 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				current := store.Current()
				one := current.Prompt == "one" && current.Config.Polling.Interval == time.Millisecond && current.Config.Agent.MaxConcurrent == 1 && current.Config.Codex.Command == "one" && current.Config.Tracker.ActiveStates[0] == "One"
				two := current.Prompt == "two" && current.Config.Polling.Interval == 2*time.Millisecond && current.Config.Agent.MaxConcurrent == 2 && current.Config.Codex.Command == "two" && current.Config.Tracker.ActiveStates[0] == "Two"
				if !one && !two {
					select {
					case errors <- "reader observed fields from different workflow versions":
					default:
					}
					return
				}
			}
		}()
	}
	for index := 0; index < 40; index++ {
		if err := os.WriteFile(workflow, []byte(versions[index%2]), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := store.Reload(); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	readers.Wait()
	close(errors)
	for message := range errors {
		t.Error(message)
	}
}
