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
  host credential variable names (`ReservedSecretEnvNames`), the fixed part of
  the four-part filter applied to the environment of every process Symphony
  spawns. The one description of that filter is
  [docs/architecture.md](docs/architecture.md)'s "The host credential filter".
- `internal/hostenv` — the one implementation of that filter (`Filter`), applied
  by every launcher. It depends only on `internal/config`, so a caller with no
  session passes no secret matcher and still gets the other three parts.
- `internal/coordinator` — the scheduling core: polling, capacity, retries, and reconciliation.
- `internal/linear` — the Linear GraphQL tracker adapter and the host-side review handoff, transitions, and bounded `create_followup_issue` tooling.
- `internal/github` — the optional, fixed-repository GitHub PR publish/context/land adapter.
- `internal/agent` — routes new sessions to the configured `agent.backend` and pins continuation and cancellation to the backend that started each session.
- `internal/capability` — the agent-neutral registry of bounded session capabilities: definitions, availability, argument validation, invocation, and typed results/refusals, plus the one dispatch that runs a call for either transport — lookup, gates, invocation, item records, and terminal outcome — so only the wire envelope is a backend's own.
- `internal/mcpbridge` — the in-process loopback MCP endpoint that serves that registry to an MCP-capable agent process: one listener, per-session bearer tokens, one invocation in flight per session, and drain-before-finalize revocation. Wired into the Claude backend and reachable: a Claude workflow may bind Symphony's session capabilities.
- `internal/codex` — the Codex app-server JSON-RPC backend and dynamic tool wiring.
- `internal/claude` — the Claude Code CLI backend: the fixed, non-configurable launch contract (tool surface, permission mode, settings sources, sandbox, MCP configuration) re-applied and re-verified on every turn, the per-turn capability-endpoint registration retired before the next turn is minted, the launch-time cross-check that the rendered prompt's capability promises match what this session advertises, and the narrow `--print` stream decode.
- `internal/agentstream` — the output path both backends share and neither owns:
  the bounded line framing a child's stdout is read through (an oversized line is
  skipped, never fatal) and the sink that owns a turn's event channel — one
  mutex, one terminal latch, a reserved slot for the outcome, and the optional
  pre-activation hold for a stream whose opening event is not known yet.
- `internal/agenttest` — the test support both backends share: the fake
  Linear/GitHub boundary a landing session runs against, the one shared suite for
  the host-side deferred landing behaviour (each backend runs it through a
  fixture of its own and keeps only its own finalize trigger), and the fake timer
  both backends' `Timer` seams accept, so a bound is asserted by elapsing it
  rather than by waiting it out. Imported only by tests.
- `internal/workspace` — local Git worktree lifecycle, ownership, and recovery,
  and the `WORKFLOW.md` hook runner: a hook is a child like an agent backend, so
  it runs under the same filtered environment with no session matcher, and so is
  every host-side `git` this package and `internal/github` spawn.
- `internal/domain` — the shared, provider-agnostic issue/agent/workspace types.
- `internal/observability` — structured log redaction and level policy.
- `internal/preflight` — the `--dry-run` side-effect-free validation path.
- `internal/status` — the versioned, redacted runtime status snapshot written for operator clients.
- `internal/operator` — read-only discovery of local LaunchAgent instances, and the display-safe model built from launchd, status, configuration, and log observations.
- `internal/service` — management of one repository-scoped macOS LaunchAgent (`install`, `status`, `restart`, `uninstall`, `migrate`).
- `internal/tui` — the read-only operator dashboard (`symphony tui`), which renders `internal/operator` and can mutate nothing.
- `docs/` — architecture, the Linear tracker profile, workspace recovery, observability, macOS services, the `--dry-run` preflight, runtime status, the live smoke profile, and the dogfooding operator guide.

An in-code comment states the invariant, and the one alternative a reader must
have already ruled out before editing the next line. The derivation behind it —
the multi-paragraph argument, the failure walkthrough, the record of what was
measured live — belongs to the `docs/` page that owns the topic, named by a
one-line pointer at the code (PMR-171).

## Working here

1. Move the Linear issue to **In Progress** (or let Symphony do it) and work
   in an isolated Git worktree, never the primary checkout.
2. Implement a focused, validated change with a clean local commit.
3. Follow WORKFLOW.md's delivery-mode instructions to publish a pull request;
   merge only after required checks and review.
4. Documenting a behaviour change means updating **one** page — the one that
   owns that topic — and letting the links to it stand:

   | Topic | Owning page |
   | --- | --- |
   | Entry point: what Symphony is, how to run it, the topic map | `README.md` |
   | This repository's layout and conventions | `AGENTS.md` (served as `CLAUDE.md`) |
   | Delivery policy and this repository's configured values | `WORKFLOW.md` |
   | Every configuration field, its default, and its failure mode | `WORKFLOW.example.md` |
   | Trust boundary, credential filter, backend contracts, capabilities, scheduling | `docs/architecture.md` |
   | Lifecycle states, host-owned transitions, tracker configuration | `docs/linear-tracker.md` |
   | Log records, levels, and redaction | `docs/observability.md` |
   | macOS services, credential layout, the TUI | `docs/macos-services.md` |
   | `--dry-run` | `docs/preflight.md` |
   | `--status-file` | `docs/runtime-status.md` |
   | Workspace ownership, cleanup, recovery | `docs/completion-markers.md` |
   | Live smoke testing | `docs/live-smoke.md` |
   | Running Symphony on Symphony | `docs/dogfooding.md` |

   If a change genuinely spans two topics, say it once on each owner's page in
   its own terms and cross-link; do not paste the same paragraph into both.
