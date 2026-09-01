// Package domain contains tracker-neutral types and the replaceable execution boundaries.
package domain

import (
	"context"
	"fmt"
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
	// AssigneeMismatch reports whether the tracker's resolved assignee policy
	// (config.Tracker.Provider["assignee"], with a "me" value already resolved
	// to the acting viewer's ID) does not match AssigneeID. It lets a caller
	// identify an assignee-policy rejection specifically without re-reading the
	// unresolved config value itself, which cannot distinguish "me" from a
	// genuine mismatch without the same network resolution Dispatchable used.
	AssigneeMismatch     bool
	CreatedAt, UpdatedAt *time.Time
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
	// Usage carries a backend's running-total token figure for an EventUsage
	// record. Whether it is a settled total or a provisional estimate is
	// UsageAuthoritative's job to say -- Usage alone cannot be trusted to be
	// monotonically non-decreasing across a run (PMR-153).
	Usage Usage
	// UsageAuthoritative reports whether Usage is a settled figure the
	// coordinator should adopt outright, rather than a provisional estimate to
	// merge with what it already has by taking the component-wise maximum.
	// Claude's end-of-turn result sets this: it is the CLI's own authoritative
	// turn total, which can legitimately be lower than the mid-turn figure the
	// backend emitted moments earlier while the turn was still running
	// (PMR-136), because that mid-turn figure is this host's own running sum
	// of per-API-call deltas and nothing guarantees it agrees with the CLI's
	// own turn total once the turn closes. Codex's notifications leave this
	// false: they are genuinely cumulative and monotonically increasing by
	// construction, so max() is the correct merge for them and always agrees
	// with taking the value outright. False for every event that is not
	// EventUsage.
	UsageAuthoritative bool
	RateLimit          map[string]any
	// ItemID and ItemType identify the outstanding operation for an EventItem
	// record: a stable protocol-assigned call/item identifier and its
	// protocol-defined type (for example "commandExecution", "mcpToolCall",
	// "fileChange", or "dynamicToolCall"). ToolName is only ever a
	// protocol-provided identifier (an MCP/dynamic tool's fixed name), never a
	// value parsed out of tool arguments or command bodies.
	ItemID, ItemType, ToolName string
	Outcome                    string
	DurationMs                 int64
	// RateLimitStatus is the backend's fixed, host-owned rate-limit category
	// (Claude currently emits allowed_warning, rejected, or unrecognized; the
	// healthy allowed status emits no event), set on an EventDiagnostic or
	// EventRateLimited record so the scheduler can log and classify it without
	// parsing formatted Message text. It is never arbitrary backend wire text.
	// Empty for every other event.
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
	// RunStalled is the one timeout-shaped outcome a run has: the coordinator's
	// own idle watchdog stopping a session that went quiet. There is deliberately
	// no separate "timed out" status, because nothing else times a run out --
	// the one that existed was reached only by matching "timeout" in an error's
	// text, which labelled a tracker outage an agent timeout (PMR-179).
	RunStalled RunStatus = "stalled"
	RunBlocked RunStatus = "blocked"
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
	// Transition moves an issue from fromState into toState using the host
	// tracker credential. It backs the coordinator's deterministic
	// dispatch-time start transition (Todo -> In Progress). The adapter
	// re-reads the issue inside the call and writes only while that fresh
	// state still equals fromState, so a human who moved the issue after the
	// caller's snapshot always wins; it is idempotent, an issue already in
	// toState is a no-op. It never widens the running agent's capability
	// surface.
	Transition(ctx context.Context, issue Issue, fromState, toState string) (TransitionResult, error)
}

// TransitionResult reports what a host-side transition observed and did, so a
// caller logs the state the write decision was actually made against rather
// than the snapshot it asked with.
type TransitionResult struct {
	// FromState is the issue's freshly read state at the moment of that
	// decision. It is empty only when the read itself failed.
	FromState string
	// Applied is true when the issue is in toState after the call — because
	// this call wrote it, or because it was already there. It is false, with no
	// mutation sent, when FromState no longer matches the requested fromState.
	Applied bool
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
	// GitMetadataRoots are the only paths outside the workspace directory
	// Symphony asks a workspace-write turn to be granted: the source
	// repository's shared object store and this linked worktree's own
	// per-worktree metadata directory. It deliberately names neither the rest
	// of the source common directory (branch refs, the primary index,
	// packed-refs, other worktrees) nor anything above it (PMR-65).
	//
	// It is a request, and what enforces it is the backend, not this field.
	// Codex confines a workspace-write turn to exactly these roots. The Claude
	// CLI widens its own git-metadata grant to the whole enclosing .git
	// directory once any subpath of it is granted, so a Bash command in that
	// session still reaches the source repository's refs/heads and packed-refs,
	// and no setting Symphony's launch contract can render narrows it back
	// (PMR-161). Do not read this comment as a guarantee that the source
	// repository's branches cannot move: under Claude the enforced backstop is
	// the post-run source-integrity check, which detects an unexplained move
	// and fails the run (see WorkspaceExecutor.AfterRun and
	// SourceIntegrityError). docs/architecture.md's "Workspace isolation and
	// the sandbox boundary" states the accepted exposure, why prevention was
	// not built, and the containment requirement that stands while it does.
	Workspace        string
	GitMetadataRoots []string
	// CacheRoot is the one host-owned directory outside the worktree a session
	// may write, carrying config.Agent.CacheRoot to whichever backend runs this
	// request. Empty grants nothing. Like GitMetadataRoots it is a request, and
	// the backend is what enforces it.
	CacheRoot                     string
	Prompt, Command               string
	ApprovalPolicy, ThreadSandbox string
	TurnSandboxPolicy             any
	TurnTimeout, ReadTimeout      time.Duration
	// StartTimeout bounds the cold-start handshake and thread/start RPCs, which
	// on a cold codex app-server include process spawn and first model load. It
	// is deliberately separate from ReadTimeout so a generous cold-start budget
	// does not loosen steady-state mid-turn hang detection.
	StartTimeout time.Duration
	// Capabilities is the bounded capability set the host prepared for this
	// dispatch, built once from the same settings snapshot that rendered Prompt
	// and carried here so a backend never builds one of its own (PMR-182). A
	// request carrying nothing leaves the session with no capability at all,
	// which is what a host with no providers wired prepares.
	Capabilities SessionCapabilities
}

// SessionCapabilities is what a host-side capability preparation produces for
// one request, carried opaquely.
//
// It has no method set, and that is a constraint rather than a preference:
// internal/capability imports this package -- its Bindings name a domain.Issue,
// its results a domain.EventKind -- so naming Registry, Definition, or
// Capability here would close an import cycle. The two backends narrow it back
// with capability.From, which is the one place the concrete type is asserted and
// the one place a request carrying something else is refused.
type SessionCapabilities any

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
	// SourceRoot is the repository this workspace is a linked worktree of, as
	// resolved at preparation time. The post-run integrity check reads it from
	// here rather than from the workspace's own state record, because a run
	// whose issue reached a terminal state removes that record before AfterRun
	// is reached -- which used to skip the check on exactly the runs that ended
	// cleanly (PMR-161). Empty for a workspace with no source repository.
	SourceRoot string
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

// WorkspaceExecutor is the workspace lifecycle the coordinator drives: prepare,
// bracket each run, and clean up. It deliberately has no general "run this
// command in the workspace" method. It carried one until PMR-175 -- with no
// caller, no environment filter, and unbounded output -- and a method like that
// is a host-side child running in an agent-written working directory, so its
// first caller would have shipped the daemon's credentials there before anyone
// reviewed it. A future caller adds it back with hostenv.Filter applied at the
// exec site, the way workspace.hook and the package's git runners do.
type WorkspaceExecutor interface {
	Prepare(context.Context, Issue) (Workspace, error)
	BeforeRun(context.Context, Workspace, Issue) error
	// AfterRun brackets the run and returns its source-integrity verdict: a
	// SourceIntegrityError when the run left the source repository's branch
	// refs changed in a way no operator activity explains, and nil otherwise.
	// That error is the only thing it reports. An after_run hook failure is
	// logged and not returned, because a hook is repository-owned automation
	// whose failure says nothing about whether the write boundary held.
	AfterRun(context.Context, Workspace, Issue) error
	Cleanup(context.Context, Issue) (CleanupOutcome, error)
}

// SourceIntegrityError is AfterRun's verdict that a run left the source
// repository's branch refs changed, with nothing an operator did explaining it
// (PMR-65, PMR-161). Under the Claude backend the write boundary this reports
// on is enforced by the CLI's own sandbox and is known not to cover a Bash
// command inside the source .git directory, so this is a detection that must
// fail its run rather than a redundant assertion about a boundary already held.
//
// Changes carries the rendered ref changes, each attributed where possible to
// the workspace whose HEAD carries the commit the ref moved to -- which is
// frequently not the run reporting the error, because whichever run finishes
// first is the one that observes another's write.
type SourceIntegrityError struct {
	SourceRoot string
	Changes    string
}

func (e SourceIntegrityError) Error() string {
	return fmt.Sprintf("source repository %s has ref changes no operator activity explains: %s", e.SourceRoot, e.Changes)
}
