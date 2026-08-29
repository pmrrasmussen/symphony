package config

import (
	"errors"
	"net/url"
	"sort"
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

// githubDisabledWarning names the fields that forced a present github: block to
// decode disabled. Disabling stays silent to the run itself -- worktrees are cut
// from the workspace default branch and the agent is told delivery is manual --
// so the operator learns which field did it from Settings.Warnings and
// preflight, rather than from a pull request opened off the wrong base
// (PMR-178).
func githubDisabledWarning(fields ...string) []string {
	sort.Strings(fields)
	return []string{"github integration is disabled and delivery falls back to manual: invalid or missing " + strings.Join(fields, ", ")}
}

// validBaseBranch accepts the branch names Git and the GitHub API accept as a
// pull request base. Unlike an owner or a repository, each a single path
// segment, a branch name may contain slashes: release/1.0 is legal, and holding
// base_branch to the owner/repository rule instead disabled the whole
// integration for it and cut every worktree from main (PMR-178). The rules
// below are git-check-ref-format's, applied to the refs/heads/<value> this name
// expands to.
func validBaseBranch(value string) bool {
	if value == "" || strings.HasSuffix(value, ".") || strings.Contains(value, "..") || strings.Contains(value, "@{") {
		return false
	}
	if strings.ContainsAny(value, "\\ ~^:?*[") || strings.ContainsFunc(value, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return false
	}
	// An empty component rejects a leading, trailing, or doubled slash; the other
	// two are Git's own per-component rules.
	for _, component := range strings.Split(value, "/") {
		if component == "" || strings.HasPrefix(component, ".") || strings.HasSuffix(component, ".lock") {
			return false
		}
	}
	return true
}

// decodeGitHub decodes the github: block, returning the warnings a present but
// disabled block must surface alongside it. An absent block warns about
// nothing: not configuring the integration is a supported choice.
func decodeGitHub(raw map[string]any, objectValid bool, base string, sources *sourceSnapshot) (GitHub, []string) {
	if !objectValid {
		return GitHub{}, githubDisabledWarning("github")
	}
	if raw == nil {
		return GitHub{}, nil
	}
	read := func(key string) (string, bool) {
		value, exists := raw[key]
		text, ok := value.(string)
		return strings.TrimSpace(text), exists && ok
	}
	owner, ownerOK := read("owner")
	repository, repositoryOK := read("repository")
	baseBranch, baseOK := read("base_branch")
	// An absent or blank base_branch means main. A present non-string value is
	// left invalid rather than defaulted, so it disables the integration with a
	// warning naming the field instead of quietly basing every worktree on main.
	if _, exists := raw["base_branch"]; !exists || (baseOK && baseBranch == "") {
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
	// tokenField follows the token to whichever key supplied it, so an empty
	// credential file is reported as github.token_file rather than sending the
	// operator to a github.token they never wrote.
	token, tokenOK := read("token")
	tokenField := "github.token"
	if file, exists := raw["token_file"]; exists {
		tokenField = "github.token_file"
		path, ok := file.(string)
		if !ok {
			return GitHub{}, githubDisabledWarning("github.token_file")
		}
		expanded, err := sources.expand(path, "github.token_file")
		if err != nil || strings.TrimSpace(expanded) == "" {
			return GitHub{}, githubDisabledWarning("github.token_file")
		}
		content, err := sources.readFile(normalizePath(expanded, base))
		if err != nil {
			return GitHub{}, githubDisabledWarning("github.token_file")
		}
		token, tokenOK = strings.TrimSpace(string(content)), true
	} else if tokenOK && strings.HasPrefix(token, "$") {
		resolved, err := sources.expand(token, "github.token")
		if err != nil {
			return GitHub{}, githubDisabledWarning("github.token")
		}
		token = strings.TrimSpace(resolved)
	} else if tokenOK {
		return GitHub{}, githubDisabledWarning("github.token")
	}
	endpointURL, err := url.Parse(endpoint)
	endpointValid := err == nil && endpointURL.Host != "" && (endpointURL.Scheme == "https" || endpointURL.Scheme == "http" && isLocalConfigHost(endpointURL.Hostname()))
	// owner and repository are single path segments of a GitHub URL, so a slash
	// in either is a mistake; base_branch has its own rule above.
	validName := func(value string) bool {
		return value != "" && !strings.ContainsAny(value, "/\\\r\n\t ") && value != "." && value != ".."
	}
	var invalid []string
	if !ownerOK || !validName(owner) {
		invalid = append(invalid, "github.owner")
	}
	if !repositoryOK || !validName(repository) {
		invalid = append(invalid, "github.repository")
	}
	if !baseOK || !validBaseBranch(baseBranch) {
		invalid = append(invalid, "github.base_branch")
	}
	if !endpointOK || !endpointValid {
		invalid = append(invalid, "github.endpoint")
	}
	if !pollOK || pollMS <= 0 {
		invalid = append(invalid, "github.poll_interval_ms")
	}
	if !tokenOK || token == "" {
		invalid = append(invalid, tokenField)
	}
	if len(invalid) > 0 {
		return GitHub{}, githubDisabledWarning(invalid...)
	}
	return GitHub{Enabled: true, Owner: owner, Repository: repository, BaseBranch: baseBranch, Token: token, Endpoint: strings.TrimRight(endpoint, "/"), PollInterval: time.Duration(pollMS) * time.Millisecond}, nil
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
