package coordinator

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/domain"
)

func TestObservabilityNormalizesEventsAndProtectsSensitiveMessages(t *testing.T) {
	w := testSettings(t)
	issue := testIssue()
	events := make(chan domain.Event, 6)
	events <- domain.Event{Kind: domain.EventSessionStarted, At: time.Now(), SessionID: "session", ThreadID: "thread", TurnID: "turn", PID: 123}
	events <- domain.Event{Kind: domain.EventUsage, At: time.Now(), Usage: domain.Usage{InputTokens: 4, OutputTokens: 6}}
	events <- domain.Event{Kind: domain.EventUsage, At: time.Now(), Usage: domain.Usage{InputTokens: 4, OutputTokens: 6, TotalTokens: 10}}
	events <- domain.Event{Kind: domain.EventRateLimit, At: time.Now(), RateLimit: map[string]any{"remaining": float64(9), "token": "do-not-log-this"}}
	events <- domain.Event{Kind: domain.EventDiagnostic, At: time.Now(), Message: "token=do-not-log-this"}
	events <- domain.Event{Kind: domain.EventCompleted, At: time.Now(), Message: "prompt=do-not-log-this"}
	close(events)
	agent := &fakeAgent{events: func() <-chan domain.Event { return events }}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{})}
	var logs bytes.Buffer
	c := New(&fakeTracker{issue: issue}, agent, ws, func() config.Settings { return w.Config }, slog.New(slog.NewJSONHandler(&logs, nil)))
	c.Tick(context.Background())
	<-ws.after
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	output := logs.String()
	for _, secret := range []string{"do-not-log-this", "prompt=do-not-log-this", issue.Description} {
		if secret != "" && strings.Contains(output, secret) {
			t.Fatalf("operator log leaked %q: %s", secret, output)
		}
	}
	for _, field := range []string{`"thread_id":"thread"`, `"turn_id":"turn"`, `"pid":123`, `"input_tokens":4`, `"output_tokens":6`, `"total_tokens":10`, `"remaining":9`, `"runtime_ms"`} {
		if !strings.Contains(output, field) {
			t.Fatalf("operator log missing %s: %s", field, output)
		}
	}
}

func TestEmptyRateLimitSnapshotIsOmittedFromTheLog(t *testing.T) {
	w := testSettings(t)
	issue := testIssue()
	events := make(chan domain.Event, 2)
	events <- domain.Event{Kind: domain.EventRateLimit, At: time.Now(), RateLimit: map[string]any{"token": "do-not-log-this"}}
	events <- domain.Event{Kind: domain.EventCompleted, At: time.Now()}
	close(events)
	agent := &fakeAgent{events: func() <-chan domain.Event { return events }}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{})}
	var logs bytes.Buffer
	c := New(&fakeTracker{issue: issue}, agent, ws, func() config.Settings { return w.Config }, slog.New(slog.NewJSONHandler(&logs, nil)))
	c.Tick(context.Background())
	<-ws.after
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if output := logs.String(); strings.Contains(output, "agent rate limit") {
		t.Fatalf("empty rate-limit snapshot was logged: %s", output)
	}
}

func TestGenericProgressEventsAreDebugOnlyAndCoalesceRepeats(t *testing.T) {
	w := testSettings(t)
	issue := testIssue()
	events := make(chan domain.Event, 25)
	for i := 0; i < 22; i++ {
		events <- domain.Event{Kind: domain.EventProgress, At: time.Now(), Message: "thread/tokenUsage/updated"}
	}
	events <- domain.Event{Kind: domain.EventCompleted, At: time.Now()}
	close(events)
	agent := &fakeAgent{events: func() <-chan domain.Event { return events }}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{})}
	var infoLogs, debugLogs bytes.Buffer
	c := New(&fakeTracker{issue: issue}, agent, ws, func() config.Settings { return w.Config }, slog.New(slog.NewJSONHandler(&infoLogs, nil)))
	c.Tick(context.Background())
	<-ws.after
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if output := infoLogs.String(); strings.Contains(output, `"msg":"agent event"`) {
		t.Fatalf("generic progress flooded the default info log: %s", output)
	}

	events = make(chan domain.Event, 25)
	for i := 0; i < 22; i++ {
		events <- domain.Event{Kind: domain.EventProgress, At: time.Now(), Message: "thread/tokenUsage/updated"}
	}
	events <- domain.Event{Kind: domain.EventCompleted, At: time.Now()}
	close(events)
	agent = &fakeAgent{events: func() <-chan domain.Event { return events }}
	ws = &fakeWorkspace{shouldRun: true, after: make(chan struct{})}
	c = New(&fakeTracker{issue: issue}, agent, ws, func() config.Settings { return w.Config }, slog.New(slog.NewJSONHandler(&debugLogs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	c.Tick(context.Background())
	<-ws.after
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	count := strings.Count(debugLogs.String(), `"msg":"agent event"`)
	if count == 0 || count >= 22 {
		t.Fatalf("repeated generic progress was not coalesced at debug level: count=%d log=%s", count, debugLogs.String())
	}
}

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
			name:   "eligible",
			issue:  domain.Issue{ID: "a", Identifier: "X-1", Title: "t", State: "Todo", Dispatchable: true},
			s:      config.Settings{Tracker: config.Tracker{ActiveStates: []string{"Todo"}}},
			reason: "",
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

func TestHeartbeatAndStallRecordOutstandingOperation(t *testing.T) {
	w := testSettings(t)
	w.Config.Codex.StallTimeout = time.Second
	issue := testIssue()
	tracker := &fakeTracker{issue: issue}
	events := make(chan domain.Event, 1)
	events <- domain.Event{Kind: domain.EventItem, ItemID: "item-1", ItemType: "commandExecution", Outcome: domain.ItemStarted}
	agent := &fakeAgent{events: func() <-chan domain.Event { return events }, started: make(chan struct{}, 1)}
	ws := &fakeWorkspace{after: make(chan struct{}, 1)}
	logs := &syncBuffer{}
	c := New(tracker, agent, ws, func() config.Settings { return w.Config }, slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	clock := &mutableClock{now: time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)}
	c.clock = clock
	timer := &fakeTimer{signal: make(chan struct{}, 1)}
	c.timer = timer

	c.Tick(context.Background())
	<-agent.started
	waitForSubstring(t, logs, `"msg":"agent item event"`, time.Second)

	clock.set(clock.now.Add(200 * time.Millisecond))
	c.Tick(context.Background())
	heartbeat := waitForSubstring(t, logs, `"msg":"agent heartbeat"`, time.Second)
	if !strings.Contains(heartbeat, `"outstanding_item_type":"commandExecution"`) || !strings.Contains(heartbeat, `"outstanding_item_id":"item-1"`) {
		t.Fatalf("heartbeat missing outstanding operation: %s", heartbeat)
	}

	clock.set(clock.now.Add(2 * time.Second))
	c.Tick(context.Background())
	<-ws.after
	<-timer.signal
	stalled := waitForSubstring(t, logs, `"reason":"stalled"`, time.Second)
	if !strings.Contains(stalled, `"outstanding_item_id":"item-1"`) || !strings.Contains(stalled, `"outstanding_item_type":"commandExecution"`) || !strings.Contains(stalled, `"last_activity_age_ms"`) {
		t.Fatalf("stall record missing outstanding operation: %s", stalled)
	}
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestCleanupStatusClassifiesWorkspaceOutcome(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "clean", err: nil, want: "clean"},
		{name: "dirty", err: errors.New("refusing to remove Git workspace with uncommitted or untracked changes"), want: "dirty"},
		{name: "committed", err: fmt.Errorf("refusing to remove Git workspace whose HEAD %s differs from recorded base commit %s", "abc", "def"), want: "committed"},
		{name: "blocked", err: errors.New("refusing to remove workspace without durable ownership state"), want: "blocked"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := cleanupStatus(test.err); got != test.want {
				t.Fatalf("cleanupStatus=%q, want %q", got, test.want)
			}
		})
	}
}

// TestNewDebugRecordsNeverCarryToolInputsOrSecrets exercises every new debug
// and info log path added for actionable diagnostics (poll summaries, item
// lifecycle, heartbeat/stall records, claim/preparation records, cleanup
// status) with representative secret-shaped values in the fields an operator
// cannot control, and asserts none of them appear in the emitted log.
func TestNewDebugRecordsNeverCarryToolInputsOrSecrets(t *testing.T) {
	w := testSettings(t)
	issue := testIssue()
	issue.Description = "issue description must-not-appear"
	tracker := &fakeTracker{issue: issue}
	events := make(chan domain.Event, 4)
	events <- domain.Event{Kind: domain.EventItem, ItemID: "item-1", ItemType: "commandExecution", ToolName: "token=do-not-log-this", Outcome: domain.ItemStarted}
	events <- domain.Event{Kind: domain.EventItem, ItemID: "item-1", ItemType: "commandExecution", ToolName: "token=do-not-log-this", Outcome: "failed", DurationMs: 5}
	events <- domain.Event{Kind: domain.EventProgress, Message: "prompt=do-not-log-this"}
	events <- domain.Event{Kind: domain.EventCompleted}
	close(events)
	agent := &fakeAgent{events: func() <-chan domain.Event { return events }}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{})}
	var logs bytes.Buffer
	c := New(tracker, agent, ws, func() config.Settings { return w.Config }, slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	c.Tick(context.Background())
	<-ws.after
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	output := logs.String()
	for _, secret := range []string{"do-not-log-this", "must-not-appear"} {
		if strings.Contains(output, secret) {
			t.Fatalf("new debug/info record leaked %q: %s", secret, output)
		}
	}
	if !strings.Contains(output, `"msg":"agent item event"`) || !strings.Contains(output, `"item_type":"commandExecution"`) {
		t.Fatalf("item event record missing: %s", output)
	}
}

// syncBuffer is a concurrency-safe io.Writer/String() pair used to poll a log
// sink that a background coordinator goroutine is still writing to.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}
func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func waitForSubstring(t *testing.T, buf *syncBuffer, substr string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		output := buf.String()
		if strings.Contains(output, substr) {
			return output
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for log containing %q; got: %s", substr, output)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// waitForRunning blocks until identifier appears in c's running snapshot.
// agent.started only confirms Start was called; the launch goroutine still
// needs to record the session in c.running afterward, and a test that
// advances the clock and re-ticks before that happens will see reconcile
// find no running sessions at all, so a stall or eligibility change it
// expects to observe is silently missed and any following blocking receive
// (e.g. <-ws.after) hangs until the test binary's own timeout.
func waitForRunning(t *testing.T, c *Coordinator, identifier string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		for _, r := range c.Snapshot().Running {
			if r.IssueIdentifier == identifier {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("issue %s never appeared in the running snapshot", identifier)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestRenderExplainsHostAndManualDeliveryModes(t *testing.T) {
	settings := config.Settings{Prompt: "Work on {{.issue.identifier}}"}
	issue := domain.Issue{Identifier: "PMR-40"}
	manual, err := render(settings, issue, 0)
	if err != nil || !strings.Contains(manual, "Delivery mode: manual") || !strings.Contains(manual, "Do not run gh, git push") {
		t.Fatalf("manual prompt=%q err=%v", manual, err)
	}
	settings.GitHub.Enabled = true
	settings.Tracker.HandoffState = "In Review"
	host, err := render(settings, issue, 0)
	if err != nil || !strings.Contains(host, "Delivery mode: host-side publish") || !strings.Contains(host, "github_publish_pr with why, what_changed, and on_call") || !strings.Contains(host, "github_pr_context") {
		t.Fatalf("host prompt=%q err=%v", host, err)
	}
}

func TestBlankRequiredLabelFailsClosed(t *testing.T) {
	issue := domain.Issue{Dispatchable: true, Labels: []string{"ready"}}
	settings := config.Settings{Tracker: config.Tracker{RequiredLabels: []string{"ready", ""}}}
	if routable(issue, settings) {
		t.Fatal("blank required label allowed an issue to be routed")
	}
}

func TestSnapshotCopiesOnlySafeOperationalMetadata(t *testing.T) {
	c := New(&fakeTracker{}, &fakeAgent{}, &fakeWorkspace{}, func() config.Settings { return config.Settings{} }, nil)
	now := time.Now()
	c.claimed["provider-id"] = true
	c.running["provider-id"] = &running{issue: domain.Issue{ID: "provider-id", Identifier: "PMR-6", Description: "must-not-appear"}, session: domain.AgentSession{ID: "session", ThreadID: "thread", TurnID: "turn"}, last: now, run: domain.Run{Attempt: 2, TurnCount: 1, StartedAt: now, Usage: domain.Usage{InputTokens: 1}}, rateLimit: map[string]int64{"remaining": 2}}
	c.retries["retry-id"] = retryState{issue: domain.Issue{ID: "retry-id", Identifier: "PMR-9", Description: "must-not-appear"}, attempt: 3, kind: retryAgent, reason: "agent_event", due: now}
	snapshot := c.Snapshot()
	if snapshot.Claimed != 1 || len(snapshot.Running) != 1 || len(snapshot.Retrying) != 1 {
		t.Fatalf("snapshot=%+v", snapshot)
	}
	if snapshot.Running[0].IssueIdentifier != "PMR-6" || snapshot.Running[0].RateLimit["remaining"] != 2 || snapshot.Retrying[0].IssueIdentifier != "PMR-9" {
		t.Fatalf("snapshot=%+v", snapshot)
	}
	snapshot.Running[0].RateLimit["remaining"] = 99
	if c.running["provider-id"].rateLimit["remaining"] != 2 {
		t.Fatal("snapshot mutated live coordinator state")
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

type fakeTracker struct {
	mu            sync.Mutex
	issue         domain.Issue
	fresh         domain.Issue
	hasFresh      bool
	gets          int
	transitions   []trackerTransition
	transitionErr error
}

// trackerTransition records one host-side dispatch transition request so tests
// can assert the coordinator moved an issue into its started state (or did not).
type trackerTransition struct{ id, from, to string }

func (f *fakeTracker) ListCandidates(context.Context, []string) ([]domain.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return []domain.Issue{f.issue}, nil
}
func (f *fakeTracker) GetIssues(context.Context, []string) ([]domain.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets++
	if f.hasFresh {
		return []domain.Issue{f.fresh}, nil
	}
	return []domain.Issue{f.issue}, nil
}

func (f *fakeTracker) getCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gets
}
func (f *fakeTracker) ListTerminal(context.Context, []string) ([]domain.Issue, error) {
	return nil, nil
}
func (f *fakeTracker) Transition(_ context.Context, issue domain.Issue, toState string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.transitions = append(f.transitions, trackerTransition{id: issue.ID, from: issue.State, to: toState})
	return f.transitionErr
}
func (f *fakeTracker) transitionCalls() []trackerTransition {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]trackerTransition(nil), f.transitions...)
}
func (f *fakeTracker) setIssue(issue domain.Issue) {
	f.mu.Lock()
	f.issue = issue
	f.mu.Unlock()
}
func (f *fakeTracker) setFresh(issue domain.Issue) {
	f.mu.Lock()
	f.fresh = issue
	f.hasFresh = true
	f.mu.Unlock()
}

// issueMapTracker makes capacity and reconciliation tests deterministic when
// more than one issue is eligible at the same time.
type issueMapTracker struct {
	mu         sync.Mutex
	candidates []domain.Issue
	issues     map[string]domain.Issue
	getErr     error
}

func (t *issueMapTracker) ListCandidates(context.Context, []string) ([]domain.Issue, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]domain.Issue(nil), t.candidates...), nil
}

func (t *issueMapTracker) GetIssues(_ context.Context, ids []string) ([]domain.Issue, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.getErr != nil {
		return nil, t.getErr
	}
	issues := make([]domain.Issue, 0, len(ids))
	for _, id := range ids {
		if issue, ok := t.issues[id]; ok {
			issues = append(issues, issue)
		}
	}
	return issues, nil
}

func (*issueMapTracker) ListTerminal(context.Context, []string) ([]domain.Issue, error) {
	return nil, nil
}

func (*issueMapTracker) Transition(context.Context, domain.Issue, string) error {
	return nil
}

func (t *issueMapTracker) setIssue(issue domain.Issue) {
	t.mu.Lock()
	t.issues[issue.ID] = issue
	for index, candidate := range t.candidates {
		if candidate.ID == issue.ID {
			t.candidates[index] = issue
		}
	}
	t.mu.Unlock()
}

type fakeAgent struct {
	mu                 sync.Mutex
	starts             int
	continues          int
	cancels            int
	started            chan struct{}
	events             func() <-chan domain.Event
	continuationEvents []func() <-chan domain.Event
	continueErr        error
	continueSessions   []domain.AgentSession
	continuePrompts    []string
	onContinue         func(int)
}

func (f *fakeAgent) Start(context.Context, domain.AgentRequest) (domain.AgentSession, <-chan domain.Event, error) {
	f.mu.Lock()
	f.starts++
	started := f.started
	f.mu.Unlock()
	if started != nil {
		started <- struct{}{}
	}
	return domain.AgentSession{ID: "t-u", ThreadID: "t", TurnID: "u"}, f.events(), nil
}
func (f *fakeAgent) Continue(_ context.Context, session domain.AgentSession, prompt string) (<-chan domain.Event, error) {
	f.mu.Lock()
	index := f.continues
	f.continues++
	f.continueSessions = append(f.continueSessions, session)
	f.continuePrompts = append(f.continuePrompts, prompt)
	events := f.continuationEvents
	err := f.continueErr
	onContinue := f.onContinue
	f.mu.Unlock()
	if onContinue != nil {
		onContinue(index)
	}
	if err != nil {
		return nil, err
	}
	if index >= len(events) {
		return closedEvents(), nil
	}
	return events[index](), nil
}
func (f *fakeAgent) Cancel(context.Context, domain.AgentSession) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancels++
	return nil
}
func (f *fakeAgent) counts() (starts, continues, cancels int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.starts, f.continues, f.cancels
}

func (f *fakeAgent) continuations() ([]domain.AgentSession, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]domain.AgentSession(nil), f.continueSessions...), append([]string(nil), f.continuePrompts...)
}

type fakeWorkspace struct {
	mu             sync.Mutex
	shouldRun      bool
	shouldRunCalls int
	prepares       int
	marks          int
	cleanups       int
	cleaned        chan struct{}
	marked         chan struct{}
	markErr        error
	after          chan struct{}
	prepareStarted chan struct{}
	prepareGate    <-chan struct{}
}

func (f *fakeWorkspace) Prepare(ctx context.Context, _ domain.Issue) (domain.Workspace, error) {
	f.mu.Lock()
	f.prepares++
	started := f.prepareStarted
	gate := f.prepareGate
	f.mu.Unlock()
	if started != nil {
		started <- struct{}{}
	}
	if gate != nil {
		select {
		case <-ctx.Done():
			return domain.Workspace{}, ctx.Err()
		case <-gate:
		}
	}
	return domain.Workspace{Path: "/tmp/work"}, nil
}
func (f *fakeWorkspace) BeforeRun(context.Context, domain.Workspace, domain.Issue) error { return nil }
func (f *fakeWorkspace) AfterRun(context.Context, domain.Workspace, domain.Issue) {
	if f.after != nil {
		f.after <- struct{}{}
	}
}
func (f *fakeWorkspace) Cleanup(context.Context, domain.Issue) error {
	f.mu.Lock()
	f.cleanups++
	cleaned := f.cleaned
	f.mu.Unlock()
	if cleaned != nil {
		cleaned <- struct{}{}
	}
	return nil
}
func (f *fakeWorkspace) Execute(context.Context, domain.Workspace, string, []string) ([]byte, error) {
	return nil, nil
}
func (f *fakeWorkspace) counts() (prepares, marks, cleanups, checks int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.prepares, f.marks, f.cleanups, f.shouldRunCalls
}

type fakeTimer struct {
	mu      sync.Mutex
	delays  []time.Duration
	entries []fakeTimerEntry
	signal  chan struct{}
}
type fakeTimerEntry struct {
	callback func()
	stopped  bool
}
type fakeTimerHandle struct {
	timer *fakeTimer
	index int
}

func (f *fakeTimer) AfterFunc(d time.Duration, callback func()) TimerHandle {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.delays = append(f.delays, d)
	f.entries = append(f.entries, fakeTimerEntry{callback: callback})
	if f.signal != nil {
		f.signal <- struct{}{}
	}
	return fakeTimerHandle{timer: f, index: len(f.entries) - 1}
}
func (h fakeTimerHandle) Stop() bool {
	h.timer.mu.Lock()
	defer h.timer.mu.Unlock()
	if h.index >= len(h.timer.entries) || h.timer.entries[h.index].stopped {
		return false
	}
	h.timer.entries[h.index].stopped = true
	return true
}
func (f *fakeTimer) fire(index int) {
	f.mu.Lock()
	entry := f.entries[index]
	f.mu.Unlock()
	if !entry.stopped {
		entry.callback()
	}
}

// fireStale simulates a callback that was already queued when Stop raced with
// it. The coordinator must still reject it by generation.
func (f *fakeTimer) fireStale(index int) {
	f.mu.Lock()
	callback := f.entries[index].callback
	f.mu.Unlock()
	callback()
}
func (f *fakeTimer) scheduled() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.entries)
}

type fakeClock struct{ now time.Time }

func (f fakeClock) Now() time.Time { return f.now }

type mutableClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *mutableClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *mutableClock) set(now time.Time) {
	c.mu.Lock()
	c.now = now
	c.mu.Unlock()
}

type retryPollError struct{ delay time.Duration }

func (e retryPollError) Error() string             { return "rate limited" }
func (e retryPollError) RetryDelay() time.Duration { return e.delay }

type rateLimitPollTracker struct {
	mu    sync.Mutex
	calls int
}

func (t *rateLimitPollTracker) ListCandidates(context.Context, []string) ([]domain.Issue, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.calls++
	if t.calls == 1 {
		return nil, retryPollError{delay: 2 * time.Minute}
	}
	return nil, nil
}
func (*rateLimitPollTracker) GetIssues(context.Context, []string) ([]domain.Issue, error) {
	return nil, nil
}
func (*rateLimitPollTracker) ListTerminal(context.Context, []string) ([]domain.Issue, error) {
	return nil, nil
}
func (*rateLimitPollTracker) Transition(context.Context, domain.Issue, string) error {
	return nil
}

func TestStartSchedulesRateLimitRecoveryWithInjectedTimer(t *testing.T) {
	w := testSettings(t)
	w.Config.Polling.Interval = 30 * time.Second
	tracker := &rateLimitPollTracker{}
	c := testCoordinator(w.Config, tracker, &fakeAgent{}, &fakeWorkspace{})
	timer := &fakeTimer{signal: make(chan struct{}, 2)}
	c.timer = timer
	ctx, cancel := context.WithCancel(context.Background())
	c.Start(ctx)
	<-timer.signal
	timer.mu.Lock()
	firstDelay := timer.delays[0]
	timer.mu.Unlock()
	if firstDelay != 2*time.Minute {
		t.Fatalf("rate-limit delay=%v want 2m", firstDelay)
	}
	timer.fire(0)
	<-timer.signal
	timer.mu.Lock()
	secondDelay := timer.delays[1]
	timer.mu.Unlock()
	if secondDelay != 30*time.Second {
		t.Fatalf("recovery delay=%v want 30s", secondDelay)
	}
	cancel()
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestActiveIssueAtTurnLimitIsBlockedAndRetriedWithoutCompletionMarker(t *testing.T) {
	w := testSettings(t)
	issue := testIssue()
	agent := &fakeAgent{events: completedEvents}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{}, 1)}
	c := testCoordinator(w.Config, &fakeTracker{issue: issue}, agent, ws)
	timer := &fakeTimer{signal: make(chan struct{}, 1)}
	c.timer = timer

	c.Tick(context.Background())
	<-ws.after
	<-timer.signal

	starts, continues, cancels := agent.counts()
	if starts != 1 || continues != 0 || cancels != 1 {
		t.Fatalf("starts=%d continues=%d cancels=%d", starts, continues, cancels)
	}
	_, marks, _, _ := ws.counts()
	if marks != 0 {
		t.Fatalf("completion markers=%d, want 0 for an active exhausted issue", marks)
	}
	c.mu.Lock()
	retry := c.retries[issue.ID]
	c.mu.Unlock()
	if retry.kind != retryAgent || retry.reason != "turn_limit_exhausted" || retry.attempt != 1 {
		t.Fatalf("retry=%+v", retry)
	}
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestBoundedRunRefreshesAndContinuesSameSessionToExactMaxTurns(t *testing.T) {
	w := testSettings(t)
	w.Config.Agent.MaxTurns = 3
	issue := testIssue()
	tracker := &fakeTracker{issue: issue}
	observedRefreshes := make([]int, 0, 2)
	var observedMu sync.Mutex
	agent := &fakeAgent{
		events:             completedEvents,
		continuationEvents: []func() <-chan domain.Event{completedEvents, completedEvents},
		onContinue: func(_ int) {
			observedMu.Lock()
			observedRefreshes = append(observedRefreshes, tracker.getCount())
			observedMu.Unlock()
		},
	}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{}, 1)}
	c := testCoordinator(w.Config, tracker, agent, ws)
	timer := &fakeTimer{signal: make(chan struct{}, 3)}
	c.timer = timer

	c.Tick(context.Background())
	for index := 0; index < 2; index++ {
		<-timer.signal
		timer.fire(index)
	}
	<-ws.after
	<-timer.signal

	starts, continues, cancels := agent.counts()
	if starts != 1 || continues != 2 || cancels != 1 {
		t.Fatalf("starts=%d continues=%d cancels=%d", starts, continues, cancels)
	}
	sessions, prompts := agent.continuations()
	wantSession := (domain.AgentSession{ID: "t-u", ThreadID: "t", TurnID: "u"})
	if len(sessions) != 2 || sessions[0] != wantSession || sessions[1] != wantSession {
		t.Fatalf("continuation sessions=%+v, want same initial session twice", sessions)
	}
	for index, prompt := range prompts {
		want := continuationGuidance(index+2, 3)
		if prompt != want {
			t.Fatalf("continuation prompt %d=%q, want configured guidance %q", index+2, prompt, want)
		}
	}
	observedMu.Lock()
	refreshes := append([]int(nil), observedRefreshes...)
	observedMu.Unlock()
	if !reflect.DeepEqual(refreshes, []int{1, 2}) {
		t.Fatalf("tracker refresh counts at continuation=%v, want [1 2]", refreshes)
	}
	if tracker.getCount() != 3 {
		t.Fatalf("tracker refreshes=%d, want one after each turn", tracker.getCount())
	}
	_, marks, _, _ := ws.counts()
	if marks != 0 {
		t.Fatalf("completion markers=%d, want 0 for an active exhausted issue", marks)
	}
	c.mu.Lock()
	retry := c.retries[issue.ID]
	c.mu.Unlock()
	if retry.kind != retryAgent || retry.reason != "turn_limit_exhausted" || retry.attempt != 1 {
		t.Fatalf("retry=%+v", retry)
	}
}

func TestBlockedEventStopsContinuationAndLogsSafeRetryReason(t *testing.T) {
	w := testSettings(t)
	w.Config.Agent.MaxTurns = 3
	issue := testIssue()
	events := make(chan domain.Event, 1)
	events <- domain.Event{Kind: domain.EventBlocked, At: time.Now(), Message: "Codex GitHub publication request was rejected"}
	close(events)
	agent := &fakeAgent{events: func() <-chan domain.Event { return events }}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{}, 1)}
	var logs bytes.Buffer
	c := New(&fakeTracker{issue: issue}, agent, ws, func() config.Settings { return w.Config }, slog.New(slog.NewJSONHandler(&logs, nil)))
	c.clock = fakeClock{now: time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)}
	timer := &fakeTimer{signal: make(chan struct{}, 1)}
	c.timer = timer

	c.Tick(context.Background())
	<-ws.after
	<-timer.signal

	starts, continues, cancels := agent.counts()
	if starts != 1 || continues != 0 || cancels != 1 {
		t.Fatalf("starts=%d continues=%d cancels=%d", starts, continues, cancels)
	}
	c.mu.Lock()
	retry := c.retries[issue.ID]
	c.mu.Unlock()
	if retry.kind != retryAgent || retry.reason != "agent_blocked" || retry.attempt != 1 {
		t.Fatalf("retry=%+v", retry)
	}
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	output := logs.String()
	if !strings.Contains(output, `"blocker":"github_publication"`) {
		t.Fatalf("blocked retry did not identify its safe category: %s", output)
	}
	if strings.Contains(output, "Codex GitHub publication request was rejected") {
		t.Fatalf("blocked retry logged raw event text: %s", output)
	}
}

func TestBoundedRunStopsAtHandoffWithoutContinuationOrMarker(t *testing.T) {
	w := testSettings(t)
	w.Config.Agent.MaxTurns = 3
	w.Config.Tracker.HandoffState = "Review"
	issue := testIssue()
	handoff := issue
	handoff.State = "Review"
	handoff.Dispatchable = false
	tracker := &fakeTracker{issue: issue}
	tracker.setFresh(handoff)
	agent := &fakeAgent{events: completedEvents}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{}, 1)}
	c := testCoordinator(w.Config, tracker, agent, ws)
	timer := &fakeTimer{}
	c.timer = timer

	c.Tick(context.Background())
	<-ws.after

	starts, continues, cancels := agent.counts()
	if starts != 1 || continues != 0 || cancels != 1 {
		t.Fatalf("handoff starts=%d continues=%d cancels=%d", starts, continues, cancels)
	}
	_, marks, _, _ := ws.counts()
	if marks != 0 || timer.scheduled() != 0 {
		t.Fatalf("handoff markers=%d timers=%d, want neither", marks, timer.scheduled())
	}
}

func TestBoundedRunCancellationDuringContinuationDelayStopsCleanly(t *testing.T) {
	w := testSettings(t)
	w.Config.Agent.MaxTurns = 2
	issue := testIssue()
	agent := &fakeAgent{events: completedEvents, continuationEvents: []func() <-chan domain.Event{completedEvents}}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{}, 1)}
	c := testCoordinator(w.Config, &fakeTracker{issue: issue}, agent, ws)
	timer := &fakeTimer{signal: make(chan struct{}, 1)}
	c.timer = timer

	c.Tick(context.Background())
	<-timer.signal
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-ws.after

	starts, continues, cancels := agent.counts()
	if starts != 1 || continues != 0 || cancels != 1 {
		t.Fatalf("cancelled starts=%d continues=%d cancels=%d", starts, continues, cancels)
	}
	_, marks, _, _ := ws.counts()
	if marks != 0 {
		t.Fatalf("cancelled completion markers=%d, want 0", marks)
	}
}

func TestBoundedRunContinuationFailureStopsSessionAndUsesFailureRetry(t *testing.T) {
	w := testSettings(t)
	w.Config.Agent.MaxTurns = 2
	issue := testIssue()
	agent := &fakeAgent{events: completedEvents, continueErr: errors.New("continuation unavailable")}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{}, 1)}
	c := testCoordinator(w.Config, &fakeTracker{issue: issue}, agent, ws)
	timer := &fakeTimer{signal: make(chan struct{}, 2)}
	c.timer = timer

	c.Tick(context.Background())
	<-timer.signal
	timer.fire(0)
	<-ws.after
	<-timer.signal

	starts, continues, cancels := agent.counts()
	if starts != 1 || continues != 1 || cancels != 1 {
		t.Fatalf("failed continuation starts=%d continues=%d cancels=%d", starts, continues, cancels)
	}
	_, marks, _, _ := ws.counts()
	if marks != 0 {
		t.Fatalf("failed continuation markers=%d, want 0", marks)
	}
	c.mu.Lock()
	retry := c.retries[issue.ID]
	c.mu.Unlock()
	if retry.kind != retryAgent || retry.reason != "agent_event" || retry.attempt != 1 {
		t.Fatalf("failed continuation retry=%+v", retry)
	}
}

func TestClosedEventStreamSchedulesDeterministicAgentRetry(t *testing.T) {
	w := testSettings(t)
	issue := testIssue()
	agent := &fakeAgent{events: closedEvents}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{}, 1)}
	c := testCoordinator(w.Config, &fakeTracker{issue: issue}, agent, ws)
	timer := &fakeTimer{signal: make(chan struct{}, 1)}
	c.timer = timer

	c.Tick(context.Background())
	<-timer.signal
	if timer.scheduled() != 1 || timer.delays[0] != 10*time.Second {
		t.Fatalf("retries=%v", timer.delays)
	}
	c.mu.Lock()
	retry := c.retries[issue.ID]
	c.mu.Unlock()
	if retry.kind != retryAgent || retry.reason != "agent_event" || retry.attempt != 1 {
		t.Fatalf("retry=%+v", retry)
	}
}

func TestRetryRechecksCurrentIssueEligibility(t *testing.T) {
	w := testSettings(t)
	issue := testIssue()
	tracker := &fakeTracker{issue: issue}
	agent := &fakeAgent{events: closedEvents}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{}, 1)}
	c := testCoordinator(w.Config, tracker, agent, ws)
	timer := &fakeTimer{signal: make(chan struct{}, 1)}
	c.timer = timer

	c.Tick(context.Background())
	<-timer.signal
	terminal := issue
	terminal.State = "Done"
	terminal.Dispatchable = false
	tracker.setIssue(terminal)
	timer.fire(0)

	starts, _, _ := agent.counts()
	if starts != 1 {
		t.Fatalf("starts=%d, retry ran stale issue", starts)
	}
	_, _, cleanups, _ := ws.counts()
	if cleanups != 1 {
		t.Fatalf("terminal retry cleanups=%d, want 1", cleanups)
	}
}

func TestStoppedRetryCallbackCannotReclaimIssue(t *testing.T) {
	w := testSettings(t)
	issue := testIssue()
	agent := &fakeAgent{events: completedEvents}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{}, 1)}
	c := testCoordinator(w.Config, &fakeTracker{issue: issue}, agent, ws)
	timer := &fakeTimer{}
	c.timer = timer
	if !c.claim(issue, w.Config) {
		t.Fatal("claim failed")
	}
	c.scheduleRetry(context.Background(), issue, domain.Workspace{}, 1, retryAgent, "test", time.Second)
	c.release(issue.ID)
	timer.fireStale(0)

	starts, _, _ := agent.counts()
	if starts != 0 {
		t.Fatalf("stale callback started %d sessions", starts)
	}
}

func TestReconciliationCancellationDoesNotRetry(t *testing.T) {
	w := testSettings(t)
	issue := testIssue()
	tracker := &fakeTracker{issue: issue}
	block := make(chan domain.Event)
	agent := &fakeAgent{events: func() <-chan domain.Event { return block }, started: make(chan struct{}, 1)}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{}, 1)}
	c := testCoordinator(w.Config, tracker, agent, ws)
	timer := &fakeTimer{}
	c.timer = timer

	c.Tick(context.Background())
	<-agent.started
	waitForRunning(t, c, issue.Identifier)
	ineligible := issue
	ineligible.Dispatchable = false
	tracker.setIssue(ineligible)
	c.Tick(context.Background())
	<-ws.after

	if timer.scheduled() != 0 {
		t.Fatalf("reconciliation cancellation scheduled retries=%d", timer.scheduled())
	}
	starts, _, cancels := agent.counts()
	if starts != 1 || cancels != 1 {
		t.Fatalf("starts=%d cancels=%d", starts, cancels)
	}
}

func TestCompletedEventAfterReconciliationCancellationDoesNotComplete(t *testing.T) {
	w := testSettings(t)
	issue := testIssue()
	agent := &fakeAgent{events: completedEvents}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{}, 1)}
	c := testCoordinator(w.Config, &fakeTracker{issue: issue}, agent, ws)
	r := &running{issue: issue, stopped: stopTerminal}
	events := make(chan domain.Event, 1)
	events <- domain.Event{Kind: domain.EventCompleted, SessionID: "t-u"}
	close(events)

	completed, err := c.consume(context.Background(), r, events)
	if completed {
		t.Fatal("completed event after reconciliation cancellation was accepted")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v, want context cancellation", err)
	}
}

func TestCompletionRevalidatesTerminalIssueBeforeMarker(t *testing.T) {
	w := testSettings(t)
	issue := testIssue()
	tracker := &fakeTracker{issue: issue}
	terminal := issue
	terminal.State = "Done"
	terminal.Dispatchable = false
	tracker.setFresh(terminal)
	agent := &fakeAgent{events: completedEvents}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{}, 1), cleaned: make(chan struct{}, 1)}
	c := testCoordinator(w.Config, tracker, agent, ws)

	c.Tick(context.Background())
	<-ws.cleaned
	_, marks, cleanups, _ := ws.counts()
	if marks != 0 {
		t.Fatalf("completion marker writes=%d, want 0 for terminal issue", marks)
	}
	if cleanups != 1 {
		t.Fatalf("terminal workspace cleanups=%d, want 1", cleanups)
	}
}

func TestShutdownCancellationDoesNotRetry(t *testing.T) {
	w := testSettings(t)
	issue := testIssue()
	block := make(chan domain.Event)
	agent := &fakeAgent{events: func() <-chan domain.Event { return block }, started: make(chan struct{}, 1)}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{}, 1)}
	c := testCoordinator(w.Config, &fakeTracker{issue: issue}, agent, ws)
	timer := &fakeTimer{}
	c.timer = timer

	c.Tick(context.Background())
	<-agent.started
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-ws.after
	if timer.scheduled() != 0 {
		t.Fatalf("shutdown cancellation scheduled retries=%d", timer.scheduled())
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

func TestQueuedRetryDoesNotConsumeAnOrchestratorSlot(t *testing.T) {
	w := testSettings(t)
	retrying := testIssue()
	retrying.ID, retrying.Identifier = "retrying", "ENG-2"
	ready := testIssue()
	ready.ID, ready.Identifier = "ready", "ENG-3"
	tracker := &issueMapTracker{candidates: []domain.Issue{ready}, issues: map[string]domain.Issue{retrying.ID: retrying, ready.ID: ready}}
	block := make(chan domain.Event)
	agent := &fakeAgent{events: func() <-chan domain.Event { return block }, started: make(chan struct{}, 1)}
	ws := &fakeWorkspace{after: make(chan struct{}, 1)}
	c := testCoordinator(w.Config, tracker, agent, ws)
	timer := &fakeTimer{}
	c.timer = timer

	if !c.claim(retrying, w.Config) {
		t.Fatal("retrying issue was not claimed")
	}
	c.scheduleRetry(context.Background(), retrying, domain.Workspace{}, 1, retryAgent, "test", time.Minute)
	c.Tick(context.Background())
	<-agent.started

	starts, _, _ := agent.counts()
	if starts != 1 {
		t.Fatalf("starts=%d, want unrelated ready issue to use the slot", starts)
	}
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-ws.after
}

func TestLaunchReservationPreventsOversubscriptionBeforeSessionStart(t *testing.T) {
	w := testSettings(t)
	first := testIssue()
	second := testIssue()
	second.ID, second.Identifier = "second", "ENG-2"
	gate := make(chan struct{})
	ws := &fakeWorkspace{prepareStarted: make(chan struct{}, 1), prepareGate: gate, after: make(chan struct{}, 1)}
	block := make(chan domain.Event)
	agent := &fakeAgent{events: func() <-chan domain.Event { return block }, started: make(chan struct{}, 1)}
	c := testCoordinator(w.Config, &fakeTracker{issue: first}, agent, ws)

	if !c.claim(first, w.Config) || !c.launch(context.Background(), first, 0) {
		t.Fatal("first launch was not admitted")
	}
	<-ws.prepareStarted
	if c.claim(second, w.Config) {
		t.Fatal("second issue claimed a slot while first preparation had reserved it")
	}
	close(gate)
	<-agent.started
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-ws.after
}

func TestRetryAtCapacityRequeuesWithBoundedBackoff(t *testing.T) {
	w := testSettings(t)
	w.Config.Agent.MaxRetryBackoff = 15 * time.Second
	retrying := testIssue()
	retrying.ID, retrying.Identifier = "retrying", "ENG-2"
	running := testIssue()
	running.ID, running.Identifier = "running", "ENG-3"
	tracker := &issueMapTracker{issues: map[string]domain.Issue{retrying.ID: retrying, running.ID: running}}
	block := make(chan domain.Event)
	agent := &fakeAgent{events: func() <-chan domain.Event { return block }, started: make(chan struct{}, 1)}
	ws := &fakeWorkspace{after: make(chan struct{}, 1)}
	c := testCoordinator(w.Config, tracker, agent, ws)
	timer := &fakeTimer{}
	c.timer = timer

	if !c.claim(retrying, w.Config) {
		t.Fatal("retrying issue was not claimed")
	}
	c.scheduleRetry(context.Background(), retrying, domain.Workspace{}, 1, retryAgent, "test", time.Second)
	if !c.claim(running, w.Config) || !c.launch(context.Background(), running, 0) {
		t.Fatal("running issue was not admitted")
	}
	<-agent.started
	timer.fire(0)

	c.mu.Lock()
	retry := c.retries[retrying.ID]
	c.mu.Unlock()
	if retry.reason != "no available orchestrator slots" || retry.attempt != 2 {
		t.Fatalf("retry=%+v", retry)
	}
	if len(timer.delays) != 2 || timer.delays[1] != 15*time.Second {
		t.Fatalf("retry delays=%v, want capped 15s second retry", timer.delays)
	}
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-ws.after
}

func TestRetryRefreshFailureIncrementsAttemptAndRetries(t *testing.T) {
	w := testSettings(t)
	w.Config.Agent.MaxRetryBackoff = 15 * time.Second
	issue := testIssue()
	tracker := &issueMapTracker{issues: map[string]domain.Issue{issue.ID: issue}, getErr: errors.New("temporary tracker failure")}
	c := testCoordinator(w.Config, tracker, &fakeAgent{}, &fakeWorkspace{})
	timer := &fakeTimer{}
	c.timer = timer

	if !c.claim(issue, w.Config) {
		t.Fatal("issue was not claimed")
	}
	c.scheduleRetry(context.Background(), issue, domain.Workspace{}, 1, retryAgent, "test", time.Second)
	timer.fire(0)

	c.mu.Lock()
	retry := c.retries[issue.ID]
	c.mu.Unlock()
	if retry.reason != "retry_refresh" || retry.attempt != 2 {
		t.Fatalf("retry=%+v", retry)
	}
	if len(timer.delays) != 2 || timer.delays[1] != 15*time.Second {
		t.Fatalf("retry delays=%v, want capped 15s refresh retry", timer.delays)
	}
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestStalledRunCancelsAndSchedulesRetry(t *testing.T) {
	w := testSettings(t)
	w.Config.Codex.StallTimeout = time.Second
	issue := testIssue()
	tracker := &fakeTracker{issue: issue}
	block := make(chan domain.Event)
	agent := &fakeAgent{events: func() <-chan domain.Event { return block }, started: make(chan struct{}, 1)}
	ws := &fakeWorkspace{after: make(chan struct{}, 1)}
	c := testCoordinator(w.Config, tracker, agent, ws)
	clock := &mutableClock{now: time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)}
	c.clock = clock
	timer := &fakeTimer{signal: make(chan struct{}, 1)}
	c.timer = timer

	c.Tick(context.Background())
	<-agent.started
	waitForRunning(t, c, issue.Identifier)
	clock.set(time.Date(2026, 7, 18, 12, 0, 2, 0, time.UTC))
	c.Tick(context.Background())
	<-ws.after
	<-timer.signal

	starts, _, cancels := agent.counts()
	if starts != 1 || cancels != 1 {
		t.Fatalf("starts=%d cancels=%d, want stalled session cancelled once", starts, cancels)
	}
	c.mu.Lock()
	retry := c.retries[issue.ID]
	c.mu.Unlock()
	if retry.reason != "stalled" || retry.attempt != 1 {
		t.Fatalf("retry=%+v", retry)
	}
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestReconciliationRefreshesStateCapacityForLaterAdmissions(t *testing.T) {
	w := testSettings(t)
	w.Config.Agent.MaxConcurrent = 2
	w.Config.Tracker.ActiveStates = []string{"Todo", "Doing"}
	w.Config.Agent.ByState = map[string]int{"todo": 1, "doing": 1}
	first := testIssue()
	second := testIssue()
	second.ID, second.Identifier = "second", "ENG-2"
	tracker := &issueMapTracker{candidates: []domain.Issue{first, second}, issues: map[string]domain.Issue{first.ID: first, second.ID: second}}
	block := make(chan domain.Event)
	agent := &fakeAgent{events: func() <-chan domain.Event { return block }, started: make(chan struct{}, 2)}
	ws := &fakeWorkspace{after: make(chan struct{}, 2)}
	c := testCoordinator(w.Config, tracker, agent, ws)

	c.Tick(context.Background())
	<-agent.started
	waitForRunning(t, c, first.Identifier)
	fresh := first
	fresh.State = "Doing"
	tracker.setIssue(fresh)
	c.Tick(context.Background())
	<-agent.started

	c.mu.Lock()
	state := c.admitted[first.ID]
	c.mu.Unlock()
	if state != "doing" {
		t.Fatalf("first admitted state=%q, want refreshed doing", state)
	}
	starts, _, _ := agent.counts()
	if starts != 2 {
		t.Fatalf("starts=%d, want Todo admission after first moved to Doing", starts)
	}
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-ws.after
	<-ws.after
}

// TestMergingAndUnrelatedImplementationRunConcurrentlyUnderByStateCapacity
// exercises the PMR-38 two-agent rollout end to end at the coordinator level:
// one Merging landing agent and one unrelated implementation agent admit and
// run at the same time, a queued retry timer for a third unrelated issue
// never occupies a concurrency slot while it waits (so a later genuinely
// free slot still admits a new candidate), and max_concurrent_agents_by_state
// still refuses a second concurrent Merging issue even though overall
// capacity has spare room.
func TestMergingAndUnrelatedImplementationRunConcurrentlyUnderByStateCapacity(t *testing.T) {
	w := testSettings(t)
	w.Config.Agent.MaxConcurrent = 3
	w.Config.Tracker.ActiveStates = []string{"In Progress", "Merging"}
	w.Config.Agent.ByState = map[string]int{"merging": 1}

	implementation := testIssue()
	implementation.ID, implementation.Identifier, implementation.State = "impl", "ENG-1", "In Progress"
	landing := testIssue()
	landing.ID, landing.Identifier, landing.State = "landing", "ENG-2", "Merging"
	secondLanding := testIssue()
	secondLanding.ID, secondLanding.Identifier, secondLanding.State = "landing-2", "ENG-3", "Merging"
	retryable := testIssue()
	retryable.ID, retryable.Identifier, retryable.State = "retryable", "ENG-4", "In Progress"
	extra := testIssue()
	extra.ID, extra.Identifier, extra.State = "extra", "ENG-5", "In Progress"

	tracker := &issueMapTracker{
		candidates: []domain.Issue{implementation, landing},
		issues: map[string]domain.Issue{
			implementation.ID: implementation, landing.ID: landing, secondLanding.ID: secondLanding,
			retryable.ID: retryable, extra.ID: extra,
		},
	}
	block := make(chan domain.Event)
	agent := &fakeAgent{events: func() <-chan domain.Event { return block }, started: make(chan struct{}, 3)}
	ws := &fakeWorkspace{after: make(chan struct{}, 3)}
	c := testCoordinator(w.Config, tracker, agent, ws)
	timer := &fakeTimer{}
	c.timer = timer

	// One unrelated implementation issue and one Merging landing issue admit
	// and launch together in the same poll.
	c.Tick(context.Background())
	<-agent.started
	<-agent.started
	if starts, _, _ := agent.counts(); starts != 2 {
		t.Fatalf("starts=%d, want the unrelated implementation and Merging issues both admitted", starts)
	}

	// A queued retry for a third, unrelated issue must not occupy a
	// concurrency slot while it waits.
	if !c.claim(retryable, w.Config) {
		t.Fatal("retryable issue was not claimed")
	}
	c.scheduleRetry(context.Background(), retryable, domain.Workspace{}, 1, retryAgent, "test", time.Minute)
	c.mu.Lock()
	admitted := len(c.admitted)
	c.mu.Unlock()
	if admitted != 2 {
		t.Fatalf("admitted=%d, want a queued retry timer to consume no concurrency slot", admitted)
	}

	// A second concurrent Merging issue is refused by the per-state cap even
	// though overall capacity (2 of 3) still has room.
	if c.claim(secondLanding, w.Config) {
		t.Fatal("a second concurrent Merging issue must be refused by max_concurrent_agents_by_state")
	}

	// The retry's reserved claim must not itself block a genuinely free
	// general-capacity slot from admitting a new, unrelated candidate.
	tracker.mu.Lock()
	tracker.candidates = append(tracker.candidates, extra)
	tracker.mu.Unlock()
	c.Tick(context.Background())
	<-agent.started
	if starts, _, _ := agent.counts(); starts != 3 {
		t.Fatalf("starts=%d, want the free general-capacity slot admitted despite the queued retry", starts)
	}

	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-ws.after
	<-ws.after
	<-ws.after
}

// startTransitionSettings adds a Todo -> In Progress dispatch-time start
// transition to the base test workflow so the coordinator's host-side move is
// exercised. In Progress is added as an active state so the moved issue stays
// eligible for reconciliation exactly as production configuration requires.
func startTransitionSettings(t *testing.T) config.Settings {
	t.Helper()
	w := testSettings(t)
	s := w.Config
	s.Tracker.ActiveStates = []string{"Todo", "In Progress"}
	s.Tracker.StartTransitions = map[string]string{"todo": "In Progress"}
	return s
}

func TestDispatchMovesTodoIssueToInProgress(t *testing.T) {
	s := startTransitionSettings(t)
	issue := testIssue()
	tracker := &fakeTracker{issue: issue}
	agent := &fakeAgent{events: completedEvents}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{}, 1)}
	c := testCoordinator(s, tracker, agent, ws)

	c.Tick(context.Background())
	<-ws.after
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}

	calls := tracker.transitionCalls()
	if len(calls) != 1 {
		t.Fatalf("start transitions=%+v, want exactly one dispatch transition", calls)
	}
	if calls[0].id != issue.ID || calls[0].to != "In Progress" {
		t.Fatalf("start transition=%+v, want issue %q moved to In Progress", calls[0], issue.ID)
	}
	if starts, _, _ := agent.counts(); starts != 1 {
		t.Fatalf("starts=%d, want the session to start after the transition", starts)
	}
}

func TestDispatchStartTransitionLogsOperationAndEdge(t *testing.T) {
	s := startTransitionSettings(t)
	issue := testIssue()
	tracker := &fakeTracker{issue: issue}
	agent := &fakeAgent{events: completedEvents}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{}, 1)}
	var logs bytes.Buffer
	c := New(tracker, agent, ws, func() config.Settings { return s }, slog.New(slog.NewJSONHandler(&logs, nil)))
	c.clock = fakeClock{now: time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)}

	c.Tick(context.Background())
	<-ws.after
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}

	output := logs.String()
	if !strings.Contains(output, `"msg":"issue moved to started state"`) ||
		!strings.Contains(output, `"operation":"start_transition"`) ||
		!strings.Contains(output, `"from_state":"todo"`) ||
		!strings.Contains(output, `"to_state":"in progress"`) {
		t.Fatalf("host-side start transition edge not reconstructable from log: %s", output)
	}
}

func TestDispatchDoesNotTransitionAlreadyStartedIssue(t *testing.T) {
	s := startTransitionSettings(t)
	// An issue re-observed already In Progress (a restart or turn-limit
	// re-dispatch) has no configured start edge for its state, so the
	// coordinator must not request any transition.
	started := testIssue()
	started.State = "In Progress"
	tracker := &fakeTracker{issue: started}
	agent := &fakeAgent{events: completedEvents}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{}, 1)}
	c := testCoordinator(s, tracker, agent, ws)

	c.Tick(context.Background())
	<-ws.after
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}

	if calls := tracker.transitionCalls(); len(calls) != 0 {
		t.Fatalf("start transitions=%+v, want none for an already-started issue", calls)
	}
	if starts, _, _ := agent.counts(); starts != 1 {
		t.Fatalf("starts=%d, want the session to start", starts)
	}
}

func TestDispatchTransitionFailureDoesNotBlockOrDoubleDispatch(t *testing.T) {
	s := startTransitionSettings(t)
	issue := testIssue()
	tracker := &fakeTracker{issue: issue, transitionErr: errors.New("tracker write failed")}
	agent := &fakeAgent{events: completedEvents}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{}, 1)}
	var logs bytes.Buffer
	c := New(tracker, agent, ws, func() config.Settings { return s }, slog.New(slog.NewJSONHandler(&logs, nil)))
	c.clock = fakeClock{now: time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)}

	c.Tick(context.Background())
	<-ws.after
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}

	if calls := tracker.transitionCalls(); len(calls) != 1 {
		t.Fatalf("start transitions=%+v, want exactly one attempt even on failure", calls)
	}
	// A failed transition must degrade gracefully: the session still starts
	// exactly once and the run is not double-dispatched.
	if starts, _, _ := agent.counts(); starts != 1 {
		t.Fatalf("starts=%d, want the run to proceed once despite the failed transition", starts)
	}
	if output := logs.String(); !strings.Contains(output, `"msg":"dispatch start transition failed"`) {
		t.Fatalf("failed transition was not logged: %s", output)
	}
}

func testCoordinator(settings config.Settings, tracker domain.Tracker, agent domain.AgentBackend, ws domain.WorkspaceExecutor) *Coordinator {
	c := New(tracker, agent, ws, func() config.Settings { return settings }, nil)
	c.clock = fakeClock{now: time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)}
	return c
}

func testSettings(t *testing.T) config.Workflow {
	t.Helper()
	d := t.TempDir()
	workflow := filepath.Join(d, "WORKFLOW.md")
	if err := os.WriteFile(workflow, []byte("---\ntracker: {kind: linear, active_states: [Todo], terminal_states: [Done]}\nagent: {max_concurrent_agents: 1, max_turns: 1}\nworkspace: {root: /tmp/work}\n---\nWork on {{.issue.identifier}}"), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := config.Load(workflow, "")
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func testIssue() domain.Issue {
	return domain.Issue{ID: "id", Identifier: "ENG-1", Title: "Work", State: "Todo", Dispatchable: true}
}

func completedEvents() <-chan domain.Event {
	ch := make(chan domain.Event, 1)
	ch <- domain.Event{Kind: domain.EventCompleted, At: time.Now(), SessionID: "t-u"}
	close(ch)
	return ch
}
func closedEvents() <-chan domain.Event {
	ch := make(chan domain.Event)
	close(ch)
	return ch
}
