package config

import (
	"strings"
	"time"
)

type Settings struct {
	Tracker            Tracker
	Polling            Polling
	Workspace          Workspace
	Hooks              Hooks
	Agent              Agent
	Codex              Codex
	Claude             Claude
	GitHub             GitHub
	HostSecretEnvNames []string
	HostSecretValues   []string
	WorkflowPath       string
	LogRoot            string
	Prompt             string
	Warnings           []string
}

// GitHub is an optional, fixed-repository host integration. Invalid optional
// settings remain disabled so they cannot affect the manual workflow.
//
// MergeState, MergeMethod, RequiredChecks, UpdateStaleBranch, and the
// bounded-fix fields (LandFixEnabled, MaxLandAttempts, AllowConflictResolution)
// are the landing policy (PMR-37/PMR-45/PMR-46)
// and deliberately do not follow that same fail-open-to-disabled rule: unlike
// owner/repository/token/etc, which silently disable the whole optional
// integration on any invalid value, an invalid landing field is rejected as a
// hard configuration error the same way tracker.provider.transitions is.
// Granting an irreversible merge capability from an ambiguous or
// partially-invalid configuration is never an acceptable fallback.
type GitHub struct {
	Enabled                                        bool
	Owner, Repository, BaseBranch, Token, Endpoint string
	// PollInterval paces the host's linked pull-request poll loop and, since
	// PMR-78, is also the floor for the coordinator's delayed landing
	// redispatch after github_land_pr reports a non-terminal wait. Consecutive
	// waits escalate that delay toward Agent.MaxRetryBackoff.
	PollInterval time.Duration
	// MergeState is the exact Linear state that grants the zero-argument
	// github_land_pr capability to a session bound to an issue currently in
	// that state. Empty means landing is not configured.
	MergeState string
	// MergeMethod is one of "merge", "squash", or "rebase" and defaults to
	// "merge" when MergeState is configured.
	MergeMethod string
	// RequiredChecks are the exact check/status names that must all be
	// present and successful (or neutral) before github_land_pr will merge.
	// Non-empty whenever MergeState is configured.
	RequiredChecks []string
	// UpdateStaleBranch permits github_land_pr to ask GitHub to merge the
	// current base into a clean pull-request branch when the base branch moves
	// between Land's early base read and its immediate pre-merge one. It guards
	// that time-of-check/time-of-use window only: a pull request that was
	// already behind the base when landing started is landed as-is, because
	// nothing in Land compares the head to the base. Without it, a base that
	// moves mid-landing refuses instead. It is opt-in and disabled by default.
	UpdateStaleBranch bool
	// LandFixEnabled permits github_land_pr, for a retryable hard gate, to
	// return a non-terminal fix request (naming the gate) so the same Codex
	// turn can fix, push, and retry, instead of immediately refusing. It is
	// opt-in and disabled by default; with it off, every gate refuses exactly
	// as before (PMR-46).
	LandFixEnabled bool
	// MaxLandAttempts bounds how many non-terminal fix requests a single
	// session may hand back before it refuses and returns the issue to review.
	// It defaults to 2 and is only meaningful when LandFixEnabled is true.
	MaxLandAttempts int
	// AllowConflictResolution makes a merge conflict a retryable gate (only
	// when LandFixEnabled is true). Off by default, so a merge conflict refuses
	// immediately exactly as before.
	AllowConflictResolution bool
}

// LandingDispatch reports whether a run bound to an issue in issueState is a
// landing run: landing is configured, and the issue is in the exact configured
// MergeState. It is the one predicate for that question, and four call sites in
// three packages ask it -- which capabilities are advertised
// (capability.Build), which delivery mode the prompt renders
// (Settings.DeliveryInstructions), whether the Claude launch guard expects a
// publish capability (claude.verifyPromises), and whether github_publish_pr
// refuses (github.Session.Publish). A landing run and a publishing run are
// mutually exclusive deliveries, so a second, paraphrased copy of this
// condition anywhere would be a way for a prompt to invite exactly the tool the
// session refuses (PMR-169).
//
// It answers a question about a dispatch, not about live tracker state: the
// state is the one the session was bound to. A human move after that is
// re-validated where it matters, by Land's EnsureMergeState and Publish's
// EnsureActive, immediately before either mutates anything.
func (g GitHub) LandingDispatch(issueState string) bool {
	mergeState := strings.TrimSpace(g.MergeState)
	return mergeState != "" && strings.EqualFold(strings.TrimSpace(issueState), mergeState)
}

type Tracker struct {
	Kind                                         string
	Provider                                     map[string]any
	RequiredLabels, ActiveStates, TerminalStates []string
	HandoffState, HandoffCommentTemplate         string
	// HostTransitions is the single host-owned tracker transition policy
	// (tracker.provider.transitions). Symphony applies every edge in it itself,
	// with the host Linear credential; none is ever exposed to a Codex session.
	// The agent has no issue-state transition capability.
	HostTransitions HostTransitions
	// FollowupIssueCreation enables the session-bound Codex
	// create_followup_issue tool. It is opt-in and disabled by default; see
	// followup_issue_creation in tracker.provider.
	FollowupIssueCreation bool
}

// HostTransitions holds the two host-applied tracker transition edge sets.
// They are kept structurally distinct on purpose and must NOT be folded into
// one flat source->target map: Merging is both a dispatchable/active state and
// the land-fallback source, so a flat map consumed at dispatch would wrongly
// move a freshly dispatched Merging landing agent's issue to In Review. Start
// is keyed by the issue's current state and applied only at dispatch;
// RefuseLanding is keyed by github.merge_state and applied only when
// github_land_pr hits a hard gate. Both maps use lowercased source keys for
// direct comparison against a normalized issue state.
type HostTransitions struct {
	// Start are the dispatch-time edges the coordinator applies when it
	// launches an issue (the canonical lifecycle's Todo -> In Progress). Both
	// endpoints of every edge must be active, non-terminal states. The move is
	// idempotent (an already-started issue is untouched) and fail-safe (a
	// failed move is logged and never blocks or double-dispatches the run).
	Start map[string]string
	// RefuseLanding are the edges RefuseLanding uses after a github_land_pr
	// hard gate refuses to merge (the canonical lifecycle's Merging -> In
	// Review), keyed by github.merge_state. They are never applied at dispatch.
	// Terminal and same-state edges are rejected.
	RefuseLanding map[string]string
}

type Polling struct{ Interval time.Duration }
type Workspace struct{ Root, SourceRoot string }
type Hooks struct {
	AfterCreate, BeforeRun, AfterRun, BeforeRemove string
	Timeout                                        time.Duration
}
type Agent struct {
	MaxConcurrent, MaxTurns int
	// MaxAttempts bounds how many times one dispatch episode may launch the
	// same issue. MaxTurns bounds the turns inside a run and MaxRetryBackoff
	// bounds the delay between runs; neither bounds the number of runs, so
	// before PMR-111 an issue that failed deterministically (a corrupted
	// worktree, an always-failing before_run hook, a template error, an
	// unreachable agent binary) re-dispatched at the backoff ceiling for the
	// daemon's lifetime while holding its claim. Reaching this ceiling
	// abandons the dispatch: the coordinator logs one error-level record,
	// drops the claim, and leaves the tracker state alone.
	MaxAttempts     int
	MaxRetryBackoff time.Duration
	ByState         map[string]int
	// Backend names the agent runtime new sessions are started on. It is
	// validated against agentBackends, so an unknown value fails the whole
	// candidate rather than silently falling back to a default.
	Backend string
}

// AgentLaunch is the backend-neutral launch contract the scheduler applies to
// one run: what to execute, where, under which sandbox, and the four timeout
// budgets. Coordination reads this instead of any single backend's settings
// block, so adding a backend does not spread its vocabulary through the
// scheduler. The workspace directory and writable paths are not here: they are
// per-run values the workspace layer owns and travel on domain.AgentRequest.
// Every field is captured per launch; see Settings.AgentLaunch.
//
// TurnSandboxPolicy is interface-typed and may hold a map, so this struct is not
// safely comparable: compare fields, never two launches with ==.
type AgentLaunch struct {
	Backend                                              string
	Command, ApprovalPolicy, ThreadSandbox, Model        string
	TurnSandboxPolicy                                    any
	TurnTimeout, ReadTimeout, StartTimeout, StallTimeout time.Duration
}
type Codex struct {
	Command, ApprovalPolicy, ThreadSandbox string
	TurnSandboxPolicy                      any
	TurnTimeout, ReadTimeout, StallTimeout time.Duration
	// StartTimeout bounds the cold-start handshake and thread/start RPCs
	// separately from ReadTimeout. A cold codex app-server start (process
	// spawn plus first model load) routinely exceeds the small steady-state
	// read timeout, so it gets a generous budget that does not loosen
	// mid-turn hang detection.
	StartTimeout time.Duration
}

// Claude configures the Claude Code agent backend. It is deliberately small:
// the launch policy (tool set, permission mode, settings sources, and the
// sandbox) is fixed by Symphony rather than configurable, so an operator cannot
// widen the boundary the child runs under.
type Claude struct {
	Command, Model            string
	TurnTimeout, StallTimeout time.Duration
}
