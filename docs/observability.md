# Observability: log levels, poll summaries, and stall records

Symphony writes one structured JSON log line per event to
`<logs-root>/symphony.jsonl` (`--logs-root`, default `.symphony/logs`). This
document covers the two log levels, what each adds, and how to follow the
log during a live run without reading the agent's own transcript — the raw
Codex rollout, or the Claude Code CLI's session JSONL — which can contain
prompts, issue content, tool arguments, and other sensitive data.

For a managed macOS repository service, this file is
`.symphony/logs/symphony.jsonl`, separate from
`.symphony/service/status.json`. The log is redacted event history, not an
authoritative current-state record; status is an observational snapshot and
launchd/process state determines liveness. See
[macOS repository services](macos-services.md) for the operator model.

## Log levels

`--log-level info` (the default) is concise: issue claim through completion
(or cancellation/retry), agent session start, token usage, non-empty rate
limit snapshots, and stderr diagnostics. It never floods with the app-server's
generic protocol notifications.

Every Linear state change Symphony performs — all of them host-side, since no
model-invokable tool can write the tracker — also logs one info-level
`"msg":"Linear transition"` record so a tracker edge is reconstructable from
the log alone. Each carries `operation` (`start_transition` for the
coordinator's dispatch-time move; `handoff` for the host review handoff;
`landing_refused`, `landing_completed`, `merge_reconciled`, and
`review_completed` for the GitHub landing edges; `transition` for a host-side
move applied through the tracker adapter itself), the `from_state` and
`to_state` state NAMES, and the issue (`issue_id`/`issue_identifier`). An
idempotent no-op (the issue is already in the target state) instead logs a
debug-level `"msg":"Linear transition skipped"` with the same fields, so a
skip never appears as a state change. These records are redaction-safe: state
names and issue identifiers only — never a rendered comment, issue
description, or credential. (Agent turn token accounting is tracked
separately.)

Symphony also logs state changes it did **not** perform. The poll loop
remembers each issue it drove into the review `handoff_state`, and if such an
issue later reappears as an active candidate it logs exactly one record for
that change. `handoff_state` is human-controlled and Symphony has no writer out
of it, so every such change comes from outside Symphony — but only some of them
are a fault, and the `operation` field says which:

* `operation: review_approved` — the handoff state -> `github.merge_state`
  (`In Review -> Merging`). Moving the issue there is itself the human
  approval to land, so this is normal operation, logged at **info** level with
  `"msg":"human review state change observed"`. The edge is recognized *by*
  `github.merge_state`: with landing unconfigured there is no merge state to
  match, so the same move is unnameable and warns instead.
* `operation: rework_requested` — the handoff state -> the lifecycle's rework
  state (`In Review -> Rework`), the human review decision that sends the work
  back for changes. Also **info**, with the same message. Symphony names the
  rework state by elimination: `tracker.provider.transitions.start` enumerates
  the pre-review implementation states (the canonical `Todo -> In Progress`
  edge) and `github.merge_state` is the landing authorization, so removing both
  from `active_states` leaves the states only a human review decision moves an
  issue into. That naming is trusted **only when exactly one state remains** —
  canonically `Rework`. Configure a second unaccounted-for active state (a
  parked `Blocked`, a dispatchable `Backlog`, or a dispatch entry state no
  start edge names) and nothing qualifies: Symphony will not guess which of
  them is the rework state, so every such change warns instead.
* `operation: external_reversion` — anything else: the handoff was contradicted
  by reactivating handed-off work as though implementation had not happened
  (typically the tracker's native GitHub PR automation flapping
  `In Review -> In Progress`; PMR-63), or the destination is one the configured
  lifecycle cannot name (no start policy, or two or more remaining candidates as
  above). This one keeps its actionable **warn**-level
  `"msg":"external tracker state change observed"` record. The warning is the
  default on purpose: an expected-change record for a state Symphony merely
  failed to recognize would hide exactly the fault this record exists to
  surface.

Every one of the three carries the `from_state` (the handoff state) and
`to_state` (the state it was moved to) NAMES, the
`issue_id`/`issue_identifier`, and `since_handoff_ms` (elapsed time since the
handoff). Each is logged once per change and, like every transition record, is
redaction-safe. Symphony does not itself re-assert a reverted handoff — the
operator mitigation is to disable the tracker's native PR-to-status automation
(see the [Linear tracker profile](linear-tracker.md)); automatically
re-asserting the handoff without overriding a legitimate human reactivation is
a deferred follow-up.

Every `operation` value — the performed edges above and these three observed
ones — comes from one bounded vocabulary of fixed literals
(`internal/observability`), so a log query can rely on the field's values
instead of matching per-call-site strings.

Landing decisions Symphony settles for the agent are visible at info level
too, so a pending GitHub gate is never mistaken for an agent problem. A
non-terminal landing wait logs `"msg":"agent landing waiting"` with
`operation: landing_waiting` and the bounded, host-generated `reason`
(`required checks are pending`, `github has not yet computed mergeability`, …),
followed by `"msg":"landing wait retry scheduled"` (same `operation`, plus
`attempt`, `wait_attempt`, and `delay_ms`; it is logged only once the retry was
actually armed) and the ordinary `"msg":"agent retry scheduled"` record carrying
`retry_kind: landing`. A landing retry is always identified by that
`retry_kind`; its `reason` is `landing_waiting`, or `landing_slot_unavailable`
when the redispatch had to queue behind the state's concurrency limit. The
`wait_attempt` count is the "this landing is stuck" signal — the agent `attempt`
deliberately stays put for a non-failure, while consecutive waits escalate
`delay_ms` from `github.poll_interval_ms` toward `agent.max_retry_backoff_ms`
and also appear as `wait_attempt` in the coordinator snapshot's `retrying`
entries. The run itself finishes as
`"status":"waiting"` in `"msg":"agent logical run finished"` — deliberately
distinct from the `blocked`/`failed` statuses and from an agent failure's
warn-level `"msg":"agent run retry scheduled"` (`reason: turn_limit_exhausted`
and friends). A terminal landing logs `"msg":"agent landing resolved"` with
`operation: landing_resolved`, and the hard-gate fallback keeps its existing
`"msg":"Linear transition"` record with `operation: landing_refused`. Together
these four records distinguish waiting, the delayed retry, a hard refusal, and
an agent failure without reading the Codex rollout.

At startup, the info log also records `startup credential configuration` with
`linear_credentials_configured` and `github_credentials_configured` booleans.
They confirm that the required Linear credential and any configured GitHub
credential were resolved from the workflow; they do not verify remote
authentication, and neither credential value nor its source path is logged.

`--log-level debug` is opt-in and adds the actionable detail needed to
diagnose a run that looks idle:

* **Poll summaries** (`"msg":"poll summary"`) — one per poll, with
  `candidates`, `eligible`, and `admitted` counts plus a `rejected` map of
  categorized counts (`not_active`, `terminal`, `not_routable`,
  `already_claimed`, `at_capacity`, `stopping`, …). Each rejected candidate
  also gets its own `"msg":"poll candidate rejected"` record with its
  `issue_identifier` and `reason`.
* **Claim and preparation records** — `"msg":"issue claimed"`,
  `"msg":"workspace preparation started"`, `"msg":"workspace prepared"`, and
  `"msg":"agent launch requested"` trace an issue from admission through the
  point the selected agent backend is asked to start a turn. The launch record
  carries `agent_backend`, naming the runtime that session starts on.
* **Tool/item lifecycle records** (`"msg":"agent item event"`) — one per
  app-server tool or item transition: `item_type` (for example
  `commandExecution`, `mcpToolCall`, `fileChange`, `dynamicToolCall`),
  `item_id`, `outcome` (`started`, `completed`, `failed`, `declined`), and
  `duration_ms` once known. `item_name` appears only when the protocol
  supplies a fixed tool name directly (an MCP tool's name), or when the name is
  a registry-owned constant for one of Symphony's own bound capabilities
  (`github_publish_pr`, `github_pr_context`, and `github_land_pr`); it is never
  derived from command bodies, tool arguments, or the tool name an agent sent.
  `create_followup_issue` deliberately emits no `dynamicToolCall` records: it is
  a single bounded tracker round trip, so there is no slow outstanding operation
  to make visible, and its argument validation belongs to the provider rather
  than to a reported call.

  All of this describes `agent.backend: codex`. Under `agent.backend: claude` a
  bounded capability is invoked over the private MCP endpoint rather than as an
  app-server item, and the Claude `--print` stream reports no item lifecycle for
  it, so **none** of the four capabilities emits these records -- including the
  three whose `Lifecycle()` is the documented reason they do under Codex. The
  `outstanding_item_type`/`outstanding_item_id` fields in the heartbeat records
  below therefore never name a Claude capability call, and a capability call that
  runs long is not what makes a Claude turn look stalled. Closing that gap is not
  part of the capability wiring.
* **Heartbeat and stall records** — every reconciliation pass for a still-active
  run logs `"msg":"agent heartbeat"` with `last_activity_age_ms` and, when one
  tool/item is outstanding, `outstanding_item_type`, `outstanding_item_id`, and
  `outstanding_age_ms`. When the selected backend's `stall_timeout_ms`
  (`codex.stall_timeout_ms` or `claude.stall_timeout_ms`) fires, the existing
  `"msg":"agent reconciled"` record (`"reason":"stalled"`) carries the same
  outstanding-operation fields, so the log names what the agent was waiting on
  instead of only "no events for N minutes".
* **Generic progress coalescing** — any app-server notification Symphony does
  not otherwise classify still logs as `"msg":"agent event"`, but only at
  debug level, and repeats of the identical message are coalesced (logged
  once, then again only every 20th repeat with a `repeated` count) so a chatty
  protocol stream cannot flood even the debug log.

Empty rate-limit snapshots (the app-server sends these often) are never
logged at any level; only a populated snapshot appears in
`"msg":"agent rate limit"`.

## The Claude backend

With `agent.backend: claude` the records and the field vocabulary are the same,
but four of them mean something slightly different, because the Claude Code
CLI's `--print` stream is not the app-server protocol.

* **Tool/item lifecycle** — the CLI emits no discrete tool start/complete
  notifications and no protocol-supplied durations. The backend pairs each
  `tool_use` with its `tool_result` by ID and times the pair itself, so
  `duration_ms` on an `"msg":"agent item event"` record is measured by Symphony
  rather than reported by the runtime, and is absent when a result arrives for a
  call this process never saw start. The `item_type` values are deliberately the
  same categories the Codex backend reports: `commandExecution` for the `Bash`
  tool, `fileChange` for the file-editing tools (`Edit`, `Write`, and the
  editing tools not on the current surface), `mcpToolCall` for any `mcp__*`
  tool, and `toolCall` for everything else. `item_name` is the CLI's own fixed
  tool name; nothing is derived from tool arguments or command bodies. A refused
  call appears as `outcome: declined`, and the terminating result's own list of
  refusals is logged as a diagnostic naming only the tool.
* **Token usage** — the CLI reports usage per turn, while the coordinator keeps
  a component-wise maximum across a run (app-server notifications are
  cumulative). The backend therefore accumulates turns itself and reports a
  running total, so `"msg":"agent usage"` still grows monotonically across the
  run. Its input count folds `cache_creation_input_tokens` and
  `cache_read_input_tokens` in with `input_tokens`: on a resumed session almost
  all input arrives as cache reads, so `input_tokens` alone understates what the
  model processed by orders of magnitude.
* **Session start** — one `"msg":"agent session started"` record per *turn*, not
  per run. `claude --print` runs a single turn and exits, so each continuation
  is a new process with a new `pid` and an incremented `turn_id`, resumed under
  the same `session_id`/`thread_id`. That record is emitted only after the CLI's
  `system`/`init` event confirms the launch contract; a mismatch, or no init
  event at all, fails the turn instead.
* **Failure text** — a turn's outcome is read from the terminating `result`
  event's `is_error` and `terminal_reason`, never its `subtype` (an
  authentication failure arrives as `subtype: "success"` with `is_error` set).
  Only that bounded reason, an API error status, or a stop reason reaches the
  log; the result's own text does not. When a turn ends without any result
  event, the tail of the child's stderr is reported as a diagnostic, truncated
  to the shared redaction bound.

## Following the log

All logs, pretty-printed as they arrive:

```sh
tail -F .symphony/logs/symphony.jsonl | jq .
```

One issue only:

```sh
tail -F .symphony/logs/symphony.jsonl | jq 'select(.issue_identifier == "PMR-39")'
```

Meaningful lifecycle events only (skip debug-level poll/heartbeat noise even
when debug is enabled):

```sh
tail -F .symphony/logs/symphony.jsonl \
  | jq 'select(.msg | test("claimed|prepared|launch requested|session started|turn completed|reconciled|retry scheduled|cleanup"))'
```

What is Codex waiting on right now (requires `--log-level debug`):

```sh
tail -F .symphony/logs/symphony.jsonl \
  | jq 'select(.msg == "agent item event" or .msg == "agent heartbeat")'
```

Just the categorized poll rejections, to see why an expected issue never
dispatched:

```sh
tail -F .symphony/logs/symphony.jsonl \
  | jq 'select(.msg == "poll summary" or .msg == "poll candidate rejected")'
```

Every tracker state change Symphony made, as an operation + from → to trail:

```sh
tail -F .symphony/logs/symphony.jsonl \
  | jq 'select(.msg == "Linear transition") | {operation, from_state, to_state, issue_identifier}'
```

Every state change out of the review handoff state Symphony did not make,
with its classification (`review_approved`, `rework_requested`, or the
actionable `external_reversion`):

```sh
tail -F .symphony/logs/symphony.jsonl \
  | jq 'select(.msg | test("human review state change|external tracker state change")) | {operation, from_state, to_state, issue_identifier, since_handoff_ms}'
```

Workspace lifecycle final status (removal vs. kept for review):

```sh
tail -F .symphony/logs/symphony.jsonl | jq 'select(.msg == "workspace cleanup")'
```

Each record carries one fixed `status`: `clean` (removed, no local commits
past the recorded base commit), `landed` (removed, and it did hold local
commits, which Symphony verified as the merged pull request head for that
issue), `dirty` (kept: uncommitted or untracked changes), `committed` (kept:
local commits that are not verifiably merged, including a landing that could
not be verified), or `blocked` (kept: any other fail-closed refusal, such as
unowned or unverifiable state). Only `clean` and `landed` removed anything; the
warn-level records carry the workspace package's own refusal text as `error`.
The read-only landing check that separates `landed` from `committed` logs its
own info-level `"msg":"GitHub landing verified for workspace cleanup"` or
`"msg":"GitHub landing unverified; workspace commits are preserved"` record
with the `repository`, the shortened `workspace_commit`, and, when verified,
the `pr_number`. See [workspace ownership and
recovery](completion-markers.md) for the full cleanup safety table.

## What never appears in the log

Regardless of level, Symphony never logs: Linear or GitHub credentials or
credential-file contents; inherited secret environment values; rendered
prompts or issue descriptions; tool arguments, tool outputs, command bodies,
or model reasoning; or raw Codex protocol payloads. Item/tool lifecycle
records are built by decoding only a fixed, narrow set of protocol fields
(item type, item/call ID, an already-fixed tool name, status, and a
protocol-computed duration) — arguments, command bodies, and outputs have no
matching field and are never read into a log record. The same holds for the
Claude backend's stream: every event is decoded into a narrow struct with no
member for `assistant.message.content[].text`, `tool_use.input`,
`tool_result.content`, or `result.result`, so that content is discarded before
anything is logged. The `agent_authentication` preflight check reads only the
`loggedIn` boolean from `claude auth status`; the email, organization, and
subscription that command also returns are neither read nor logged.

**One caveat, and it is about Symphony's log only.** Everything above describes
what Symphony writes. The Claude Code CLI keeps its own transcript: with
`agent.backend: claude`, the full session — rendered prompts, issue
descriptions, tool arguments and tool output — is persisted by the CLI to
`~/.claude/projects/<cwd-slug>/<session-id>.jsonl`. That file is outside the
worktree and is not removed by workspace cleanup, it is the Claude equivalent of
the raw Codex rollout this document exists to let you avoid reading, and
`--resume` depends on it, so it cannot be disabled. Redirecting
`CLAUDE_CONFIG_DIR` relocates it but breaks the CLI's subscription
authentication, so Symphony leaves it in place. Treat that directory with the
same care as a credential file. See
[internal/observability](../internal/observability) for the shared
redaction and truncation boundary every log record passes through, and
[architecture.md](architecture.md) for the broader trust model.
