# Live Linear and Codex smoke profile

The normal development and CI commands are credential-free and never contact
Linear or launch Codex:

```sh
go test ./...
go vet ./...
```

Run `go test -race ./...` locally when practical. Race CI runs the same suite
with a finite timeout now that the Codex and coordinator process lifecycles are
bounded and cancellation-safe.

The live profile is deliberately separate. It verifies configuration and a
local full-lifecycle preflight with `--dry-run`; that flag does not contact
Linear, launch Codex, execute hooks, or prepare a workspace. This
keeps the default live check read-only and prevents the profile from consuming
Codex capacity or changing an issue by accident.

## Prerequisites

- Use a dedicated, disposable Linear project that contains only smoke-test
  issues. Never use a production project, team, workspace, or customer data.
- Create a least-privilege Linear credential for that dedicated project and
  export it as `LINEAR_API_KEY` only in the shell or CI environment that runs
  the smoke test. Long-running local operation should instead set
  `SYMPHONY_LINEAR_API_KEY_FILE` to an absolute mode-600 credential file path.
- For a future non-dry Codex exercise, authenticate the Codex CLI with its
  normal supported mechanism. In GitHub Actions, store that credential as the
  `OPENAI_API_KEY` secret; do not print it, place it in a workflow file, or
  include it in an issue prompt.
- Keep logs and generated workspaces private. Delete the dedicated test issue
  and `.symphony/smoke-workspaces` after a non-dry run.

The checked-in template, [WORKFLOW.smoke.example.md](../WORKFLOW.smoke.example.md),
contains no project identifier or credential. It is not the default workflow.

## Local preflight

Copy the template to an ignored local location, replace
`__LINEAR_SMOKE_PROJECT_SLUG__` with the dedicated project's slug, and use a
placeholder key. The preflight requires the field but does not send its value.
Do not put a live credential in the copied file.

```sh
mkdir -p .symphony
cp WORKFLOW.smoke.example.md .symphony/smoke-workflow.md
# Edit .symphony/smoke-workflow.md and replace __LINEAR_SMOKE_PROJECT_SLUG__.
LINEAR_API_KEY=preflight go run ./cmd/symphony --dry-run .symphony/smoke-workflow.md
```

A successful command emits a structured preflight result and does not verify
live Linear connectivity. Treat an attempted command that returns non-zero as
**failed**. The preflight does not generate smoke workspaces; remove the copied
workflow when the test ends.

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

## Opt-in local full-lifecycle landing exercise

This is a separate, mutating smoke profile for validating PR landing. Do
not extend the checked-in `WORKFLOW.smoke.example.md` or the `live-smoke` CI
job with these capabilities; the existing CI profile must remain read-only.
Create an ignored local workflow for each exercise, and opt in only after its
`--dry-run` preflight succeeds.

The local full-lifecycle profile must:

- target both a dedicated disposable Linear project and a dedicated
  disposable GitHub repository, never production artifacts;
- configure the host-owned `tracker.provider.transitions.start` edge from
  `Todo` to `In Progress`, the
  `tracker.provider.transitions.refuse_landing` edge from `Merging` to
  `In Review`, and `handoff_state: In Review`;
- include `Merging` in `active_states` so landing work is dispatched, while
  excluding `In Review` so human review remains non-dispatchable;
- set `github.merge_state: Merging`, choose an explicit `github.merge_method`,
  and list the exact check names reported by the disposable repository in
  `github.required_checks`;
- provide a state-specific `Merging` prompt branch that calls the zero-argument
  `github_land_pr` tool. That branch must not rebuild the change or call
  `github_publish_pr`; a pending-check result waits in `Merging`, while a hard
  refusal uses the host-owned fallback to `In Review`.

Create one disposable issue in `Todo` and observe the complete path: the host
starts it in `In Progress`, implementation publishes its pull request to
`In Review`, a human moves it to `Merging`, and the next dispatch lands that
same pull request and reconciles the issue to `Done`. This final dispatch is
what distinguishes a full-lifecycle landing smoke from another implementation
handoff exercise. Stop the service and remove all disposable artifacts when
the run finishes.

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
go run ./cmd/symphony --dry-run "$RUNNER_TEMP/symphony-smoke-workflow.md"
```

Its command does not echo the secret, and workflow files must continue to
avoid shell tracing (`set -x`) or diagnostic dumps of environments, workflow
files, request payloads, or agent prompts.
