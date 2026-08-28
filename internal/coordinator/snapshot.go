package coordinator

import (
	"sort"
	"time"

	"github.com/pmrrasmussen/symphony/internal/domain"
)

// waitingState is the coordinator's memory of one issue sitting idle for
// waitReasonAtCapacity or waitReasonBlockedByRelation. It stores the issue
// itself, exactly as retryState does, only so Snapshot can report its current
// identifier and state. blockedBy carries the open blockers' identifiers
// (PMR-146's blockerIdentifiers, identifiers only, never titles or
// descriptions) and is set only when reason is waitReasonBlockedByRelation.
// since is the timestamp of the first poll this issue was seen under its
// current reason; a reason change is treated as a new wait.
type waitingState struct {
	issue     domain.Issue
	reason    string
	blockedBy []string
	since     time.Time
}

// Snapshot is a read-only, intentionally reduced view of coordinator state.
// It excludes issue bodies, prompts, workspace paths, raw events, and tracker
// identifiers that may be provider-specific.
type Snapshot struct {
	Claimed  int               `json:"claimed"`
	Running  []RunningSnapshot `json:"running"`
	Retrying []RetrySnapshot   `json:"retrying"`
	// Waiting lists an eligible issue that has reserved neither an
	// orchestrator slot nor (unlike Retrying) a retry timer: a candidate the
	// poll rejected only for capacity, re-checked fresh on every poll (PMR-139).
	// It never overlaps Running or Retrying -- a claimed issue is removed here
	// the moment it is claimed.
	Waiting  []WaitingSnapshot `json:"waiting"`
	Stopping bool              `json:"stopping"`
}

type RunningSnapshot struct {
	IssueIdentifier string       `json:"issue_identifier"`
	IssueState      string       `json:"issue_state"`
	SessionID       string       `json:"session_id"`
	ThreadID        string       `json:"thread_id"`
	TurnID          string       `json:"turn_id"`
	Attempt         int          `json:"attempt"`
	TurnCount       int          `json:"turn_count"`
	StartedAt       time.Time    `json:"started_at"`
	LastEventAt     time.Time    `json:"last_activity_at"`
	Usage           domain.Usage `json:"usage"`
	// IssueUsage is what the issue has spent across every attempt of the
	// dispatch episode this run belongs to, where Usage above is this run's own
	// figure. A retry starts a fresh domain.Run, so the per-run figure alone
	// leaves an issue on its thirty-eighth attempt looking as cheap as one on
	// its first (PMR-151); see issueState.usage for the episode it covers.
	IssueUsage           domain.Usage                  `json:"issue_usage"`
	RateLimit            map[string]int64              `json:"rate_limit,omitempty"`
	OutstandingOperation *OutstandingOperationSnapshot `json:"outstanding_operation,omitempty"`
}

type RetrySnapshot struct {
	IssueIdentifier string `json:"issue_identifier"`
	Attempt         int    `json:"attempt"`
	Kind            string `json:"kind"`
	Reason          string `json:"reason"`
	// WaitAttempt is the number of consecutive landing waits behind a
	// "landing" retry. It is the operator's "this landing is stuck" signal:
	// the agent attempt deliberately stays put for a non-failure, so a climbing
	// wait count (and the growing delay it drives) is what distinguishes a slow
	// check run from a gate that will never settle (PMR-78).
	WaitAttempt int       `json:"wait_attempt,omitempty"`
	Due         time.Time `json:"due_at"`
	// IssueUsage is what the issue has already spent across this episode's
	// finished attempts. A waiting retry holds no run at all, so this is the
	// only place its cost is visible -- and an issue queued for yet another
	// attempt is exactly when "is this worth continuing" is being asked
	// (PMR-151).
	IssueUsage domain.Usage `json:"issue_usage"`
}

// WaitingSnapshot is one issue sitting idle for waitReasonAtCapacity (eligible
// but not yet admitted) or waitReasonBlockedByRelation (held ineligible by an
// open blocker, PMR-146/PMR-152). Reason distinguishes the two; an issue is
// never reported under both, since dispatchable() decides them in a fixed,
// mutually exclusive order. BlockedBy carries only the open blockers'
// identifiers -- never titles or descriptions -- and is empty for
// waitReasonAtCapacity. WaitingMS is how long the issue has held its current
// reason, in milliseconds, computed at snapshot time rather than stored as a
// duration so JSON does not have to carry Go's duration encoding.
type WaitingSnapshot struct {
	IssueIdentifier string    `json:"issue_identifier"`
	IssueState      string    `json:"issue_state"`
	Reason          string    `json:"reason"`
	BlockedBy       []string  `json:"blocked_by,omitempty"`
	Since           time.Time `json:"since"`
	WaitingMS       int64     `json:"waiting_ms"`
}

// OutstandingOperationSnapshot identifies the one safe app-server operation
// that has started but not finished. It intentionally excludes arguments,
// command bodies, outputs, and the protocol item's opaque identifier.
type OutstandingOperationSnapshot struct {
	Type      string    `json:"type"`
	Name      string    `json:"name,omitempty"`
	StartedAt time.Time `json:"started_at"`
	AgeMS     int64     `json:"age_ms"`
}

// Snapshot copies the coordinator's public operational metadata while holding
// its mutex, so callers cannot observe or mutate its live scheduling maps.
func (c *Coordinator) Snapshot() Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.clock.Now()
	snapshot := Snapshot{Claimed: c.claimedCountLocked(), Running: make([]RunningSnapshot, 0, len(c.states)), Retrying: make([]RetrySnapshot, 0, len(c.states)), Waiting: make([]WaitingSnapshot, 0, len(c.states)), Stopping: c.stopping}
	for _, st := range c.states {
		if run := st.run; run != nil {
			item := snapshotOutstanding(run.outstanding, now)
			snapshot.Running = append(snapshot.Running, RunningSnapshot{IssueIdentifier: run.issue.Identifier, IssueState: run.issue.State, SessionID: run.session.ID, ThreadID: run.session.ThreadID, TurnID: run.session.TurnID, Attempt: run.run.Attempt, TurnCount: run.run.TurnCount, StartedAt: run.run.StartedAt, LastEventAt: run.last, Usage: run.run.Usage, IssueUsage: st.usage, RateLimit: copyRateLimit(run.rateLimit), OutstandingOperation: item})
		}
		if retry := st.retry; retry != nil {
			snapshot.Retrying = append(snapshot.Retrying, RetrySnapshot{IssueIdentifier: retry.issue.Identifier, Attempt: retry.attempt, Kind: string(retry.kind), Reason: retry.reason, WaitAttempt: st.landingWaits, Due: retry.due, IssueUsage: st.usage})
		}
		if wait := st.waiting; wait != nil {
			age := now.Sub(wait.since).Milliseconds()
			if age < 0 {
				age = 0
			}
			var blockedBy []string
			if len(wait.blockedBy) > 0 {
				blockedBy = append([]string(nil), wait.blockedBy...)
			}
			snapshot.Waiting = append(snapshot.Waiting, WaitingSnapshot{IssueIdentifier: wait.issue.Identifier, IssueState: wait.issue.State, Reason: wait.reason, BlockedBy: blockedBy, Since: wait.since, WaitingMS: age})
		}
	}
	sort.Slice(snapshot.Running, func(i, j int) bool { return snapshot.Running[i].IssueIdentifier < snapshot.Running[j].IssueIdentifier })
	sort.Slice(snapshot.Retrying, func(i, j int) bool {
		return snapshot.Retrying[i].IssueIdentifier < snapshot.Retrying[j].IssueIdentifier
	})
	sort.Slice(snapshot.Waiting, func(i, j int) bool {
		return snapshot.Waiting[i].IssueIdentifier < snapshot.Waiting[j].IssueIdentifier
	})
	return snapshot
}

func snapshotOutstanding(operation *outstandingOp, now time.Time) *OutstandingOperationSnapshot {
	if operation == nil {
		return nil
	}
	age := now.Sub(operation.Since).Milliseconds()
	if age < 0 {
		age = 0
	}
	return &OutstandingOperationSnapshot{Type: operation.ItemType, Name: operation.ToolName, StartedAt: operation.Since, AgeMS: age}
}
