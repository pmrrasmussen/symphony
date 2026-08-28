# Architecture and trust model

This service follows the Symphony specification using a coordination core that
only knows normalized issues, agent events, and workspace operations.  Linear
GraphQL, Codex JSON-RPC, the Claude Code CLI, and local process execution are
adapters behind `Tracker`, `AgentBackend`, and `WorkspaceExecutor`
respectively.  Which `AgentBackend` a session starts on is configuration-driven
(`agent.backend`): `codex` (the default) and `claude` are the accepted values,
and an unrecognized one is rejected at load rather than defaulted.  Only the
selected backend's launch contract has to be complete, so an absent `codex:` or
`claude:` block fails a candidate only when that backend is the one selected.
A `claude` workflow may enable Symphony's session capabilities.  The backend
prepares the same provider sessions Codex does, builds the same registry, and
serves it over the private loopback MCP endpoint (`internal/mcpbridge`)
described below; the host-generated delivery instructions name each capability
by the name its transport serves it under, which for that endpoint is
`mcp__symphony__<tool>`; and `claude.Backend.Start` cross-checks the
rendered prompt against what the session it is about to start can serve: it
refuses a prompt that promises host-side publish where the registry advertises no
`github_publish_pr`, a session advertising publish with no handoff state to
publish into, and a prompt that names any advertised capability without its
`mcp__symphony__` prefix.  That last one is what verifies the naming at runtime;
without it the whole of the naming guarantee is a single backend-name comparison
in `internal/config`.  The cross-check exists because the promise and the grant
are made by different components, in a fixed order, from *different settings
snapshots*: the coordinator renders the prompt from its own snapshot before the
backend builds the registry from a later one plus the bound issue and its
prepared provider sessions.  A reload that disables the GitHub integration
between the two is enough to produce a promise no session can keep, on an
ordinary issue.  The
shape that removes the problem rather than detecting it is to hoist
`capability.Build` into the host and pass the registry on `domain.AgentRequest`;
that touches `internal/domain`, both backends, and the router, and has not been
done.  Detecting it is not optional, because the undetected failure is the worst
available: every gate passes -- configuration valid, preflight green, init echo
exactly as expected -- while the turn ends `EventCompleted` with committed,
unpublished work.

Two residual configuration rules remain, both applying only to `claude`.  An
enabled GitHub integration requires `handoff_state`.  Without one and with
follow-up issues off, no Linear handoff session is prepared, so no GitHub session
is either and the integration grants nothing; without one and with follow-up
issues *on*, `followup_issue_creation` alone satisfies
`LinearSessionCapabilityEnabled`, so a handoff session is prepared, a GitHub
session is built on it, and `github_publish_pr` is advertised while the delivery
guidance tells the run that publishing is unavailable.  It is not true that such
a configuration advertises nothing, and the case where it advertises is the
dangerous one: a worker acting on the advertised tool reaches `LinkAndHandoff`
with no target state, which comments the pull request onto the issue and then
transitions it to no state at all, so the refusal lands after an irreversible
GitHub mutation.  The second rule is the narrower one it leaves behind: a
`handoff_state` with neither an enabled GitHub integration nor
`followup_issue_creation` prepares a handoff object nothing model-facing uses.
Both stay accepted under `codex`, where they behave identically because the
advertisement is the same registry's; they always have been, and the
prompt/advertisement mismatch above is pre-existing there and is not addressed
here.  Under `claude` they are refused so that "no MCP server at all" in the init
echo keeps a single meaning -- this workflow configures no capability -- rather
than also standing for a capability that was configured and could not be
reached.  A `github:` block that does not resolve stays disabled, as it does
under `codex`, so it configures nothing and reaches neither rule.

The initial implementation is intentionally for a trusted local machine.
`WORKFLOW.md` is repository-owned, versioned policy and its hooks are trusted
shell code -- trusted to run, which is not the same as trusted with the host's
credentials: a hook runs inside the agent's worktree and is filtered like any
other child, as described under the host credential filter below.  The agent can
make changes and execute commands according to the
policy its backend runs under -- for Codex the configured approval and sandbox
policy, for Claude the fixed launch contract described below; this service does
not provide Docker, VM, SSH, distributed execution, or a database.  Linear
credentials stay in the host process and are removed from the environment of
every process Symphony spawns.

On macOS, one repository may have one independently configured LaunchAgent.
The LaunchAgent is machine-local instance configuration, not repository policy:
it selects the shared executable, repository `WorkingDirectory`, and separate
workflow, workspace, log, service-log, and status paths. `WORKFLOW.md` remains
the repository-owned policy source. The per-repository `status.json` is a safe
current observation that can be stale after a crash, and `symphony.jsonl` is
redacted event history; neither establishes process liveness or replaces
Linear/workspace durable state. The TUI only presents local discovery and has
no mutation capability. See [macOS repository services](macos-services.md)
for the complete operator convention.

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
Retries themselves are bounded by `agent.max_attempts`: after that many failed
dispatches of one issue the coordinator abandons the episode, logging one
error-level record with `operation: dispatch_abandoned` and releasing the
claim, so a boundary that fails deterministically cannot occupy a claim and
re-dispatch at the backoff ceiling for the daemon's lifetime. The tracker is
untouched by that give-up, which is why the record is the operator's only
signal; a landing wait does not escalate the attempt and so never reaches the
ceiling.

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
Each Codex session can receive three capabilities bound to its active
Linear issue and managed worktree: a zero-argument `refresh_base_ref`
capability, a publish capability that accepts bounded `why`, `what_changed`,
and `on_call` structured fields (no repository, issue, or branch selection),
and a read-only `github_pr_context` capability with no input at all. The host
verifies a clean, committed descendant of the
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

`refresh_base_ref` exists because a merge that lands on the base branch mid-run
leaves a session's `refs/remotes/origin/<base>` stale, and the session cannot
fix that itself by design. `git merge origin/main` only writes objects and its
own worktree HEAD, both inside the sandbox's per-worktree grant
(`internal/workspace/local.go`'s `GitMetadataRoots`: the shared object store
plus this session's own worktree directory, never the Git common directory's
`refs` or `packed-refs`), so the `WORKFLOW.md` guidance to merge onto the base
branch works unmodified. `git fetch`, in contrast, must update
`refs/remotes/origin/<base>` in the common directory, and a fetch may rewrite
`packed-refs`, which also holds `refs/heads/*` -- exactly the source
repository write access PMR-65 narrowed the grant to prevent, and one no
static grant can safely widen for, since whether a given fetch touches it
depends on packing state and auto-gc. `refresh_base_ref` is the bounded,
zero-argument capability that performs that one fetch host-side instead: it
runs the same `git fetch --no-tags origin +refs/heads/<base>:refs/remotes/origin/<base>`
workspace creation already runs, keyed by `github.base_branch`, and returns
the resolved base commit so the caller can tell whether anything moved. It is
advertised whenever a GitHub session is bound, independent of issue state, and
a fetch failure is a refusal rather than a fatal error, so a transient network
failure costs a refused tool call rather than the run.

That linked-pair table is bounded by a defined end of life rather than by
process lifetime, because Symphony runs for weeks (PMR-112). A pair leaves it
on the very poll that can learn nothing further from it — the PR was observed
merged and reconciled, or closed without merge — which is also why the
closed-unmerged warning is logged exactly once without a suppression flag. A
pair whose PR is still open leaves only when the scheduler reports its issue
terminal: reaching a terminal tracker state is the one signal that no merge is
still coming, so nothing is evicted on age, which would silently stop
reconciling a merge that lands later. A reconciliation that failed keeps its
pair for the next tick, and an issue that publishes again after its pair was
dropped is tracked afresh.

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
return a non-terminal waiting result without mutating Linear. A waiting result
is settled by the host, not by the model (PMR-78): it ends the logical run at
once, releases the concurrency slot, keeps the issue in `merge_state`, and the
coordinator schedules one delayed landing redispatch whose timer holds only the
duplicate-prevention claim. That delay starts at `github.poll_interval_ms` and
escalates with the number of consecutive waits toward
`agent.max_retry_backoff_ms`, so an unsettling gate degrades to a slow poll
rather than a permanent per-interval respawn; the wait count is reset with the
claim and surfaced as `wait_attempt`. A redispatch that finds the state's
concurrency limit taken keeps that same cadence and attempt instead of
escalating as a failure would. A terminal result -- merged, already merged, or a
completed reconciliation -- ends the run the same way and closes
`github_land_pr` for that run, so no later turn or duplicate tool call can
re-invoke it; `Land` itself stays idempotent purely as the recovery path. With
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
paths, and agent settings, including a changed `agent.backend`. An agent session
already launched keeps the backend that started it and the command, sandbox, and
timeout values captured for that launch; concurrency changes do not evict it, but
subsequent reconciliation still applies the current state and stall policy, read
under the backend that owns the run rather than whichever one is configured now.
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

No host credential reaches a child Symphony spawns as an environment variable.
There are three kinds of child, not two: the Codex app-server, each Claude turn,
and each `WORKFLOW.md` workspace hook. One filter, described once, is applied by
all of them, and it
has four parts. First, a fixed set of reserved variable names -- the documented
names Symphony's own tracker and forge credentials are read from -- is removed
whatever the workflow configures, so it still applies to a workflow that
references no credential at all. Second, the variable names this workflow
actually references are removed; those are repository-chosen, so the fixed set
cannot know them. This second part is also the only one that covers a
credential *file path*: with `api_key_file` or `token_file` the variable holds a
path rather than the credential, so the value filter below never matches it
(PMR-80), and a worker that learned the path could read the file, which no
sandbox mode prevents. Third, any variable whose value contains a configured
credential is removed under any name, because an inherited variable Symphony has
never heard of can still carry the credential, plain or wrapped as
`Bearer <token>`; for a loaded workflow this is the broadest of the four, since
the loader resolves both credentials to their values before deriving it. Fourth,
the credential the run's bound providers actually hold is removed. That fourth
part is defence-in-depth today rather than the sole cover for anything -- no
loadable configuration separates it from the third -- and it becomes
load-bearing as soon as a backend relaunches per turn with providers bound,
because the first three parts are re-read from live settings on every turn while
a provider session holds the credential it froze at session build. Both sides of
that divergence are covered: every bound provider is asked, including the
process-wide GitHub manager, which reads its settings live, so a token rotated
by a reload is stripped alongside the frozen one the run still authenticates
with. The Claude backend, which spawns one process per turn, is exactly that
shape; it also adds exactly one variable after
filtering -- this turn's capability endpoint token, the credential it
deliberately hands over -- and blocks that name on the way in so it can have no
other source. Everything else is inherited on purpose: both CLIs authenticate
through the operator's own stored login, which lives in the home directory they
read.

A workspace hook is a child on the same terms. Its script is repository-owned
policy and is trusted as such, but `cmd.Dir` is the agent's own worktree, so a
hook that runs `make setup`, `./scripts/...`, or `npm run ...` executes code the
agent wrote and committed, outside the agent sandbox -- and `before_run` and
`after_run` bracket every turn on that worktree, so this is the ordinary
lifecycle rather than operator error. A hook therefore gets the filter too; it
has no session, so it passes no `SecretMatcher` and gets the three parts derived
from settings alone, forgoing only a credential held by a live provider under a
name and value no configuration mentions. Symphony adds `SYMPHONY_ISSUE_ID` and
`SYMPHONY_ISSUE_IDENTIFIER` after filtering and blocks both names on the way in,
so it is their only source. Hooks run under `sh -c` rather than `sh -lc`: a
login shell sources the operator's profile, a second uncontrolled input to a
process running in an agent-writable directory, and it is not the form
`--dry-run` validates the script with (`sh -n -c`). A hook resolves commands
from the daemon's own `PATH` -- under a LaunchAgent, the one written into the
plist -- rather than from a profile.

`internal/config`'s `ReservedSecretEnvNames` owns the reserved names and this
description; `internal/hostenv`'s `Filter` is the one implementation, applied by
every launcher, so the four parts cannot hold for one child and not another --
which is what they did not, before `Filter` existed. They did not hold for hooks
either until PMR-113, for the same reason in a different place: the doctrine
enumerated the agent backends, so the third child was never weighed against it.
`internal/capability`'s
`SecretMatcher` builds the fourth part from the same bindings that decide which
capabilities a session gets, so the providers a session can reach and the
credentials it strips cannot diverge; it is the one part a caller may omit,
because a process spawned outside any session has no bindings to build it from.
Each part is proven once over `Filter`, and each launcher -- both backends and
the hook path -- is proven to reach it with everything it must contribute.

When `workspace.source_root` is configured, `LocalWorkspaceExecutor` creates a
detached Git worktree for each issue. Before creating a new workspace, it
refreshes and resolves `origin/main`, so the initial commit is independent of
the source checkout's current branch, local `main`, index, and working tree.
Existing issue workspaces are reused without fetching, rebasing, or resetting
their history. This isolates Codex changes from the checkout running Symphony;
a human must review and integrate the resulting changes. The source root must
have a reachable `origin/main`, and Git worktrees require the local repository
to be trusted. A workspace-write Codex turn is granted
write access to only the two paths a detached-HEAD commit needs -- the source
repository's shared object store and this linked worktree's own per-worktree
metadata directory -- and never the rest of the common directory, so the agent
cannot write the source repository's branch refs (including the primary branch)
or the primary working tree's index. No host-owned Linear or GitHub credential
is passed to the worker as a variable.

That bound covers writes and the environment only. A Codex sandbox does not
restrict reads: a worker can read any file the user running Symphony can,
including credential files outside the worktree. This is a property of Codex's
sandbox modes generally -- the same reads succeed under `read-only` -- not of
the workspace-write grant. Network authority is separate and repository-owned:
`codex.turn_sandbox_policy` is validated against the Codex `SandboxPolicy`
schema and forwarded verbatim, overriding `thread_sandbox` for that turn and
later ones, and this repository configures `type: workspaceWrite` with
`networkAccess: true` (PMR-80). That boolean grants unrestricted outbound
network access, not merely local socket binding; repository validation that
binds loopback listeners is the reason it is enabled, not the limit of what it
permits. Omitting the key leaves outbound access denied.

So the operative boundary for a worker under this repository's policy is:
writes confined to its own worktree and the narrowed Git roots, no host
credential in its environment, but local reads and outbound network both
available. What protects host credentials from exfiltration is therefore the
absence of untrusted input reaching the worker, not the sandbox. Symphony's
validation keeps the policy from widening further: `writableRoots` is rejected
so the launcher remains the only source of writable paths, and unknown keys
inside the policy are rejected so a misspelled field cannot silently change
what the operator believes is configured.

## Sandbox ownership decision (PMR-85)

**Decision: do not add a Symphony-owned OS sandbox now.** Such a sandbox would
make the write boundary uniform and independent of a backend vendor, and would
need to cover every process and tool write path to close the remaining Claude
gap recorded below. It would also duplicate the protection Codex already provides and change
the launch path for both backends. That extra layer can break repository test
suites that need loopback listeners or module downloads, so it needs its own
cross-platform design, compatibility evidence, and review rather than being
added as an incidental documentation change.

This is not an assertion that the present Claude boundary is safe or uniform.
The selected `agent.backend` determines its strength: Codex's configured
`workspaceWrite` policy confines writes to the worktree and narrowed Git roots,
while Claude confines `Bash` and its children by path and confines `Edit`/
`Write` by their own path-scoped permission rule (PMR-156), which together
close every write path this launcher's settings payload can express. What that
payload cannot express, and what a Symphony-owned OS sandbox would (PMR-156's
still-open finding, see above): a `Bash` command inside the source
repository's `.git` directory can still reach `refs/heads/*`, `packed-refs`,
and the primary index, because the CLI's own sandbox generator widens the
declared `.git/objects`/`.git/worktrees/<id>` grant to the whole enclosing
`.git` directory, and no setting on this launcher's contract was found to
narrow that back down. If Symphony-owned enforcement is wanted, it must be a
separate issue with an explicit OS-level profile and validation plan; this
decision records the current trade-off rather than promising that the backend
contracts are equivalent. No current product decision requires Symphony to
impose a uniform OS-level boundary, so that work is not adopted as part of
PMR-85 or of PMR-156.

The Claude backend has a fixed launch contract but not equivalent write
containment, and none of that contract is configurable: the `claude:` block
carries only a command, an optional model, and two timeout budgets, while the
launch contract itself is fixed in `internal/claude`. Each turn is launched with
`--print --output-format stream-json --verbose`, `--permission-mode dontAsk`
(the only fail-closed non-interactive mode; the prompt occupies stdin, so a
mode that can block on stdin is unusable, and `bypassPermissions` is the
opposite of fail-closed), `--tools` restricted to
`Bash`/`Edit`/`Glob`/`Grep`/`Read`/`Write` and `--allowedTools` naming the same
tools -- except that `Edit` and `Write` are each named once per write root as a
path-scoped rule rather than bare (PMR-156; see below) -- with
`--disallowedTools WebFetch,WebSearch`, `--strict-mcp-config`,
`--setting-sources ""`, and an inline `--settings` payload. `--tools` is what
removes a tool from the surface -- a permission allowlist alone still
advertises it. `--strict-mcp-config` is load-bearing rather than hygienic, and
in both directions: it confines the session to the MCP configuration on its own
command line, so a session with no `--mcp-config` gets no MCP server at all and
a session with one can reach nothing but Symphony's own endpoint, while without
it the child additionally inherits the operator's own user-level MCP servers,
credentials included.

A session whose registry advertises at least one capability adds two things to
that contract. `--mcp-config` carries an inline configuration naming exactly one
`http` server, Symphony's loopback endpoint, whose `Authorization` header is a
`${SYMPHONY_MCP_TOKEN}` reference the CLI expands from the child's environment
-- so the per-registration bearer token lives only in an environment the owner
can read, never in a command line that on Linux any local account can. And
`mcp__symphony__<capability>` is added to both `--tools` and `--allowedTools`,
one explicit name per advertised capability rather than an `mcp__symphony__*`
glob, because the init echo is checked for set equality and a glob would let the
CLI advertise a capability Symphony never asked for and still pass. Both the
`--tools` list and the `tools/list` the endpoint serves are derived from one
registry frozen when the session was built, so a mid-run configuration reload
cannot make them disagree.

Those flags bound what the CLI offers the model, and they are not by themselves
a bound on what the child can invoke. The child's shell holds the bearer token
-- it is in the environment `Bash` runs with -- and loopback is inside the
sandbox, so a command can read the URL out of its own command line and address
the endpoint directly, naming a capability that appeared in neither flag nor in
`tools/list`, and no `tool_use` record is produced for it. The endpoint
therefore refuses any capability its registry does not advertise, which makes
the launch contract's set equality a statement about what is reachable and not
only about what is advertised: a directly addressed call can reach nothing the
model was not already permitted to call, so it grants no authority. Combined
with each provider re-validating its own preconditions before it mutates
anything, what is left is an observability gap -- such a call runs unrecorded --
and that is stated in README.md rather than claimed away.

The registration itself is per turn and is retired before the next turn's is
minted, not merely at turn end. After a turn emits its terminal event its
goroutine is still waiting on stderr, on `Wait`, and on the process-group kill,
while the coordinator has already asked for the next turn; a registration left
live across that window would let an escaped descendant of the previous turn
call a capability concurrently with the new one, against the same provider
sessions whose idempotency latches are all that stand between that and a second
landing attempt presenting itself as a first. Revocation drains any invocation
still running before it fires the registry's turn-ended finalizer, and it runs
on every path a turn can end on -- completion, failure, turn timeout, a hard
cancellation, and a launch that never produced a child -- because a missed
revocation is a credential lifetime leak rather than a leaked struct: the
registration holds the GitHub session, so a stale one keeps a loopback-reachable,
token-bearing capability set alive for the daemon's lifetime.

That finalizer runs on a context of the endpoint's own making, not on the run's.
Both are deliberate and both are operator-visible. The finalizer's work is the
deferred `Merging -> In Review` transition, which is precisely the work a stopped
run still owes its issue, so it must survive the cancellation that stopped the
run -- the coordinator cancels the run's context before it cancels the session,
and a finalizer inheriting that could never issue the transition. It is therefore
derived after the drain, where a budget is spent on the finalizer and not on a
two-minute wait that ignores contexts by design, and it is bounded at five
seconds. The consequence to expect while operating: stopping a run, and shutting
the daemon down with SIGTERM, can each still issue one tracker transition per
affected session after the stop was requested. The alternative is an issue left
in the configured merge state that no turn end will ever move. `--setting-sources ""` excludes user, project, and local
settings, which matters because the workspace is a checkout of a repository
that may ship `.claude/settings.json`, `CLAUDE.md`, skills, plugins, and hooks,
and hooks run arbitrary commands -- discovery left enabled would let repository
content widen the boundary the launcher fixes. The settings payload is
marshaled from Go structs and passed inline because the CLI silently ignores a
payload it cannot parse, so a hand-assembled string, or a file unreadable at
launch, would leave a session running with no policy and no diagnostic. Every
one of these is re-applied on every turn: `claude --print` runs one turn and
exits, a continuation is a new process resumed by an ID Symphony assigned
itself, and the CLI restores none of the contract on `--resume`.

That payload sets `sandbox.enabled` with `failIfUnavailable: true` and
`allowUnsandboxedCommands: false`, `filesystem.allowWrite` to the worktree plus
the same two narrow Git metadata roots the Codex profile is granted, and
`network.allowedDomains` to `["*"]` with `network.allowLocalBinding: true`.
The two network grants are separate in the CLI's own sandbox schema:
`allowedDomains` covers outbound connect, and without `allowLocalBinding` a
session cannot `bind(2)` a loopback listener at all -- `net.Listen("tcp",
"127.0.0.1:0")` fails with "bind: operation not permitted" regardless of the
domain allowlist (PMR-143), which is what several of this repository's own
`httptest`- and `mcpbridge`-backed test suites need to run. Claude's default
when `failIfUnavailable` is omitted permits a fail-open fallback after an
initialization failure; Symphony sets the flag to prevent that fallback. It
does not make the sandbox cover tools it never governs: PMR-156 found that the
sandbox initialized and enforced `Bash` while `Edit`/`Write` could still write
outside the worktree, and closed that gap for those two tools specifically --
see below. On the same CLI version, Bash writes to `$HOME` and `$TMPDIR` were
refused with "operation not permitted" and per-domain network control worked
in both directions.

Five limits are stated rather than implied. First, the sandbox governs `Bash`
and its children by path, via `sandbox.filesystem.allowWrite`; `Edit` and
`Write` are never covered by that setting at all, because a permission rule
decides whether a tool exists, not where it may write. PMR-156 verified the
consequence using Symphony's exact launch contract -- a `Bash` append to an
absolute path in the source working tree was denied, while `Write` wrote that
same path successfully -- and closed it by giving `Edit` and `Write` their own
path-scoped permission rule instead of the bare tool name: `Allow` carries one
`Edit(//root/**)` and `Write(//root/**)` rule per write root (`scopedAllow` in
`internal/claude/launch.go`), and `--allowedTools` carries the identical
rendered list so the flag and the payload cannot disagree about what either
tool may touch. The CLI's own builtin `/statusline` command was the source for
this rule's syntax (`Read(~/**)`, `Edit(~/.claude/settings.json)`, verified
against claude 2.1.248) since it is not part of the documented `--settings`
schema. `Bash` itself keeps the bare tool name: its confinement was never the
permission rule, and always came from `allowWrite`.

Second, that leaves one confirmed gap `allowWrite` does not close, and it is a
limitation of the CLI's own sandbox rather than of this launcher's payload: a
`Bash` command can still write anywhere under the source repository's whole
`.git` directory, not only the two roots the payload names. PMR-156's
investigation demonstrated it directly -- with the declared grant naming only
`.git/objects` and this worktree's own `.git/worktrees/<id>`, a plain redirect
to `refs/heads/`, `git update-ref`, and `git commit` all succeeded from inside
a live session, and the latter two moved `refs/heads/main`. The rendered
`allowWrite` array itself names only the two documented roots -- this is a
gap in what the CLI enforces from that array, not in what Symphony asks it to
enforce, and no combination of settings this payload can express was found to
narrow it. It was invisible in the session that first hit it: concurrent
agents committed to the operator's `main` and left another agent's edits in
the primary working tree before it was discovered. Closing it for `Bash` the
way `Edit`/`Write` were closed -- with a boundary this process owns rather
than one the CLI's own sandbox generator applies -- is exactly the
Symphony-owned OS sandbox the "Sandbox ownership decision (PMR-85)" section
below declined to build; that decision, not this one, is where reopening the
question belongs. Third, reads are not confined,
exactly as for Codex. Fourth, `network.allowedDomains: ["*"]` is unrestricted
outbound access, the same deliberate choice as the Codex profile's
`networkAccess: true`; per-domain control exists and works but is not used to
restrict anything. `allowLocalBinding: true` is the separate grant that lets a
session bind loopback, matching what `networkAccess: true` already covers in
the Codex profile in one flag; a Bash call that still fails to bind despite
the grant is reported as an `EventDiagnostic` naming the sandbox denial
(`internal/claude`'s stream decoder recognizes the fixed "bind: operation not
permitted" marker in a failed tool result), rather than surfacing only as an
undifferentiated failed item indistinguishable from a real regression.
Fifth, the only confirmation that the contract applied is
the CLI's own `system`/`init` event, which reports the working directory, tool
surface, permission mode, and attached MCP servers: Symphony requires the tool
surface and permission mode to match the contract that turn was launched under
exactly, requires exactly the MCP servers that contract asked for -- none for a
session with no capability, and Symphony's own endpoint reporting itself
`connected` rather than `pending` for a session with one -- requires the
reported working directory to resolve to the issue's workspace, and fails the
turn closed otherwise or when no init event arrives at all -- but the event does not report sandbox state, so
the sandbox's own status is not observable in the stream. A sandbox
initialization failure can show up only as failed `Bash` items whose tool-result
text Symphony does not parse; it does not itself fail the turn. The PMR-156
coverage gap produced no such failed items or other warning. The child
environment is filtered by exactly the same host credential filter as Codex's,
with the one addition described there: this turn's capability endpoint token.

One consequence of running the CLI is not a boundary Symphony controls at all:
the CLI persists its own full transcript -- rendered prompts, issue
descriptions, and tool output -- to
`~/.claude/projects/<cwd-slug>/<session-id>.jsonl`, outside the worktree and
untouched by workspace cleanup. `--resume` reads it, so it cannot be disabled,
and relocating it with `CLAUDE_CONFIG_DIR` breaks subscription authentication
(verified). See [observability.md](observability.md).

As a defense-in-depth backstop, after each run Symphony re-checks that the
source repository's non-`symphony/*` branch heads and primary
index are unchanged from a baseline captured at preparation, and alerts on
drift. The host still owns all GitHub publishing authority. Workspace state below the configured
root records durable ownership and Git cleanup identity; it never suppresses an
otherwise active issue. Invalid ownership state, or missing state beside an
existing workspace, fails closed during preparation. The schema, restart
behavior, and deliberate operator remediation procedure are documented in
[workspace ownership and recovery](completion-markers.md). Cleanup refuses to
remove a worktree with local changes, and refuses to remove a worktree whose
commit differs from its recorded base revision unless the host GitHub adapter
verifies that exact commit as the merged head of the issue's pull request.

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
The Claude Code CLI's launch flags, `--print` stream-JSON event shapes, and
sandbox behaviour -- including the initialization fallback
`failIfUnavailable` prevents, the PMR-156 `Edit`/`Write` tool-coverage gap and
its fix, and the still-open `.git`-widening gap -- were inspected empirically
against locally installed `claude` 2.1.245 and 2.1.248 on macOS. None of it is
a published protocol schema, so a CLI upgrade can change it: re-verify the
launch contract and the event decode against the new version rather than
assuming they carried over.

One bounded-run recovery detail is intentionally Go-specific: active issues
continue on the same live Codex session, with a scheduler-controlled one-second
delay between turns, until `agent.max_turns`. Reaching that boundary while the
issue is still active is an explicit blocked/exhausted result, not successful
completion. The coordinator logs `turn_limit_exhausted` and schedules its
normal backoff retry -- up to `agent.max_attempts` dispatches -- leaving the
workspace eligible so a resolved external condition or a Linear update can be
dispatched safely. A run that ends on a
host gate no model turn can advance -- today only a landing wait -- is
deliberately not that failure path: it finishes as a `waiting` run and gets a
delayed, non-escalating `landing` retry instead of consuming turns and an
agent-exhaustion attempt. Durable completion is
reserved for a verified handoff or terminal tracker transition; an ordinary
Codex turn completion is never enough to suppress an active issue. The same
bound applies to a Claude session, where "the same session" is a resumed
session ID rather than a live process: each turn is a new child resumed with
`--resume`, and the inter-turn delay, turn limit, and exhaustion result are
unchanged.

The upstream workflow schema does not define a continuation-prompt setting.
Accordingly, the workflow body remains the configured first-turn task prompt,
while later turns receive generated upstream-style continuation guidance using
the configured `agent.max_turns` value. This avoids replaying the original task
prompt already present in the live thread.
