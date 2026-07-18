# Live Linear and Codex smoke profile

The normal development and CI commands are credential-free and never contact
Linear or launch Codex:

```sh
go test ./...
go vet ./...
```

Run `go test -race ./...` locally when practical. Race CI is deferred to
PMR-13 because the Codex app-server process lifecycle can hang on Linux; it is
not a required GitHub Actions check until that repair is complete.

The live profile is deliberately separate. It verifies configuration and a
single Linear poll with `--dry-run`; that flag does not launch Codex. This
keeps the default live check read-only and prevents the profile from consuming
Codex capacity or changing an issue by accident.

## Prerequisites

- Use a dedicated, disposable Linear project that contains only smoke-test
  issues. Never use a production project, team, workspace, or customer data.
- Create a least-privilege Linear credential for that dedicated project and
  export it as `LINEAR_API_KEY` only in the shell or CI environment that runs
  the smoke test.
- For a future non-dry Codex exercise, authenticate the Codex CLI with its
  normal supported mechanism. In GitHub Actions, store that credential as the
  `OPENAI_API_KEY` secret; do not print it, place it in a workflow file, or
  include it in an issue prompt.
- Keep logs and generated workspaces private. Delete the dedicated test issue
  and `.symphony/smoke-workspaces` after a non-dry run.

The checked-in template, [WORKFLOW.smoke.example.md](../WORKFLOW.smoke.example.md),
contains no project identifier or credential. It is not the default workflow.

## Local Linear smoke

Copy the template to an ignored local location, replace
`__LINEAR_SMOKE_PROJECT_SLUG__` with the dedicated project's slug, and export
the credential in the current shell. Do not put the credential in the copied
file.

```sh
mkdir -p .symphony
cp WORKFLOW.smoke.example.md .symphony/smoke-workflow.md
# Edit .symphony/smoke-workflow.md and replace __LINEAR_SMOKE_PROJECT_SLUG__.
export LINEAR_API_KEY
go run ./cmd/symphony --workflow .symphony/smoke-workflow.md --dry-run
```

A successful command prints that the configuration and Linear poll succeeded
and that Codex was not started. Treat a missing credential or project as
**skipped**, not passed. Treat an attempted command that returns non-zero as
**failed**. Remove the copied workflow and generated smoke workspaces when the
test ends.

## Deliberate Codex exercise

Only run this after the dry run succeeds and only with a disposable issue whose
prompt is safe to process. Confirm that the issue asks Codex to make no changes
and that the project contains no other eligible issues. The service runs until
interrupted, so stop it after the one expected issue has been observed.

```sh
export LINEAR_API_KEY
export OPENAI_API_KEY
go run ./cmd/symphony --workflow .symphony/smoke-workflow.md
```

Interrupt the process, inspect the private logs, and remove the temporary
workflow, smoke worktree, and smoke issue. Never upload raw logs or full agent
prompts: they can contain sensitive issue content even though Symphony redacts
known credential values.

## GitHub Actions smoke

The `live-smoke` job is not triggered by pushes or pull requests. It runs only
when a maintainer manually dispatches **CI**, checks `run_live_smoke`, and
supplies the disposable project's slug. The protected `live-smoke` environment
must define this secret:

- `LINEAR_API_KEY` — the credential for the dedicated smoke project.

For an explicitly added non-dry Codex job, also store the Codex CLI credential
as `OPENAI_API_KEY` in the protected environment. Do not add that credential
to the read-only smoke job.

The job prepares a temporary workflow and runs:

```sh
go run ./cmd/symphony --workflow "$RUNNER_TEMP/symphony-smoke-workflow.md" --dry-run
```

Its command does not echo the secret, and workflow files must continue to
avoid shell tracing (`set -x`) or diagnostic dumps of environments, workflow
files, request payloads, or agent prompts.
