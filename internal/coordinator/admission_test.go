package coordinator

import (
	"bytes"
	"context"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/domain"
)

func TestIneligibleReasonCategorizesEachRejection(t *testing.T) {
	tests := []struct {
		name   string
		issue  domain.Issue
		s      config.Settings
		reason string
	}{
		{name: "missing identity", issue: domain.Issue{}, s: config.Settings{}, reason: "missing_identity"},
		{
			name:   "not active",
			issue:  domain.Issue{ID: "a", Identifier: "X-1", Title: "t", State: "Backlog", Dispatchable: true},
			s:      config.Settings{Tracker: config.Tracker{ActiveStates: []string{"Todo"}}},
			reason: "not_active",
		},
		{
			name:   "terminal",
			issue:  domain.Issue{ID: "a", Identifier: "X-1", Title: "t", State: "Done", Dispatchable: true},
			s:      config.Settings{Tracker: config.Tracker{ActiveStates: []string{"Done"}, TerminalStates: []string{"Done"}}},
			reason: "terminal",
		},
		{
			name:   "not routable",
			issue:  domain.Issue{ID: "a", Identifier: "X-1", Title: "t", State: "Todo", Dispatchable: true},
			s:      config.Settings{Tracker: config.Tracker{ActiveStates: []string{"Todo"}, RequiredLabels: []string{"ready"}}},
			reason: "not_routable",
		},
		{
			name:   "blocked by relation",
			issue:  domain.Issue{ID: "a", Identifier: "X-1", Title: "t", State: "Todo", Dispatchable: false, BlockedBy: []domain.Blocker{{ID: "b", Identifier: "X-0", State: "In Progress", Dispatchable: false}}},
			s:      config.Settings{Tracker: config.Tracker{ActiveStates: []string{"Todo"}}},
			reason: "blocked_by_relation",
		},
		{
			name:   "eligible",
			issue:  domain.Issue{ID: "a", Identifier: "X-1", Title: "t", State: "Todo", Dispatchable: true},
			s:      config.Settings{Tracker: config.Tracker{ActiveStates: []string{"Todo"}}},
			reason: "",
		},
		// dispatchable() (internal/linear/tracker.go) checks the assignee-policy
		// mismatch before it checks blockers, so an issue carrying both must
		// report the assignee cause here too, not blocked_by_relation: naming
		// the blocker would send an operator to resolve a relation that was
		// never why the issue was refused.
		{
			name: "assignee mismatch outranks an open blocker",
			issue: domain.Issue{
				ID: "a", Identifier: "X-1", Title: "t", State: "Todo", Dispatchable: false, AssigneeID: "someone-else",
				AssigneeMismatch: true,
				BlockedBy:        []domain.Blocker{{ID: "b", Identifier: "X-0", State: "In Progress", Dispatchable: false}},
			},
			s: config.Settings{
				Tracker: config.Tracker{ActiveStates: []string{"Todo"}, Provider: map[string]any{"assignee": "required-assignee"}},
			},
			reason: "not_routable",
		},
		// AssigneeMismatch is populated by the tracker from its own resolved
		// policy value (internal/linear/tracker.go), never re-derived here from
		// the raw, possibly-"me" config string, so a false AssigneeMismatch
		// correctly falls through to the real cause instead of masking it.
		{
			name: "open blocker reported when assignee actually matches a resolved me policy",
			issue: domain.Issue{
				ID: "a", Identifier: "X-1", Title: "t", State: "Todo", Dispatchable: false, AssigneeID: "viewer-id",
				AssigneeMismatch: false,
				BlockedBy:        []domain.Blocker{{ID: "b", Identifier: "X-0", State: "In Progress", Dispatchable: false}},
			},
			s: config.Settings{
				Tracker: config.Tracker{ActiveStates: []string{"Todo"}, Provider: map[string]any{"assignee": "me"}},
			},
			reason: "blocked_by_relation",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := ineligibleReason(test.issue, test.s); got != test.reason {
				t.Fatalf("ineligibleReason=%q, want %q", got, test.reason)
			}
		})
	}
}

func TestPollSummaryReportsNoCandidatesAtDebugLevel(t *testing.T) {
	w := testSettings(t)
	tracker := &issueMapTracker{issues: map[string]domain.Issue{}}
	var logs bytes.Buffer
	c := New(tracker, &fakeAgent{}, &fakeWorkspace{}, func() config.Settings { return w.Config }, slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	c.Tick(context.Background())
	output := logs.String()
	if !strings.Contains(output, `"msg":"poll summary"`) || !strings.Contains(output, `"candidates":0`) || !strings.Contains(output, `"eligible":0`) || !strings.Contains(output, `"admitted":0`) {
		t.Fatalf("no-candidate poll summary missing expected counts: %s", output)
	}
}

func TestPollSummaryCategorizesRejectionsAndOmitsAtInfoLevel(t *testing.T) {
	w := testSettings(t)
	ready := testIssue()
	ready.ID, ready.Identifier = "ready", "ENG-3"
	claimed := testIssue()
	claimed.ID, claimed.Identifier = "claimed", "ENG-4"
	tracker := &issueMapTracker{candidates: []domain.Issue{ready, claimed}, issues: map[string]domain.Issue{ready.ID: ready, claimed.ID: claimed}}
	agent := &fakeAgent{events: closedEvents}
	ws := &fakeWorkspace{after: make(chan struct{}, 1)}
	var infoLogs, debugLogs bytes.Buffer
	c := New(tracker, agent, ws, func() config.Settings { return w.Config }, slog.New(slog.NewJSONHandler(&infoLogs, nil)))
	timer := &fakeTimer{signal: make(chan struct{}, 1)}
	c.timer = timer
	if !c.claim(claimed, w.Config) {
		t.Fatal("pre-claim failed")
	}
	c.Tick(context.Background())
	<-timer.signal
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-ws.after
	if output := infoLogs.String(); strings.Contains(output, `"msg":"poll summary"`) {
		t.Fatalf("poll summary debug detail leaked into the default info log: %s", output)
	}

	ready2 := testIssue()
	ready2.ID, ready2.Identifier = "ready2", "ENG-5"
	claimed2 := testIssue()
	claimed2.ID, claimed2.Identifier = "claimed2", "ENG-6"
	tracker2 := &issueMapTracker{candidates: []domain.Issue{ready2, claimed2}, issues: map[string]domain.Issue{ready2.ID: ready2, claimed2.ID: claimed2}}
	agent2 := &fakeAgent{events: closedEvents}
	ws2 := &fakeWorkspace{after: make(chan struct{}, 1)}
	c2 := New(tracker2, agent2, ws2, func() config.Settings { return w.Config }, slog.New(slog.NewJSONHandler(&debugLogs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	timer2 := &fakeTimer{signal: make(chan struct{}, 1)}
	c2.timer = timer2
	if !c2.claim(claimed2, w.Config) {
		t.Fatal("pre-claim failed")
	}
	c2.Tick(context.Background())
	<-timer2.signal
	if err := c2.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-ws2.after
	output := debugLogs.String()
	if !strings.Contains(output, `"candidates":2`) || !strings.Contains(output, `"eligible":2`) || !strings.Contains(output, `"admitted":1`) {
		t.Fatalf("poll summary counts=%s", output)
	}
	if !strings.Contains(output, `"already_claimed":1`) {
		t.Fatalf("poll summary missing categorized rejection: %s", output)
	}
	if !strings.Contains(output, `"issue_identifier":"ENG-6"`) || !strings.Contains(output, `"reason":"already_claimed"`) {
		t.Fatalf("per-issue rejection record missing: %s", output)
	}
}

// TestPollRejectionNamesTheBlockingIssue guards the PMR-146 operator-visibility
// requirement: a Todo issue held non-dispatchable by an open blocker relation
// must be identifiable, and its blocker named, from the poll log -- not lumped
// into the generic not_routable rejection an assignee mismatch or a missing
// required label also produces.
func TestPollRejectionNamesTheBlockingIssue(t *testing.T) {
	w := testSettings(t)
	blocked := testIssue()
	blocked.Dispatchable = false
	blocked.BlockedBy = []domain.Blocker{{ID: "blocker-id", Identifier: "ENG-0", State: "In Progress", Dispatchable: false}}
	tracker := &issueMapTracker{candidates: []domain.Issue{blocked}, issues: map[string]domain.Issue{blocked.ID: blocked}}
	var logs bytes.Buffer
	c := New(tracker, &fakeAgent{}, &fakeWorkspace{}, func() config.Settings { return w.Config }, slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	c.Tick(context.Background())
	output := logs.String()
	if !strings.Contains(output, `"blocked_by_relation":1`) {
		t.Fatalf("poll summary missing blocked_by_relation rejection: %s", output)
	}
	if !strings.Contains(output, `"issue_identifier":"ENG-1"`) || !strings.Contains(output, `"reason":"blocked_by_relation"`) || !strings.Contains(output, `"blocked_by":"ENG-0"`) {
		t.Fatalf("per-issue rejection record missing the blocking issue: %s", output)
	}
}

// TestPollSummaryNeverCarriesIssueIdentifiers guards the privacy property
// pollSummary documents on itself: adding Waiting to the snapshot must not
// leak an identifier into the aggregate debug line the way the per-issue
// "poll candidate rejected" record deliberately does.
func TestPollSummaryNeverCarriesIssueIdentifiers(t *testing.T) {
	w := testSettings(t)
	w.Config.Agent.MaxConcurrent = 1
	occupying := testIssue()
	occupying.ID, occupying.Identifier = "occupying", "ENG-OCCUPY"
	queued := testIssue()
	queued.ID, queued.Identifier = "queued", "ENG-QUEUED"
	tracker := &issueMapTracker{candidates: []domain.Issue{queued}, issues: map[string]domain.Issue{occupying.ID: occupying, queued.ID: queued}}
	var logs bytes.Buffer
	c := New(tracker, &fakeAgent{}, &fakeWorkspace{}, func() config.Settings { return w.Config }, slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	c.mu.Lock()
	c.claimed[occupying.ID] = true
	c.admitted[occupying.ID] = config.Norm(occupying.State)
	c.mu.Unlock()

	c.Tick(context.Background())

	var summaryLine string
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, `"msg":"poll summary"`) {
			summaryLine = line
		}
	}
	if summaryLine == "" {
		t.Fatalf("poll summary line missing: %s", logs.String())
	}
	if !strings.Contains(summaryLine, `"at_capacity":1`) {
		t.Fatalf("poll summary missing the at_capacity rejection: %s", summaryLine)
	}
	for _, prohibited := range []string{"ENG-QUEUED", "ENG-OCCUPY", "issue_identifier", "issue_id"} {
		if strings.Contains(summaryLine, prohibited) {
			t.Fatalf("poll summary leaked an issue identifier: %s", summaryLine)
		}
	}
}

func TestBlankRequiredLabelFailsClosed(t *testing.T) {
	issue := domain.Issue{Dispatchable: true, Labels: []string{"ready"}}
	settings := config.Settings{Tracker: config.Tracker{RequiredLabels: []string{"ready", ""}}}
	if routable(issue, settings) {
		t.Fatal("blank required label allowed an issue to be routed")
	}
}

func TestSortIssuesUsesTotalDeterministicOrder(t *testing.T) {
	older := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	newer := older.Add(time.Hour)
	tests := []struct {
		name      string
		createdAt map[string]*time.Time
		want      []string
	}{
		{name: "both nil", createdAt: map[string]*time.Time{}, want: []string{"PMR-1", "PMR-2"}},
		{name: "one nil", createdAt: map[string]*time.Time{"PMR-2": &older}, want: []string{"PMR-2", "PMR-1"}},
		{name: "equal", createdAt: map[string]*time.Time{"PMR-1": &older, "PMR-2": &older}, want: []string{"PMR-1", "PMR-2"}},
		{name: "distinct", createdAt: map[string]*time.Time{"PMR-1": &newer, "PMR-2": &older}, want: []string{"PMR-2", "PMR-1"}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for _, input := range [][]string{{"PMR-1", "PMR-2"}, {"PMR-2", "PMR-1"}} {
				issues := []domain.Issue{
					{Identifier: input[0], CreatedAt: test.createdAt[input[0]]},
					{Identifier: input[1], CreatedAt: test.createdAt[input[1]]},
				}

				sortIssues(issues)

				got := []string{issues[0].Identifier, issues[1].Identifier}
				if !reflect.DeepEqual(got, test.want) {
					t.Fatalf("sortIssues(%v) = %v, want %v", input, got, test.want)
				}
			}
		})
	}
}

func TestClaimPreventsDuplicateConcurrentLaunches(t *testing.T) {
	w := testSettings(t)
	issue := testIssue()
	block := make(chan domain.Event)
	agent := &fakeAgent{events: func() <-chan domain.Event { return block }, started: make(chan struct{}, 1)}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{}, 1)}
	c := testCoordinator(w.Config, &fakeTracker{issue: issue}, agent, ws)

	c.Tick(context.Background())
	<-agent.started
	c.Tick(context.Background())
	starts, _, _ := agent.counts()
	if starts != 1 {
		t.Fatalf("starts=%d, want one owner", starts)
	}
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-ws.after
}
