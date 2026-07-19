---
tracker:
  kind: linear
  provider:
    # Linear project slug ID for the pmrrasmussen/Symphony project.
    project_slug_id: 6e13e4a9f215
    # Set this to an absolute path for a mode-600 file outside the repository.
    api_key_file: $SYMPHONY_LINEAR_API_KEY_FILE
    # Exact state edges a bound Codex session may request. These are not a
    # general destination allowlist.
    agent_transitions:
      Todo: In Progress
      Merging: In Review
    # Optional, opt-in Codex client tool. It creates a new issue only in this
    # project/team and records the active issue as its Linear parent; it
    # cannot select an arbitrary project, team, or issue. Intended for
    # decomposing one task into several independently reviewable pull
    # requests: normally one child issue per isolated worktree and PR.
    # child_issue_creation: true
    # Enables the scoped github_publish_pr/github_pr_context handoff tools
    # below for the bound issue only. Single-agent bootstrap stage (PMR-36):
    # Merging is not an active state yet, so only Todo and In Progress are
    # ever dispatched, and no session can move an issue past In Review.
    handoff_state: In Review
  active_states: [Todo, In Progress]
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
  # Develop this repository one issue at a time by default. Two-agent
  # operation is deliberately deferred until coordinator conformance and
  # final rollout (PMR-38).
  max_concurrent_agents: 1
  max_turns: 20
codex:
  command: codex app-server
  approval_policy: never
  thread_sandbox: workspace-write
  turn_timeout_ms: 3600000
  read_timeout_ms: 5000
  stall_timeout_ms: 300000
# Host-side GitHub PR handoff, fixed to this repository only (PMR-36). The
# token is host-side and repository-scoped: it is read once from a mode-600
# file outside the repository via $SYMPHONY_GITHUB_TOKEN_FILE, is never
# committed, and Symphony strips it (and any inherited environment value
# containing it) from the Codex child process. See README.md for the
# fine-grained token's required scopes and permissions.
github:
  owner: pmrrasmussen
  repository: symphony
  base_branch: main
  token_file: $SYMPHONY_GITHUB_TOKEN_FILE
  poll_interval_ms: 30000
---
Work on {{.issue.identifier}}: {{.issue.title}}

{{.issue.description}}

Follow the repository instructions. If the linear_graphql transition
operation is available and this issue is still in Todo, move it to In
Progress before you start implementing. Make a focused, validated change
with a clean local commit, then follow the delivery-mode instructions
supplied by Symphony below to hand the work off for review.
