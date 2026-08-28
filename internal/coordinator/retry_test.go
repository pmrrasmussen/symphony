package coordinator

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/domain"
)

func TestStartSchedulesRateLimitRecoveryWithInjectedTimer(t *testing.T) {
	w := testSettings(t)
	w.Config.Polling.Interval = 30 * time.Second
	tracker := &rateLimitPollTracker{}
	c := testCoordinator(w.Config, tracker, &fakeAgent{}, &fakeWorkspace{})
	defer assertInvariants(t, c)
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

func TestRetryRechecksCurrentIssueEligibility(t *testing.T) {
	w := testSettings(t)
	issue := testIssue()
	tracker := &fakeTracker{issue: issue}
	agent := &fakeAgent{events: closedEvents}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{}, 1)}
	c := testCoordinator(w.Config, tracker, agent, ws)
	defer assertInvariants(t, c)
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
	defer assertInvariants(t, c)
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

func TestShutdownCancellationDoesNotRetry(t *testing.T) {
	w := testSettings(t)
	issue := testIssue()
	block := make(chan domain.Event)
	agent := &fakeAgent{events: func() <-chan domain.Event { return block }, started: make(chan struct{}, 1)}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{}, 1)}
	c := testCoordinator(w.Config, &fakeTracker{issue: issue}, agent, ws)
	defer assertInvariants(t, c)
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
	defer assertInvariants(t, c)
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

// TestRetryAtCapacityRequeuesOnFixedCadence covers runRetry's contended-slot
// branch for retryAgent: losing the race for an orchestrator slot is capacity
// contention, not a dispatch failure, so it must keep the attempt fixed and
// retry on agentSlotRetryDelay's fixed poll-interval cadence rather than
// attempt+1 and the escalating failure backoff.
func TestRetryAtCapacityRequeuesOnFixedCadence(t *testing.T) {
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
	defer assertInvariants(t, c)
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

	retry, _ := c.armedRetry(retrying.ID)
	if retry.reason != "agent_slot_unavailable" || retry.attempt != 1 {
		t.Fatalf("retry=%+v, want attempt unchanged and reason agent_slot_unavailable", retry)
	}
	if len(timer.delays) != 2 || timer.delays[1] != w.Config.Polling.Interval {
		t.Fatalf("retry delays=%v, want second retry at the poll interval (not the 15s failure backoff)", timer.delays)
	}
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-ws.after
}

// TestRetryAtCapacityNeverAbandons pins the invariant review settled
// empirically on this repository: PMR-100 completed successfully on attempt
// 11 after eleven straight lost slot races, never once having failed a
// dispatch. A contended orchestrator slot must never consume
// agent.max_attempts, however many times the slot is lost, or a healthy but
// busy queue would abandon issues that were never broken.
func TestRetryAtCapacityNeverAbandons(t *testing.T) {
	w := testSettings(t)
	w.Config.Agent.MaxAttempts = 2
	w.Config.Agent.MaxRetryBackoff = 15 * time.Second
	retrying := testIssue()
	retrying.ID, retrying.Identifier = "retrying", "ENG-2"
	running := testIssue()
	running.ID, running.Identifier = "running", "ENG-3"
	tracker := &issueMapTracker{issues: map[string]domain.Issue{retrying.ID: retrying, running.ID: running}}
	block := make(chan domain.Event)
	agent := &fakeAgent{events: func() <-chan domain.Event { return block }, started: make(chan struct{}, 1)}
	ws := &fakeWorkspace{after: make(chan struct{}, 1)}
	var log syncBuffer
	c := New(tracker, agent, ws, func() config.Settings { return w.Config }, slog.New(slog.NewJSONHandler(&log, nil)))
	defer assertInvariants(t, c)
	c.clock = fakeClock{now: time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)}
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

	const lostRaces = 5 // well past max_attempts=2, which a contended slot must never consume
	for i := 0; i < lostRaces; i++ {
		timer.fire(i)
	}

	claimed := c.claimHeld(retrying.ID)
	retry, stillRetrying := c.armedRetry(retrying.ID)
	if !claimed || !stillRetrying {
		t.Fatal("contended slot abandoned the retry before a real dispatch failure ever occurred")
	}
	if retry.attempt != 1 {
		t.Fatalf("attempt=%d, want unchanged across %d lost slot races", retry.attempt, lostRaces)
	}
	if retry.reason != "agent_slot_unavailable" {
		t.Fatalf("reason=%q, want agent_slot_unavailable", retry.reason)
	}
	if strings.Contains(log.String(), `"msg":"dispatch abandoned after max attempts"`) {
		t.Fatalf("contended slot armed an abandonment record: %s", log.String())
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
	defer assertInvariants(t, c)
	timer := &fakeTimer{}
	c.timer = timer

	if !c.claim(issue, w.Config) {
		t.Fatal("issue was not claimed")
	}
	c.scheduleRetry(context.Background(), issue, domain.Workspace{}, 1, retryAgent, "test", time.Second)
	timer.fire(0)

	retry, _ := c.armedRetry(issue.ID)
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

// TestRetryRefreshFailureNeverAbandonsIssue pins the PMR-142 correction:
// runRetry's pre-dispatch refresh wraps the same tracker.GetIssues failure
// as the post-turn refresh covered by TestPostTurnRefreshFailureNeverAbandonsIssue
// (see systemicFailureReasons), just observed at a different moment -- the
// moment an issue is waiting to redispatch rather than the moment one just
// finished a turn. A sustained Linear outage drives every retrying issue
// through exactly this site, so it must keep climbing the ordinary
// escalating backoff ladder past agent.max_attempts, the same as
// "issue_refresh", instead of abandoning the issue on infrastructure that
// says nothing about whether its work is workable.
func TestRetryRefreshFailureNeverAbandonsIssue(t *testing.T) {
	w := testSettings(t)
	w.Config.Agent.MaxAttempts = 2
	w.Config.Agent.MaxRetryBackoff = 15 * time.Second
	issue := testIssue()
	tracker := &issueMapTracker{issues: map[string]domain.Issue{issue.ID: issue}, getErr: errors.New("temporary tracker failure")}
	var log syncBuffer
	c := New(tracker, &fakeAgent{}, &fakeWorkspace{}, func() config.Settings { return w.Config }, slog.New(slog.NewJSONHandler(&log, nil)))
	defer assertInvariants(t, c)
	c.clock = fakeClock{now: time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)}
	timer := &fakeTimer{}
	c.timer = timer

	if !c.claim(issue, w.Config) {
		t.Fatal("issue was not claimed")
	}
	c.scheduleRetry(context.Background(), issue, domain.Workspace{}, 1, retryAgent, "test", time.Second)

	const repeats = 6 // well past max_attempts=2, which a Linear-infrastructure cause must never consume
	for i := 0; i < repeats; i++ {
		timer.fire(i)
	}

	claimed := c.claimHeld(issue.ID)
	retry, stillRetrying := c.armedRetry(issue.ID)
	if !claimed || !stillRetrying {
		t.Fatal("retry_refresh abandoned the issue before any classified failure occurred")
	}
	if retry.reason != "retry_refresh" {
		t.Fatalf("reason=%q, want retry_refresh", retry.reason)
	}
	if retry.attempt <= w.Config.Agent.MaxAttempts {
		t.Fatalf("attempt=%d, want it to keep climbing the ordinary ladder past max_attempts", retry.attempt)
	}
	if strings.Contains(log.String(), `"msg":"dispatch abandoned after max attempts"`) {
		t.Fatalf("retry_refresh armed an abandonment record: %s", log.String())
	}
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}
