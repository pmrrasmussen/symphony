# Symphony (Go)

Symphony is a long-running, Codex-only issue runner. It reads repository-owned
policy from `WORKFLOW.md`, polls Linear, and runs each eligible issue in a
deterministic local workspace.

```sh
go run ./cmd/symphony --workflow ./WORKFLOW.md
```

## Working with Symphony

1. Create one focused Linear issue, move it to **In Progress**, and work from
   an isolated Git worktree—not the primary checkout.
2. Keep the workflow policy in the repository. Configure
   `workspace.source_root` for the source repository; Symphony creates a
   separate agent workspace for each eligible issue.
3. Validate the narrow change first, then run broader checks when shared
   behavior changes. Review the generated workspace before keeping its work.
4. Open a PR with **Why**, **What was changed**, and **On Call**; merge only
   after required checks and review, then move the Linear issue to **Done**.
5. Use `--dry-run` before any live run. Live smoke tests are manual and must
   use dedicated Symphony test artifacts, never Dagligvare-app.

For the delivery sequence, see [HOW_WE_WORK.md](HOW_WE_WORK.md). For runtime
configuration and operational details, use [WORKFLOW.example.md](WORKFLOW.example.md)
and [docs/architecture.md](docs/architecture.md).

Run with `--dry-run` to validate configuration and scheduling without starting
Codex. A live smoke test is opt-in: set `LINEAR_API_KEY`, configure a dedicated
Symphony test project in `WORKFLOW.md`, and use `--dry-run` first. Never run a
Symphony smoke test against Dagligvare-app or any of its Linear workspace,
team, or projects. Report a smoke test as **skipped** when its dedicated test
credential or project is unavailable; report it as **failed** when an attempted
test command or validation fails. A skipped smoke test is not a passed test.

Linear accepts `tracker.provider.api_key: $LINEAR_API_KEY` or a trusted local
`tracker.provider.api_key_file`; the latter is read only by the service.

`WORKFLOW.md` front matter is validated for the supported core fields while
unknown extension fields are preserved. Changes are reloaded for future work;
an invalid replacement keeps the last valid configuration. Prompt templates use
strict, lowercase variables: `issue` (for example
`{{.issue.identifier}}`) and `attempt` (nil on the first run, then a 1-based
retry/continuation number). Template errors fail only that run attempt.
Relative workspace and log paths are resolved from the workflow file; omitted
`workspace.root` defaults to the system temporary directory's
`symphony_workspaces` path.

See [docs/architecture.md](docs/architecture.md), the
[Linear tracker profile](docs/linear-tracker.md), and
[WORKFLOW.example.md](WORKFLOW.example.md).

For ongoing repository development, configure `workspace.source_root: .`.
Symphony then creates one detached Git worktree per issue beneath
`workspace.root`; the original checkout is never used as an agent workspace.
Create focused issues in the configured Linear project and move them to `Todo`
or `In Progress` to make them eligible. Symphony records a completed turn and
will not rerun an unchanged issue; editing the issue makes it eligible again.
Review each worktree's changes before merging or cherry-picking them into your
development branch. Terminal cleanup preserves worktrees with uncommitted,
untracked, or newly committed changes rather than deleting work that needs
review.

To let a Codex session hand an issue off safely, optionally configure
`tracker.provider.handoff_state` (and, if useful, a fixed
`handoff_comment_template`). This enables a session-bound compatibility tool,
not general Linear or GraphQL access; see the Linear tracker profile for its
strict scope.
