package coordinator

import (
	"fmt"
	"testing"

	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/domain"
)

// checkInvariants asserts the relations the ten parallel per-issue maps used
// to leave to each call site, and that the one owned record now makes
// structural (PMR-123). It is deliberately test-only: the point of the record
// is that production code cannot express a violation, so a production check
// would only be dead weight -- but the relations are still worth stating once,
// in a form the suite can hold every state transition to.
//
// It must hold at every moment c.mu is unlocked, which is why the assertion
// helper is safe to run against a coordinator whose worker goroutines are
// still live.
func (c *Coordinator) checkInvariants() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	admitted := 0
	byState := map[string]int{}
	for id, st := range c.states {
		switch {
		// A record exists only while it remembers something: the map tracks
		// live issues rather than one entry per issue ever seen.
		case !st.stateLive():
			return fmt.Errorf("%s: empty record was not pruned", id)
		// admitted ⊆ claimed, running ⊆ claimed, retrying ⊆ claimed: an
		// orchestrator slot, a session, and an armed timer all belong to a
		// claim, and none may outlive it.
		case st.reservation != 0 && !st.claimed:
			return fmt.Errorf("%s: holds reservation %d without a claim", id, st.reservation)
		case st.run != nil && !st.claimed:
			return fmt.Errorf("%s: is running without a claim", id)
		// running ⊆ admitted: a live session occupies the slot its launch
		// reserved for it, for as long as the session lasts. This is the
		// relation PMR-121's positional release broke -- a run left holding no
		// reservation is a worker the capacity check cannot see.
		case st.run != nil && st.reservation == 0:
			return fmt.Errorf("%s: is running without a reservation", id)
		case st.retry != nil && !st.claimed:
			return fmt.Errorf("%s: has an armed retry without a claim", id)
		// running ∩ retries = ∅: an issue is either executing or waiting to be
		// redispatched, never both.
		case st.run != nil && st.retry != nil:
			return fmt.Errorf("%s: is running and retrying at once", id)
		// The claim's state is what the per-state capacity limit counts, so a
		// claim must always name one and a released record must not.
		case st.claimed && st.state == "":
			return fmt.Errorf("%s: is claimed with no state", id)
		case !st.claimed && st.state != "":
			return fmt.Errorf("%s: kept state %q without a claim", id, st.state)
		// The landing-wait accounting is meaningful only under the claim it
		// was counted against, and its escalation only under a wait.
		case !st.claimed && (st.landingWaits != 0 || st.landingEscalated):
			return fmt.Errorf("%s: kept landing-wait accounting without a claim", id)
		case st.landingEscalated && st.landingWaits == 0:
			return fmt.Errorf("%s: escalated a landing wait it never had", id)
		// The systemic-failure streak is counted against its claim the same way,
		// and it keys the retry delay: a released record still carrying one would
		// start the next episode's first outage part-way up the ladder (PMR-179).
		case !st.claimed && st.systemicFailures != 0:
			return fmt.Errorf("%s: kept a systemic-failure streak without a claim", id)
		// The accumulated per-issue usage is meaningful only under the claim
		// whose attempts spent it: the claim is the episode, so a released
		// record still carrying a total would attribute one episode's cost to
		// the next one to claim the issue (PMR-151).
		case !st.claimed && st.usage != (domain.Usage{}):
			return fmt.Errorf("%s: kept accumulated usage %+v without a claim", id, st.usage)
		// The total only ever accrues, so no component may go negative: an
		// authoritative correction subtracts an overshoot that was added to
		// this same total first (see usageSpent).
		case st.usage.InputTokens < 0 || st.usage.OutputTokens < 0 || st.usage.TotalTokens < 0:
			return fmt.Errorf("%s: accumulated negative usage %+v", id, st.usage)
		// The waiting escalation likewise only under a wait.
		case st.waitingEscalated && st.waiting == nil:
			return fmt.Errorf("%s: escalated a wait it is not in", id)
		// An abandonment cooldown and a claim are mutually exclusive: the cooldown
		// is recorded as the abandoned episode's claim is dropped, and claim clears
		// an elapsed one on its way in, so a record holding both would mean an
		// issue was re-admitted during its own cooldown (PMR-191).
		case st.claimed && st.cooldown != nil:
			return fmt.Errorf("%s: is claimed while cooling down after an abandoned dispatch", id)
		}
		if st.reservation != 0 {
			admitted++
			byState[st.state]++
		}
	}
	// The capacity counters are derived state, so they must agree exactly with
	// the records they are derived from -- including carrying no zero entries,
	// which would leak one map entry per state ever admitted.
	if admitted != c.admittedCount {
		return fmt.Errorf("admittedCount is %d but %d records hold a reservation", c.admittedCount, admitted)
	}
	if len(byState) != len(c.admittedByState) {
		return fmt.Errorf("admittedByState is %v but the records give %v", c.admittedByState, byState)
	}
	for state, count := range byState {
		if c.admittedByState[state] != count {
			return fmt.Errorf("admittedByState[%q] is %d but %d records hold a reservation in it", state, c.admittedByState[state], count)
		}
	}
	return nil
}

// assertInvariants fails the test if the coordinator's per-issue state
// violates any relation checkInvariants describes.
func assertInvariants(t *testing.T, c *Coordinator) {
	t.Helper()
	if err := c.checkInvariants(); err != nil {
		t.Fatalf("coordinator state invariant violated: %v", err)
	}
}

// The rest of this file is the small read-only surface the tests use to look
// at one issue's record, so an assertion about a claim, a reservation, or an
// armed retry does not have to restate how the record is laid out.

// claimHeld reports whether the coordinator holds a claim on id.
func (c *Coordinator) claimHeld(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.claimedStateLocked(id) != nil
}

// admittedTotal is the number of issues holding an orchestrator slot.
func (c *Coordinator) admittedTotal() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.admittedCount
}

// runningCount is the number of issues with a live session.
func (c *Coordinator) runningCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	count := 0
	for _, st := range c.states {
		if st.run != nil {
			count++
		}
	}
	return count
}

// armedRetry returns a copy of the issue's armed retry, if it has one.
func (c *Coordinator) armedRetry(id string) (retryState, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if st := c.states[id]; st != nil && st.retry != nil {
		return *st.retry, true
	}
	return retryState{}, false
}

// handoffMemory returns a copy of the issue's handoff observation, if it has one.
func (c *Coordinator) handoffMemory(id string) (handoffObservation, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if st := c.states[id]; st != nil && st.handoff != nil {
		return *st.handoff, true
	}
	return handoffObservation{}, false
}

// abandonCooldownMemory returns a copy of the issue's abandonment cooldown, if
// it still has one.
func (c *Coordinator) abandonCooldownMemory(id string) (abandonCooldown, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if st := c.states[id]; st != nil && st.cooldown != nil {
		return *st.cooldown, true
	}
	return abandonCooldown{}, false
}

// landingWaitRecords is the number of issues carrying landing-wait
// accounting, so a test can assert the accounting is dropped with the claim
// rather than leaking one entry per finished landing.
func (c *Coordinator) landingWaitRecords() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	count := 0
	for _, st := range c.states {
		if st.landingWaits > 0 {
			count++
		}
	}
	return count
}

// issueUsage is the issue's accumulated per-episode usage, so a test can
// assert the total across attempts directly rather than only through whichever
// surface happens to render it.
func (c *Coordinator) issueUsage(id string) domain.Usage {
	c.mu.Lock()
	defer c.mu.Unlock()
	if st := c.states[id]; st != nil {
		return st.usage
	}
	return domain.Usage{}
}

// admittedState is the normalized tracker state an issue's orchestrator slot
// is counted under, or "" when it holds none.
func (c *Coordinator) admittedState(id string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if st := c.states[id]; st != nil && st.reservation != 0 {
		return st.state
	}
	return ""
}

// admittedInState is the number of orchestrator slots held by issues in the
// given normalized state -- the figure the per-state limit bounds.
func (c *Coordinator) admittedInState(state string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.admittedByState[state]
}

// seedClaim fakes a claim so a test can exercise a path that requires one
// without dispatching the issue through the poll loop.
func (c *Coordinator) seedClaim(i domain.Issue) {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.ensureStateLocked(i.ID)
	st.claimed = true
	c.setClaimStateLocked(st, config.Norm(i.State))
}

// seedRunning fakes a claimed, admitted issue with a live session, so a
// reconciliation or snapshot test can drive the run loop without starting a
// backend. It takes the slot too, because a live session always holds one.
func (c *Coordinator) seedRunning(i domain.Issue, r *running) {
	c.occupySlot(i)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ensureStateLocked(i.ID).run = r
}

// occupySlot fakes a claimed, admitted issue so a capacity test can put the
// coordinator at its limit without running one. releaseSlot undoes it.
func (c *Coordinator) occupySlot(i domain.Issue) {
	c.seedClaim(i)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.reserveLocked(i, config.Settings{Agent: config.Agent{MaxConcurrent: len(c.states) + 1}}) == 0 {
		panic("occupySlot could not reserve a slot for " + i.ID)
	}
}

func (c *Coordinator) releaseSlot(id string) { c.release(id) }
