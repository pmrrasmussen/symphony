# macOS repository services

Symphony's macOS operator model is **one long-running process and one
LaunchAgent per repository**.  The process that runs a repository is not a
machine-wide coordinator: its `WORKFLOW.md`, workspaces, logs, and runtime
status all belong to that repository.

The normal setup is repository-driven.  Install one shared executable once,
then register each repository from its own checkout:

```sh
cd ~/repos/symphony
./scripts/install

cd ~/repos/foo
symphony service install
```

`./scripts/install` is machine bootstrap. It builds Symphony and atomically
installs the shared executable at `~/.local/bin/symphony` (or
`$SYMPHONY_INSTALL_DIR/symphony`). It does not add, change, or restart a
service. `symphony service install` is per-repository registration: it checks
the repository's workflow and credentials, generates and validates a
LaunchAgent, then loads that one service. Run it once from every repository
that should run Symphony. The shared binary is intentionally not copied into
those repositories and does not update itself.

This guide describes the supported operator surface. The raw plist example
near the end is a lower-level troubleshooting/manual option, not the primary
installation procedure.

## Instance convention

For a repository at `<repo>`, a managed installation creates these independent
resources:

| Resource | Convention |
| --- | --- |
| Workflow policy | `<repo>/WORKFLOW.md` |
| LaunchAgent | `~/Library/LaunchAgents/com.pmrrasmussen.symphony.<instance>.plist` |
| Working directory | `<repo>` |
| Agent workspaces | `<repo>/.symphony/workspaces` |
| Structured event history | `<repo>/.symphony/logs/symphony.jsonl` |
| Service stdout/stderr | `<repo>/.symphony/service/{stdout,stderr}.log` |
| Current runtime snapshot | `<repo>/.symphony/service/status.json` |

`<instance>` is a stable, machine-local identity, normally derived from the
GitHub origin as `<owner>-<repo>`. Pass `--name` when that is not sufficiently
unambiguous or when preserving a chosen local name:

```sh
symphony service install --name client-foo
```

The generated label is
`com.pmrrasmussen.symphony.<instance>`. Its matching plist filename carries
the same name. A label is the durable local service identity; a PID is only
runtime metadata and changes on every restart. Discovery also recognizes the
legacy single-instance label and filename,
`com.pmrrasmussen.symphony` / `com.pmrrasmussen.symphony.plist`, so it remains
visible during migration.

Each registration uses its repository as `WorkingDirectory` and passes
absolute `--workflow`, `--logs-root`, and `--status-file` arguments. Do not
point two services at the same workflow, status file, log root, or workspace
source. `service install` detects such existing registrations and refuses the
new one rather than silently accepting a collision.

## Service commands

Run these commands from the repository whose service you intend to manage:

```sh
symphony service install
symphony service status
symphony service restart
symphony service uninstall
```

`install` is idempotent: an identical managed plist is left alone; a changed
managed plist is reloaded only for that repository. It runs the normal
workflow preflight and `plutil` validation before changing launchd. It refuses
to overwrite an unmarked/manual plist. `status` emits a safe JSON description
of the selected managed instance. `restart` uses launchd to restart only that
instance, and `uninstall` unloads and removes only its managed plist. There is
currently no `service list`; use `symphony tui` for the all-instance read-only
view.

All commands accept the same selection/configuration flags when needed:

```text
--workflow PATH
--name NAME
--binary PATH
--linear-api-key-file PATH
--github-token-file PATH
--log-level info|debug
```

Normally none are necessary. They are useful for an explicitly named
instance, a workflow outside the repository root, an alternative shared
binary, or a deliberate credential-file override.

## Credentials

The workflow contains credential *references*, never secret values. Services
write only file paths into their LaunchAgent environment and require those
files to be owner-only. A usual setup is:

```text
~/.config/symphony/linear-api-key
~/.config/symphony/github/<owner>-<repo>.token
```

The Linear key can be shared by repository instances that are authorized to
use the same Linear workspace. For GitHub, prefer one fine-grained,
repository-scoped token file per repository. In `WORKFLOW.md`, use the
corresponding environment references, for example:

```yaml
tracker:
  provider:
    api_key_file: $SYMPHONY_LINEAR_API_KEY_FILE
github:
  token_file: $SYMPHONY_GITHUB_TOKEN_FILE
```

`symphony service install` supplies the conventional Linear path and derives
the GitHub path from the repository origin. An explicit
`--linear-api-key-file` or `--github-token-file` overrides that selection.
The old shared GitHub path `~/.config/symphony/github.token` is not selected
automatically: if it exists, installation asks for an explicit
`--github-token-file` instead. Treat that legacy shared token as compatibility
only, not the least-privilege default.

Never put a secret value in `WORKFLOW.md`, a plist, shell history, or service
logs.

## Status, logs, and the TUI

`status.json` is a small, redacted, atomically replaced snapshot of the
current process observation. It includes such things as the PID, generated
time, effective safe paths, and coordinator activity. It is **not** liveness
authority: a crash can leave a final `"running"` snapshot on disk. Determine
liveness from launchd/process observation, then use snapshot freshness as a
supporting signal.

`symphony.jsonl` is separate redacted event history. It is useful for recent
activity and troubleshooting, but it is neither an authoritative current-state
model nor a replacement for Linear/workspace state. The TUI combines those
local observations without changing them:

```sh
symphony tui
```

The TUI is an ephemeral, read-only process. It scans convention-matching
LaunchAgents in `~/Library/LaunchAgents`, reads launchd state, each configured
status snapshot, effective configuration, validation findings, and a bounded
redacted log tail. Close it whenever you like: it does not start, stop,
restart, pause, or otherwise affect repository services, and it has no
connection to a central Symphony daemon.

## Two repositories on one machine

One shared binary can run any number of independent repository services. For
example, these two installations use the same executable but no runtime paths
in common:

| | `~/repos/acme/api` | `~/repos/acme/web` |
| --- | --- | --- |
| Command | `symphony service install --name acme-api` | `symphony service install --name acme-web` |
| Label | `com.pmrrasmussen.symphony.acme-api` | `com.pmrrasmussen.symphony.acme-web` |
| Binary | `~/.local/bin/symphony` | `~/.local/bin/symphony` |
| `WorkingDirectory` / workflow | `~/repos/acme/api` / `~/repos/acme/api/WORKFLOW.md` | `~/repos/acme/web` / `~/repos/acme/web/WORKFLOW.md` |
| Workspaces | `~/repos/acme/api/.symphony/workspaces` | `~/repos/acme/web/.symphony/workspaces` |
| Event log | `~/repos/acme/api/.symphony/logs/symphony.jsonl` | `~/repos/acme/web/.symphony/logs/symphony.jsonl` |
| Service logs | `~/repos/acme/api/.symphony/service/{stdout,stderr}.log` | `~/repos/acme/web/.symphony/service/{stdout,stderr}.log` |
| Status | `~/repos/acme/api/.symphony/service/status.json` | `~/repos/acme/web/.symphony/service/status.json` |

After either installation or update, run `symphony tui` from any directory.
It will list these labels separately. If an attempted registration reuses the
other instance's workflow or status path, `service install` reports the shared
path as a service conflict and leaves the candidate registration untouched.

## Migrating a hand-authored service

Older setups commonly have a repo-local executable and a hand-authored
`~/Library/LaunchAgents/com.pmrrasmussen.symphony.plist`. Migrate one
repository at a time so its existing workflow, credentials, logs, and
workspaces remain intact.

1. From the Symphony repository, run `./scripts/install` to create the shared
   stable binary. Do not remove the existing binary yet; the old agent may
   still be using it.
2. In the repository being migrated, keep the current `WORKFLOW.md`,
   `.symphony/logs`, and `.symphony/workspaces`. Ensure workflow credentials
   are file references, then place or retain the credential files at the
   conventional paths above (or plan explicit override flags).
3. Stop and remove the old registration before installing the managed one.
   For the legacy label, this is typically:

   ```sh
   launchctl bootout "gui/$(id -u)/com.pmrrasmussen.symphony" || true
   rm ~/Library/LaunchAgents/com.pmrrasmussen.symphony.plist
   ```

   Inspect the label and plist path first; do not remove an agent belonging to
   another repository. This step matters because the installer intentionally
   refuses duplicate workflow/status registrations and refuses to overwrite
   unmarked plists.
4. Register the repository, choosing a stable name when necessary:

   ```sh
   cd ~/repos/foo
   symphony service install --name foo
   symphony service status
   ```

   The resulting agent uses the canonical
   `.symphony/service/status.json` argument. Existing structured logs and
   workspaces stay where they were; the service adds its separate stdout and
   stderr files under `.symphony/service`.
5. Run `symphony tui` and verify that the new generated label appears with the
   expected workflow, paths, and launchd state. The legacy label remains
   discoverable by the TUI only until its plist is removed.

If a transition must be rolled back, first uninstall only the managed service
from that repository, then restore the prior plist from a known-good backup.

## Manual LaunchAgent reference

Use this only to troubleshoot launchd or to operate a deliberately manual
service. The normal `symphony service install` path is safer: it preflights,
validates, detects collisions, and manages updates. Replace every example
path, label, and credential reference below; do not paste secret values.

Save as `~/Library/LaunchAgents/com.pmrrasmussen.symphony.acme-api.plist`:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
  <dict>
    <key>Label</key>
    <string>com.pmrrasmussen.symphony.acme-api</string>
    <key>ProgramArguments</key>
    <array>
      <string>/Users/you/.local/bin/symphony</string>
      <string>--workflow</string>
      <string>/Users/you/repos/acme/api/WORKFLOW.md</string>
      <string>--logs-root</string>
      <string>/Users/you/repos/acme/api/.symphony/logs</string>
      <string>--status-file</string>
      <string>/Users/you/repos/acme/api/.symphony/service/status.json</string>
      <string>--log-level</string>
      <string>info</string>
    </array>
    <key>WorkingDirectory</key>
    <string>/Users/you/repos/acme/api</string>
    <key>EnvironmentVariables</key>
    <dict>
      <key>PATH</key>
      <string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin:/Users/you/.local/bin</string>
      <key>SYMPHONY_LINEAR_API_KEY_FILE</key>
      <string>/Users/you/.config/symphony/linear-api-key</string>
      <key>SYMPHONY_GITHUB_TOKEN_FILE</key>
      <string>/Users/you/.config/symphony/github/acme-api.token</string>
    </dict>
    <key>StandardOutPath</key>
    <string>/Users/you/repos/acme/api/.symphony/service/stdout.log</string>
    <key>StandardErrorPath</key>
    <string>/Users/you/repos/acme/api/.symphony/service/stderr.log</string>
    <key>RunAtLoad</key><true/>
    <key>KeepAlive</key><true/>
    <key>ThrottleInterval</key><integer>10</integer>
  </dict>
</plist>
```

Validate and load it with your own label/path:

```sh
plutil -lint ~/Library/LaunchAgents/com.pmrrasmussen.symphony.acme-api.plist
launchctl bootstrap "gui/$(id -u)" \
  ~/Library/LaunchAgents/com.pmrrasmussen.symphony.acme-api.plist
launchctl kickstart -k "gui/$(id -u)/com.pmrrasmussen.symphony.acme-api"
```

This manual plist intentionally has no `SymphonyManaged` marker. Discovery and
the TUI can inspect it, but `service install`, `service restart`, and `service
uninstall` will not take it over. Remove it safely and use the managed setup
when ready to migrate.

## Source-of-truth boundaries

| Surface | Responsibility |
| --- | --- |
| `WORKFLOW.md` | Repository-owned policy: tracker, workspace, agent, and integration configuration. |
| LaunchAgent | Machine-local configured instance: executable, working directory, safe path/credential references, and launchd registration. |
| `status.json` | Redacted current runtime observation; potentially stale after a crash, never liveness authority. |
| `symphony.jsonl` | Redacted event history for diagnostics, not authoritative current state. |
| Service CLI | Explicit machine-level install, update, inspection, restart, and removal for one repository service. |
| TUI | Ephemeral read-only presentation of local discovery; it cannot mutate configuration or service state. |

For the snapshot schema and freshness details, see
[runtime status](runtime-status.md). For log-level and redaction guarantees,
see [observability](observability.md).
