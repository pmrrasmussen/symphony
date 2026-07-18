# Completion markers and recovery

Symphony stores durable workspace ownership and completion state below the
configured `workspace.root` in `.symphony-state/`. The state file name is a
SHA-256-derived key; use the warning's marker path or inspect the files in that
directory instead of guessing a name.

## Schema and ownership

New writes use the `symphony.workspace-state/v2` schema:

```json
{
  "schema": "symphony.workspace-state/v2",
  "issue_id": "the Linear issue UUID",
  "identifier": "PMR-15",
  "base_commit": "the detached worktree's initial Git commit",
  "preparation": "ready",
  "source_root": "/canonical/source/repository",
  "git_common_dir": "/canonical/source/repository/.git",
  "git_worktree_dir": "/canonical/source/repository/.git/worktrees/PMR-15",
  "git_common_device": 16777230,
  "git_common_inode": 12345678,
  "completed_updated_at": "2026-07-18T19:00:00Z"
}
```

`issue_id` and `identifier` bind the file to one Linear issue. `base_commit` is
present for Git worktrees and makes terminal cleanup refuse locally changed or
committed work. `preparation` advances through `creating`, `hook_pending`, and
`ready`; an interrupted pre-ready workspace is discarded and recreated before
the after-create hook is retried. The three source-worktree paths and the
common-directory filesystem identity reject replaced/unowned repositories and
support safe Git registration reconciliation.
`completed_updated_at` is present only after a successful full lifecycle.

Schema v1 and state without `schema`, written by older Symphony builds, remain
readable and are rewritten as v2 on the next state write. Other schema values,
unknown fields, malformed JSON, and missing or mismatched ownership fields are
invalid and fail closed. A legacy Git marker without source-worktree identity
cannot use source-loss recovery and requires the manual procedure below.

Symphony owns these files. Do not edit them while Symphony is running. Writes
are atomic replacements with owner-only file permissions.

## Suppression and redispatch

The scheduler applies this policy before claiming an issue:

| State | Result |
| --- | --- |
| No state and no existing workspace | Dispatch; this is a new lifecycle. |
| Valid state without `completed_updated_at` | Dispatch; the workspace belongs to an incomplete lifecycle. |
| Valid completion matching Linear `updatedAt` | Suppress, including after restart. |
| Valid completion older than Linear `updatedAt` | Dispatch the updated issue as a new bounded lifecycle. |
| Missing state beside an existing workspace | Suppress and require manual recovery. |
| Corrupt, unknown, or ownership-mismatched state | Suppress and require manual recovery. |
| Completion newer than Linear `updatedAt`, or Linear omits `updatedAt` | Suppress and require manual recovery. |

An issue becomes eligible again only when Linear reports an `updatedAt` later
than the completed marker, for example after a description or state edit that
advances that field. Make an intentional edit only when it is safe for the
issue to run again, and verify that `updatedAt` advanced. Symphony reuses the
existing issue workspace for that new lifecycle.

The coordinator writes `completed_updated_at` only after all configured
bounded continuation turns complete normally and a final Linear refresh still
shows the same active issue version. Handoff, cancellation, failure, terminal
state, or an intervening Linear edit does not write a completion marker.

## Manual recovery

An invalid marker never becomes an automatic rerun. The scheduler logs the
validation error and leaves the issue unclaimed. To recover deliberately:

1. Stop Symphony and keep it stopped throughout recovery.
2. Read the reported marker and inspect the matching issue workspace. For
   Git workspaces, review `git status --short` and `git log` before deciding
   whether the work is already complete or needs another run.
3. If the marker is valid and the completed work is safe to redispatch, make a
   description or state edit and verify that Linear advanced `updatedAt`. Do
   not remove the marker; the later timestamp makes the new version eligible
   while retaining ownership and cleanup data.
4. If state is corrupt, unknown, or missing, move the existing workspace and
   marker (when present) to an operator-owned quarantine directory outside
   `workspace.root`. Preserve them together for review. Do not delete either
   artifact and do not leave the old workspace at its managed path.
5. Restart Symphony. With neither a managed workspace nor marker present, the
   issue is treated as a deliberate clean lifecycle and receives a fresh
   workspace. Remove the quarantined copy only after its work has been
   reconciled manually.

For example, after replacing the paths with the exact values from the log and
configuration:

```sh
mkdir -p /safe/operator-quarantine/PMR-15
mv /configured/workspace/root/PMR-15 /safe/operator-quarantine/PMR-15/workspace
mv /configured/workspace/root/.symphony-state/<reported-file>.json \
  /safe/operator-quarantine/PMR-15/state.json
```

If the state file is missing, move only the workspace. If the workspace is
missing, preserve only the reported state file. Never use a broad recursive
delete or wildcard for recovery.
