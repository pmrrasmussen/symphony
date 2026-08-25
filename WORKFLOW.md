---
tracker:
  kind: linear
  provider:
    # Linear project slug ID for the pmrrasmussen/Symphony project.
    project_slug_id: 6e13e4a9f215
    # Set this to an absolute path for a mode-600 file outside the repository.
    api_key_file: $SYMPHONY_LINEAR_API_KEY_FILE
    # Host-owned tracker transition policy. Symphony applies every edge here
    # itself, with the host Linear credential; none is ever exposed to a Codex
    # session, so the agent has no issue-state transition capability. The two edge
    # sets are kept structurally distinct on purpose and must not be flattened
    # into one map: Merging is both a dispatchable/active state and the
    # land-fallback source, so a flat source->target map consumed at dispatch
    # would wrongly move a freshly dispatched Merging landing agent's issue to
    # In Review.
    transitions:
      # Applied at dispatch, keyed by the issue's current state: a dispatched
      # Todo issue is deterministically moved to In Progress before the session
      # starts, so board-level observability never depends on the agent. Both
      # endpoints must be active, non-terminal states. Idempotent and fail-safe:
      # an already-started issue is untouched and a failed move never blocks the
      # run.
      start:
        Todo: In Progress
      # Applied only when a github_land_pr attempt hits a hard gate: the
      # Merging -> In Review fallback, keyed by github.merge_state below.
      refuse_landing:
        Merging: In Review
    # Optional, opt-in Codex client tool for capturing meaningful out-of-scope
    # work. It creates a parentless issue only in this project/team, always in
    # Backlog, with worker-supplied title, description, and acceptance criteria.
    # The worker may only relate it to the active issue or mark it blocked by
    # the active issue; it cannot select arbitrary scope or an initial state.
    # Backlog must remain excluded from active_states below so a human promotion
    # to Todo is required before Symphony can dispatch the follow-up.
    # followup_issue_creation: true
    # Enables the scoped github_publish_pr/github_pr_context handoff tools for
    # the bound issue and is where github_publish_pr hands the issue off for
    # review, host-side. In Review is the single, fixed human-controlled review
    # state: it is deliberately excluded from active_states below, so it is
    # never dispatched. No Codex tool can move an issue into it (or any state);
    # the host performs the handoff. Operator prerequisite: disable Linear's
    # native GitHub PR-to-status automation for this team/project — it is an
    # external writer that races this handoff and can flap the issue back to an
    # active state (In Review -> In Progress; PMR-63). Symphony logs any such
    # external revert (operation: external_reversion) but does not re-assert the
    # handoff itself; see README.md and docs/linear-tracker.md.
    handoff_state: In Review
  # The canonical lifecycle (PMR-38): Todo -> In Progress -> In Review <->
  # Rework -> Merging -> Done. Rework and Merging are active/dispatchable so a
  # human can resume implementation after requesting changes, or dispatch a
  # landing agent, from the same worktree and branch. In Review stays
  # human-controlled and non-dispatchable. See WORKFLOW.md's prompt body below
  # for the full per-state playbook; it is this repository's single
  # executable source of delivery policy.
  active_states: [Todo, In Progress, Rework, Merging]
  terminal_states: [Done, Canceled]
polling:
  interval_ms: 30000
workspace:
  root: .symphony/workspaces
  # Each issue receives a detached Git worktree of this repository.
  source_root: .
hooks:
  timeout_ms: 60000
agent:
  # Four-agent operation: implementation/rework work can scale across the
  # available global capacity while max_concurrent_agents_by_state keeps
  # landing serialized at exactly one Merging agent.
  max_concurrent_agents: 4
  max_concurrent_agents_by_state:
    Merging: 1
  max_turns: 20
codex:
  command: codex app-server
  approval_policy: never
  thread_sandbox: workspace-write
  turn_timeout_ms: 3600000
  # read_timeout_ms bounds every steady-state JSON-RPC round trip; keep it small
  # so a hung session is detected mid-turn.
  read_timeout_ms: 5000
  # start_timeout_ms applies only to the cold-start handshake and thread/start,
  # which include app-server spawn and the first model load. It is deliberately
  # generous so a cold start does not spuriously time out, without loosening
  # read_timeout_ms.
  start_timeout_ms: 120000
  stall_timeout_ms: 300000
# Host-side GitHub PR handoff and landing, fixed to this repository only
# (PMR-36, PMR-37). The token is host-side and repository-scoped: it is read
# once from a mode-600 file outside the repository via
# $SYMPHONY_GITHUB_TOKEN_FILE, is never committed, and Symphony strips it (and
# any inherited environment value containing it) from the Codex child
# process. See README.md for the fine-grained token's required scopes and
# permissions.
github:
  owner: pmrrasmussen
  repository: symphony
  base_branch: main
  token_file: $SYMPHONY_GITHUB_TOKEN_FILE
  poll_interval_ms: 30000
  # Landing capability (PMR-37, activated for real dispatch by PMR-38). A
  # session bound to an issue currently in Merging receives the zero-argument
  # github_land_pr tool; moving the issue to Merging is itself the human
  # approval to land. required_checks names the exact GitHub check names this
  # repository's CI reports (the job names in .github/workflows/ci.yml).
  merge_state: Merging
  merge_method: merge
  required_checks:
    - scripts/check format
    - scripts/check test
    - scripts/check vet
    - scripts/check race
---
Work on {{.issue.identifier}}: {{.issue.title}}

{{.issue.description}}

Current Linear state: {{.issue.state}}

Follow the repository instructions in AGENTS.md, README.md, and WORKFLOW.md.
WORKFLOW.md is the single executable source of delivery policy: the
state-specific guidance below is generated from it for this issue's current
state.
{{if or (eq .issue.state "Todo") (eq .issue.state "In Progress") (eq .issue.state "Rework")}}
## Scope management
- When implementation reveals meaningful work outside this issue, use
  `create_followup_issue` when available to capture it with a clear title,
  description, and acceptance criteria. Use `related` for contextual work or
  `blocked_by_current` only when the follow-up depends on this issue.
- Continue the current issue after filing the follow-up; do not expand its
  scope, wait for the follow-up, or treat it as child-agent orchestration. The
  new issue remains parentless and non-dispatchable in Backlog until a human
  promotes it to an eligible state such as Todo.
{{end}}
{{if or (eq .issue.state "Todo") (eq .issue.state "In Progress")}}
## Implementation and validation
- Implement a focused, validated change for this issue. Run the narrowest
  relevant check first (for example, one package's tests), then
  `scripts/check all` once shared behavior changes.
- Create a clean local commit once validation passes; do not leave
  uncommitted or untracked changes in the worktree.

## Review handoff
- Follow the delivery-mode instructions Symphony supplies below (host-side
  publish via github_publish_pr, or the manual fallback) once the change is
  committed and validated. Publishing hands the issue to human review in In
  Review; never attempt to merge or move the issue past review yourself.
{{end}}
{{if eq .issue.state "Rework"}}
## Rework
- A human moved this issue back to Rework with feedback on its pull request.
  Resume in this same worktree, branch, and prior commit history; do not
  start over, discard history, or create a new branch.
- Read the requested changes (for example via github_pr_context, which
  reports bounded check status, review state, and unresolved feedback), then
  address them and validate again the same way as above: the narrowest
  relevant check first, then `scripts/check all` once shared behavior
  changes.
- Call github_publish_pr again once the worktree is clean and committed. It
  reuses the existing deterministic pull request and hands the issue back to
  In Review, unless the pull request was merged (irrecoverable) or closed and
  could not be reopened; report either outcome as a blocker instead of
  creating a new pull request yourself.
{{end}}
{{if eq .issue.state "Merging"}}
## Landing
- A human moved this issue to Merging: that move is itself the approval to
  land, so no separate approving review is required.
- Call github_land_pr with no arguments. It re-verifies the worktree,
  branch, pull request, required checks, effective review state, unresolved
  review threads, mergeability, and the base branch immediately before the
  irreversible merge call.

## Hard landing blockers
- A pending-checks result is not an error: take no further action. A later
  Merging dispatch retries automatically once checks settle.
- Any other refusal (a failing check, an effective changes-requested review,
  an unresolved review thread, a stale base, a merge conflict, or a closed or
  mismatched pull request) has already returned this issue to In Review for a
  human; do not retry the merge yourself or transition the issue directly.
  Report the refusal reason as a blocker.

## Completion
- A successful merge, or a pull request GitHub already reports merged,
  transitions this issue to Done automatically. Take no further Linear action
  yourself once github_land_pr reports a merged result.
{{end}}
