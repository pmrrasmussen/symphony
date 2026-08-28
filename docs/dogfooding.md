# Dogfooding: developing Symphony with Symphony

Symphony develops itself. Issues on the Linear board are implemented by
Symphony's own agent sessions, reviewed by a human operator, and landed by
Symphony's own `github_land_pr` capability. The operator does not write the
code; the operator decides *what* is worked, *in what order*, and *whether the
result is good enough to land*.

This guide is what an operator needs to pick that loop up on a machine that has
never run it. It documents the environment, the one untracked file, the board
conventions, and the failure modes that have actually occurred -- not the ones
that theoretically could.

For the delivery policy itself, read [`WORKFLOW.md`](../WORKFLOW.md); it is the
single executable source of that policy and this guide never restates it.

## 1. Prerequisites

| Requirement | Notes |
| --- | --- |
| Go | the toolchain version in [`go.mod`](../go.mod) |
| `gh` CLI, authenticated | operator-side review and inspection only; Symphony never shells out to it |
| the selected agent CLI | `claude` for `agent.backend: claude`, `codex` for `codex` |
| a Linear API key | personal key, 1500 requests/hour |
| a GitHub **classic** PAT | see below -- this one is not a preference |

### The GitHub token must be a classic PAT

The token must be a classic PAT (`ghp_...`) with `repo` scope. A fine-grained PAT
**cannot read the Checks API**: GitHub disabled the Checks permission for
fine-grained tokens entirely, so `GET /commits/{sha}/check-runs` returns 403
"Resource not accessible". Symphony reads check runs in `github_pr_context`
(always) and in `github_land_pr`'s required-check gate, so a fine-grained token
silently breaks the entire review-and-land half of the lifecycle while leaving
publish working. The durable non-personal alternative is a GitHub App
installation token.

## 2. Credentials

Both credentials are referenced from the workflow file by environment variable,
never inlined. Symphony resolves the variable to a path and reads the file, so
the secret never appears in the workflow, in the environment of an agent
process, or in a log.

```sh
mkdir -p ~/.config/symphony
printf '%s' 'lin_api_...' > ~/.config/symphony/linear-api-key
printf '%s' 'ghp_...'     > ~/.config/symphony/github.token
chmod 600 ~/.config/symphony/*
```

```sh
export SYMPHONY_LINEAR_API_KEY_FILE=~/.config/symphony/linear-api-key
export SYMPHONY_GITHUB_TOKEN_FILE=~/.config/symphony/github.token
```

Both names are reserved: `internal/config` lists them in
`ReservedSecretEnvNames`, and `internal/hostenv.Filter` strips them from the
environment of every process Symphony spawns. An agent session therefore cannot
read the credentials that published its own pull request -- which is also why a
`github_land_pr` hard-gate refusal is undiagnosable from inside a session, and
why the operator has to read the daemon log to explain one: the info-level
`"msg":"Linear transition"` record with `operation: landing_refused` carries
the exact gate as `reason` (`github required checks failed: ...`, `github land
worktree head diverged from the published pull request`, and so on -- see
[observability.md](observability.md)), so which gate fired is answered by that
one record instead of by reading `internal/github/lifecycle.go` (PMR-159).

A `github_publish_pr` refusal is diagnosed the same way: the warn-level
`"msg":"GitHub publish refused"` record with `operation: publish_refused`
carries the exact gate as `reason`, so a run that spends its whole turn budget
retrying the same unrecoverable refusal (for example a push a repository-level
branch or workflow-scope restriction will never let through) still leaves a
host-side trace of why -- including, for the push gate specifically, the
underlying `git push` error as `push_error` (PMR-163).

## 3. `WORKFLOW.local.md`: the one untracked file

`WORKFLOW.md` is committed policy. `WORKFLOW.local.md` is an **untracked
operator overlay** passed with `--workflow`, and it is the single thing a new
machine will not have. It exists so machine-local and session-local tuning --
polling rate, backend choice, a fetch hook -- never has to be committed as
repository policy.

Recreate it by copying `WORKFLOW.md` and applying exactly these four
front-matter deltas:

| Key | `WORKFLOW.md` | overlay | why |
| --- | --- | --- | --- |
| `polling.interval_ms` | `30000` | `5000` | operator responsiveness. Each tick costs two Linear requests while agents run, so roughly 1440/hour against a 1500/hour key. A 429 degrades into backoff, not failure. |
| `hooks.before_run` | absent | `git fetch --no-tags origin +refs/heads/main:refs/remotes/origin/main \|\| true` | `internal/workspace` fetches only when a worktree is *created*, so a reused worktree can hold an `origin/main` many merges old (PMR-135). It has to be host-side: an agent's sandbox cannot write `refs/remotes`. The `\|\| true` is load-bearing -- a `before_run` failure is fatal to the dispatch, and an offline host must still dispatch. |
| `agent.backend` | absent (defaults `codex`) | `claude` | both backends are proven end to end. This overlay chose Claude while in-flight cost telemetry worked only there; PMR-155 closed that gap for Codex too, so the choice is no longer load-bearing. |
| `claude:` block | absent | `command: claude`, `model: claude-sonnet-5`, `turn_timeout_ms: 1800000`, `stall_timeout_ms: 300000` | required once `backend: claude` is selected. |

```sh
cp WORKFLOW.md WORKFLOW.local.md
# apply the four deltas above, then:
go run ./cmd/symphony --workflow ./WORKFLOW.local.md --dry-run
```

The preflight is side-effect free and validates credentials, the agent command,
hook syntax, and a synthetic scheduler pass against fakes. Run it before every
config change reaches the daemon.

### The overlay's prompt body must stay identical to `WORKFLOW.md`'s

The front matter is the overlay. **The prompt body is policy and must not
diverge.** Because the overlay is a copy rather than a true layer, every
improvement merged into `WORKFLOW.md`'s body has to be carried across by hand,
and nothing enforces it.

This has already failed once. On 2026-08-27 the running overlay's body was two
instruction blocks behind `WORKFLOW.md` -- missing the `refresh_base_ref`
guidance (PMR-141) and the loopback-sandbox guidance (PMR-143) -- so every agent
dispatched for roughly a day was told neither. The symptom is invisible from the
board: agents simply do not use a capability that exists, and the resulting
stale-base publish refusals look like a Symphony bug rather than a config drift.

Check for it explicitly after any `WORKFLOW.md` change lands:

```sh
body() { awk '/^---$/{n++; next} n>=2' "$1"; }
cmp <(body WORKFLOW.md) <(body WORKFLOW.local.md) && echo "in sync"
```

To re-sync, keep the overlay's front matter and take the body from `WORKFLOW.md`:

```sh
awk 'BEGIN{n=0} {print} /^---$/{n++; if(n==2) exit}' WORKFLOW.local.md  > new.md
awk 'BEGIN{n=0} n>=2{print} /^---$/{n++}'            WORKFLOW.md       >> new.md
go run ./cmd/symphony --workflow ./new.md --dry-run && mv new.md WORKFLOW.local.md
```

The daemon hot-reloads a changed workflow file and logs
`workflow configuration reloaded`; a config edit needs no restart.

## 4. Running the daemon

Foreground, for a session you intend to watch:

```sh
go run ./cmd/symphony --workflow ./WORKFLOW.local.md \
  --logs-root ./.symphony/logs --log-level info
```

Or as the managed macOS service (see
[the repository-services guide](macos-services.md)):

```sh
./scripts/install          # builds and installs ~/.local/bin/symphony
symphony service install   # registers this repository's LaunchAgent
```

Observation surfaces, in increasing order of detail:

- `symphony tui` -- the read-only operator dashboard; it can mutate nothing.
- `.symphony/service/status.json` -- the versioned, redacted runtime snapshot.
  Note that `operator.EffectiveConfig` is **not** in here: it comes from a live
  re-parse of the workflow file, so a question about effective configuration
  must be answered from the file or the TUI, never from the status snapshot.
- `.symphony/logs/symphony.jsonl` -- the full structured log, appended across
  runs rather than rotated per run.

## 5. The board

The lifecycle is `Todo -> In Progress -> In Review <-> Rework -> Merging ->
Done`. `In Review` is the human gate: it is deliberately excluded from
`active_states`, so Symphony never dispatches it and no agent capability can
move an issue into or out of it. Essentially everything the operator does is a
transition into or out of that one state.

Board conventions that are not derivable from the code:

- **Never rewrite or delete existing issue prose.** Every issue's value is its
  evidence -- line numbers, log excerpts, measured counts. To retract or narrow
  a claim, *prepend* a `>` blockquote header and leave the original underneath
  as the record of what was observed.
- **`Duplicate` is a distinct status from `Canceled`.** Use `Duplicate` for a
  merged-away issue (set `duplicateOf`, which moves the status by itself --
  setting `state: Duplicate` first fails), and reserve `Canceled` for "we
  decided not to do this".
- **A blocker clears only when its state is in `terminal_states`**, which is
  `[Done, Canceled]`. An issue parked in `Duplicate` therefore blocks its
  dependents permanently and silently. Before closing anything as a duplicate,
  check what it `blocks` and re-point the edge to the survivor.
- `blockedBy` gates dispatch **only for `Todo`**, so adding relations to Backlog
  issues is free until promotion -- and far better than encoding sequencing in
  description prose the tracker cannot enforce.

## 6. Failure modes that have actually happened

**Rework re-renders the issue description into the prompt.** Correcting a
mistake in a PR comment without also correcting the issue gives the next attempt
two contradictory instructions, and the description wins. If review finds an
*acceptance criterion* was wrong, strike it in the issue before requesting
rework; a PR comment alone will not do it. Reconcile a stale description at
*promotion* time, too -- an edit made after dispatch arrives too late to affect
the attempt it was meant to fix.

**Never rebase a branch whose pull request already exists.**
`github_publish_pr` pushes without `--force`, so a rebase rewrites commits the
remote already has and every later publish is refused as a non-fast-forward --
unfixable from inside a session. Use `git merge origin/main`, which satisfies
the same descends-from-base gate while keeping the push a fast-forward. That
doubles as the fix for a base that went stale during an outage: the merge also
produces the push event that generates a fresh CI run.

**Use `--detach` for every operator review worktree.** Creating or deleting a
`refs/heads/*` ref while the daemon runs trips the PMR-65 integrity alert and
produces a false ERROR. `symphony/*` refs are excluded from that alert;
`claude/*` and personal-prefix branches are not.

**Do not hand-author a branch named `symphony/pmr-<NN>`.** That shape is the
daemon's ownership signal, and workspace recovery will adopt the branch as its
own agent's work -- committing onto it, or opening a competing pull request.

**Linear timestamps are UTC (`...Z`); `symphony.jsonl` is machine-local.** A
state change at `09:33Z` and a dispatch logged at `11:23+02:00` are ten minutes
apart, not two hours. More than one "the daemon dispatched an issue parked for
hours" conclusion has been this and nothing else.

**Check for a second operator before mutating shared state.** Another agent
session in the same checkout will silently undo board edits and worktree
changes. Run `ps -eo pid,ppid,etime,command | grep "[c]laude"` and compare
working directories before the first mutation, not after something looks wrong.

**Expect the provider usage wall.** With `backend: claude` and four concurrent
agents, a five-hour usage window has been exhausted in roughly two hours of real
work. Since PMR-131 a rejection is its own classified terminal event, retried
from the window reset rather than from the backoff ladder and exempt from the
`max_attempts` ceiling, so the wall is now visible and cheap rather than
invisible and expensive. Lower `max_concurrent_agents` to stretch a session.

**A host-side capability refusal carries no reason across the MCP boundary.**
The agent sees a bare "rejected". One cheap probe separates the two causes: send
a minimal payload with every field set to a single word. If that is rejected
identically it is not argument validation, so it is a host-side block -- stop
retrying with reworded bodies and read the daemon log instead.

## 7. Stale workspaces

Symphony refuses to delete a workspace it cannot prove is safe: one with
uncommitted changes, one whose HEAD has moved off the recorded base commit, or
one whose recorded source path no longer identifies the same repository. Each
refusal logs `terminal workspace cleanup failed` on every restart, so they
accumulate as a visible backlog rather than as silent data loss. That is the
guard working, not a bug.

Clearing the backlog is an operator decision, and the safe order is archive,
then remove:

```sh
d=.symphony/workspaces/PMR-NN
git -C "$d" status --porcelain                 # uncommitted work?
git for-each-ref --contains "$(git -C "$d" rev-parse HEAD)" refs/remotes/origin
```

If the HEAD is reachable from no `origin` ref, bundle it before deleting
anything (`git -C "$d" bundle create ... origin/main..HEAD`), along with
`git diff HEAD` and any untracked files. Then `git worktree remove --force`,
`git worktree prune`, and delete the matching record under
`.symphony/workspaces/.symphony-state/` -- that record is what regenerates the
warning, so leaving it behind leaves the warning behind.

## 8. Where things stood on 2026-08-27

Eleven issues landed on 2026-08-26/27, closing every then-known breaking bug in
the dispatch, publish, and landing paths along with the operator-visibility gaps
that had made those bugs hard to diagnose. Both backends are proven end to end:
Codex published and landed its own pull requests during a deliberate validation,
with the single asymmetry filed as PMR-155.

The backlog at that point held no known-breaking work. It is mostly structural
(PMR-123's owned per-issue record, PMR-107's duplicated declarations),
diagnosability (PMR-154's discarded errors, PMR-150's rate-limit vocabulary),
and cost telemetry (PMR-151, PMR-153, PMR-155). PMR-119 and PMR-144 were
deliberately left in Backlog rather than promoted.

The orchestration method itself -- how to plan a wave, get it approved, dispatch
it, review it, and land it -- is written down as a skill:
[`.claude/skills/symphony-dogfood`](../.claude/skills/symphony-dogfood/SKILL.md).
