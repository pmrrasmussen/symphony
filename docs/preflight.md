# The `--dry-run` preflight

`--dry-run` validates a workflow file and a synthetic full lifecycle without
contacting Linear, launching an agent, or touching the filesystem paths the
workflow configures. It is the check to run before any live run and before any
configuration change reaches a running daemon.

```sh
SYMPHONY_LINEAR_API_KEY_FILE=/path/to/a/mode-600-key-file \
  go run ./cmd/symphony --dry-run ./WORKFLOW.example.md
```

## What it checks, and what it deliberately does not do

The command emits one structured result. Its checks are `workflow` (parsing and
validation), `tracker` (provider selection and credential resolution),
`github_handoff` (the configured handoff and GitHub capabilities, reported under
the names the selected backend actually serves them as), `workspace_root`,
`workspace_source`, `log_root`, `status_file` (see
[runtime status](runtime-status.md)), `agent_command` (command syntax and
executable availability), `agent_authentication`, `hooks` (syntax only), and
`scheduler_lifecycle`, a synthetic active issue run against fakes with no agent
backend, router, capability registry, or capability transport started.

It does not contact Linear, execute hooks, start an agent session, or create
the configured logs or workspaces. The referenced workflow file is read only to
validate required configuration and is never sent anywhere. A missing future
root is a warning; an invalid boundary is a failure and exits non-zero. Treat a
non-zero exit as a failed preflight.

Hook scripts are validated with `sh -n`, a shell syntax check. That is exact
for `codex.command`, which runs through `bash -c`, and a loose superset for
`claude.command`, which is argv rather than a shell command: the syntax check
accepts quoting that the Claude launcher passes through as literal argument
text rather than interpreting.

What that check validates is the command itself and nothing around it. No child
Symphony spawns gets a login shell, so a passing check is a statement about the
configured text rather than about an operator profile the run would then also
execute -- there is none to check, and the same holds for hooks, which run under
`sh -c` and are checked with `sh -n -c`.

## The `agent_authentication` check

Every backend gets an `agent_authentication` check, so `Result.Checks` never
silently omits it depending on which backend is selected. Both forms read a
single value and nothing else — the same "only the boolean is read" rule that
keeps credentials and account details out of
[the structured log](observability.md).

**Under `agent.backend: claude`** it runs the CLI's own read-only `claude auth
status` and reads only the `loggedIn` boolean. That command also reports the
operator's email, organization, and subscription, none of which is read or
logged. `claude.command` is expected to be a bare program name (`claude` by
default); anything else -- a wrapper script, `mise exec -- claude`, a test stub
with extra arguments -- has no reliable way to be asked for status, so the check
does not probe it and does not fail on it. A pass would then be evidence of
nothing.

**Under `agent.backend: codex`** it runs `codex login status` and reads only its
exit code: 0 logged in, 1 not, ignoring the human sentence the CLI prints
alongside naming the auth method. Any exit other than 0 or 1 is a probe failure
rather than an authentication answer, since the CLI did not report a status this
check understands. That subcommand is distinct from `codex app-server`, the
long-lived JSON-RPC service the coordinator actually drives, so asking it
carries none of that service's side effects. `codex.command` defaults to `codex
app-server` -- two words, not a bare program name -- so this form does not
require a single-word command the way Claude's does. It instead checks that the
command's own trailing arguments are exactly `app-server`, the fixed subcommand
`codex.command` always launches with, and if so runs `login status` against the
leading program name. A command shaped any other way -- a wrapper, a container
entrypoint, extra flags after `app-server` -- is treated the same as Claude's
wrapper case: not probed, and not a pass.

The probe is bounded at five seconds. Every other preflight probe is a `sh -n`
syntax check, a `PATH` lookup, or a `stat`, so this is the one call that runs a
foreign program, and a CLI blocked on a keychain prompt or a token refresh must
fail the check rather than leave `--dry-run` waiting.

**Operator prerequisite:** the user the Symphony process runs as must already be
logged in to the CLI the selected backend drives. Symphony passes it no
credential and performs no login; the CLI authenticates through that user's own
stored login under its home directory, which is why the child environment is
inherited apart from the host secrets Symphony strips (see
[architecture](architecture.md)).

This check exists because an unauthenticated agent CLI otherwise surfaces only
at dispatch, where it looks like a finished turn rather than a setup problem:
Claude reports an authentication failure as a result event with `is_error` set,
and an unauthenticated Codex app-server fails at `thread/start` or mid-turn.
Either way a live dispatch has already consumed an attempt and a workspace. One
caveat for Codex: the check reflects a login the CLI itself persisted (`codex
login` or `codex login --with-api-key`), not a bare `OPENAI_API_KEY` export with
no stored login, which `codex login status` reports as logged out even though
the app-server picks the variable up directly at spawn time. See
[the live smoke profile](live-smoke.md) for how to treat that case.
