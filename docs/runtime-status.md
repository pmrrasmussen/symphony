# Runtime status snapshot

An optional local JSON file gives a TUI or other operator client a safe,
point-in-time view of one Symphony process without connecting to the
coordinator. `symphony service install` enables it for a managed repository
service at `.symphony/service/status.json`. A manually started process can
choose its own separate path explicitly:

```sh
symphony --workflow ./WORKFLOW.md \
  --status-file .symphony/service/status.json
```

`.symphony/service/status.json` is the recommended path for a managed
repository service. It is intentionally still only a convention: every
Symphony process can choose a different `--status-file`, and there is no shared
registry, lock, or singleton path. The parent runtime directory is created mode
`0700`; the snapshot is replaced atomically and mode `0600` so a reader sees
either the previous complete JSON document or the next complete document,
never a partial write. If the selected runtime directory already exists, it
must already be owner-only; Symphony refuses to change permissions on an
arbitrary existing parent directory. `--dry-run` checks this ahead of time as
its `status_file` check, so a bad directory mode fails preflight instead of
surfacing only as a repeating write-failure warning once the daemon is
already running.

The versioned document includes the process PID and start time, generation
time, effective workflow and log paths, coordinator claim/running/retry state,
token usage, safe rate-limit counters, the current Linear state and last
activity time for each running issue, plus any outstanding operation's fixed
type/name and start/age. Token usage appears twice, as two `usage` and
`issue_usage` objects of plain integer counts: the first is the attempt in
front of you, the second what the issue has spent across every attempt of the
dispatch episode, which a retrying entry carries even though it holds no run
(PMR-151). Adding the second was purely additive, so the schema stays at
version 1. It deliberately excludes credentials, environment
values, issue descriptions, prompts, workspace paths or contents, command
bodies, tool arguments and output, and raw Codex payloads.

`symphony service status` and `symphony tui` combine this runtime document with
a separately loaded, display-safe effective configuration. That configuration
is backend-aware: both backends report the common `turn_timeout` and
`stall_timeout`; a Codex daemon additionally reports `codex_command`,
`codex_approval_policy`, `codex_thread_sandbox`, `read_timeout`, and
`start_timeout`; a Claude daemon instead reports `claude_command` and, when
configured, `claude_model`. Fields for the other backend are omitted. The
runtime snapshot schema remains version 1 because this effective-configuration
projection is not part of `status.json`.

`state` is one of three values, each written at a specific point in the
process lifecycle:

* `"running"` -- written on startup and then every second for as long as the
  coordinator is neither draining nor stopped. It is derived from the same
  coordinator snapshot as every other field on the document (specifically its
  `stopping` flag), so it cannot disagree with `"stopping"` below.
* `"stopping"` -- written once immediately when shutdown begins (`SIGINT` or
  `SIGTERM`), and then on every subsequent one-second tick for as long as
  `Coordinator.Shutdown` is still draining running sessions. The periodic
  publisher is deliberately kept running through this whole window rather than
  stopped ahead of it, specifically so `generated_at` keeps advancing while a
  shutdown that can take up to twenty seconds is in progress: without it, the
  file would go stale the moment shutdown began and only jump straight from
  `"running"` to `"stopped"` once it finished, leaving an operator unable to
  tell a draining daemon from a hung one.
* `"stopped"` -- written once, after `Coordinator.Shutdown` returns (whether it
  drained cleanly or timed out) and the periodic publisher has been stopped.
  This is always the final write of a graceful shutdown.

Snapshot publication is observational: a filesystem failure is logged but
never blocks or changes coordinator scheduling. A write failure is logged once
and then suppressed while it stays identical from tick to tick, rather than
once per second for the rest of the process's life; it is logged again if the
failure changes or the writes recover. A crash can leave a `"running"` or
`"stopping"` record behind, so clients must treat `state` as a hint rather
than liveness authority: compare `generated_at` with a freshness threshold and
check the PID/service manager state (for example launchd) independently. The
structured log at `.symphony/logs/symphony.jsonl` is separate redacted event
history, not this current-state snapshot. For the managed multi-instance
layout, see [macOS repository services](macos-services.md).
