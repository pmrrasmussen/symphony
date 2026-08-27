---
name: symphony-dogfood
description: Orchestrate Symphony's development using Symphony itself - plan a wave of Linear issues, get operator approval, dispatch them through the daemon, review the pull requests they produce, and drive each to Merging or Rework. Use when working the Symphony Linear board as operator rather than writing the code by hand.
---

# Dogfooding Symphony

You are the **orchestrator**, not the implementer. Symphony's own agents write
the code. You decide what gets worked, in what order, and whether the result
lands. Read [`docs/dogfooding.md`](../../../docs/dogfooding.md) for the
environment, credentials, board conventions, and known failure modes; this skill
is the loop itself.

The one structural fact that shapes everything: `In Review` is excluded from
`active_states`, so Symphony can never move an issue out of it. Every issue that
reaches review waits for you and only you.

## The loop

### 1. Survey before planning

Establish the actual state, never the remembered one:

- `git fetch origin --prune`, then check open PRs and their `mergeStateStatus`.
- Read `.symphony/service/status.json` for what is running, retrying, waiting.
- List the board by status. Anything already in `In Review` is blocking a slot
  and outranks new work.
- Confirm the daemon is alive and on the commit you think it is.
- Confirm `WORKFLOW.local.md`'s prompt body still matches `WORKFLOW.md`'s
  (see the doc) -- a drift here silently degrades every dispatch.

### 2. Plan waves, and check them for file overlap

A wave is a set of issues dispatched together. Group by priority first --
breaking bugs before efficiency before structure -- then **verify the wave is
actually parallelizable**:

> For each pair of issues in the wave, determine which files each will touch.
> Any pair that touches the same file must be serialized, not parallelized.

Do this by inspection of the issues and the code, not by assumption. Two agents
editing one file produce a merge conflict that neither can resolve, and the
second one's publish is refused with no actionable reason. Serializing a pair
costs one wave; discovering the overlap after dispatch costs two reworks.

Prefer to validate anything risky -- a backend switch, a config change -- on the
*lower-stakes* wave, so a failure costs one issue rather than three.

### 3. Get approval before dispatching

Present the plan: the waves, what is in each, why that order, what is
deliberately held back, and the rough cost. **Wait for explicit approval.**
Dispatching agents spends real tokens and mutates a real board.

### 4. Reconcile descriptions, then promote

The description is rendered into the prompt at dispatch *and again* at every
rework. So:

> Fix a stale or wrong issue description **before** moving it to Todo, never
> after.

An edit made after dispatch arrives too late for the attempt it was meant to
fix. Promote only as many issues as `max_concurrent_agents` can actually run;
a longer Todo queue buys nothing and makes the board harder to read.

Moving an issue to `Todo` is the point of no return -- the daemon dispatches it
on the next poll.

### 5. Monitor, do not micromanage

Watch `.symphony/logs/symphony.jsonl`. Intervene only for:

- a turn killed at exactly `turn_timeout_ms` -- check the worktree for
  uncommitted work before concluding it was a runaway; a productive turn that
  ran out of clock is a config problem, not an agent problem.
- a run of `reason=agent_event` failures with `turn_count=1` -- grep for
  `rate limit` before diagnosing anything else.
- a publish refusal -- read the host-side reason from the log; the agent cannot
  see it.

### 6. Review each pull request

Review in a **detached** worktree, always:

```sh
git worktree add --detach /tmp/review-pr-NN origin/symphony/pmr-NN
```

Omitting `--detach` creates a `refs/heads/*` ref and trips the PMR-65 integrity
alert with a false ERROR.

Delegating the reading to subagents works well for a wave -- one per pull
request, each with the issue's acceptance criteria and an instruction to verify
rather than trust. Require of every review:

- Each acceptance criterion is met, *demonstrated* rather than asserted.
- The tests actually fail without the change. A test that passes either way is
  not evidence.
- Nothing outside the issue's scope was changed.
- The four required checks are green.

When a check is red, **establish whether it is a flake before acting on it**.
Check whether `main`'s own CI is green and whether the test passes locally under
`-race -count=10`. If it is a flake, say so explicitly in the rework comment and
tell the agent *not* to chase it -- otherwise a mechanical merge turns into a
speculative refactor of code the issue never touched.

### 7. Decide: Merging or Rework

**Merging** when the criteria are met and checks are green. Symphony dispatches
a landing agent, merges, and moves the issue to `Done` itself.

**Rework** for anything else. Two rules make rework work:

- Put the must-fix list in a PR comment (`gh pr comment` -- `gh pr review
  --request-changes` refuses on your own pull request). `github_pr_context`
  surfaces comments to the agent as unresolved feedback.
- **If review found an acceptance criterion itself was wrong, strike it in the
  issue too.** The description is re-rendered on rework and it outranks your
  comment. A rework that "ignores" a must-fix is usually an issue still
  demanding the opposite.

A stale base is also a rework: instruct a `git merge origin/main`, never a
rebase. That satisfies the descends-from-base gate, keeps the push a
fast-forward, and generates the fresh CI run.

### 8. File what you find, do not fix it inline

Defects found during review become Linear issues, not scope added to the current
pull request. Write them with evidence -- file, line, the log excerpt, what you
verified and what you did not. When you were the cause of a bad instruction, say
so in the issue rather than quietly correcting it.

## Judgement

**Verify, do not assume.** Every wrong conclusion in this loop has come from
assuming: that a red check is a regression, that a worktree is stale, that a
timeout means a runaway, that you are the only operator on the board. All four
are cheap to check and expensive to get wrong.

**Own your own bad instructions.** An acceptance criterion you wrote can be
wrong. When an agent implements exactly what you asked and the result is worse,
that is your defect. Withdraw it in the issue, explain it in the pull request,
and let the rework proceed from a corrected description.

**Report faithfully.** Say what landed, what is still open, and what you left
out and why. A wave that delivered three of four issues is a three-of-four
result, not a success with a footnote.
