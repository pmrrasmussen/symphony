# Observability: log levels, poll summaries, and stall records

Symphony writes one structured JSON log line per event to
`<logs-root>/symphony.jsonl` (`--logs-root`, default `.symphony/logs`). This
document covers the two log levels, what each adds, and how to follow the
log during a live run without reading the raw Codex rollout, which can
contain prompts, issue content, tool arguments, and other sensitive data.

## Log levels

`--log-level info` (the default) is concise: issue claim through completion
(or cancellation/retry), agent session start, token usage, non-empty rate
limit snapshots, and stderr diagnostics. It never floods with the app-server's
generic protocol notifications.

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
  bound `linear_graphql`/`github_publish_pr` capability); it is never derived
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
