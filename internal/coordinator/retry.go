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
	if c.stopping || !c.claimed[i.ID] {
		c.mu.Unlock()
		return false
	}
	if previous, ok := c.retries[i.ID]; ok && previous.timer != nil {
		previous.timer.Stop()
	}
	c.nextRetry++
	generation := c.nextRetry
	state := retryState{ctx: ctx, issue: i, workspace: ws, attempt: attempt, kind: kind, reason: reason, due: c.clock.Now().Add(delay), generation: generation}
	c.retries[i.ID] = state
	c.mu.Unlock()
	handle := c.timer.AfterFunc(delay, func() { c.runRetry(i.ID, generation) })
	c.mu.Lock()
	current, ok := c.retries[i.ID]
	if ok && current.generation == generation {
		current.timer = handle
		c.retries[i.ID] = current
	} else if handle != nil {
		handle.Stop()
	}
	c.mu.Unlock()
	c.log.Info("agent retry scheduled", "issue_id", i.ID, "issue_identifier", i.Identifier, "retry_kind", kind, "reason", reason, "attempt", attempt, "due", state.due)
	return true
}

func (c *Coordinator) runRetry(id string, generation uint64) {
	c.mu.Lock()
	retry, ok := c.retries[id]
	stopping := c.stopping
	if !ok || retry.generation != generation || stopping {
		c.mu.Unlock()
		if ok && stopping {
			c.release(id)
		}
		return // a stopped or superseded timer is stale
	}
	delete(c.retries, id)
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
			c.mu.Lock()
			waits := c.landingWaits[id]
			c.mu.Unlock()
			c.scheduleRetry(ctx, retry.issue, retry.workspace, retry.attempt, retryLanding, "retry_refresh", landingRetryDelay(s, waits))
			return
		}
		attempt := retry.attempt + 1
		attrs := []any{"issue_id", retry.issue.ID, "issue_identifier", retry.issue.Identifier, "reason", "retry_refresh", "attempt", attempt}
		if c.attemptsExhausted(retry.issue, retry.kind, "retry_refresh", attempt, s, attrs) {
			return
		}
		c.scheduleRetry(ctx, retry.issue, retry.workspace, attempt, retry.kind, "retry_refresh", backoff(attempt, s.Agent.MaxRetryBackoff))
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
		c.mu.Lock()
		waits := c.landingWaits[id]
		c.mu.Unlock()
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

func (c *Coordinator) stopRun(id string, reason stopReason) bool {
	c.mu.Lock()
	r, ok := c.running[id]
	if !ok || r.stopped != "" {
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

func (c *Coordinator) release(id string) {
	c.mu.Lock()
	if retry, ok := c.retries[id]; ok && retry.timer != nil {
		retry.timer.Stop()
	}
	delete(c.claimed, id)
	delete(c.claimState, id)
	delete(c.admitted, id)
	delete(c.retries, id)
	delete(c.landingWaits, id)
	delete(c.landingEscalated, id)
	c.mu.Unlock()
}

func (c *Coordinator) unreserve(id string) {
	c.mu.Lock()
	delete(c.admitted, id)
	c.mu.Unlock()
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
