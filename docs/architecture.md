# Architecture and trust model

This service follows the Symphony specification using a coordination core that
only knows normalized issues, agent events, and workspace operations.  Linear
GraphQL, Codex JSON-RPC, and local process execution are adapters behind
`Tracker`, `AgentBackend`, and `WorkspaceExecutor` respectively.

The initial implementation is intentionally for a trusted local machine.
`WORKFLOW.md` is repository-owned, versioned policy and its hooks are trusted
shell code.  Codex can make changes and execute commands according to its
configured approval and sandbox policy; this service does not provide Docker,
VM, SSH, distributed execution, or a database.  Linear credentials stay in
the host process and are removed from Codex's environment.

Authoritative durable state is Linear plus the workspace tree under the
configured root.  In-memory claims are rebuilt after restart; startup cleans
workspaces for terminal issues.  Logs are written to the configured log root
and must not contain credentials or complete agent payloads.

When `workspace.source_root` is configured, `LocalWorkspaceExecutor` creates a
detached Git worktree for each issue. This isolates Codex changes from the
checkout running Symphony; a human must review and integrate the resulting
changes. The source root must already have a commit, and Git worktrees require
the local repository to be trusted.

Implementation baseline: upstream Symphony commit
`7af5a7648c9fbffa08825fe0c0b18be00100aff3`.  Codex app-server protocol was
inspected from the locally installed Codex schema generated on 2026-07-18;
upstream Codex HEAD at inspection was `56395bddaf26eb2829387ca6a417bf9128e5b239`.
