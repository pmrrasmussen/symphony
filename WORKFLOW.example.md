---
tracker:
  kind: linear
  provider:
    project_slug: example-project
    # Environment expansion accepts the exact $VARNAME form only.
    api_key: $LINEAR_API_KEY
    # Optional: only dispatch issues assigned to this Linear user ID, or `me`.
    # assignee: me
    # Optional, opt-in Codex client tool. It can only read/comment on the
    # active issue or move it to this non-active state; it is never a GraphQL
    # proxy. Configure a state from this project's issue team.
    # handoff_state: In Review
    # handoff_comment_template: "Ready for review: {{.issue.identifier}}"
  active_states: [Todo, In Progress]
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
  max_concurrent_agents: 2
  max_turns: 20
codex:
  command: codex app-server
  approval_policy: never
  thread_sandbox: workspace-write
  turn_timeout_ms: 3600000
  read_timeout_ms: 5000
  stall_timeout_ms: 300000
# Optional. Requires tracker.provider.handoff_state and a fine-grained token
# restricted to exactly this repository. token_file may replace token.
# github:
#   owner: your-github-owner
#   repository: your-repository
#   base_branch: main
#   token: $SYMPHONY_GITHUB_TOKEN
#   poll_interval_ms: 30000
---
Work on {{.issue.identifier}}: {{.issue.title}}

{{.issue.description}}

Follow the repository instructions, validate your changes, and leave the issue
in its workflow-defined handoff state.
