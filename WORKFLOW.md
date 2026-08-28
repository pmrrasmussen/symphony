---
# This is Symphony's own delivery policy: the values this repository runs on,
# plus the prompt body below, which is the single executable source of that
# policy. Every field is explained once in WORKFLOW.example.md; the comments
# here say only why this repository chose what it chose.
tracker:
  kind: linear
  provider:
    # Linear project slug ID for the pmrrasmussen/Symphony project.
    project_slug_id: 6e13e4a9f215
    # Set this to an absolute path for a mode-600 file outside the repository.
    api_key_file: $SYMPHONY_LINEAR_API_KEY_FILE
    transitions:
      start:
        Todo: In Progress
      refuse_landing:
        Merging: In Review
    # followup_issue_creation: true
    handoff_state: In Review
  # The canonical lifecycle (PMR-38): Todo -> In Progress -> In Review <->
  # Rework -> Merging -> Done. Operator prerequisites for running it live,
  # including disabling Linear's own PR-to-status automation, are in
  # docs/linear-tracker.md.
  active_states: [Todo, In Progress, Rework, Merging]
  terminal_states: [Done, Canceled]
polling:
  interval_ms: 30000
workspace:
  root: .symphony/workspaces
  # Symphony develops itself, so each issue gets a detached worktree of this
  # very repository.
  source_root: .
hooks:
  timeout_ms: 60000
agent:
  # Four-agent operation. The load-bearing gate is PMR-131 (merged
  # 2026-08-26): a Claude quota rejection now ends its own attempt as a
  # classified terminal event instead of being retried as a generic agent
  # failure, so four concurrent sessions -- which burn a five-hour usage
  # window roughly twice as fast as two -- fail visibly as one quota wall
  # instead of as four issues each looking like an unrelated failure.
  # refresh_base_ref (PMR-141, PR #98, merged 2026-08-26) is the other half:
  # each concurrent implementation/rework agent can clear its own stale
  # origin/main when a peer's merge lands mid-run, instead of failing a
  # stale-base publish. max_concurrent_agents_by_state keeps landing
  # serialized at exactly one Merging agent regardless.
  max_concurrent_agents: 4
  max_concurrent_agents_by_state:
    Merging: 1
  # Lowered from 20 (PMR-134): across the 2026-08-26 dogfood session
  # (.symphony/logs/symphony.jsonl), no run that published used more than 5
  # turns -- including landing (always 1) and Rework runs -- while doomed runs
  # burned the full 20-turn budget without publishing. 8 leaves headroom above
  # every observed success without paying for as long a death spiral.
  max_turns: 8
  # Left at the default. Bounds the number of runs, which max_turns and
  # max_retry_backoff_ms do not.
  max_attempts: 5
codex:
  command: codex app-server
  approval_policy: never
  thread_sandbox: workspace-write
  # networkAccess is enabled because this repository's own tests bind loopback
  # listeners. It grants unrestricted egress, not just local sockets.
  turn_sandbox_policy:
    type: workspaceWrite
    networkAccess: true
  # Left at its default: PMR-134's turn-timeout evidence came from
  # agent.backend claude sessions -- an operator-side override never
  # committed to this file -- so it does not bear on the codex backend this
  # block configures. See WORKFLOW.example.md's commented-out claude: block
  # for that number and the incident that set it.
  turn_timeout_ms: 3600000
  read_timeout_ms: 5000
  start_timeout_ms: 120000
  stall_timeout_ms: 300000
# Host-side GitHub PR handoff and landing, fixed to this repository only
# (PMR-36, PMR-37). WORKFLOW.example.md documents the token's required
# permissions and every field below.
github:
  owner: pmrrasmussen
  repository: symphony
  base_branch: main
  token_file: $SYMPHONY_GITHUB_TOKEN_FILE
  poll_interval_ms: 30000
  # Landing capability (PMR-37, activated for real dispatch by PMR-38).
  # required_checks names the exact GitHub check names this repository's CI
  # reports (the job names in .github/workflows/ci.yml); change both together.
  merge_state: Merging
  merge_method: merge
  required_checks:
    - scripts/check format
    - scripts/check test
    - scripts/check vet
    - scripts/check race
---
Work on {{.issue.identifier}}: {{.issue.title}}

{{.issue.description}}

Current Linear state: {{.issue.state}}

Follow the repository instructions in AGENTS.md, README.md, and WORKFLOW.md.
WORKFLOW.md is the single executable source of delivery policy: the
state-specific guidance below is generated from it for this issue's current
state.
{{if or (eq .issue.state "Todo") (eq .issue.state "In Progress") (eq .issue.state "Rework")}}
## Scope management
- When implementation reveals meaningful work outside this issue, use
  `create_followup_issue` when available to capture it with a clear title,
  description, and acceptance criteria. Use `related` for contextual work or
  `blocked_by_current` only when the follow-up depends on this issue.
- Continue the current issue after filing the follow-up; do not expand its
  scope, wait for the follow-up, or treat it as child-agent orchestration. The
  new issue remains parentless and non-dispatchable in Backlog until a human
  promotes it to an eligible state such as Todo.

## Base branch
- Call refresh_base_ref before bringing the worktree onto the base branch and
  before every github_publish_pr call. It fetches the configured base branch
  host-side and returns its resolved commit, so origin/main is never more
  stale than your last call to it -- including a merge that landed on main
  after you were dispatched. A fetch failure is refused rather than fatal:
  proceed against the base ref you already have.
- Bring the worktree onto the current base branch before every
  github_publish_pr call, and re-run validation if the update moved anything.
  Publishing is refused outright when the worktree HEAD no longer descends from
  origin/main, and any merge to main while you were working causes exactly that.
  Updating your own worktree is expected: the delivery instructions below
  prohibit pushing and opening pull requests yourself, not fetching or merging.
- Which command to use depends on whether this branch has already been
  published, and github_pr_context is how you tell -- you hold no credential
  with which to inspect the remote directly:
  - No pull request yet: `git rebase origin/main`.
  - A pull request already exists: `git merge origin/main`. **Do not rebase.**
    A rebase rewrites commits the remote branch already carries, and publishing
    pushes without force, so the push is then rejected as a non-fast-forward --
    permanently, with no remedy available to you. A merge commit satisfies the
    same descendant check while keeping the push a fast-forward.
- An issue in Rework always has a published pull request, because Rework is
  reachable only through review. Merge; never rebase.
{{end}}
{{if or (eq .issue.state "Todo") (eq .issue.state "In Progress")}}
## Implementation and validation
- Implement a focused, validated change for this issue. Run the narrowest
  relevant check first (for example, one package's tests), then
  `scripts/check all` once shared behavior changes.
- Create a clean local commit once validation passes; do not leave
  uncommitted or untracked changes in the worktree.
- A Claude session's sandbox grants `network.allowLocalBinding`, so a test
  suite that binds a loopback listener (`httptest`, `mcpbridge`) is expected
  to run in-session. If a command still fails with `bind: operation not
  permitted` despite that grant, do not treat it as a regression: it is a
  sandbox limitation Symphony's own diagnostics already flag, not evidence
  about the change. Say so explicitly in the pull request's On Call section
  and rely on CI's required checks to validate that suite instead of
  retrying the command.

## Review handoff
- Follow the delivery-mode instructions Symphony supplies below (host-side
  publish via github_publish_pr, or the manual fallback) once the change is
  committed and validated. Publishing hands the issue to human review in In
  Review; never attempt to merge or move the issue past review yourself.
{{end}}
{{if eq .issue.state "Rework"}}
## Rework
- A human moved this issue back to Rework with feedback on its pull request.
  Resume in this same worktree, branch, and prior commit history; do not
  start over, discard history, or create a new branch.
- Read the requested changes (for example via github_pr_context, which
  reports bounded check status, review state, and unresolved feedback), then
  address them and validate again the same way as above: the narrowest
  relevant check first, then `scripts/check all` once shared behavior
  changes.
- Call github_publish_pr again once the worktree is clean and committed. It
  reuses the existing deterministic pull request and hands the issue back to
  In Review, unless the pull request was merged (irrecoverable) or closed and
  could not be reopened; report either outcome as a blocker instead of
  creating a new pull request yourself.
{{end}}
{{if eq .issue.state "Merging"}}
## Landing
- A human moved this issue to Merging: that move is itself the approval to
  land, so no separate approving review is required.
- Call github_land_pr with no arguments. It re-verifies the worktree,
  branch, pull request, required checks, effective review state, unresolved
  review threads, mergeability, and the base branch immediately before the
  irreversible merge call.

## Hard landing blockers
- A pending-checks or pending-mergeability result is not an error: it ends
  this run. Take no further action and do not call github_land_pr again --
  Symphony releases the worker and redispatches landing itself once checks
  settle, so retrying here only wastes turns.
- Any other refusal (a failing check, an effective changes-requested review,
  an unresolved review thread, a stale base, a merge conflict, or a closed or
  mismatched pull request) has already returned this issue to In Review for a
  human; do not retry the merge yourself or transition the issue directly.
  Report the refusal reason as a blocker.

## Completion
- A successful merge, or a pull request GitHub already reports merged,
  transitions this issue to Done automatically and ends this run. Take no
  further Linear action yourself, and never call github_land_pr again after a
  merged result: the capability is closed for this run and refuses.
{{end}}
