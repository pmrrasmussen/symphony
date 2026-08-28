# Live Linear and agent smoke profile

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

`--dry-run` verifies the Codex credential the same way it verifies Claude's,
below: it runs the read-only `codex login status` and reads only its exit
code. That reflects a login the CLI itself persisted -- `codex login` or
`codex login --with-api-key` -- not a bare `OPENAI_API_KEY` export with no
stored login, which `codex login status` reports as logged out even though
the app-server picks the variable up directly at spawn time. If you rely on
the export alone rather than persisting it with `codex login --with-api-key`,
treat a failing `agent_authentication` check as inconclusive for this backend
and confirm authentication some other way before running this live -- an
unauthenticated app-server fails at `thread/start` or mid-turn instead, which
still consumes a dispatch and a workspace.

```sh
export LINEAR_API_KEY
export OPENAI_API_KEY
go run ./cmd/symphony --workflow .symphony/smoke-workflow.md
```

Interrupt the process, inspect the private logs, and remove the temporary
workflow, smoke worktree, and smoke issue. Never upload raw logs or full agent
prompts: they can contain sensitive issue content even though Symphony redacts
known credential values.

## Deliberate Claude exercise

The same shape as the Codex exercise above, with the differences that matter.
Run it only after `--dry-run` succeeds against a Claude workflow, and only with a
disposable issue.

Set `agent.backend: claude` and a `claude:` block in the copied smoke workflow
(see `WORKFLOW.example.md`). There is no API key to export: the Claude backend
authenticates through the operator's own CLI login, which is why `--dry-run`
includes a bounded, read-only `agent_authentication` check. If that check does
not pass, fix the login before running anything live -- a session that cannot
authenticate still consumes a dispatch and a workspace.

```sh
export SYMPHONY_LINEAR_API_KEY_FILE
go run ./cmd/symphony --workflow .symphony/smoke-workflow.md
```

Three things to confirm afterwards, none of which the Codex exercise covers:

- **The capability endpoint is absent from the record.** Neither the endpoint URL
  nor its bearer token may appear in any log line, event, or status snapshot. The
  token is passed to the child through its environment and the URL through
  `--mcp-config`; both are per turn. Grep the private logs for the port and for
  `SYMPHONY_MCP_TOKEN` and expect nothing.
- **The tool names the model saw carry the MCP prefix.** A Claude worker reaches
  Symphony's bounded capabilities as `mcp__symphony__<tool>`, so a log search for
  a bare `github_publish_pr` finds the host's own records and not the model's
  calls. `--dry-run`'s `github_handoff` check prints the name this backend
  actually uses.
- **The issue reached the handoff state with a pull request attached**, and the
  transition was applied by the host rather than the agent. No agent-facing
  transition capability exists on either backend.

Then remove the temporary workflow, the worktree, and the smoke issue, and stop
the process. Never upload raw logs or full agent prompts.

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
handoff exercise. Before cleanup, verify that the disposable source checkout
is still on its default branch and has a clean worktree, proving the exercise
remained isolated. Stop the service and remove all disposable artifacts when
the run finishes.

## Live Claude sandbox boundary verification (PMR-156)

`internal/claude.TestLiveClaudeSandboxDeniesEditAndWriteOutsideWriteRoots` is a
separate, opt-in verification that a live `claude` session cannot use `Edit` or
`Write` to escape its granted write roots. It is not part of `go test ./...`:
it spawns the real CLI against a real, credentialed account and consumes real
usage on every run, so `go test` skips it unless
`SYMPHONY_LIVE_CLAUDE_SANDBOX_FIXTURE` is set.

Point that variable at an existing, writable directory **outside `/tmp`**. The
CLI's sandbox permits `/tmp` by default, and building this fixture there
previously produced a false "the sandbox is not enforced at all" conclusion on
this exact issue -- see the test's own doc comment for the full account. A
process already running inside a sandboxed agent session cannot reliably
create such a location for itself, which is why this is a manual step rather
than something the test provisions on its own:

```sh
mkdir -p ~/symphony-sandbox-fixture
SYMPHONY_LIVE_CLAUDE_SANDBOX_FIXTURE=~/symphony-sandbox-fixture \
  go test ./internal/claude/... -run TestLiveClaudeSandboxDeniesEditAndWriteOutsideWriteRoots -v
```

The test builds a disposable source repository and linked worktree under that
directory, launches a live turn instructed to overwrite a tracked file in the
source working tree with both `Bash` and `Write`, and asserts the file is
byte-for-byte unchanged afterward -- checking the CLI's actual enforcement on
disk, not merely what the model's own report claims. It intentionally does not
exercise `refs/heads/*`, `packed-refs`, or the primary index: those remain
writable through `Bash` today, a CLI-level gap the "Sandbox ownership decision
(PMR-85)" section of [architecture.md](architecture.md) records as open and
declines to close without a separate, explicit OS-sandbox decision. Remove the
fixture directory's contents when finished; nothing in it is durable state.

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
