# Symphony (Go)

Symphony is a long-running, Codex-only issue runner. It reads repository-owned
policy from `WORKFLOW.md` -- this repository's single executable source of
delivery policy -- polls Linear, and runs each eligible issue in a
deterministic local workspace.

```sh
go run ./cmd/symphony --workflow ./WORKFLOW.md
# Equivalent positional workflow-file form:
go run ./cmd/symphony ./WORKFLOW.md
```

## Canonical lifecycle

`Todo -> In Progress -> In Review <-> Rework -> Merging -> Done`. `Todo`,
`In Progress`, `Rework`, and `Merging` are active/dispatchable; `In Review` is
the single, fixed, human-controlled review state and is never dispatched;
`Done` and `Canceled` are terminal. A Codex session:

1. Moves a `Todo` issue to `In Progress`, then implements and validates the
   change.
2. Publishes a structured pull request (`github_publish_pr`) and hands the
   issue to `In Review` once validated.
3. Resumes -- in the same worktree, branch, and pull request -- when a human
   moves the issue to `Rework`, and republishes to hand it back to
   `In Review`.
4. Is dispatched again, with only the bounded zero-argument `github_land_pr`
   tool, when a human moves the issue to `Merging`: that move is itself the
   approval to land. Pending checks wait without changing Linear state; any
   other hard gate returns the issue to `In Review`; a successful or
   already-completed merge reconciles the issue to `Done`.

Two-agent operation: one implementation/rework agent may run concurrently
with one landing agent (`agent.max_concurrent_agents: 2`,
`agent.max_concurrent_agents_by_state: {Merging: 1}`), while a delayed retry
never occupies a concurrency slot as it waits.

See `WORKFLOW.md`'s prompt body for the full per-state start, implementation,
validation, review handoff, rework, landing, and completion playbook, and
[docs/architecture.md](docs/architecture.md) for the underlying trust model.

## Working with Symphony

1. Create one focused Linear issue in `Todo` (or move it to `In Progress`
   yourself), and work from an isolated Git worktree—not the primary
   checkout.
2. Keep the workflow policy in the repository. Configure
   `workspace.source_root` for the source repository; Symphony creates a
   separate agent workspace for each eligible issue.
3. Validate the narrow change first, then run broader checks when shared
   behavior changes. Review the generated workspace before keeping its work.
4. Publish a PR with **Why**, **What changed**, and **On Call** (via
   `github_publish_pr`, or manually when host-side publishing is not
   configured); merge only after required checks and review. Moving the
   issue to `Merging` dispatches `github_land_pr`, which lands the PR and
   moves the issue to **Done** automatically; without GitHub landing
   configured, move the Linear issue to **Done** manually after merging.
5. Use `--dry-run` before any live run. Live smoke tests are manual and must
   use dedicated Symphony test artifacts, never Dagligvare-app.

For runtime configuration and operational details, use
[WORKFLOW.example.md](WORKFLOW.example.md) and
[docs/architecture.md](docs/architecture.md).

Run a full-lifecycle local preflight against the example without live
credentials:

```sh
SYMPHONY_LINEAR_API_KEY_FILE=/path/to/a/mode-600-key-file \
  go run ./cmd/symphony --dry-run ./WORKFLOW.example.md
```

`--dry-run` emits a structured result for workflow parsing, tracker selection,
workspace and log roots, hook syntax, executable availability, and a synthetic
scheduler lifecycle. It does not contact Linear, execute hooks, start Codex, or
create configured logs or workspaces. A missing future root is a warning; an
invalid boundary is a failure and exits non-zero. The referenced file is read
only to validate required configuration and is never sent anywhere during
preflight.

Live Linear and Codex smoke testing is always opt-in and uses a dedicated
disposable project; see [the live smoke profile](docs/live-smoke.md).
Never run a Symphony smoke test against Dagligvare-app or any of its Linear
workspace, team, or projects.

Linear configuration uses canonical `tracker.provider.project_slug_id`.
Legacy `project_slug` remains supported during its documented deprecation and
emits a value-free migration warning; configuring both names is rejected.
For credentials, prefer
`tracker.provider.api_key_file: $SYMPHONY_LINEAR_API_KEY_FILE`, where the
environment value is an absolute path to a mode-600 file outside the
repository. `tracker.provider.api_key: $LINEAR_API_KEY` remains supported when
the deployment injects the credential value directly. A literal trusted local
`api_key_file` path is also supported for non-versioned workflow files.

`WORKFLOW.md` front matter is validated for the supported core fields while
unknown extension fields are preserved. Changes are reloaded for future work;
an invalid replacement keeps the last valid configuration. Reload detection
includes referenced environment values and `api_key_file` contents, so those
inputs can change without touching `WORKFLOW.md`. Environment-backed fields use
exact `$VARNAME` syntax; braced or compound forms are rejected instead of being
treated as literal paths or secrets. Inline and file-backed API keys are
whitespace-trimmed before use and are never included in reload logs.

Each successful reload publishes one complete settings snapshot. Settings
reads that start afterward use the new polling, state, concurrency, hook,
workspace, and Codex policy. An already-started Codex process keeps the command,
sandbox, and timeout values captured for its launch; new concurrency limits
govern later admissions, while current state and stall policies continue to be
applied by reconciliation. `--logs-root` selects the process log destination at
startup and is not a reloadable `WORKFLOW.md` field.

Prompt templates use strict, lowercase variables: `issue` (for example
`{{.issue.identifier}}`) and `attempt` (nil on the first run, then a 1-based
retry/continuation number). Template errors fail only that run attempt.
Relative workspace and log paths are resolved from the workflow file; omitted
`workspace.root` defaults to the system temporary directory's
`symphony_workspaces` path.

See [docs/architecture.md](docs/architecture.md), the
[Linear tracker profile](docs/linear-tracker.md), the
[workspace ownership and recovery guide](docs/completion-markers.md), the
[observability guide](docs/observability.md), and
[WORKFLOW.example.md](WORKFLOW.example.md).

By default the structured log at `--logs-root` (`.symphony/logs/symphony.jsonl`)
stays concise. Pass `--log-level debug` for actionable detail on an
apparently idle run: categorized poll admission/rejection summaries, safe
Codex tool/item lifecycle records, and heartbeat/stall records naming the
outstanding operation. See [docs/observability.md](docs/observability.md) for
`tail`/`jq` examples and the full level and redaction contract.

For ongoing repository development, configure `workspace.source_root: .`.
Symphony then creates one detached Git worktree per issue beneath
`workspace.root`; the original checkout is never used as an agent workspace.
Create focused issues in the configured Linear project and move them to `Todo`
or `In Progress` to make them eligible. Symphony keeps active issues eligible
across restarts and bounded retries. Invalid or missing ownership state beside
an existing workspace fails closed; follow the [workspace recovery
procedure](docs/completion-markers.md) before redispatch.
Review each worktree's changes before merging or cherry-picking them into your
development branch. Terminal cleanup preserves worktrees with uncommitted,
untracked, or newly committed changes rather than deleting work that needs
review.

To let a Codex session hand an issue off safely, optionally configure
`tracker.provider.handoff_state` (and, if useful, a fixed
`handoff_comment_template`). This enables a session-bound compatibility tool,
not general Linear or GraphQL access; see the Linear tracker profile for its
strict scope.

To let a Codex session split a task into independently reviewable pull
requests, optionally configure `tracker.provider.child_issue_creation: true`.
This enables a session-bound `create_child_issue` tool, not general Linear
issue creation: it can only create a new issue in the active issue's already
configured Linear project and team, always records the active issue as the
new issue's Linear parent, and accepts only a bounded set of fields (title,
description, priority, labels drawn from the team's existing labels, and
dependencies on issues the same session already created). The intended
pattern is one child issue per isolated Symphony worktree and pull request;
see the [Linear tracker profile](docs/linear-tracker.md) for its exact scope.

An optional host-side GitHub integration can publish a completed, clean
worktree and create or reuse its pull request. It requires the Linear handoff
policy above and a fixed repository configuration:

```yaml
github:
  owner: pmrrasmussen
  repository: symphony
  base_branch: main
  token_file: $SYMPHONY_GITHUB_TOKEN_FILE
  # Alternatively: token: $SYMPHONY_GITHUB_TOKEN
  poll_interval_ms: 30000
```

Use a fine-grained token restricted to that single repository, granting only
the pull request, checks, contents, and review permissions the integration
needs. Store it in a mode-600 file outside the repository and reference that
path with `$SYMPHONY_GITHUB_TOKEN_FILE`; the token is never committed to the
repository. Symphony removes the resolved token (including inherited
environment values containing it) from the Codex child environment, so it is
never exposed to the child process. The `github_publish_pr` dynamic tool
accepts no repository, branch, issue, or credential input, only bounded `why`,
`what_changed`, and `on_call` structured handoff fields: it verifies the
worktree's credential-free GitHub origin and committed clean changes, pushes
`symphony/<lowercase-issue-identifier>`, creates or reuses that branch's pull
request with a deterministic `Why`/`What changed`/`On Call` body plus the
bound Linear issue URL, links the PR, and moves only the active issue to the
configured review state. Repeat publication with the same structured fields
leaves an already-correct body untouched; changed fields update it in place. A
pull request that was merged is irrecoverable and rejected; one that was
closed without merging is reopened. A companion read-only `github_pr_context`
dynamic tool, bound to the same issue, repository, branch, and pull request,
reports bounded check status, effective review state, and redacted
comment/review excerpts and unresolved review-thread counts, with no
repository, issue, branch, or pull request selection of its own. A confirmed
human merge moves the linked issue to `Done`; closing without merge leaves it
in review and emits a warning. Invalid or incomplete GitHub settings disable
both tools, preserving the manual workflow. In host-publish mode, workers
create local commits but do not use `gh` or `git push`; they invoke these
host capabilities instead. Without them, Symphony tells workers that PR
delivery is unavailable and reports the missing configuration rather than
asking them to publish directly.

Symphony can also land an already-approved pull request itself once
`github.merge_state`, a bounded `github.merge_method` (`merge`, `squash`, or
`rebase`; defaults to `merge`), and a non-empty `github.required_checks` list
are configured:

```yaml
github:
  # ...owner/repository/base_branch/token as above...
  merge_state: Merging
  merge_method: merge
  required_checks: [ci/build, ci/test]
```

Unlike the rest of the `github:` block, an invalid `merge_state`,
`merge_method`, or `required_checks` value rejects the whole workflow instead
of silently disabling the feature, the same fail-closed treatment as
`tracker.provider.agent_transitions`. `merge_state` must be an
`active_state` and differ from `handoff_state` and every terminal state
(`Merging` in the canonical lifecycle above): a session only receives the
tool once actually dispatched for an issue currently in that exact state. A
session bound to an issue currently in the exact configured `merge_state` receives a
zero-argument `github_land_pr` tool: it re-verifies the worktree, branch,
pull request, required checks, effective review state (moving the issue to
`merge_state` is itself the human
approval; no separate approving review is required), unresolved review
threads, mergeability, and current base immediately before merging, and
transitions only the bound issue to `Done` on success. Pending checks wait
without changing Linear state. With `github.update_stale_branch: true`, one
clean stale-base update also waits for checks on its new head; any other hard
gate falls back to the configured `merge_state -> In Review` transition.
Duplicate landing calls, and a GitHub merge that succeeds despite a failed Linear
completion, are reconciled idempotently rather than merging or transitioning
twice. Symphony never merges a pull request outside this narrow, explicitly
configured capability.

## Operator prerequisites for the canonical lifecycle

`WORKFLOW.md` is fully configured for the canonical lifecycle, but going live
against this repository's real Linear team requires the following manual,
human-gated steps; none of them can be performed by Symphony, by this MCP
integration (which only reads and lists issue statuses), or by an automated
agent:

1. **Create the `Rework` and `Merging` Started states** in the Linear team
   used by `tracker.provider.project_slug_id`. Until both states exist,
   issues can reach only `Todo`, `In Progress`, and `In Review`; `--dry-run`
   does not contact Linear and cannot detect a missing remote state, so this
   is a live-run precondition, not something automated validation checks.
2. **Provision the repository-scoped GitHub token file** referenced by
   `$SYMPHONY_GITHUB_TOKEN_FILE`: a fine-grained personal access token
   restricted to exactly this repository, granting only the pull request,
   checks, contents, and review permissions described above, saved to a
   mode-600 file outside the repository.
3. **Confirm the configured `github.required_checks` names** match what
   GitHub actually reports for this repository. `WORKFLOW.md` uses this
   repository's current CI job names (`scripts/check format`,
   `scripts/check test`, `scripts/check vet`, `scripts/check race`, from
   `.github/workflows/ci.yml`); update both files together if CI job names
   change.

Until all three are complete, Symphony keeps running safely: two-agent
capacity and the `Todo`/`In Progress`/review-handoff path already work today,
an issue simply never reaches `Rework` or `Merging` in Linear (Linear itself
has no such state to move it to), `github_land_pr` is never advertised, and
completion continues through the existing human-merge fallback described
above.
