---
# This file is the annotated reference for every WORKFLOW.md front-matter
# field: what each key does, what it defaults to, and which of them fail the
# whole configuration rather than being ignored. This repository's own
# WORKFLOW.md carries only its chosen values and points here for the
# explanation.
#
# Front matter is validated for the supported core fields while unknown
# extension keys are preserved for forward compatibility. Environment-backed
# fields use exact $VARNAME syntax; braced or compound forms are rejected
# rather than treated as a literal path or secret. Relative workspace and log
# paths resolve from this file's own location. A changed workflow file is
# reloaded for future work -- including a changed environment value or
# credential-file content, so those can change without editing the file -- and
# an invalid replacement keeps the last valid configuration. See
# docs/architecture.md for what a reload does and does not affect, and
# docs/preflight.md for validating a change with --dry-run before it reaches a
# running daemon.
tracker:
  kind: linear
  provider:
    # Canonical Linear project scope. The legacy project_slug alias still works
    # during its deprecation and emits a value-free migration warning;
    # configuring both names is rejected as ambiguous.
    project_slug_id: example-project
    # Recommended: point this variable at a mode-600 file outside the repo. A
    # literal trusted local path is also accepted for a workflow file that is
    # not versioned. Inline and file-backed keys are whitespace-trimmed before
    # use and never appear in a log.
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
      # failed move never blocks or double-dispatches the run. The move is
      # applied only while the issue's freshly read state is still the edge's
      # source, so a human who moves it during dispatch is never overridden.
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
    # such change keeps the warning; see docs/linear-tracker.md.
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
  #
  # The two lists must be disjoint, compared without regard to case or
  # surrounding whitespace, and a state named in both fails validation. The
  # scheduler tests them independently, so an overlapping state would be a
  # dispatch candidate and a stop-and-clean-up target at once, and the two
  # would take turns acting on the same issue on every poll.
  active_states: [Todo, In Progress, Rework, Merging]
  terminal_states: [Done, Canceled]
polling:
  interval_ms: 30000
workspace:
  # Where per-issue agent workspaces are created. Omitting it defaults to
  # symphony_workspaces under the system temporary directory.
  root: .symphony/workspaces
  # Optional: create each new issue workspace as a detached Git worktree at a
  # freshly fetched origin/main. Existing issue workspaces retain their history.
  # The original checkout is never used as an agent workspace. See
  # docs/completion-markers.md for ownership, cleanup, and recovery.
  #
  # Under agent.backend: claude, point this at a clone you can throw away, not
  # at the checkout you work in. A Bash command in that backend reaches the
  # whole of this repository's .git -- branch refs included -- because the CLI
  # widens its own git-metadata grant; Symphony detects an unexplained ref move
  # after the fact and fails that run, but does not prevent it. See
  # docs/architecture.md, "The source .git exposure".
  source_root: .
hooks:
  timeout_ms: 60000
agent:
  # Selects the agent runtime new sessions start on: codex (the Codex
  # app-server) or claude (the Claude Code CLI). Any other value fails the whole
  # configuration candidate, and a workflow that omits the key runs on codex. A
  # change takes effect for sessions started after the reload; an in-flight
  # session keeps the backend it started on. Only the selected backend's launch
  # block below has to be complete.
  backend: codex
  # Four-agent operation. The load-bearing gate is PMR-131: a Claude quota
  # rejection now ends its own attempt as a classified terminal event instead
  # of being retried as a generic agent failure, so four concurrent sessions
  # -- which burn a five-hour usage window roughly twice as fast as two --
  # fail visibly as one quota wall instead of as four issues each looking like
  # an unrelated failure. refresh_base_ref (PMR-141) is the other half: each
  # concurrent implementation/rework agent can clear its own stale
  # origin/main when a peer's merge lands mid-run, instead of failing a
  # stale-base publish. max_concurrent_agents_by_state keeps landing
  # serialized at exactly one Merging agent regardless.
  max_concurrent_agents: 4
  max_concurrent_agents_by_state:
    Merging: 1
  # Lowered from 20 (PMR-134): a dogfood session showed no run that published
  # used more than 5 turns -- including landing and Rework runs -- while
  # doomed runs burned the full 20-turn budget without publishing. 8 leaves
  # headroom above every observed success without paying for as long a death
  # spiral.
  max_turns: 8
  # The one bound on the number of *runs*. max_turns bounds the turns inside a
  # run and max_retry_backoff_ms bounds the delay between runs; neither stops an
  # issue that fails the same way every time -- a corrupted worktree, a
  # before_run hook that always exits non-zero, a prompt template error, an
  # unreachable agent binary -- from re-dispatching at the backoff ceiling for
  # the daemon's lifetime while holding its claim.
  #
  # After this many failed dispatches of one issue Symphony abandons that
  # dispatch: one error-level "dispatch abandoned after max attempts" record
  # (operation: dispatch_abandoned) naming the classified reason, the claim and
  # the retry timer dropped. The tracker is deliberately left alone -- no
  # transition, no comment -- so the issue stays visible and a later poll may
  # start a fresh, equally bounded episode; the error record is the signal that
  # a human, not another retry, is what the issue needs.
  #
  # "A later poll" is not the next one. Abandonment also cools the issue down
  # for ten times the longer of max_retry_backoff_ms and polling.interval_ms
  # (reported as cooldown_ms on that record), during which the poll refuses it
  # under its own abandon_cooldown reason and lists it in the waiting set. There
  # is no setting for the window: it is derived so that this ceiling bounds the
  # spend and not merely one episode of it. The cooldown is in-process, so
  # restarting the service clears it and re-admits the issue at once.
  #
  # A non-terminal landing wait is exempt: it is not an agent failure, does not
  # escalate the attempt, and keeps the bounded-delay redispatch described under
  # github.poll_interval_ms below. Defaults to 5; must be a positive integer.
  max_attempts: 5
# Read only when agent.backend selects codex. Unknown keys inside it are
# refused rather than ignored, exactly as in the claude: block below, so a
# misspelled field cannot leave a default silently in place.
codex:
  # A shell command: it runs through `bash -c`, so quoting, expansion, and
  # operators all work here. That shell is not a login shell, so ~/.bash_profile
  # and its siblings are never sourced: a profile would otherwise re-export a
  # credential the host filter strips on the way in, and put its own greetings
  # on the stdout Symphony parses as JSON-RPC, failing the session at start.
  # Anything this command needs from a profile -- a version manager, a PATH
  # entry -- belongs in the command itself or in the daemon's own environment.
  # Defaults to `codex app-server`.
  command: codex app-server
  approval_policy: never
  thread_sandbox: workspace-write
  # Per-turn sandbox policy, validated against the Codex SandboxPolicy schema
  # here and then forwarded verbatim to Codex, where it overrides
  # thread_sandbox for this and every later turn -- so the two must agree, and
  # a policy requesting write authority the thread mode does not have is
  # rejected at load. writableRoots is rejected outright, since Symphony
  # supplies the narrowed Git roots itself, and an unknown key inside the
  # policy is rejected too, so a typo such as `networkAcces` cannot silently
  # leave the setting off.
  #
  # workspaceWrite bounds *writes* to the issue's own worktree plus the two
  # narrow Git metadata roots Symphony grants for a local commit. It does not
  # restrict *reads*: a worker can read any file this user can, including
  # credential files outside the worktree. That is a property of Codex's
  # sandbox modes generally -- read-only included -- not of this setting.
  #
  # networkAccess: true grants unrestricted outbound network access. Codex
  # exposes only this boolean; nothing narrower is expressible. Loopback
  # listeners for repository tests (Go httptest servers, for example) are the
  # reason to enable it, not the limit of what it permits.
  #
  # Host-owned Linear and GitHub secrets are stripped from the Codex child
  # *environment* by name and by value, so no host credential is passed to the
  # worker as a variable. With reads and egress both available, the protection
  # against credential exfiltration is that no untrusted input reaches the
  # worker -- not the sandbox. Omit the key to leave egress denied.
  turn_sandbox_policy:
    type: workspaceWrite
    networkAccess: true
  # Left at its default: PMR-134's turn-timeout evidence concerned
  # agent.backend claude, not codex -- see the commented-out claude: block
  # below for that number and the incident that set it.
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
# Read only when agent.backend selects claude; with backend: codex above, this
# block is ignored (and the codex: block is ignored under backend: claude).
# Unknown keys inside it are refused rather than ignored, so a misspelled field
# cannot leave a default silently in place.
#
# There is deliberately no approval_policy, thread_sandbox, or
# turn_sandbox_policy counterpart here. Symphony fixes the Claude launch
# contract itself and re-applies it on every turn -- the CLI restores none of it
# on --resume -- so it cannot be widened from this file: a Bash/Edit/Glob/Grep/
# Read/Write tool surface with WebFetch and WebSearch denied, a fail-closed
# permission mode, no settings discovery at all (user, project, and local
# sources are excluded, so a workspace repository shipping its own
# .claude/settings.json or hooks cannot widen the boundary),
# at most one MCP server -- Symphony's own private loopback capability
# endpoint, and only when this workflow configures a capability, as below --
# and a sandbox that refuses to start rather than degrading,
# bounding *writes* to the worktree plus the same two narrow Git metadata roots
# the codex profile is granted.
#
# The same honest limits apply as above: *reads* are not confined and outbound
# network is unrestricted, with loopback binding separately granted so
# repository tests that stand up an httptest server or similar can run
# in-session. The sandbox itself governs Bash by path; Edit and Write are
# confined by their own path-scoped permission rule instead (PMR-156). One
# gap remains open, in the CLI's own sandbox rather than in this payload: a
# Bash command can still write anywhere under the source repository's .git
# directory. docs/architecture.md states all five limits in full.
#
# The CLI also persists its own full transcript -- prompts, issue text, and tool
# output -- under ~/.claude/projects/, outside the worktree; see
# docs/observability.md.
#
# A claude workflow may configure Symphony's session capabilities --
# tracker.provider.handoff_state, tracker.provider.followup_issue_creation, and
# an enabled github: integration below. They reach the session over a private
# loopback MCP endpoint, so the CLI serves each one as mcp__symphony__<tool>;
# Symphony's host-generated delivery guidance renders those names, so the prompt
# body below needs no per-backend wording. Two combinations are rejected at load
# for this backend: an enabled github: integration without handoff_state (the
# scoped publish tool is then either absent, or advertised while the run is told
# publishing is unavailable and a publish would leave the pull request created
# and the issue untransitioned), and a handoff_state with neither an enabled
# github: integration nor followup_issue_creation (nothing model-facing uses it).
# Both stay valid under codex. (A github: block that does not resolve stays
# disabled, so it configures nothing and reaches neither rule.) The service
# user must already be logged in to the CLI; docs/preflight.md describes the
# read-only check --dry-run makes of that.
# claude:
#   # argv, not a shell command: split on whitespace and executed directly, so
#   # no shell, no quoting, no expansion, and no operators. codex.command above
#   # runs through bash -c, so quoting that works there breaks here. --dry-run
#   # checks both with sh -n, which is a loose superset for this field.
#   # Defaults to claude.
#   command: claude
#   # Optional, and unset by default: omit it and the CLI is given no --model
#   # flag at all, so it selects its own.
#   model: sonnet
#   # Defaults to 3600000 (one hour), as codex.turn_timeout_ms does.
#   # Lowered from one hour (PMR-134), but not to the 900000 (15 min) first
#   # tried: that value was live-tested during a dogfood session and killed a
#   # real, productive turn at exactly 900000 ms, destroying 433 uncommitted
#   # insertions across 7 files that a resumed turn went on to commit in 3
#   # minutes. 1800000 (30 min) is still half the one-hour ceiling that let a
#   # legitimate 17-minute, 10.4M-token turn run to completion.
#   turn_timeout_ms: 1800000
#   # There is no read_timeout_ms or start_timeout_ms counterpart: one turn is
#   # one process, so there is no steady-state round trip to bound. Defaults to
#   # 300000, as codex.stall_timeout_ms does.
#   stall_timeout_ms: 300000
# Optional host-side GitHub integration: it publishes a completed, clean
# worktree, creates or reuses that branch's pull request, and can land an
# approved one. It requires tracker.provider.handoff_state above and a token
# scoped to exactly this one repository, granting only the pull request,
# checks, contents, and review permissions the integration uses. Store it in a
# mode-600 file outside the repository and reference that path here; the token
# is never committed, and Symphony strips the resolved value (and any inherited
# environment variable containing it) from every child process it spawns. Note
# that GitHub disabled the Checks permission for fine-grained tokens, which the
# required-checks gate depends on -- see docs/dogfooding.md before choosing a
# token type. Invalid or incomplete settings here disable the capabilities and
# preserve the manual delivery path, except where noted below. Disabling that
# way is not silent: a present but unusable block adds a warning naming every
# offending field, which `--dry-run` reports as its own check.
# github:
#   owner: your-github-owner
#   repository: your-repository
#   # Any branch name Git accepts, slashes included: release/1.0 is valid here
#   # even though a slash in owner or repository above is not. Omit it for
#   # main. Worktrees are cut from this branch and pull requests target it.
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
#   # review is required. Such a session is served github_land_pr *instead of*
#   # github_publish_pr, and its delivery instructions say so: publishing from
#   # a landing dispatch would push the branch and hand an already-approved
#   # issue back to review, so it is withheld and refused (PMR-169). It must be one of active_states and differ from
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
#   # Opt in to GitHub's deterministic update-branch operation for the one
#   # window this setting is about: the base branch moving between
#   # github_land_pr's early base read and the one it takes immediately before
#   # merging, while every other landing gate passes. It creates only a
#   # merge-from-base commit, then github_land_pr waits for checks on that new
#   # head. Disabled by default; a later base move still refuses landing.
#   #
#   # It is not about a pull request that is merely behind the base branch.
#   # Landing never compares the head to the base, so a behind-but-mergeable
#   # pull request merges as-is whether this is set or not; what this setting
#   # buys is that a base moving underneath a landing session recovers instead
#   # of returning the issue to review (PMR-169).
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
