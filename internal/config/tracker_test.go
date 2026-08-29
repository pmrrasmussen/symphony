package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

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

// TestActiveAndTerminalStatesMustBeDisjoint covers PMR-178's second gap. The
// coordinator matches both lists with config.Norm, so the overlap this refuses
// is the one that predicate sees: case and surrounding whitespace do not make
// two spellings of one state distinct.
func TestActiveAndTerminalStatesMustBeDisjoint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "WORKFLOW.md")
	load := func(t *testing.T, tracker string) error {
		t.Helper()
		content := "---\ntracker: {kind: linear, " + tracker + "}\n---\nprompt"
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := Load(path, "")
		return err
	}

	for _, test := range []struct{ tracker, active, terminal string }{
		{tracker: "active_states: [Todo, Done], terminal_states: [Done, Canceled]", active: "Done", terminal: "Done"},
		{tracker: "active_states: [Todo, done], terminal_states: [Done]", active: "done", terminal: "Done"},
		// stringList already trims, so the reported spelling is the trimmed one.
		{tracker: `active_states: [Todo, " Merging "], terminal_states: [MERGING]`, active: "Merging", terminal: "MERGING"},
	} {
		err := load(t, test.tracker)
		if err == nil {
			t.Fatalf("overlapping states were accepted: %s", test.tracker)
		}
		// Both spellings must appear: with only one, an operator cannot tell
		// which of the two lists to edit.
		if !strings.Contains(err.Error(), fmt.Sprintf("%q", test.active)) || !strings.Contains(err.Error(), fmt.Sprintf("%q", test.terminal)) {
			t.Fatalf("error %q does not name both %q and %q", err, test.active, test.terminal)
		}
	}

	if err := load(t, "active_states: [Todo, In Progress, Rework, Merging], terminal_states: [Done, Canceled]"); err != nil {
		t.Fatalf("disjoint lifecycle states must load: %v", err)
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
	if _, err := s.ReloadIfChanged(); err == nil || !strings.Contains(err.Error(), "resolved secret is empty") {
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
	if _, err := store.ReloadIfChanged(); err == nil || !strings.Contains(err.Error(), "must not both be set") {
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
		if _, err := s.ReloadIfChanged(); err == nil {
			t.Fatalf("invalid handoff policy reloaded: %s", invalid)
		}
		if got := s.Current().Config.Tracker.HandoffState; got != "In Review" {
			t.Fatalf("invalid reload replaced handoff policy: %q", got)
		}
	}
}

func TestHostTransitionPolicyIsExactAndReloadSafe(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "WORKFLOW.md")
	good := "---\ntracker: {kind: linear, provider: {transitions: {start: {Todo: \"In Progress\"}, refuse_landing: {Merging: \"In Review\"}}}, active_states: [Todo, In Progress, Merging, In Review], terminal_states: [Done]}\n---\nprompt"
	if err := os.WriteFile(p, []byte(good), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := NewStore(p, "")
	if err != nil {
		t.Fatal(err)
	}
	policy := s.Current().Config.Tracker.HostTransitions
	if len(policy.Start) != 1 || policy.Start["todo"] != "In Progress" {
		t.Fatalf("start policy=%#v, want lowercased todo -> In Progress", policy.Start)
	}
	if len(policy.RefuseLanding) != 1 || policy.RefuseLanding["merging"] != "In Review" {
		t.Fatalf("refuse_landing policy=%#v, want lowercased merging -> In Review", policy.RefuseLanding)
	}
	// The host transition policy is never an agent capability: it narrows the
	// agent's authority rather than widening it.
	if s.Current().Config.LinearSessionCapabilityEnabled() {
		t.Fatal("transitions alone must not enable the agent Linear session capability")
	}
	for _, invalid := range []string{
		// Empty / unknown keys.
		"tracker: {kind: linear, provider: {transitions: {}}, active_states: [Todo], terminal_states: [Done]}",
		"tracker: {kind: linear, provider: {transitions: {bogus: {Todo: \"In Progress\"}}}, active_states: [Todo, In Progress], terminal_states: [Done]}",
		// start: same-state, target not active, terminal endpoint.
		"tracker: {kind: linear, provider: {transitions: {start: {Todo: Todo}}}, active_states: [Todo], terminal_states: [Done]}",
		"tracker: {kind: linear, provider: {transitions: {start: {Todo: \"In Progress\"}}}, active_states: [Todo], terminal_states: [Done]}",
		"tracker: {kind: linear, provider: {transitions: {start: {Todo: Done}}}, active_states: [Todo, Done], terminal_states: [Done]}",
		// refuse_landing: terminal endpoints.
		"tracker: {kind: linear, provider: {transitions: {refuse_landing: {Done: \"In Review\"}}}, active_states: [In Review], terminal_states: [Done]}",
		"tracker: {kind: linear, provider: {transitions: {refuse_landing: {Merging: Done}}}, active_states: [Merging], terminal_states: [Done]}",
	} {
		if err := os.WriteFile(p, []byte("---\n"+invalid+"\n---\nprompt"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ReloadIfChanged(); err == nil {
			t.Fatalf("invalid host transition policy reloaded: %s", invalid)
		}
		if got := s.Current().Config.Tracker.HostTransitions.Start["todo"]; got != "In Progress" {
			t.Fatalf("invalid reload replaced host transition policy: %#v", s.Current().Config.Tracker.HostTransitions)
		}
	}
}

func TestFollowupIssueCreationPolicyIsOptInBooleanBacklogGatedAndReloadSafe(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "WORKFLOW.md")
	base := "tracker: {kind: linear, provider: {%s}, active_states: [Todo, In Progress], terminal_states: [Done]}"
	write := func(provider string) {
		t.Helper()
		if err := os.WriteFile(p, []byte("---\n"+fmt.Sprintf(base, provider)+"\n---\nprompt"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("")
	s, err := NewStore(p, "")
	if err != nil {
		t.Fatal(err)
	}
	if s.Current().Config.Tracker.FollowupIssueCreation {
		t.Fatal("follow-up issue creation must default to disabled")
	}
	if s.Current().Config.LinearSessionCapabilityEnabled() {
		t.Fatal("no Linear session capability should be enabled by default")
	}

	write("followup_issue_creation: true")
	if _, err := s.ReloadIfChanged(); err != nil {
		t.Fatal(err)
	}
	if !s.Current().Config.Tracker.FollowupIssueCreation {
		t.Fatal("follow-up issue creation was not enabled")
	}
	if !s.Current().Config.LinearSessionCapabilityEnabled() {
		t.Fatal("follow-up issue creation alone should enable the Linear session capability")
	}

	write("followup_issue_creation: \"true\"")
	if _, err := s.ReloadIfChanged(); err == nil {
		t.Fatal("non-boolean followup_issue_creation reloaded")
	}
	if !s.Current().Config.Tracker.FollowupIssueCreation {
		t.Fatal("invalid reload replaced follow-up issue creation policy")
	}

	write("followup_issue_creation: false")
	if _, err := s.ReloadIfChanged(); err != nil {
		t.Fatal(err)
	}
	if s.Current().Config.Tracker.FollowupIssueCreation {
		t.Fatal("explicit false did not disable follow-up issue creation")
	}

	dispatchableBacklog := "---\ntracker: {kind: linear, provider: {followup_issue_creation: true}, active_states: [Todo, Backlog], terminal_states: [Done]}\n---\nprompt"
	if err := os.WriteFile(p, []byte(dispatchableBacklog), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReloadIfChanged(); err == nil {
		t.Fatal("follow-up issue creation accepted dispatchable Backlog")
	}
}

func TestLegacyChildIssueCreationMigratesToFollowupCapability(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "WORKFLOW.md")
	write := func(provider string) {
		t.Helper()
		content := "---\ntracker: {kind: linear, provider: {" + provider + "}, active_states: [Todo], terminal_states: [Done]}\n---\nprompt"
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("child_issue_creation: true")
	w, err := Load(p, "")
	if err != nil {
		t.Fatal(err)
	}
	if !w.Config.Tracker.FollowupIssueCreation {
		t.Fatal("legacy child setting did not enable the follow-up capability")
	}
	if len(w.Config.Warnings) != 1 || w.Config.Warnings[0] != legacyChildIssueCreationWarning {
		t.Fatalf("migration warnings=%q", w.Config.Warnings)
	}
	if _, exists := w.Config.Tracker.Provider["child_issue_creation"]; exists {
		t.Fatalf("legacy setting was not normalized: %#v", w.Config.Tracker.Provider)
	}
	if value := w.Config.Tracker.Provider["followup_issue_creation"]; value != true {
		t.Fatalf("normalized follow-up setting=%#v", value)
	}

	write("child_issue_creation: true, followup_issue_creation: true")
	if _, err := Load(p, ""); err == nil {
		t.Fatal("legacy and canonical follow-up settings were both accepted")
	}
}

func TestAnAllWhitespaceHandoffStateIsNoHandoffStateAtAll(t *testing.T) {
	s := Settings{GitHub: GitHub{Enabled: true}, Tracker: Tracker{HandoffState: "   "}}
	if s.HostSidePublishPromised() {
		t.Fatal("an all-whitespace handoff state promised host-side publish")
	}
	if s.LinearSessionCapabilityEnabled() {
		t.Fatal("an all-whitespace handoff state asked for a bound Linear session")
	}
	if s.SessionCapabilityAdvertisable() {
		t.Fatal("an all-whitespace handoff state reported a capability as advertisable")
	}
	for _, backend := range AgentBackends() {
		guidance := s.DeliveryInstructions(backend)
		if strings.Contains(guidance, HostSidePublishPromiseMarker) {
			t.Fatalf("backend %q was promised host-side publish: %q", backend, guidance)
		}
		if !strings.Contains(guidance, "Delivery mode: manual") {
			t.Fatalf("backend %q was not told delivery is manual: %q", backend, guidance)
		}
	}
}
