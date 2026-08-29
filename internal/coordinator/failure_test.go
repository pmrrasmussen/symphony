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

// TestPermanentDispatchFailureStopsAtMaxAttempts covers the PMR-111 defect: a
// boundary that fails identically on every dispatch used to reschedule itself
// forever at the backoff ceiling, holding its claim and leaving nothing in the
// log but a warning that reads like progress. The ladder now stops at exactly
// agent.max_attempts dispatches, arms no further timer, drops the claim, and
// says so once at error level.
func TestPermanentDispatchFailureStopsAtMaxAttempts(t *testing.T) {
	w := testSettings(t)
	w.Config.Agent.MaxAttempts = 3
	w.Config.Agent.MaxRetryBackoff = time.Minute
	issue := testIssue()
	var log syncBuffer
	agent := &fakeAgent{startErr: errors.New("agent binary not found")}
	ws := &fakeWorkspace{after: make(chan struct{}, 4)}
	c := New(&fakeTracker{issue: issue}, agent, ws, func() config.Settings { return w.Config }, slog.New(slog.NewJSONHandler(&log, nil)))
	defer assertInvariants(t, c)
	c.clock = fakeClock{now: time.Date(2026, 8, 26, 9, 41, 0, 0, time.UTC)}
	timer := &fakeTimer{signal: make(chan struct{}, 4)}
	c.timer = timer

	c.Tick(context.Background())
	<-ws.after
	<-timer.signal
	// Two armed retries, and no more: the third dispatch reaches the ceiling.
	timer.fire(0)
	<-ws.after
	<-timer.signal
	timer.fire(1)
	<-ws.after
	waitForRelease(t, c, issue.ID)

	if starts, _, _ := agent.counts(); starts != 3 {
		t.Fatalf("starts=%d, want exactly max_attempts dispatches", starts)
	}
	if timer.scheduled() != 2 {
		t.Fatalf("armed %d retries, want one fewer than max_attempts", timer.scheduled())
	}
	_, retrying := c.armedRetry(issue.ID)
	claimed, admitted := c.claimHeld(issue.ID), c.admittedTotal()
	if claimed || retrying || admitted != 0 {
		t.Fatalf("claimed=%v retrying=%v admitted=%d, want the abandoned dispatch to hold nothing", claimed, retrying, admitted)
	}
	record := waitForSubstring(t, &log, `"msg":"dispatch abandoned after max attempts"`, time.Second)
	for _, want := range []string{`"level":"ERROR"`, `"operation":"dispatch_abandoned"`, `"issue_identifier":"ENG-1"`, `"reason":"session_start"`, `"attempt":3`, `"max_attempts":3`} {
		if !strings.Contains(record, want) {
			t.Fatalf("abandonment record missing %s: %s", want, record)
		}
	}
	records := log.String()
	if count := strings.Count(records, `"msg":"dispatch abandoned after max attempts"`); count != 1 {
		t.Fatalf("abandonment was logged %d times, want exactly one: %s", count, records)
	}
	// The two dispatches below the ceiling keep their ordinary retry warning,
	// so the abandonment is the only new record on this path.
	if count := strings.Count(records, `"msg":"agent run retry scheduled"`); count != 2 {
		t.Fatalf("retry warnings=%d, want one per dispatch below the ceiling: %s", count, records)
	}
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestUnexplainedSourceIntegrityAlertFailsTheRun covers the PMR-161 decision:
// the source-integrity check is the only thing enforcing the write boundary the
// Claude CLI widens, so an unexplained alert has to fail its run rather than
// leave a successful run with an ERROR beside it. The run here ends the way a
// good run does -- the agent completes and the issue reaches the review handoff
// state -- and is still recorded as a failure, carrying the ref change and the
// workspace the alert attributed it to. The handoff observation is recorded
// regardless: the handoff did happen, and a later external revert of it must
// stay attributable.
func TestUnexplainedSourceIntegrityAlertFailsTheRun(t *testing.T) {
	w := testSettings(t)
	w.Config.Agent.MaxAttempts = 3
	w.Config.Tracker.HandoffState = "In Review"
	w.Config.Tracker.ActiveStates = []string{"Todo", "In Progress"}
	issue := testIssue()
	issue.State = "In Progress"
	handoff := issue
	handoff.State = "In Review"
	handoff.Dispatchable = false
	tracker := &fakeTracker{issue: issue}
	tracker.setFresh(handoff)
	agent := &fakeAgent{events: completedEvents}
	verdict := domain.SourceIntegrityError{SourceRoot: "/src", Changes: "refs/heads/main aaa->bbb attributed_to=ENG-2"}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{}, 1), afterErr: verdict}
	var log syncBuffer
	c := New(tracker, agent, ws, func() config.Settings { return w.Config }, slog.New(slog.NewJSONHandler(&log, nil)))
	defer assertInvariants(t, c)
	timer := &fakeTimer{signal: make(chan struct{}, 1)}
	c.timer = timer

	c.Tick(context.Background())
	<-timer.signal

	retry, armed := c.armedRetry(issue.ID)
	if !armed || retry.kind != retryAgent || retry.reason != "source_integrity" {
		t.Fatalf("retry=%+v armed=%v, want a source_integrity agent failure", retry, armed)
	}
	if retry.attempt != 1 {
		t.Fatalf("attempt=%d, want the failure to spend an attempt", retry.attempt)
	}
	records := log.String()
	if !strings.Contains(records, `"msg":"agent logical run finished","issue_id":"id","issue_identifier":"ENG-1","session_id":"t-u","status":"failed"`) {
		t.Fatalf("a run that moved the source repository's refs was not recorded as failed: %s", records)
	}
	if !strings.Contains(records, "attributed_to=ENG-2") {
		t.Fatalf("failure record dropped the attributed ref change: %s", records)
	}
	if observation, ok := c.handoffMemory(issue.ID); !ok || observation.state != "in review" {
		t.Fatalf("handoff observation=%+v ok=%v, want the handoff remembered despite the failure", observation, ok)
	}
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestUnclassifiedAgentEventNeverAbandonsIssue covers the correction from
// review round 3: agentFailureReason's fallback, "agent_event", means the
// coordinator does not know why a run ended. That is not the deterministic,
// classified failure the ceiling was built for, so it must keep retrying
// without ever arming abandonment, however many times it repeats, and since
// PMR-179 without spending an attempt either -- unlike workspace_prepare, before_run,
// prompt_render, and session_start, which are issue-attributable and still
// consume the ceiling. See systemicFailureReasons for the three PMR-115
// added alongside it (stream_closed, issue_refresh, session_continue), and
// TestRateLimitedEventNeverAbandonsIssueAndIgnoresOrdinaryBackoff for the
// case that most commonly reached "agent_event" before it was named
// "agent_rate_limited" and given its own exemption and retry delay
// (PMR-131): a Claude quota rejection.
func TestUnclassifiedAgentEventNeverAbandonsIssue(t *testing.T) {
	w := testSettings(t)
	w.Config.Agent.MaxAttempts = 2
	w.Config.Agent.MaxRetryBackoff = time.Minute
	issue := testIssue()
	var log syncBuffer
	agent := &fakeAgent{events: failedEvents("model reported a failure")}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{}, 8)}
	c := New(&fakeTracker{issue: issue}, agent, ws, func() config.Settings { return w.Config }, slog.New(slog.NewJSONHandler(&log, nil)))
	defer assertInvariants(t, c)
	c.clock = fakeClock{now: time.Date(2026, 8, 26, 9, 41, 0, 0, time.UTC)}
	timer := &fakeTimer{signal: make(chan struct{}, 8)}
	c.timer = timer

	c.Tick(context.Background())
	<-ws.after
	<-timer.signal

	const repeats = 6 // well past max_attempts=2, which an unclassified cause must never consume
	for i := 0; i < repeats; i++ {
		timer.fire(i)
		<-ws.after
		<-timer.signal
	}

	if starts, _, _ := agent.counts(); starts != repeats+1 {
		t.Fatalf("starts=%d, want one dispatch per fire plus the initial one", starts)
	}
	claimed := c.claimHeld(issue.ID)
	retry, stillRetrying := c.armedRetry(issue.ID)
	if !claimed || !stillRetrying {
		t.Fatal("an unclassified agent_event abandoned the issue before any classified failure occurred")
	}
	if retry.reason != "agent_event" {
		t.Fatalf("reason=%q, want agent_event", retry.reason)
	}
	if retry.attempt != 0 {
		t.Fatalf("attempt=%d, want the first attempt held fixed across every systemic repeat", retry.attempt)
	}
	if strings.Contains(log.String(), `"msg":"dispatch abandoned after max attempts"`) {
		t.Fatalf("an unclassified agent_event armed an abandonment record: %s", log.String())
	}
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestClosedEventStreamNeverAbandonsIssue pins the review-round-4 decision in
// systemicFailureReasons for "stream_closed": by construction (see
// errStreamClosed) it is never a repository- or issue-specific outcome, only
// ever a host bug in the coordinator's own event plumbing, so -- like
// agent_event -- it must keep retrying without arming abandonment and without
// spending an attempt (PMR-179), however many times it repeats. It
// drives finishFailure directly, the same way TestRetryAtCapacityNeverAbandons
// drives scheduleRetry directly, rather than replaying a full dispatch for
// every repeat: the reason and the ceiling interaction are what is under
// test, not the dispatch machinery TestClosedEventStreamSchedulesDeterministicAgentRetry
// already covers for a single occurrence.
func TestClosedEventStreamNeverAbandonsIssue(t *testing.T) {
	w := testSettings(t)
	w.Config.Agent.MaxAttempts = 2
	issue := testIssue()
	var log syncBuffer
	c := New(&fakeTracker{issue: issue}, &fakeAgent{}, &fakeWorkspace{}, func() config.Settings { return w.Config }, slog.New(slog.NewJSONHandler(&log, nil)))
	defer assertInvariants(t, c)
	c.timer = &fakeTimer{}
	if !c.claim(issue, w.Config) {
		t.Fatal("issue was not claimed")
	}

	attempt := 0
	const repeats = 6 // well past max_attempts=2, which a host-generated cause must never consume
	for i := 0; i < repeats; i++ {
		c.finishFailure(context.Background(), issue, attempt, agentFailureReason(errStreamClosed), errStreamClosed)
		retry, stillRetrying := c.armedRetry(issue.ID)
		if !stillRetrying {
			t.Fatalf("stream_closed abandoned the issue on repeat %d", i)
		}
		if retry.reason != "stream_closed" {
			t.Fatalf("reason=%q, want stream_closed", retry.reason)
		}
		attempt = retry.attempt
	}

	if attempt != 0 {
		t.Fatalf("attempt=%d, want the first attempt held fixed across every repeat", attempt)
	}
	if strings.Contains(log.String(), `"msg":"dispatch abandoned after max attempts"`) {
		t.Fatalf("a closed event stream armed an abandonment record: %s", log.String())
	}
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestRateLimitedEventNeverAbandonsIssueAndIgnoresOrdinaryBackoff covers
// PMR-131: a Claude quota rejection says nothing about this issue's own
// work, so -- like stream_closed, issue_refresh, retry_refresh, and
// session_continue -- it must keep retrying without ever arming abandonment
// or spending an attempt. It must also never fall back onto the ordinary
// escalating backoff(escalation, max_retry_backoff) ladder: every repeat here
// carries the same reported reset time, so a delay that escalated with the
// attempt count (the ladder's own shape) would prove the ladder was still in
// play, exactly the bug this test exists to catch.
func TestRateLimitedEventNeverAbandonsIssueAndIgnoresOrdinaryBackoff(t *testing.T) {
	w := testSettings(t)
	w.Config.Agent.MaxAttempts = 2
	w.Config.Agent.MaxRetryBackoff = 10 * time.Second
	issue := testIssue()
	var log syncBuffer
	c := New(&fakeTracker{issue: issue}, &fakeAgent{}, &fakeWorkspace{}, func() config.Settings { return w.Config }, slog.New(slog.NewJSONHandler(&log, nil)))
	defer assertInvariants(t, c)
	timer := &fakeTimer{}
	c.timer = timer
	if !c.claim(issue, w.Config) {
		t.Fatal("issue was not claimed")
	}

	const resetIn = 90 * time.Minute
	err := rateLimitedError{retryAfter: resetIn}
	attempt := 0
	const repeats = 6 // well past max_attempts=2, which a systemic quota rejection must never consume
	for i := 0; i < repeats; i++ {
		c.finishFailure(context.Background(), issue, attempt, agentFailureReason(err), err)
		retry, stillRetrying := c.armedRetry(issue.ID)
		if !stillRetrying {
			t.Fatalf("a rate-limited rejection abandoned the issue on repeat %d", i)
		}
		if retry.reason != "agent_rate_limited" {
			t.Fatalf("reason=%q, want agent_rate_limited", retry.reason)
		}
		attempt = retry.attempt
	}

	if attempt != 0 {
		t.Fatalf("attempt=%d, want the first attempt held fixed across every repeat", attempt)
	}
	if strings.Contains(log.String(), `"msg":"dispatch abandoned after max attempts"`) {
		t.Fatalf("a rate-limited rejection armed an abandonment record: %s", log.String())
	}
	if got := len(timer.delays); got != repeats {
		t.Fatalf("scheduled %d retries, want %d", got, repeats)
	}
	for i, delay := range timer.delays {
		if delay != resetIn {
			t.Fatalf("retry %d delay=%s, want the reported reset time %s regardless of attempt count -- the ordinary backoff ladder is still in play", i, delay, resetIn)
		}
	}
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestRateLimitedEventWithNoResetFloorsWellAboveMaxRetryBackoff covers a
// rejection Claude reports with no reset time at all: the ordinary backoff
// ceiling (agent.max_retry_backoff_ms) is built for transient failures and is
// routinely minutes, far too short for a multi-hour quota window, so the
// fallback here must be a floor well above it rather than that ceiling
// itself (PMR-131).
func TestRateLimitedEventWithNoResetFloorsWellAboveMaxRetryBackoff(t *testing.T) {
	w := testSettings(t)
	w.Config.Agent.MaxRetryBackoff = 30 * time.Second
	issue := testIssue()
	var log syncBuffer
	c := New(&fakeTracker{issue: issue}, &fakeAgent{}, &fakeWorkspace{}, func() config.Settings { return w.Config }, slog.New(slog.NewJSONHandler(&log, nil)))
	defer assertInvariants(t, c)
	timer := &fakeTimer{}
	c.timer = timer
	if !c.claim(issue, w.Config) {
		t.Fatal("issue was not claimed")
	}

	err := rateLimitedError{}
	c.finishFailure(context.Background(), issue, 0, agentFailureReason(err), err)

	if len(timer.delays) != 1 {
		t.Fatalf("scheduled %d retries, want 1", len(timer.delays))
	}
	if got, want := timer.delays[0], 10*w.Config.Agent.MaxRetryBackoff; got != want {
		t.Fatalf("delay=%s, want %s (well above max_retry_backoff_ms=%s)", got, want, w.Config.Agent.MaxRetryBackoff)
	}
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestPostTurnRefreshFailureNeverAbandonsIssue is the ceiling-interaction
// test review round 4 asked for: PMR-115's confirmed-live failure mode -- a
// Linear timeout on the post-turn GetIssues refresh -- is this codebase's
// shared tracker infrastructure, not this issue, so repeating it more times
// than agent.max_attempts must keep retrying rather than abandon the issue,
// and must leave its attempt budget untouched (see
// systemicFailureReasons). It drives finishFailure directly rather than
// replaying a full dispatch (see TestClosedEventStreamNeverAbandonsIssue):
// a permanently failing tracker would also fail runRetry's own pre-dispatch
// refresh, reclassifying every retry after the first as "retry_refresh" (a
// separate, already-covered ceiling interaction -- TestRetryRefreshFailureNeverAbandonsIssue,
// PMR-142) instead of exercising "issue_refresh" a second time.
func TestPostTurnRefreshFailureNeverAbandonsIssue(t *testing.T) {
	w := testSettings(t)
	w.Config.Agent.MaxAttempts = 2
	issue := testIssue()
	var log syncBuffer
	c := New(&fakeTracker{issue: issue}, &fakeAgent{}, &fakeWorkspace{}, func() config.Settings { return w.Config }, slog.New(slog.NewJSONHandler(&log, nil)))
	defer assertInvariants(t, c)
	c.timer = &fakeTimer{}
	if !c.claim(issue, w.Config) {
		t.Fatal("issue was not claimed")
	}

	refreshErr := issueRefreshError{err: errors.New("linear tracker_request: Linear request failed")}
	attempt := 0
	const repeats = 6 // well past max_attempts=2, which a Linear-infrastructure cause must never consume
	for i := 0; i < repeats; i++ {
		c.finishFailure(context.Background(), issue, attempt, agentFailureReason(refreshErr), refreshErr)
		retry, stillRetrying := c.armedRetry(issue.ID)
		if !stillRetrying {
			t.Fatalf("issue_refresh abandoned the issue on repeat %d", i)
		}
		if retry.reason != "issue_refresh" {
			t.Fatalf("reason=%q, want issue_refresh", retry.reason)
		}
		attempt = retry.attempt
	}

	if attempt != 0 {
		t.Fatalf("attempt=%d, want the first attempt held fixed across every repeat", attempt)
	}
	if strings.Contains(log.String(), `"msg":"dispatch abandoned after max attempts"`) {
		t.Fatalf("issue_refresh armed an abandonment record: %s", log.String())
	}
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestContinuationFailureNeverAbandonsIssue pins the same decision for
// "session_continue": it is Symphony's own backend adapter (agent.Continue)
// failing to resume a session, so a broken `claude` binary or lapsed backend
// auth would fail every running issue's next turn identically -- the same
// account-wide shape as the quota rejection that motivates agent_event's
// exemption, just raised by Symphony's own code instead of the model's (see
// systemicFailureReasons).
func TestContinuationFailureNeverAbandonsIssue(t *testing.T) {
	w := testSettings(t)
	w.Config.Agent.MaxAttempts = 2
	issue := testIssue()
	var log syncBuffer
	c := New(&fakeTracker{issue: issue}, &fakeAgent{}, &fakeWorkspace{}, func() config.Settings { return w.Config }, slog.New(slog.NewJSONHandler(&log, nil)))
	defer assertInvariants(t, c)
	c.timer = &fakeTimer{}
	if !c.claim(issue, w.Config) {
		t.Fatal("issue was not claimed")
	}

	continueErr := sessionContinueError{err: errors.New("continuation unavailable")}
	attempt := 0
	const repeats = 6 // well past max_attempts=2, which a backend-adapter cause must never consume
	for i := 0; i < repeats; i++ {
		c.finishFailure(context.Background(), issue, attempt, agentFailureReason(continueErr), continueErr)
		retry, stillRetrying := c.armedRetry(issue.ID)
		if !stillRetrying {
			t.Fatalf("session_continue abandoned the issue on repeat %d", i)
		}
		if retry.reason != "session_continue" {
			t.Fatalf("reason=%q, want session_continue", retry.reason)
		}
		attempt = retry.attempt
	}

	if attempt != 0 {
		t.Fatalf("attempt=%d, want the first attempt held fixed across every repeat", attempt)
	}
	if strings.Contains(log.String(), `"msg":"dispatch abandoned after max attempts"`) {
		t.Fatalf("session_continue armed an abandonment record: %s", log.String())
	}
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestSystemicFailuresLeaveTheGenuineRetryBudgetIntact is PMR-179's failure
// scenario end to end. A Claude quota wall produces far more rejections than
// agent.max_attempts (PMR-131 observed 203), and every one of them was exempt
// from abandonment but still escalated the attempt counter -- so when the quota
// reopened, the issue's first genuine failure arrived already past the ceiling
// and abandoned the dispatch on the spot, with zero real retries and every
// backoff along the way already saturated. Both halves are pinned here: the
// wall must leave the budget untouched, and the genuine failures after it must
// then get the full ladder, delay included, before the ceiling fires.
func TestSystemicFailuresLeaveTheGenuineRetryBudgetIntact(t *testing.T) {
	w := testSettings(t)
	w.Config.Agent.MaxAttempts = 3
	w.Config.Agent.MaxRetryBackoff = time.Minute
	issue := testIssue()
	var log syncBuffer
	c := New(&fakeTracker{issue: issue}, &fakeAgent{}, &fakeWorkspace{}, func() config.Settings { return w.Config }, slog.New(slog.NewJSONHandler(&log, nil)))
	defer assertInvariants(t, c)
	timer := &fakeTimer{}
	c.timer = timer
	if !c.claim(issue, w.Config) {
		t.Fatal("issue was not claimed")
	}

	quota := rateLimitedError{retryAfter: time.Hour}
	attempt := 0
	const wall = 8 // a quota wall is bounded by nothing the issue does, so it runs well past max_attempts=3
	for i := 0; i < wall; i++ {
		c.finishFailure(context.Background(), issue, attempt, agentFailureReason(quota), quota)
		retry, stillRetrying := c.armedRetry(issue.ID)
		if !stillRetrying {
			t.Fatalf("the quota wall abandoned the issue on rejection %d", i)
		}
		attempt = retry.attempt
	}
	if attempt != 0 {
		t.Fatalf("attempt=%d after %d systemic rejections, want the issue's budget untouched", attempt, wall)
	}

	// The quota reopens: every failure from here is the issue's own, so it gets
	// exactly max_attempts dispatches -- max_attempts-1 further retries, then
	// the abandonment -- and its delays start from the ladder's first rung
	// rather than the ceiling the wall would have driven them to.
	blocked := blockedError{category: "agent_reported"}
	wantDelays := []time.Duration{10 * time.Second, 20 * time.Second}
	for i := 1; i < w.Config.Agent.MaxAttempts; i++ {
		c.finishFailure(context.Background(), issue, attempt, agentFailureReason(blocked), blocked)
		retry, stillRetrying := c.armedRetry(issue.ID)
		if !stillRetrying {
			t.Fatalf("genuine failure %d of max_attempts=%d abandoned the issue early", i, w.Config.Agent.MaxAttempts)
		}
		if retry.attempt != i {
			t.Fatalf("attempt=%d after genuine failure %d, want one rung per genuine failure", retry.attempt, i)
		}
		if got := timer.delays[wall+i-1]; got != wantDelays[i-1] {
			t.Fatalf("genuine retry %d delay=%s, want %s -- the ladder did not restart after the wall", i, got, wantDelays[i-1])
		}
		attempt = retry.attempt
	}

	c.finishFailure(context.Background(), issue, attempt, agentFailureReason(blocked), blocked)
	if _, stillRetrying := c.armedRetry(issue.ID); stillRetrying {
		t.Fatal("max_attempts genuine failures did not end the dispatch")
	}
	if c.claimHeld(issue.ID) {
		t.Fatal("the abandoned dispatch kept its claim")
	}
	record := waitForSubstring(t, &log, `"msg":"dispatch abandoned after max attempts"`, time.Second)
	if !strings.Contains(record, `"attempt":3`) {
		t.Fatalf("abandonment record did not count only the genuine failures: %s", record)
	}
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestRateLimitedEventEndsTheRunAndSchedulesFromTheReportedResetTime drives a
// full dispatch through a domain.EventRateLimited terminal event, the shape
// the Claude backend now reports for a quota rejection (PMR-131), and checks
// the operator-visible outcome end to end: a distinct reason naming quota
// rather than "agent_event", a Warn-level record carrying the status instead
// of "agent stderr", and a retry delay taken from the reported reset time
// rather than the ordinary backoff(attempt, max_retry_backoff) ladder.
func TestRateLimitedEventEndsTheRunAndSchedulesFromTheReportedResetTime(t *testing.T) {
	w := testSettings(t)
	w.Config.Agent.MaxRetryBackoff = 5 * time.Second
	issue := testIssue()
	var log syncBuffer
	resetIn := 3 * time.Hour
	agent := &fakeAgent{events: rateLimitedEvents(resetIn)}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{}, 1)}
	c := New(&fakeTracker{issue: issue}, agent, ws, func() config.Settings { return w.Config }, slog.New(slog.NewJSONHandler(&log, nil)))
	defer assertInvariants(t, c)
	timer := &fakeTimer{signal: make(chan struct{}, 1)}
	c.timer = timer

	c.Tick(context.Background())
	<-timer.signal

	retry, _ := c.armedRetry(issue.ID)
	// A quota wall is systemic, so the redispatch repeats attempt 0 rather
	// than spending one of the issue's own attempts (PMR-179).
	if retry.kind != retryAgent || retry.reason != "agent_rate_limited" || retry.attempt != 0 {
		t.Fatalf("retry=%+v, want agent_rate_limited", retry)
	}
	if len(timer.delays) != 1 || timer.delays[0] != resetIn {
		t.Fatalf("delays=%v, want the reported reset time %s and not backoff(1, max_retry_backoff)", timer.delays, resetIn)
	}
	records := log.String()
	if !strings.Contains(records, `"msg":"agent rate limit rejected"`) || !strings.Contains(records, `"status":"rejected"`) {
		t.Fatalf("rejection did not log its own status: %s", records)
	}
	if !strings.Contains(records, `"operation":"rate_limit"`) {
		t.Fatalf("rejection did not log its rate-limit operation: %s", records)
	}
	if strings.Contains(records, `"msg":"agent stderr"`) {
		t.Fatalf("rejection was logged as undecodable stderr instead of its own record: %s", records)
	}
}
