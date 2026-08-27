package config

import (
	"errors"
	"net/url"
	"strings"
	"time"
)

// githubBlock extracts the raw github: object, distinguishing an object that
// is present but malformed (objectValid false, forcing the whole integration
// disabled) from one that is simply absent (objectValid true, nil object).
func githubBlock(raw map[string]any) (map[string]any, bool) {
	github, objectValid := raw["github"].(map[string]any)
	if _, exists := raw["github"]; !exists {
		github, objectValid = nil, true
	}
	return github, objectValid
}

func decodeGitHub(raw map[string]any, objectValid bool, base string, sources *sourceSnapshot) GitHub {
	if raw == nil || !objectValid {
		return GitHub{}
	}
	read := func(key string) (string, bool) {
		value, exists := raw[key]
		text, ok := value.(string)
		return strings.TrimSpace(text), exists && ok
	}
	owner, ownerOK := read("owner")
	repository, repositoryOK := read("repository")
	baseBranch, baseOK := read("base_branch")
	if !baseOK || baseBranch == "" {
		baseBranch, baseOK = "main", true
	}
	endpoint, endpointOK := read("endpoint")
	if !endpointOK || endpoint == "" {
		endpoint, endpointOK = "https://api.github.com", true
	}
	pollMS, pollOK := raw["poll_interval_ms"].(int)
	if _, exists := raw["poll_interval_ms"]; !exists {
		pollMS, pollOK = 30_000, true
	}
	token, tokenOK := read("token")
	if file, exists := raw["token_file"]; exists {
		path, ok := file.(string)
		if !ok {
			return GitHub{}
		}
		expanded, err := sources.expand(path, "github.token_file")
		if err != nil || strings.TrimSpace(expanded) == "" {
			return GitHub{}
		}
		content, err := sources.readFile(normalizePath(expanded, base))
		if err != nil {
			return GitHub{}
		}
		token, tokenOK = strings.TrimSpace(string(content)), true
	} else if tokenOK && strings.HasPrefix(token, "$") {
		resolved, err := sources.expand(token, "github.token")
		if err != nil {
			return GitHub{}
		}
		token = strings.TrimSpace(resolved)
	} else if tokenOK {
		return GitHub{}
	}
	endpointURL, err := url.Parse(endpoint)
	endpointValid := err == nil && endpointURL.Host != "" && (endpointURL.Scheme == "https" || endpointURL.Scheme == "http" && isLocalConfigHost(endpointURL.Hostname()))
	validName := func(value string) bool {
		return value != "" && !strings.ContainsAny(value, "/\\\r\n\t ") && value != "." && value != ".."
	}
	enabled := ownerOK && repositoryOK && baseOK && endpointOK && pollOK && tokenOK && validName(owner) && validName(repository) && validName(baseBranch) && token != "" && pollMS > 0 && endpointValid
	if !enabled {
		return GitHub{}
	}
	return GitHub{Enabled: true, Owner: owner, Repository: repository, BaseBranch: baseBranch, Token: token, Endpoint: strings.TrimRight(endpoint, "/"), PollInterval: time.Duration(pollMS) * time.Millisecond}
}

func isLocalConfigHost(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// applyLandingPolicy folds the strictly-validated optional landing policy into
// an already-decoded GitHub. It is kept separate from githubLandingPolicy
// because the landing fields must be validated before GitHub.Enabled is known
// (githubLandingPolicy needs only the tracker state lists and handoff state),
// while the merge_state-requires-an-enabled-integration check the caller makes
// afterward needs the merged result.
func applyLandingPolicy(gh GitHub, landing githubLanding) GitHub {
	gh.MergeState = landing.mergeState
	gh.MergeMethod = landing.mergeMethod
	gh.RequiredChecks = landing.requiredChecks
	gh.UpdateStaleBranch = landing.updateStaleBranch
	gh.LandFixEnabled = landing.landFixEnabled
	gh.MaxLandAttempts = landing.maxLandAttempts
	gh.AllowConflictResolution = landing.allowConflictResolution
	return gh
}

// validMergeMethods is the bounded merge-method enum accepted by
// github.merge_method. It intentionally mirrors GitHub's own three merge
// strategies and nothing else.
var validMergeMethods = map[string]bool{"merge": true, "squash": true, "rebase": true}

// githubLanding is the strictly-validated optional landing policy. Every field
// is meaningful only when mergeState is non-empty.
type githubLanding struct {
	mergeState              string
	mergeMethod             string
	requiredChecks          []string
	updateStaleBranch       bool
	landFixEnabled          bool
	maxLandAttempts         int
	allowConflictResolution bool
}

// githubLandingPolicy parses and strictly validates the optional
// github.merge_state, github.merge_method, github.required_checks,
// github.update_stale_branch, github.land_fix_enabled,
// github.max_land_attempts, and github.allow_conflict_resolution fields.
// Unlike the rest of the github: block, any malformed or ambiguous value here
// is a hard configuration error (see the GitHub struct doc comment) rather
// than a silently-disabled optional feature.
func githubLandingPolicy(github map[string]any, activeStates, terminalStates []string, handoffState string) (githubLanding, error) {
	if github == nil {
		return githubLanding{}, nil
	}
	updateStaleBranch := false
	if value, exists := github["update_stale_branch"]; exists {
		enabled, ok := value.(bool)
		if !ok {
			return githubLanding{}, errors.New("invalid configuration: github.update_stale_branch must be a boolean")
		}
		updateStaleBranch = enabled
	}
	landFixEnabled := false
	if value, exists := github["land_fix_enabled"]; exists {
		enabled, ok := value.(bool)
		if !ok {
			return githubLanding{}, errors.New("invalid configuration: github.land_fix_enabled must be a boolean")
		}
		landFixEnabled = enabled
	}
	maxLandAttempts := 2
	if value, exists := github["max_land_attempts"]; exists {
		attempts, ok := value.(int)
		if !ok || attempts <= 0 {
			return githubLanding{}, errors.New("invalid configuration: github.max_land_attempts must be a positive integer")
		}
		maxLandAttempts = attempts
	}
	allowConflictResolution := false
	if value, exists := github["allow_conflict_resolution"]; exists {
		enabled, ok := value.(bool)
		if !ok {
			return githubLanding{}, errors.New("invalid configuration: github.allow_conflict_resolution must be a boolean")
		}
		allowConflictResolution = enabled
	}
	mergeMethod := "merge"
	if value, exists := github["merge_method"]; exists {
		method, ok := value.(string)
		method = strings.ToLower(strings.TrimSpace(method))
		if !ok || !validMergeMethods[method] {
			return githubLanding{}, errors.New("invalid configuration: github.merge_method must be one of merge, squash, rebase")
		}
		mergeMethod = method
	}
	requiredChecksValue, hasRequiredChecks := github["required_checks"]
	var requiredChecks []string
	if hasRequiredChecks {
		list, ok := requiredChecksValue.([]any)
		if !ok || len(list) == 0 {
			return githubLanding{}, errors.New("invalid configuration: github.required_checks must be a non-empty list of strings")
		}
		seen := make(map[string]struct{}, len(list))
		for _, item := range list {
			name, ok := item.(string)
			name = strings.TrimSpace(name)
			if !ok || name == "" {
				return githubLanding{}, errors.New("invalid configuration: github.required_checks entries must be non-empty strings")
			}
			key := strings.ToLower(name)
			if _, duplicate := seen[key]; duplicate {
				return githubLanding{}, errors.New("invalid configuration: github.required_checks must not contain duplicate entries")
			}
			seen[key] = struct{}{}
			requiredChecks = append(requiredChecks, name)
		}
	}
	stateValue, hasState := github["merge_state"]
	if !hasState {
		if hasRequiredChecks {
			return githubLanding{}, errors.New("invalid configuration: github.required_checks requires github.merge_state")
		}
		if _, hasMethod := github["merge_method"]; hasMethod {
			return githubLanding{}, errors.New("invalid configuration: github.merge_method requires github.merge_state")
		}
		if _, hasUpdate := github["update_stale_branch"]; hasUpdate {
			return githubLanding{}, errors.New("invalid configuration: github.update_stale_branch requires github.merge_state")
		}
		if _, has := github["land_fix_enabled"]; has {
			return githubLanding{}, errors.New("invalid configuration: github.land_fix_enabled requires github.merge_state")
		}
		if _, has := github["max_land_attempts"]; has {
			return githubLanding{}, errors.New("invalid configuration: github.max_land_attempts requires github.merge_state")
		}
		if _, has := github["allow_conflict_resolution"]; has {
			return githubLanding{}, errors.New("invalid configuration: github.allow_conflict_resolution requires github.merge_state")
		}
		return githubLanding{}, nil
	}
	state, ok := stateValue.(string)
	state = strings.TrimSpace(state)
	if !ok || state == "" {
		return githubLanding{}, errors.New("invalid configuration: github.merge_state must be a non-empty string")
	}
	// merge_state must be an active/dispatchable state (the canonical
	// lifecycle's Merging): a session must actually be dispatched for that
	// issue before it can be bound and receive the zero-argument
	// github_land_pr tool (see codex/backend.go). It must never be terminal or
	// coincide with handoff_state, either of which would make the landing gate
	// unreachable or ambiguous.
	if !stateInList(state, activeStates) {
		return githubLanding{}, errors.New("invalid configuration: github.merge_state must be an active state")
	}
	if stateInList(state, terminalStates) {
		return githubLanding{}, errors.New("invalid configuration: github.merge_state must not be a terminal state")
	}
	if handoffState != "" && strings.EqualFold(handoffState, state) {
		return githubLanding{}, errors.New("invalid configuration: github.merge_state must differ from tracker.provider.handoff_state")
	}
	if len(requiredChecks) == 0 {
		return githubLanding{}, errors.New("invalid configuration: github.merge_state requires a non-empty github.required_checks list")
	}
	return githubLanding{
		mergeState:              state,
		mergeMethod:             mergeMethod,
		requiredChecks:          requiredChecks,
		updateStaleBranch:       updateStaleBranch,
		landFixEnabled:          landFixEnabled,
		maxLandAttempts:         maxLandAttempts,
		allowConflictResolution: allowConflictResolution,
	}, nil
}
