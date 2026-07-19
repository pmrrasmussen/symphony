// Package config loads and validates the repository-owned WORKFLOW.md contract.
package config

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"text/template"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/pmrrasmussen/symphony/internal/domain"
)

const fallbackPrompt = "Work on {{.issue.identifier}}: {{.issue.title}}\n\n{{.issue.description}}"

type Workflow struct {
	// Raw preserves the complete front-matter object, including extension keys.
	Raw    map[string]any
	Prompt string
	Config Settings
}

type Settings struct {
	Tracker            Tracker
	Polling            Polling
	Workspace          Workspace
	Hooks              Hooks
	Agent              Agent
	Codex              Codex
	GitHub             GitHub
	HostSecretEnvNames []string
	HostSecretValues   []string
	WorkflowPath       string
	LogRoot            string
	Prompt             string
	Warnings           []string
}

// GitHub is an optional, fixed-repository host integration. Invalid optional
// settings remain disabled so they cannot affect the manual workflow.
//
// MergeState, MergeMethod, and RequiredChecks are the landing policy (PMR-37)
// and deliberately do not follow that same fail-open-to-disabled rule: unlike
// owner/repository/token/etc, which silently disable the whole optional
// integration on any invalid value, an invalid landing field is rejected as a
// hard configuration error the same way tracker.provider.agent_transitions
// is. Granting an irreversible merge capability from an ambiguous or
// partially-invalid configuration is never an acceptable fallback.
type GitHub struct {
	Enabled                                        bool
	Owner, Repository, BaseBranch, Token, Endpoint string
	PollInterval                                   time.Duration
	// MergeState is the exact Linear state that grants the zero-argument
	// github_land_pr capability to a session bound to an issue currently in
	// that state. Empty means landing is not configured.
	MergeState string
	// MergeMethod is one of "merge", "squash", or "rebase" and defaults to
	// "merge" when MergeState is configured.
	MergeMethod string
	// RequiredChecks are the exact check/status names that must all be
	// present and successful (or neutral) before github_land_pr will merge.
	// Non-empty whenever MergeState is configured.
	RequiredChecks []string
}

const legacyProjectSlugWarning = "tracker.provider.project_slug is deprecated; migrate to project_slug_id"

type Tracker struct {
	Kind                                         string
	Provider                                     map[string]any
	RequiredLabels, ActiveStates, TerminalStates []string
	HandoffState, HandoffCommentTemplate         string
	AgentTransitions                             map[string]string
	// ChildIssueCreation enables the session-bound Codex create_child_issue
	// tool. It is opt-in and disabled by default; see child_issue_creation in
	// tracker.provider.
	ChildIssueCreation bool
}

type Polling struct{ Interval time.Duration }
type Workspace struct{ Root, SourceRoot string }
type Hooks struct {
	AfterCreate, BeforeRun, AfterRun, BeforeRemove string
	Timeout                                        time.Duration
}
type Agent struct {
	MaxConcurrent, MaxTurns int
	MaxRetryBackoff         time.Duration
	ByState                 map[string]int
}
type Codex struct {
	Command, ApprovalPolicy, ThreadSandbox string
	TurnSandboxPolicy                      any
	TurnTimeout, ReadTimeout, StallTimeout time.Duration
}

// Load validates the known core fields while retaining unknown extension keys.
// Prompt parsing is intentionally deferred to Render: a malformed template must
// fail only the affected run attempt, not stop polling or configuration reload.
func Load(path, logRoot string) (Workflow, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return Workflow{}, fmt.Errorf("missing_workflow_file: %w", err)
	}
	w, _, err := loadCandidate(abs, logRoot)
	return w, err
}

func loadCandidate(path, logRoot string) (Workflow, [sha256.Size]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Workflow{}, sha256.Sum256(nil), fmt.Errorf("missing_workflow_file: %w", err)
	}
	raw, body, err := parse(b)
	if err != nil {
		return Workflow{}, sha256.Sum256(b), err
	}
	sources := newSourceSnapshot(b)
	s, err := decode(raw, filepath.Dir(path), path, logRoot, sources)
	digest := sources.digest()
	if err != nil {
		return Workflow{}, digest, err
	}
	s.Prompt = strings.TrimSpace(body)
	if s.Prompt == "" {
		s.Prompt = fallbackPrompt
	}
	return Workflow{Raw: raw, Prompt: s.Prompt, Config: s}, digest, nil
}

func parse(b []byte) (map[string]any, string, error) {
	text := string(b)
	if !strings.HasPrefix(text, "---\n") && !strings.HasPrefix(text, "---\r\n") {
		return map[string]any{}, text, nil
	}
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	end := -1
	for i := 1; i < len(lines); i++ {
		if lines[i] == "---" {
			end = i
			break
		}
	}
	if end < 0 {
		return nil, "", errors.New("workflow_parse_error: unterminated front matter")
	}
	frontMatter := []byte(strings.Join(lines[1:end], "\n"))
	if len(bytes.TrimSpace(frontMatter)) == 0 {
		return map[string]any{}, strings.Join(lines[end+1:], "\n"), nil
	}
	var node yaml.Node
	if err := yaml.Unmarshal(frontMatter, &node); err != nil {
		return nil, "", fmt.Errorf("workflow_parse_error: %w", err)
	}
	if len(node.Content) != 1 || node.Content[0].Kind != yaml.MappingNode {
		return nil, "", errors.New("workflow_front_matter_not_a_map")
	}
	var raw map[string]any
	if err := node.Content[0].Decode(&raw); err != nil {
		return nil, "", fmt.Errorf("workflow_parse_error: %w", err)
	}
	if raw == nil {
		raw = map[string]any{}
	}
	return raw, strings.Join(lines[end+1:], "\n"), nil
}

func decode(raw map[string]any, base, path, logRoot string, sources *sourceSnapshot) (Settings, error) {
	tr, err := object(raw, "tracker")
	if err != nil {
		return Settings{}, err
	}
	provider, err := object(tr, "provider")
	if err != nil {
		return Settings{}, err
	}
	polling, err := object(raw, "polling")
	if err != nil {
		return Settings{}, err
	}
	workspace, err := object(raw, "workspace")
	if err != nil {
		return Settings{}, err
	}
	hooks, err := object(raw, "hooks")
	if err != nil {
		return Settings{}, err
	}
	agent, err := object(raw, "agent")
	if err != nil {
		return Settings{}, err
	}
	codex, err := object(raw, "codex")
	if err != nil {
		return Settings{}, err
	}
	github, githubObjectValid := raw["github"].(map[string]any)
	if _, exists := raw["github"]; !exists {
		github, githubObjectValid = nil, true
	}

	trackerKind, err := stringValue(tr, "kind")
	if err != nil {
		return Settings{}, err
	}
	requiredLabels, err := requiredLabelList(tr, "required_labels")
	if err != nil {
		return Settings{}, err
	}
	activeStates, err := stringList(tr, "active_states")
	if err != nil {
		return Settings{}, err
	}
	terminalStates, err := stringList(tr, "terminal_states")
	if err != nil {
		return Settings{}, err
	}
	resolvedProvider, providerWarnings, err := resolveProvider(provider, base, sources)
	if err != nil {
		return Settings{}, err
	}
	handoffState, handoffCommentTemplate, err := handoffPolicy(resolvedProvider, activeStates, terminalStates)
	if err != nil {
		return Settings{}, err
	}
	agentTransitions, err := agentTransitionPolicy(resolvedProvider, terminalStates)
	if err != nil {
		return Settings{}, err
	}
	childIssueCreation, err := childIssueCreationPolicy(resolvedProvider)
	if err != nil {
		return Settings{}, err
	}
	mergeState, mergeMethod, requiredChecks, err := githubLandingPolicy(github, activeStates, terminalStates, handoffState)
	if err != nil {
		return Settings{}, err
	}

	pollInterval, err := durationMS(polling, "interval_ms", 30_000)
	if err != nil {
		return Settings{}, err
	}
	workspaceRoot, err := pathValue(workspace, "root", filepath.Join(os.TempDir(), "symphony_workspaces"), base, sources)
	if err != nil {
		return Settings{}, err
	}
	sourceRoot, err := optionalPathValue(workspace, "source_root", base, sources)
	if err != nil {
		return Settings{}, err
	}
	hookTimeout, err := durationMS(hooks, "timeout_ms", 60_000)
	if err != nil {
		return Settings{}, err
	}
	afterCreate, err := script(hooks, "after_create")
	if err != nil {
		return Settings{}, err
	}
	beforeRun, err := script(hooks, "before_run")
	if err != nil {
		return Settings{}, err
	}
	afterRun, err := script(hooks, "after_run")
	if err != nil {
		return Settings{}, err
	}
	beforeRemove, err := script(hooks, "before_remove")
	if err != nil {
		return Settings{}, err
	}
	maxConcurrent, err := integer(agent, "max_concurrent_agents", 10)
	if err != nil {
		return Settings{}, err
	}
	maxTurns, err := integer(agent, "max_turns", 20)
	if err != nil {
		return Settings{}, err
	}
	maxRetryBackoff, err := durationMS(agent, "max_retry_backoff_ms", 300_000)
	if err != nil {
		return Settings{}, err
	}
	byState, err := stateLimits(agent["max_concurrent_agents_by_state"])
	if err != nil {
		return Settings{}, err
	}
	command, err := stringDefault(codex, "command", "codex app-server")
	if err != nil {
		return Settings{}, err
	}
	approvalPolicy, err := stringDefault(codex, "approval_policy", "never")
	if err != nil {
		return Settings{}, err
	}
	threadSandbox, err := stringDefault(codex, "thread_sandbox", "workspace-write")
	if err != nil {
		return Settings{}, err
	}
	turnTimeout, err := durationMS(codex, "turn_timeout_ms", 3_600_000)
	if err != nil {
		return Settings{}, err
	}
	readTimeout, err := durationMS(codex, "read_timeout_ms", 5_000)
	if err != nil {
		return Settings{}, err
	}
	stallTimeout, err := durationMS(codex, "stall_timeout_ms", 300_000)
	if err != nil {
		return Settings{}, err
	}
	githubSettings := decodeGitHub(github, githubObjectValid, base, sources)
	if mergeState != "" && !githubSettings.Enabled {
		return Settings{}, errors.New("invalid configuration: github.merge_state requires a fully configured github integration")
	}
	githubSettings.MergeState = mergeState
	githubSettings.MergeMethod = mergeMethod
	githubSettings.RequiredChecks = requiredChecks

	s := Settings{
		WorkflowPath: path,
		LogRoot:      normalizePath(logRootOrDefault(logRoot), base),
		Tracker: Tracker{
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
			AgentTransitions:       agentTransitions,
			ChildIssueCreation:     childIssueCreation,
		},
		Polling:   Polling{Interval: pollInterval},
		Workspace: Workspace{Root: workspaceRoot, SourceRoot: sourceRoot},
		Hooks:     Hooks{AfterCreate: afterCreate, BeforeRun: beforeRun, AfterRun: afterRun, BeforeRemove: beforeRemove, Timeout: hookTimeout},
		Agent:     Agent{MaxConcurrent: maxConcurrent, MaxTurns: maxTurns, MaxRetryBackoff: maxRetryBackoff, ByState: byState},
		Codex:     Codex{Command: command, ApprovalPolicy: approvalPolicy, ThreadSandbox: threadSandbox, TurnSandboxPolicy: codex["turn_sandbox_policy"], TurnTimeout: turnTimeout, ReadTimeout: readTimeout, StallTimeout: stallTimeout},
		GitHub:    githubSettings,
		// Keep only the names of environment variables that carry host
		// credentials. The Codex launcher uses this metadata to prevent those
		// variables from crossing the child-process boundary.
		HostSecretEnvNames: hostSecretEnvNames(provider, github),
		HostSecretValues:   hostSecretValues(resolvedProvider, github, base, sources),
		Warnings:           providerWarnings,
	}
	if s.Tracker.Kind != "linear" {
		return s, fmt.Errorf("invalid configuration: tracker.kind must be linear")
	}
	if len(s.Tracker.ActiveStates) == 0 || len(s.Tracker.TerminalStates) == 0 {
		return s, errors.New("invalid configuration: tracker active_states and terminal_states are required")
	}
	if s.Polling.Interval <= 0 || s.Hooks.Timeout <= 0 || s.Agent.MaxConcurrent <= 0 || s.Agent.MaxTurns <= 0 || s.Agent.MaxRetryBackoff <= 0 || strings.TrimSpace(s.Codex.Command) == "" || s.Codex.TurnTimeout <= 0 || s.Codex.ReadTimeout <= 0 {
		return s, errors.New("invalid configuration: non-positive duration or agent limit")
	}
	if s.Workspace.SourceRoot != "" {
		info, err := os.Stat(s.Workspace.SourceRoot)
		if err != nil || !info.IsDir() {
			return s, fmt.Errorf("invalid configuration: workspace.source_root is not a directory: %s", s.Workspace.SourceRoot)
		}
	}
	return s, nil
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

// hostSecretEnvNames extracts only environment variable names from credential
// references. It deliberately inspects the repository-owned raw fields so an
// optional GitHub integration that is currently disabled cannot accidentally
// leak its credential into a future Codex child process.
func hostSecretEnvNames(provider, github map[string]any) []string {
	names := map[string]struct{}{}
	collect := func(source map[string]any, keys ...string) {
		for _, key := range keys {
			value, ok := source[key].(string)
			if !ok {
				continue
			}
			if name, ok := environmentReferenceName(value); ok {
				names[name] = struct{}{}
			}
		}
	}
	collect(provider, "api_key", "api_key_file")
	collect(github, "token", "token_file")
	if len(names) == 0 {
		return nil
	}
	out := make([]string, 0, len(names))
	for name := range names {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// hostSecretValues keeps the resolved credentials needed to remove inherited
// values from the Codex environment. It deliberately includes an optional
// GitHub token even when the GitHub integration is disabled: configuration
// validity must not decide whether a host credential crosses the boundary.
func hostSecretValues(provider, github map[string]any, base string, sources *sourceSnapshot) []string {
	values := map[string]struct{}{}
	add := func(value string) {
		if value = strings.TrimSpace(value); value != "" {
			values[value] = struct{}{}
		}
	}
	if value, ok := provider["api_key"].(string); ok {
		add(value)
	}
	if github != nil {
		if file, ok := github["token_file"].(string); ok {
			if expanded, err := sources.expand(file, "github.token_file"); err == nil && strings.TrimSpace(expanded) != "" {
				if content, err := sources.readFile(normalizePath(expanded, base)); err == nil {
					add(string(content))
				}
			}
		} else if token, ok := github["token"].(string); ok && strings.HasPrefix(token, "$") {
			if expanded, err := sources.expand(token, "github.token"); err == nil {
				add(expanded)
			}
		}
	}
	if len(values) == 0 {
		return nil
	}
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func environmentReferenceName(value string) (string, bool) {
	if !strings.HasPrefix(value, "$") {
		return "", false
	}
	name := strings.TrimPrefix(value, "$")
	return name, validEnvironmentName(name)
}

func isLocalConfigHost(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
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

// agentTransitionPolicy parses the repository-owned exact state edges exposed
// to a Codex session. A mapping deliberately expresses one destination per
// source; it is not a destination allowlist that could permit reverse edges.
func agentTransitionPolicy(provider map[string]any, terminalStates []string) (map[string]string, error) {
	value, exists := provider["agent_transitions"]
	if !exists {
		return nil, nil
	}
	edges, ok := value.(map[string]any)
	if !ok || len(edges) == 0 {
		return nil, errors.New("invalid configuration: tracker.provider.agent_transitions must be a non-empty object")
	}
	result := make(map[string]string, len(edges))
	seen := make(map[string]struct{}, len(edges))
	for sourceValue, targetValue := range edges {
		source := strings.TrimSpace(sourceValue)
		target, ok := targetValue.(string)
		target = strings.TrimSpace(target)
		if source == "" || !ok || target == "" {
			return nil, errors.New("invalid configuration: tracker.provider.agent_transitions entries must map non-empty state names to non-empty state names")
		}
		key := strings.ToLower(source)
		if _, duplicate := seen[key]; duplicate {
			return nil, errors.New("invalid configuration: tracker.provider.agent_transitions has duplicate source states")
		}
		seen[key] = struct{}{}
		if strings.EqualFold(source, target) {
			return nil, errors.New("invalid configuration: tracker.provider.agent_transitions must not contain same-state edges")
		}
		if stateInList(source, terminalStates) || stateInList(target, terminalStates) {
			return nil, errors.New("invalid configuration: tracker.provider.agent_transitions must not contain terminal states")
		}
		result[source] = target
	}
	return result, nil
}

// childIssueCreationPolicy is deliberately a single boolean: unlike
// handoff_state or agent_transitions, the scope of the create_child_issue
// tool (project, team, and parent issue) is entirely derived from the active
// issue at session start, so there is no separate destination value to
// validate here.
func childIssueCreationPolicy(provider map[string]any) (bool, error) {
	value, exists := provider["child_issue_creation"]
	if !exists {
		return false, nil
	}
	enabled, ok := value.(bool)
	if !ok {
		return false, errors.New("invalid configuration: tracker.provider.child_issue_creation must be a boolean")
	}
	return enabled, nil
}

// validMergeMethods is the bounded merge-method enum accepted by
// github.merge_method. It intentionally mirrors GitHub's own three merge
// strategies and nothing else.
var validMergeMethods = map[string]bool{"merge": true, "squash": true, "rebase": true}

// githubLandingPolicy parses and strictly validates the optional
// github.merge_state, github.merge_method, and github.required_checks
// fields. Unlike the rest of the github: block, any malformed or ambiguous
// value here is a hard configuration error (see the GitHub struct doc
// comment) rather than a silently-disabled optional feature.
func githubLandingPolicy(github map[string]any, activeStates, terminalStates []string, handoffState string) (string, string, []string, error) {
	if github == nil {
		return "", "", nil, nil
	}
	mergeMethod := "merge"
	if value, exists := github["merge_method"]; exists {
		method, ok := value.(string)
		method = strings.ToLower(strings.TrimSpace(method))
		if !ok || !validMergeMethods[method] {
			return "", "", nil, errors.New("invalid configuration: github.merge_method must be one of merge, squash, rebase")
		}
		mergeMethod = method
	}
	requiredChecksValue, hasRequiredChecks := github["required_checks"]
	var requiredChecks []string
	if hasRequiredChecks {
		list, ok := requiredChecksValue.([]any)
		if !ok || len(list) == 0 {
			return "", "", nil, errors.New("invalid configuration: github.required_checks must be a non-empty list of strings")
		}
		seen := make(map[string]struct{}, len(list))
		for _, item := range list {
			name, ok := item.(string)
			name = strings.TrimSpace(name)
			if !ok || name == "" {
				return "", "", nil, errors.New("invalid configuration: github.required_checks entries must be non-empty strings")
			}
			key := strings.ToLower(name)
			if _, duplicate := seen[key]; duplicate {
				return "", "", nil, errors.New("invalid configuration: github.required_checks must not contain duplicate entries")
			}
			seen[key] = struct{}{}
			requiredChecks = append(requiredChecks, name)
		}
	}
	stateValue, hasState := github["merge_state"]
	if !hasState {
		if hasRequiredChecks {
			return "", "", nil, errors.New("invalid configuration: github.required_checks requires github.merge_state")
		}
		if _, hasMethod := github["merge_method"]; hasMethod {
			return "", "", nil, errors.New("invalid configuration: github.merge_method requires github.merge_state")
		}
		return "", "", nil, nil
	}
	state, ok := stateValue.(string)
	state = strings.TrimSpace(state)
	if !ok || state == "" {
		return "", "", nil, errors.New("invalid configuration: github.merge_state must be a non-empty string")
	}
	// merge_state must be an active/dispatchable state (the canonical
	// lifecycle's Merging): a session must actually be dispatched for that
	// issue before it can be bound and receive the zero-argument
	// github_land_pr tool (see codex/backend.go). It must never be terminal or
	// coincide with handoff_state, either of which would make the landing gate
	// unreachable or ambiguous.
	if !stateInList(state, activeStates) {
		return "", "", nil, errors.New("invalid configuration: github.merge_state must be an active state")
	}
	if stateInList(state, terminalStates) {
		return "", "", nil, errors.New("invalid configuration: github.merge_state must not be a terminal state")
	}
	if handoffState != "" && strings.EqualFold(handoffState, state) {
		return "", "", nil, errors.New("invalid configuration: github.merge_state must differ from tracker.provider.handoff_state")
	}
	if len(requiredChecks) == 0 {
		return "", "", nil, errors.New("invalid configuration: github.merge_state requires a non-empty github.required_checks list")
	}
	return state, mergeMethod, requiredChecks, nil
}

func stateInList(state string, states []string) bool {
	for _, candidate := range states {
		if strings.EqualFold(strings.TrimSpace(state), strings.TrimSpace(candidate)) {
			return true
		}
	}
	return false
}

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

// LinearSessionCapabilityEnabled reports whether any optional session-bound
// Linear capability is configured. Codex only receives a bound Linear session
// (and therefore any of the linear_graphql or create_child_issue tools) when
// at least one of these capabilities is enabled.
func (s Settings) LinearSessionCapabilityEnabled() bool {
	return strings.TrimSpace(s.Tracker.HandoffState) != "" || len(s.Tracker.AgentTransitions) > 0 || s.Tracker.ChildIssueCreation
}

// DeliveryInstructions describe the only PR delivery capability available to
// a worker. Host-generated guidance prevents a stale repository prompt from
// telling a restricted worker to publish directly to GitHub.
func (s Settings) DeliveryInstructions() string {
	if s.GitHub.Enabled && s.Tracker.HandoffState != "" {
		return `Delivery mode: host-side publish is available for this run.

- Make and validate the change in this workspace, then create a local commit.
- Do not run gh, git push, or otherwise try to publish directly to GitHub.
- When the worktree is clean and committed, call github_publish_pr with why, what_changed, and on_call. It is bound to this issue, repository, and branch and will create or update the PR body from those fields and hand the issue to review.
- Call github_pr_context (no arguments) to read bounded check status, review state, and unresolved feedback for that same pull request.`
	}
	requirements := "configure github.owner, github.repository, github.base_branch, and a repository-scoped GitHub token"
	if s.GitHub.Enabled {
		requirements = "configure tracker.provider.handoff_state in addition to the existing GitHub settings"
	} else if s.Tracker.HandoffState != "" {
		requirements = "configure the fixed github owner, repository, base branch, and repository-scoped token"
	}
	return `Delivery mode: manual. Host-side PR publishing is unavailable for this run.

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

type Store struct {
	mu            sync.RWMutex
	reloadMu      sync.Mutex
	path, logRoot string
	current       Workflow
	digest        [sha256.Size]byte
	rejected      [sha256.Size]byte
	hasRejected   bool
}

func NewStore(path, logRoot string) (*Store, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("missing_workflow_file: %w", err)
	}
	w, digest, err := loadCandidate(abs, logRoot)
	if err != nil {
		return nil, err
	}
	return &Store{path: w.Config.WorkflowPath, logRoot: logRoot, current: w, digest: digest}, nil
}

func (s *Store) Current() Workflow {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneWorkflow(s.current)
}

// Reload builds and validates a complete candidate before publishing it. Its
// digest includes referenced environment values and files, so fixing an input
// retries a previously rejected WORKFLOW.md without requiring an edit.
func (s *Store) Reload() error {
	_, err := s.ReloadIfChanged()
	return err
}

// ReloadIfChanged reports whether a new valid snapshot was published. A
// repeated rejected input is suppressed, while changes to its referenced
// environment values or files cause it to be validated again.
func (s *Store) ReloadIfChanged() (bool, error) {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	w, digest, err := loadCandidate(s.path, s.logRoot)
	if err != nil {
		s.mu.Lock()
		if s.hasRejected && digest == s.rejected {
			s.mu.Unlock()
			return false, nil
		}
		s.rejected, s.hasRejected = digest, true
		s.mu.Unlock()
		return false, err
	}
	s.mu.Lock()
	if digest == s.digest {
		s.hasRejected = false
		s.mu.Unlock()
		return false, nil
	}
	s.current = w
	s.digest = digest
	s.hasRejected = false
	s.mu.Unlock()
	return true, nil
}

func cloneWorkflow(w Workflow) Workflow {
	w.Raw = cloneMap(w.Raw)
	w.Config.Tracker.Provider = cloneMap(w.Config.Tracker.Provider)
	w.Config.Tracker.RequiredLabels = append([]string(nil), w.Config.Tracker.RequiredLabels...)
	w.Config.Tracker.ActiveStates = append([]string(nil), w.Config.Tracker.ActiveStates...)
	w.Config.Tracker.TerminalStates = append([]string(nil), w.Config.Tracker.TerminalStates...)
	w.Config.Tracker.AgentTransitions = cloneStringMap(w.Config.Tracker.AgentTransitions)
	w.Config.GitHub.RequiredChecks = append([]string(nil), w.Config.GitHub.RequiredChecks...)
	w.Config.HostSecretEnvNames = append([]string(nil), w.Config.HostSecretEnvNames...)
	w.Config.HostSecretValues = append([]string(nil), w.Config.HostSecretValues...)
	w.Config.Warnings = append([]string(nil), w.Config.Warnings...)
	byState := w.Config.Agent.ByState
	w.Config.Agent.ByState = make(map[string]int, len(byState))
	for state, limit := range byState {
		w.Config.Agent.ByState[state] = limit
	}
	w.Config.Codex.TurnSandboxPolicy = cloneValue(w.Config.Codex.TurnSandboxPolicy)
	return w
}

func cloneStringMap(source map[string]string) map[string]string {
	if source == nil {
		return nil
	}
	copy := make(map[string]string, len(source))
	for key, value := range source {
		copy[key] = value
	}
	return copy
}

func cloneMap(source map[string]any) map[string]any {
	if source == nil {
		return nil
	}
	copy := make(map[string]any, len(source))
	for key, value := range source {
		copy[key] = cloneValue(value)
	}
	return copy
}

func cloneValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneMap(typed)
	case []any:
		copy := make([]any, len(typed))
		for index, value := range typed {
			copy[index] = cloneValue(value)
		}
		return copy
	default:
		return value
	}
}

func object(parent map[string]any, key string) (map[string]any, error) {
	v, exists := parent[key]
	if !exists {
		return map[string]any{}, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("invalid configuration: %s must be an object", key)
	}
	return m, nil
}

func stringValue(m map[string]any, key string) (string, error) {
	v, exists := m[key]
	if !exists {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("invalid configuration: %s must be a string", key)
	}
	return s, nil
}

func stringDefault(m map[string]any, key, fallback string) (string, error) {
	v, exists := m[key]
	if !exists {
		return fallback, nil
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("invalid configuration: %s must be a string", key)
	}
	return s, nil
}

func stringList(m map[string]any, key string) ([]string, error) {
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
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out, nil
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

func integer(m map[string]any, key string, fallback int) (int, error) {
	v, exists := m[key]
	if !exists {
		return fallback, nil
	}
	i, ok := v.(int)
	if !ok {
		return 0, fmt.Errorf("invalid configuration: %s must be an integer", key)
	}
	return i, nil
}

func durationMS(m map[string]any, key string, fallback int) (time.Duration, error) {
	i, err := integer(m, key, fallback)
	if err != nil {
		return 0, err
	}
	return time.Duration(i) * time.Millisecond, nil
}

func script(m map[string]any, key string) (string, error) {
	v, exists := m[key]
	if !exists || v == nil {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("invalid configuration: %s must be a string", key)
	}
	return s, nil
}

func stateLimits(v any) (map[string]int, error) {
	out := map[string]int{}
	if v == nil {
		return out, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("invalid configuration: max_concurrent_agents_by_state must be an object")
	}
	for state, value := range m {
		limit, ok := value.(int)
		// This map deliberately ignores invalid per-state entries, as specified.
		if !ok || limit <= 0 || strings.TrimSpace(state) == "" {
			continue
		}
		out[strings.ToLower(strings.TrimSpace(state))] = limit
	}
	return out, nil
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

func pathValue(m map[string]any, key, fallback, base string, sources *sourceSnapshot) (string, error) {
	v, exists := m[key]
	if !exists {
		return normalizePath(fallback, base), nil
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("invalid configuration: %s must be a path string", key)
	}
	s, err := sources.expand(s, "workspace."+key)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(s) == "" {
		return "", fmt.Errorf("invalid configuration: %s must not be empty", key)
	}
	return normalizePath(s, base), nil
}

func optionalPathValue(m map[string]any, key, base string, sources *sourceSnapshot) (string, error) {
	v, exists := m[key]
	if !exists || v == nil {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("invalid configuration: %s must be a path string", key)
	}
	if strings.TrimSpace(s) == "" {
		return "", nil
	}
	s, err := sources.expand(s, "workspace."+key)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(s) == "" {
		return "", fmt.Errorf("invalid configuration: workspace.%s environment reference is unresolved", key)
	}
	return normalizePath(s, base), nil
}

type fileSource struct {
	content []byte
	err     error
}

// sourceSnapshot freezes process environment values and caches referenced
// files while one candidate is decoded. The resulting digest deliberately
// includes only source identities and bytes, never values in errors or logs.
type sourceSnapshot struct {
	workflow    []byte
	environment map[string]string
	references  map[string]string
	files       map[string]fileSource
}

func newSourceSnapshot(workflow []byte) *sourceSnapshot {
	environment := make(map[string]string)
	for _, assignment := range os.Environ() {
		name, value, found := strings.Cut(assignment, "=")
		if found {
			environment[name] = value
		}
	}
	return &sourceSnapshot{workflow: workflow, environment: environment, references: map[string]string{}, files: map[string]fileSource{}}
}

func (s *sourceSnapshot) expand(value, field string) (string, error) {
	if !strings.HasPrefix(value, "$") {
		return value, nil
	}
	name := strings.TrimPrefix(value, "$")
	if !validEnvironmentName(name) {
		return "", fmt.Errorf("invalid configuration: %s must use exact $VARNAME environment syntax", field)
	}
	resolved := s.environment[name]
	s.references[name] = resolved
	return resolved, nil
}

func validEnvironmentName(name string) bool {
	if name == "" {
		return false
	}
	for index, character := range name {
		if character == '_' || character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || index > 0 && character >= '0' && character <= '9' {
			continue
		}
		return false
	}
	return true
}

func (s *sourceSnapshot) readFile(path string) ([]byte, error) {
	if source, ok := s.files[path]; ok {
		return append([]byte(nil), source.content...), source.err
	}
	content, err := os.ReadFile(path)
	s.files[path] = fileSource{content: append([]byte(nil), content...), err: err}
	return content, err
}

func (s *sourceSnapshot) digest() [sha256.Size]byte {
	hash := sha256.New()
	_, _ = hash.Write(s.workflow)
	names := make([]string, 0, len(s.references))
	for name := range s.references {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		_, _ = fmt.Fprintf(hash, "\x00env:%s\x00%s", name, s.references[name])
	}
	paths := make([]string, 0, len(s.files))
	for path := range s.files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		source := s.files[path]
		_, _ = fmt.Fprintf(hash, "\x00file:%s\x00", path)
		_, _ = hash.Write(source.content)
		if source.err != nil {
			_, _ = fmt.Fprint(hash, "\x00unreadable")
		}
	}
	var digest [sha256.Size]byte
	copy(digest[:], hash.Sum(nil))
	return digest
}

func normalizePath(value, base string) string {
	if value == "~" || strings.HasPrefix(value, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			if value == "~" {
				value = home
			} else {
				value = filepath.Join(home, value[2:])
			}
		}
	}
	if !filepath.IsAbs(value) {
		value = filepath.Join(base, value)
	}
	abs, err := filepath.Abs(filepath.Clean(value))
	if err != nil {
		return filepath.Clean(value)
	}
	return abs
}

func logRootOrDefault(value string) string {
	if strings.TrimSpace(value) == "" {
		return ".symphony/logs"
	}
	return value
}

func stringsLower(values []string) []string {
	for i := range values {
		values[i] = strings.ToLower(strings.TrimSpace(values[i]))
	}
	return values
}
