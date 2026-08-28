# Symphony (Go)

Symphony is a long-running issue runner for one configured agent backend, the
Codex app-server or the Claude Code CLI (`agent.backend`). It reads
repository-owned policy from `WORKFLOW.md` -- this repository's single
executable source of delivery policy -- polls Linear, and runs each eligible
issue in its own deterministic local workspace: a detached Git worktree that is
never the checkout Symphony itself runs from.

Two properties shape everything else. Every tracker state change is performed
host-side with the host credential, so no agent-invokable tool can move an issue
on the board. And every capability an agent does get is bounded and
zero-to-few-argument -- publish this worktree, land this pull request -- rather
than general access to Linear, GitHub, or a shell on the host.

## Running it

```sh
go run ./cmd/symphony --workflow ./WORKFLOW.md
# Equivalent positional workflow-file form:
go run ./cmd/symphony ./WORKFLOW.md
```

Validate a workflow file, and a synthetic full lifecycle, without contacting
Linear or launching an agent. Run this before any live run and before any
configuration change reaches a running daemon:

```sh
SYMPHONY_LINEAR_API_KEY_FILE=/path/to/a/mode-600-key-file \
  go run ./cmd/symphony --dry-run ./WORKFLOW.example.md
```

On macOS, run it continuously as one LaunchAgent per repository. Install the
shared executable once, then register each repository that owns a valid
`WORKFLOW.md` from its own checkout:

```sh
cd ~/repos/symphony
./scripts/install

cd ~/repos/foo
symphony service install
```

`symphony tui` is a read-only dashboard over every local instance; it can mutate
nothing. The other process-level flags are `--logs-root`, `--log-level`, and
`--status-file`.

## The canonical lifecycle

`Todo -> In Progress -> In Review <-> Rework -> Merging -> Done`

`Todo`, `In Progress`, `Rework`, and `Merging` are dispatchable. `In Review` is
the single, fixed, human-controlled review state and is never dispatched.
`Done` and `Canceled` are terminal.

| Edge | Moved by | Meaning |
| --- | --- | --- |
| `Todo -> In Progress` | Symphony, host-side at dispatch | Board state never depends on the agent starting itself. |
| `In Progress`/`Rework -> In Review` | Symphony, host-side after `github_publish_pr` | The pull request exists and is linked. |
| `In Review -> Rework` | a human | Changes requested; the same worktree, branch, and pull request resume. |
| `In Review -> Merging` | a human | The approval to land. Nothing else authorizes a merge. |
| `Merging -> Done` | Symphony, after `github_land_pr` merges | Also reconciles a merge a human performed. |
| `Merging -> In Review` | Symphony, on a hard landing gate | Pending checks instead wait, without changing state. |

This repository runs up to four implementation sessions concurrently while
landing stays serialized. [docs/linear-tracker.md](docs/linear-tracker.md) owns
this state machine, its configuration, and the operator prerequisites for
running it live; [WORKFLOW.md](WORKFLOW.md)'s prompt body is what each session
is actually told.

## Where each topic is documented

Every topic below has exactly one owning page. Anything that mentions it
elsewhere links to that page rather than restating it, so a behaviour change
updates one page and the links to it stand.

| Topic | Owner |
| --- | --- |
| What Symphony is, how to run it, where each topic lives | this file |
| Repository layout and how to work in it | [AGENTS.md](AGENTS.md) (also served as `CLAUDE.md`) |
| Delivery policy: per-state agent instructions, and this repository's own configured values | [WORKFLOW.md](WORKFLOW.md) |
| Annotated configuration reference: every front-matter field, its default, and whether it fails closed | [WORKFLOW.example.md](WORKFLOW.example.md) |
| Trust boundary, host credential filter, backend launch contracts, capability model, scheduling and reload behaviour | [docs/architecture.md](docs/architecture.md) |
| Lifecycle states, host-owned transitions, Linear configuration, follow-up issue creation, live-run operator prerequisites | [docs/linear-tracker.md](docs/linear-tracker.md) |
| Log levels, every structured record and its fields, redaction guarantees, following a live run | [docs/observability.md](docs/observability.md) |
| macOS repository services, credential file layout, `symphony tui`, migrating a hand-authored LaunchAgent | [docs/macos-services.md](docs/macos-services.md) |
| The `--dry-run` preflight and the `agent_authentication` check | [docs/preflight.md](docs/preflight.md) |
| The `--status-file` runtime snapshot and its freshness rules | [docs/runtime-status.md](docs/runtime-status.md) |
| Workspace ownership, terminal cleanup safety, manual recovery | [docs/completion-markers.md](docs/completion-markers.md) |
| Live Linear and agent smoke testing | [docs/live-smoke.md](docs/live-smoke.md) |
| Operating Symphony on Symphony, and the failure modes that have actually happened | [docs/dogfooding.md](docs/dogfooding.md) |

## Working with Symphony

1. Create one focused Linear issue in `Todo` (or move it to `In Progress`
   yourself). Symphony creates the isolated worktree; never point it at, or work
   in, the primary checkout.
2. Keep policy in the repository. `WORKFLOW.md` is versioned and reloaded live;
   machine-local tuning belongs in an untracked overlay
   ([docs/dogfooding.md](docs/dogfooding.md)), not in committed policy.
3. Validate the narrowest relevant check first, then `scripts/check all` once
   shared behaviour changes. Review the generated workspace before keeping its
   work.
4. Publish a pull request with **Why**, **What changed**, and **On Call** --
   via `github_publish_pr`, or manually when host-side publishing is not
   configured -- and merge only after required checks and review. Moving the
   issue to `Merging` dispatches `github_land_pr`, which lands the pull request
   and moves the issue to **Done**; without landing configured, move the issue
   to **Done** by hand after merging.
5. Use `--dry-run` before any live run. Live smoke tests are manual, opt-in, and
   must use dedicated disposable artifacts
   ([docs/live-smoke.md](docs/live-smoke.md)).

## A note on what the sandbox does and does not protect

The selected `agent.backend` determines the operative write boundary, and the
two backends are not equivalent -- [docs/architecture.md](docs/architecture.md)
states the current contracts, their verified limits, and the one gap that
remains open. What holds for both: writes are confined, host Linear and GitHub
credentials are stripped from every child process Symphony spawns, but local
reads and outbound network are available. What protects host credentials from
exfiltration is therefore that no untrusted input reaches the worker -- the
issue text and repository content it acts on are operator-owned -- rather than
the sandbox itself.
