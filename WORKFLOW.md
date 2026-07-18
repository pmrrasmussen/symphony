---
tracker:
  kind: linear
  provider:
    # Linear project slug ID for the pmrrasmussen/Symphony project.
    project_slug_id: 6e13e4a9f215
    # Set this to an absolute path for a mode-600 file outside the repository.
    api_key_file: $SYMPHONY_LINEAR_API_KEY_FILE
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
# Optional GitHub PR lifecycle (disabled until configured). Requires
# tracker.provider.handoff_state and a repository-scoped fine-grained token.
# github:
#   owner: pmrrasmussen
#   repository: symphony
#   base_branch: main
#   token_file: $SYMPHONY_GITHUB_TOKEN_FILE
#   poll_interval_ms: 30000
---
Work on {{.issue.identifier}}: {{.issue.title}}

{{.issue.description}}

Follow the repository instructions, validate your changes, and follow the
delivery-mode instructions supplied by Symphony.
