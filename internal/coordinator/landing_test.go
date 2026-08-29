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

// TestLandingWaitEndsRunWithoutTurnsAndSchedulesBoundedLandingRetry reproduces
// the PMR-77 defect: a non-terminal landing wait must end the run at once
// instead of spending the remaining turns on repeated landing calls and then a
// turn-limit agent retry. The run instead ends as a wait, releases its
// orchestrator slot, keeps the duplicate-prevention claim, and schedules one
// delayed landing retry at the configured GitHub poll interval.
func TestLandingWaitEndsRunWithoutTurnsAndSchedulesBoundedLandingRetry(t *testing.T) {
	w := testSettings(t)
	w.Config.Agent.MaxTurns = 20
	w.Config.Agent.MaxRetryBackoff = 10 * time.Minute
	w.Config.GitHub.PollInterval = 30 * time.Second
	issue := testIssue()
	var log syncBuffer
	agent := &fakeAgent{events: landingWaitingEvents}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{}, 1)}
	c := New(&fakeTracker{issue: issue}, agent, ws, func() config.Settings { return w.Config }, slog.New(slog.NewJSONHandler(&log, nil)))
	defer assertInvariants(t, c)
	c.clock = fakeClock{now: time.Date(2026, 8, 25, 9, 41, 0, 0, time.UTC)}
	timer := &fakeTimer{signal: make(chan struct{}, 1)}
	c.timer = timer

	c.Tick(context.Background())
	<-ws.after
	<-timer.signal

	starts, continues, cancels := agent.counts()
	if starts != 1 || continues != 0 || cancels != 1 {
		t.Fatalf("starts=%d continues=%d cancels=%d, want one start, no continuation turn, one cancel", starts, continues, cancels)
	}
	retry, _ := c.armedRetry(issue.ID)
	claimed, admitted, running := c.claimHeld(issue.ID), c.admittedTotal(), c.runningCount()
	if retry.kind != retryLanding || retry.reason != "landing_waiting" || retry.attempt != 0 {
		t.Fatalf("retry=%+v, want an unescalated landing retry", retry)
	}
	if !claimed || admitted != 0 || running != 0 {
		t.Fatalf("claimed=%v admitted=%d running=%d, want the claim held with no slot occupied", claimed, admitted, running)
	}
	if timer.scheduled() != 1 || timer.delays[0] != 30*time.Second {
		t.Fatalf("landing retry delays=%v, want one github poll interval", timer.delays)
	}
	if _, marks, cleanups, _ := ws.counts(); marks != 0 || cleanups != 0 {
		t.Fatalf("landing wait marks=%d cleanups=%d, want neither completion nor cleanup", marks, cleanups)
	}
	for _, want := range []string{`"msg":"agent landing waiting"`, `"operation":"landing_waiting"`, `"reason":"required checks are pending"`, `"msg":"landing wait retry scheduled"`, `"wait_attempt":1`, `"status":"waiting"`, `"retry_kind":"landing"`} {
		waitForSubstring(t, &log, want, time.Second)
	}
	if records := log.String(); strings.Contains(records, "turn_limit_exhausted") || strings.Contains(records, `"msg":"agent run retry scheduled"`) {
		t.Fatalf("landing wait logged an agent failure: %s", records)
	}
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestLandingWaitRedispatchesWithEscalatingDelay drives the mechanism itself:
// the delayed landing retry relaunches the same attempt in a fresh session, and
// a gate that stays unsettled backs off instead of respawning at a fixed
// cadence forever. Adapted from the PMR-78 review probe.
func TestLandingWaitRedispatchesWithEscalatingDelay(t *testing.T) {
	w := testSettings(t)
	w.Config.Agent.MaxTurns = 20
	w.Config.Agent.MaxRetryBackoff = 10 * time.Minute
	// A poll interval below the first backoff step so escalation is visible.
	w.Config.GitHub.PollInterval = 5 * time.Second
	issue := testIssue()
	var log syncBuffer
	agent := &fakeAgent{events: landingWaitingEvents}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{}, 4)}
	c := New(&fakeTracker{issue: issue}, agent, ws, func() config.Settings { return w.Config }, slog.New(slog.NewJSONHandler(&log, nil)))
	defer assertInvariants(t, c)
	c.clock = fakeClock{now: time.Date(2026, 8, 25, 9, 41, 0, 0, time.UTC)}
	timer := &fakeTimer{signal: make(chan struct{}, 4)}
	c.timer = timer

	c.Tick(context.Background())
	<-ws.after
	<-timer.signal

	// Firing the landing timer must relaunch landing, not fail the issue.
	timer.fire(0)
	<-ws.after
	<-timer.signal

	starts, continues, cancels := agent.counts()
	if starts != 2 || continues != 0 || cancels != 2 {
		t.Fatalf("starts=%d continues=%d cancels=%d, want two dispatches, no continuation turn, one cancel each", starts, continues, cancels)
	}
	retry, ok := c.armedRetry(issue.ID)
	waits, claimed, admitted := c.landingWaitsFor(issue.ID), c.claimHeld(issue.ID), c.admittedTotal()
	if !ok || retry.kind != retryLanding || retry.reason != "landing_waiting" || retry.attempt != 0 {
		t.Fatalf("retry=%+v ok=%v, want a second landing retry on the same attempt", retry, ok)
	}
	if waits != 2 || !claimed || admitted != 0 {
		t.Fatalf("waits=%d claimed=%v admitted=%d", waits, claimed, admitted)
	}
	if len(timer.delays) != 2 || timer.delays[0] != 10*time.Second || timer.delays[1] != 20*time.Second {
		t.Fatalf("landing retry delays=%v, want an escalating sequence", timer.delays)
	}
	waitForSubstring(t, &log, `"wait_attempt":2`, time.Second)
	if records := log.String(); strings.Contains(records, "turn_limit_exhausted") {
		t.Fatalf("repeated landing waits escalated into an agent failure: %s", records)
	}
	if snapshot := c.Snapshot(); len(snapshot.Retrying) != 1 || snapshot.Retrying[0].WaitAttempt != 2 || snapshot.Retrying[0].Attempt != 0 {
		t.Fatalf("snapshot retrying=%+v, want wait_attempt 2 on attempt 0", snapshot.Retrying)
	}
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	leaked, leakedWaits := c.claimHeld(issue.ID), c.landingWaitRecords()
	if leaked || leakedWaits != 0 {
		t.Fatalf("shutdown leaked claim=%v landing waits=%d", leaked, leakedWaits)
	}
}

// TestCapacityBlockedLandingRetryKeepsItsCadence covers the redispatch that
// loses the state's single landing slot: it stays a landing retry on the same
// attempt at the landing cadence, instead of becoming a faster failure-backoff
// retry that would poll GitHub harder than configured. Adapted from the PMR-78
// review probe.
func TestCapacityBlockedLandingRetryKeepsItsCadence(t *testing.T) {
	w := testSettings(t)
	w.Config.Agent.MaxTurns = 20
	w.Config.Agent.MaxConcurrent = 1
	w.Config.Agent.MaxRetryBackoff = 10 * time.Minute
	w.Config.GitHub.PollInterval = 30 * time.Second
	issue := testIssue()
	agent := &fakeAgent{events: landingWaitingEvents}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{}, 4)}
	c := testCoordinator(w.Config, &fakeTracker{issue: issue}, agent, ws)
	defer assertInvariants(t, c)
	timer := &fakeTimer{signal: make(chan struct{}, 4)}
	c.timer = timer

	c.Tick(context.Background())
	<-ws.after
	<-timer.signal

	// Another issue takes the only orchestrator slot before the timer fires.
	c.occupySlot(domain.Issue{ID: "other", State: "Todo"})

	timer.fire(0)
	<-timer.signal

	retry, _ := c.armedRetry(issue.ID)
	claimed := c.claimHeld(issue.ID)
	if !claimed {
		t.Fatal("capacity-blocked landing retry dropped its claim")
	}
	if retry.kind != retryLanding || retry.reason != "landing_slot_unavailable" || retry.attempt != 0 {
		t.Fatalf("retry=%+v, want a landing retry on the same attempt", retry)
	}
	if len(timer.delays) != 2 || timer.delays[1] != 30*time.Second {
		t.Fatalf("delays=%v, want the landing cadence rather than a failure backoff", timer.delays)
	}
	if starts, _, _ := agent.counts(); starts != 1 {
		t.Fatalf("starts=%d, want no dispatch while the slot is taken", starts)
	}
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestLandingWaitRedispatchesPastMaxAttempts pins the exemption the ceiling
// must not swallow: a non-terminal landing wait is not an agent failure, so it
// keeps its unbounded redispatch and its unescalated attempt even after more
// dispatches than agent.max_attempts. Bounding it here would give up on a
// pull request whose checks are merely slow.
func TestLandingWaitRedispatchesPastMaxAttempts(t *testing.T) {
	w := testSettings(t)
	w.Config.Agent.MaxTurns = 20
	w.Config.Agent.MaxAttempts = 2
	w.Config.Agent.MaxRetryBackoff = 10 * time.Minute
	w.Config.GitHub.PollInterval = 30 * time.Second
	issue := testIssue()
	var log syncBuffer
	agent := &fakeAgent{events: landingWaitingEvents}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{}, 4)}
	c := New(&fakeTracker{issue: issue}, agent, ws, func() config.Settings { return w.Config }, slog.New(slog.NewJSONHandler(&log, nil)))
	defer assertInvariants(t, c)
	c.clock = fakeClock{now: time.Date(2026, 8, 26, 9, 41, 0, 0, time.UTC)}
	timer := &fakeTimer{signal: make(chan struct{}, 4)}
	c.timer = timer

	c.Tick(context.Background())
	<-ws.after
	<-timer.signal
	for fired := 0; fired < 2; fired++ {
		timer.fire(fired)
		<-ws.after
		<-timer.signal
	}

	if starts, _, _ := agent.counts(); starts != 3 {
		t.Fatalf("starts=%d, want landing to keep redispatching past max_attempts", starts)
	}
	retry, ok := c.armedRetry(issue.ID)
	claimed, waits := c.claimHeld(issue.ID), c.landingWaitsFor(issue.ID)
	if !ok || retry.kind != retryLanding || retry.attempt != 0 {
		t.Fatalf("retry=%+v ok=%v, want a further landing retry on the same attempt", retry, ok)
	}
	if !claimed || waits != 3 {
		t.Fatalf("claimed=%v wait_attempt=%d, want the claim held and only the wait count climbing", claimed, waits)
	}
	if records := log.String(); strings.Contains(records, "dispatch_abandoned") {
		t.Fatalf("a landing wait was abandoned at the agent ceiling: %s", records)
	}
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestLandingRetryDelayFloorsEscalatesAndCaps(t *testing.T) {
	settings := config.Settings{
		Polling: config.Polling{Interval: time.Minute},
		Agent:   config.Agent{MaxRetryBackoff: 90 * time.Second},
		GitHub:  config.GitHub{PollInterval: 30 * time.Second},
	}
	for _, test := range []struct {
		name     string
		settings config.Settings
		waits    int
		want     time.Duration
	}{
		{name: "first wait uses the github poll floor", settings: settings, waits: 1, want: 30 * time.Second},
		{name: "escalates past the floor", settings: settings, waits: 3, want: 40 * time.Second},
		{name: "capped by the retry ceiling", settings: settings, waits: 9, want: 90 * time.Second},
		{
			name:     "the poll floor is never undercut by a small ceiling",
			settings: config.Settings{Agent: config.Agent{MaxRetryBackoff: 5 * time.Second}, GitHub: config.GitHub{PollInterval: 30 * time.Second}},
			waits:    4,
			want:     30 * time.Second,
		},
		{
			name:     "falls back to the tracker poll interval",
			settings: config.Settings{Polling: config.Polling{Interval: 2 * time.Minute}, Agent: config.Agent{MaxRetryBackoff: 10 * time.Minute}},
			waits:    1,
			want:     2 * time.Minute,
		},
		{name: "unconfigured intervals use the documented default", settings: config.Settings{}, waits: 1, want: 30 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := landingRetryDelay(test.settings, test.waits); got != test.want {
				t.Fatalf("landingRetryDelay(waits=%d)=%s, want %s", test.waits, got, test.want)
			}
		})
	}
}

func TestLandingWaitEscalatedMatchesBackoffSaturation(t *testing.T) {
	for _, test := range []struct {
		name string
		max  time.Duration
		// waits is the smallest wait count at which backoff(waits, max) first
		// reaches max, so landingWaitEscalated must be false immediately
		// before it and true from it onward.
		waits int
	}{
		{name: "saturates on the fourth wait", max: 80 * time.Second, waits: 4},
		{name: "saturates on the third wait", max: 40 * time.Second, waits: 3},
		{name: "saturates immediately when the ceiling is at the starting delay", max: 10 * time.Second, waits: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			settings := config.Settings{Agent: config.Agent{MaxRetryBackoff: test.max}}
			if test.waits > 1 && landingWaitEscalated(settings, test.waits-1) {
				t.Fatalf("landingWaitEscalated(waits=%d) = true, want false (backoff not yet saturated)", test.waits-1)
			}
			if !landingWaitEscalated(settings, test.waits) {
				t.Fatalf("landingWaitEscalated(waits=%d) = false, want true (backoff saturated)", test.waits)
			}
			if !landingWaitEscalated(settings, test.waits+1) {
				t.Fatalf("landingWaitEscalated(waits=%d) = false, want true (backoff stays saturated)", test.waits+1)
			}
		})
	}
}

// TestLandingWaitLogEscalatesOnceThenStaysAtInfo reproduces the PMR-116 gap:
// a landing stuck behind a required check that will never report retries
// forever at Info, indistinguishable in the log from a slow but healthy
// check. Once landingWaits crosses the point where landingRetryDelay's
// backoff has saturated at the configured ceiling, one Warn record must name
// the issue and wait count -- and only one, not a repeat on every
// subsequent poll-cadence wait.
func TestLandingWaitLogEscalatesOnceThenStaysAtInfo(t *testing.T) {
	w := testSettings(t)
	w.Config.Agent.MaxRetryBackoff = 80 * time.Second
	w.Config.GitHub.PollInterval = time.Second
	issue := testIssue()
	var log syncBuffer
	c := New(&fakeTracker{issue: issue}, &fakeAgent{}, &fakeWorkspace{}, func() config.Settings { return w.Config }, slog.New(slog.NewJSONHandler(&log, nil)))
	defer assertInvariants(t, c)
	c.timer = &fakeTimer{}
	c.seedClaim(issue)

	const consecutiveWaits = 5
	for i := 0; i < consecutiveWaits; i++ {
		c.finishLandingWait(context.Background(), issue, 0, "required checks have not reported: ci/build")
	}

	var warnLine string
	warnCount := 0
	for _, line := range strings.Split(strings.TrimSpace(log.String()), "\n") {
		if !strings.Contains(line, `"msg":"landing wait retry scheduled"`) {
			continue
		}
		if strings.Contains(line, `"level":"WARN"`) {
			warnCount++
			warnLine = line
		} else if !strings.Contains(line, `"level":"INFO"`) {
			t.Fatalf("unexpected log level: %s", line)
		}
	}
	if warnCount != 1 {
		t.Fatalf("want exactly one Warn escalation across %d consecutive waits, got %d:\n%s", consecutiveWaits, warnCount, log.String())
	}
	if !strings.Contains(warnLine, `"wait_attempt":4`) || !strings.Contains(warnLine, `"issue_identifier":"ENG-1"`) || !strings.Contains(warnLine, `"reason":"required checks have not reported: ci/build"`) {
		t.Fatalf("warn line missing issue/wait_attempt/reason: %s", warnLine)
	}
}

// TestLandingWaitCountResetsOnAnInterleavedFailure pins landingWaits to the
// "consecutive" its whole escalation is built on (PMR-189). A landing run that
// fails rather than waits goes through finishFailure -> scheduleRetry, which
// keeps the claim the count is stored under, so before this the count survived
// an interleaved failure: the redispatch ladder climbed, and the stuck-landing
// Warn fired, on waits that were never consecutive.
func TestLandingWaitCountResetsOnAnInterleavedFailure(t *testing.T) {
	w := testSettings(t)
	w.Config.Agent.MaxAttempts = 10
	w.Config.Agent.MaxRetryBackoff = 10 * time.Minute
	// A poll interval below the first backoff step, so the ladder is visible in
	// the delays rather than being flattened onto the floor.
	w.Config.GitHub.PollInterval = 5 * time.Second
	issue := testIssue()
	var log syncBuffer
	c := New(&fakeTracker{issue: issue}, &fakeAgent{}, &fakeWorkspace{}, func() config.Settings { return w.Config }, slog.New(slog.NewJSONHandler(&log, nil)))
	defer assertInvariants(t, c)
	timer := &fakeTimer{}
	c.timer = timer
	c.seedClaim(issue)

	ctx := context.Background()
	c.finishLandingWait(ctx, issue, 0, "required checks are pending")
	c.finishLandingWait(ctx, issue, 0, "required checks are pending")
	if waits := c.landingWaitsFor(issue.ID); waits != 2 {
		t.Fatalf("waits=%d after two consecutive waits, want 2", waits)
	}

	// The failure keeps its claim -- which is exactly why the count could
	// outlive it -- so the reset asserted below cannot be coming from a release.
	c.finishFailure(ctx, issue, 0, "turn_limit_exhausted", turnLimitError{limit: 1})
	waits, claimed := c.landingWaitsFor(issue.ID), c.claimHeld(issue.ID)
	if waits != 0 || !claimed {
		t.Fatalf("waits=%d claimed=%v after an interleaved failure, want the count reset under a still-held claim", waits, claimed)
	}

	c.finishLandingWait(ctx, issue, 1, "required checks are pending")
	if waits := c.landingWaitsFor(issue.ID); waits != 1 {
		t.Fatalf("waits=%d after the failure, want the streak restarted at one", waits)
	}
	// The delays are what the count is for: the wait after the failure starts
	// the ladder again instead of continuing it at a third consecutive wait.
	want := []time.Duration{10 * time.Second, 20 * time.Second, 10 * time.Second, 10 * time.Second}
	if len(timer.delays) != len(want) {
		t.Fatalf("delays=%v, want %v", timer.delays, want)
	}
	for i, delay := range want {
		if timer.delays[i] != delay {
			t.Fatalf("delays=%v, want %v", timer.delays, want)
		}
	}
	var last string
	for _, line := range strings.Split(strings.TrimSpace(log.String()), "\n") {
		if strings.Contains(line, `"msg":"landing wait retry scheduled"`) {
			last = line
		}
	}
	if !strings.Contains(last, `"wait_attempt":1`) {
		t.Fatalf("the wait after the failure was reported as a continuing streak: %s", last)
	}
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestLandingRetryRefreshFailureIgnoresMaxAttempts pins the same exemption
// TestLandingWaitRedispatchesPastMaxAttempts pins for the wait itself: a
// landing retry that fails to refresh its issue is still not an agent
// failure, so it must keep redispatching past agent.max_attempts rather than
// being abandoned by the ceiling that only retryAgent consumes — and, like the
// slot-contention escalation in runRetry, it must not inflate the attempt
// that feeds the rendered prompt either.
func TestLandingRetryRefreshFailureIgnoresMaxAttempts(t *testing.T) {
	w := testSettings(t)
	w.Config.Agent.MaxAttempts = 1
	w.Config.Agent.MaxRetryBackoff = 15 * time.Second
	issue := testIssue()
	tracker := &issueMapTracker{issues: map[string]domain.Issue{issue.ID: issue}, getErr: errors.New("temporary tracker failure")}
	c := testCoordinator(w.Config, tracker, &fakeAgent{}, &fakeWorkspace{})
	defer assertInvariants(t, c)
	timer := &fakeTimer{}
	c.timer = timer

	if !claims(c, issue, w.Config) {
		t.Fatal("issue was not claimed")
	}
	c.scheduleRetry(context.Background(), issue, domain.Workspace{}, 3, retryLanding, "landing_waiting", time.Second)
	timer.fire(0)

	retry, ok := c.armedRetry(issue.ID)
	claimed := c.claimHeld(issue.ID)
	if !claimed {
		t.Fatal("landing retry refresh failure dropped its claim below max_attempts=1")
	}
	if !ok || retry.kind != retryLanding || retry.reason != "retry_refresh" || retry.attempt != 3 {
		t.Fatalf("retry=%+v ok=%v, want a further landing retry past the ceiling with its attempt unchanged", retry, ok)
	}
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}
