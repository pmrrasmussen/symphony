# Workspace ownership and recovery

Symphony stores durable workspace ownership below the configured
`workspace.root` in `.symphony-state/`. The state file name is a
SHA-256-derived key; use the warning's marker path or inspect that directory
instead of guessing a name.

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
  "git_common_inode": 12345678
}
```

`issue_id` and `identifier` bind the file to one Linear issue. `base_commit`
keeps terminal cleanup from deleting locally changed work, and from deleting
committed work unless Symphony verifies that exact commit as the merged pull
request head (see [Terminal cleanup safety](#terminal-cleanup-safety)).
`preparation` advances through `creating`, `hook_pending`, and `ready`; an
interrupted pre-ready workspace is discarded and recreated before the
after-create hook is retried. The source-worktree paths and common-directory
filesystem identity reject replaced or unowned repositories.

Older releases may have written `completed_updated_at`. Symphony accepts that
field while reading old state, ignores it for dispatch, and omits it on the
next state write. Completion is authoritative in Linear and the host-side
handoff lifecycle, not in a workspace timestamp.

Schema v1 and state without `schema` remain readable and are rewritten as v2
on the next state write. Other schema values, unknown fields, malformed JSON,
and missing or mismatched ownership fields are invalid and fail closed.

## Dispatch, restart, and recovery

The scheduler claims every active, routable tracker issue. It never suppresses
an issue because a workspace state file contains an unchanged timestamp.
`Prepare` owns the local safety boundary:

| State | Result |
| --- | --- |
| No state and no existing workspace | Create and own a fresh workspace. |
| Valid ownership state | Reuse the existing workspace, including after restart. |
| Legacy `completed_updated_at` state | Reuse the workspace and rewrite state without the legacy field. |
| Missing state beside an existing workspace | Fail closed and require manual recovery. |
| Corrupt, unknown, or ownership-mismatched state | Fail closed and require manual recovery. |

After a restart, Symphony rediscovers active issues from Linear and prepares
their owned workspaces. An active issue that reaches the Codex turn limit is
recorded as an explicit blocked/exhausted run and retried with normal backoff;
it remains dispatchable without a tracker edit. Terminal issues are cleaned up
only when worktree safety checks allow it.

## Terminal cleanup safety

Cleanup of a terminal issue's owned Git worktree is fail-closed. Only the first
two outcomes below remove anything and the third finds the removal already
done; the remaining three preserve the worktree for a human:

| Worktree | Result |
| --- | --- |
| Clean, HEAD still at the recorded `base_commit` | Removed, logged `status: clean`. |
| Clean, HEAD is a local commit that a landing verification confirms is the merged pull request head for this issue | Removed, logged `status: landed`. |
| Already gone, whichever step discovers that | Nothing left to remove, logged `status: clean`. |
| Uncommitted or untracked changes | Preserved, logged `status: dirty`. |
| Clean, HEAD is a local commit that is not verifiably merged | Preserved, logged `status: committed`. |
| Unowned, replaced, or unverifiable source identity | Preserved, logged `status: blocked` (or `status: failed` for a refusal whose reason does not name itself one). |

The landing verification is host-owned, read-only, and reachable from every
cleanup path — end of run, terminal reconciliation, retry, and the startup
sweep after a restart — because it re-reads GitHub instead of relying on
in-process memory of the landing. The first two of those paths can reach the
same finished run, so they serialize on it and the second does nothing once the
first has removed the workspace: one finished issue produces one cleanup
attempt (PMR-160). It asks one question: does the configured
repository have exactly one pull request for this issue's `symphony/<issue>`
branch, is it merged, and is its head commit the commit still checked out in
the worktree? Only an exact match permits removal, so a locally amended or
rebased HEAD, a commit never pushed to the bound branch, an unconfigured GitHub
integration, and any request failure all keep the committed work for manual
review. The configured `github.merge_method` does not affect this: a pull
request's head commit is the source branch tip, which GitHub does not rewrite
when it squashes or rebases onto the base branch, so verified cleanup works
under all three merge methods. The verification never merges,
comments, or transitions anything.

A removal deletes the whole worktree directory, including files Git ignores.
`git status` does not report them, so a `.gitignore`d local file in a
verified-landed worktree does not survive cleanup. Before verified cleanup
existed a worktree holding local commits was always preserved, so in practice
such files survived every completed run.

## Manual recovery

State ownership errors require deliberate operator action:

1. Stop Symphony and keep it stopped throughout recovery.
2. Read the reported marker and inspect the matching workspace. For Git
   workspaces, review `git status --short` and `git log` first.
3. Move the existing workspace and state marker (when present) to an
   operator-owned quarantine directory outside `workspace.root`. Preserve them
   together for review; do not delete either artifact or leave the old
   workspace at its managed path.
4. Restart Symphony. With neither managed workspace nor marker present, an
   active issue receives a fresh workspace. Remove the quarantined copy only
   after reconciling its work manually.

For example, after replacing the paths with exact values from the log and
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
