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
type Usage struct{ InputTokens, OutputTokens, TotalTokens int64 }
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
