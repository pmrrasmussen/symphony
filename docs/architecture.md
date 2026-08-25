# Architecture and trust model

This service follows the Symphony specification using a coordination core that
only knows normalized issues, agent events, and workspace operations.  Linear
GraphQL, Codex JSON-RPC, and local process execution are adapters behind
`Tracker`, `AgentBackend`, and `WorkspaceExecutor` respectively.

The initial implementation is intentionally for a trusted local machine.
`WORKFLOW.md` is repository-owned, versioned policy and its hooks are trusted
shell code.  Codex can make changes and execute commands according to its
configured approval and sandbox policy; this service does not provide Docker,
VM, SSH, distributed execution, or a database.  Linear credentials stay in
the host process and are removed from Codex's environment.

The canonical lifecycle (PMR-38) is `Todo -> In Progress -> In Review <->
Rework -> Merging -> Done`. `Todo`, `In Progress`, `Rework`, and `Merging` are
configured as `tracker.active_states`, so the coordinator dispatches a session
for an issue in any of them; `In Review` is deliberately excluded so it stays
the single, fixed, human-controlled review state
(`tracker.provider.handoff_state`) that is never dispatched. When the
coordinator dispatches an issue whose state matches a configured
`tracker.provider.transitions.start` source (the canonical `Todo -> In
Progress`), it performs that move itself, host-side, with the Linear
credential before the session starts, so board-level observability does not
depend on the agent self-starting. The move reuses the same read-and-verify,
resolve-state, and `issueUpdate` primitives as the host handoff path but is
never exposed to Codex; it is idempotent (an already-started or restart-
re-observed issue is untouched) and fail-safe (a failed move is logged and
never blocks or double-dispatches the run). Resuming in
`Rework` uses no
separate code path from an ordinary dispatch: the same durable workspace
ownership, worktree, and branch described below are reused, and republishing
the same deterministic pull request (see the GitHub adapter below) hands the
issue back to `In Review` regardless of which active state the session
started from. `Merging` is likewise an ordinary active state; what is
special is that a session dispatched for an issue currently in the exact
configured `github.merge_state` additionally receives the bounded
`github_land_pr` tool described below. Coordinator capacity is state-aware:
`agent.max_concurrent_agents` bounds total concurrent sessions, and the
optional `agent.max_concurrent_agents_by_state` map additionally bounds
concurrency within one normalized state name (for example, at most one
concurrent `Merging` landing session even when overall capacity allows more).
A queued retry timer never occupies this capacity -- only a live session or a
launch already in flight does -- so one landing session and one unrelated
implementation session can run concurrently when global capacity permits.

The optional `create_followup_issue` capability follows the same model: it is
disabled unless `tracker.provider.followup_issue_creation` is configured and
derives its project, team, Backlog state, and originating issue from the same
bound active-issue read used by the handoff and transition operations. It never
accepts a project, team, initial state, parent, arbitrary issue, or credential
from Codex. The only optional relationship is to the originating issue itself:
`related`, or blocked by the current issue. Because Backlog must be excluded
from dispatchable states, capturing follow-up scope cannot fan out another
worker or make the current issue wait. See the [Linear tracker
profile](linear-tracker.md) for the exact bounded fields.

The optional GitHub adapter follows the same capability model. Configuration
fixes one owner, repository, base branch, and host-only fine-grained token.
Each Codex session can receive only two capabilities bound to its active
Linear issue and managed worktree: a publish capability that accepts bounded
`why`, `what_changed`, and `on_call` structured fields (no repository, issue,
or branch selection), and a read-only `github_pr_context` capability with no
input at all. The host verifies a clean, committed descendant of the
configured base, pushes a deterministic issue branch, and creates or reuses
that branch's PR with a deterministic `Why`/`What changed`/`On Call` body
built from the structured fields plus the bound Linear issue URL; repeat
publication with unchanged fields leaves the body untouched, while changed
fields update it in place. A merged PR is irrecoverable and rejected; a
closed-unmerged PR is reopened; a PR whose head, base, or repository no
longer matches the bound branch is rejected rather than reused. The context
capability performs no mutation: it resolves the same bound PR and returns
bounded, redacted check status, an effective review state computed from each
reviewer's latest review, capped comment/review excerpts, and unresolved
review-thread counts read over the GitHub GraphQL API, never raw provider
payloads or credentials. Invalid arguments, provider failures, and
unsupported states (a merged or unrecoverably closed PR, an ambiguous or
mismatched pull request list, a missing PR for `github_pr_context`) are
returned as structured tool failures so a rejected call never ends an
otherwise recoverable Codex turn. The host records the PR/issue pair for
polling once publication succeeds. Polling can transition that one review
issue to `Done` after GitHub confirms a human merge; it has no merge
operation. Closed-unmerged PRs only produce an operator warning. The
linked-pair and completion guard are process-local, while retries reconcile
durable GitHub PRs and Linear comments.

An additional optional landing capability (PMR-37) is gated by
`github.merge_state`: a session bound to an issue currently in that exact
Linear state receives a zero-argument `github_land_pr` tool. Unlike the rest
of the GitHub adapter's optional settings, `merge_state`, the bounded
`merge_method` enum (merge, squash, or rebase), and the non-empty
`required_checks` list it requires are validated the same strict, fail-closed
way as `tracker.provider.transitions`: an invalid value rejects the
whole workflow rather than silently disabling the feature. Landing re-fetches
the configured base, verifies the credential-free origin, a clean committed
worktree, the one deterministic open PR for the bound branch, and the current
Linear scope/state, pushing the worktree's HEAD first if it is ahead of the
published branch. Immediately before the irreversible merge call it re-reads
required checks, the effective review state (each reviewer's latest
non-dismissed review; moving the issue to `merge_state` is itself the human
approval, so no separate approving review is required), unresolved review
threads, the pull request's state and mergeability, and the base commit
again. Missing or pending required checks, or undetermined mergeability,
return a non-terminal waiting result without mutating Linear. With
`github.update_stale_branch: true`, one clean stale-base update waits for
checks on its new head. Any other hard gate -- a failing check, an effective
changes-requested review, an unresolved thread, a stale base, a merge
conflict, or a closed/mismatched pull request -- refuses landing and attempts
the configured `merge_state -> In Review`
fallback transition, itself a safe no-op once the issue is no longer exactly
in `merge_state`. A successful merge transitions the bound issue to `Done`
exactly once; a pull request GitHub already reports merged, discovered at any
point (including a race during the merge call itself), reconciles that same
`Done` transition idempotently instead of merging again, which is also how a
GitHub merge that succeeds despite a failed Linear completion call is
recovered on retry.

The loader validates the supported core front-matter fields but preserves
unknown extension keys for forward compatibility. It applies documented
defaults, resolves explicit `$VARNAME` references only for documented secret
and path fields, rejects ambiguous expansion syntax, and normalizes paths
relative to the workflow file. A candidate snapshots environment values and
reads each referenced file once before it is validated and atomically
published. The reload fingerprint includes those dependencies; an environment
or secret-file correction therefore retries a rejected workflow without an
unrelated file edit. Readers receive defensive copies of the last complete
snapshot, and invalid reloads retain that snapshot. Prompts render strictly per
run with lowercase `issue` and nullable `attempt` variables, so template
failures do not prevent polling or configuration reload.

Linear project scope is normalized to `project_slug_id`. The deprecated
`project_slug` alias is converted before publication and produces only a
constant migration warning; simultaneous canonical and legacy keys are rejected
as ambiguous. Credential-file paths should enter repository-owned policy through
`$SYMPHONY_LINEAR_API_KEY_FILE`. Neither migration warnings nor secret-file read
errors include configured project, credential, or path values.

Successful reloads affect settings reads which begin after publication. Future
polls and run launches therefore use the new states, intervals, limits, hooks,
paths, and Codex settings. A Codex process already launched keeps its captured
command, sandbox, and timeout values; concurrency changes do not evict it, but
subsequent reconciliation still applies the current state and stall policy.
The process log destination is selected by `--logs-root` at startup rather than
by reloadable workflow policy.

Authoritative durable state is Linear plus the workspace tree under the
configured root.  In-memory claims are rebuilt after restart; startup cleans
workspaces for terminal issues.  Logs are written to the configured log root
and must not contain credentials or complete agent payloads. The default log
level is concise; `--log-level debug` is an opt-in CLI setting that adds
categorized poll admission/rejection summaries, safe Codex tool/item
lifecycle classification, and heartbeat/stall records naming the outstanding
operation, all built from a fixed, narrow decode of protocol fields that
never includes tool arguments, command bodies, outputs, or raw payloads. See
[observability.md](observability.md).

When `workspace.source_root` is configured, `LocalWorkspaceExecutor` creates a
detached Git worktree for each issue. This isolates Codex changes from the
checkout running Symphony; a human must review and integrate the resulting
changes. The source root must already have a commit, and Git worktrees require
the local repository to be trusted. A workspace-write Codex turn is granted
write access to only the two paths a detached-HEAD commit needs -- the source
repository's shared object store and this linked worktree's own per-worktree
metadata directory -- and never the rest of the common directory, so the agent
cannot write the source repository's branch refs (including the primary branch)
or the primary working tree's index. This does not grant network access or a
GitHub credential. As a defense-in-depth backstop, after each run Symphony
re-checks that the source repository's non-`symphony/*` branch heads and primary
index are unchanged from a baseline captured at preparation, and alerts on
drift. The host still owns all GitHub publishing authority. Workspace state below the configured
root records durable ownership and Git cleanup identity; it never suppresses an
otherwise active issue. Invalid ownership state, or missing state beside an
existing workspace, fails closed during preparation. The schema, restart
behavior, and deliberate operator remediation procedure are documented in
[workspace ownership and recovery](completion-markers.md). Cleanup refuses to
remove a worktree with local changes or a commit that differs from its recorded
base revision.

Workspace containment is checked against canonical filesystem paths, including
existing symlink ancestors. Service-owned workspace directories, the durable
state directory, and state-marker files must not themselves be symlinks; a
path that resolves outside the configured root is rejected before it is read,
executed, or removed. This is a trusted-local-machine boundary, not a defence
against a malicious same-host process: filesystem checks and subsequent use
cannot atomically prevent a concurrent rename or symlink swap. Keep the
configured workspace root writable only by trusted users and processes.

The behavioral contract is the pinned upstream
[Symphony specification](https://github.com/openai/symphony/blob/7af5a7648c9fbffa08825fe0c0b18be00100aff3/SPEC.md).
It is linked rather than copied into this repository so it cannot silently
diverge from its source. Implementation baseline: upstream Symphony commit
`7af5a7648c9fbffa08825fe0c0b18be00100aff3`. Codex app-server protocol was
inspected from the locally installed Codex schema generated on 2026-07-18;
upstream Codex HEAD at inspection was `56395bddaf26eb2829387ca6a417bf9128e5b239`.

One bounded-run recovery detail is intentionally Go-specific: active issues
continue on the same live Codex session, with a scheduler-controlled one-second
delay between turns, until `agent.max_turns`. Reaching that boundary while the
issue is still active is an explicit blocked/exhausted result, not successful
completion. The coordinator logs `turn_limit_exhausted` and schedules its
normal backoff retry, leaving the workspace eligible so a resolved external
condition or a Linear update can be dispatched safely. Durable completion is
reserved for a verified handoff or terminal tracker transition; an ordinary
Codex turn completion is never enough to suppress an active issue.

The upstream workflow schema does not define a continuation-prompt setting.
Accordingly, the workflow body remains the configured first-turn task prompt,
while later turns receive generated upstream-style continuation guidance using
the configured `agent.max_turns` value. This avoids replaying the original task
prompt already present in the live thread.
