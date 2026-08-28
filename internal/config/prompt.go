package config

import (
	"bytes"
	"fmt"
	"strings"
	"text/template"

	"github.com/pmrrasmussen/symphony/internal/domain"
)

// Render renders a prompt for one run. The first run has a nil attempt;
// retries and continuations receive the spec's 1-based attempt number.
func (s Settings) Render(issue any, attempt int) (string, error) {
	t, err := template.New("workflow").Option("missingkey=error").Parse(s.Prompt)
	if err != nil {
		return "", fmt.Errorf("template_parse_error: %w", err)
	}
	var templateAttempt any
	if attempt > 0 {
		templateAttempt = attempt
	}
	var out bytes.Buffer
	err = t.Execute(&out, map[string]any{"issue": templateIssue(issue), "attempt": templateAttempt})
	if err != nil {
		return "", fmt.Errorf("template_render_error: %w", err)
	}
	return out.String(), nil
}

// LinearSessionCapabilityEnabled reports whether a bound Linear session must be
// prepared for a Codex run. The agent has no issue-state transition tool: the only
// things that still require a bound session are the host-owned review handoff
// object (handoff_state; used by github_publish_pr's LinkAndHandoff and by the
// landing/reconciliation host methods) and the opt-in create_followup_issue
// tool.
// Every board-affecting transition is applied host-side, so no model-invokable
// path can change an issue's workflow state.
func (s Settings) LinearSessionCapabilityEnabled() bool {
	return strings.TrimSpace(s.Tracker.HandoffState) != "" || s.Tracker.FollowupIssueCreation
}

// HostSidePublishPromiseMarker opens the host-side publish delivery mode. It is
// exported because it is the only part of the rendered prompt another package
// reads: internal/claude's launch guard compares what the prompt actually says
// against what the session it is about to start can actually serve, and reading
// the artifact is strictly stronger than re-deriving the promise from settings --
// the prompt was rendered from a settings snapshot the backend never sees.
const HostSidePublishPromiseMarker = "Delivery mode: host-side publish is available for this run."

// HostSidePublishPromised reports whether a run under these settings is told
// that host-side publish is available. It is the exact condition
// DeliveryInstructions branches on, named so that a backend can cross-check the
// promise against what its own session actually advertises.
//
// It exists as a predicate rather than as an inline condition for one reason: the
// prompt is rendered before a session exists, so the only thing that can catch a
// promise the session cannot keep is a comparison made at launch, and a
// comparison against a paraphrase of this condition would drift from the branch
// it is meant to mirror. internal/claude.Backend.Start is the caller.
//
// HandoffState is trimmed for the same reason every sibling predicate trims it
// (LinearSessionCapabilityEnabled here, linear.PrepareWithSettings,
// capability.landAdvertised): an all-whitespace value is unreachable through
// Load, but this predicate is now consumed by two other packages, and an
// untrimmed one would make the promise true while the handoff session and the
// GitHub session built from the same field are both nil -- every launch then
// refusing at session_start, with retry and backoff.
func (s Settings) HostSidePublishPromised() bool {
	return s.GitHub.Enabled && strings.TrimSpace(s.Tracker.HandoffState) != ""
}

// SessionCapabilityAdvertisable reports whether any bounded capability could be
// advertised to this run's agent under these settings.
//
// It is deliberately the settings-only half of what internal/capability.Build
// decides, and it is an upper bound: github_land_pr additionally depends on the
// bound issue's current state, which no configuration can answer.
//
// The github term is HostSidePublishPromised rather than GitHub.Enabled, and the
// reason is narrower than it looks: an enabled integration with no handoff_state
// does advertise github_publish_pr whenever follow-up issues are on, and what
// makes the term correct anyway is that the boolean is already true through the
// FollowupIssueCreation term in exactly that case. That configuration is
// separately refused for claude -- see the residual rule in decode, and
// docs/architecture.md's opening section for why advertising publish with no
// handoff state is worse than advertising nothing.
func (s Settings) SessionCapabilityAdvertisable() bool {
	return s.Tracker.FollowupIssueCreation || s.HostSidePublishPromised()
}

// MCPToolPrefix is how an MCP-framed backend renames a Symphony capability: the
// Claude Code CLI derives every tool name it serves from the MCP server the tool
// came from, so the capability the registry calls github_publish_pr reaches the
// model as mcp__symphony__github_publish_pr.
//
// The authority for the server name is internal/claude's --mcp-config payload,
// not this constant. This is the mirror the prompt has to render, and
// internal/claude owns the test that the two are identical. Importing the
// launcher from here is a cycle, and the remaining alternative -- moving the
// launcher's server name into the workflow schema -- would make a transport
// detail look like repository policy and let a workflow rename it.
const MCPToolPrefix = "mcp__symphony__"

// MCPNamingRuleMarker opens the tool-naming rule the Claude branch of
// DeliveryInstructions prepends. Like HostSidePublishPromiseMarker it is exported
// because internal/claude's launch guard reads the rendered prompt: a prompt that
// names a capability without the prefix is safe exactly when it also carries this
// rule, and unsafe otherwise.
const MCPNamingRuleMarker = "Tool naming: Symphony's bounded tools reach you through a single MCP server"

// DeliveryInstructions describe the only PR delivery capability available to
// a worker. Host-generated guidance prevents a stale repository prompt from
// telling a restricted worker to publish directly to GitHub.
//
// backend is the resolved backend this dispatch will start on. It is a parameter
// rather than a field read off s.Agent.Backend, and rather than a new workflow
// key, because the coordinator has already resolved it for this dispatch and
// hands the same value to the agent router: passing it makes the prompt and the
// launch one decision, so there is no representable state where guidance is
// rendered for a backend other than the one that starts the session. How a
// transport names a tool is also not repository policy, so it must not be
// configurable.
//
// The Claude branch is why this function takes a backend at all: WORKFLOW.md's
// prompt body is repository-owned and names Symphony's tools bare, which is what
// a Codex dynamic tool is called and not what the same capability is called once
// it is served over MCP. The rule below is stated over the prefix rather than
// over an enumeration of names, so it cannot go stale against a workflow body
// that adds one. See docs/architecture.md's opening section.
func (s Settings) DeliveryInstructions(backend string) string {
	// Only the Claude transport renames Symphony's tools. Every other backend --
	// today only codex, whose dynamic tools carry the registry's own names --
	// keeps the bare names, so for those this function renders byte-identically
	// to what it rendered before this parameter existed. A future MCP-framed
	// backend has to opt in here explicitly rather than inherit either answer by
	// default.
	tool := func(name string) string { return name }
	preamble := ""
	if backend == ClaudeAgentBackend && s.SessionCapabilityAdvertisable() {
		tool = func(name string) string { return MCPToolPrefix + name }
		preamble = MCPNamingRuleMarker + `, so each one is named ` + MCPToolPrefix + `<tool> and not <tool>. Wherever these instructions or the task above name a Symphony tool without that prefix, call ` + MCPToolPrefix + ` followed by that name. Your own tool list decides availability: a Symphony tool that is not in it is unavailable for this run, whatever the instructions say.

`
	}
	if s.HostSidePublishPromised() {
		return preamble + HostSidePublishPromiseMarker + `

- Make and validate the change in this workspace, then create a local commit.
- Do not run gh, git push, or otherwise try to publish directly to GitHub.
- When the worktree is clean and committed, call ` + tool("github_publish_pr") + ` with why, what_changed, and on_call. It is bound to this issue, repository, and branch and will create or update the PR body from those fields and hand the issue to review.
- Call ` + tool("github_pr_context") + ` (no arguments) to read bounded check status, review state, and unresolved feedback for that same pull request.`
	}
	requirements := "configure github.owner, github.repository, github.base_branch, and a repository-scoped GitHub token"
	if s.GitHub.Enabled {
		requirements = "configure tracker.provider.handoff_state in addition to the existing GitHub settings"
	} else if s.Tracker.HandoffState != "" {
		requirements = "configure the fixed github owner, repository, base branch, and repository-scoped token"
	}
	return preamble + `Delivery mode: manual. Host-side PR publishing is unavailable for this run.

- Do not run gh, git push, or try to open a pull request directly.
- You may make and commit local changes, but leave the issue active after reporting the ready work.
- Report this actionable blocker: PR handoff is unavailable; ` + requirements + `.`
}

// RenderHandoffComment renders the repository-owned optional comment template.
// It is never populated from a Codex tool argument, so a handoff comment has
// the same reviewable policy source as the target state.
func (s Settings) RenderHandoffComment(issue domain.Issue) (string, error) {
	if s.Tracker.HandoffCommentTemplate == "" {
		return "", nil
	}
	t, err := template.New("handoff_comment").Option("missingkey=error").Parse(s.Tracker.HandoffCommentTemplate)
	if err != nil {
		return "", fmt.Errorf("handoff_comment_template: %w", err)
	}
	var out bytes.Buffer
	if err := t.Execute(&out, map[string]any{"issue": templateIssue(issue)}); err != nil {
		return "", fmt.Errorf("handoff_comment_template: %w", err)
	}
	return out.String(), nil
}

func templateIssue(issue any) any {
	i, ok := issue.(domain.Issue)
	if !ok {
		if p, pointer := issue.(*domain.Issue); pointer && p != nil {
			i, ok = *p, true
		}
	}
	if !ok {
		return issue
	}
	blockers := make([]map[string]any, 0, len(i.BlockedBy))
	for _, b := range i.BlockedBy {
		blockers = append(blockers, map[string]any{"id": b.ID, "identifier": b.Identifier, "state": b.State, "dispatchable": b.Dispatchable})
	}
	return map[string]any{
		"id": i.ID, "identifier": i.Identifier, "title": i.Title, "description": i.Description,
		"state": i.State, "branch_name": i.BranchName, "url": i.URL, "assignee_id": i.AssigneeID,
		"native_ref": i.NativeRef, "priority": i.Priority, "labels": i.Labels, "blocked_by": blockers,
		"dispatchable": i.Dispatchable, "created_at": i.CreatedAt, "updated_at": i.UpdatedAt,
	}
}
