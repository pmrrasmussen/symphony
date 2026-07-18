---
tracker:
  kind: linear
  provider:
    # Replace only in a local copy or the manually dispatched CI workflow.
    project_slug_id: __LINEAR_SMOKE_PROJECT_SLUG__
    api_key: $LINEAR_API_KEY
  active_states: [Todo, In Progress]
  terminal_states: [Done, Canceled]
polling:
  interval_ms: 30000
workspace:
  root: .symphony/smoke-workspaces
  source_root: .
hooks:
  timeout_ms: 60000
agent:
  max_concurrent_agents: 1
  max_turns: 1
codex:
  command: codex app-server
  approval_policy: never
  thread_sandbox: workspace-write
  turn_timeout_ms: 3600000
  read_timeout_ms: 5000
  stall_timeout_ms: 300000
---
Smoke-test only. Do not modify files, create branches, commit, push, or change
Linear issue state. Report the result in the existing dedicated smoke issue.
