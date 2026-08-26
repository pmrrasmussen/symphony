// Package domain contains tracker-neutral types and the replaceable execution boundaries.
package domain

import (
	"context"
	"time"
)

type Blocker struct {
	ID, Identifier, State string
	// StateType is Linear's workflow-state type for State (for example
	// "completed", "canceled", or "duplicate"). Dispatchability is decided by
	// this classification rather than by State's display name, so a resolved
	// status a workflow's terminal_states does not happen to name still
	// satisfies the blocker.
	StateType string
	// Dispatchable reports whether this blocker is itself resolved, i.e. no
	// longer an open dependency of the issue it blocks. It lets a caller
	// identify which specific blocker is holding an otherwise-eligible issue
	// non-dispatchable without re-deriving Linear's state-type classification.
	Dispatchable bool
}
type Issue struct {
	ID, Identifier, Title, Description, State, BranchName, URL, AssigneeID string
	NativeRef                                                              any
	Priority                                                               *int
	Labels                                                                 []string
	BlockedBy                                                              []Blocker
	Dispatchable                                                           bool
	CreatedAt, UpdatedAt                                                   *time.Time
}
type Usage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	TotalTokens  int64 `json:"total_tokens"`
}
type EventKind string

const (
	EventSessionStarted EventKind = "session_started"
	EventProgress       EventKind = "progress"
	EventUsage          EventKind = "usage"
	EventRateLimit      EventKind = "rate_limit"
	EventDiagnostic     EventKind = "diagnostic"
	EventBlocked        EventKind = "blocked"
	EventCompleted      EventKind = "completed"
	EventFailed         EventKind = "failed"
	// EventItem reports a single safe app-server tool or item lifecycle
	// transition (a command execution, file change, MCP/dynamic tool call, and
	// so on). It never carries tool arguments, command bodies, or outputs; see
	// ItemID, ItemType, ToolName, Outcome, and DurationMs.
	EventItem EventKind = "item"
	// EventLandingWaiting reports that the host-side landing capability returned
	// a non-terminal waiting result: required checks or GitHub's own
	// mergeability computation have not settled, so no model turn can advance
	// the issue. It is terminal for the logical run — the coordinator ends the
	// session and schedules its own delayed landing retry instead of spending
	// Codex turns or an agent-exhaustion retry (PMR-78). Message carries the
	// bounded, host-generated waiting reason, never model or provider text.
	EventLandingWaiting EventKind = "landing_waiting"
	// EventLandingResolved reports a terminal landing outcome: the pull request
	// is merged (by this call or already) and the bound issue was reconciled to
	// its terminal state. It ends the logical run immediately so no later turn
	// or landing tool call is possible for it (PMR-78).
	EventLandingResolved EventKind = "landing_resolved"
	// EventRateLimited reports a definitive backend rate-limit rejection: the
	// account's quota for the reported window is closed, not a model or
	// provider failure this issue's work caused. It is terminal for the
	// logical run -- continuing to retry a turn against a limit already known
	// to be closed only spends launches the coordinator cannot make progress
	// with (PMR-131). RateLimitStatus and RetryAfter carry the backend's own
	// classification and reset time so the scheduler can name the outcome and
	// schedule the next attempt without parsing Message.
	EventRateLimited EventKind = "rate_limited"
)

// Terminal reports whether an event of this kind ends the logical run: no
// later event follows it on the same stream.
func (k EventKind) Terminal() bool {
	switch k {
	case EventCompleted, EventFailed, EventBlocked, EventLandingWaiting, EventLandingResolved, EventRateLimited:
		return true
	}
	return false
}

// ItemOutcome enumerates the safe, protocol-derived lifecycle outcomes an
// EventItem can report. These mirror the Codex app-server's own status enum
// values plus the synthetic "started" outcome Symphony assigns on arrival.
const (
	ItemStarted   = "started"
	ItemCompleted = "completed"
	ItemFailed    = "failed"
	ItemDeclined  = "declined"
	ItemCanceled  = "canceled"
)

type Event struct {
	Kind                                 EventKind
	At                                   time.Time
	SessionID, ThreadID, TurnID, Message string
	PID                                  int
	Usage                                Usage
	RateLimit                            map[string]any
	// ItemID and ItemType identify the outstanding operation for an EventItem
	// record: a stable protocol-assigned call/item identifier and its
	// protocol-defined type (for example "commandExecution", "mcpToolCall",
	// "fileChange", or "dynamicToolCall"). ToolName is only ever a
	// protocol-provided identifier (an MCP/dynamic tool's fixed name), never a
	// value parsed out of tool arguments or command bodies.
	ItemID, ItemType, ToolName string
	Outcome                    string
	DurationMs                 int64
	// RateLimitStatus is the backend's own rate-limit classification (for
	// example Claude's "allowed_warning" or "rejected"), set on an
	// EventDiagnostic or EventRateLimited record so the scheduler can log and
	// classify it without parsing formatted Message text. Empty for every
	// other event.
	RateLimitStatus string
	// RetryAfter is the backend-reported delay before a rate-limited run
	// should be retried, set alongside an EventRateLimited record when the
	// backend gave a reset time. Zero when it did not; the scheduler falls
	// back to its own floor in that case (PMR-131).
	RetryAfter time.Duration
}
type RunStatus string

const (
	RunSucceeded RunStatus = "succeeded"
	RunFailed    RunStatus = "failed"
	RunCanceled  RunStatus = "canceled"
	RunTimedOut  RunStatus = "timed_out"
	RunStalled   RunStatus = "stalled"
	RunBlocked   RunStatus = "blocked"
	// RunWaiting is a run that ended on a non-terminal host gate outside the
	// agent's control (a landing wait). It is deliberately not a failure: the
	// coordinator redispatches the same attempt after a bounded delay.
	RunWaiting RunStatus = "waiting"
)

type Run struct {
	IssueID, IssueIdentifier, WorkspacePath, SessionID string
	Attempt, TurnCount                                 int
	StartedAt                                          time.Time
	Status                                             RunStatus
	Usage                                              Usage
}

type Tracker interface {
	ListCandidates(context.Context, []string) ([]Issue, error)
	GetIssues(context.Context, []string) ([]Issue, error)
	ListTerminal(context.Context, []string) ([]Issue, error)
	// Transition moves an issue into the named workflow state using the host
	// tracker credential. It backs the coordinator's deterministic
	// dispatch-time start transition (Todo -> In Progress). The adapter
	// re-reads the issue inside the call, so a stale caller-supplied state
	// cannot drive a wrong transition, and it is idempotent: an issue already
	// in the target state is a no-op. It never widens the running agent's
	// capability surface.
	Transition(ctx context.Context, issue Issue, toState string) error
}
type AgentRequest struct {
	Issue Issue
	// Model optionally overrides the agent runtime's default model.
	Model string
	// Backend names the agent runtime this request must run on. The scheduler
	// resolves the selection once, together with the command, sandbox, and
	// timeout values below, and the router honors it rather than resolving the
	// selection again -- two independent reads of a hot-reloadable configuration
	// could otherwise start a session on one runtime with another's launch
	// parameters. An empty value lets the router choose the configured backend.
	Backend string
	// GitMetadataRoots are the only paths outside the workspace directory a
	// workspace-write turn may write: the source repository's shared object
	// store and this linked worktree's own per-worktree metadata directory. It
	// deliberately excludes the rest of the source common directory (branch
	// refs, the primary index, packed-refs, other worktrees) so a misbehaving
	// agent cannot mutate the source repository's branches or primary working
	// tree (PMR-65).
	Workspace                     string
	GitMetadataRoots              []string
	Prompt, Command               string
	ApprovalPolicy, ThreadSandbox string
	TurnSandboxPolicy             any
	TurnTimeout, ReadTimeout      time.Duration
	// StartTimeout bounds the cold-start handshake and thread/start RPCs, which
	// on a cold codex app-server include process spawn and first model load. It
	// is deliberately separate from ReadTimeout so a generous cold-start budget
	// does not loosen steady-state mid-turn hang detection.
	StartTimeout time.Duration
}
type AgentSession struct {
	ID, ThreadID, TurnID string
	// Backend is the runtime that created this session, stamped by the router.
	// It is the authority for every later per-backend lookup about this run, so
	// no caller has to re-derive it from configuration that may since have
	// changed.
	Backend string
}
type AgentBackend interface {
	Start(context.Context, AgentRequest) (AgentSession, <-chan Event, error)
	Continue(context.Context, AgentSession, string) (<-chan Event, error)
	Cancel(context.Context, AgentSession) error
}
type Workspace struct {
	Path, Key        string
	GitMetadataRoots []string
	// GitIntegrityBaseline is a JSON-encoded snapshot of the source
	// repository's non-symphony branch heads at preparation time, so a
	// post-run assertion can detect drift that slips past the narrowed
	// sandbox grant (PMR-65) while telling an agent's write apart from a
	// concurrent operator fast-forward pull (PMR-145). Empty for non-Git
	// workspaces or when the baseline could not be captured.
	GitIntegrityBaseline string
	CreatedNow           bool
}

// CleanupOutcome is the fixed, secret-free vocabulary a successful terminal
// workspace cleanup reports. It exists so the operator log distinguishes an
// ordinary removal from one that discarded local commits, which is only ever
// allowed after Symphony itself verified those commits landed.
type CleanupOutcome string

const (
	// CleanupClean is a workspace that was already absent, or was removed with
	// no local commits past its recorded base commit.
	CleanupClean CleanupOutcome = "clean"
	// CleanupLanded is a clean, owned Git worktree whose HEAD was a local
	// commit past the recorded base commit, removed only because a
	// LandingVerifier confirmed that exact commit as the merged pull request
	// head for the bound issue and repository.
	CleanupLanded CleanupOutcome = "landed"
)

// LandingVerifier answers the one bounded question terminal cleanup must ask
// before it may discard committed work: was this exact local commit published
// and merged for this issue in the configured repository? Implementations are
// read-only and never widen the running agent's capability surface. A false
// answer, an unconfigured integration, or any error keeps cleanup fail-closed.
type LandingVerifier interface {
	VerifyLanded(ctx context.Context, issue Issue, commit string) (bool, error)
}

// IssueForgetter releases the in-process state a host integration still holds
// for an issue that reached a terminal tracker state and will never be
// dispatched again. It is the explicit end-of-life signal the GitHub
// linked-pull-request poller needs: without one, a process that runs for weeks
// keeps requesting the pull request of every issue it ever published and keeps
// that issue's credential snapshot and tracker session resident (PMR-112). It
// is deliberately a notification and not a question -- there is nothing for the
// scheduler to do about a failure -- so implementations must be idempotent,
// non-blocking, and safe for an issue ID they never saw.
type IssueForgetter interface {
	Forget(issueID string)
}

type WorkspaceExecutor interface {
	Prepare(context.Context, Issue) (Workspace, error)
	BeforeRun(context.Context, Workspace, Issue) error
	AfterRun(context.Context, Workspace, Issue)
	Cleanup(context.Context, Issue) (CleanupOutcome, error)
	Execute(context.Context, Workspace, string, []string) ([]byte, error)
}
