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

// TestBaseBranchAcceptsSlashesAndIsValidatedApartFromOwnerAndRepository covers
// PMR-178's first gap. owner and repository are single path segments, so a
// slash in either is a mistake; a branch name is not, and holding base_branch
// to the same rule silently disabled the integration for release/1.0 and then
// cut every worktree from main.
func TestBaseBranchAcceptsSlashesAndIsValidatedApartFromOwnerAndRepository(t *testing.T) {
	t.Setenv("PMR178_GITHUB_TOKEN", "github-secret")
	path := filepath.Join(t.TempDir(), "WORKFLOW.md")
	load := func(t *testing.T, baseBranch string) Settings {
		t.Helper()
		github := "github: {owner: pmrrasmussen, repository: symphony, token: $PMR178_GITHUB_TOKEN"
		if baseBranch != "" {
			github += ", base_branch: " + baseBranch
		}
		content := "---\ntracker: {kind: linear, active_states: [Todo], terminal_states: [Done]}\n" + github + "}\n---\nprompt"
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		w, err := Load(path, "")
		if err != nil {
			t.Fatalf("an invalid github block must never fail the load: %v", err)
		}
		return w.Config
	}

	for _, branch := range []string{"main", "release/1.0", "feature/team/nested-name", "v1.2.3"} {
		settings := load(t, branch)
		if !settings.GitHub.Enabled || settings.GitHub.BaseBranch != branch {
			t.Fatalf("legal base branch %q: github=%+v", branch, settings.GitHub)
		}
		if len(settings.Warnings) != 0 {
			t.Fatalf("legal base branch %q warned: %q", branch, settings.Warnings)
		}
	}

	// Names Git itself refuses as refs/heads/<value> still disable the
	// integration -- but now they say so.
	for _, branch := range []string{"/leading", "trailing/", "double//slash", "'has space'", "'.hidden'", "'a..b'", "'ends.'", "'caret^'", "'ref@{0}'", "'work.lock'", "'[bracket]'", "12"} {
		settings := load(t, branch)
		if settings.GitHub.Enabled {
			t.Fatalf("illegal base branch %q was accepted: %+v", branch, settings.GitHub)
		}
		if len(settings.Warnings) != 1 || !strings.Contains(settings.Warnings[0], "github.base_branch") {
			t.Fatalf("illegal base branch %q warnings=%q", branch, settings.Warnings)
		}
	}
}

// TestADisabledGitHubBlockWarnsNamingTheOffendingField keeps the fallback to
// manual delivery from being silent. Preflight emits one check per
// Settings.Warnings entry, so naming the field here is what an operator sees
// under --dry-run.
func TestADisabledGitHubBlockWarnsNamingTheOffendingField(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	blank := filepath.Join(dir, "blank-token")
	if err := os.WriteFile(blank, []byte("  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		github string
		fields []string
	}{
		{github: "github: []", fields: []string{"github"}},
		{github: "github: {owner: owner, repository: repo, token: $UNSET_PMR178_TOKEN}", fields: []string{"github.token"}},
		{github: "github: {owner: owner, repository: repo, token: literal-secret}", fields: []string{"github.token"}},
		{github: "github: {owner: owner, repository: repo, token_file: /nonexistent/pmr178}", fields: []string{"github.token_file"}},
		// A readable but blank credential file must name token_file, not the
		// github.token the operator never wrote.
		{github: "github: {owner: owner, repository: repo, token_file: blank-token}", fields: []string{"github.token_file"}},
		{github: "github: {owner: '../owner', repository: repo}", fields: []string{"github.owner", "github.token"}},
		{github: "github: {owner: owner/nested, repository: 'repo space', base_branch: 'bad branch'}", fields: []string{"github.base_branch", "github.owner", "github.repository", "github.token"}},
		{github: "github: {owner: owner, repository: repo, endpoint: 'http://example.com', poll_interval_ms: 0}", fields: []string{"github.endpoint", "github.poll_interval_ms", "github.token"}},
	} {
		t.Run(test.github, func(t *testing.T) {
			content := "---\ntracker: {kind: linear, active_states: [Todo], terminal_states: [Done]}\n" + test.github + "\n---\nmanual"
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			w, err := Load(path, "")
			if err != nil {
				t.Fatalf("optional invalid config affected workflow: %v", err)
			}
			if w.Config.GitHub.Enabled {
				t.Fatalf("github=%+v", w.Config.GitHub)
			}
			if len(w.Config.Warnings) != 1 {
				t.Fatalf("warnings=%q", w.Config.Warnings)
			}
			warning := w.Config.Warnings[0]
			if !strings.Contains(warning, "disabled") || !strings.Contains(warning, "manual") {
				t.Fatalf("warning does not state the consequence: %q", warning)
			}
			for _, field := range test.fields {
				if !strings.Contains(warning, field) {
					t.Fatalf("warning %q omits %q", warning, field)
				}
			}
			// The warning names fields, never values: a token that failed to
			// resolve must not be echoed into logs or the status file.
			if strings.Contains(warning, "literal-secret") {
				t.Fatalf("warning exposed a configured secret: %q", warning)
			}
		})
	}

	// An absent github block is a supported choice, not a misconfiguration.
	content := "---\ntracker: {kind: linear, active_states: [Todo], terminal_states: [Done]}\n---\nmanual"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := Load(path, "")
	if err != nil {
		t.Fatal(err)
	}
	if w.Config.GitHub.Enabled || len(w.Config.Warnings) != 0 {
		t.Fatalf("an absent github block warned: %+v %q", w.Config.GitHub, w.Config.Warnings)
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
