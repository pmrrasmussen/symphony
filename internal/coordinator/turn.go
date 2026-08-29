package coordinator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/domain"
	"github.com/pmrrasmussen/symphony/internal/observability"
)

const continuationDelay = time.Second

func (c *Coordinator) runTurns(ctx context.Context, r *running, events <-chan domain.Event, settings config.Settings) (ended bool, current domain.Issue, err error) {
	c.mu.Lock()
	current = r.issue
	c.mu.Unlock()
	for {
		completed, err := c.consume(ctx, r, events)
		if !completed {
			return false, current, err
		}
		fresh, err := c.tracker.GetIssues(ctx, []string{current.ID})
		if err != nil {
			return false, current, issueRefreshError{err: err}
		}
		if len(fresh) != 1 || fresh[0].ID != current.ID {
			return true, current, nil
		}
		current = fresh[0]
		c.refreshRunIssue(r, current)
		c.mu.Lock()
		turnCount := r.run.TurnCount
		c.mu.Unlock()
		if !eligible(current, settings) {
			if issueTerminal(current, settings) {
				c.cleanupWorkspaceAtRunEnd(ctx, r, current)
			}
			return true, current, nil
		}
		c.mu.Lock()
		landingResolved := r.landingResolved
		c.mu.Unlock()
		if landingResolved {
			// Landing already merged the pull request and reconciled the issue.
			// End the run here even when this refresh still reports the pre-merge
			// state, so no later turn or landing tool call is possible (PMR-78).
			// This issue will never be polled or retried again, so the workspace
			// must be released here exactly as on the terminal-state path above.
			c.cleanupWorkspaceAtRunEnd(ctx, r, current)
			return true, current, nil
		}
		if turnCount >= settings.Agent.MaxTurns {
			return false, current, turnLimitError{limit: settings.Agent.MaxTurns}
		}
		if err := c.waitForContinuation(ctx); err != nil {
			return false, current, err
		}
		guidance := continuationGuidance(turnCount+1, settings.Agent.MaxTurns)
		events, err = c.agent.Continue(ctx, r.session, guidance)
		if err != nil {
			return false, current, sessionContinueError{err: err}
		}
		c.mu.Lock()
		r.run.TurnCount++
		c.mu.Unlock()
	}
}

func isLandingWait(err error) bool {
	_, ok := landingWait(err)
	return ok
}

func agentFailureReason(err error) string {
	var blocked blockedError
	if errors.As(err, &blocked) {
		return "agent_blocked"
	}
	var limited rateLimitedError
	if errors.As(err, &limited) {
		return "agent_rate_limited"
	}
	var exhausted turnLimitError
	if errors.As(err, &exhausted) {
		return "turn_limit_exhausted"
	}
	if errors.Is(err, errStreamClosed) {
		return "stream_closed"
	}
	var refresh issueRefreshError
	if errors.As(err, &refresh) {
		return "issue_refresh"
	}
	var cont sessionContinueError
	if errors.As(err, &cont) {
		return "session_continue"
	}
	// Anything else reaching here is domain.EventFailed carrying e.Message
	// verbatim (consume, "agent failed: %s") -- genuine model or provider
	// text, the one case this reason is now reserved for.
	return "agent_event"
}

func (c *Coordinator) waitForContinuation(ctx context.Context) error {
	fired := make(chan struct{})
	timer := c.timer.AfterFunc(continuationDelay, func() { close(fired) })
	select {
	case <-ctx.Done():
		timer.Stop()
		return ctx.Err()
	case <-fired:
		return nil
	}
}

// runBackend records the runtime that actually created the session. The router
// stamps it, so a reload between resolving the launch and starting the session
// cannot leave the run tagged with a backend it never ran on. The resolved
// launch is only a fallback, for a backend that does not stamp sessions.
func runBackend(session domain.AgentSession, launch config.AgentLaunch) string {
	if backend := strings.TrimSpace(session.Backend); backend != "" {
		return backend
	}
	return launch.Backend
}

func continuationGuidance(turn, maxTurns int) string {
	// The upstream workflow schema configures the turn bound but deliberately
	// has no continuation-prompt field. Generate its prescribed guidance here
	// so continuation turns do not resend the repository task template.
	//
	// This text is backend-neutral on purpose, and the rule that decides what may
	// live here is what keeps it so: anything that varies from turn to turn belongs
	// in this function, and everything else -- tool naming included -- belongs in
	// the initial prompt, which every fresh dispatch and every resume replays.
	// Neither backend's vocabulary ("workpad" and "thread"; tool-name prefixes) may
	// appear here, because the other backend has no such thing.
	return fmt.Sprintf(`Continuation guidance:

- The previous agent turn completed normally, but the tracker work item is still in an active state.
- This is continuation turn #%d of %d for the current agent run.
- Resume from the current workspace and session state instead of restarting from scratch.
- The original task instructions and prior turn context are already present in this session, so do not restate them before acting.
- Focus on the remaining ticket work and do not end the turn while the issue stays active unless you are truly blocked.`, turn, maxTurns)
}

func (c *Coordinator) finishRun(r *running, completed bool, stopped stopReason, ctx context.Context, err error) {
	c.mu.Lock()
	// A failure is classified from the typed error it ended with -- the same
	// classification finishFailure's reason and retry policy come from -- and
	// never from that error's text. Matching "timeout" anywhere in the message
	// recorded a tracker outage wrapped in an issueRefreshError as an agent
	// timeout: a status about the agent, asserted from evidence about Linear
	// (PMR-179).
	switch reason := agentFailureReason(err); {
	case stopped == stopStalled:
		r.run.Status = domain.RunStalled
	case stopped != "" || ctx.Err() != nil:
		r.run.Status = domain.RunCanceled
	case completed:
		r.run.Status = domain.RunSucceeded
	case isLandingWait(err):
		r.run.Status = domain.RunWaiting
	case reason == "agent_blocked", reason == "turn_limit_exhausted":
		r.run.Status = domain.RunBlocked
	default:
		r.run.Status = domain.RunFailed
	}
	run := r.run
	c.mu.Unlock()
	c.log.Info("agent logical run finished", "issue_id", run.IssueID, "issue_identifier", run.IssueIdentifier, "session_id", run.SessionID, "status", string(run.Status), "attempt", run.Attempt, "turn_count", run.TurnCount)
}

func (c *Coordinator) consume(ctx context.Context, r *running, events <-chan domain.Event) (bool, error) {
	for {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case e, ok := <-events:
			if !ok {
				return false, errStreamClosed
			}
			at := e.At
			if at.IsZero() {
				at = c.clock.Now()
			}
			e.At = at
			c.mu.Lock()
			r.last = at
			c.mu.Unlock()
			c.logEvent(r, e)
			switch e.Kind {
			case domain.EventBlocked:
				return false, blockedError{category: blockerCategory(e.Message)}
			case domain.EventRateLimited:
				return false, rateLimitedError{retryAfter: e.RetryAfter}
			case domain.EventFailed:
				return false, fmt.Errorf("agent failed: %s", e.Message)
			case domain.EventLandingWaiting:
				// A landing wait ends the run without another turn; the
				// coordinator owns the delayed landing retry from here (PMR-78).
				return false, landingWaitError{reason: observability.Text(e.Message)}
			case domain.EventLandingResolved, domain.EventCompleted:
				if e.Kind == domain.EventLandingResolved {
					c.mu.Lock()
					r.landingResolved = true
					c.mu.Unlock()
				}
				// Reconciliation and event delivery can race. An event that
				// arrives after reconciliation has canceled this run must never
				// turn into a successful terminal outcome.
				c.mu.Lock()
				stopped := r.stopped
				stopping := c.stopping
				c.mu.Unlock()
				if stopped != "" || stopping || ctx.Err() != nil {
					if err := ctx.Err(); err != nil {
						return false, err
					}
					return false, context.Canceled
				}
				c.logTerminalSummary(r)
				return true, nil
			}
		}
	}
}
