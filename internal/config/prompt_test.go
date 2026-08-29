package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pmrrasmussen/symphony/internal/domain"
)

func TestTemplateErrorsAreDeferredAndPromptContractIsLowercase(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "WORKFLOW.md")
	base := "---\ntracker: {kind: linear, active_states: [Todo], terminal_states: [Done]}\n---\n"
	if err := os.WriteFile(p, []byte(base+"{{.issue.identifier}} retry={{.attempt}}"), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := Load(p, "")
	if err != nil {
		t.Fatal(err)
	}
	first, err := w.Config.Render(domain.Issue{Identifier: "PMR-7"}, 0)
	if err != nil || first != "PMR-7 retry=<no value>" {
		t.Fatalf("first render=%q err=%v", first, err)
	}
	retry, err := w.Config.Render(domain.Issue{Identifier: "PMR-7"}, 1)
	if err != nil || retry != "PMR-7 retry=1" {
		t.Fatalf("retry render=%q err=%v", retry, err)
	}
	if err := os.WriteFile(p, []byte(base+"{{.issue.unknown}}"), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err = Load(p, "")
	if err != nil {
		t.Fatalf("template parse/render errors must not fail Load: %v", err)
	}
	if _, err := w.Config.Render(domain.Issue{}, 0); err == nil || !strings.Contains(err.Error(), "template_render_error") {
		t.Fatalf("unknown variable error=%v", err)
	}
	if err := os.WriteFile(p, []byte(base+"{{.Issue.Identifier}}"), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err = Load(p, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Config.Render(domain.Issue{Identifier: "PMR-7"}, 0); err == nil || !strings.Contains(err.Error(), "template_render_error") {
		t.Fatalf("legacy uppercase contract was accepted: %v", err)
	}
	if err := os.WriteFile(p, []byte(base+"{{.issue.identifier"), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err = Load(p, "")
	if err != nil {
		t.Fatalf("template parse errors must not fail Load: %v", err)
	}
	if _, err := w.Config.Render(domain.Issue{}, 0); err == nil || !strings.Contains(err.Error(), "template_parse_error") {
		t.Fatalf("invalid template error=%v", err)
	}
}

func TestEmptyPromptUsesFallback(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "WORKFLOW.md")
	if err := os.WriteFile(p, []byte("---\ntracker: {kind: linear, active_states: [Todo], terminal_states: [Done]}\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := Load(p, "")
	if err != nil {
		t.Fatal(err)
	}
	got, err := w.Config.Render(domain.Issue{Identifier: "PMR-7", Title: "Workflow", Description: "Contract"}, 0)
	if err != nil || got != "Work on PMR-7: Workflow\n\nContract" {
		t.Fatalf("fallback render=%q err=%v", got, err)
	}
}

// hostPublishGuidance is the exact host-side publish guidance every dispatch
// received before DeliveryInstructions took a backend. It is a byte-level golden
// rather than a set of Contains checks because it is the parity statement this
// change rests on: for the backend that was there first, the new parameter must
// change nothing whatsoever, and a Contains check would pass through an inserted
// preamble, a renamed tool, or a dropped bullet.
const hostPublishGuidance = `Delivery mode: host-side publish is available for this run.

- Make and validate the change in this workspace, then create a local commit.
- Do not run gh, git push, or otherwise try to publish directly to GitHub.
- When the worktree is clean and committed, call github_publish_pr with why, what_changed, and on_call. It is bound to this issue, repository, and branch and will create or update the PR body from those fields and hand the issue to review.
- Call github_pr_context (no arguments) to read bounded check status, review state, and unresolved feedback for that same pull request.`

// mcpNamingPreamble is the golden naming rule the Claude branch prepends. It is
// written out here rather than assembled from MCPToolPrefix so that the
// assertion is against the text a model actually reads: a preamble built from the
// same expression as the code under test would agree with any prefix at all,
// including none.
const mcpNamingPreamble = "Tool naming: Symphony's bounded tools reach you through a single MCP server, " +
	"so each one is named mcp__symphony__<tool> and not <tool>. Wherever these instructions or the task " +
	"above name a Symphony tool without that prefix, call mcp__symphony__ followed by that name. Your own " +
	"tool list decides availability: a Symphony tool that is not in it is unavailable for this run, " +
	"whatever the instructions say.\n\n"

// symphonyToolNames are the capability names a workflow prompt may use. They are
// duplicated from internal/capability because importing it here is a cycle; the
// list only has to stay a subset for these assertions to hold, and
// internal/capability owns the names themselves.
var symphonyToolNames = []string{"create_followup_issue", "github_publish_pr", "github_pr_context", "github_land_pr"}

func hostPublishSettings() Settings {
	return Settings{GitHub: GitHub{Enabled: true}, Tracker: Tracker{HandoffState: "In Review"}}
}

// landingSettings is hostPublishSettings plus a configured merge state, so the
// only thing separating the two modes in these tests is the issue state passed
// to DeliveryInstructions.
func landingSettings() Settings {
	s := hostPublishSettings()
	s.GitHub.MergeState = "Merging"
	return s
}

func TestDeliveryInstructionsReportExactAvailableMode(t *testing.T) {
	manual := (Settings{}).DeliveryInstructions(DefaultAgentBackend, "In Progress")
	if !strings.Contains(manual, "Delivery mode: manual") || !strings.Contains(manual, "github.owner") {
		t.Fatalf("manual instructions=%q", manual)
	}
	if host := hostPublishSettings().DeliveryInstructions(DefaultAgentBackend, "In Progress"); host != hostPublishGuidance {
		t.Fatalf("host instructions=%q, want the unchanged Codex golden %q", host, hostPublishGuidance)
	}
	// An unrecognized backend is not given MCP names. Bare names are what the
	// only two implemented transports need -- Codex serves them verbatim -- so a
	// backend this function has never heard of must not be told its tools are
	// renamed.
	if unknown := hostPublishSettings().DeliveryInstructions("docker", "In Progress"); unknown != hostPublishGuidance {
		t.Fatalf("unknown-backend instructions=%q, want the bare-name golden", unknown)
	}
}

// TestALandingDispatchIsToldToLandAndOnlyToLand covers the state half of the
// mode: the same settings render publish guidance for an implementation state
// and landing guidance for the configured merge state. The 2026-08-28 divergence
// this fixes was one landing run publishing and another merging from the same
// state, so the assertion that matters is that the landing text does not invite
// the publish call at all -- naming github_publish_pr to refuse it is fine, but
// the publish mode's "call github_publish_pr with why, what_changed, and
// on_call" instruction must be absent (PMR-169).
func TestALandingDispatchIsToldToLandAndOnlyToLand(t *testing.T) {
	s := landingSettings()
	for _, state := range []string{"Merging", "merging", "  Merging  "} {
		landing := s.DeliveryInstructions(DefaultAgentBackend, state)
		if !strings.HasPrefix(landing, LandingDeliveryMarker) {
			t.Fatalf("state %q was not given the landing delivery mode: %q", state, landing)
		}
		if strings.Contains(landing, HostSidePublishPromiseMarker) || strings.Contains(landing, "call github_publish_pr with why") {
			t.Fatalf("state %q was invited to publish: %q", state, landing)
		}
		if !strings.Contains(landing, "github_land_pr") {
			t.Fatalf("state %q was not told to land: %q", state, landing)
		}
	}
	for _, state := range []string{"In Progress", "Rework", "In Review", "", "Merging Soon"} {
		publish := s.DeliveryInstructions(DefaultAgentBackend, state)
		if publish != hostPublishGuidance {
			t.Fatalf("state %q got %q, want the unchanged publish golden", state, publish)
		}
	}
	// Landing is a mode of host-side delivery, not a mode of its own: a workflow
	// that cannot publish host-side cannot land either, and must still be told
	// which configuration is missing rather than to call a tool it is not served.
	unconfigured := Settings{GitHub: GitHub{Enabled: true, MergeState: "Merging"}}
	if manual := unconfigured.DeliveryInstructions(DefaultAgentBackend, "Merging"); !strings.Contains(manual, "Delivery mode: manual") {
		t.Fatalf("a merge-state dispatch with no handoff state was not told delivery is manual: %q", manual)
	}
}

// TestTheLandingModeRenamesItsToolsForClaudeToo repeats the naming contract for
// the branch the publish-mode test cannot reach. A landing prompt that names a
// bare tool is exactly the failure PMR-169 is about, one step earlier: the model
// is told to call something the CLI does not serve under that name.
func TestTheLandingModeRenamesItsToolsForClaudeToo(t *testing.T) {
	claude := landingSettings().DeliveryInstructions(ClaudeAgentBackend, "Merging")
	if !strings.HasPrefix(claude, mcpNamingPreamble) {
		t.Fatalf("claude landing instructions did not open with the naming rule: %q", claude)
	}
	want := landingSettings().DeliveryInstructions(DefaultAgentBackend, "Merging")
	for _, name := range symphonyToolNames {
		want = strings.ReplaceAll(want, name, MCPToolPrefix+name)
	}
	if body := strings.TrimPrefix(claude, mcpNamingPreamble); body != want {
		t.Fatalf("claude landing guidance=%q, want the Codex guidance with prefixed tool names %q", body, want)
	}
	for _, name := range symphonyToolNames {
		if strings.Count(claude, name) != strings.Count(claude, MCPToolPrefix+name) {
			t.Fatalf("tool %q appears unprefixed in the claude landing guidance: %q", name, claude)
		}
	}
}

// TestLandingDispatchIsOneTrimmedCaseInsensitiveStateMatch pins the predicate
// three packages read. It is asserted against fixed expectations rather than
// against DeliveryInstructions, which branches on it: comparing the two would
// agree for any definition at all.
func TestLandingDispatchIsOneTrimmedCaseInsensitiveStateMatch(t *testing.T) {
	for _, tc := range []struct {
		mergeState, issueState string
		want                   bool
	}{
		{"Merging", "Merging", true},
		{"Merging", "merging", true},
		{"  Merging  ", "Merging", true},
		{"Merging", "\tMerging\n", true},
		{"Merging", "In Progress", false},
		{"Merging", "Merging Soon", false},
		{"Merging", "", false},
		{"", "Merging", false},
		{"   ", "   ", false},
	} {
		if got := (GitHub{MergeState: tc.mergeState}).LandingDispatch(tc.issueState); got != tc.want {
			t.Fatalf("LandingDispatch(merge_state=%q, state=%q)=%v, want %v", tc.mergeState, tc.issueState, got, tc.want)
		}
	}
}

// TestClaudeGuidanceRenamesEveryToolItNames is the load-bearing assertion for the
// backend branch. WORKFLOW.md is repository-owned and names Symphony's tools
// bare, so the only thing that keeps one prompt correct on the MCP transport is
// that this function renders the prefixed names and says how the mapping works.
//
// It is asserted as "the Claude text is the Codex text, plus the preamble, with
// each named tool prefixed and nothing else different", which is exactly the
// contract and is falsifiable in both directions: drop the branch and the
// preamble is missing, keep the preamble but leave a name bare and the
// substitution no longer matches.
func TestClaudeGuidanceRenamesEveryToolItNames(t *testing.T) {
	claude := hostPublishSettings().DeliveryInstructions(ClaudeAgentBackend, "In Progress")
	if !strings.HasPrefix(claude, mcpNamingPreamble) {
		t.Fatalf("claude instructions did not open with the naming rule: %q", claude)
	}
	want := hostPublishGuidance
	for _, name := range symphonyToolNames {
		want = strings.ReplaceAll(want, name, MCPToolPrefix+name)
	}
	if body := strings.TrimPrefix(claude, mcpNamingPreamble); body != want {
		t.Fatalf("claude delivery guidance=%q, want the Codex guidance with prefixed tool names %q", body, want)
	}
	// No tool name may survive unprefixed anywhere in the rendered text: a bare
	// name is a name the CLI does not serve, and the model has no way to tell.
	for _, name := range symphonyToolNames {
		if strings.Count(claude, name) != strings.Count(claude, MCPToolPrefix+name) {
			t.Fatalf("tool %q appears unprefixed in the claude guidance: %q", name, claude)
		}
	}
}

// TestAClaudeRunThatCanAdvertiseNothingReadsExactlyLikeACodexRun keeps the
// preamble from becoming noise. A run with no reachable capability is told about
// no tool at all, so a naming rule for tools that do not exist would be an
// invitation to look for them.
func TestAClaudeRunThatCanAdvertiseNothingReadsExactlyLikeACodexRun(t *testing.T) {
	for name, s := range map[string]Settings{
		"nothing configured":                   {},
		"github enabled with no handoff state": {GitHub: GitHub{Enabled: true}},
		"handoff state with no github":         {Tracker: Tracker{HandoffState: "In Review"}},
	} {
		t.Run(name, func(t *testing.T) {
			codex := s.DeliveryInstructions(DefaultAgentBackend, "In Progress")
			claude := s.DeliveryInstructions(ClaudeAgentBackend, "In Progress")
			if claude != codex {
				t.Fatalf("claude=%q, want byte-identical to codex %q", claude, codex)
			}
			if strings.Contains(claude, MCPToolPrefix) {
				t.Fatalf("a run with nothing to advertise was given the MCP naming rule: %q", claude)
			}
		})
	}
}

// TestTheNamingRuleCoversACapabilityTheDeliveryModeNeverNames is why the
// preamble is a rule over the prefix rather than a list of the tools these
// bullets happen to mention. create_followup_issue is named only by WORKFLOW.md's
// own body, in a manual-delivery run that mentions no tool at all, and it is
// still renamed by the transport.
func TestTheNamingRuleCoversACapabilityTheDeliveryModeNeverNames(t *testing.T) {
	s := Settings{Tracker: Tracker{FollowupIssueCreation: true}}
	claude := s.DeliveryInstructions(ClaudeAgentBackend, "In Progress")
	if !strings.HasPrefix(claude, mcpNamingPreamble) {
		t.Fatalf("a follow-up-only claude run was given no naming rule: %q", claude)
	}
	if body := strings.TrimPrefix(claude, mcpNamingPreamble); body != s.DeliveryInstructions(DefaultAgentBackend, "In Progress") {
		t.Fatalf("delivery mode changed with the backend: %q", body)
	}
	if !strings.Contains(claude, "Delivery mode: manual") {
		t.Fatalf("a run with no publish capability was not told delivery is manual: %q", claude)
	}
}

// TestHostSidePublishPromisedIsTheConditionTheGuidanceBranchesOn pins the
// predicate internal/claude cross-checks its registry against to the branch it
// claims to mirror. The two live in different packages and are compared only at
// launch, so a predicate that drifted from the branch would leave the guard
// checking a condition nothing renders.
func TestHostSidePublishPromisedIsTheConditionTheGuidanceBranchesOn(t *testing.T) {
	for _, s := range []Settings{
		{},
		{GitHub: GitHub{Enabled: true}},
		{Tracker: Tracker{HandoffState: "In Review"}},
		{Tracker: Tracker{HandoffState: "In Review", FollowupIssueCreation: true}},
		hostPublishSettings(),
		{GitHub: GitHub{Enabled: true}, Tracker: Tracker{HandoffState: "   "}},
	} {
		for _, backend := range AgentBackends() {
			promised := strings.Contains(s.DeliveryInstructions(backend, "In Progress"), "Delivery mode: host-side publish")
			if promised != s.HostSidePublishPromised() {
				t.Fatalf("settings %+v backend %q: guidance promises publish=%v, HostSidePublishPromised=%v", s, backend, promised, s.HostSidePublishPromised())
			}
		}
	}
}

// TestAnAllWhitespaceHandoffStateIsNoHandoffStateAtAll asserts the trim against
// fixed expectations rather than against another predicate. The agreement test
// above cannot cover it: DeliveryInstructions branches on
// HostSidePublishPromised, so comparing the two agrees for any definition of the
// predicate, trimmed or not.
//
// The value is unreachable through Load, but this predicate is now consumed by
// internal/claude and internal/preflight, and every sibling that reads the same
// field trims it. Untrimmed, the promise is true while the handoff session and
// the GitHub session built from that field are both nil -- so the guidance
// promises a publish, no session serves it, and every claude launch refuses at
// session_start with retry and backoff.
func TestRenderHandoffCommentUsesOnlyRepositoryPolicy(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "WORKFLOW.md")
	content := "---\ntracker: {kind: linear, provider: {handoff_state: \"In Review\", handoff_comment_template: \"Handoff {{.issue.identifier}}: {{.issue.title}}\"}, active_states: [Todo], terminal_states: [Done]}\n---\nprompt"
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := Load(p, "")
	if err != nil {
		t.Fatal(err)
	}
	got, err := w.Config.RenderHandoffComment(domain.Issue{Identifier: "PMR-5", Title: "Handoff"})
	if err != nil || got != "Handoff PMR-5: Handoff" {
		t.Fatalf("comment=%q err=%v", got, err)
	}
}
