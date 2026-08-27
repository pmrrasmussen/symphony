package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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

// TestGitHubLandingPolicyIsStrictAndFailsClosed exercises the PMR-37
// github.merge_state/merge_method/required_checks fields. Unlike the rest of
// the github: block (which silently disables on any invalid value), these
// fields must reject the whole workflow load, the same way
// tracker.provider.agent_transitions does.
func TestGitHubLandingPolicyIsStrictAndFailsClosed(t *testing.T) {
	t.Setenv("PMR37_GITHUB_TOKEN", "github-secret")
	path := filepath.Join(t.TempDir(), "WORKFLOW.md")
	full := "github: {owner: pmrrasmussen, repository: symphony, base_branch: main, token: $PMR37_GITHUB_TOKEN, "
	for _, test := range []struct {
		name   string
		github string
	}{
		{name: "merge_state is not an active state", github: full + "merge_state: Merging, required_checks: [ci]}"},
		{name: "merge_state is a terminal state", github: full + "merge_state: Done, required_checks: [ci]}"},
		{name: "merge_state equals handoff_state", github: full + `merge_state: "In Review", required_checks: [ci]}`},
		{name: "merge_state missing required_checks", github: full + "merge_state: Merging}"},
		{name: "required_checks is an empty list", github: full + "merge_state: Merging, required_checks: []}"},
		{name: "required_checks has duplicate entries", github: full + "merge_state: Merging, required_checks: [ci, CI]}"},
		{name: "required_checks has a blank entry", github: full + `merge_state: Merging, required_checks: [" "]}`},
		{name: "merge_method is not in the bounded enum", github: full + "merge_state: Merging, merge_method: rewrite, required_checks: [ci]}"},
		{name: "merge_method without merge_state", github: "github: {owner: pmrrasmussen, repository: symphony, base_branch: main, token: $PMR37_GITHUB_TOKEN, merge_method: squash}"},
		{name: "required_checks without merge_state", github: "github: {owner: pmrrasmussen, repository: symphony, base_branch: main, token: $PMR37_GITHUB_TOKEN, required_checks: [ci]}"},
		{name: "update_stale_branch is not a boolean", github: full + "merge_state: Merging, required_checks: [ci], update_stale_branch: yes}"},
		{name: "update_stale_branch without merge_state", github: "github: {owner: pmrrasmussen, repository: symphony, base_branch: main, token: $PMR37_GITHUB_TOKEN, update_stale_branch: true}"},
		{name: "land_fix_enabled is not a boolean", github: full + "merge_state: Merging, required_checks: [ci], land_fix_enabled: maybe}"},
		{name: "max_land_attempts is not an integer", github: full + `merge_state: Merging, required_checks: [ci], max_land_attempts: "two"}`},
		{name: "max_land_attempts is not positive", github: full + "merge_state: Merging, required_checks: [ci], max_land_attempts: 0}"},
		{name: "allow_conflict_resolution is not a boolean", github: full + "merge_state: Merging, required_checks: [ci], allow_conflict_resolution: sometimes}"},
		{name: "land_fix_enabled without merge_state", github: "github: {owner: pmrrasmussen, repository: symphony, base_branch: main, token: $PMR37_GITHUB_TOKEN, land_fix_enabled: true}"},
		{name: "max_land_attempts without merge_state", github: "github: {owner: pmrrasmussen, repository: symphony, base_branch: main, token: $PMR37_GITHUB_TOKEN, max_land_attempts: 3}"},
		{name: "allow_conflict_resolution without merge_state", github: "github: {owner: pmrrasmussen, repository: symphony, base_branch: main, token: $PMR37_GITHUB_TOKEN, allow_conflict_resolution: true}"},
		{name: "merge_state without a fully configured github integration", github: "github: {owner: pmrrasmussen, merge_state: Merging, required_checks: [ci]}"},
	} {
		t.Run(test.name, func(t *testing.T) {
			content := "---\ntracker: {kind: linear, provider: {handoff_state: \"In Review\"}, active_states: [Todo, \"In Progress\"], terminal_states: [Done, Canceled]}\n" + test.github + "\n---\nprompt"
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path, ""); err == nil {
				t.Fatalf("invalid landing configuration was accepted rather than failing closed: %s", test.github)
			}
		})
	}
}

func TestGitHubLandingPolicyParsesValidConfiguration(t *testing.T) {
	t.Setenv("PMR37_GITHUB_TOKEN", "github-secret")
	path := filepath.Join(t.TempDir(), "WORKFLOW.md")
	content := "---\ntracker: {kind: linear, provider: {handoff_state: \"In Review\"}, active_states: [Todo, \"In Progress\", Merging], terminal_states: [Done, Canceled]}\ngithub: {owner: pmrrasmussen, repository: symphony, base_branch: main, token: $PMR37_GITHUB_TOKEN, merge_state: Merging, required_checks: [ci/build, ci/test]}\n---\nprompt"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := Load(path, "")
	if err != nil {
		t.Fatal(err)
	}
	got := w.Config.GitHub
	if !got.Enabled || got.MergeState != "Merging" || got.MergeMethod != "merge" || got.UpdateStaleBranch || len(got.RequiredChecks) != 2 || got.RequiredChecks[0] != "ci/build" || got.RequiredChecks[1] != "ci/test" {
		t.Fatalf("github=%+v", got)
	}
	// Bounded-fix fields default off (feature disabled) with a positive attempt
	// budget so an enabled feature always has a valid bound.
	if got.LandFixEnabled || got.AllowConflictResolution || got.MaxLandAttempts != 2 {
		t.Fatalf("bounded-fix defaults=%+v", got)
	}

	content = strings.Replace(content, "merge_state: Merging, required_checks", "merge_state: Merging, merge_method: squash, required_checks", 1)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err = Load(path, "")
	if err != nil {
		t.Fatal(err)
	}
	if w.Config.GitHub.MergeMethod != "squash" {
		t.Fatalf("merge_method=%q", w.Config.GitHub.MergeMethod)
	}

	content = strings.Replace(content, "required_checks: [ci/build, ci/test]", "required_checks: [ci/build, ci/test], update_stale_branch: true", 1)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err = Load(path, "")
	if err != nil {
		t.Fatal(err)
	}
	if !w.Config.GitHub.UpdateStaleBranch {
		t.Fatal("update_stale_branch was not enabled")
	}

	content = strings.Replace(content, "update_stale_branch: true", "update_stale_branch: true, land_fix_enabled: true, max_land_attempts: 3, allow_conflict_resolution: true", 1)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err = Load(path, "")
	if err != nil {
		t.Fatal(err)
	}
	if fix := w.Config.GitHub; !fix.LandFixEnabled || fix.MaxLandAttempts != 3 || !fix.AllowConflictResolution {
		t.Fatalf("bounded-fix fields=%+v", fix)
	}
}

// TestGitHubLandingPolicyAllowsMergeStateAsAnActiveState exercises the
// canonical lifecycle rollout (PMR-38): Merging must be able to be a
// dispatchable active state, because a session only receives the
// github_land_pr tool once it has actually been dispatched for an issue
// currently in that exact state (see codex/backend.go). It remains rejected
// as a terminal state or as the configured handoff_state.
func TestGitHubLandingPolicyAllowsMergeStateAsAnActiveState(t *testing.T) {
	t.Setenv("PMR38_GITHUB_TOKEN", "github-secret")
	path := filepath.Join(t.TempDir(), "WORKFLOW.md")
	content := "---\ntracker: {kind: linear, provider: {handoff_state: \"In Review\"}, active_states: [Todo, \"In Progress\", Rework, Merging], terminal_states: [Done, Canceled]}\ngithub: {owner: pmrrasmussen, repository: symphony, base_branch: main, token: $PMR38_GITHUB_TOKEN, merge_state: Merging, required_checks: [ci]}\n---\nprompt"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := Load(path, "")
	if err != nil {
		t.Fatalf("merge_state as an active state must be accepted: %v", err)
	}
	if !w.Config.GitHub.Enabled || w.Config.GitHub.MergeState != "Merging" {
		t.Fatalf("github=%+v", w.Config.GitHub)
	}
	if got := w.Config.Tracker.ActiveStates; len(got) != 4 || got[3] != "Merging" {
		t.Fatalf("active_states=%v", got)
	}
}
