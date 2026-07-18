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

The loader validates the supported core front-matter fields but preserves
unknown extension keys for forward compatibility. It applies documented
defaults, resolves explicit `$VARNAME` references only for documented secret
and path fields, and normalizes paths relative to the workflow file. Valid
changes take effect for future work without a restart; invalid reloads retain
the last known good configuration. Prompts render strictly per run with
lowercase `issue` and nullable `attempt` variables, so template failures do
not prevent polling or configuration reload.

Authoritative durable state is Linear plus the workspace tree under the
configured root.  In-memory claims are rebuilt after restart; startup cleans
workspaces for terminal issues.  Logs are written to the configured log root
and must not contain credentials or complete agent payloads.

When `workspace.source_root` is configured, `LocalWorkspaceExecutor` creates a
detached Git worktree for each issue. This isolates Codex changes from the
checkout running Symphony; a human must review and integrate the resulting
changes. The source root must already have a commit, and Git worktrees require
the local repository to be trusted. Completion markers are held below the
configured workspace root and are keyed to the issue's Linear `updatedAt`
value: a completed unchanged issue is not dispatched again, while a later
issue edit is eligible for another turn. Cleanup refuses to remove a worktree
with local changes or a commit that differs from its recorded base revision.

Workspace containment is checked against canonical filesystem paths, including
existing symlink ancestors. Service-owned workspace directories, the durable
state directory, and state-marker files must not themselves be symlinks; a
path that resolves outside the configured root is rejected before it is read,
executed, or removed. This is a trusted-local-machine boundary, not a defence
against a malicious same-host process: filesystem checks and subsequent use
cannot atomically prevent a concurrent rename or symlink swap. Keep the
configured workspace root writable only by trusted users and processes.

The behavioral contract is the pinned upstream
[Symphony specification](https://github.com/openai/symphony/blob/7af5a7648c9fbffa08825fe0c0b18be00100aff3/SPEC.md).
It is linked rather than copied into this repository so it cannot silently
diverge from its source. Implementation baseline: upstream Symphony commit
`7af5a7648c9fbffa08825fe0c0b18be00100aff3`. Codex app-server protocol was
inspected from the locally installed Codex schema generated on 2026-07-18;
upstream Codex HEAD at inspection was `56395bddaf26eb2829387ca6a417bf9128e5b239`.
