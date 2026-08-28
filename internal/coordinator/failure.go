package coordinator

import (
	"context"
	"errors"
	"time"

	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/domain"
	"github.com/pmrrasmussen/symphony/internal/observability"
)

func (c *Coordinator) finishFailure(ctx context.Context, i domain.Issue, attempt int, reason string, err error) {
	if ctx.Err() != nil {
		c.log.Info("agent run cancelled", "issue_id", i.ID, "issue_identifier", i.Identifier, "reason", cancellationReason("", ctx))
		c.release(i.ID)
		return
	}
	s := c.settings()
	next := attempt + 1
	attrs := []any{"issue_id", i.ID, "issue_identifier", i.Identifier, "reason", reason, "attempt", next}
	var blocked blockedError
	if errors.As(err, &blocked) {
		attrs = append(attrs, "blocker", blocked.category)
	}
	// The error is attached for every reason, including prompt_render (a Go
	// template error over repository-owned WORKFLOW.md content) and
	// agent_event (which, after PMR-115, is reserved for domain.EventFailed's
	// verbatim model/provider text). observability.safeAttr already routes an
	// "error"-valued attribute through observability.Text for every log call,
	// masking credential-shaped text and truncating it, so excluding a reason
	// here bought no additional safety -- it only discarded the three
	// host-generated diagnostics (stream_closed, issue_refresh,
	// session_continue) this issue exists to stop discarding.
	if err != nil {
		attrs = append(attrs, "error", err)
	}
	if c.attemptsExhausted(i, retryAgent, reason, next, s, attrs) {
		return
	}
	c.log.Warn("agent run retry scheduled", attrs...)
	delay := backoff(next, s.Agent.MaxRetryBackoff)
	var limited rateLimitedError
	if errors.As(err, &limited) {
		// A backend-reported reset time always wins over the ordinary
		// attempt-keyed ladder: retrying against a limit already known to be
		// closed is wasted regardless of how many attempts have accumulated,
		// and the ladder's own cap (max_retry_backoff_ms) is far too short
		// for a multi-hour quota window (PMR-131).
		delay = rateLimitRetryDelay(limited.retryAfter, s.Agent.MaxRetryBackoff)
	}
	c.scheduleRetry(ctx, i, domain.Workspace{}, next, retryAgent, reason, delay)
}

// rateLimitRetryDelay bounds the wait before a retryAgent episode ended by a
// Claude quota rejection is retried. It defers entirely to the backend's own
// reset time when one was reported: a five-hour window's remainder is
// routinely far longer than agent.max_retry_backoff_ms, and honoring the
// ordinary ladder instead is the bug this exists to fix (PMR-131: 203
// launches rejected across six issues at a five-minute cadence, against a
// limit with hours left to run). When Claude reports no reset at all, the
// floor is ten times the ordinary ceiling -- still bounded, but far enough
// above max_retry_backoff_ms that this path can never collapse back onto the
// ladder it exists to replace.
func rateLimitRetryDelay(retryAfter, maxRetryBackoff time.Duration) time.Duration {
	if retryAfter > 0 {
		return retryAfter
	}
	return 10 * maxRetryBackoff
}

// systemicFailureReasons are the reasons -- from agentFailureReason or raised
// directly at a call site -- that name a boundary the coordinator's own
// host, or a shared backend, crosses -- never evidence that this issue's
// work is unworkable -- so none of them arms attemptsExhausted's ceiling:
//
//   - "agent_event": the ceiling's original exemption (PMR-111). It is
//     agentFailureReason's fallback for domain.EventFailed carrying model
//     or provider text the coordinator cannot itself name.
//   - "agent_rate_limited": a Claude quota rejection (PMR-131), named by its
//     own reason rather than falling through to "agent_event" as it did
//     before this reason existed. Observed live: 203 such rejections across
//     six healthy issues in one 2.5-hour window, an account-wide condition
//     that would have abandoned every one of them under the ceiling -- and
//     finishFailure schedules its retry from the backend's own reset time
//     rather than the ordinary ladder (see rateLimitRetryDelay), so the
//     exemption here is what stops the ceiling from cutting that wait short
//     with an abandonment instead.
//   - "issue_refresh": a tracker error from runTurns' post-turn GetIssues
//     refresh (PMR-115, confirmed live: a 30s Linear client timeout
//     following a turn the agent completed successfully). That is Linear
//     infrastructure, not this issue: with this codebase's default
//     max_attempts=5 and its escalating backoff, a two-and-a-half-minute
//     Linear outage would otherwise abandon every issue currently running,
//     since they all fail the same way at the same time.
//   - "retry_refresh": the same tracker.GetIssues failure, observed instead
//     at runRetry's pre-dispatch refresh (PMR-142). It is the same Linear
//     infrastructure as "issue_refresh", just crossed at a different
//     moment -- the moment an issue is waiting to redispatch rather than
//     the moment one just finished a turn -- and a sustained outage drives
//     every retrying issue through exactly this site, raising `attempt`
//     each time. Governing it from the same map as "issue_refresh" is
//     deliberate: the two must not be able to drift into opposite verdicts
//     on identical evidence again.
//   - "session_continue": Symphony's own backend adapter (agent.Continue)
//     failing to resume a session. A broken `claude` binary or lapsed
//     backend auth fails every running issue's next turn the same way, at
//     the same time -- the same account-wide shape as the quota rejection
//     above, just raised by Symphony's own code instead of the model's.
//   - "stream_closed": the host's own event plumbing failing to deliver a
//     verdict (see errStreamClosed). By construction every backend emits
//     a terminal event before its channel closes, so this can never be a
//     repository- or issue-specific outcome -- only ever a host bug, and a
//     host bug affects whichever issues happen to be running when it
//     fires, not the one that happened to surface it first.
//
// Once PMR-128 gives tracker errors a real Retryable signal, "issue_refresh"
// and "retry_refresh" can narrow together from a blanket exemption to "arm
// the ceiling only when the wrapped error is not Retryable" -- but that is
// future work, not a precondition for this one.
//
// Of agentFailureReason's outputs, only "turn_limit_exhausted" and
// "agent_blocked" are left to arm the ceiling -- each is evidence about
// *this* issue's run (it exhausted its turns; its own agent reported a
// blocker), the same way "prompt_render" is evidence about this issue's own
// WORKFLOW.md template. None of the six reasons above says anything about
// the issue at all; each says something about the shared environment
// dispatching it. This map is not exhaustive for finishFailure's other
// direct reasons ("workspace_prepare", "before_run", "prompt_render",
// "session_start", "stalled") -- those are deliberately absent because they
// are issue-attributable, not systemic.
var systemicFailureReasons = map[string]bool{
	"agent_event":        true,
	"agent_rate_limited": true,
	"issue_refresh":      true,
	"retry_refresh":      true,
	"session_continue":   true,
	"stream_closed":      true,
}

// attemptsExhausted reports whether next has reached agent.max_attempts for a
// retryAgent episode, abandoning the dispatch (and releasing its claim) if so.
// Only retryAgent consumes the ceiling, and only on a genuine, classified
// dispatch failure that is not systemic (see systemicFailureReasons): a
// retryLanding redispatch — whether from finishLandingWait or either
// escalation in runRetry — never raises its attempt counter, and neither does
// a retryAgent episode that merely lost an orchestrator slot race (see
// agentSlotRetryDelay). config rejects a non-positive max_attempts, so the
// MaxAttempts <= 0 case only covers a hand-built Settings, which keeps the
// pre-PMR-111 unbounded ladder rather than having a zero ceiling abandon
// every first failure.
func (c *Coordinator) attemptsExhausted(i domain.Issue, kind retryKind, reason string, next int, s config.Settings, attrs []any) bool {
	if kind != retryAgent || systemicFailureReasons[reason] || s.Agent.MaxAttempts <= 0 || next < s.Agent.MaxAttempts {
		return false
	}
	c.abandonDispatch(i, s.Agent.MaxAttempts, attrs)
	return true
}

// abandonDispatch ends one dispatch episode that reached agent.max_attempts
// (PMR-111). The escalated attempt counter is the ceiling's unit: a failure at
// attempt N ends the (N+1)th launch of the episode, so a boundary that fails
// every time dispatches exactly max_attempts times and arms no further retry.
// Of runRetry's two host-side escalations for a retryAgent episode, only the
// failed retry refresh raises that counter and routes it through
// attemptsExhausted at all -- but "retry_refresh" is itself one of
// systemicFailureReasons' exemptions (PMR-142), so that check never actually
// abandons the episode; the counter keeps climbing the ordinary backoff
// ladder instead, the same way a classified "issue_refresh" failure does.
// The other escalation, a contended orchestrator slot, never reaches here at
// all: it is capacity contention rather than a failure, so it keeps the
// attempt fixed (see agentSlotRetryDelay), the same way a retryLanding
// episode's own escalations do (see runRetry), since the attempt feeds the
// rendered prompt and neither a wait nor contention should inflate it.
//
// What it does NOT do is as deliberate as what it does:
//
//   - It does not touch the tracker. The issue stays where a human left it and
//     stays re-poll-able, so a later poll starts a fresh, equally bounded
//     episode rather than an in-process loop nothing can kill. That makes this
//     record load-bearing: it is the only trace of the give-up, and it is at
//     error level because abandonment bounds one episode rather than
//     quarantining the issue — with nobody acting on it, new episodes keep
//     starting at the poll interval.
//   - It does not comment either, and that is the same decision rather than an
//     omission. The coordinator holds only domain.Tracker (candidates, refresh,
//     and the one start edge), and an abandoned dispatch has often failed before
//     any session existed (workspace_prepare, before_run, prompt_render), so
//     there is no HandoffSession whose LandComment shape could be reused —
//     only a new host tracker-write path, for a record that would repeat on
//     every episode.
//   - It does not apply to a landing wait. finishLandingWait keeps its own
//     unbounded redispatch on purpose (see landingRetryDelay): a wait is not an
//     agent failure, does not escalate the attempt, and never reaches here.
func (c *Coordinator) abandonDispatch(i domain.Issue, maxAttempts int, attrs []any) {
	attrs = append(attrs, "operation", observability.OperationDispatchAbandoned, "max_attempts", maxAttempts)
	c.log.Error("dispatch abandoned after max attempts", attrs...)
	c.release(i.ID)
}

// finishLandingWait ends a run whose landing capability reported a
// non-terminal wait. A wait is not an agent failure: the same attempt is
// redispatched after landingRetryDelay, so it consumes neither Codex turns nor
// the failure backoff escalation, and while the timer waits it holds only the
// duplicate-prevention claim — never an orchestrator slot (PMR-78).
func (c *Coordinator) finishLandingWait(ctx context.Context, i domain.Issue, attempt int, reason string) {
	if ctx.Err() != nil {
		c.log.Info("agent run cancelled", "issue_id", i.ID, "issue_identifier", i.Identifier, "reason", cancellationReason("", ctx))
		c.release(i.ID)
		return
	}
	s := c.settings()
	c.mu.Lock()
	var waits int
	escalate := false
	// The wait is counted against the claim it happened under, so an issue that
	// has already lost its claim records nothing and scheduleRetry below
	// declines the redispatch anyway.
	if st := c.claimedStateLocked(i.ID); st != nil {
		st.landingWaits++
		waits = st.landingWaits
		escalate = landingWaitEscalated(s, waits) && !st.landingEscalated
		st.landingEscalated = st.landingEscalated || escalate
	}
	c.mu.Unlock()
	delay := landingRetryDelay(s, waits)
	if !c.scheduleRetry(ctx, i, domain.Workspace{}, attempt, retryLanding, "landing_waiting", delay) {
		return
	}
	attrs := []any{"operation", "landing_waiting", "issue_id", i.ID, "issue_identifier", i.Identifier, "reason", reason, "attempt", attempt, "wait_attempt", waits, "delay_ms", delay.Milliseconds()}
	if escalate {
		c.log.Warn("landing wait retry scheduled", attrs...)
		return
	}
	c.log.Info("landing wait retry scheduled", attrs...)
}

// landingRetryDelay bounds the wait before a landing is redispatched. Its floor
// is the configured GitHub poll interval -- the cadence at which the GitHub
// state being waited on can change -- and it escalates with the number of
// consecutive waits toward agent.max_retry_backoff_ms, so a gate that never
// settles cannot respawn a session every poll interval forever. The coordinator
// deliberately does not itself give up on a stuck landing: returning the issue
// to review is the landing capability's own bounded, commented authority
// (github_land_pr's merge_state -> In Review fallback), and a Merging issue that
// stops making progress stays visible on the board with a climbing wait_attempt.
// The floor is never undercut, so a small backoff ceiling cannot turn landing
// into a tighter GitHub poll than the configured interval.
func landingRetryDelay(s config.Settings, waits int) time.Duration {
	floor := s.GitHub.PollInterval
	if floor <= 0 {
		floor = s.Polling.Interval
	}
	if floor <= 0 {
		// The same fallback the GitHub linked-PR poll loop uses when no interval
		// is configured (see internal/github.Manager.Run).
		floor = defaultPollRetryDelay
	}
	// backoff already caps its escalation at the ceiling, and the poll floor
	// always wins: a small max_retry_backoff_ms must never make landing poll
	// GitHub faster than its configured interval.
	delay := floor
	if escalated := backoff(waits, s.Agent.MaxRetryBackoff); escalated > delay {
		delay = escalated
	}
	return delay
}

// landingWaitEscalated reports whether waits has reached the point where
// backoff's own escalation has saturated at agent.max_retry_backoff_ms: the
// ceiling landingRetryDelay's ladder climbs toward and then holds at. Below
// that point a wait's Info-level log is enough -- the redispatch delay is
// still climbing on its own. At and above it a wait that is still recurring
// is no longer distinguishable, on the timeline alone, from a landing that
// will never settle (PMR-116), so finishLandingWait raises the log level to
// Warn the first time this turns true for the issue and leaves it there.
func landingWaitEscalated(s config.Settings, waits int) bool {
	max := s.Agent.MaxRetryBackoff
	return backoff(waits, max) >= max
}

// agentSlotRetryDelay bounds the wait before a retryAgent episode that lost
// the race for an orchestrator slot is retried. Unlike landingRetryDelay it
// does not escalate: a contended slot is no more an agent failure than a
// contended landing slot (see the runRetry branch that calls this), so there
// is no failure ladder to climb and no ceiling for it to threaten -- just the
// poll interval, the cadence at which a fresh admission could actually free
// one up.
func agentSlotRetryDelay(s config.Settings) time.Duration {
	if s.Polling.Interval > 0 {
		return s.Polling.Interval
	}
	return defaultPollRetryDelay
}
