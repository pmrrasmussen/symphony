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
arbitrary existing parent directory.

The versioned document includes the process PID and start time, generation
time, effective workflow and log paths, coordinator claim/running/retry state,
token usage, safe rate-limit counters, the current Linear state and last
activity time for each running issue, plus any outstanding operation's fixed
type/name and start/age. It deliberately excludes credentials, environment
values, issue descriptions, prompts, workspace paths or contents, command
bodies, tool arguments and output, and raw Codex payloads.

Symphony writes the snapshot on startup and every second while it runs, then
writes a final `"state":"stopped"` record after graceful shutdown. Snapshot
publication is observational: a filesystem failure is logged but never blocks
or changes coordinator scheduling. A crash can leave a `"running"` record
behind, so clients must treat it as a hint rather than liveness authority:
compare `generated_at` with a freshness threshold and check the PID/service
manager state (for example launchd) independently. The structured log at
`.symphony/logs/symphony.jsonl` is separate redacted event history, not this
current-state snapshot. For the managed multi-instance layout, see
[macOS repository services](macos-services.md).
