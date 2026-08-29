package coordinator

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/pmrrasmussen/symphony/internal/domain"
)

type retryKind string

const (
	retryAgent retryKind = "agent"
	// retryLanding is a coordinator-owned landing redispatch after the
	// host-side landing capability reported a non-terminal wait. It is
	// deliberately distinct from retryAgent: it is not an agent failure, it does
	// not escalate the attempt, and its delay follows the GitHub poll interval
	// instead of the failure backoff (PMR-78).
	retryLanding retryKind = "landing"
	// defaultPollRetryDelay is the fixed-cadence redispatch floor -- for a
	// contended landing slot or a contended orchestrator slot alike -- when no
	// poll interval is configured to derive one from.
	defaultPollRetryDelay = 30 * time.Second
)

// retryState is the coordinator's complete durable-in-process intent for a
// claimed issue. Durable completion itself remains in the workspace; this
// state only governs a live process and is discarded safely on restart.
type retryState struct {
	ctx        context.Context
	issue      domain.Issue
	workspace  domain.Workspace
	attempt    int
	kind       retryKind
	reason     string
	due        time.Time
	generation uint64
	timer      TimerHandle
}

type retryDelayError interface {
	RetryDelay() time.Duration
}

func pollDelay(interval time.Duration, err error) time.Duration {
	delay := interval
	var retry retryDelayError
	if errors.As(err, &retry) && retry.RetryDelay() > delay {
		delay = retry.RetryDelay()
	}
	return delay
}

// scheduleRetry arms one delayed redispatch and reports whether it did: a
// shutdown or a released claim declines it, so a caller that logs the retry
// separately can stay truthful.
func (c *Coordinator) scheduleRetry(ctx context.Context, i domain.Issue, ws domain.Workspace, attempt int, kind retryKind, reason string, delay time.Duration) bool {
	c.mu.Lock()
	st := c.claimedStateLocked(i.ID)
	if c.stopping || st == nil {
		c.mu.Unlock()
		return false
	}
	if st.retry != nil && st.retry.timer != nil {
		st.retry.timer.Stop()
	}
	c.nextRetry++
	generation := c.nextRetry
	due := c.clock.Now().Add(delay)
	st.retry = &retryState{ctx: ctx, issue: i, workspace: ws, attempt: attempt, kind: kind, reason: reason, due: due, generation: generation}
	c.mu.Unlock()
	handle := c.timer.AfterFunc(delay, func() { c.runRetry(i.ID, generation) })
	c.mu.Lock()
	if current := c.stateLocked(i.ID); current != nil && current.retry != nil && current.retry.generation == generation {
		current.retry.timer = handle
	} else if handle != nil {
		handle.Stop()
	}
	c.mu.Unlock()
	c.log.Info("agent retry scheduled", "issue_id", i.ID, "issue_identifier", i.Identifier, "retry_kind", kind, "reason", reason, "attempt", attempt, "due", due)
	return true
}

func (c *Coordinator) runRetry(id string, generation uint64) {
	c.mu.Lock()
	st := c.stateLocked(id)
	armed := st != nil && st.retry != nil
	stopping := c.stopping
	if !armed || st.retry.generation != generation || stopping {
		if armed && stopping {
			c.releaseLocked(id)
		}
		c.mu.Unlock()
		return // a stopped or superseded timer is stale
	}
	retry := *st.retry
	st.retry = nil
	c.mu.Unlock()
	ctx := retry.ctx
	if managed := c.context(); managed != nil {
		ctx = managed
	}
	if ctx == nil || ctx.Err() != nil {
		c.release(id)
		return
	}
	s := c.settings()
	fresh, err := c.tracker.GetIssues(ctx, []string{id})
	if err != nil {
		c.logIssueRefreshFailure(ctx, "retry issue refresh failed", "issue_id", id, "reason", retry.reason, "error", err)
		if retry.kind == retryLanding {
			// A stale tracker read is no more an agent failure than the wait
			// itself: keep the attempt (it feeds the rendered prompt) and the
			// landing cadence, exactly like the slot-contention branch below.
			waits := c.landingWaitsFor(id)
			c.scheduleRetry(ctx, retry.issue, retry.workspace, retry.attempt, retryLanding, "retry_refresh", landingRetryDelay(s, waits))
			return
		}
		// "retry_refresh" is systemic, so failureCounters holds the attempt fixed
		// here for the same reason the branch above holds it fixed for a landing:
		// the tracker being unreachable is not an attempt at this issue's work.
		// Only the backoff climbs, keyed to the outage's own streak.
		attempt, escalation, _ := c.failureCounters(id, retry.attempt, "retry_refresh")
		attrs := []any{"issue_id", retry.issue.ID, "issue_identifier", retry.issue.Identifier, "reason", "retry_refresh", "attempt", attempt}
		if c.attemptsExhausted(retry.issue, retry.kind, "retry_refresh", attempt, s, attrs) {
			return
		}
		c.scheduleRetry(ctx, retry.issue, retry.workspace, attempt, retry.kind, "retry_refresh", backoff(escalation, s.Agent.MaxRetryBackoff))
		return
	}
	if len(fresh) != 1 || fresh[0].ID != id {
		c.release(id)
		return
	}
	issue := fresh[0]
	if !eligible(issue, s) {
		if issueTerminal(issue, s) {
			c.cleanupWorkspace(ctx, issue)
		}
		c.release(id)
		return
	}
	if c.launch(ctx, issue, retry.attempt) {
		return
	}
	// The retry still owns its claim, but another admitted run used the slot
	// after we refreshed it. Keep that duplicate-prevention claim and retry on
	// a fixed cadence instead of dropping the work: losing a slot race is
	// capacity contention, not a failure, for either retry kind.
	if retry.kind == retryLanding {
		// A contended landing slot is no more an agent failure than the wait
		// itself: keep the attempt (it feeds the rendered prompt) and the
		// landing cadence, so a queued landing never polls GitHub faster than
		// the configured interval (PMR-78).
		waits := c.landingWaitsFor(id)
		c.scheduleRetry(ctx, issue, retry.workspace, retry.attempt, retryLanding, "landing_slot_unavailable", landingRetryDelay(s, waits))
		return
	}
	// A contended orchestrator slot is no more an agent failure than a
	// contended landing slot is (see the retryLanding branch above): keep the
	// attempt fixed and retry on agentSlotRetryDelay's fixed cadence instead
	// of attempt+1 and the escalating failure backoff, and do not consume
	// attemptsExhausted's ceiling. A healthy issue that merely loses slot
	// races against fresh poll candidates must keep waiting for capacity
	// indefinitely rather than being abandoned for a failure it never had.
	c.scheduleRetry(ctx, issue, retry.workspace, retry.attempt, retryAgent, "agent_slot_unavailable", agentSlotRetryDelay(s))
}

// stopRun cancels a run and waits for its backend cancellation inline. Its
// caller is usually reconcile, on the poll goroutine, so the wait is a cost that
// pass pays: cancelSession bounds each call at 5s, and a pass that stops several
// runs pays that per run before it polls again. Left as is: it is bounded and
// its callers depend on the cancellation having been delivered by the time they
// act on the stop, unlike the workspace cleanup a stopTerminal run also
// triggers, which had no bound at all until PMR-180 gave it one.
func (c *Coordinator) stopRun(id string, reason stopReason) bool {
	c.mu.Lock()
	r := c.runLocked(id)
	if r == nil || r.stopped != "" {
		c.mu.Unlock()
		return false
	}
	r.stopped = reason
	r.cancel()
	c.mu.Unlock()
	c.cancelSession(context.Background(), r.session)
	return true
}

func (c *Coordinator) cancelAll(ctx context.Context, runs []*running) {
	var wg sync.WaitGroup
	for _, r := range runs {
		wg.Add(1)
		go func(session domain.AgentSession) {
			defer wg.Done()
			c.cancelSession(ctx, session)
		}(r.session)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

func (c *Coordinator) cancelSession(parent context.Context, session domain.AgentSession) {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.agent.Cancel(ctx, session) }()
	select {
	case err := <-done:
		if err != nil {
			c.log.Warn("agent cancellation failed", "session_id", session.ID, "error", err)
		}
	case <-ctx.Done():
		c.log.Warn("agent cancellation timed out", "session_id", session.ID)
	}
}

func backoff(a int, max time.Duration) time.Duration {
	d := 10 * time.Second
	for n := 1; n < a && d < max; n++ {
		d *= 2
	}
	if d > max {
		return max
	}
	return d
}
