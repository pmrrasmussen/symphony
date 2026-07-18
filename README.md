# Symphony (Go)

Symphony is a long-running, Codex-only issue runner. It reads repository-owned
policy from `WORKFLOW.md`, polls Linear, and runs each eligible issue in a
deterministic local workspace.

```sh
go run ./cmd/symphony --workflow ./WORKFLOW.md
```

Run with `--dry-run` to validate configuration and scheduling without starting
Codex.  A live smoke test is opt-in: set `LINEAR_API_KEY`, configure a test
Linear project in `WORKFLOW.md`, and use `--dry-run` first.

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
