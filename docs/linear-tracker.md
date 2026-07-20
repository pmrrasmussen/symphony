# Linear tracker profile

`tracker.kind` is `linear`. Its adapter-owned configuration lives under
`tracker.provider`:

| Key | Required | Meaning |
| --- | --- | --- |
| `project_slug_id` | yes | Linear project slug ID. Every read is restricted to this project. |
| `project_slug` | deprecated | Legacy alias for `project_slug_id`. It emits a value-free migration warning; setting both names is rejected. |
| `api_key` | alternative | Linear personal API key. Exact `$VARNAME` expansion is supported by `WORKFLOW.md`; surrounding whitespace is trimmed and the value is never logged. |
| `api_key_file` | recommended | Trusted local file containing the API key. Prefer `$SYMPHONY_LINEAR_API_KEY_FILE`, whose value is the absolute file path; file contents and path dependencies are tracked by configuration loading. When both credential fields exist, this file takes precedence. |
| `endpoint` | optional | Absolute HTTPS GraphQL endpoint; defaults to `https://api.linear.app/graphql`. HTTP is accepted only for `localhost`, `127.0.0.1`, or `::1` test hosts. |
| `assignee` | optional | Unset permits all assignees. A non-empty ID permits only that assignee. `me` resolves the current Linear viewer ID for each read. |
| `handoff_state` | optional | The single human-controlled review state `github_publish_pr` hands a bound issue off to, host-side. The name must be a non-active workflow state in the active issue's Linear team. It binds a Linear session and enables the scoped GitHub handoff tools, but is not itself a model-invokable tool. |
| `handoff_comment_template` | optional | A repository-owned Go template for the comment Symphony posts when it performs the host-side review handoff. It requires `handoff_state` and receives only `issue`. |
| `transitions` | optional | The single host-owned tracker transition policy. A structured object with two independent edge sets, both applied host-side with the host credential and never exposed to a Codex session: `start` (dispatch-time edges keyed by the issue's current state — the canonical `Todo -> In Progress`; both endpoints must be active, non-terminal states) and `refuse_landing` (the `Merging -> In Review` fallback `github_land_pr` applies on a hard gate, keyed by `github.merge_state`). The two sets are kept structurally distinct — never flattened into one map — because `Merging` is both a dispatchable state and the land-fallback source. Terminal and same-state edges are rejected; `start` moves are idempotent and fail-safe. |
| `child_issue_creation` | optional | Boolean. Enables the session-bound Codex `create_child_issue` tool, described below. Disabled by default. |

Invalid provider values produce `invalid_tracker_config`; a missing or empty key
produces `missing_tracker_secret`. `api_key_file` loading errors are reported by
configuration before the service starts without including the configured path.
Changes to a referenced environment value or secret file participate in reload
detection even when `WORKFLOW.md` itself is unchanged. A rejected ambiguous
project-key migration retains the last valid configuration.
The legacy alias will be removed only in a future breaking configuration
release; the warning is the migration notice for existing workflows.

Candidate and terminal reads use a project-and-state filter. Configured state
names keep their original spelling for Linear's case-sensitive filter; internal
state comparisons remain case-insensitive. Each read freezes its project,
states, assignee policy, terminal states, endpoint, and credential before its
first request, so a workflow reload takes effect only on the next read.

Reads normally fetch 50 issues per page and require a fresh non-repeating cursor
while `hasNextPage` is true. If a page exceeds the bounded response size, only
that cursor is retried with progressively smaller pages. A malformed page or a
page that is still oversized at one issue fails that poll without returning a
partial list; the next poll starts cleanly at the normal page size.
Refresh-by-ID uses project-and-ID filtering, batches requested IDs in groups of
50, rejects malformed returned records, and preserves the requested ID order for
records still in scope.

The stable dispatch ID is Linear's issue ID; `native_ref` is omitted because no
additional provider ID is needed. Required `id`, `identifier`, `title`, and
`state` values must be non-empty after trimming. Candidate reads log and omit
malformed records, while refresh reads fail. Labels are lowercase, blank labels
are dropped, and duplicate labels are removed. Invalid optional timestamps and
priority values normalize to null. `inverseRelations` with type `blocks` become
best-effort `blocked_by` records. The bounded relation query includes page info;
a `Todo` issue is conservatively non-dispatchable if all blocker relations were
not returned.

An issue is dispatchable only when it matches the optional assignee policy and,
while in `Todo`, has no blocker outside the workflow's terminal states. A
non-terminal blocker for an in-progress issue does not make it disappear from
reconciliation.

Errors are redacted `linear.Error` values. Categories are
`invalid_tracker_config`, `missing_tracker_secret`, `tracker_request`,
`tracker_status`, `tracker_response`, `tracker_pagination`, and
`tracker_rate_limited`. HTTP 429 is retryable and honors numeric or HTTP-date
`Retry-After` plus Linear's request-reset header. The coordinator schedules the
later of that delay and the normal polling interval through its timer; it never
sleeps or immediately hot-loops. 5xx status errors are retryable. GraphQL
response bodies and transport details are intentionally not included in public
error text or logs.

## Host-owned review handoff and transitions

The agent has no tool that writes the active issue's tracker state. Every state
change except the human review gates is performed host-side, with the host
Linear credential, so no model-invokable path can transition the board:

- `transitions.start` is applied by the coordinator at dispatch (`Todo -> In
  Progress`), before the Codex session starts.
- `github_publish_pr` performs the review handoff (`In Progress`/`Rework ->
  handoff_state`) host-side after it publishes the pull request.
- `github_land_pr` completes landing (`Merging -> Done`) or, on a hard gate,
  applies the `transitions.refuse_landing` fallback (`Merging -> In Review`).
- The poll loop reconciles an externally merged pull request to `Done`.

### Disable the tracker's native PR-to-status automation for managed issues

Symphony owns every managed issue's state transitions. Linear's own **native
GitHub PR automation** (for example "move the issue to In Progress when its
linked pull request opens") is an *external* writer that races Symphony's host
review handoff: within roughly a second of `github_publish_pr` moving an issue
to `handoff_state` and linking its pull request, the automation can move it
back to an active state (an `In Review -> In Progress` flap; PMR-63). Symphony
has no code path that moves an issue out of `handoff_state` into an active
state, so any such backward edge is external.

Disable Linear's PR-linked status automations for the team and project that
back `tracker.provider.project_slug_id` (Linear → Settings → the team → GitHub
integration / workflow automations). This is a live-run operator prerequisite,
not something `--dry-run` or the MCP integration can verify — check it in the
Linear UI.

If the automation is left enabled, Symphony no longer silently re-dispatches
the reverted issue: the poll loop remembers each issue it drove into
`handoff_state` and logs an `external tracker state change observed`
(`operation: external_reversion`) record — with the from/to state names and the
elapsed time since the handoff — the first time that issue reappears as an
active candidate. Symphony does not automatically re-assert the handoff; see
[docs/observability.md](observability.md) for the log record and the deferred
auto-reconcile follow-up.

When neither `handoff_state` nor `child_issue_creation` is configured, Symphony
binds no Linear session for the Codex child and advertises no session-bound
Linear tool. When either is configured, the service validates the active issue's
project and team and freezes the policy for the session before the child process
starts. A workflow reload affects later sessions only; an invalid reload retains
the last valid policy.

Before every host handoff or transition mutation, Symphony re-reads the bound
issue and rejects the action if its project, team, or state changed, or if it is
no longer in an active workflow state, so a human terminal transition wins
without any agent involvement.

The host review handoff trims and validates the repository-rendered comment,
checks the bound issue's existing comments, and applies the comment before the
configured state transition. The exact configured comment is the reconciliation
record: if either mutation returns an ambiguous or retryable failure, a retry
observes the comment and current state before doing more work. A completed
transition is accepted only when that comment already exists (or no comment is
configured), and a target-state issue missing its comment is repaired without
another transition. This makes repeated delivery idempotent without storing a
key or exposing a broader Linear write API. Handoff and transition logs contain
only the operation, the from/to state names, and the bound issue ID/identifier;
comment bodies, credentials, and agent arguments are never logged.

The `refuse_landing` fallback accepts no agent input. On a refused landing
Symphony refreshes the bound issue, permits the mutation only when the refreshed
source matches the configured `github.merge_state`, and moves it to the mapped
state. A call already off the merge state is an idempotent no-op; reversed,
terminal, stale, cross-project, and cross-team states are rejected. Host
transitions are serialized per session, so a race or ambiguous provider result
is reconciled by the next scoped call.

## Optional child issue creation capability

`tracker.provider.child_issue_creation: true` enables the only session-bound
Codex Linear tool, `create_child_issue`. Either `handoff_state` or
`child_issue_creation` is enough to bind a Linear session for the Codex child
process, but the tool is advertised only when its own setting is configured.
Unlike the removed `linear_graphql` tool, it never transitions the active
issue; it only creates a scoped sub-issue. `create_child_issue` requires
no separate project or team configuration: it always creates the new issue in
the active issue's already-configured Linear project and team, and always
records the active issue as the new issue's Linear parent (a native Linear
sub-issue relationship, visible in the Linear UI).

The tool's only accepted fields are `title` (required), `description`,
`priority` (0-4), `labels` (resolved only against label names that already
exist on the active issue's team; an unrecognized name fails the whole call
rather than creating a new label or silently dropping it), and `depends_on`.
There is no field for an issue ID, project, team, endpoint, or credential.

`depends_on` is deliberately bounded to this session's own lineage: each entry
must be the `id` or `identifier` returned by an earlier `create_child_issue`
call in the same session, and is applied as a Linear `blocks` relation (the
referenced child blocks the new one). A reference to any other issue,
including the active issue itself or a child issue created by a different
session, is rejected before any mutation is attempted; the tool cannot create
a relation against an issue it did not itself create.

Before creating an issue, Symphony re-reads the active issue and rejects the
call if its project, team, or state changed since session setup, using the
same scope check as the `comment` operation above. A completed child issue
creation is logged with only the parent and child issue IDs/identifiers
(never title, description, or label content) as the audit record.

The intended pattern is to decompose one task into several independently
reviewable pull requests: normally create one `create_child_issue` call per
unit of work, and each resulting issue produces its own isolated Symphony
worktree and its own pull request once it reaches an eligible state.
