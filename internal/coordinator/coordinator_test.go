package coordinator

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
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

func TestSnapshotCopiesOnlySafeOperationalMetadata(t *testing.T) {
	c := New(&fakeTracker{}, &fakeAgent{}, &fakeWorkspace{}, func() config.Settings { return config.Settings{} }, nil)
	now := time.Now()
	c.claimed["provider-id"] = true
	c.running["provider-id"] = &running{issue: domain.Issue{ID: "provider-id", Identifier: "PMR-6", Description: "must-not-appear"}, session: domain.AgentSession{ID: "session", ThreadID: "thread", TurnID: "turn"}, attempt: 2, started: now, last: now, usage: domain.Usage{InputTokens: 1}, rateLimit: map[string]int64{"remaining": 2}}
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

type fakeTracker struct {
	mu       sync.Mutex
	issue    domain.Issue
	fresh    domain.Issue
	hasFresh bool
}

func (f *fakeTracker) ListCandidates(context.Context, []string) ([]domain.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return []domain.Issue{f.issue}, nil
}
func (f *fakeTracker) GetIssues(context.Context, []string) ([]domain.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.hasFresh {
		return []domain.Issue{f.fresh}, nil
	}
	return []domain.Issue{f.issue}, nil
}
func (f *fakeTracker) ListTerminal(context.Context, []string) ([]domain.Issue, error) {
	return nil, nil
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

type fakeAgent struct {
	mu        sync.Mutex
	starts    int
	continues int
	cancels   int
	started   chan struct{}
	events    func() <-chan domain.Event
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
func (f *fakeAgent) Continue(context.Context, domain.AgentSession, string) (<-chan domain.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.continues++
	return nil, nil
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

type fakeWorkspace struct {
	mu             sync.Mutex
	shouldRun      bool
	shouldRunCalls int
	prepares       int
	marks          int
	cleanups       int
	cleaned        chan struct{}
	markErr        error
	after          chan struct{}
}

func (f *fakeWorkspace) ShouldRun(context.Context, domain.Issue) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.shouldRunCalls++
	return f.shouldRun, nil
}
func (f *fakeWorkspace) Prepare(context.Context, domain.Issue) (domain.Workspace, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prepares++
	return domain.Workspace{Path: "/tmp/work"}, nil
}
func (f *fakeWorkspace) BeforeRun(context.Context, domain.Workspace, domain.Issue) error { return nil }
func (f *fakeWorkspace) AfterRun(context.Context, domain.Workspace, domain.Issue) {
	if f.after != nil {
		f.after <- struct{}{}
	}
}
func (f *fakeWorkspace) MarkCompleted(context.Context, domain.Workspace, domain.Issue) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.marks++
	return f.markErr
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

func TestCompletionIsRecordedAndDoesNotContinueOrRetry(t *testing.T) {
	w := testSettings(t)
	issue := testIssue()
	agent := &fakeAgent{events: completedEvents}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{}, 1)}
	c := testCoordinator(w.Config, &fakeTracker{issue: issue}, agent, ws)
	timer := &fakeTimer{}
	c.timer = timer

	c.Tick(context.Background())
	<-ws.after

	starts, continues, cancels := agent.counts()
	if starts != 1 || continues != 0 || cancels != 1 {
		t.Fatalf("starts=%d continues=%d cancels=%d", starts, continues, cancels)
	}
	_, marks, _, _ := ws.counts()
	if marks != 1 {
		t.Fatalf("completed work marks=%d, want 1", marks)
	}
	if timer.scheduled() != 0 {
		t.Fatalf("completed run scheduled retries=%d", timer.scheduled())
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

func TestCompletionMarkerRetryDoesNotRerunCodex(t *testing.T) {
	w := testSettings(t)
	issue := testIssue()
	agent := &fakeAgent{events: completedEvents}
	ws := &fakeWorkspace{shouldRun: true, markErr: errors.New("disk full"), after: make(chan struct{}, 1)}
	c := testCoordinator(w.Config, &fakeTracker{issue: issue}, agent, ws)
	timer := &fakeTimer{signal: make(chan struct{}, 1)}
	c.timer = timer

	c.Tick(context.Background())
	<-timer.signal
	ws.mu.Lock()
	ws.markErr = nil
	ws.mu.Unlock()
	timer.fire(0)

	starts, _, _ := agent.counts()
	if starts != 1 {
		t.Fatalf("starts=%d, want one completed Codex turn", starts)
	}
	_, marks, _, _ := ws.counts()
	if marks != 2 {
		t.Fatalf("marker attempts=%d, want 2", marks)
	}
	c.mu.Lock()
	claimed := c.claimed[issue.ID]
	c.mu.Unlock()
	if claimed {
		t.Fatal("completion marker retry retained claim")
	}
}

func TestShouldRunPreventsClaimAndLaunch(t *testing.T) {
	w := testSettings(t)
	issue := testIssue()
	agent := &fakeAgent{events: completedEvents}
	ws := &fakeWorkspace{shouldRun: false, after: make(chan struct{}, 1)}
	c := testCoordinator(w.Config, &fakeTracker{issue: issue}, agent, ws)

	c.Tick(context.Background())
	starts, _, _ := agent.counts()
	prepares, _, _, checks := ws.counts()
	if starts != 0 || prepares != 0 || checks != 1 {
		t.Fatalf("starts=%d prepares=%d should-run-checks=%d", starts, prepares, checks)
	}
	c.mu.Lock()
	claimed := c.claimed[issue.ID]
	c.mu.Unlock()
	if claimed {
		t.Fatal("issue was claimed despite workspace refusing to run")
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

func testCoordinator(settings config.Settings, tracker domain.Tracker, agent domain.AgentBackend, ws domain.WorkspaceExecutor) *Coordinator {
	c := New(tracker, agent, ws, func() config.Settings { return settings }, nil)
	c.clock = fakeClock{now: time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)}
	return c
}

func testSettings(t *testing.T) config.Workflow {
	t.Helper()
	d := t.TempDir()
	workflow := filepath.Join(d, "WORKFLOW.md")
	if err := os.WriteFile(workflow, []byte("---\ntracker: {kind: linear, active_states: [Todo], terminal_states: [Done]}\nagent: {max_concurrent_agents: 1}\nworkspace: {root: /tmp/work}\n---\nWork on {{.issue.identifier}}"), 0o600); err != nil {
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
