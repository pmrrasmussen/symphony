# Repository map

Symphony is a long-running Linear issue runner for one configured agent
backend, Codex or Claude Code (`agent.backend`). `WORKFLOW.md` is this
repository's single executable source of delivery policy: its front matter
configures the tracker lifecycle, capacity, and GitHub integration, and its
prompt body gives state-specific start, implementation, validation, review
handoff, rework, landing, and completion instructions. Read it, and
[README.md](README.md), before starting issue work.

## Layout

- `cmd/symphony` — the CLI entry point (`--workflow`, `--dry-run`, `--logs-root`, `--log-level`).
- `internal/config` — loads and validates `WORKFLOW.md`, and owns the reserved
  host credential variable names plus the one description of the four-part filter
  both backends apply to an agent child environment (`ReservedSecretEnvNames`).
  The other three parts are derived and applied by the backends.
- `internal/coordinator` — the scheduling core: polling, capacity, retries, and reconciliation.
- `internal/linear` — the Linear GraphQL tracker adapter and the host-side review handoff, transitions, and bounded `create_followup_issue` tooling.
- `internal/github` — the optional, fixed-repository GitHub PR publish/context/land adapter.
- `internal/agent` — routes new sessions to the configured `agent.backend` and pins continuation and cancellation to the backend that started each session.
- `internal/capability` — the agent-neutral registry of bounded session capabilities: definitions, availability, argument validation, invocation, and typed results/refusals.
- `internal/mcpbridge` — the in-process loopback MCP endpoint that serves that registry to an MCP-capable agent process: one listener, per-session bearer tokens, one invocation in flight per session, and drain-before-finalize revocation. Wired into the Claude backend and reachable: a Claude workflow may bind Symphony's session capabilities.
- `internal/codex` — the Codex app-server JSON-RPC backend and dynamic tool wiring.
- `internal/claude` — the Claude Code CLI backend: the fixed, non-configurable launch contract (tool surface, permission mode, settings sources, sandbox, MCP configuration) re-applied and re-verified on every turn, the per-turn capability-endpoint registration retired before the next turn is minted, the launch-time cross-check that the rendered prompt's capability promises match what this session advertises, and the narrow `--print` stream decode.
- `internal/workspace` — local Git worktree lifecycle, ownership, and recovery.
- `internal/domain` — the shared, provider-agnostic issue/agent/workspace types.
- `internal/observability` — structured log redaction and level policy.
- `internal/preflight` — the `--dry-run` side-effect-free validation path.
- `internal/status` — the versioned, redacted runtime status snapshot written for operator clients.
- `internal/operator` — read-only discovery of local LaunchAgent instances, and the display-safe model built from launchd, status, configuration, and log observations.
- `internal/service` — management of one repository-scoped macOS LaunchAgent (`install`, `status`, `restart`, `uninstall`, `migrate`).
- `internal/tui` — the read-only operator dashboard (`symphony tui`), which renders `internal/operator` and can mutate nothing.
- `docs/` — architecture, the Linear tracker profile, workspace recovery, observability, and the live smoke profile.

## Working here

1. Move the Linear issue to **In Progress** (or let Symphony do it) and work
   in an isolated Git worktree, never the primary checkout.
2. Implement a focused, validated change with a clean local commit.
3. Follow WORKFLOW.md's delivery-mode instructions to publish a pull request;
   merge only after required checks and review, per `README.md`.
4. Keep repository guidance (`README.md`, `WORKFLOW.md`, `WORKFLOW.example.md`,
   `docs/`) describing one consistent state machine, bounded capabilities,
   trust boundary, and recovery behavior.
