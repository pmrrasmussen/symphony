---
tracker:
  kind: linear
  provider:
    # Linear project slug ID for the pmrrasmussen/Symphony project.
    project_slug: 6e13e4a9f215
    # Keep the API key outside this repository; create this file with mode 600.
    api_key_file: /Users/peter/.config/symphony/linear-api-key
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
  # Develop this repository one issue at a time by default.
  max_concurrent_agents: 1
  max_turns: 20
codex:
  command: codex app-server
  approval_policy: never
  thread_sandbox: workspace-write
  turn_timeout_ms: 3600000
  read_timeout_ms: 5000
  stall_timeout_ms: 300000
---
Work on {{.issue.identifier}}: {{.issue.title}}

{{.issue.description}}

Follow the repository instructions, validate your changes, and leave the issue
in its workflow-defined handoff state.
