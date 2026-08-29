package coordinator

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/domain"
)

// TestContinuationGuidanceIsBackendNeutral covers the belt-and-braces half. The
// only assertion the existing continuation test makes about this text is that the
// coordinator sends what this function returns, which is true of any wording at
// all.
func TestContinuationGuidanceIsBackendNeutral(t *testing.T) {
	guidance := continuationGuidance(2, 3)
	// Both backends' vocabulary is banned, and the second half is the half that
	// was missing: this text is shared, so MCP wording leaks into every Codex
	// continuation turn, where there is no prefix to speak of. That is the same
	// mistake this change fixes, pointed the other way.
	for _, forbidden := range []string{"Codex", "codex", "workpad", "thread", "mcp", "MCP", "prefix"} {
		if strings.Contains(guidance, forbidden) {
			t.Fatalf("continuation guidance names %q, which is one backend's vocabulary: %q", forbidden, guidance)
		}
	}
	if !strings.Contains(guidance, "continuation turn #2 of 3") {
		t.Fatalf("continuation guidance lost its turn counter: %q", guidance)
	}
}

func TestActiveIssueAtTurnLimitIsBlockedAndRetriedWithoutCompletionMarker(t *testing.T) {
	w := testSettings(t)
	issue := testIssue()
	agent := &fakeAgent{events: completedEvents}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{}, 1)}
	c := testCoordinator(w.Config, &fakeTracker{issue: issue}, agent, ws)
	defer assertInvariants(t, c)
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
	retry, _ := c.armedRetry(issue.ID)
	if retry.kind != retryAgent || retry.reason != "turn_limit_exhausted" || retry.attempt != 1 {
		t.Fatalf("retry=%+v", retry)
	}
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestLandingResolvedEndsRunWithoutAnotherTurn covers the PMR-77 duplicate
// terminal call: once landing merged the pull request and reconciled the issue,
// the run ends immediately — even when this tracker refresh still reports the
// issue active — so no later turn can call the landing tool again.
func TestLandingResolvedEndsRunWithoutAnotherTurn(t *testing.T) {
	w := testSettings(t)
	w.Config.Agent.MaxTurns = 20
	issue := testIssue()
	var log syncBuffer
	agent := &fakeAgent{events: landingResolvedEvents}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{}, 1)}
	c := New(&fakeTracker{issue: issue}, agent, ws, func() config.Settings { return w.Config }, slog.New(slog.NewJSONHandler(&log, nil)))
	defer assertInvariants(t, c)
	c.clock = fakeClock{now: time.Date(2026, 8, 25, 9, 41, 0, 0, time.UTC)}
	timer := &fakeTimer{}
	c.timer = timer

	c.Tick(context.Background())
	<-ws.after
	waitForRelease(t, c, issue.ID)

	starts, continues, cancels := agent.counts()
	if starts != 1 || continues != 0 || cancels != 1 {
		t.Fatalf("starts=%d continues=%d cancels=%d, want one start, no continuation turn, one cancel", starts, continues, cancels)
	}
	if timer.scheduled() != 0 {
		t.Fatalf("terminal landing scheduled %d retries, want none", timer.scheduled())
	}
	if _, _, cleanups, _ := ws.counts(); cleanups != 1 {
		t.Fatalf("cleanups=%d, want the workspace released even though the refresh still reports the issue active", cleanups)
	}
	records := log.String()
	if !strings.Contains(records, `"msg":"agent landing resolved"`) || !strings.Contains(records, `"operation":"landing_resolved"`) {
		t.Fatalf("log missing the landing resolution record: %s", records)
	}
	if !strings.Contains(records, `"status":"succeeded"`) {
		t.Fatalf("terminal landing did not finish the run successfully: %s", records)
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
	defer assertInvariants(t, c)
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
	retry, _ := c.armedRetry(issue.ID)
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
	defer assertInvariants(t, c)
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
	retry, _ := c.armedRetry(issue.ID)
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
	defer assertInvariants(t, c)
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
	defer assertInvariants(t, c)
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
	var log syncBuffer
	c := New(&fakeTracker{issue: issue}, agent, ws, func() config.Settings { return w.Config }, slog.New(slog.NewJSONHandler(&log, nil)))
	defer assertInvariants(t, c)
	c.clock = fakeClock{now: time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)}
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
	retry, _ := c.armedRetry(issue.ID)
	// "session_continue" is systemic, so the redispatch repeats the same
	// attempt rather than spending one of the issue's own (PMR-179).
	if retry.kind != retryAgent || retry.reason != "session_continue" || retry.attempt != 0 {
		t.Fatalf("failed continuation retry=%+v", retry)
	}
	if records := log.String(); !strings.Contains(records, "continuation unavailable") {
		t.Fatalf("failed continuation dropped the backend error: %s", records)
	}
}

func TestClosedEventStreamSchedulesDeterministicAgentRetry(t *testing.T) {
	w := testSettings(t)
	issue := testIssue()
	agent := &fakeAgent{events: closedEvents}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{}, 1)}
	c := testCoordinator(w.Config, &fakeTracker{issue: issue}, agent, ws)
	defer assertInvariants(t, c)
	timer := &fakeTimer{signal: make(chan struct{}, 1)}
	c.timer = timer

	c.Tick(context.Background())
	<-timer.signal
	if timer.scheduled() != 1 || timer.delays[0] != 10*time.Second {
		t.Fatalf("retries=%v", timer.delays)
	}
	retry, _ := c.armedRetry(issue.ID)
	// "stream_closed" is systemic, so the attempt is unchanged (PMR-179); the
	// 10s delay above is the first rung of the systemic streak's own ladder.
	if retry.kind != retryAgent || retry.reason != "stream_closed" || retry.attempt != 0 {
		t.Fatalf("retry=%+v", retry)
	}
}

// TestPostTurnRefreshFailureSchedulesDistinctAgentRetry pins the PMR-115 fix:
// a tracker error from runTurns' post-turn GetIssues -- confirmed live as a
// Linear request timeout following a turn the agent completed successfully
// -- is named "issue_refresh" rather than collapsing into "agent_event", and
// the underlying tracker error text is not discarded. The live error text is
// used verbatim here for a second reason (PMR-179): the run's own status is
// classified from the typed error, so a tracker error that happens to say
// "timeout" is recorded as the failure it is and not as an agent timeout.
func TestPostTurnRefreshFailureSchedulesDistinctAgentRetry(t *testing.T) {
	w := testSettings(t)
	issue := testIssue()
	var log syncBuffer
	agent := &fakeAgent{events: completedEvents}
	tracker := &fakeTracker{issue: issue, getErr: errors.New("linear tracker_request: Linear request failed: context deadline exceeded (Client.Timeout exceeded while awaiting headers)")}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{}, 1)}
	c := New(tracker, agent, ws, func() config.Settings { return w.Config }, slog.New(slog.NewJSONHandler(&log, nil)))
	defer assertInvariants(t, c)
	timer := &fakeTimer{signal: make(chan struct{}, 1)}
	c.timer = timer

	c.Tick(context.Background())
	<-timer.signal

	retry, _ := c.armedRetry(issue.ID)
	// "issue_refresh" is systemic: the tracker being unreachable is not an
	// attempt at this issue's work, so the attempt is unchanged (PMR-179).
	if retry.kind != retryAgent || retry.reason != "issue_refresh" || retry.attempt != 0 {
		t.Fatalf("retry=%+v", retry)
	}
	records := log.String()
	if !strings.Contains(records, "linear tracker_request: Linear request failed") {
		t.Fatalf("post-turn refresh failure dropped the tracker error: %s", records)
	}
	if !strings.Contains(records, `"msg":"agent logical run finished","issue_id":"id","issue_identifier":"ENG-1","session_id":"t-u","status":"failed"`) {
		t.Fatalf("a tracker error whose text says timeout was recorded as something other than a failure: %s", records)
	}
}

func TestCompletedEventAfterReconciliationCancellationDoesNotComplete(t *testing.T) {
	w := testSettings(t)
	issue := testIssue()
	agent := &fakeAgent{events: completedEvents}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{}, 1)}
	c := testCoordinator(w.Config, &fakeTracker{issue: issue}, agent, ws)
	defer assertInvariants(t, c)
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
	defer assertInvariants(t, c)

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
