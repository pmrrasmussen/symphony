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

One `operation` value names no tracker edge at all. When a dispatch reaches
`agent.max_attempts` — the ceiling on how many times one issue may be launched
before the coordinator gives up on that episode — Symphony logs a single
**error**-level `"msg":"dispatch abandoned after max attempts"` with
`operation: dispatch_abandoned`, the issue, the classified failure `reason`
(`workspace_prepare`, `before_run`, `prompt_render`, `session_start`,
`stalled`, `agent_blocked`, `turn_limit_exhausted`), the final `attempt`, and
`max_attempts`. Each of these names something about *this issue's* run — its
template, its own agent, its own turn budget — never the shared environment
dispatching it.

Six reasons never appear on that abandonment record, because none of them is
evidence the issue itself is unworkable (`systemicFailureReasons` in
`internal/coordinator/coordinator.go`):

* `agent_event` — a run that ended on `domain.EventFailed` carrying model or
  provider text the coordinator cannot itself classify.
* `agent_rate_limited` — a Claude quota rejection (PMR-131; see "Rate limit
  status" under "The Claude backend" below). Unlike the other five reasons
  here, its retry is not scheduled from the ordinary backoff ladder either:
  `finishFailure` takes the delay from the rejection's own reset time, or a
  floor well above `agent.max_retry_backoff_ms` when the CLI reported none.
* `issue_refresh` — a tracker error from the post-turn `GetIssues` refresh
  that follows a turn the agent completed successfully (PMR-115; confirmed
  live as a 30s Linear client timeout). This is tracker infrastructure, not
  the issue: with an escalating backoff ladder, a Linear outage lasting a
  couple of minutes would otherwise abandon every issue running at the time,
  since they would all fail the post-turn refresh the same way at once.
* `retry_refresh` — the same tracker error from the *pre-dispatch* `GetIssues`
  refresh a queued retry runs before redispatching (PMR-142). It wraps the
  same failure as `issue_refresh`, just observed at a different moment: an
  issue that already failed once and is waiting to retry, rather than one
  that just finished a turn. A sustained outage drives every retrying issue
  through exactly this site, so leaving it off this exemption let the same
  outage abandon issues at that moment while sparing ones still running.
* `session_continue` — Symphony's own backend adapter (`agent.Continue`)
  failing to resume a session. A broken agent binary or lapsed backend auth
  fails every running issue's next turn identically.
* `stream_closed` — the coordinator's own event plumbing closing its channel
  without ever delivering `EventFailed` or `EventCompleted`. Every backend
  emits one of those before it closes its channel, so this is never a
  repository- or issue-specific outcome, only ever a host bug.

Each of these six is exempt from the ceiling, so a transient, account-wide
condition cannot abandon an otherwise-healthy issue; five of them (every
reason but `agent_rate_limited`) also keep climbing the ordinary backoff
ladder while they do. Every classified `reason` — armed or exempt alike, on
the `"msg":"agent run retry scheduled"` warning that precedes abandonment as
well as on the abandonment record itself — also carries the underlying
`error`, redacted and bounded the same way as any other `error`-keyed
attribute (`internal/observability.Text`) rather than omitted, including
`agent_event`'s model/provider text.

That record is deliberately the *whole* outcome:
the claim and the retry timer are dropped, and the tracker is left exactly as
it was. That is what
makes the record load-bearing — the board will keep showing the issue as
active work, so this is the only place the give-up is visible. A later poll may
start a fresh, equally bounded episode; an issue that keeps producing this
record needs a person, not another retry. Below the ceiling nothing changes:
each earlier failure keeps its warn-level `"msg":"agent run retry scheduled"`.
Landing waits never reach it, because a wait does not escalate the attempt.

Every `operation` value — the performed edges above, these three observed ones,
and `dispatch_abandoned` — comes from one bounded vocabulary of fixed literals
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
  (`refresh_base_ref`, `github_publish_pr`, `github_pr_context`, and
  `github_land_pr`); it is never
  derived from command bodies, tool arguments, or the tool name an agent sent.
  `create_followup_issue` deliberately emits no `dynamicToolCall` records: it is
  a single bounded tracker round trip, so there is no slow outstanding operation
  to make visible, and its argument validation belongs to the provider rather
  than to a reported call.

  `dynamicToolCall` records are emitted for **both** backends, by the one shared
  capability dispatch (`internal/capability`), so which capabilities are reported
  does not depend on which transport carried the call: the four GitHub
  capabilities are, and `create_followup_issue` is not, under `codex` and under
  `claude` alike. `duration_ms` is measured by Symphony around the provider round
  trip. The `item_id` is the app-server's own request ID under `codex`, and a
  host-minted `mcp-call-<n>` under `claude` — never the JSON-RPC ID the child
  chose, which is a value from the wire and so may not reach a record.

  One `claude` capability call therefore appears twice, under two item types, and
  neither record replaces the other. The CLI's own stream reports the model's tool
  call as an `mcpToolCall` named `mcp__symphony__<tool>`, timed by the backend's
  `tool_use`/`tool_result` pairing: that is what the model did. The capability
  endpoint reports the same work as a `dynamicToolCall` named `github_land_pr`,
  timed around the provider round trip: that is what the host ran, and it is the
  only record produced at all by a capability call the child made by some route
  other than the model's own tool use (its shell holds the endpoint token). The
  two carry different `item_id`s. The `outstanding_item_type`/`outstanding_item_id`
  fields in the heartbeat records below name a running Claude capability call the
  same way they name a Codex one.
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
  refusals is logged as a diagnostic naming only the tool. A call to one of
  Symphony's own bounded capabilities also produces the `dynamicToolCall` pair
  the capability endpoint emits, as described above; that pair, not this one, is
  the host-side view of the provider round trip.
* **Token usage** — both backends report real usage, at different granularity
  and through different notifications, and the coordinator's `updateUsage`
  reconciles the two contracts (PMR-153): a non-authoritative figure is merged
  with a component-wise maximum, so `"msg":"agent usage"` grows monotonically
  across the run even from a cumulative or out-of-order source; an
  authoritative figure replaces the recorded total outright. Claude reports
  per API call, mid-turn, from the assistant message's own usage block
  (non-authoritative: this host's running sum of deltas, which can overshoot
  the turn's own settled figure); the terminating `result` event's usage then
  replaces that running estimate outright (authoritative) once the turn
  closes (PMR-136). Its input count folds `cache_creation_input_tokens` and
  `cache_read_input_tokens` in with `input_tokens`: on a resumed session
  almost all input arrives as cache reads, so `input_tokens` alone
  understates what the model processed by orders of magnitude. Codex reports
  from the app-server's `thread/tokenUsage/updated` notification, which
  arrives during the turn as tokens are spent, not only at its end --
  `turn/completed` itself carries no usage field at all. That is what makes
  usage survive a session cancelled immediately after a successful publish
  (PMR-155): the coordinator's own happy path ends the run by cancelling it
  the moment the landing capability resolves, which kills the app-server
  before it would ever reach `turn/completed`. The notification's own running
  total across the thread is reported non-authoritative, since it is already
  cumulative and monotonically increasing by construction. Its input count
  folds `cachedInputTokens` and `cacheWriteInputTokens` into `inputTokens`,
  and `reasoningOutputTokens` into `outputTokens`, mirroring Claude's own
  cache folding. An extraction that finds no usage in a notification that
  should carry it logs one diagnostic per session, naming only the
  notification method, rather than staying silent.
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
* **Rate limit status** (PMR-131/PMR-126/PMR-150) — the CLI's own
  `rate_limit_event` reports a string `status`, which is normalized before it
  leaves the Claude backend. The only values that can appear as log `status`
  are `allowed`, `allowed_warning`, `rejected`, and `unrecognized`; an
  unfamiliar CLI value is never echoed. The first three are the currently
  recognized CLI statuses. It is not the app-server's numeric snapshot, so it
  is never logged as `"msg":"agent rate limit"` under `EventRateLimit`'s
  empty-snapshot rule above; that numeric path only ever applies to Codex.
  `allowed` is the default, healthy state and reaches no event or log record
  at all.
  `allowed_warning` reaches a non-terminal diagnostic logged as
  `"msg":"agent rate limit"` at **warn**, carrying `operation: rate_limit`
  and `status` — distinct from
  `"msg":"agent stderr"`, which stays reserved for child output that
  genuinely could not be decoded. `rejected` is definitive: the account's
  quota for the reported window is closed, so the backend ends the turn
  there rather than waiting for the result event the CLI still sends a
  moment later, and reports it as its own terminal event logged as
  `"msg":"agent rate limit rejected"` at **warn**, carrying
  `operation: rate_limit`, `status`, and
  `retry_after_ms`. The coordinator names the retry reason
  `agent_rate_limited` (distinct from the unclassified `agent_event`
  fallback) in `"msg":"agent run retry scheduled"`, exempts it from
  `agent.max_attempts` the same way `issue_refresh` and `stream_closed` are
  (see `systemicFailureReasons` in `internal/coordinator/coordinator.go`),
  and schedules the next attempt from the CLI's own reset time when it
  reported one, falling back to ten times `agent.max_retry_backoff_ms`
  otherwise — never the ordinary escalating ladder, which caps in minutes
  and is far too short for an account-wide window that can run for hours.

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

Issues Symphony gave up dispatching, which no tracker state reflects:

```sh
tail -F .symphony/logs/symphony.jsonl \
  | jq 'select(.operation == "dispatch_abandoned") | {issue_identifier, reason, attempt, max_attempts}'
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

The `internal/github` package's own warn-level records name the operation that
failed and carry the underlying cause as a bounded `error`
(`internal/observability.Text`), the same way the records above do (PMR-154).
GitHub's HTTP errors are fixed strings or interpolate only a numeric status
code (`"github request failed with status %d"`) — a response body never
reaches one of these `error` attributes:

* `"msg":"GitHub landing verification failed"` — the read-only pull request
  lookup `VerifyLanded` needs failed; `issue_id`/`issue_identifier`,
  `repository`, `error`.
* `"msg":"GitHub land Merging fallback transition failed"` and
  `"msg":"GitHub land deferred Merging fallback transition failed"` — the
  best-effort `Merging -> In Review` fallback transition failed after an
  immediate or deferred landing refusal; `issue_id`/`issue_identifier` (the
  immediate case also carries the hard-gate `reason`), `error`.
* `"msg":"GitHub land refusal comment failed"`,
  `"msg":"GitHub land push audit Linear comment failed"`, and
  `"msg":"GitHub land push audit PR comment failed"` — a best-effort audit
  comment (the refusal reason, or a fix turn's pushed commit) failed to post to
  Linear or the pull request; `issue_id`/`issue_identifier` (the latter two
  also carry `pr_number`), `error`.
* `"msg":"GitHub pull request poll failed"` — the linked-PR poll's read of the
  pull request failed; `issue_id`/`issue_identifier`, `pr_number`, `error`.
  This fires again on the same link every `github.poll_interval_ms` until the
  read succeeds or the issue is forgotten, so a permanently-failing poll (for
  example a 404 on a deleted pull request) is not yet distinguished in the
  record from a transient one and currently retries indefinitely.
* `"msg":"GitHub merge Linear completion failed"` — the pull request merged
  but reconciling the bound Linear issue to Done failed, so the link stays
  live and is retried next poll; `issue_id`/`issue_identifier`, `pr_number`,
  `error`.
* `"msg":"GitHub pull request closed without merge; Linear issue remains in
  review"` — a terminal state observation rather than a failed operation, so
  it carries no `error`; `issue_id`/`issue_identifier`, `pr_number`, `pr_url`
  identify which pull request a human needs to finish reconciling by hand.

The `internal/workspace` package logs three more records, all through the same
redaction boundary rather than to `os.Stderr` — the JSONL log is the operator's
complete record of them:

* `"msg":"workspace source integrity alert"` is the
  [PMR-65](https://linear.app/pmrrasmussen/issue/PMR-65) defense-in-depth
  backstop: after a run, Symphony re-checks that the source repository's
  branch heads (other than the `symphony/*` publish branches it creates
  itself) and primary working tree index are exactly as they were before the
  run, and this **error**-level record is written if they are not — the
  narrowed sandbox grant a linked worktree receives is supposed to make that
  impossible, so this is the highest-signal record the package can produce. It
  carries `operation: source_integrity_alert`, `issue_id`/`issue_identifier`,
  and `source_root`. A failure to even compute the post-run fingerprint (for
  example, the source repository became unreadable) instead logs
  `"msg":"workspace source integrity check failed"` at **warn**, with the same
  issue attributes and a bounded `error`.
* `"msg":"workspace hook failed"` covers an `after_run` or `before_remove` hook
  that exits non-zero. Neither hook's failure stops the run or the cleanup it
  brackets, so unlike `before_run` (whose failure reaches the coordinator's own
  `"msg":"agent run retry scheduled"` record) this package logs it directly, at
  **warn**, with `hook` naming which hook (`after_run` or `before_remove`),
  the issue attributes, and a bounded `error`.

Every hook's stdout/stderr is bounded and credential-masked once, in
`internal/workspace`, before it reaches either an error a caller may format
into a log record or one of the two records above: both paths carry the same
`observability.Text`-bounded (`observability.MaxDiagnosticBytes`, currently
512 bytes) and masked diagnostic, so a hook that dumps its environment or a
failing `curl -v` on failure cannot write a credential into either the JSONL
log or an error string.

Query for the integrity alert specifically:

```sh
tail -F .symphony/logs/symphony.jsonl \
  | jq 'select(.msg == "workspace source integrity alert")'
```

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
anything is logged. The `agent_authentication` preflight check applies the
same rule to both backends: it reads only the `loggedIn` boolean from `claude
auth status` (the email, organization, and subscription that command also
returns are neither read nor logged), and only the exit code from `codex
login status` (the human sentence that command prints is neither read nor
logged).

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
