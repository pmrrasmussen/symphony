# Symphony (Go)

Symphony is a long-running, Codex-only issue runner. It reads repository-owned
policy from `WORKFLOW.md`, polls Linear, and runs each eligible issue in a
deterministic local workspace.

```sh
go run ./cmd/symphony --workflow ./WORKFLOW.md
# Equivalent positional workflow-file form:
go run ./cmd/symphony ./WORKFLOW.md
```

## Working with Symphony

1. Create one focused Linear issue, move it to **In Progress**, and work from
   an isolated Git worktree—not the primary checkout.
2. Keep the workflow policy in the repository. Configure
   `workspace.source_root` for the source repository; Symphony creates a
   separate agent workspace for each eligible issue.
3. Validate the narrow change first, then run broader checks when shared
   behavior changes. Review the generated workspace before keeping its work.
4. Open a PR with **Why**, **What changed**, and **On Call**; merge only
   after required checks and review, then move the Linear issue to **Done**.
5. Use `--dry-run` before any live run. Live smoke tests are manual and must
   use dedicated Symphony test artifacts, never Dagligvare-app.

For the delivery sequence, see [HOW_WE_WORK.md](HOW_WE_WORK.md). For runtime
configuration and operational details, use [WORKFLOW.example.md](WORKFLOW.example.md)
and [docs/architecture.md](docs/architecture.md).

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
[workspace ownership and recovery guide](docs/completion-markers.md), and
[WORKFLOW.example.md](WORKFLOW.example.md).

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

Use a fine-grained token restricted to that repository. Symphony removes the
resolved token (including inherited environment values containing it) from the
Codex child environment. The dynamic tool accepts no issue, repository,
branch, or credential input: it verifies the worktree's credential-free GitHub
origin and committed clean changes, pushes
`symphony/<lowercase-issue-identifier>`, links the PR, and moves only the active
issue to the configured review state. A confirmed human merge moves that
linked issue to `Done`; closing without merge leaves it in review and emits a
warning. Invalid or incomplete GitHub settings disable the tool, preserving
the manual workflow. In host-publish mode, workers create local commits but do
not use `gh` or `git push`; they invoke the zero-argument host capability.
Without it, Symphony tells workers that PR delivery is unavailable and reports
the missing configuration rather than asking them to publish directly.
Symphony never merges pull requests.
