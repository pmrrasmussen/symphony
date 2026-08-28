package coordinator

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/domain"
)

// waitReasonAtCapacity and waitReasonBlockedByRelation are the two disjoint
// causes waitingState.reason can hold, mirroring the identically named
// ineligibleReason/admissionRejectReason values that produce them: an issue
// is either eligible and short only of an orchestrator slot, or held
// ineligible by an open blocker relation (PMR-146). dispatchable() decides
// Dispatchable before capacity is ever consulted, so the two are mutually
// exclusive and an issue is never reported under both.
const (
	waitReasonAtCapacity        = "at_capacity"
	waitReasonBlockedByRelation = "blocked_by_relation"
)

// waitingCandidate is one poll's observation of an issue sitting idle for
// waitReasonAtCapacity or waitReasonBlockedByRelation, collected by tick and
// handed to updateWaiting to reconcile against the coordinator's own memory.
// blockedBy is set only when reason is waitReasonBlockedByRelation.
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
		if reason := c.admissionRejectReason(i, s); reason != "" {
			summary.rejected[reason]++
			c.log.Debug("poll candidate rejected", "issue_identifier", i.Identifier, "reason", reason)
			if reason == waitReasonAtCapacity {
				waiting[i.ID] = waitingCandidate{issue: i, reason: waitReasonAtCapacity}
			}
			continue
		}
		if !c.claim(i, s) {
			// A concurrent reconciliation or retry changed capacity between the
			// check above and this claim; still a rejection, just a narrower one.
			summary.rejected["claim_raced"]++
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
// under waitReasonAtCapacity or waitReasonBlockedByRelation against the
// previous poll's memory, and reports every genuinely new entry -- or one
// whose reason just changed -- once, by logging its identifier, state, and
// (for a blocker hold) the blocker itself: the things pollSummary is not
// allowed to carry. An issue absent from seen this poll is no longer waiting
// for whatever reason (admitted, unblocked, turned ineligible for some other
// reason, or dropped from the tracker's candidate list) and its memory is
// dropped here, so the waiting set can only ever describe issues this exact
// poll actually re-observed.
func (c *Coordinator) updateWaiting(seen map[string]waitingCandidate, now time.Time, s config.Settings) {
	c.mu.Lock()
	for id := range c.waiting {
		if _, ok := seen[id]; !ok {
			delete(c.waiting, id)
			delete(c.waitingEscalated, id)
		}
	}
	var newlyWaiting []waitingCandidate
	for id, candidate := range seen {
		entry, already := c.waiting[id]
		if !already || entry.reason != candidate.reason {
			entry = waitingState{issue: candidate.issue, reason: candidate.reason, blockedBy: candidate.blockedBy, since: now}
			delete(c.waitingEscalated, id)
			newlyWaiting = append(newlyWaiting, candidate)
		} else {
			entry.issue = candidate.issue
			entry.blockedBy = candidate.blockedBy
		}
		c.waiting[id] = entry
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
	message := "issue eligible but waiting for capacity"
	if stuck {
		message = "issue still waiting for capacity"
	}
	if reason == waitReasonBlockedByRelation {
		attrs = append(attrs, "blocked_by", strings.Join(blockedBy, ","))
		message = "issue blocked by an open dependency"
		if stuck {
			message = "issue still blocked by an open dependency"
		}
	}
	level(message, attrs...)
}

// escalateStuckWaits raises a one-time Warn for any issue that has sat in the
// waiting set past waitingEscalationFloor's effective threshold, exactly the
// way finishLandingWait escalates a landing wait past
// landingWaitEscalated. Below the threshold the Info logged by updateWaiting
// on entry is enough; at and above it a wait that is still recurring is no
// longer distinguishable, on the timeline alone, from one that will never
// clear -- for a blocker hold, from a dependency nobody intends to schedule.
func (c *Coordinator) escalateStuckWaits(now time.Time, s config.Settings) {
	threshold := waitingEscalationFloor
	if window := waitingEscalationMultiplier * s.Polling.Interval; window > threshold {
		threshold = window
	}
	c.mu.Lock()
	var escalated []waitingState
	for id, entry := range c.waiting {
		if c.waitingEscalated[id] || now.Sub(entry.since) < threshold {
			continue
		}
		c.waitingEscalated[id] = true
		escalated = append(escalated, entry)
	}
	c.mu.Unlock()
	for _, entry := range escalated {
		c.logWaiting(c.log.Warn, entry.issue, entry.reason, entry.blockedBy, true)
	}
}

// ineligibleReason mirrors eligible's own checks so a rejected candidate's
// debug record explains exactly which one failed. blocked_by_relation is
// split out from the generic not_routable so a Todo issue held by an open
// blocker (PMR-146) is distinguishable, at the poll log, from one rejected
// for an assignee mismatch or a missing required label.
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

// admissionRejectReason peeks at claim's own admission checks purely to
// categorize a poll rejection; it does not itself claim or mutate state.
func (c *Coordinator) admissionRejectReason(i domain.Issue, s config.Settings) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case c.stopping:
		return "stopping"
	case c.claimed[i.ID]:
		return "already_claimed"
	case !c.capacityAvailableLocked(config.Norm(i.State), s):
		return waitReasonAtCapacity
	default:
		return ""
	}
}

func (c *Coordinator) claim(i domain.Issue, s config.Settings) bool {
	c.mu.Lock()
	if c.stopping || c.claimed[i.ID] || !c.capacityAvailableLocked(config.Norm(i.State), s) {
		c.mu.Unlock()
		return false
	}
	c.claimed[i.ID] = true
	c.claimState[i.ID] = config.Norm(i.State)
	// A claimed issue is being actively worked; any prior handoff memory is
	// stale (the poll loop already reported an external revert before this).
	delete(c.handoffs, i.ID)
	c.mu.Unlock()
	c.log.Debug("issue claimed", "issue_id", i.ID, "issue_identifier", i.Identifier, "state", config.Norm(i.State))
	return true
}

func (c *Coordinator) reserveLocked(i domain.Issue, s config.Settings) bool {
	if _, admitted := c.admitted[i.ID]; !c.claimed[i.ID] || admitted || !c.capacityAvailableLocked(config.Norm(i.State), s) {
		return false
	}
	state := config.Norm(i.State)
	c.admitted[i.ID] = state
	c.claimState[i.ID] = state
	return true
}

func (c *Coordinator) capacityAvailableLocked(state string, s config.Settings) bool {
	if len(c.admitted) >= s.Agent.MaxConcurrent {
		return false
	}
	limit, ok := s.Agent.ByState[state]
	if !ok {
		return true
	}
	count := 0
	for _, admittedState := range c.admitted {
		if admittedState == state {
			count++
		}
	}
	return count < limit
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

func eligible(i domain.Issue, s config.Settings) bool {
	return i.ID != "" && i.Identifier != "" && i.Title != "" && active(i, s) && !issueTerminal(i, s) && routable(i, s)
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
