---
tracker:
  kind: linear
  provider:
    project_slug: example-project
    api_key: $LINEAR_API_KEY
    # Optional: only dispatch issues assigned to this Linear user ID, or `me`.
    # assignee: me
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
---
Work on {{.Issue.Identifier}}: {{.Issue.Title}}

{{.Issue.Description}}

Follow the repository instructions, validate your changes, and leave the issue
in its workflow-defined handoff state.
