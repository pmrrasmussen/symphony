package coordinator

import (
	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/domain"
)

// issueState is everything the coordinator remembers about one issue, in one
// record with one owner. It replaces the ten parallel maps that preceded it
// (PMR-123): claimed, claimState, admitted, running, retries, handoffs,
// landingWaits, landingEscalated, waiting, and waitingEscalated were each
// keyed by issue ID, and every relation between them -- an admitted issue
// holds a claim, a landing wait is meaningful only while claimed, a claim
// clears the handoff memory -- was enforced by hand at each call site. Here
// those relations are properties of one value, so clearing an issue's
// scheduling state is one operation and cannot half-happen.
//
// Two groups of fields have deliberately different lifetimes:
//
//   - The claim group (claimed, state, reservation, run, retry, landingWaits,
//     landingEscalated, usage) is created by claim and dropped as a unit by
//     releaseLocked. Nothing in it outlives the claim.
//   - handoff, waiting, and waitingEscalated describe an issue Symphony is
//     *not* currently working: the memory that it drove the issue into the
//     review handoff state (which must survive the release that immediately
//     follows the handoff), and the memory that a candidate is sitting idle
//     for capacity or an open blocker without holding a claim at all.
//
// A record exists only while it remembers something; see stateLive.
type issueState struct {
	// claimed marks the issue as owned by this process: no second dispatch of
	// it can start while it holds true, whether or not a slot is reserved.
	claimed bool
	// state is the issue's normalized tracker state as of the claim, the
	// reservation, or the last reconciliation refresh. It is what the per-state
	// capacity limit counts, so it is meaningful only while claimed.
	state string
	// reservation is nonzero exactly while the issue holds an orchestrator
	// slot, and identifies which launch owns that slot. Unlike a claim it
	// deliberately excludes delayed retry timers: a timer still owns its issue
	// to prevent duplicate dispatch, but it must not idle a worker slot while
	// it waits.
	//
	// The value is the ownership rule PMR-121 needed. launch stamps its
	// goroutine with the generation reserveLocked minted, and unreserve
	// releases the slot only when the record still holds that exact
	// generation. A late release from an outgoing goroutine therefore cannot
	// free the slot a redispatch of the same issue has already taken -- the
	// over-delete that let the coordinator run one worker over
	// agent.max_concurrent_agents.
	reservation uint64
	// run is the executing session, nil unless the issue is executing.
	run *running
	// retry is the armed delayed redispatch, nil unless a timer is pending. An
	// issue is either executing or waiting to be redispatched, never both.
	retry *retryState
	// landingWaits counts consecutive non-terminal landing waits. It escalates
	// the delayed landing redispatch so a gate that never settles (a genuinely
	// long check run, or a required_checks name that does not match any GitHub
	// job) backs off toward agent.max_retry_backoff_ms instead of respawning a
	// session at the GitHub poll cadence forever. It is cleared with the claim,
	// so any other landing outcome resets it (PMR-78).
	landingWaits int
	// landingEscalated records whether the "landing wait retry scheduled" log
	// has already been raised to Warn once landingWaits crossed the point where
	// landingRetryDelay's backoff saturates at agent.max_retry_backoff_ms (see
	// landingWaitEscalated). It keeps that escalation a one-time signal --
	// naming a stuck landing once it stops being distinguishable from a slow
	// one -- rather than a Warn on every subsequent poll-cadence wait. Cleared
	// with the claim alongside landingWaits (PMR-116).
	landingEscalated bool
	// systemicFailures counts the issue's consecutive failures whose reason was
	// systemic (see systemicFailureReasons). Those failures deliberately hold
	// the attempt counter fixed, so this is the only counter left that climbs
	// during a sustained outage: it keys the retry backoff in their place and is
	// the operator-visible measure of how long the outage has been repeating. A
	// genuine, issue-attributable failure resets it -- it is a streak, not a
	// total -- and it is cleared with the claim alongside landingWaits
	// (PMR-179).
	systemicFailures int
	// usage is every token this issue has spent across the attempts of the
	// dispatch episode it is currently claimed under, including the attempt in
	// flight. Both figures are kept because both are questions an operator
	// asks: the per-run domain.Run.Usage answers "was that turn expensive",
	// this answers "is this issue worth continuing" -- which attempt count
	// alone cannot, since a cheap issue on attempt 30 and an expensive one on
	// attempt 5 are different situations (PMR-151).
	//
	// It is deliberately part of the claim group rather than a survivor like
	// handoff, because the claim *is* the episode: retries hold it across every
	// attempt (scheduleRetry requires a claim), and domain.Run.Attempt -- the
	// counter this total is the cost behind -- likewise restarts with the next
	// claim. Outliving the release would mean either keeping a record for every
	// issue ever dispatched, which pruneLocked exists to prevent, or a second
	// lifetime rule for a total whose own episode the terminal summary has
	// already recorded in the log.
	usage domain.Usage
	// handoff records that Symphony itself drove this issue into the configured
	// review handoff state, so the poll loop can recognize and log an external
	// actor (for example Linear's native GitHub PR automation) reverting that
	// handoff to an active state instead of silently re-dispatching it. It is
	// set as the run ends and must therefore outlive the release that follows,
	// which is why releaseLocked leaves it alone. It is in-process only and
	// discarded safely on restart.
	handoff *handoffObservation
	// waiting records a Todo issue that is sitting idle for a reason that earns
	// neither a claim nor a retry timer, so no other tracking remembers it
	// (PMR-139, PMR-152): a candidate rejected only for capacity, or one held
	// ineligible by an open blocker relation, is re-evaluated fresh from
	// ListCandidates on the next poll, with nothing else tracking it in the
	// interim. It is cleared the moment the issue is no longer seen in either
	// state -- admitted, unblocked, turned ineligible for some other reason, or
	// dropped from the tracker's candidate list.
	waiting *waitingState
	// waitingEscalated marks that the "still waiting" Warn has already fired
	// once, mirroring landingEscalated: an issue that stays stuck keeps logging
	// Info only on the poll it first entered (or changed reason for) the
	// waiting set, not a Warn every poll.
	waitingEscalated bool
}

// stateLive reports whether a record still remembers anything. The claim group
// is covered by claimed alone -- reservation, retry, landingWaits,
// landingEscalated, systemicFailures, and usage exist only under a claim, and
// run is checked separately only because it is dropped a moment before the
// claim it belongs to.
func (s *issueState) stateLive() bool {
	return s.claimed || s.run != nil || s.handoff != nil || s.waiting != nil
}

// stateLocked returns the issue's record, or nil when the coordinator
// remembers nothing about it. Callers must hold c.mu.
func (c *Coordinator) stateLocked(id string) *issueState { return c.states[id] }

// claimedStateLocked returns the issue's record only while it holds a claim,
// which is what every "does this issue already belong to us" check wants.
// Callers must hold c.mu.
func (c *Coordinator) claimedStateLocked(id string) *issueState {
	if st := c.states[id]; st != nil && st.claimed {
		return st
	}
	return nil
}

// ensureStateLocked returns the issue's record, creating an empty one if the
// coordinator remembers nothing about it yet. Every caller must go on to store
// something the record's liveness depends on, or call pruneLocked, so an
// empty record is never left behind. Callers must hold c.mu.
func (c *Coordinator) ensureStateLocked(id string) *issueState {
	st := c.states[id]
	if st == nil {
		st = &issueState{}
		c.states[id] = st
	}
	return st
}

// pruneLocked drops a record that no longer remembers anything, so the map
// tracks live issues rather than growing one entry per issue ever seen.
// Callers must hold c.mu.
func (c *Coordinator) pruneLocked(id string) {
	if st := c.states[id]; st != nil && !st.stateLive() {
		delete(c.states, id)
	}
}

// runLocked returns the issue's executing session, or nil when it is not
// executing. Callers must hold c.mu.
func (c *Coordinator) runLocked(id string) *running {
	if st := c.states[id]; st != nil {
		return st.run
	}
	return nil
}

// landingWaitsFor reports the issue's consecutive landing-wait count, which is
// zero unless it currently holds a claim to have waited under.
func (c *Coordinator) landingWaitsFor(id string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if st := c.claimedStateLocked(id); st != nil {
		return st.landingWaits
	}
	return 0
}

// recordSystemicFailure folds one classified retryAgent failure into the
// issue's consecutive-systemic-failure streak and returns that streak, which is
// zero for a genuine failure: a failure the issue itself is answerable for ends
// the streak, because whatever host or backend boundary preceded it has plainly
// stopped stopping the work. Like the landing-wait accounting the streak
// belongs to the claim it was observed under, so an issue that no longer holds
// one records nothing -- and scheduleRetry declines its redispatch anyway.
func (c *Coordinator) recordSystemicFailure(id string, systemic bool) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.claimedStateLocked(id)
	if st == nil {
		return 0
	}
	if !systemic {
		st.systemicFailures = 0
		return 0
	}
	st.systemicFailures++
	return st.systemicFailures
}

// addIssueUsageLocked folds one run's change in recorded usage into its
// issue's cumulative per-episode total and returns that total. Like the
// landing-wait accounting, the spend is counted against the claim it happened
// under: an issue that no longer holds one has no episode left to attribute it
// to, and by construction (see checkInvariants: running ⊆ claimed) a live
// session always still holds its claim. Callers must hold c.mu.
func (c *Coordinator) addIssueUsageLocked(id string, spent domain.Usage) domain.Usage {
	st := c.claimedStateLocked(id)
	if st == nil {
		return domain.Usage{}
	}
	st.usage.InputTokens += spent.InputTokens
	st.usage.OutputTokens += spent.OutputTokens
	st.usage.TotalTokens += spent.TotalTokens
	return st.usage
}

// issueUsageLocked reports the issue's cumulative per-episode usage, which is
// zero unless it currently holds a claim to have spent it under. Callers must
// hold c.mu.
func (c *Coordinator) issueUsageLocked(id string) domain.Usage {
	if st := c.claimedStateLocked(id); st != nil {
		return st.usage
	}
	return domain.Usage{}
}

// claimedCountLocked is the number of issues this process owns, reserved or
// not: Snapshot's Claimed. Callers must hold c.mu.
func (c *Coordinator) claimedCountLocked() int {
	count := 0
	for _, st := range c.states {
		if st.claimed {
			count++
		}
	}
	return count
}

// reserveLocked takes an orchestrator slot for an already-claimed issue and
// returns the generation identifying that reservation, or 0 when no slot was
// taken. The generation is the caller's proof of ownership: it must hand the
// same value back to unreserve, which is how a release is bound to the
// reservation it belongs to rather than to whatever occupies the slot at the
// time (PMR-121). Callers must hold c.mu.
func (c *Coordinator) reserveLocked(i domain.Issue, s config.Settings) uint64 {
	st := c.claimedStateLocked(i.ID)
	if st == nil || st.reservation != 0 || !c.capacityAvailableLocked(config.Norm(i.State), s) {
		return 0
	}
	c.setClaimStateLocked(st, config.Norm(i.State))
	c.nextReservation++
	st.reservation = c.nextReservation
	c.admittedCount++
	c.admittedByState[st.state]++
	return st.reservation
}

// unreserve releases the orchestrator slot held under reservation, and does
// nothing at all if the issue no longer holds exactly that reservation --
// because it was released, or because a redispatch has since taken a fresh
// one. That check is the whole point: launch calls this from several exits
// plus a deferred backstop, and without it the backstop would delete whichever
// reservation happened to occupy the slot, including one a retry had just
// taken for the same issue (PMR-121).
func (c *Coordinator) unreserve(id string, reservation uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.states[id]
	if reservation == 0 || st == nil || st.reservation != reservation {
		return
	}
	c.clearReservationLocked(st)
}

// clearReservationLocked frees the slot the record holds and keeps the
// admitted counters in step with it. Callers must hold c.mu.
func (c *Coordinator) clearReservationLocked(st *issueState) {
	if st.reservation == 0 {
		return
	}
	st.reservation = 0
	c.admittedCount--
	c.decrementStateCountLocked(st.state)
}

// setClaimStateLocked records the issue's current normalized tracker state,
// moving its admitted-per-state tally with it so the per-state limit cannot
// drift from the records it is computed over. Callers must hold c.mu.
func (c *Coordinator) setClaimStateLocked(st *issueState, state string) {
	if st.state == state {
		return
	}
	if st.reservation != 0 {
		c.decrementStateCountLocked(st.state)
		c.admittedByState[state]++
	}
	st.state = state
}

func (c *Coordinator) decrementStateCountLocked(state string) {
	c.admittedByState[state]--
	if c.admittedByState[state] <= 0 {
		delete(c.admittedByState, state)
	}
}

// capacityAvailableLocked reports whether one more issue in state may be
// admitted. Both bounds are O(1) reads of counters maintained alongside the
// records (PMR-123): the per-state limit used to be a linear scan of the
// admitted map, which recomputed from a structure that was not indexed by
// state. Callers must hold c.mu.
func (c *Coordinator) capacityAvailableLocked(state string, s config.Settings) bool {
	if c.admittedCount >= s.Agent.MaxConcurrent {
		return false
	}
	limit, ok := s.Agent.ByState[state]
	if !ok {
		return true
	}
	return c.admittedByState[state] < limit
}

// release drops an issue's entire claim in one operation: the reservation, any
// armed retry timer, the landing-wait accounting, the episode's accumulated
// usage, and the claim itself. The handoff and waiting memories are
// deliberately untouched -- they describe an issue Symphony is not working, and
// the handoff one is recorded immediately before the release that ends the run
// it belongs to.
func (c *Coordinator) release(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.releaseLocked(id)
}

// releaseLocked is release for a caller that already holds c.mu.
func (c *Coordinator) releaseLocked(id string) {
	st := c.states[id]
	if st == nil {
		return
	}
	if st.retry != nil && st.retry.timer != nil {
		st.retry.timer.Stop()
	}
	st.retry = nil
	c.clearReservationLocked(st)
	st.claimed = false
	st.state = ""
	st.landingWaits = 0
	st.landingEscalated = false
	st.systemicFailures = 0
	// The accumulated cost goes with the episode that spent it: a later
	// dispatch of the same issue is a new episode, starting from attempt one
	// and from zero tokens.
	st.usage = domain.Usage{}
	c.pruneLocked(id)
}
