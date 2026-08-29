package github

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
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

// TestRequiredCheckOutcomesFollowsPaginationOfBothTables places the one
// configured required name on the second page of each check table, behind a
// page of unrelated entries. Before this, both fetches read a single
// unpaginated page (GitHub defaults them to 30 items), so a head commit with
// enough matrix jobs, bots, and scanners to push a required check onto page 2
// left that check permanently checkMissing and landing waiting forever
// (PMR-190).
func TestRequiredCheckOutcomesFollowsPaginationOfBothTables(t *testing.T) {
	for _, test := range []struct {
		name  string
		place func(*apiFixture, map[string]any)
		want  checkOutcome
	}{
		{
			name: "check run on the second page",
			place: func(api *apiFixture, entry map[string]any) {
				api.checkRuns = append(api.checkRuns, entry)
			},
			want: checkPassed,
		},
		{
			name: "commit status on the second page",
			place: func(api *apiFixture, entry map[string]any) {
				api.statuses = append(api.statuses, map[string]any{"context": "ci/build", "state": "success"})
			},
			want: checkPassed,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			api := newAPI(t)
			api.pageSize = 3
			for i := range 3 {
				api.checkRuns = append(api.checkRuns, map[string]any{"name": "noise/run-" + strconv.Itoa(i), "status": "completed", "conclusion": "success"})
				api.statuses = append(api.statuses, map[string]any{"context": "noise/status-" + strconv.Itoa(i), "state": "success"})
			}
			test.place(api, map[string]any{"name": "ci/build", "status": "completed", "conclusion": "success"})
			settings := api.settings()
			m := New(func() config.Settings { return config.Settings{GitHub: settings} }, slog.Default())
			outcomes, err := m.requiredCheckOutcomes(context.Background(), settings, api.prSHA, []string{"ci/build"})
			if err != nil {
				t.Fatal(err)
			}
			if outcomes["ci/build"] != test.want {
				t.Fatalf("outcomes=%v want %v for a required check reported past the first page", outcomes, test.want)
			}
		})
	}
}

// TestPaginatedReadStopsAtThePageCap asserts the runaway guard: a server that
// offers an endless rel="next" is followed maxPages deep and no further, so a
// paginated read terminates instead of looping for as long as the endpoint
// keeps advertising another page.
func TestPaginatedReadStopsAtThePageCap(t *testing.T) {
	pages := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages++
		w.Header().Set("Link", fmt.Sprintf(`<%s/endless?page=%d>; rel="next"`, "http://"+r.Host, pages+1))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"n":1}]`))
	}))
	defer server.Close()
	settings := config.GitHub{Owner: "owner", Repository: "repo", Token: "t", Endpoint: server.URL}
	m := New(func() config.Settings { return config.Settings{GitHub: settings} }, slog.Default())
	collected := 0
	complete, err := paginate(context.Background(), m, settings, "/endless", func(page []map[string]any) { collected += len(page) })
	if err != nil {
		t.Fatal(err)
	}
	if complete {
		t.Fatal("a walk stopped by the page cap must report itself incomplete")
	}
	if pages != maxPages || collected != maxPages {
		t.Fatalf("pages=%d collected=%d want %d of each", pages, collected, maxPages)
	}
}

// TestNextPagePathIgnoresLinksOutsideTheConfiguredEndpoint pins the one
// security property of Link following: every request carries the bearer
// token, so a rel="next" pointing anywhere but the configured endpoint ends
// pagination rather than being followed.
func TestNextPagePathIgnoresLinksOutsideTheConfiguredEndpoint(t *testing.T) {
	for _, test := range []struct {
		name   string
		header string
		want   string
	}{
		{name: "no link header", header: "", want: ""},
		{name: "next under the endpoint", header: `<https://api.github.com/repositories/1/reviews?page=2>; rel="next"`, want: "/repositories/1/reviews?page=2"},
		{name: "next after other rels", header: `<https://api.github.com/repositories/1/reviews?page=9>; rel="last", <https://api.github.com/repositories/1/reviews?page=2>; rel="next"`, want: "/repositories/1/reviews?page=2"},
		{name: "only a last link", header: `<https://api.github.com/repositories/1/reviews?page=9>; rel="last"`, want: ""},
		{name: "next on another host", header: `<https://evil.example.com/repositories/1/reviews?page=2>; rel="next"`, want: ""},
		{name: "next on a host the endpoint merely prefixes", header: `<https://api.github.com.evil.example.com/x>; rel="next"`, want: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			header := http.Header{}
			if test.header != "" {
				header.Set("Link", test.header)
			}
			if got := nextPagePath(header, "https://api.github.com"); got != test.want {
				t.Fatalf("nextPagePath=%q want %q", got, test.want)
			}
		})
	}
}

// TestReviewsAccumulatePagesBeforeFoldingReviewerPrecedence asserts that
// pagination feeds the effective-state fold rather than bypassing it: a
// reviewer's CHANGES_REQUESTED on the first page is still superseded by that
// same reviewer's later APPROVED on the second, and another reviewer's
// CHANGES_REQUESTED past the first page is still seen (PMR-174, PMR-190).
func TestReviewsAccumulatePagesBeforeFoldingReviewerPrecedence(t *testing.T) {
	for _, test := range []struct {
		name    string
		reviews []map[string]any
		want    string
	}{
		{
			name: "later page approval supersedes an earlier page's changes requested",
			reviews: []map[string]any{
				{"user": map[string]any{"login": "bob"}, "state": "CHANGES_REQUESTED", "submitted_at": "t1"},
				{"user": map[string]any{"login": "carol"}, "state": "COMMENTED", "submitted_at": "t2"},
				{"user": map[string]any{"login": "bob"}, "state": "APPROVED", "submitted_at": "t3"},
			},
			want: "approved",
		},
		{
			name: "changes requested past the first page is seen",
			reviews: []map[string]any{
				{"user": map[string]any{"login": "alice"}, "state": "APPROVED", "submitted_at": "t1"},
				{"user": map[string]any{"login": "carol"}, "state": "COMMENTED", "submitted_at": "t2"},
				{"user": map[string]any{"login": "bob"}, "state": "CHANGES_REQUESTED", "submitted_at": "t3"},
			},
			want: "changes_requested",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			api := newAPI(t)
			api.pageSize = 2
			api.reviews = test.reviews
			settings := api.settings()
			m := New(func() config.Settings { return config.Settings{GitHub: settings} }, slog.Default())
			state, _, _, err := m.reviews(context.Background(), settings, 7)
			if err != nil {
				t.Fatal(err)
			}
			if state != test.want {
				t.Fatalf("review state=%q want %q", state, test.want)
			}
		})
	}
}

// TestReviewThreadsWalkTheConnectionCursor asserts the thread read follows the
// GraphQL connection to its end -- an unresolved thread past the first page is
// counted, and a fully-read listing is not reported truncated.
func TestReviewThreadsWalkTheConnectionCursor(t *testing.T) {
	api := newAPI(t)
	api.threadPageSize = 2
	api.threads = []map[string]any{{"isResolved": true}, {"isResolved": true}, {"isResolved": true}, {"isResolved": false}}
	settings := api.settings()
	m := New(func() config.Settings { return config.Settings{GitHub: settings} }, slog.Default())
	unresolved, total, truncated, err := m.reviewThreads(context.Background(), settings, 7)
	if err != nil {
		t.Fatal(err)
	}
	if unresolved != 1 || total != 4 || truncated {
		t.Fatalf("unresolved=%d total=%d truncated=%v want 1, 4, false", unresolved, total, truncated)
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
