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
	next, escalation, systemic := c.failureCounters(i.ID, attempt, reason)
	attrs := []any{"issue_id", i.ID, "issue_identifier", i.Identifier, "reason", reason, "attempt", next}
	if systemic {
		// On this path escalation is the streak of consecutive systemic failures,
		// and with the attempt held fixed it is the only operator-visible measure
		// of how long the condition has been repeating.
		attrs = append(attrs, "systemic_failures", escalation)
	}
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
	if c.attemptsExhausted(i, reason, next, s, attrs) {
		return
	}
	c.log.Warn("agent run retry scheduled", attrs...)
	delay := backoff(escalation, s.Agent.MaxRetryBackoff)
	var limited rateLimitedError
	if errors.As(err, &limited) {
		// A backend-reported reset time always wins over the ordinary escalating
		// ladder: retrying against a limit already known to be closed is wasted
		// regardless of how many attempts or repeats have accumulated,
		// and the ladder's own cap (max_retry_backoff_ms) is far too short
		// for a multi-hour quota window (PMR-131).
		delay = rateLimitRetryDelay(limited.retryAfter, s.Agent.MaxRetryBackoff)
	}
	c.scheduleRetry(ctx, i, domain.Workspace{}, next, retryAgent, reason, delay)
}

// failureCounters resolves the two counters one classified retryAgent failure
// moves, and reports whether its reason was systemic: next is the attempt the
// redispatch runs under, and escalation is the count backoff keys its delay to.
// For an issue-attributable failure they are the one escalated attempt they
// have always been. recordFailureOutcome, which supplies the second, also ends
// the two streaks such a failure ends: the consecutive systemic failures, and
// the consecutive landing waits.
//
// A systemic reason splits them (PMR-179). It names a boundary the host or a
// shared backend crossed and says nothing about this issue's work (see
// systemicFailureReasons), so it must not spend the issue's attempt budget:
// next repeats the attempt unchanged, exactly as runRetry repeats it for a lost
// slot race. The delay must still escalate, though -- freezing it as well would
// turn a sustained outage into a fixed-cadence relaunch loop at the ladder's
// first rung, the failure mode agent.max_retry_backoff_ms exists to bound -- so
// it climbs the streak of consecutive systemic failures under this claim
// instead, which a genuine failure resets.
//
// docs/observability.md's "Abandoned dispatches" section holds the rest: what
// the escalated counter did to an issue's budget before this split, and what
// the streak is worth to an operator reading the retry warnings.
func (c *Coordinator) failureCounters(id string, attempt int, reason string) (next, escalation int, systemic bool) {
	systemic = systemicFailureReasons[reason]
	streak := c.recordFailureOutcome(id, systemic)
	if systemic {
		return attempt, streak, true
	}
	return attempt + 1, attempt + 1, false
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
// directly at a call site -- that name a boundary the coordinator's own host, or
// a shared backend, crosses rather than evidence that this issue's work is
// unworkable, so none of them arms attemptsExhausted's ceiling. Each says
// something about the shared environment dispatching the issue, and a transient
// account-wide condition would otherwise abandon every issue running at once.
//
// The map is deliberately not exhaustive over finishFailure's reasons: the
// absent ones ("workspace_prepare", "before_run", "prompt_render",
// "capability_prepare", "session_start", "stalled", "agent_blocked",
// "turn_limit_exhausted", "source_integrity") are issue-attributable and do arm
// the ceiling. Adding or removing an entry here changes which failures can
// abandon an issue, so state the case first. "source_integrity" is absent on purpose even though the write
// it reports may have come from another session (see attributeRefChanges): a
// dispatch that keeps ending with the source repository's branches moved must
// stop being redispatched, and the ceiling is what stops it.
//
// docs/observability.md's "Abandoned dispatches" section is the one description of
// what each of these six reasons is evidence of, what was observed live to
// justify it, and the PMR-128 narrowing the two tracker-refresh entries are
// waiting on.
var systemicFailureReasons = map[string]bool{
	"agent_event":        true,
	"agent_rate_limited": true,
	"issue_refresh":      true,
	"retry_refresh":      true,
	"session_continue":   true,
	"stream_closed":      true,
}

// attemptsExhausted reports whether next has reached agent.max_attempts for a
// dispatch episode, abandoning it (and releasing its claim) if so. The ceiling
// is consumed only by a genuine, classified dispatch failure that is not
// systemic (see systemicFailureReasons). Since PMR-179 a systemic failure no
// longer raises next either (see failureCounters), which is the stronger half
// of that exemption -- it keeps the budget intact rather than merely deferring
// the abandonment. The reason gate stays because it, not the arithmetic, is
// where the rule is stated: a systemic reason may never end a dispatch,
// whatever counter it arrives with.
//
// finishFailure is the one caller, and it only ever schedules retryAgent, which
// is why no retry kind is passed: a retryLanding redispatch -- whether from
// finishLandingWait or either escalation in runRetry -- never raises its
// attempt counter and so can never reach a ceiling, and neither does a
// retryAgent episode that merely lost an orchestrator slot race (see
// agentSlotRetryDelay). runRetry's failed pre-dispatch refresh used to call
// this too, on a "retry_refresh" that is itself systemic, so the check could
// never return true; it was removed in PMR-191 rather than left reading like a
// bound that exists.
//
// config rejects a non-positive max_attempts, so the MaxAttempts <= 0 case only
// covers a hand-built Settings, which keeps the pre-PMR-111 unbounded ladder
// rather than having a zero ceiling abandon every first failure.
func (c *Coordinator) attemptsExhausted(i domain.Issue, reason string, next int, s config.Settings, attrs []any) bool {
	if systemicFailureReasons[reason] || s.Agent.MaxAttempts <= 0 || next < s.Agent.MaxAttempts {
		return false
	}
	c.abandonDispatch(i, s, attrs)
	return true
}

// abandonDispatch ends one dispatch episode that reached agent.max_attempts
// (PMR-111). The escalated attempt counter is the ceiling's unit: a failure at
// attempt N ends the (N+1)th launch of the episode, so a boundary that fails
// every time dispatches exactly max_attempts times and arms no further retry.
//
// What it does NOT do is as deliberate as what it does. It does not touch the
// tracker: the issue stays where a human left it and stays re-poll-able, which
// is what makes this error-level record the only trace of the give-up. It does
// not comment either. And it does not apply to a landing wait -- finishLandingWait
// keeps its own unbounded redispatch on purpose (see landingRetryDelay), because
// a wait is not an agent failure, does not escalate the attempt, and never
// reaches here.
//
// Staying re-poll-able is not the same as being re-admissible at once, which is
// what it used to mean: the release below was the whole of the give-up, so the
// next poll started a fresh episode at attempt 0 and a permanently failing issue
// spent max_attempts launches per poll interval, for good -- a faster cadence
// than the backoff ceiling the episode had just climbed to. The cooldown is what
// separates the two (PMR-191): the issue stays visible and is admitted again on
// its own, but not before a window derived from the delays this coordinator
// already schedules.
//
// docs/observability.md's "Abandoned dispatches" section states why each of those
// decisions is a decision rather than an omission, and what the cooldown is worth
// to an operator reading the record.
func (c *Coordinator) abandonDispatch(i domain.Issue, s config.Settings, attrs []any) {
	window := abandonCooldownWindow(s)
	attrs = append(attrs, "operation", observability.OperationDispatchAbandoned, "max_attempts", s.Agent.MaxAttempts, "cooldown_ms", window.Milliseconds())
	// The record is written while the claim still stands, so an observer that
	// sees the issue released has necessarily already seen the give-up that
	// released it.
	c.log.Error("dispatch abandoned after max attempts", attrs...)
	c.startAbandonCooldown(i.ID, window)
}

// abandonCooldownMultiplier is how many times the longest delay the coordinator
// already schedules -- the intra-episode backoff ceiling, or the poll interval
// when that is longer -- an abandoned issue waits before a new episode may
// start. Ten is the same order-of-magnitude step rateLimitRetryDelay takes above
// the same ceiling, for the same reason: the next launch has to land well beyond
// the ladder the episode just exhausted, or giving up would be followed by a
// tighter relaunch cadence than the failures that provoked it. It stays a
// bounded multiple rather than a permanent quarantine because a human fix -- an
// edited hook, a corrected template -- must be picked up without a restart.
const abandonCooldownMultiplier = 10

// abandonCooldown is the coordinator's memory of one abandoned dispatch
// episode: the moment the issue may be admitted again, and nothing else --
// never issue content, and not the abandonment's own timestamp, which the error
// record already carries with its window. Exactly like handoffObservation it
// governs only a live process: a restart discards it and the issue is
// admissible on the new process's first poll.
type abandonCooldown struct {
	until time.Time
}

// abandonCooldownWindow derives how long one abandoned issue stays out of the
// admission set. There is no setting of its own: the two delays that already
// describe how fast this instance may act on an issue are the backoff ceiling
// an episode climbs to and the poll interval it would otherwise be re-admitted
// at, so the window is a fixed multiple of whichever is longer. Deriving it
// keeps the cooldown correct for a fast-polling instance and a slow one alike
// without asking an operator to keep a third number in step with the other two.
func abandonCooldownWindow(s config.Settings) time.Duration {
	longest := s.Agent.MaxRetryBackoff
	if s.Polling.Interval > longest {
		longest = s.Polling.Interval
	}
	if longest <= 0 {
		// config rejects a non-positive interval and backoff alike, so this covers
		// only a hand-built Settings; it uses the same floor the retry paths do.
		longest = defaultPollRetryDelay
	}
	return abandonCooldownMultiplier * longest
}

// startAbandonCooldown holds the issue out of admission for window and drops the
// claim the abandoned episode held, in one critical section so no concurrent
// poll can observe it released but not yet cooling. The order matters within it
// too: the cooldown is recorded first, which is what keeps releaseLocked's
// pruneLocked from deleting the record on its way out.
func (c *Coordinator) startAbandonCooldown(id string, window time.Duration) {
	until := c.clock.Now().Add(window)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ensureStateLocked(id).cooldown = &abandonCooldown{until: until}
	c.releaseLocked(id)
}

// cooldownActiveLocked reports whether an abandonment cooldown is still holding
// the issue out of admission. It tests the deadline rather than the record's
// presence, so a cooldown that elapsed since the last sweep admits the issue at
// once instead of waiting a poll for the sweep to catch up. Callers must hold
// c.mu.
func (c *Coordinator) cooldownActiveLocked(id string, now time.Time) bool {
	st := c.states[id]
	return st != nil && st.cooldown != nil && now.Before(st.cooldown.until)
}

// sweepAbandonCooldowns discards elapsed cooldowns, so an issue that is never
// polled again -- closed, deleted, or no longer a candidate -- leaves no record
// behind and the per-issue records stay bounded, exactly as
// sweepHandoffObservations keeps handoff memories bounded. It decides nothing:
// admission tests the deadline itself (see cooldownActiveLocked), so a sweep
// that has not run yet cannot hold an issue back.
func (c *Coordinator) sweepAbandonCooldowns(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, st := range c.states {
		if st.cooldown != nil && !now.Before(st.cooldown.until) {
			st.cooldown = nil
			c.pruneLocked(id)
		}
	}
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
