package observability

// Operation is the bounded vocabulary for the `operation` field of a lifecycle
// log record. It is a closed set on purpose: every record that names a
// lifecycle decision — a tracker edge Symphony performed, one it merely
// observed, or a scheduling decision an operator has to be able to find — uses
// a constant from this file, so an operator (or a log query) can rely on the
// field's values instead of matching free-form strings that drift per call
// site. Values are lowercase snake_case and are treated as a stable log
// contract: rename one only alongside the documentation in
// docs/observability.md.
//
// Every value is a fixed literal here, never derived from tracker content, so
// an Operation is always redaction-safe.
type Operation string

const (
	// OperationStartTransition is the coordinator's dispatch-time start edge
	// (the canonical Todo -> In Progress move) applied before a session starts.
	OperationStartTransition Operation = "start_transition"
	// OperationTransition is a host-side tracker transition applied through the
	// generic tracker adapter rather than a named delivery edge.
	OperationTransition Operation = "transition"
	// OperationHandoff is the host review handoff into the handoff state.
	OperationHandoff Operation = "handoff"
	// OperationLandingRefused is the landing fallback applied when a
	// github_land_pr attempt hits a hard gate (merge state -> handoff state).
	OperationLandingRefused Operation = "landing_refused"
	// OperationLandingCompleted is the terminal edge after a successful merge.
	OperationLandingCompleted Operation = "landing_completed"
	// OperationMergeReconciled is the terminal edge for a pull request GitHub
	// already reports merged.
	OperationMergeReconciled Operation = "merge_reconciled"
	// OperationReviewCompleted is the terminal edge for review work GitHub
	// reports as already delivered.
	OperationReviewCompleted Operation = "review_completed"
	// OperationDispatchAbandoned is the one operation here that names no
	// tracker edge: it reports that the coordinator hit agent.max_attempts for
	// one issue and gave up on that dispatch episode rather than retrying
	// forever (PMR-111). It deliberately leaves the tracker state alone, so
	// "the issue was abandoned, and the board still says otherwise" is exactly
	// what an operator has to be able to query for.
	OperationDispatchAbandoned Operation = "dispatch_abandoned"

	// The three operations below describe state changes Symphony did NOT
	// perform: the handoff state is human-controlled, so every change out of it
	// comes from outside Symphony. Only some of those are a fault.

	// OperationReviewApproved is the expected human approval edge out of the
	// review handoff state into the configured merge state: moving the issue
	// there is itself the authorization to land, so it is normal operation.
	OperationReviewApproved Operation = "review_approved"
	// OperationReworkRequested is the expected human review decision that sends
	// the work back for changes: the handoff state -> the lifecycle's rework
	// state.
	OperationReworkRequested Operation = "rework_requested"
	// OperationExternalReversion is an unexpected, actionable reversion of a
	// handoff: an external writer (typically the tracker's native GitHub
	// PR-to-status automation, PMR-63) reactivated handed-off work by moving the
	// issue back into a pre-review implementation state. It is the only one of
	// the three logged at warn level.
	OperationExternalReversion Operation = "external_reversion"
)

// known is the closed set above, checkable at log time. safeAttr writes an
// Operation only when it is one of these literals: the "bounded vocabulary"
// claim is then enforced by the redaction boundary itself rather than asserted
// by comment, so a value converted from arbitrary text (which would otherwise
// bypass Text's scrubbing and truncation) can never reach a log record.
var known = map[Operation]bool{
	OperationStartTransition:   true,
	OperationTransition:        true,
	OperationHandoff:           true,
	OperationLandingRefused:    true,
	OperationLandingCompleted:  true,
	OperationMergeReconciled:   true,
	OperationReviewCompleted:   true,
	OperationDispatchAbandoned: true,
	OperationReviewApproved:    true,
	OperationReworkRequested:   true,
	OperationExternalReversion: true,
}
