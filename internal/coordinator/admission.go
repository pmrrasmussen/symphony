package coordinator

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/domain"
)

// The three disjoint causes waitingState.reason can hold, mirroring the
// identically named ineligibleReason/claim values that produce them: an issue
// is eligible and short only of an orchestrator slot, held ineligible by an
// open blocker relation (PMR-146), or serving the cooldown that follows an
// abandoned dispatch episode (PMR-191). They are mutually exclusive by
// construction: dispatchable() decides Dispatchable before capacity is ever
// consulted, and claim tests the cooldown before capacity, so an issue is
// never reported under two of them.
const (
	waitReasonAtCapacity        = "at_capacity"
	waitReasonBlockedByRelation = "blocked_by_relation"
	waitReasonAbandonCooldown   = "abandon_cooldown"
)

// waitingCandidate is one poll's observation of an issue sitting idle for one
// of the three wait reasons above, collected by tick and handed to
// updateWaiting to reconcile against the coordinator's own memory. blockedBy is
// set only when reason is waitReasonBlockedByRelation.
type waitingCandidate struct {
	issue     domain.Issue
	reason    string
	blockedBy []string
}

func (c *Coordinator) tick(ctx context.Context) error {
	if ctx.Err() != nil || c.isStopping() {
		return ctx.Err()
	}
	if err := c.reconcile(ctx); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	s := c.settings()
	now := c.clock.Now()
	c.sweepHandoffObservations(now, s)
	c.sweepAbandonCooldowns(now)
	issues, err := c.tracker.ListCandidates(ctx, s.Tracker.ActiveStates)
	if err != nil {
		c.log.Error("candidate poll failed", "error", err)
		return err
	}
	sortIssues(issues)
	summary := pollSummary{candidates: len(issues), rejected: map[string]int64{}}
	waiting := map[string]waitingCandidate{}
	for _, i := range issues {
		if ctx.Err() != nil || c.isStopping() {
			c.logPollSummary(summary)
			return ctx.Err()
		}
		// A candidate the tracker returns in an active state that Symphony itself
		// just handed off to the review state was moved by someone else: a human
		// review decision (approve for landing, or send back for rework), or an
		// external reversion of the handoff (see PMR-63: Linear's native GitHub
		// PR automation). Log the external delta, classified, so the edge is
		// visible in the JSONL instead of only in Linear's history. Symphony does
		// not itself re-assert the handoff here.
		c.notePostHandoffStateChange(i, s, now)
		if reason := ineligibleReason(i, s); reason != "" {
			summary.rejected[reason]++
			attrs := []any{"issue_identifier", i.Identifier, "reason", reason}
			if reason == waitReasonBlockedByRelation {
				blockers := blockerIdentifierList(openBlockers(i))
				attrs = append(attrs, "blocked_by", strings.Join(blockers, ","))
				waiting[i.ID] = waitingCandidate{issue: i, reason: waitReasonBlockedByRelation, blockedBy: blockers}
			}
			c.log.Debug("poll candidate rejected", attrs...)
			continue
		}
		summary.eligible++
		if ok, reason := c.claim(i, s); !ok {
			summary.rejected[reason]++
			c.log.Debug("poll candidate rejected", "issue_identifier", i.Identifier, "reason", reason)
			if reason == waitReasonAtCapacity || reason == waitReasonAbandonCooldown {
				waiting[i.ID] = waitingCandidate{issue: i, reason: reason}
			}
			continue
		}
		summary.admitted++
		if !c.launch(ctx, i, 0) {
			c.release(i.ID)
			summary.admitted--
			summary.rejected["launch_reservation_lost"]++
		}
	}
	c.updateWaiting(waiting, now, s)
	c.logPollSummary(summary)
	return nil
}

// pollSummary is the opt-in debug accounting of one poll pass: how many
// candidates the tracker returned, how many were eligible, how many were
// admitted (reserved an orchestrator slot), and a categorized count of every
// rejection. It never carries issue identifiers or content, only counts.
type pollSummary struct {
	candidates, eligible, admitted int
	rejected                       map[string]int64
}

func (c *Coordinator) logPollSummary(summary pollSummary) {
	attrs := []any{"candidates", summary.candidates, "eligible", summary.eligible, "admitted", summary.admitted}
	if len(summary.rejected) > 0 {
		attrs = append(attrs, "rejected", summary.rejected)
	}
	c.log.Debug("poll summary", attrs...)
}

// waitingEscalationFloor is the lower bound on how long an issue can sit
// unadmitted for capacity, or held by an open blocker, before the wait is
// escalated to Warn. The effective threshold is max(this,
// waitingEscalationMultiplier*poll interval), mirroring
// handoffObservationFloor, so a fast-polling instance does not warn after a
// couple of missed cycles and a slow-polling one is not held to an
// unreasonably short deadline.
const waitingEscalationFloor = 5 * time.Minute

// waitingEscalationMultiplier is how many poll intervals an issue may go
// unadmitted, or blocker-held, before waitingEscalationFloor's alternative
// kicks in. It is deliberately generous: losing one or two admission races to
// fresher candidates (PMR-129) is the queue working as designed, not a stuck
// issue, and a freshly opened blocker relation deserves the same grace before
// it is treated as a dependency nobody intends to schedule (PMR-152).
const waitingEscalationMultiplier = 10

// updateWaiting reconciles the coordinator's memory of issues seen this poll
// under any of the three wait reasons against the previous poll's memory, and
// reports every genuinely new entry -- or one whose reason just changed --
// once, by logging its identifier, state, and (for a blocker hold) the blocker
// itself: the things pollSummary is not allowed to carry. An issue absent from
// seen this poll is no longer waiting for whatever reason (admitted, unblocked,
// cooled down, turned ineligible for some other reason, or dropped from the
// tracker's candidate list) and its memory is
// dropped here, so the waiting set can only ever describe issues this exact
// poll actually re-observed.
func (c *Coordinator) updateWaiting(seen map[string]waitingCandidate, now time.Time, s config.Settings) {
	c.mu.Lock()
	for id, st := range c.states {
		if _, ok := seen[id]; st.waiting != nil && !ok {
			st.waiting = nil
			st.waitingEscalated = false
			c.pruneLocked(id)
		}
	}
	var newlyWaiting []waitingCandidate
	for id, candidate := range seen {
		st := c.ensureStateLocked(id)
		if st.waiting == nil || st.waiting.reason != candidate.reason {
			st.waiting = &waitingState{issue: candidate.issue, reason: candidate.reason, blockedBy: candidate.blockedBy, since: now}
			st.waitingEscalated = false
			newlyWaiting = append(newlyWaiting, candidate)
			continue
		}
		st.waiting.issue = candidate.issue
		st.waiting.blockedBy = candidate.blockedBy
	}
	c.mu.Unlock()
	for _, candidate := range newlyWaiting {
		c.logWaiting(c.log.Info, candidate.issue, candidate.reason, candidate.blockedBy, false)
	}
	c.escalateStuckWaits(now, s)
}

// logWaiting emits the "just entered" or "still stuck" waiting record at the
// given level, naming the blocker for a waitReasonBlockedByRelation entry
// exactly as the poll-rejection debug record does (PMR-146), and never
// carrying anything beyond identifiers.
func (c *Coordinator) logWaiting(level func(string, ...any), issue domain.Issue, reason string, blockedBy []string, stuck bool) {
	attrs := []any{"issue_id", issue.ID, "issue_identifier", issue.Identifier, "issue_state", config.Norm(issue.State)}
	if reason == waitReasonBlockedByRelation {
		attrs = append(attrs, "blocked_by", strings.Join(blockedBy, ","))
	}
	level(waitingMessage(reason, stuck), attrs...)
}

// waitingMessage is one waiting record's text. Only the two open-ended waits
// have a "still" wording: an abandonment cooldown ends at a deadline the
// coordinator set itself, so escalateStuckWaits never raises one and it needs
// no second phrasing.
func waitingMessage(reason string, stuck bool) string {
	switch reason {
	case waitReasonBlockedByRelation:
		if stuck {
			return "issue still blocked by an open dependency"
		}
		return "issue blocked by an open dependency"
	case waitReasonAbandonCooldown:
		return "issue cooling down after an abandoned dispatch"
	default:
		if stuck {
			return "issue still waiting for capacity"
		}
		return "issue eligible but waiting for capacity"
	}
}

// escalateStuckWaits raises a one-time Warn for any issue that has sat in the
// waiting set past waitingEscalationFloor's effective threshold, exactly the
// way finishLandingWait escalates a landing wait past
// landingWaitEscalated. Below the threshold the Info logged by updateWaiting
// on entry is enough; at and above it a wait that is still recurring is no
// longer distinguishable, on the timeline alone, from one that will never
// clear -- for a blocker hold, from a dependency nobody intends to schedule.
//
// An abandonment cooldown is exempt, and is the one wait that is: it is not
// waiting on anything outside the coordinator, its end is a deadline the
// coordinator itself set, and the abandonment it follows already logged at
// error level. Warning about it would only repeat that record, on a schedule
// that says nothing new (PMR-191).
func (c *Coordinator) escalateStuckWaits(now time.Time, s config.Settings) {
	threshold := waitingEscalationFloor
	if window := waitingEscalationMultiplier * s.Polling.Interval; window > threshold {
		threshold = window
	}
	c.mu.Lock()
	var escalated []waitingState
	for _, st := range c.states {
		if st.waiting == nil || st.waitingEscalated || st.waiting.reason == waitReasonAbandonCooldown || now.Sub(st.waiting.since) < threshold {
			continue
		}
		st.waitingEscalated = true
		escalated = append(escalated, *st.waiting)
	}
	c.mu.Unlock()
	for _, entry := range escalated {
		c.logWaiting(c.log.Warn, entry.issue, entry.reason, entry.blockedBy, true)
	}
}

// ineligibleReason is the one eligibility predicate: it names the check that
// refused a candidate, and eligible is defined as its empty verdict.
// blocked_by_relation is split out from the generic not_routable so a Todo
// issue held by an open blocker (PMR-146) is distinguishable, at the poll log,
// from one rejected for an assignee mismatch or a missing required label.
//
// The assignee check is ordered ahead of the blocker check because
// dispatchable() in internal/linear/tracker.go decides Dispatchable in that
// same order: an assignee-policy mismatch fails an issue regardless of its
// blockers, so an issue carrying both must not be misreported as
// blocked_by_relation, which would name a resolvable blocker as the cause of
// something an operator resolving it would never fix.
//
// The check reads i.AssigneeMismatch rather than re-reading
// config.Tracker.Provider["assignee"] itself, because that config value can be
// "me" -- a policy dispatchable() only compares after resolving it to the
// acting viewer's ID over the network. Re-deriving the comparison here from
// the unresolved string would assert a mismatch whenever the policy is "me",
// regardless of the issue's actual assignee.
func ineligibleReason(i domain.Issue, s config.Settings) string {
	switch {
	case i.ID == "" || i.Identifier == "" || i.Title == "":
		return "missing_identity"
	case !active(i, s):
		return "not_active"
	case issueTerminal(i, s):
		return "terminal"
	case !i.Dispatchable && i.AssigneeMismatch:
		return "not_routable"
	case !i.Dispatchable && len(openBlockers(i)) > 0:
		return waitReasonBlockedByRelation
	case !routable(i, s):
		return "not_routable"
	default:
		return ""
	}
}

// openBlockers is the subset of the issue's blockers that are not yet
// resolved -- the ones actually responsible for a Dispatchable=false Todo
// issue -- so a poll rejection can name the blocker instead of only refusing
// the candidate.
func openBlockers(i domain.Issue) []domain.Blocker {
	var open []domain.Blocker
	for _, b := range i.BlockedBy {
		if !b.Dispatchable {
			open = append(open, b)
		}
	}
	return open
}

// blockerIdentifierList renders a safe, content-free value from a blocker
// list: tracker issue identifiers only, never titles or descriptions. Callers
// that need a single log attribute join it, mirroring the observability
// logger's attribute allowlist, which omits unrecognized non-scalar kinds.
func blockerIdentifierList(blockers []domain.Blocker) []string {
	identifiers := make([]string, 0, len(blockers))
	for _, b := range blockers {
		if b.Identifier != "" {
			identifiers = append(identifiers, b.Identifier)
		}
	}
	return identifiers
}

// claim reserves the orchestrator slot for the issue, or reports the single
// admission check that refused it -- the same categories the poll summary
// counts. Deciding and reserving under one acquisition of c.mu is the point:
// a separate read-only peek would leave a window in which capacity freed or
// filled between the categorization and the reservation, so a refusal here is
// never a stale reading of a state some other goroutine has since changed
// (PMR-194).
//
// The abandonment cooldown is tested ahead of capacity for the reason
// ineligibleReason orders its own checks: an issue serving one is refused
// however much capacity is free, so reporting it as at_capacity would name a
// cause an operator freeing a slot would never fix.
func (c *Coordinator) claim(i domain.Issue, s config.Settings) (bool, string) {
	c.mu.Lock()
	switch {
	case c.stopping:
		c.mu.Unlock()
		return false, "stopping"
	case c.claimedStateLocked(i.ID) != nil:
		c.mu.Unlock()
		return false, "already_claimed"
	case c.cooldownActiveLocked(i.ID, c.clock.Now()):
		c.mu.Unlock()
		return false, waitReasonAbandonCooldown
	case !c.capacityAvailableLocked(config.Norm(i.State), s):
		c.mu.Unlock()
		return false, waitReasonAtCapacity
	}
	st := c.ensureStateLocked(i.ID)
	st.claimed = true
	c.setClaimStateLocked(st, config.Norm(i.State))
	// A claimed issue is being actively worked; any prior handoff memory is
	// stale (the poll loop already reported an external revert before this).
	st.handoff = nil
	// An elapsed abandonment cooldown no sweep has reached yet goes the same way:
	// the gate above already decided this issue may start a fresh episode, so
	// leaving the record would only make "claimed" and "cooling down" look
	// simultaneously true (see checkInvariants).
	st.cooldown = nil
	// The waiting memory goes with it, for the same reason and one tick sooner
	// than updateWaiting would drop it: an issue this poll just admitted is no
	// longer waiting for capacity or a blocker. Leaving it to the end of the
	// tick would let a Snapshot taken while the launch goroutine is installing
	// the session report one issue as both Waiting and Running -- the overlap
	// Snapshot.Waiting promises never happens (PMR-189).
	st.waiting = nil
	st.waitingEscalated = false
	c.mu.Unlock()
	c.log.Debug("issue claimed", "issue_id", i.ID, "issue_identifier", i.Identifier, "state", config.Norm(i.State))
	return true, ""
}

func active(i domain.Issue, s config.Settings) bool {
	for _, x := range s.Tracker.ActiveStates {
		if config.Norm(i.State) == config.Norm(x) {
			return true
		}
	}
	return false
}

// issueTerminal reports whether the issue's tracker state is one of the
// configured terminal states -- unrelated to domain.EventKind.Terminal, which
// answers the same question for an agent session's event stream.
func issueTerminal(i domain.Issue, s config.Settings) bool {
	for _, x := range s.Tracker.TerminalStates {
		if config.Norm(i.State) == config.Norm(x) {
			return true
		}
	}
	return false
}

func routable(i domain.Issue, s config.Settings) bool {
	if !i.Dispatchable {
		return false
	}
	have := map[string]bool{}
	for _, x := range i.Labels {
		have[config.Norm(x)] = true
	}
	for _, x := range s.Tracker.RequiredLabels {
		if x == "" || !have[x] {
			return false
		}
	}
	return true
}

// eligible is ineligibleReason's own verdict, not a second spelling of it: the
// callers that only need the yes/no answer (retry, run, turn) must never be
// able to disagree with the reason the poll log reported (PMR-194).
func eligible(i domain.Issue, s config.Settings) bool {
	return ineligibleReason(i, s) == ""
}

func sortIssues(v []domain.Issue) {
	sort.SliceStable(v, func(i, j int) bool {
		a, b := v[i], v[j]
		ap, bp := priority(a), priority(b)
		if ap != bp {
			return ap < bp
		}
		if a.CreatedAt != nil && b.CreatedAt != nil && !a.CreatedAt.Equal(*b.CreatedAt) {
			return a.CreatedAt.Before(*b.CreatedAt)
		}
		if (a.CreatedAt == nil) != (b.CreatedAt == nil) {
			return a.CreatedAt != nil
		}
		return a.Identifier < b.Identifier
	})
}

func priority(i domain.Issue) int {
	if i.Priority != nil && *i.Priority >= 1 && *i.Priority <= 4 {
		return *i.Priority
	}
	return 5
}
