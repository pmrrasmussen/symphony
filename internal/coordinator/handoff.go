package coordinator

import (
	"time"

	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/domain"
	"github.com/pmrrasmussen/symphony/internal/observability"
)

// handoffObservationFloor is the lower bound for how long the coordinator
// remembers that it drove an issue into the review handoff state. The effective
// retention is max(this, 2*poll interval) so a poll always runs while the
// memory is live and an external automation that reverts the handoff (the
// PMR-63 In Review -> In Progress flap) is observed and logged rather than
// silently re-dispatched with no trace. Healthy handoffs that stay in review
// are swept out after this window, so the coordinator's per-issue records
// never grow unbounded.
const handoffObservationFloor = 2 * time.Minute

// handoffObservation is the coordinator's memory of one host-driven transition
// into the review handoff state. It stores only the normalized state name and
// the time of the observation — never issue content — so the poll loop can
// attribute a later active-state re-appearance to an external actor.
type handoffObservation struct {
	state string
	at    time.Time
}

// noteHandoffObservation records that Symphony itself drove issue into the
// configured review handoff state, so a later external revert of that handoff
// is attributable at poll time. It is a no-op when no handoff state is
// configured or the issue is not in it.
func (c *Coordinator) noteHandoffObservation(issue domain.Issue, s config.Settings, now time.Time) {
	handoff := config.Norm(s.Tracker.HandoffState)
	if handoff == "" || issue.ID == "" || config.Norm(issue.State) != handoff {
		return
	}
	c.mu.Lock()
	c.ensureStateLocked(issue.ID).handoff = &handoffObservation{state: handoff, at: now}
	c.mu.Unlock()
}

// notePostHandoffStateChange logs, exactly once, when an active candidate is an
// issue Symphony recently handed off to the review state. Every such change is
// external — the review state is human-controlled and Symphony has no
// In Review -> active writer — but not every one is a fault: the human approval
// and rework decisions are the lifecycle working as designed and are logged as
// expected changes, while an unexpected reactivation stays a warning. It
// consumes the observation so a single change is reported once, and never
// mutates the tracker — re-asserting a reverted handoff is a documented
// follow-up.
func (c *Coordinator) notePostHandoffStateChange(i domain.Issue, s config.Settings, now time.Time) {
	if config.Norm(s.Tracker.HandoffState) == "" || i.ID == "" {
		return
	}
	c.mu.Lock()
	var observation handoffObservation
	st := c.stateLocked(i.ID)
	ok := st != nil && st.handoff != nil
	if ok {
		observation = *st.handoff
		st.handoff = nil
		c.pruneLocked(i.ID)
	}
	c.mu.Unlock()
	if !ok || config.Norm(i.State) == observation.state {
		return
	}
	operation := postHandoffOperation(config.Norm(i.State), s)
	attrs := []any{
		"operation", operation,
		"issue_id", i.ID,
		"issue_identifier", i.Identifier,
		"from_state", observation.state,
		"to_state", config.Norm(i.State),
		"since_handoff_ms", now.Sub(observation.at).Milliseconds(),
	}
	if operation == observability.OperationExternalReversion {
		c.log.Warn("external tracker state change observed", attrs...)
		return
	}
	c.log.Info("human review state change observed", attrs...)
}

// postHandoffOperation classifies one state change out of the review handoff
// state that Symphony did not perform. Moving the issue into the configured
// github.merge_state is the documented human approval to land, and moving it
// into the lifecycle's rework state is the documented human request for
// changes; both are expected. Everything else — including any destination
// Symphony cannot name from the configured lifecycle — contradicts the handoff
// by reactivating handed-off work as though implementation had not happened
// (the PMR-63 flap of the tracker's native PR-to-status automation) and stays
// an actionable warning. The warning is the default on purpose: a silent
// expected-change record for a state Symphony merely failed to recognize would
// hide exactly the fault this record exists to surface.
func postHandoffOperation(to string, s config.Settings) observability.Operation {
	switch {
	case to != "" && to == config.Norm(s.GitHub.MergeState):
		return observability.OperationReviewApproved
	case reworkDecision(to, s):
		return observability.OperationReworkRequested
	default:
		return observability.OperationExternalReversion
	}
}

// reworkDecision reports whether state is the lifecycle's human rework state.
// Symphony names that state by elimination against the configured lifecycle:
// tracker.provider.transitions.start enumerates the pre-review implementation
// states (the canonical Todo -> In Progress edge) and github.merge_state is the
// landing authorization, so removing both from active_states leaves the states
// only a human review decision moves an issue into.
//
// That naming is trusted only when exactly one state remains, which is the
// canonical lifecycle's Rework. With no start policy configured, or with two or
// more remaining candidates (an extra parked state such as Blocked, a Backlog
// in active_states, or a dispatch entry state that no start edge names),
// Symphony cannot tell the rework state from a state an external writer parked
// handed-off work in, so nothing qualifies and every such change keeps its
// warning. The merge state is excluded here too, so this predicate is correct
// on its own rather than relying on postHandoffOperation's case order.
func reworkDecision(state string, s config.Settings) bool {
	if state == "" || state == config.Norm(s.GitHub.MergeState) {
		return false
	}
	candidates := reworkCandidates(s)
	return len(candidates) == 1 && candidates[0] == state
}

// reworkCandidates returns the normalized active states that neither the host
// start policy nor the merge state accounts for. An empty start policy yields
// no candidates: without it Symphony cannot identify the pre-review
// implementation states, so it can name nothing by elimination.
func reworkCandidates(s config.Settings) []string {
	if len(s.Tracker.HostTransitions.Start) == 0 {
		return nil
	}
	accounted := map[string]bool{config.Norm(s.GitHub.MergeState): true}
	for source, target := range s.Tracker.HostTransitions.Start {
		accounted[config.Norm(source)] = true
		accounted[config.Norm(target)] = true
	}
	candidates := make([]string, 0, len(s.Tracker.ActiveStates))
	for _, state := range s.Tracker.ActiveStates {
		name := config.Norm(state)
		if name == "" || accounted[name] {
			continue
		}
		accounted[name] = true // Also de-duplicates a repeated active state.
		candidates = append(candidates, name)
	}
	return candidates
}

// sweepHandoffObservations discards handoff memories older than the retention
// window (max of the floor and two poll intervals, so a poll always runs while
// a memory is live). A healthy handoff that stays in review is never reverted
// and so is only ever cleared here, which is what keeps the per-issue records
// bounded.
func (c *Coordinator) sweepHandoffObservations(now time.Time, s config.Settings) {
	ttl := handoffObservationFloor
	if window := 2 * s.Polling.Interval; window > ttl {
		ttl = window
	}
	c.mu.Lock()
	for id, st := range c.states {
		if st.handoff != nil && now.Sub(st.handoff.at) > ttl {
			st.handoff = nil
			c.pruneLocked(id)
		}
	}
	c.mu.Unlock()
}
