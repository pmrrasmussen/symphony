# Symphony (Go)

Symphony is a long-running issue runner for one configured agent backend, the
Codex app-server or the Claude Code CLI (`agent.backend`). It reads
repository-owned policy from `WORKFLOW.md` -- this repository's single
executable source of delivery policy -- polls Linear, and runs each eligible
issue in a deterministic local workspace.

```sh
go run ./cmd/symphony --workflow ./WORKFLOW.md
# Equivalent positional workflow-file form:
go run ./cmd/symphony ./WORKFLOW.md
```

## macOS repository services

Install one shared executable from this repository, then install a separate
LaunchAgent from each repository that owns a valid `WORKFLOW.md`:

```sh
cd ~/repos/symphony
./scripts/install

cd ~/repos/foo
symphony service install
```

The installer atomically updates `~/.local/bin/symphony`; it never copies a
binary into a managed repository or self-updates a running service. Each
repository service gets its own `.symphony/workspaces`, `.symphony/logs`, and
`.symphony/service/{status.json,stdout.log,stderr.log}`, while its LaunchAgent
is registered as `com.pmrrasmussen.symphony.<instance>` under
`~/Library/LaunchAgents`. See [the macOS repository-services guide](docs/macos-services.md)
for the multi-instance layout, credential references, migration from a manual
plist, and a troubleshooting-level raw plist example.

`service install` runs the normal workflow preflight and validates its
generated plist before changing launchd. Repeating it is a no-op when the
effective managed plist is unchanged; a changed managed plist is reloaded only
for that repository. It rejects duplicate workflow/status registrations and
never overwrites an unmarked LaunchAgent. Use `--workflow`, `--name`,
`--linear-api-key-file`, or `--github-token-file` for explicit non-default
configuration. `service status`, `service restart`, and `service uninstall`
select only the current repository’s managed instance.

If a repository already has a hand-authored Symphony LaunchAgent, run
`symphony service migrate` there once. That command is the only explicit way to
replace an unmarked plist: it adopts a legacy agent only when its label,
repository, workflow, executable, and runtime paths match this repository
exactly, refuses while any unaccounted-for Symphony job is still registered
with launchd, verifies the legacy service really unloaded before replacing it
so two schedulers never run, backs the old plist up under `.symphony/service`,
and restores it -- re-starting it only if it was running -- if installation or
bootstrap fails. Unrelated or ambiguous agents are left untouched with
concrete diagnostics.

Services pass credential file paths only, never credential values. The normal
Linear reference is `$SYMPHONY_LINEAR_API_KEY_FILE` in `WORKFLOW.md`, resolved
by default to `~/.config/symphony/linear-api-key`. For a GitHub-enabled
workflow using `$SYMPHONY_GITHUB_TOKEN_FILE`, the documented default is
`~/.config/symphony/github/<owner>-<repo>.token`; pass
`--github-token-file` to select another file. All credential files must be
owner-only.

## Canonical lifecycle

`Todo -> In Progress -> In Review <-> Rework -> Merging -> Done`. `Todo`,
`In Progress`, `Rework`, and `Merging` are active/dispatchable; `In Review` is
the single, fixed, human-controlled review state and is never dispatched;
`Done` and `Canceled` are terminal. When the coordinator dispatches a `Todo`
issue it moves it to `In Progress` itself, with the host Linear credential,
before the session starts (`tracker.provider.transitions.start`); this is
deterministic, idempotent, and fail-safe, so board-level observability never
depends on the agent. A Codex session then:

1. Implements and validates the change (the issue is already `In Progress`).
2. Publishes a structured pull request (`github_publish_pr`) and hands the
   issue to `In Review` once validated.
3. Resumes -- in the same worktree, branch, and pull request -- when a human
   moves the issue to `Rework`, and republishes to hand it back to
   `In Review`.
4. Is dispatched again, with only the bounded zero-argument `github_land_pr`
   tool, when a human moves the issue to `Merging`: that move is itself the
   approval to land. Pending checks wait without changing Linear state: the
   run ends there and Symphony itself redispatches landing after the
   configured `github.poll_interval_ms` (escalating toward
   `agent.max_retry_backoff_ms` while the gate stays unsettled), so a wait
   spends no further model turns. Any other hard gate returns the issue to `In Review`; a successful or
   already-completed merge reconciles the issue to `Done` and closes the
   landing tool for that run.

Four-agent operation allows up to four implementation/rework sessions to run
concurrently (`agent.max_concurrent_agents: 4`). Landing remains serialized
(`agent.max_concurrent_agents_by_state: {Merging: 1}`), while a delayed retry
never occupies a concurrency slot as it waits.

Dispatch itself is bounded by `agent.max_attempts` (default 5). `max_turns`
bounds the turns inside one run and `max_retry_backoff_ms` bounds the delay
between runs; `max_attempts` is the only bound on the *number* of runs, so an
issue that fails the same way every time — a corrupted worktree, a `before_run`
hook that always exits non-zero, a prompt template error, an unreachable agent
binary — stops instead of re-dispatching at the backoff ceiling for the
daemon's lifetime. On the last attempt Symphony abandons that dispatch: one
error-level `dispatch abandoned after max attempts` record
(`operation: dispatch_abandoned`) naming the classified failure reason and the
attempt count, and the claim and retry timer dropped. It deliberately makes no
tracker change — no transition and no comment — so the issue stays where a
human left it and a later poll may start a fresh, equally bounded episode;
that error record, not a silent state change, is the signal that the issue
needs a person rather than another retry. That also means abandonment is not
quarantine: an issue nobody acts on keeps starting new bounded episodes at the
poll interval, so `dispatch_abandoned` is a record to alert on rather than one
to let accumulate. A non-terminal landing wait is exempt, since it is not an
agent failure (see below), and so is losing the race for a contended
orchestrator slot: that is capacity contention rather than a dispatch
failure, so it leaves the attempt where it was and retries on a fixed,
poll-interval cadence instead of consuming `max_attempts` or the escalating
failure backoff.

See `WORKFLOW.md`'s prompt body for the full per-state start, implementation,
validation, review handoff, rework, landing, and completion playbook, and
[docs/architecture.md](docs/architecture.md) for the underlying trust model.

## Working with Symphony

1. Create one focused Linear issue in `Todo` (or move it to `In Progress`
   yourself), and work from an isolated Git worktree—not the primary
   checkout.
2. Keep the workflow policy in the repository. Configure
   `workspace.source_root` for the source repository; Symphony creates a
   separate agent workspace for each eligible issue.
3. Validate the narrow change first, then run broader checks when shared
   behavior changes. Review the generated workspace before keeping its work.
4. Publish a PR with **Why**, **What changed**, and **On Call** (via
   `github_publish_pr`, or manually when host-side publishing is not
   configured); merge only after required checks and review. Moving the
   issue to `Merging` dispatches `github_land_pr`, which lands the PR and
   moves the issue to **Done** automatically; without GitHub landing
   configured, move the Linear issue to **Done** manually after merging.
5. Use `--dry-run` before any live run. Live smoke tests are manual and must
   use dedicated Symphony test artifacts, never Dagligvare-app.

For runtime configuration and operational details, use
[WORKFLOW.example.md](WORKFLOW.example.md) and
[docs/architecture.md](docs/architecture.md).

Run a full-lifecycle local preflight against the example without live
credentials:

```sh
SYMPHONY_LINEAR_API_KEY_FILE=/path/to/a/mode-600-key-file \
  go run ./cmd/symphony --dry-run ./WORKFLOW.example.md
```

`--dry-run` emits a structured result for workflow parsing, tracker selection,
workspace and log roots, hook syntax, executable availability, and a synthetic
scheduler lifecycle. It does not contact Linear, execute hooks, start an agent
session, or create configured logs or workspaces. With `agent.backend: claude`
it additionally runs the CLI's own read-only `claude auth status` locally, as
the `agent_authentication` check described below. A missing future root is a
warning; an
invalid boundary is a failure and exits non-zero. The referenced file is read
only to validate required configuration and is never sent anywhere during
preflight.

Live Linear and agent smoke testing is always opt-in and uses a dedicated
disposable project; see [the live smoke profile](docs/live-smoke.md).
Never run a Symphony smoke test against Dagligvare-app or any of its Linear
workspace, team, or projects.

Linear configuration uses canonical `tracker.provider.project_slug_id`.
Legacy `project_slug` remains supported during its documented deprecation and
emits a value-free migration warning; configuring both names is rejected.
For credentials, prefer
`tracker.provider.api_key_file: $SYMPHONY_LINEAR_API_KEY_FILE`, where the
environment value is an absolute path to a mode-600 file outside the
repository. `tracker.provider.api_key: $LINEAR_API_KEY` remains supported when
the deployment injects the credential value directly. A literal trusted local
`api_key_file` path is also supported for non-versioned workflow files.

`WORKFLOW.md` front matter is validated for the supported core fields while
unknown extension fields are preserved. Changes are reloaded for future work;
an invalid replacement keeps the last valid configuration. Reload detection
includes referenced environment values and `api_key_file` contents, so those
inputs can change without touching `WORKFLOW.md`. Environment-backed fields use
exact `$VARNAME` syntax; braced or compound forms are rejected instead of being
treated as literal paths or secrets. Inline and file-backed API keys are
whitespace-trimmed before use and are never included in reload logs.

Each successful reload publishes one complete settings snapshot. Settings
reads that start afterward use the new polling, state, concurrency, hook,
workspace, and Codex policy. An already-started agent session keeps the
backend, command, sandbox, and timeout values captured for its launch, so a
changed `agent.backend` applies only to sessions started later; new concurrency
limits govern later admissions, while current state and stall policies continue
to be applied by reconciliation. `--logs-root` selects the process log
destination at startup and is not a reloadable `WORKFLOW.md` field.

`agent.backend` selects the agent runtime new sessions start on. It defaults
to `codex`; `claude` is the only other accepted value, and validation is
fail-closed: any other value rejects the whole configuration candidate rather
than falling back to the default. Only the selected backend's launch contract
has to be complete. The `codex:` block supplies command, sandbox, and timeout
budgets for `codex` and the `claude:` block supplies them for `claude`, so a
workflow that never starts a Codex session is not rejected for an absent
`codex:` block, and vice versa.

`codex.thread_sandbox` sets the session's sandbox mode and the optional
`codex.turn_sandbox_policy` object is validated against the Codex
`SandboxPolicy` schema and then forwarded verbatim as every turn's sandbox
policy. Codex treats that policy as an override of `thread_sandbox` for the
turn and all later turns, so the two must agree; a policy that requests write
authority the thread mode does not have is rejected at load. `writableRoots` is
rejected outright -- Symphony supplies the narrowed Git roots itself -- and
unknown keys inside the policy are rejected so a typo such as `networkAcces`
cannot silently leave the setting off. This repository configures the policy a
worker needs to run its own validation:

```yaml
codex:
  thread_sandbox: workspace-write
  turn_sandbox_policy:
    type: workspaceWrite
    networkAccess: true
```

`workspaceWrite` bounds **writes** to the issue's own Git worktree plus only
the two narrow Git metadata roots Symphony grants so a detached-HEAD commit can
succeed. It does not restrict **reads**: a worker can read any file the user
running Symphony can, including credential files outside the worktree. That is
true of Codex's sandbox modes generally -- the same reads succeed under
`read-only` -- and is not a consequence of this setting.

`networkAccess: true` grants **unrestricted outbound network access**, not
merely the ability to bind a local socket. Codex exposes a single boolean here
and nothing narrower is expressible. Repository tests that bind loopback
listeners -- Go `httptest` servers, for example -- are the reason this
repository enables it, not the limit of what it permits. Omit
`turn_sandbox_policy` to leave outbound access denied.

Host-owned Linear and GitHub secrets are absent from the Codex child
**environment**: Symphony strips them before launch by reserved name, by
configured name, by configured value, and by the credential the run's bound
providers hold, so no host credential is handed to the worker as a variable, and
all publishing authority stays with the host. The same filter applies to both
backends and to `WORKFLOW.md` hooks, which run inside the issue worktree and can
invoke what the agent committed there; a hook receives only
`SYMPHONY_ISSUE_ID` and `SYMPHONY_ISSUE_IDENTIFIER` on top of the filtered
environment, and runs under `sh -c`, so it resolves commands from the daemon's
own `PATH` rather than the operator's login profile.
[docs/architecture.md](docs/architecture.md) describes the filter in full.
That guarantee covers the environment, not reachability and not files on disk.
With both local reads and outbound network available to the worker, what
protects host credentials from exfiltration is that no untrusted input reaches
the worker -- the issue text
and repository content it acts on are operator-owned -- rather than the sandbox
itself.

Prompt templates use strict, lowercase variables: `issue` (for example
`{{.issue.identifier}}`) and `attempt` (nil on the first run, then a 1-based
retry/continuation number). Template errors fail only that run attempt.
Relative workspace and log paths are resolved from the workflow file; omitted
`workspace.root` defaults to the system temporary directory's
`symphony_workspaces` path.

See [docs/architecture.md](docs/architecture.md), the
[Linear tracker profile](docs/linear-tracker.md), the
[workspace ownership and recovery guide](docs/completion-markers.md), the
[observability guide](docs/observability.md), the
[dogfooding operator guide](docs/dogfooding.md), and
[WORKFLOW.example.md](WORKFLOW.example.md).

By default the structured log at `--logs-root` (`.symphony/logs/symphony.jsonl`)
stays concise. Pass `--log-level debug` for actionable detail on an
apparently idle run: categorized poll admission/rejection summaries, safe
Codex tool/item lifecycle records, and heartbeat/stall records naming the
outstanding operation. See [docs/observability.md](docs/observability.md) for
`tail`/`jq` examples and the full level and redaction contract.

For a local TUI or another operator client, optionally pass `--status-file`
with a separate path for each process. Managed repository services use
`.symphony/service/status.json`; a manually run process may select another
owner-only path. It is an atomically replaced, versioned runtime snapshot, not
a liveness authority; see [docs/runtime-status.md](docs/runtime-status.md).

Use `symphony tui` for a read-only terminal view across convention-matching
local Symphony LaunchAgents. It reads launchd observation, each instance's
safe status snapshot, normalized effective configuration, validation findings,
and a bounded redacted structured-log tail. In a terminal it reads single
keypresses, re-reads local observations every five seconds, and refreshes on
demand with `r`; redirected output instead prints plain frames driven by
line-buffered input and never polls. The dashboard fits itself to the window:
it shows the instance list beside the selected instance from a hundred and
twenty columns, drops the two numeric columns below eighty columns, and asks
for a larger window below sixty by fourteen. A detail page taller than the
window scrolls with `ctrl+d`/`ctrl+u` and `g`/`G`, showing its position as
`13-24 of 24`; the alternate screen has no scrollback, so without that the rows
past the window would be unreachable rather than merely off screen. An instance
list longer than the window keeps the selected row visible and reports the rest
as `+N more`. `NO_COLOR` gives up hue while keeping
the dashboard, since each state is drawn as a shape and a word as well as a
color. The command does not start, stop, pause,
or connect to a Symphony daemon or
remote Linear, GitHub, or Codex service. It requires no central registry; see
[the macOS repository-services guide](docs/macos-services.md) for the
multi-repository operator workflow.

For ongoing repository development, configure `workspace.source_root: .`.
Symphony then creates one detached Git worktree per issue beneath
`workspace.root`; each new worktree starts from a freshly fetched
`origin/main`, while an existing issue worktree is reused at its current
commit. The original checkout is never used as an agent workspace, and its
branch, index, and working tree are not changed during preparation.
Create focused issues in the configured Linear project and move them to `Todo`
or `In Progress` to make them eligible. Symphony keeps active issues eligible
across restarts and bounded retries. Invalid or missing ownership state beside
an existing workspace fails closed; follow the [workspace recovery
procedure](docs/completion-markers.md) before redispatch.
Review each worktree's changes before merging or cherry-picking them into your
development branch. Terminal cleanup preserves worktrees with uncommitted or
untracked changes rather than deleting work that needs review, and preserves a
newly committed worktree too unless Symphony itself can verify that exact
commit as the merged head of the issue's pull request in the configured GitHub
repository. That one verified case is how a completed, landed issue's worktree
is removed instead of accumulating; anything unpublished, rewritten while
merging, or unverifiable is kept for review.

To let a Codex session hand an issue off safely, optionally configure
`tracker.provider.handoff_state` (and, if useful, a fixed
`handoff_comment_template`). This enables a session-bound compatibility tool,
not general Linear or GraphQL access; see the Linear tracker profile for its
strict scope.

To let a Codex session capture meaningful work that falls outside its current
issue, optionally configure `tracker.provider.followup_issue_creation: true`.
This enables a session-bound `create_followup_issue` tool, not general Linear
or GraphQL access. It can only create a parentless issue in the active issue's
already configured project and team, always in `Backlog`, and requires a title,
description, and acceptance criteria. Its only optional relationship is
bounded to the originating issue: `related`, or `blocked_by_current` when the
follow-up depends on the current work. `Backlog` is rejected as an active state
while this capability is enabled, so Symphony cannot dispatch the follow-up
until a human promotes it to an eligible state such as `Todo`. The legacy
`child_issue_creation` setting is accepted as a warning-emitting alias for
migration, but it enables these follow-up semantics and the old
`create_child_issue` tool is no longer advertised. See the
[Linear tracker profile](docs/linear-tracker.md) for the exact scope.

An optional host-side GitHub integration can publish a completed, clean
worktree and create or reuse its pull request. It requires the Linear handoff
policy above and a fixed repository configuration:

```yaml
github:
  owner: pmrrasmussen
  repository: symphony
  base_branch: main
  token_file: $SYMPHONY_GITHUB_TOKEN_FILE
  # Alternatively: token: $SYMPHONY_GITHUB_TOKEN
  poll_interval_ms: 30000
```

Use a fine-grained token restricted to that single repository, granting only
the pull request, checks, contents, and review permissions the integration
needs. Store it in a mode-600 file outside the repository and reference that
path with `$SYMPHONY_GITHUB_TOKEN_FILE`; the token is never committed to the
repository. Symphony removes the resolved token (including inherited
environment values containing it) from the Codex child environment, so it is
never exposed to the child process. The `github_publish_pr` dynamic tool
accepts no repository, branch, issue, or credential input, only bounded `why`,
`what_changed`, and `on_call` structured handoff fields: it verifies the
worktree's credential-free GitHub origin and committed clean changes, pushes
`symphony/<lowercase-issue-identifier>`, creates or reuses that branch's pull
request with a deterministic `Why`/`What changed`/`On Call` body plus the
bound Linear issue URL, links the PR, and moves only the active issue to the
configured review state. Repeat publication with the same structured fields
leaves an already-correct body untouched; changed fields update it in place. A
pull request that was merged is irrecoverable and rejected; one that was
closed without merging is reopened. A companion read-only `github_pr_context`
dynamic tool, bound to the same issue, repository, branch, and pull request,
reports bounded check status, effective review state, and redacted
comment/review excerpts and unresolved review-thread counts, with no
repository, issue, branch, or pull request selection of its own. A confirmed
human merge moves the linked issue to `Done`; closing without merge leaves it
in review and emits a warning. Either outcome ends that pull request's
polling, and so does the issue reaching a terminal tracker state, so a process
that runs for weeks does not keep requesting every pull request it ever
published; a pull request that is still open on an active issue keeps being
polled however long it takes to merge. Invalid or incomplete GitHub settings
disable both tools, preserving the manual workflow. In host-publish mode, workers
create local commits but do not use `gh` or `git push`; they invoke these
host capabilities instead. Without them, Symphony tells workers that PR
delivery is unavailable and reports the missing configuration rather than
asking them to publish directly.

Symphony can also land an already-approved pull request itself once
`github.merge_state`, a bounded `github.merge_method` (`merge`, `squash`, or
`rebase`; defaults to `merge`), and a non-empty `github.required_checks` list
are configured:

```yaml
github:
  # ...owner/repository/base_branch/token as above...
  merge_state: Merging
  merge_method: merge
  required_checks: [ci/build, ci/test]
```

Unlike the rest of the `github:` block, an invalid `merge_state`,
`merge_method`, or `required_checks` value rejects the whole workflow instead
of silently disabling the feature, the same fail-closed treatment as
`tracker.provider.transitions`. `merge_state` must be an
`active_state` and differ from `handoff_state` and every terminal state
(`Merging` in the canonical lifecycle above): a session only receives the
tool once actually dispatched for an issue currently in that exact state. A
session bound to an issue currently in the exact configured `merge_state` receives a
zero-argument `github_land_pr` tool: it re-verifies the worktree, branch,
pull request, required checks, effective review state (moving the issue to
`merge_state` is itself the human
approval; no separate approving review is required), unresolved review
threads, mergeability, and current base immediately before merging, and
transitions only the bound issue to `Done` on success. Pending checks wait
without changing Linear state, and a wait is settled host-side rather than by
the model: the run ends immediately, releases its concurrency slot, and the
coordinator schedules one delayed landing redispatch, so a pending gate can
never consume `agent.max_turns` or an agent-exhaustion retry. That redispatch
starts at `github.poll_interval_ms` and escalates with each consecutive wait
toward `agent.max_retry_backoff_ms`, so a gate that never settles — including a
`required_checks` name that matches no GitHub job — backs off to a slow poll
with a climbing `wait_attempt` in the log and snapshot instead of respawning a
session every interval. `agent.max_attempts` does not apply here either — the
wait leaves the attempt where it was, so it can never reach that ceiling.
Symphony never gives up on it by itself: returning the issue to review remains
the landing capability's own bounded fallback.

A `required_checks` name that never appears in either the combined-status or
check-run table for the evaluated commit — a typo, a renamed CI job, or a
workflow whose job is skipped on this path — cannot be told apart from a
genuinely slow check on a single snapshot, so it waits rather than refuses. It
is not silent, though: the landing wait reason distinguishes the two cases,
`required checks have not reported: <names>` for a purely-missing check versus
`required checks are pending` when any of the configured names has actually
been seen pending, so the log and `RetrySnapshot.Reason` in the status file
name the specific failure mode. And once `wait_attempt` for an issue reaches
the point where the redispatch delay above has climbed to (and saturated at)
`agent.max_retry_backoff_ms`, the coordinator raises that one `landing wait
retry scheduled` log record to Warn — once, not on every subsequent wait — so
a landing stuck behind a check name that will never report is greppable
instead of visible only to someone watching the TUI or status file. Because a
mistyped `required_checks` name still holds the issue's `Merging` slot
(`Merging: 1` in the canonical lifecycle), and that slot is shared across the
whole project, get the exact check/status names right before relying on this
feature: mismatch one and every subsequent landing queues behind it.

A terminal result (merged, already merged, or a completed reconciliation) ends
the run the same way and closes `github_land_pr` for it, so a normal landing
invokes the capability exactly once. With `github.update_stale_branch: true`, one
clean stale-base update also waits for checks on its new head; any other hard
gate falls back to the configured `merge_state -> In Review` transition exactly
once. Duplicate landing calls, and a GitHub merge that succeeds despite a failed
Linear completion, remain reconciled idempotently as a recovery path rather than
merging or transitioning twice. Symphony never merges a pull request outside this narrow, explicitly
configured capability.

## The Claude Code agent backend

`agent.backend: claude` runs each turn on the Claude Code CLI instead of the
Codex app-server. The `claude:` block is all of its configuration:

```yaml
agent:
  backend: claude
claude:
  command: claude
  # Optional. Omit it to let the CLI select its own model.
  model: sonnet
  turn_timeout_ms: 3600000
  stall_timeout_ms: 300000
```

`command` defaults to `claude`, `turn_timeout_ms` to 3600000, and
`stall_timeout_ms` to 300000; `model` is unset by default, and the CLI is then
given no `--model` flag at all. An unknown key inside `claude:` is refused
rather than ignored, so a misspelled launch field cannot leave a default
silently in place. There is no read or start timeout budget: a turn is a single
process that exits when the turn ends, so there is no steady-state round trip
to bound.

`command` is **argv, not a shell command**: it is split on whitespace and its
first field is executed directly, so there is no shell, no quoting, no variable
expansion, and no operators or redirection. That is deliberate -- the inline
`--settings` payload described below is JSON that a shell would word-split -- but
it means `claude.command` and `codex.command` are not the same kind of field:
`codex.command` runs through `bash -lc`, so quoting that works there breaks
here. Note also that `--dry-run` validates both fields with `sh -n`, a shell
syntax check, which is a loose superset for this backend: it accepts quoting
that the Claude launcher passes through as literal argument text rather than
interpreting.

The block deliberately has no approval-policy, sandbox-mode, or sandbox-policy
counterpart to `codex:`. What a Codex operator configures per repository is
instead fixed by Symphony and cannot be widened from `WORKFLOW.md`:

* `--print --output-format stream-json --verbose` -- the turn is a
  newline-delimited JSON stream on stdout, and `stream-json` without
  `--verbose` is a hard error rather than a downgrade.
* `--permission-mode dontAsk` -- the only fail-closed non-interactive mode: it
  refuses anything not explicitly allowed and tells the model instead of
  prompting. The prompt is written to stdin, so a mode that can block on stdin
  is unusable here, and `bypassPermissions` is the opposite of fail-closed.
* `--tools` and `--allowedTools` restricted to `Bash`, `Edit`, `Glob`, `Grep`,
  `Read`, and `Write`, plus `--disallowedTools WebFetch,WebSearch`. `--tools`
  is what removes a tool from the surface; a permission allowlist alone still
  advertises it.
* `--strict-mcp-config` -- required, not hygiene, and load-bearing in both
  directions. It confines the session to the MCP configuration on its own
  command line, so a session with no `--mcp-config` gets no MCP server at all
  and a session with one can reach nothing but Symphony's own endpoint. Without
  it the child additionally inherits the operator's own user-level MCP servers,
  credentials included.
* `--mcp-config`, inline, **only** for a session whose capability registry
  advertises something -- which no workflow can configure yet. It names exactly
  one `http` server, Symphony's private loopback capability endpoint, and
  carries its bearer token as an `${SYMPHONY_MCP_TOKEN}` reference the CLI
  expands from the child's environment, so the credential never appears in a
  command line that on Linux any local account can read. Each advertised
  capability is then added to `--tools` and `--allowedTools` as an explicit
  `mcp__symphony__<capability>`, never as an `mcp__symphony__*` glob: the init
  echo below is checked for set equality, and a glob would let the CLI advertise
  a capability Symphony never asked for and still pass. Those two flags govern
  what the *CLI* offers the model; what is *reachable* is bounded separately, by
  the endpoint refusing any capability its registry does not advertise -- see
  limit 5.
* `--setting-sources ""` -- excludes user, project, and local settings. The
  worktree is a checkout of a repository that may ship its own
  `.claude/settings.json`, `CLAUDE.md`, skills, plugins, and hooks, and hooks
  run arbitrary commands, so leaving discovery enabled would let repository
  content widen this boundary.
* `--settings` with a sandbox and permission payload marshaled from Go structs
  and passed inline. The CLI silently ignores a settings payload it cannot
  parse, so a hand-assembled string with one typo -- or a file that is
  unreadable or half-written at launch -- would leave the session running with
  no policy at all and no diagnostic.

Every one of those is re-applied on **every** turn, because the CLI restores
none of them on `--resume`. `claude --print` runs one turn and exits, so there
is no long-lived process: a continuation spawns a new process, and Symphony
assigns the session ID itself (`--session-id <uuid>`) rather than reading one
back, so the next turn can resume with `--resume`. Cancelling between turns is
therefore a no-op -- no process exists -- while a running turn is killed by
process group, so the CLI's own child commands go with it.

The sandbox in that payload is `enabled` with `failIfUnavailable: true` and
`allowUnsandboxedCommands: false`; `filesystem.allowWrite` is the issue's own
worktree plus exactly the two narrow Git metadata roots Symphony grants so a
detached-HEAD commit can succeed -- the same grant the Codex profile makes --
and `network.allowedDomains` is `["*"]`.

`failIfUnavailable: true` is not tidiness. Verified against `claude` 2.1.245 by
running the same broken-sandbox condition both ways. Without the flag, the CLI
announces "Sandboxing is disabled for the rest of this session!" inside a tool
result and then keeps running unconfined: a write outside `allowWrite`
succeeded, and the turn still reported exit code 0 with no error. With the flag
set, every sandboxed command failed instead and no write happened.

Note what that means for observability: a sandbox that cannot initialize
surfaces as repeated **failed** `Bash` item records, not as a failed turn. The
turn itself can still complete, because the degradation is reported only in
tool-result text, which Symphony deliberately does not parse.

What was verified to hold, on the same version: Bash writes are confined --
attempts to write to `$HOME` and to `$TMPDIR` were refused with "operation not
permitted" and created nothing -- and per-domain network control works in both
directions, an allowed domain succeeding and a denied one failing.

Five limits of that boundary, stated rather than implied:

1. The sandbox governs `Bash` and its children, and Bash writes were verified
   confined. `Edit` and `Write` are **not** sandboxed and have no path
   restriction beyond existing: the rendered payload is
   `permissions.allow: ["Bash","Edit","Glob","Grep","Read","Write"]` with
   `defaultMode: dontAsk` -- bare tool names, which decide whether a tool
   exists, not where it may write. Whether the file-editing tools themselves
   refuse an absolute path outside the worktree was not verified, so it is not
   claimed here. Nor would Symphony detect such a write: the post-run Git
   integrity check re-verifies only the source repository's non-`symphony/*`
   branch heads and its primary index, so it catches a write into the source
   checkout's Git state and nothing else. A write to a path outside the
   repository entirely is neither confined nor observed.
2. Reads are **not** confined, exactly as for Codex: a worker can read any file
   the user running Symphony can, including credential files outside the
   worktree.
3. `network.allowedDomains: ["*"]` is **unrestricted outbound network access**,
   matching the Codex profile's deliberate `networkAccess: true`. Per-domain
   control exists and works; this configuration simply does not use it to
   restrict anything.
4. The CLI's `system`/`init` event reports the working directory, tool surface,
   permission mode, and attached MCP servers. Symphony checks all of them
   against the contract that turn was launched under: the tool surface and the
   permission mode exactly as asked, exactly the MCP servers asked for -- none
   at all for a session with no capability, and Symphony's own endpoint
   reporting itself `connected` rather than `pending` for a session with one --
   and a reported working directory that resolves to this issue's workspace. A
   mismatch, or no init event at all, fails the turn closed. Requiring
   `connected` is what makes it fail closed: a `pending` or `failed` endpoint is
   a session whose capability tools are advertised, so the model will call them,
   while every call returns a transport failure. The event does **not** report
   sandbox state, so the sandbox's own status is not observable in the stream.
5. When a session has a capability endpoint, **the child holds its bearer token**
   -- it is in the environment `Bash` runs with, and loopback is inside the
   sandbox, since `network.allowedDomains` is `["*"]`. So the tool surface the
   CLI enforces is not by itself a bound on what the child can invoke: a shell
   command can read the URL out of its own `/proc/self/cmdline` and the token out
   of its own environment and call the endpoint directly, and no `tool_use`
   record appears for such a call anywhere. Two things bound it. The endpoint
   refuses any capability the session's registry does not advertise, so a
   directly addressed call can only reach what the model was already permitted
   to call and therefore grants no authority it did not already have. And every
   provider re-validates its own preconditions immediately before mutating
   anything -- a landing re-checks the tracker state, a follow-up re-checks that
   creation is enabled. What remains is an observability gap, stated rather than
   claimed away: a capability invoked this way runs unrecorded.

So the operative boundary has nearly the same shape as the Codex one: Bash
writes confined, no host Linear or GitHub credential in the child environment
(stripped by the same filter as Codex, described in
[docs/architecture.md](docs/architecture.md)), but local reads
and outbound network both available -- and, unlike the Codex profile, the
file-editing tools sitting outside the sandbox entirely, per limit 1. What
protects host credentials from exfiltration is that no untrusted input reaches
the worker, not the sandbox.

A Claude workflow may enable Symphony's session capabilities --
`tracker.provider.handoff_state`, `tracker.provider.followup_issue_creation`,
and a **configured and enabled** GitHub integration -- so the canonical
`In Review`/`Merging` lifecycle above runs on either backend. They reach the
session over the private loopback MCP endpoint, so the CLI serves each one as
`mcp__symphony__<tool>` rather than `<tool>`. Symphony's host-generated delivery
instructions render those names and state the mapping rule, which is what keeps
one repository-owned `WORKFLOW.md` -- whose prompt body names the bare tools --
correct under both backends with no per-backend prompt. As a fail-closed
cross-check, a launch whose prompt promises host-side publish while the session's
own registry advertises no `github_publish_pr` is refused rather than run.

Two narrow combinations are still **rejected at load**, for `claude` only.

An **enabled GitHub integration without `handoff_state`**. With follow-up issues
off, no Linear handoff session is prepared, so no GitHub session is either and
the enabled integration grants nothing. With follow-up issues on the outcome is
worse rather than better: `followup_issue_creation` alone satisfies
`LinearSessionCapabilityEnabled`, so a handoff session *is* prepared, a GitHub
session is built on top of it, and `github_publish_pr` and `github_pr_context`
are advertised -- while the delivery guidance branches on the handoff state and
tells the run that publishing is unavailable. A worker that trusts its tool list
over the prompt reaches `LinkAndHandoff` with no target state, which comments the
pull request onto the issue and then attempts a transition to no state at all.
The refusal arrives after the pull request already exists, which is why this is
refused at load rather than left to the launch guard.

A **`handoff_state` with neither an enabled GitHub integration nor
`followup_issue_creation`**: the handoff object is prepared and nothing
model-facing uses it.

Both stay accepted under `codex`, where they behave identically -- the
advertisement is the same registry's -- because they always have been and
narrowing them would reject workflows already in the field. The
prompt/advertisement mismatch described above is pre-existing under `codex` and
is not addressed here. Under `claude` they are refused so that a session
launched with no MCP server at all always means "this workflow configures no
capability". A `github:` block that does not resolve (an unreadable
`token_file`, say) leaves the integration disabled, exactly as it does under
`codex`, so it configures nothing and reaches neither rule.

`--dry-run` adds one check for this backend, `agent_authentication`: it runs
`claude auth status` and reads only the `loggedIn` boolean. That command also
reports the operator's email, organization, and subscription, none of which is
read or logged. The check exists because an unauthenticated CLI otherwise
surfaces only at dispatch, where it looks like a finished turn rather than a
setup problem -- the CLI reports an authentication failure as a result event
with `is_error` set. A multi-word `claude.command` is a wrapper or a test stub
with no reliable way to be asked for status, so the check does not probe it and
does not fail on it: a pass is then evidence of nothing. The probe is bounded at
five seconds -- every other preflight probe is a `sh -n` syntax check, a `PATH`
lookup, or a `stat`, so this is the one call that runs a foreign program, and a
CLI blocked on a keychain prompt or a token refresh must fail the check rather
than leave `--dry-run` waiting.

**Operator prerequisite:** the user the Symphony process runs as must already be
logged in to the Claude Code CLI. Symphony passes it no credential and performs
no login; the CLI authenticates through that user's own stored login under its
home directory, which is why the child environment is inherited apart from the
host secrets Symphony strips.

**Disclosure -- the CLI keeps its own transcript on disk.** Claude Code persists
the full session transcript, including rendered prompts, issue descriptions, and
tool output, to `~/.claude/projects/<cwd-slug>/<session-id>.jsonl`. That path is
outside the worktree, is not removed by workspace cleanup, and holds exactly the
content [docs/observability.md](docs/observability.md) promises Symphony never
writes -- a promise that covers Symphony's own structured log, not this file.
`--resume` reads that transcript, so a multi-turn run cannot work without it and
it cannot be turned off. Redirecting `CLAUDE_CONFIG_DIR` relocates it but breaks
subscription authentication (verified), so Symphony leaves it where the CLI puts
it and documents it here instead.

## Operator prerequisites for the canonical lifecycle

`WORKFLOW.md` is fully configured for the canonical lifecycle, but going live
against this repository's real Linear team requires the following manual,
human-gated steps; none of them can be performed by Symphony, by this MCP
integration (which only reads and lists issue statuses), or by an automated
agent:

1. **Create the `Rework` and `Merging` Started states** in the Linear team
   used by `tracker.provider.project_slug_id`. Until both states exist,
   issues can reach only `Todo`, `In Progress`, and `In Review`; `--dry-run`
   does not contact Linear and cannot detect a missing remote state, so this
   is a live-run precondition, not something automated validation checks.
2. **Provision the repository-scoped GitHub token file** referenced by
   `$SYMPHONY_GITHUB_TOKEN_FILE`: a fine-grained personal access token
   restricted to exactly this repository, granting only the pull request,
   checks, contents, and review permissions described above, saved to a
   mode-600 file outside the repository.
3. **Confirm the configured `github.required_checks` names** match what
   GitHub actually reports for this repository. `WORKFLOW.md` uses this
   repository's current CI job names (`scripts/check format`,
   `scripts/check test`, `scripts/check vet`, `scripts/check race`, from
   `.github/workflows/ci.yml`); update both files together if CI job names
   change.
4. **Disable the tracker's native PR-to-status automation** for the managed
   team and project. Symphony owns every managed issue's transitions; Linear's
   built-in "move to In Progress when the linked PR opens" automation is an
   external writer that races the host review handoff and can flap an issue
   `In Review -> In Progress` moments after Symphony hands it off (PMR-63).
   Turn off the PR-linked status automations in Linear → Settings → the team →
   GitHub integration / workflow automations; this is verifiable only in the
   Linear UI. If left enabled, Symphony does not silently re-dispatch the
   reverted issue — it logs a warn-level `external tracker state change
   observed` (`operation: external_reversion`) record naming the from/to
   states — but it does not currently re-assert the handoff automatically. The
   expected human review decisions out of `In Review` are not warnings: with
   this repository's lifecycle configured, an approval to land
   (`In Review -> Merging`, `operation: review_approved`) and a
   changes-requested move (`In Review -> Rework`,
   `operation: rework_requested`) are logged at info level as
   `human review state change observed`. Any other destination — including one
   the configured lifecycle cannot name unambiguously — keeps the warning. See
   the
   [Linear tracker profile](docs/linear-tracker.md) and the
   [observability guide](docs/observability.md).

Until all four are complete, Symphony keeps running safely: four-agent
capacity and the `Todo`/`In Progress`/review-handoff path already work today,
an issue simply never reaches `Rework` or `Merging` in Linear (Linear itself
has no such state to move it to), `github_land_pr` is never advertised, and
completion continues through the existing human-merge fallback described
above.
