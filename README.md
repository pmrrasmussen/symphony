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

See [docs/architecture.md](docs/architecture.md) and
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
