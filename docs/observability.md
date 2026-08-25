# Observability: log levels, poll summaries, and stall records

Symphony writes one structured JSON log line per event to
`<logs-root>/symphony.jsonl` (`--logs-root`, default `.symphony/logs`). This
document covers the two log levels, what each adds, and how to follow the
log during a live run without reading the raw Codex rollout, which can
contain prompts, issue content, tool arguments, and other sensitive data.

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
`review_completed` for the GitHub landing edges), the `from_state` and
`to_state` state NAMES, and the issue (`issue_id`/`issue_identifier`). An
idempotent no-op (the issue is already in the target state) instead logs a
debug-level `"msg":"Linear transition skipped"` with the same fields, so a
skip never appears as a state change. These records are redaction-safe: state
names and issue identifiers only — never a rendered comment, issue
description, or credential. (Agent turn token accounting is tracked
separately.)

Symphony also logs state changes it did **not** perform. The poll loop
remembers each issue it drove into the review `handoff_state`, and if such an
issue later reappears as an active candidate — an external actor (typically the
tracker's native GitHub PR automation) reverted the handoff — it logs one
warn-level `"msg":"external tracker state change observed"` record with
`operation: external_reversion`, the `from_state` (the handoff state) and
`to_state` (the active state it was reverted to) NAMES, the
`issue_id`/`issue_identifier`, and `since_handoff_ms` (elapsed time since the
handoff). It is logged once per revert and, like every transition record, is
redaction-safe. Symphony does not itself re-assert the handoff — the operator
mitigation is to disable the tracker's native PR-to-status automation (see the
[Linear tracker profile](linear-tracker.md)); automatically re-asserting the
handoff without overriding a legitimate human reactivation is a deferred
follow-up.

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
  `"msg":"codex launch requested"` trace an issue from admission through the
  point Codex is asked to start a turn.
* **Tool/item lifecycle records** (`"msg":"agent item event"`) — one per
  app-server tool or item transition: `item_type` (for example
  `commandExecution`, `mcpToolCall`, `fileChange`, `dynamicToolCall`),
  `item_id`, `outcome` (`started`, `completed`, `failed`, `declined`), and
  `duration_ms` once known. `item_name` appears only when the protocol
  supplies a fixed tool name directly (an MCP tool's name, or Symphony's own
  bound `github_publish_pr`/`github_land_pr` capability); it is never derived
  from command bodies or tool arguments.
* **Heartbeat and stall records** — every reconciliation pass for a still-active
  run logs `"msg":"agent heartbeat"` with `last_activity_age_ms` and, when one
  tool/item is outstanding, `outstanding_item_type`, `outstanding_item_id`, and
  `outstanding_age_ms`. When `codex.stall_timeout_ms` fires, the existing
  `"msg":"agent reconciled"` record (`"reason":"stalled"`) carries the same
  outstanding-operation fields, so the log names what Codex was waiting on
  instead of only "no events for N minutes".
* **Generic progress coalescing** — any app-server notification Symphony does
  not otherwise classify still logs as `"msg":"agent event"`, but only at
  debug level, and repeats of the identical message are coalesced (logged
  once, then again only every 20th repeat with a `repeated` count) so a chatty
  protocol stream cannot flood even the debug log.

Empty rate-limit snapshots (the app-server sends these often) are never
logged at any level; only a populated snapshot appears in
`"msg":"agent rate limit"`.

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

Workspace lifecycle final status (clean removal vs. kept for review):

```sh
tail -F .symphony/logs/symphony.jsonl | jq 'select(.msg == "workspace cleanup")'
```

## What never appears in the log

Regardless of level, Symphony never logs: Linear or GitHub credentials or
credential-file contents; inherited secret environment values; rendered
prompts or issue descriptions; tool arguments, tool outputs, command bodies,
or model reasoning; or raw Codex protocol payloads. Item/tool lifecycle
records are built by decoding only a fixed, narrow set of protocol fields
(item type, item/call ID, an already-fixed tool name, status, and a
protocol-computed duration) — arguments, command bodies, and outputs have no
matching field and are never read into a log record. See
[internal/observability](../internal/observability) for the shared
redaction and truncation boundary every log record passes through, and
[architecture.md](architecture.md) for the broader trust model.
