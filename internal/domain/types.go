// Package domain contains tracker-neutral types and the replaceable execution boundaries.
package domain

import (
	"context"
	"time"
)

type Blocker struct {
	ID, Identifier, State string
	Dispatchable          bool
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
)

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
	Error                                              string
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
type AgentSession struct{ ID, ThreadID, TurnID string }
type AgentBackend interface {
	Start(context.Context, AgentRequest) (AgentSession, <-chan Event, error)
	Continue(context.Context, AgentSession, string) (<-chan Event, error)
	Cancel(context.Context, AgentSession) error
}
type Workspace struct {
	Path, Key        string
	GitMetadataRoots []string
	// GitIntegrityBaseline fingerprints the source repository state an isolated
	// worktree must never modify (its non-symphony branch heads and primary
	// index) at preparation time, so a post-run assertion can detect drift that
	// slips past the narrowed sandbox grant (PMR-65). Empty for non-Git
	// workspaces or when the baseline could not be captured.
	GitIntegrityBaseline string
	CreatedNow           bool
}
type WorkspaceExecutor interface {
	Prepare(context.Context, Issue) (Workspace, error)
	BeforeRun(context.Context, Workspace, Issue) error
	AfterRun(context.Context, Workspace, Issue)
	Cleanup(context.Context, Issue) error
	Execute(context.Context, Workspace, string, []string) ([]byte, error)
}
