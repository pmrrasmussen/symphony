package github

import (
	"context"
	"log/slog"
	"testing"

	"github.com/pmrrasmussen/symphony/internal/config"
)

// TestStatusStateOutcomeMapsGitHubCombinedStatusStates pins statusStateOutcome's
// mapping against every state GitHub's combined-status API actually reports
// (error, failure, pending, success) plus an unrecognised or empty state, so a
// repository whose required checks come from a commit-status integration
// (Buildkite, CircleCI, Jenkins, and similar) is gated by an asserted mapping
// rather than an implicit one: success passes, pending waits, and everything
// else -- including a state GitHub has not documented -- fails closed.
func TestStatusStateOutcomeMapsGitHubCombinedStatusStates(t *testing.T) {
	for _, test := range []struct {
		name  string
		state string
		want  checkOutcome
	}{
		{"success", "success", checkPassed},
		{"mixed case success", "SUCCESS", checkPassed},
		{"padded success", "  success  ", checkPassed},
		{"pending", "pending", checkPending},
		{"error", "error", checkFailed},
		{"failure", "failure", checkFailed},
		{"empty state fails closed", "", checkFailed},
		{"unrecognised state fails closed", "queued", checkFailed},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := statusStateOutcome(test.state); got != test.want {
				t.Fatalf("statusStateOutcome(%q)=%v want %v", test.state, got, test.want)
			}
		})
	}
}

// TestRequiredCheckOutcomesReadsTheCombinedStatusTable drives
// requiredCheckOutcomes through a required name reported only as a commit
// status, one entry per outcome GitHub's combined-status API can report. This
// is the branch of requiredCheckOutcomes a repository without any GitHub
// Actions check runs relies on exclusively, and it had no test at all before
// this: checkRunOutcome was covered through the landing tests' check-run
// fixtures, but nothing ever populated api.statuses.
func TestRequiredCheckOutcomesReadsTheCombinedStatusTable(t *testing.T) {
	for _, test := range []struct {
		name  string
		state string
		want  checkOutcome
	}{
		{"success", "success", checkPassed},
		{"pending", "pending", checkPending},
		{"failure", "failure", checkFailed},
		{"error", "error", checkFailed},
		{"unrecognised state fails closed", "queued", checkFailed},
	} {
		t.Run(test.name, func(t *testing.T) {
			api := newAPI(t)
			api.statuses = []map[string]any{{"context": "ci/build", "state": test.state}}
			settings := api.settings()
			m := New(func() config.Settings { return config.Settings{GitHub: settings} }, slog.Default())
			outcomes, err := m.requiredCheckOutcomes(context.Background(), settings, api.prSHA, []string{"ci/build"})
			if err != nil {
				t.Fatal(err)
			}
			if outcomes["ci/build"] != test.want {
				t.Fatalf("outcomes=%v want %v", outcomes, test.want)
			}
		})
	}
}

// TestRequiredCheckOutcomesPrefersCheckRunsOverCombinedStatusOnDisagreement
// asserts and documents the precedence requiredCheckOutcomes applies when a
// required name is reported in both tables: the check-run table is read
// after the combined-status table and overwrites its entry, so a check-run
// outcome wins over a disagreeing commit status for the same name (see the
// comment on requiredCheckOutcomes in checks.go).
func TestRequiredCheckOutcomesPrefersCheckRunsOverCombinedStatusOnDisagreement(t *testing.T) {
	api := newAPI(t)
	api.statuses = []map[string]any{{"context": "ci/build", "state": "success"}}
	api.checkRuns = []map[string]any{{"name": "ci/build", "status": "completed", "conclusion": "failure"}}
	settings := api.settings()
	m := New(func() config.Settings { return config.Settings{GitHub: settings} }, slog.Default())
	outcomes, err := m.requiredCheckOutcomes(context.Background(), settings, api.prSHA, []string{"ci/build"})
	if err != nil {
		t.Fatal(err)
	}
	if outcomes["ci/build"] != checkFailed {
		t.Fatalf("outcomes=%v, want the check-run table (failure) to win over the disagreeing commit status (success)", outcomes)
	}
}

// TestRequiredCheckOutcomesMissingNameStaysMissing asserts a required name
// absent from both tables stays checkMissing rather than defaulting to any
// other outcome, which is what lets requiredCheckWaitReason tell a typo'd or
// renamed required check apart from one that is merely slow to report.
func TestRequiredCheckOutcomesMissingNameStaysMissing(t *testing.T) {
	api := newAPI(t)
	settings := api.settings()
	m := New(func() config.Settings { return config.Settings{GitHub: settings} }, slog.Default())
	outcomes, err := m.requiredCheckOutcomes(context.Background(), settings, api.prSHA, []string{"ci/build"})
	if err != nil {
		t.Fatal(err)
	}
	if outcomes["ci/build"] != checkMissing {
		t.Fatalf("outcomes=%v want checkMissing", outcomes)
	}
}
