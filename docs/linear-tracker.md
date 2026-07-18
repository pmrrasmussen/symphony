# Linear tracker profile

`tracker.kind` is `linear`. Its adapter-owned configuration lives under
`tracker.provider`:

| Key | Required | Meaning |
| --- | --- | --- |
| `project_slug` | yes | Linear project slug ID. Every read is restricted to this project. |
| `api_key` | yes | Linear personal API key. `$VARNAME` expansion is supported by `WORKFLOW.md`; it is never logged. |
| `api_key_file` | optional | Trusted local file containing the API key; it is resolved by configuration loading. |
| `endpoint` | optional | Absolute HTTPS GraphQL endpoint; defaults to `https://api.linear.app/graphql`. HTTP is accepted only for `localhost`, `127.0.0.1`, or `::1` test hosts. |
| `assignee` | optional | Unset permits all assignees. A non-empty ID permits only that assignee. `me` resolves the current Linear viewer ID for each read. |
| `handoff_state` | optional | Enables the tightly scoped Codex `linear_graphql` compatibility tool. The name must be a non-active workflow state in the active issue's Linear team. |
| `handoff_comment_template` | optional | A repository-owned Go template for the comment made by the tool's `handoff` operation. It requires `handoff_state` and receives only `issue`. |

Invalid provider values produce `invalid_tracker_config`; a missing or empty key
produces `missing_tracker_secret`. `api_key_file` loading errors are reported by
configuration before the service starts.

Candidate and terminal reads use a project-and-state filter, fetch 50 issues per
page, and require a fresh non-repeating cursor while `hasNextPage` is true. A
failed page returns no partial list. Refresh-by-ID uses project-and-ID filtering,
batches requested IDs in groups of 50, rejects malformed returned records, and
preserves the requested ID order for records still in scope.

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
`tracker_rate_limited`. HTTP 429 is retryable and honors numeric `Retry-After`;
5xx status errors are retryable. GraphQL response bodies and transport details
are intentionally not included in public error text or logs.

## Optional Codex handoff capability

When `handoff_state` is absent, Symphony does not advertise a client-side
Linear tool and all such requests remain unsupported. When it is configured,
the service validates the active issue's project and team, resolves the named
state in that team, and freezes those values for the Codex session before the
child process starts. A workflow reload affects later sessions only; an invalid
reload retains the last valid policy.

Before every handoff transition or comment mutation, Symphony re-reads the
bound issue and rejects the action if its project, team, or state changed, or
if it is no longer in an active workflow state. A human terminal transition
therefore wins without any agent mutation.

The compatibility name is `linear_graphql`, but it is not a GraphQL proxy. Its
only typed operations are `read`, `handoff`, and `comment`. They are bound to
the active issue and configured project; callers cannot supply a query, issue
ID, project, endpoint, credential, or state. `handoff` may only use the
configured state and the optional fixed comment template. `comment` can only
write a bounded comment to the active issue. Tool failures return generic
responses and never reveal the Linear credential or provider payload.
