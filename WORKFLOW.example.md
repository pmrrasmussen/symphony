---
tracker:
  kind: linear
  provider:
    project_slug_id: example-project
    # Recommended: point this variable at a mode-600 file outside the repo.
    api_key_file: $SYMPHONY_LINEAR_API_KEY_FILE
    # Alternatively, inject the credential value directly from the environment:
    # api_key: $LINEAR_API_KEY
    # Optional: only dispatch issues assigned to this Linear user ID, or `me`.
    # assignee: me
    # The canonical lifecycle's exact agent-requested state edges. Todo -> In
    # Progress starts implementation; Merging -> In Review is the fallback a
    # refused github_land_pr call uses (see github.merge_state below). These
    # are not a general destination allowlist: only these exact edges are ever
    # honored, regardless of what a Codex session requests.
    agent_transitions:
      Todo: In Progress
      Merging: In Review
    # Enables the scoped github_publish_pr/github_pr_context Codex tools
    # below. Configure a non-active, non-terminal state from this project's
    # Linear team; it is the single human-controlled review state and the
    # only state a Codex session can move an issue into.
    handoff_state: In Review
    # Optional, opt-in Codex client tool. It creates a new issue only in this
    # project/team and records the active issue as its parent; it cannot
    # select an arbitrary project, team, or issue. Intended for decomposing
    # one task into several independently reviewable pull requests: normally
    # one child issue per isolated worktree and PR.
    # child_issue_creation: true
  # The canonical lifecycle: Todo -> In Progress -> In Review <-> Rework ->
  # Merging -> Done. Rework and Merging are active/dispatchable so a human can
  # resume implementation, or dispatch a landing agent, from the same
  # worktree and branch; In Review is deliberately excluded so it stays
  # human-controlled and is never dispatched. Configure Rework and Merging as
  # Started states in this project's Linear team before enabling them here;
  # until then, keep only [Todo, In Progress] active.
  active_states: [Todo, In Progress, Rework, Merging]
  terminal_states: [Done, Canceled]
polling:
  interval_ms: 30000
workspace:
  root: .symphony/workspaces
  # Optional: populate each issue workspace as a detached Git worktree.
  source_root: .
hooks:
  timeout_ms: 60000
agent:
  # Two-agent operation: one implementation/rework agent may run concurrently
  # with one landing agent. max_concurrent_agents_by_state additionally caps
  # Merging at exactly one landing agent even though more agents are allowed
  # overall.
  max_concurrent_agents: 2
  max_concurrent_agents_by_state:
    Merging: 1
  max_turns: 20
codex:
  command: codex app-server
  approval_policy: never
  thread_sandbox: workspace-write
  turn_timeout_ms: 3600000
  read_timeout_ms: 5000
  stall_timeout_ms: 300000
# Optional. Requires tracker.provider.handoff_state and a fine-grained token
# restricted to exactly this repository.
# github:
#   owner: your-github-owner
#   repository: your-repository
#   base_branch: main
#   token_file: $SYMPHONY_GITHUB_TOKEN_FILE
#   # Alternatively: token: $SYMPHONY_GITHUB_TOKEN
#   poll_interval_ms: 30000
#   # Optional landing capability. A session bound to an issue currently in
#   # this exact Linear state receives a zero-argument github_land_pr tool;
#   # moving the issue to this state (Merging in the canonical lifecycle
#   # above) is itself the human approval to land, so no separate approving
#   # review is required. It must be one of active_states and differ from
#   # handoff_state and every terminal state, since a session only receives the
#   # tool once actually dispatched for an issue in that state. required_checks
#   # is then mandatory: every named check must be present and successful (or
#   # neutral) immediately before the merge call, or the tool waits. Unlike
#   # the rest of this github: block, an invalid merge_state, merge_method, or
#   # required_checks value fails workflow validation instead of silently
#   # disabling the integration.
#   merge_state: Merging
#   # Bounded enum: merge, squash, or rebase. Defaults to merge.
#   merge_method: merge
#   # Use the exact GitHub check names your CI reports (often the job names
#   # from your CI workflow file).
#   required_checks: [ci/build, ci/test]
#   # Opt in to GitHub's deterministic update-branch operation when the base
#   # moves during landing but all other landing gates pass. It creates only a
#   # merge-from-base commit, then github_land_pr waits for checks on that new
#   # head. Disabled by default; a later base move still refuses landing.
#   update_stale_branch: true
#   # Opt in to bounded fix attempts within the Merging turn (PMR-46). When
#   # enabled, a retryable hard gate (failing required checks or unresolved
#   # review threads -- plus merge conflicts only when allow_conflict_resolution
#   # is set) returns a non-terminal failure naming the gate so the same Codex
#   # turn can fix, push, and call github_land_pr again, instead of immediately
#   # refusing. The Merging -> In Review transition is deferred until the fix
#   # budget is exhausted or the turn ends without landing. All three fields are
#   # fail-closed and default off, so leaving them unset preserves today's
#   # immediate per-gate refusal exactly.
#   land_fix_enabled: true
#   # Number of non-terminal fix requests granted before landing refuses and
#   # returns the issue to review. Must be a positive integer; defaults to 2.
#   max_land_attempts: 2
#   # Make a merge conflict a retryable gate too (off by default, so a conflict
#   # otherwise refuses immediately exactly as before).
#   allow_conflict_resolution: false
---
Work on {{.issue.identifier}}: {{.issue.title}}

{{.issue.description}}

Current Linear state: {{.issue.state}}

Follow the repository instructions, validate your changes, and follow the
delivery-mode instructions supplied by Symphony. See this repository's own
WORKFLOW.md for a complete worked example of state-specific start,
implementation, validation, review handoff, rework, landing, and completion
instructions written as one executable policy document.
