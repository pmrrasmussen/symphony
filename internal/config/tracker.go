package config

import (
	"errors"
	"fmt"
	"strings"
	"text/template"
)

const (
	legacyProjectSlugWarning        = "tracker.provider.project_slug is deprecated; migrate to project_slug_id"
	legacyChildIssueCreationWarning = "tracker.provider.child_issue_creation is deprecated; migrate to followup_issue_creation"
)

// Norm lowercases and trims a tracker-supplied name -- state, label, or
// similar -- so callers comparing one against a configured value do not have
// to agree on case or surrounding whitespace themselves.
func Norm(value string) string { return strings.ToLower(strings.TrimSpace(value)) }

// decodeTracker validates the tracker: block, resolving tracker.provider's
// credential references and deriving the handoff, host-transition, and
// follow-up-issue policies from it. It returns the unresolved provider object
// alongside the decoded Tracker because hostSecretEnvNames must inspect
// credential *references* (a $VAR name or file path) rather than the
// resolved secret resolveProvider writes back into Tracker.Provider.
func decodeTracker(raw map[string]any, base string, sources *sourceSnapshot) (Tracker, map[string]any, []string, error) {
	tr, err := object(raw, "tracker")
	if err != nil {
		return Tracker{}, nil, nil, err
	}
	provider, err := object(tr, "provider")
	if err != nil {
		return Tracker{}, nil, nil, err
	}
	trackerKind, err := stringValue(tr, "kind")
	if err != nil {
		return Tracker{}, nil, nil, err
	}
	requiredLabels, err := requiredLabelList(tr, "required_labels")
	if err != nil {
		return Tracker{}, nil, nil, err
	}
	activeStates, err := stringList(tr, "active_states")
	if err != nil {
		return Tracker{}, nil, nil, err
	}
	terminalStates, err := stringList(tr, "terminal_states")
	if err != nil {
		return Tracker{}, nil, nil, err
	}
	resolvedProvider, warnings, err := resolveProvider(provider, base, sources)
	if err != nil {
		return Tracker{}, nil, nil, err
	}
	handoffState, handoffCommentTemplate, err := handoffPolicy(resolvedProvider, activeStates, terminalStates)
	if err != nil {
		return Tracker{}, nil, nil, err
	}
	hostTransitions, err := hostTransitionPolicy(resolvedProvider, activeStates, terminalStates)
	if err != nil {
		return Tracker{}, nil, nil, err
	}
	followupIssueCreation, err := followupIssueCreationPolicy(resolvedProvider, activeStates)
	if err != nil {
		return Tracker{}, nil, nil, err
	}
	tracker := Tracker{
		Kind:           strings.TrimSpace(trackerKind),
		Provider:       resolvedProvider,
		RequiredLabels: stringsLower(requiredLabels),
		// Linear's state-name filter is case-sensitive. Preserve the
		// repository-owned spelling here and normalize only at comparison
		// sites inside the coordinator and adapters.
		ActiveStates:           activeStates,
		TerminalStates:         terminalStates,
		HandoffState:           handoffState,
		HandoffCommentTemplate: handoffCommentTemplate,
		HostTransitions:        hostTransitions,
		FollowupIssueCreation:  followupIssueCreation,
	}
	return tracker, provider, warnings, nil
}

// handoffPolicy keeps the Linear-specific values in tracker.provider while
// exposing an immutable, typed policy to the Codex/Linear handoff adapter.
// Handoff is deliberately opt-in: a state is required before the client tool
// can be used, and it may never be one of the states the scheduler dispatches.
func handoffPolicy(provider map[string]any, activeStates, terminalStates []string) (string, string, error) {
	stateValue, hasState := provider["handoff_state"]
	commentValue, hasComment := provider["handoff_comment_template"]
	if !hasState && !hasComment {
		return "", "", nil
	}
	if !hasState {
		return "", "", errors.New("invalid configuration: tracker.provider.handoff_comment_template requires handoff_state")
	}
	state, ok := stateValue.(string)
	if !ok || strings.TrimSpace(state) == "" {
		return "", "", errors.New("invalid configuration: tracker.provider.handoff_state must be a non-empty string")
	}
	state = strings.TrimSpace(state)
	for _, active := range activeStates {
		if strings.EqualFold(strings.TrimSpace(active), state) {
			return "", "", errors.New("invalid configuration: tracker.provider.handoff_state must not be an active state")
		}
	}
	for _, terminal := range terminalStates {
		if strings.EqualFold(strings.TrimSpace(terminal), state) {
			return "", "", errors.New("invalid configuration: tracker.provider.handoff_state must not be a terminal state")
		}
	}
	if !hasComment {
		return state, "", nil
	}
	comment, ok := commentValue.(string)
	if !ok || strings.TrimSpace(comment) == "" {
		return "", "", errors.New("invalid configuration: tracker.provider.handoff_comment_template must be a non-empty string")
	}
	if _, err := template.New("handoff_comment").Option("missingkey=error").Parse(comment); err != nil {
		return "", "", fmt.Errorf("invalid configuration: tracker.provider.handoff_comment_template: %w", err)
	}
	return state, comment, nil
}

// hostTransitionPolicy parses the single repository-owned, host-applied
// transition policy under tracker.provider.transitions. Symphony applies every
// edge itself with the host credential; none is exposed to a Codex session, so
// the agent has no issue-state transition capability. The two edge sets are
// parsed and validated separately and never flattened into one map: the
// canonical Merging state is both a dispatchable active state and the
// land-fallback source, so a flat source->target map consumed at dispatch
// would wrongly move a freshly dispatched Merging landing agent's issue to In
// Review.
//
//   - transitions.start: dispatch-time edges the coordinator applies when it
//     launches an issue (Todo -> In Progress). Both endpoints of every edge
//     must be active, non-terminal states, since the coordinator only
//     dispatches active issues and the issue must remain eligible for
//     reconciliation after the move.
//   - transitions.refuse_landing: the edges RefuseLanding applies after a
//     github_land_pr hard gate (Merging -> In Review), keyed by
//     github.merge_state. Never applied at dispatch; terminal and same-state
//     edges are rejected.
//
// Source keys in both maps are lowercased so callers can compare them against a
// normalized issue state directly.
func hostTransitionPolicy(provider map[string]any, activeStates, terminalStates []string) (HostTransitions, error) {
	value, exists := provider["transitions"]
	if !exists {
		return HostTransitions{}, nil
	}
	object, ok := value.(map[string]any)
	if !ok || len(object) == 0 {
		return HostTransitions{}, errors.New("invalid configuration: tracker.provider.transitions must be a non-empty object")
	}
	for key := range object {
		if key != "start" && key != "refuse_landing" {
			return HostTransitions{}, fmt.Errorf("invalid configuration: tracker.provider.transitions has an unsupported key %q", key)
		}
	}
	start, err := startTransitionEdges(object["start"])
	if err != nil {
		return HostTransitions{}, err
	}
	refuseLanding, err := refuseLandingEdges(object["refuse_landing"], terminalStates)
	if err != nil {
		return HostTransitions{}, err
	}
	// Every declared start endpoint must be an active, non-terminal state.
	for source, target := range start {
		if !stateInList(source, activeStates) || !stateInList(target, activeStates) {
			return HostTransitions{}, errors.New("invalid configuration: tracker.provider.transitions.start source and target must both be active states")
		}
		if stateInList(source, terminalStates) || stateInList(target, terminalStates) {
			return HostTransitions{}, errors.New("invalid configuration: tracker.provider.transitions.start must not contain terminal states")
		}
	}
	return HostTransitions{Start: start, RefuseLanding: refuseLanding}, nil
}

// startTransitionEdges parses transitions.start into a lowercased source->target
// map. Terminal/active membership is validated by the caller, which has the
// state lists. A present but empty or malformed value is rejected.
func startTransitionEdges(value any) (map[string]string, error) {
	if value == nil {
		return nil, nil
	}
	return transitionEdges(value, "tracker.provider.transitions.start")
}

// refuseLandingEdges parses transitions.refuse_landing into a lowercased
// source->target map and rejects any terminal endpoint. It is the land-fallback
// edge (Merging -> In Review) applied only by RefuseLanding.
func refuseLandingEdges(value any, terminalStates []string) (map[string]string, error) {
	if value == nil {
		return nil, nil
	}
	result, err := transitionEdges(value, "tracker.provider.transitions.refuse_landing")
	if err != nil {
		return nil, err
	}
	for source, target := range result {
		if stateInList(source, terminalStates) || stateInList(target, terminalStates) {
			return nil, errors.New("invalid configuration: tracker.provider.transitions.refuse_landing must not contain terminal states")
		}
	}
	return result, nil
}

// transitionEdges is the shared parser for one transition edge map. It rejects
// a non-object, an empty object, non-string endpoints, duplicate source states,
// and same-state edges, and returns lowercased source keys.
func transitionEdges(value any, field string) (map[string]string, error) {
	edges, ok := value.(map[string]any)
	if !ok || len(edges) == 0 {
		return nil, fmt.Errorf("invalid configuration: %s must be a non-empty object", field)
	}
	result := make(map[string]string, len(edges))
	for sourceValue, targetValue := range edges {
		source := strings.TrimSpace(sourceValue)
		target, ok := targetValue.(string)
		target = strings.TrimSpace(target)
		if source == "" || !ok || target == "" {
			return nil, fmt.Errorf("invalid configuration: %s entries must map non-empty state names to non-empty state names", field)
		}
		key := strings.ToLower(source)
		if _, duplicate := result[key]; duplicate {
			return nil, fmt.Errorf("invalid configuration: %s has duplicate source states", field)
		}
		if strings.EqualFold(source, target) {
			return nil, fmt.Errorf("invalid configuration: %s must not contain same-state edges", field)
		}
		result[key] = target
	}
	return result, nil
}

// followupIssueCreationPolicy is deliberately a single boolean. The tool's
// project and team are derived from the active issue, and its initial state is
// fixed to Backlog. Enabling it while Backlog is dispatchable would defeat the
// human promotion gate, so that configuration fails closed.
func followupIssueCreationPolicy(provider map[string]any, activeStates []string) (bool, error) {
	value, exists := provider["followup_issue_creation"]
	if !exists {
		return false, nil
	}
	enabled, ok := value.(bool)
	if !ok {
		return false, errors.New("invalid configuration: tracker.provider.followup_issue_creation must be a boolean")
	}
	if enabled {
		for _, state := range activeStates {
			if strings.EqualFold(strings.TrimSpace(state), "Backlog") {
				return false, errors.New("invalid configuration: tracker.provider.followup_issue_creation requires Backlog to be non-dispatchable")
			}
		}
	}
	return enabled, nil
}

func stateInList(state string, states []string) bool {
	for _, candidate := range states {
		if strings.EqualFold(strings.TrimSpace(state), strings.TrimSpace(candidate)) {
			return true
		}
	}
	return false
}

func resolveProvider(m map[string]any, base string, sources *sourceSnapshot) (map[string]any, []string, error) {
	out := make(map[string]any, len(m)+1)
	for key, value := range m {
		out[key] = value
	}
	warnings, err := normalizeProjectSlug(out)
	if err != nil {
		return nil, nil, err
	}
	followupWarnings, err := normalizeFollowupIssueCreation(out)
	if err != nil {
		return nil, nil, err
	}
	warnings = append(warnings, followupWarnings...)
	apiKey, hasAPIKey := out["api_key"]
	if hasAPIKey {
		if _, ok := apiKey.(string); !ok {
			return nil, nil, errors.New("invalid configuration: tracker.provider.api_key must be a string")
		}
	}
	v, exists := out["api_key_file"]
	if exists {
		file, ok := v.(string)
		if !ok {
			return nil, nil, errors.New("invalid configuration: tracker.provider.api_key_file must be a string")
		}
		file, err := sources.expand(file, "tracker.provider.api_key_file")
		if err != nil {
			return nil, nil, err
		}
		if strings.TrimSpace(file) == "" {
			return nil, nil, errors.New("invalid linear api_key_file: empty path")
		}
		b, err := sources.readFile(normalizePath(file, base))
		if err != nil {
			return nil, nil, errors.New("invalid linear api_key_file: could not read configured secret file")
		}
		if value := strings.TrimSpace(string(b)); value == "" {
			return nil, nil, errors.New("invalid linear api_key_file: empty secret")
		} else {
			// The explicitly configured secret file takes precedence over an
			// inline reference, including an unset inline $VAR reference.
			out["api_key"] = value
		}
		return out, warnings, nil
	}
	if !hasAPIKey {
		return out, warnings, nil
	}
	resolved, err := sources.expand(apiKey.(string), "tracker.provider.api_key")
	if err != nil {
		return nil, nil, err
	}
	resolved = strings.TrimSpace(resolved)
	if resolved == "" {
		return nil, nil, errors.New("invalid linear api_key: resolved secret is empty")
	}
	out["api_key"] = resolved
	return out, warnings, nil
}

func normalizeProjectSlug(provider map[string]any) ([]string, error) {
	legacy, hasLegacy := provider["project_slug"]
	_, hasCanonical := provider["project_slug_id"]
	if hasLegacy && hasCanonical {
		return nil, errors.New("invalid configuration: tracker.provider.project_slug_id and deprecated project_slug must not both be set")
	}
	if !hasLegacy {
		return nil, nil
	}
	provider["project_slug_id"] = legacy
	delete(provider, "project_slug")
	return []string{legacyProjectSlugWarning}, nil
}

func normalizeFollowupIssueCreation(provider map[string]any) ([]string, error) {
	legacy, hasLegacy := provider["child_issue_creation"]
	_, hasCanonical := provider["followup_issue_creation"]
	if hasLegacy && hasCanonical {
		return nil, errors.New("invalid configuration: tracker.provider.followup_issue_creation and deprecated child_issue_creation must not both be set")
	}
	if !hasLegacy {
		return nil, nil
	}
	provider["followup_issue_creation"] = legacy
	delete(provider, "child_issue_creation")
	return []string{legacyChildIssueCreationWarning}, nil
}

// requiredLabelList deliberately preserves blank values. A blank required
// label is a fail-closed routing policy: no Linear issue can have it, so no
// issue may be dispatched until the workflow is corrected.
func requiredLabelList(m map[string]any, key string) ([]string, error) {
	v, exists := m[key]
	if !exists {
		return nil, nil
	}
	values, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("invalid configuration: %s must be a list of strings", key)
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		s, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("invalid configuration: %s must be a list of strings", key)
		}
		out = append(out, strings.TrimSpace(s))
	}
	return out, nil
}
