package capability

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/domain"
	githubhost "github.com/pmrrasmussen/symphony/internal/github"
	"github.com/pmrrasmussen/symphony/internal/linear"
)

// Advertisement, ordering, and dispatchability are decided entirely by the
// bindings, so these tests bind zero-value provider sessions: no test here
// invokes a capability, and invocation behavior stays covered by the provider
// packages and the Codex end-to-end tests.
func bindings(followup bool, withGitHub bool, issueState, mergeState string) Bindings {
	b := Bindings{
		Settings: config.Settings{},
		Issue:    domain.Issue{Identifier: "PMR-1", State: issueState},
	}
	b.Settings.Tracker.FollowupIssueCreation = followup
	b.Settings.GitHub.MergeState = mergeState
	if followup {
		b.Handoff = &linear.HandoffSession{}
	}
	if withGitHub {
		b.GitHub = &githubhost.Session{}
	}
	return b
}

func names(definitions []Definition) []string {
	out := make([]string, 0, len(definitions))
	for _, definition := range definitions {
		out = append(out, definition.Name)
	}
	return out
}

// TestAdvertisedCapabilitiesAndOrderPerConfiguration pins the advertised set for
// every configuration permutation. Order is part of the contract: it is the
// order an agent is told about capabilities in.
func TestAdvertisedCapabilitiesAndOrderPerConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name     string
		bindings Bindings
		want     []string
	}{
		{"nothing configured", bindings(false, false, "Todo", ""), nil},
		{"followup only", bindings(true, false, "Todo", ""), []string{NameCreateFollowupIssue}},
		{"github without merge state configured", bindings(false, true, "Merging", ""),
			[]string{NameGitHubPublishPR, NameGitHubPRContext}},
		{"github with issue outside merge state", bindings(false, true, "In Progress", "Merging"),
			[]string{NameGitHubPublishPR, NameGitHubPRContext}},
		{"github with issue in merge state", bindings(false, true, "Merging", "Merging"),
			[]string{NameGitHubPublishPR, NameGitHubPRContext, NameGitHubLandPR}},
		{"everything enabled", bindings(true, true, "merging", " Merging "),
			[]string{NameCreateFollowupIssue, NameGitHubPublishPR, NameGitHubPRContext, NameGitHubLandPR}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := names(Build(tc.bindings).Definitions())
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("advertised %v, want %v", got, tc.want)
			}
		})
	}
}

// TestFollowupIssueCreationIsAdvertisedOnlyWhenEnabledButStaysDispatchable
// guards the invariant that advertisement is a coarse filter and never the
// authority: a capability the session is bound to stays dispatchable so the
// provider can apply its own check and refuse.
func TestFollowupIssueCreationIsAdvertisedOnlyWhenEnabledButStaysDispatchable(t *testing.T) {
	b := bindings(false, false, "Todo", "")
	b.Handoff = &linear.HandoffSession{}
	registry := Build(b)
	if definitions := registry.Definitions(); len(definitions) != 0 {
		t.Fatalf("advertised %v with follow-up creation disabled", names(definitions))
	}
	if _, ok := registry.Lookup(NameCreateFollowupIssue); !ok {
		t.Fatal("follow-up creation must stay dispatchable so the provider can refuse it")
	}
}

func TestLandingStaysDispatchableOutsideTheMergeState(t *testing.T) {
	registry := Build(bindings(false, true, "In Progress", "Merging"))
	if _, ok := registry.Lookup(NameGitHubLandPR); !ok {
		t.Fatal("landing must stay dispatchable so Land can re-validate Linear state and refuse")
	}
}

// TestNoTrackerTransitionCapabilityIsEverRegistered keeps PMR-59's removal of
// agent-side Linear write access from regressing through this registry.
func TestNoTrackerTransitionCapabilityIsEverRegistered(t *testing.T) {
	registry := Build(bindings(true, true, "Merging", "Merging"))
	for _, name := range []string{"linear_graphql", "create_child_issue", "linear_transition", "linear_update_issue"} {
		if _, ok := registry.Lookup(name); ok {
			t.Fatalf("capability %q must not exist", name)
		}
	}
	if got := len(registry.Definitions()); got != 4 {
		t.Fatalf("advertised %d capabilities, want exactly the 4 known ones", got)
	}
}

func TestUnknownCapabilityIsNotResolved(t *testing.T) {
	registry := Build(bindings(true, true, "Merging", "Merging"))
	for _, name := range []string{"", "GITHUB_LAND_PR", "github_land_pr ", "shell"} {
		if _, ok := registry.Lookup(name); ok {
			t.Fatalf("unknown capability %q resolved", name)
		}
	}
}

// TestZeroArgumentCapabilitiesRejectAnyInputBeforeInvocation proves the shared
// no-input decoder refuses before any provider work happens: Prepare returns a
// refusal and never an invocation, so nothing is reported as a started call.
func TestZeroArgumentCapabilitiesRejectAnyInputBeforeInvocation(t *testing.T) {
	registry := Build(bindings(false, true, "Merging", "Merging"))
	for _, name := range []string{NameGitHubPRContext, NameGitHubLandPR} {
		bound, ok := registry.Lookup(name)
		if !ok {
			t.Fatalf("%s not registered", name)
		}
		for _, arguments := range []string{`{"number":1}`, `{"reason":"x"}`, `[]`, `"text"`, `not json`} {
			invoke, failure := bound.Prepare(json.RawMessage(arguments))
			if failure == nil || invoke != nil {
				t.Fatalf("%s accepted arguments %s", name, arguments)
			}
			if failure.Message != "Unsupported client-side tool." {
				t.Fatalf("%s refusal message = %q", name, failure.Message)
			}
		}
		if invoke, failure := bound.Prepare(json.RawMessage(`{}`)); failure != nil || invoke == nil {
			t.Fatalf("%s rejected an empty object: %+v", name, failure)
		}
		// Absent arguments are not the same as an empty object and stay refused:
		// a call that omits them is not a well-formed call.
		if invoke, failure := bound.Prepare(nil); failure == nil || invoke != nil {
			t.Fatalf("%s accepted absent arguments", name)
		}
	}
}

func TestPublishRejectsMalformedArgumentsBeforeInvocation(t *testing.T) {
	registry := Build(bindings(false, true, "Todo", "Merging"))
	bound, ok := registry.Lookup(NameGitHubPublishPR)
	if !ok {
		t.Fatal("publish not registered")
	}
	for _, arguments := range []string{`{}`, `{"why":"a"}`, `{"why":"a","what_changed":"b","on_call":"c","extra":1}`, `{"why":"","what_changed":"b","on_call":"c"}`} {
		invoke, failure := bound.Prepare(json.RawMessage(arguments))
		if failure == nil || invoke != nil {
			t.Fatalf("publish accepted arguments %s", arguments)
		}
		if failure.Message != "GitHub pull request publication arguments were rejected." {
			t.Fatalf("publish refusal message = %q", failure.Message)
		}
	}
}

// TestAdvertisedBoundsMatchTheProviderThatEnforcesThem replaces the comments
// that previously asked two files to be kept in sync by hand.
func TestAdvertisedBoundsMatchTheProviderThatEnforcesThem(t *testing.T) {
	publish := publishCapability{}.Definition()
	for _, tc := range []struct {
		field string
		want  int
	}{
		{"why", githubhost.MaxPublishWhyBytes},
		{"what_changed", githubhost.MaxPublishWhatChangedBytes},
		{"on_call", githubhost.MaxPublishOnCallBytes},
	} {
		if got, ok := schemaBound(publish, tc.field); !ok || got != tc.want {
			t.Fatalf("publish %s maxLength = %d (ok=%v), want %d", tc.field, got, ok, tc.want)
		}
	}

	followup := followupIssueCapability{}.Definition()
	if got, ok := schemaBound(followup, "title"); !ok || got != linear.MaxFollowupIssueTitleRunes {
		t.Fatalf("follow-up title maxLength = %d (ok=%v), want %d", got, ok, linear.MaxFollowupIssueTitleRunes)
	}
	// The provider bounds the rendered body rather than the two fields it is
	// built from, so the advertised per-field bounds must not be able to sum
	// past it -- otherwise a schema-valid call is refused after acceptance.
	description, okDescription := schemaBound(followup, "description")
	acceptance, okAcceptance := schemaBound(followup, "acceptance_criteria")
	if !okDescription || !okAcceptance {
		t.Fatalf("follow-up body bounds unreadable: description ok=%v acceptance ok=%v", okDescription, okAcceptance)
	}
	const separatorBytes = len("\n\n## Acceptance criteria\n\n")
	if total := description + acceptance + separatorBytes; total > linear.MaxFollowupIssueBodyBytes {
		t.Fatalf("advertised body bounds sum to %d, past the provider bound %d", total, linear.MaxFollowupIssueBodyBytes)
	}
	// The two body bounds are advertised values in their own right, not just
	// summands, so pin them: widening one silently narrows what the other may use.
	if description != 16000 || acceptance != 4000 {
		t.Fatalf("follow-up body bounds = %d/%d, want 16000/4000", description, acceptance)
	}
	// Every bounded string field is also required to be non-empty at the schema
	// level, so an empty value is refused before it reaches a provider.
	for _, bounded := range []struct {
		definition Definition
		fields     []string
	}{
		{publish, []string{"why", "what_changed"}},
		{followup, []string{"title", "description", "acceptance_criteria"}},
	} {
		definition := bounded.definition
		fields := bounded.fields
		properties := definition.InputSchema["properties"].(map[string]any)
		for _, field := range fields {
			if property := properties[field].(map[string]any); property["minLength"] != 1 {
				t.Fatalf("%s %s minLength = %#v, want 1", definition.Name, field, property["minLength"])
			}
		}
	}
}

// schemaBound reads a maxLength out of an advertised schema.
func schemaBound(d Definition, field string) (int, bool) {
	properties, ok := d.InputSchema["properties"].(map[string]any)
	if !ok {
		return 0, false
	}
	property, ok := properties[field].(map[string]any)
	if !ok {
		return 0, false
	}
	bound, ok := property["maxLength"].(int)
	return bound, ok
}

// The remaining tests moved here with the definitions they assert on: the
// schemas are what an agent is offered, so they belong beside the registry.

func TestPublishSchemaHasOnlyStructuredHandoffFieldsNoScopeOrCredentialInput(t *testing.T) {
	definition := publishCapability{}.Definition()
	schema := definition.InputSchema
	if schema["type"] != "object" || schema["additionalProperties"] != false {
		t.Fatalf("schema=%#v", schema)
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties=%#v", schema["properties"])
	}
	allowed := map[string]bool{"why": true, "what_changed": true, "on_call": true}
	for name := range properties {
		if !allowed[name] {
			t.Fatalf("publish unexpectedly accepts field %q: %#v", name, schema)
		}
	}
	required, ok := schema["required"].([]string)
	if !ok || len(required) != 3 {
		t.Fatalf("required=%#v", schema["required"])
	}
	for _, name := range required {
		if !allowed[name] {
			t.Fatalf("required field %q is not a structured handoff field", name)
		}
	}
	// The schema (not the free-text description) must never expose a
	// scope-selection or credential field.
	encoded, err := json.Marshal(schema)
	if err != nil || strings.Contains(string(encoded), "token") || strings.Contains(string(encoded), "owner") || strings.Contains(string(encoded), "repository") || strings.Contains(string(encoded), "branch") || strings.Contains(string(encoded), "pull_number") {
		t.Fatalf("schema exposed host scope: %s err=%v", encoded, err)
	}
}

func TestContextCapabilityHasNoInput(t *testing.T) {
	definition := contextCapability{}.Definition()
	schema := definition.InputSchema
	if schema["type"] != "object" || schema["additionalProperties"] != false {
		t.Fatalf("schema=%#v", schema)
	}
	if _, hasProperties := schema["properties"]; hasProperties {
		t.Fatalf("context capability unexpectedly accepts caller-controlled input: %#v", schema)
	}
	encoded, err := json.Marshal(definition)
	if err != nil || strings.Contains(string(encoded), "token") || strings.Contains(string(encoded), "\"owner\"") || strings.Contains(string(encoded), "\"repository\"") {
		t.Fatalf("definition exposed host scope: %s err=%v", encoded, err)
	}
}

func TestLandCapabilityHasNoInput(t *testing.T) {
	definition := landCapability{}.Definition()
	schema := definition.InputSchema
	if schema["type"] != "object" || schema["additionalProperties"] != false {
		t.Fatalf("schema=%#v", schema)
	}
	if _, hasProperties := schema["properties"]; hasProperties {
		t.Fatalf("land capability unexpectedly accepts caller-controlled input: %#v", schema)
	}
	encoded, err := json.Marshal(definition)
	if err != nil || strings.Contains(string(encoded), "token") || strings.Contains(string(encoded), "\"owner\"") || strings.Contains(string(encoded), "\"repository\"") || strings.Contains(string(encoded), "\"method\"") {
		t.Fatalf("definition exposed host scope: %s err=%v", encoded, err)
	}
}

func TestFollowupIssueSchemaHasNoCallerControlledScopeFields(t *testing.T) {
	definition := followupIssueCapability{}.Definition()
	schema := definition.InputSchema
	if schema["type"] != "object" || schema["additionalProperties"] != false {
		t.Fatalf("schema=%#v", schema)
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties=%#v", schema["properties"])
	}
	allowed := map[string]bool{"title": true, "description": true, "acceptance_criteria": true, "relationship": true}
	for name := range properties {
		if !allowed[name] {
			t.Fatalf("follow-up creation unexpectedly accepts field %q", name)
		}
	}
	for name := range allowed {
		if _, exists := properties[name]; !exists {
			t.Fatalf("follow-up creation is missing bounded field %q: %#v", name, properties)
		}
	}
	for _, forbidden := range []string{"issue", "issue_id", "project", "project_id", "team", "team_id", "state", "state_id", "endpoint", "credential", "token", "parent_id"} {
		if _, exists := properties[forbidden]; exists {
			t.Fatalf("follow-up creation exposed caller-controlled %q: %#v", forbidden, properties)
		}
	}
	required, ok := schema["required"].([]string)
	if !ok || len(required) != 3 || required[0] != "title" || required[1] != "description" || required[2] != "acceptance_criteria" {
		t.Fatalf("required=%#v", schema["required"])
	}
	relationship, ok := properties["relationship"].(map[string]any)
	if !ok {
		t.Fatalf("relationship=%#v", properties["relationship"])
	}
	values, ok := relationship["enum"].([]string)
	if !ok || len(values) != 2 || values[0] != "related" || values[1] != "blocked_by_current" {
		t.Fatalf("relationship enum=%#v", relationship["enum"])
	}
	// No parent, project, team, state, or assignee selection may be offered.
	encoded, err := json.Marshal(schema)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"parent", "project", "team", "state", "assignee", "priority", "label"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("follow-up schema exposed %q: %s", forbidden, encoded)
		}
	}
}

func TestEveryDefinitionIsNamedByARegistryConstant(t *testing.T) {
	known := map[string]bool{
		NameCreateFollowupIssue: true, NameGitHubPublishPR: true,
		NameGitHubPRContext: true, NameGitHubLandPR: true,
	}
	for _, definition := range Build(bindings(true, true, "Merging", "Merging")).Definitions() {
		if !known[definition.Name] {
			t.Fatalf("definition %q is not one of the registry-owned names", definition.Name)
		}
		if strings.TrimSpace(definition.Description) == "" {
			t.Fatalf("%s has no description", definition.Name)
		}
	}
}

// TestLifecycleReportingPerCapability pins which capabilities are reported as
// dynamicToolCall item records. This used to be structural -- the follow-up
// branch simply contained no emission -- and is now one boolean per capability,
// so it needs an assertion. A wrong value here changes what the coordinator
// tracks as an outstanding operation, and therefore heartbeat and stall records.
func TestLifecycleReportingPerCapability(t *testing.T) {
	registry := Build(bindings(true, true, "Merging", "Merging"))
	for name, want := range map[string]bool{
		NameCreateFollowupIssue: false,
		NameGitHubPublishPR:     true,
		NameGitHubPRContext:     true,
		NameGitHubLandPR:        true,
	} {
		bound, ok := registry.Lookup(name)
		if !ok {
			t.Fatalf("%s not registered", name)
		}
		if got := bound.Lifecycle(); got != want {
			t.Fatalf("%s Lifecycle() = %v, want %v", name, got, want)
		}
	}
}

// TestLandingOutcomeCarriesAReasonOnlyWhileWaiting keeps the reason channel
// closed for a resolved landing. Before the registry existed the resolved event
// could not carry a message at all; now Reason is a generic field, so the
// guarantee needs an assertion.
func TestLandingOutcomeCarriesAReasonOnlyWhileWaiting(t *testing.T) {
	waiting, reason := landingOutcome(githubhost.LandResult{Status: githubhost.LandWaiting, Reason: "required checks are pending"})
	if waiting != domain.EventLandingWaiting || reason != "required checks are pending" {
		t.Fatalf("waiting outcome = %q reason = %q", waiting, reason)
	}
	resolved, reason := landingOutcome(githubhost.LandResult{Status: githubhost.LandMerged, Reason: "merged by an earlier attempt"})
	if resolved != domain.EventLandingResolved || reason != "" {
		t.Fatalf("resolved outcome = %q reason = %q, want no reason", resolved, reason)
	}
	if kind, reason := landingOutcome(githubhost.LandResult{}); kind != "" || reason != "" {
		t.Fatalf("unknown status produced outcome %q reason %q", kind, reason)
	}
}

func TestNothingIsRegisteredWithoutAHandoffSession(t *testing.T) {
	// Follow-up creation is bound to a handoff session; enabling the setting
	// without one must register nothing rather than a capability that would
	// dereference a missing session when invoked.
	b := bindings(true, false, "Todo", "")
	b.Handoff = nil
	registry := Build(b)
	if _, ok := registry.Lookup(NameCreateFollowupIssue); ok {
		t.Fatal("follow-up creation registered without a handoff session")
	}
	if definitions := registry.Definitions(); len(definitions) != 0 {
		t.Fatalf("advertised %v without any bound provider session", names(definitions))
	}
}

func TestTurnEndedIsSafeWithoutAGitHubSession(t *testing.T) {
	// A nil registry and a registry with no GitHub session must both be no-ops,
	// because the adapter calls this on every path that ends a turn.
	var nilRegistry *Registry
	nilRegistry.TurnEnded(nil)
	Build(bindings(true, false, "Todo", "")).TurnEnded(nil)
}
