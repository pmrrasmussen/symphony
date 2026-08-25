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
    # Host-owned tracker transition policy. Symphony applies every edge itself,
    # with the host Linear credential; none is exposed to a Codex session, so
    # the agent has no tracker-write capability. The two edge sets are kept
    # distinct on purpose (never flattened into one map): Merging is both a
    # dispatchable state and the land-fallback source, so a flat map applied at
    # dispatch would wrongly move a dispatched Merging landing issue.
    transitions:
      # Applied at dispatch, keyed by the issue's current state (Todo ->
      # In Progress); both endpoints must be active, non-terminal states.
      # Idempotent and fail-safe: an already-started issue is untouched and a
      # failed move never blocks or double-dispatches the run.
      start:
        Todo: In Progress
      # The Merging -> In Review fallback a refused github_land_pr uses (keyed
      # by github.merge_state below). Never applied at dispatch.
      refuse_landing:
        Merging: In Review
    # Enables the scoped github_publish_pr/github_pr_context Codex tools below
    # and is the state github_publish_pr hands off to, host-side. Configure a
    # non-active, non-terminal state from this project's Linear team; it is the
    # single human-controlled review state. No Codex tool can move an issue to
    # it (or any state); the host performs every transition. Operator
    # prerequisite: disable the tracker's native GitHub PR-to-status automation
    # for this team/project — it races the handoff and can flap the issue back
    # to an active state (PMR-63). Symphony warns on any such external revert
    # (operation: external_reversion). The expected human decisions out of this
    # state are logged as expected instead — approval to land
    # (operation: review_approved) and changes requested
    # (operation: rework_requested) — but only once the lifecycle names them
    # unambiguously: review_approved needs github.merge_state below, and
    # rework_requested needs exactly one active_states entry that neither
    # transitions.start nor github.merge_state accounts for. Otherwise every
    # such change keeps the warning; see README.md and docs/linear-tracker.md.
    handoff_state: In Review
    # Optional, opt-in Codex client tool for capturing meaningful out-of-scope
    # work. It creates a parentless issue only in this project/team, always in
    # Backlog, and may only relate it to the active issue or mark it blocked by
    # the active issue. Backlog must stay outside active_states so a human must
    # promote the follow-up to Todo before Symphony can dispatch it.
    # followup_issue_creation: true
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
  # Optional: create each new issue workspace as a detached Git worktree at a
  # freshly fetched origin/main. Existing issue workspaces retain their history.
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
  # Per-turn sandbox policy, forwarded verbatim to Codex. workspaceWrite keeps
  # every write inside the issue's own worktree (plus the two narrow Git
  # metadata roots Symphony grants for a local commit); networkAccess only
  # lifts the socket restriction, so repository validation that binds a local
  # loopback listener -- Go httptest servers, for example -- can run. It
  # broadens no filesystem authority, and host-owned Linear/GitHub credentials
  # are still stripped from the Codex child environment, so the agent has no
  # credential to spend on the network it can now reach. Omit the key to keep
  # the launcher's narrowed workspace-write grant with sockets denied.
  turn_sandbox_policy:
    type: workspaceWrite
    networkAccess: true
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
# Optional. Requires tracker.provider.handoff_state and a fine-grained token
# restricted to exactly this repository.
# github:
#   owner: your-github-owner
#   repository: your-repository
#   base_branch: main
#   token_file: $SYMPHONY_GITHUB_TOKEN_FILE
#   # Alternatively: token: $SYMPHONY_GITHUB_TOKEN
#   # Paces the linked pull-request poll loop, and is also the floor for the
#   # delayed landing redispatch after github_land_pr reports a non-terminal
#   # wait (pending checks or undetermined mergeability). Consecutive waits
#   # escalate that delay toward agent.max_retry_backoff_ms.
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
