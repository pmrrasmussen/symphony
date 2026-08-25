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
	Claude             Claude
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
// MergeState, MergeMethod, RequiredChecks, UpdateStaleBranch, and the
// bounded-fix fields (LandFixEnabled, MaxLandAttempts, AllowConflictResolution)
// are the landing policy (PMR-37/PMR-45/PMR-46)
// and deliberately do not follow that same fail-open-to-disabled rule: unlike
// owner/repository/token/etc, which silently disable the whole optional
// integration on any invalid value, an invalid landing field is rejected as a
// hard configuration error the same way tracker.provider.transitions is.
// Granting an irreversible merge capability from an ambiguous or
// partially-invalid configuration is never an acceptable fallback.
type GitHub struct {
	Enabled                                        bool
	Owner, Repository, BaseBranch, Token, Endpoint string
	// PollInterval paces the host's linked pull-request poll loop and, since
	// PMR-78, is also the floor for the coordinator's delayed landing
	// redispatch after github_land_pr reports a non-terminal wait. Consecutive
	// waits escalate that delay toward Agent.MaxRetryBackoff.
	PollInterval time.Duration
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
	// UpdateStaleBranch permits github_land_pr to ask GitHub to merge the
	// current base into a clean, stale pull-request branch. It is opt-in and
	// disabled by default.
	UpdateStaleBranch bool
	// LandFixEnabled permits github_land_pr, for a retryable hard gate, to
	// return a non-terminal fix request (naming the gate) so the same Codex
	// turn can fix, push, and retry, instead of immediately refusing. It is
	// opt-in and disabled by default; with it off, every gate refuses exactly
	// as before (PMR-46).
	LandFixEnabled bool
	// MaxLandAttempts bounds how many non-terminal fix requests a single
	// session may hand back before it refuses and returns the issue to review.
	// It defaults to 2 and is only meaningful when LandFixEnabled is true.
	MaxLandAttempts int
	// AllowConflictResolution makes a merge conflict a retryable gate (only
	// when LandFixEnabled is true). Off by default, so a merge conflict refuses
	// immediately exactly as before.
	AllowConflictResolution bool
}

const (
	legacyProjectSlugWarning        = "tracker.provider.project_slug is deprecated; migrate to project_slug_id"
	legacyChildIssueCreationWarning = "tracker.provider.child_issue_creation is deprecated; migrate to followup_issue_creation"
)

type Tracker struct {
	Kind                                         string
	Provider                                     map[string]any
	RequiredLabels, ActiveStates, TerminalStates []string
	HandoffState, HandoffCommentTemplate         string
	// HostTransitions is the single host-owned tracker transition policy
	// (tracker.provider.transitions). Symphony applies every edge in it itself,
	// with the host Linear credential; none is ever exposed to a Codex session.
	// The agent has no issue-state transition capability.
	HostTransitions HostTransitions
	// FollowupIssueCreation enables the session-bound Codex
	// create_followup_issue tool. It is opt-in and disabled by default; see
	// followup_issue_creation in tracker.provider.
	FollowupIssueCreation bool
}

// HostTransitions holds the two host-applied tracker transition edge sets.
// They are kept structurally distinct on purpose and must NOT be folded into
// one flat source->target map: Merging is both a dispatchable/active state and
// the land-fallback source, so a flat map consumed at dispatch would wrongly
// move a freshly dispatched Merging landing agent's issue to In Review. Start
// is keyed by the issue's current state and applied only at dispatch;
// RefuseLanding is keyed by github.merge_state and applied only when
// github_land_pr hits a hard gate. Both maps use lowercased source keys for
// direct comparison against a normalized issue state.
type HostTransitions struct {
	// Start are the dispatch-time edges the coordinator applies when it
	// launches an issue (the canonical lifecycle's Todo -> In Progress). Both
	// endpoints of every edge must be active, non-terminal states. The move is
	// idempotent (an already-started issue is untouched) and fail-safe (a
	// failed move is logged and never blocks or double-dispatches the run).
	Start map[string]string
	// RefuseLanding are the edges RefuseLanding uses after a github_land_pr
	// hard gate refuses to merge (the canonical lifecycle's Merging -> In
	// Review), keyed by github.merge_state. They are never applied at dispatch.
	// Terminal and same-state edges are rejected.
	RefuseLanding map[string]string
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
	// Backend names the agent runtime new sessions are started on. It is
	// validated against agentBackends, so an unknown value fails the whole
	// candidate rather than silently falling back to a default.
	Backend string
}

// AgentLaunch is the backend-neutral launch contract the scheduler applies to
// one run: what to execute, where, under which sandbox, and the four timeout
// budgets. Coordination reads this instead of any single backend's settings
// block, so adding a backend does not spread its vocabulary through the
// scheduler. The workspace directory and writable paths are not here: they are
// per-run values the workspace layer owns and travel on domain.AgentRequest.
// Every field is captured per launch; see Settings.AgentLaunch.
//
// TurnSandboxPolicy is interface-typed and may hold a map, so this struct is not
// safely comparable: compare fields, never two launches with ==.
type AgentLaunch struct {
	Backend                                              string
	Command, ApprovalPolicy, ThreadSandbox, Model        string
	TurnSandboxPolicy                                    any
	TurnTimeout, ReadTimeout, StartTimeout, StallTimeout time.Duration
}
type Codex struct {
	Command, ApprovalPolicy, ThreadSandbox string
	TurnSandboxPolicy                      any
	TurnTimeout, ReadTimeout, StallTimeout time.Duration
	// StartTimeout bounds the cold-start handshake and thread/start RPCs
	// separately from ReadTimeout. A cold codex app-server start (process
	// spawn plus first model load) routinely exceeds the small steady-state
	// read timeout, so it gets a generous budget that does not loosen
	// mid-turn hang detection.
	StartTimeout time.Duration
}

// Claude configures the Claude Code agent backend. It is deliberately small:
// the launch policy (tool set, permission mode, settings sources, and the
// sandbox) is fixed by Symphony rather than configurable, so an operator cannot
// widen the boundary the child runs under.
type Claude struct {
	Command, Model            string
	TurnTimeout, StallTimeout time.Duration
}

// Load validates the known core fields while retaining unknown extension keys.
// Prompt parsing is intentionally deferred to Render: a malformed template must
// fail only the affected run attempt, not stop polling or configuration reload.
func Load(path, logRoot string) (Workflow, error) {
	return LoadWithEnvironment(path, logRoot, nil)
}

// LoadWithEnvironment validates a workflow with environment values supplied by
// its hosting boundary. Values in environment override are used only while
// loading this candidate; they are never persisted or exposed by Settings.
// It is intended for read-only operator inspection of LaunchAgent-specific
// environments that are not present in the interactive process.
func LoadWithEnvironment(path, logRoot string, environment map[string]string) (Workflow, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return Workflow{}, fmt.Errorf("missing_workflow_file: %w", err)
	}
	w, _, err := loadCandidateWithEnvironment(abs, logRoot, environment)
	return w, err
}

func loadCandidate(path, logRoot string) (Workflow, [sha256.Size]byte, error) {
	return loadCandidateWithEnvironment(path, logRoot, nil)
}

func loadCandidateWithEnvironment(path, logRoot string, environment map[string]string) (Workflow, [sha256.Size]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Workflow{}, sha256.Sum256(nil), fmt.Errorf("missing_workflow_file: %w", err)
	}
	raw, body, err := parse(b)
	if err != nil {
		return Workflow{}, sha256.Sum256(b), err
	}
	sources := newSourceSnapshot(b, environment)
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

// ParseRaw parses the workflow envelope without resolving configuration or
// credential values. It is intended for host-side tooling that needs to find
// credential *references* before it can construct the service environment.
// Callers must still use LoadWithEnvironment for full validation.
func ParseRaw(b []byte) (map[string]any, string, error) {
	return parse(b)
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
	claude, err := object(raw, "claude")
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
	hostTransitions, err := hostTransitionPolicy(resolvedProvider, activeStates, terminalStates)
	if err != nil {
		return Settings{}, err
	}
	followupIssueCreation, err := followupIssueCreationPolicy(resolvedProvider, activeStates)
	if err != nil {
		return Settings{}, err
	}
	landing, err := githubLandingPolicy(github, activeStates, terminalStates, handoffState)
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
	backend, err := stringDefault(agent, "backend", DefaultAgentBackend)
	if err != nil {
		return Settings{}, err
	}
	if !contains(agentBackends, backend) {
		return Settings{}, fmt.Errorf("invalid configuration: agent.backend must be one of %s, got %q", strings.Join(agentBackends, ", "), backend)
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
	if !contains(sandboxModes, threadSandbox) {
		return Settings{}, fmt.Errorf("invalid configuration: codex.thread_sandbox must be one of %s, got %q", strings.Join(sandboxModes, ", "), threadSandbox)
	}
	turnSandboxPolicy, err := sandboxPolicy(codex, threadSandbox)
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
	// A cold codex app-server start (process spawn plus first model load) far
	// exceeds the small steady-state read_timeout_ms. This budget governs only
	// the initial handshake and thread/start; 120s gives generous headroom
	// above the ~60s that survived a real cold start live (PMR-57).
	startTimeout, err := durationMS(codex, "start_timeout_ms", 120_000)
	if err != nil {
		return Settings{}, err
	}
	stallTimeout, err := durationMS(codex, "stall_timeout_ms", 300_000)
	if err != nil {
		return Settings{}, err
	}
	claudeSettings, err := decodeClaude(claude)
	if err != nil {
		return Settings{}, err
	}
	githubSettings := decodeGitHub(github, githubObjectValid, base, sources)
	if landing.mergeState != "" && !githubSettings.Enabled {
		return Settings{}, errors.New("invalid configuration: github.merge_state requires a fully configured github integration")
	}
	githubSettings.MergeState = landing.mergeState
	githubSettings.MergeMethod = landing.mergeMethod
	githubSettings.RequiredChecks = landing.requiredChecks
	githubSettings.UpdateStaleBranch = landing.updateStaleBranch
	githubSettings.LandFixEnabled = landing.landFixEnabled
	githubSettings.MaxLandAttempts = landing.maxLandAttempts
	githubSettings.AllowConflictResolution = landing.allowConflictResolution

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
			HostTransitions:        hostTransitions,
			FollowupIssueCreation:  followupIssueCreation,
		},
		Polling:   Polling{Interval: pollInterval},
		Workspace: Workspace{Root: workspaceRoot, SourceRoot: sourceRoot},
		Hooks:     Hooks{AfterCreate: afterCreate, BeforeRun: beforeRun, AfterRun: afterRun, BeforeRemove: beforeRemove, Timeout: hookTimeout},
		Claude:    claudeSettings,
		Agent:     Agent{Backend: backend, MaxConcurrent: maxConcurrent, MaxTurns: maxTurns, MaxRetryBackoff: maxRetryBackoff, ByState: byState},
		Codex:     Codex{Command: command, ApprovalPolicy: approvalPolicy, ThreadSandbox: threadSandbox, TurnSandboxPolicy: turnSandboxPolicy, TurnTimeout: turnTimeout, ReadTimeout: readTimeout, StartTimeout: startTimeout, StallTimeout: stallTimeout},
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
	if s.Polling.Interval <= 0 || s.Hooks.Timeout <= 0 || s.Agent.MaxConcurrent <= 0 || s.Agent.MaxTurns <= 0 || s.Agent.MaxRetryBackoff <= 0 {
		return s, errors.New("invalid configuration: non-positive duration or agent limit")
	}
	// Only the selected backend's launch contract has to be complete: a workflow
	// that never starts a Codex session should not be rejected for an absent
	// codex block, and vice versa.
	switch s.Agent.Backend {
	case ClaudeAgentBackend:
		if strings.TrimSpace(s.Claude.Command) == "" || s.Claude.TurnTimeout <= 0 || s.Claude.StallTimeout <= 0 {
			return s, errors.New("invalid configuration: non-positive duration or agent limit")
		}
		// The private capability bridge does not exist yet, so a Claude session
		// has no way to reach Symphony's bounded capabilities. Refuse the
		// configuration rather than run an agent that silently cannot publish a
		// pull request or file a follow-up.
		if s.LinearSessionCapabilityEnabled() || s.GitHub.Enabled {
			return s, errors.New("invalid configuration: agent.backend claude cannot yet be combined with Symphony session capabilities (tracker.provider.handoff_state, tracker.provider.followup_issue_creation, or an enabled github integration)")
		}
	case DefaultAgentBackend:
		if strings.TrimSpace(s.Codex.Command) == "" || s.Codex.TurnTimeout <= 0 || s.Codex.ReadTimeout <= 0 || s.Codex.StartTimeout <= 0 {
			return s, errors.New("invalid configuration: non-positive duration or agent limit")
		}
	default:
		// Unreachable for a loaded workflow, because backend was already checked
		// against agentBackends. It is an error rather than the Codex arm so that
		// adding a name to agentBackends without giving it an arm here fails
		// loudly instead of validating the new backend against codex.command and
		// the codex timeouts -- the same reason AgentLaunchFor answers false for a
		// name it does not know.
		return s, fmt.Errorf("invalid configuration: agent.backend %q has no validated launch contract", s.Agent.Backend)
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
	w.Config.Tracker.HostTransitions.Start = cloneStringMap(w.Config.Tracker.HostTransitions.Start)
	w.Config.Tracker.HostTransitions.RefuseLanding = cloneStringMap(w.Config.Tracker.HostTransitions.RefuseLanding)
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

// DefaultAgentBackend keeps every workflow written before agent.backend existed
// behaving exactly as it did. It is exported so the process that registers the
// backends cannot drift from the value this package accepts.
const DefaultAgentBackend = "codex"

// ClaudeAgentBackend runs turns on the Claude Code CLI.
const ClaudeAgentBackend = "claude"

// agentBackends is the closed set of selectable agent runtimes.
var agentBackends = []string{DefaultAgentBackend, ClaudeAgentBackend}

// AgentBackends returns every selectable agent runtime. The process that
// registers backend implementations reads this so a name this package accepts
// can never lack an implementation -- otherwise a valid configuration would pass
// validation and preflight and fail only at the first dispatch.
func AgentBackends() []string { return append([]string(nil), agentBackends...) }

// AgentLaunch resolves the launch contract for the configured backend. The
// configured value is always one this package knows, so the lookup cannot fail.
func (s Settings) AgentLaunch() AgentLaunch {
	launch, _ := s.AgentLaunchFor(s.Agent.Backend)
	return launch
}

// AgentLaunchFor resolves the launch contract for a named backend, which is how
// the scheduler applies current policy to a run that a previous configuration
// started: reload keeps publishing live values, but they are read under the
// backend that owns the run rather than whichever one is configured now.
//
// The boolean reports whether the name is known. It exists so a caller cannot
// mistake an unknown backend's zero-valued launch for a real policy: a zero
// stall budget, for instance, reads as "stall detection disabled" and would
// leave a run unsupervised in silence.
func (s Settings) AgentLaunchFor(backend string) (AgentLaunch, bool) {
	launch := AgentLaunch{Backend: backend}
	if launch.Backend == "" {
		launch.Backend = DefaultAgentBackend
	}
	switch launch.Backend {
	case DefaultAgentBackend:
		launch.Command = s.Codex.Command
		launch.ApprovalPolicy = s.Codex.ApprovalPolicy
		launch.ThreadSandbox = s.Codex.ThreadSandbox
		launch.TurnSandboxPolicy = s.Codex.TurnSandboxPolicy
		launch.TurnTimeout = s.Codex.TurnTimeout
		launch.ReadTimeout = s.Codex.ReadTimeout
		launch.StartTimeout = s.Codex.StartTimeout
		launch.StallTimeout = s.Codex.StallTimeout
	case ClaudeAgentBackend:
		launch.Command = s.Claude.Command
		launch.Model = s.Claude.Model
		launch.TurnTimeout = s.Claude.TurnTimeout
		launch.StallTimeout = s.Claude.StallTimeout
	default:
		return launch, false
	}
	return launch, true
}

// claudeKeys is the complete set of claude block keys. An unknown key is
// refused rather than ignored: a typo in a launch field would otherwise leave
// the default silently in place, the same failure class that made a misspelled
// sandbox key silently deny nothing.
var claudeKeys = []string{"command", "model", "turn_timeout_ms", "stall_timeout_ms"}

func decodeClaude(block map[string]any) (Claude, error) {
	for key := range block {
		if !contains(claudeKeys, key) {
			return Claude{}, fmt.Errorf("invalid configuration: unknown claude field %q", key)
		}
	}
	command, err := stringDefault(block, "command", "claude")
	if err != nil {
		return Claude{}, err
	}
	model, err := stringDefault(block, "model", "")
	if err != nil {
		return Claude{}, err
	}
	turnTimeout, err := durationMS(block, "turn_timeout_ms", 3_600_000)
	if err != nil {
		return Claude{}, err
	}
	stallTimeout, err := durationMS(block, "stall_timeout_ms", 300_000)
	if err != nil {
		return Claude{}, err
	}
	return Claude{Command: command, Model: model, TurnTimeout: turnTimeout, StallTimeout: stallTimeout}, nil
}

// sandboxModes are the thread_sandbox values Codex accepts (app-server
// SandboxMode). An unlisted value is rejected here rather than surfacing as an
// opaque thread/start failure on every dispatch.
var sandboxModes = []string{"read-only", "workspace-write", "danger-full-access"}

const (
	sandboxFieldBool          = "bool"
	sandboxFieldNetworkAccess = "networkAccess"
)

// sandboxPolicyFields lists, per Codex SandboxPolicy variant, exactly the
// fields the app-server protocol accepts and the value form each takes. Codex
// ignores unknown fields silently, so `networkAcces: true` would otherwise
// leave egress denied while the operator believed it enabled; anything outside
// these sets is rejected instead. Note that networkAccess is a boolean for
// readOnly and workspaceWrite, the NetworkAccess enum for externalSandbox, and
// not accepted at all for dangerFullAccess.
//
// workspaceWrite's real schema also accepts writableRoots. It is deliberately
// omitted: Symphony's launcher is the only supplier of writable paths (PMR-65)
// and merges its narrowed Git roots into whatever is configured, so a
// configured root would widen write authority past what the documentation
// promises. sandboxPolicy rejects the key with a dedicated message.
var sandboxPolicyFields = map[string]map[string]string{
	"dangerFullAccess": {},
	"readOnly":         {"networkAccess": sandboxFieldBool},
	"externalSandbox":  {"networkAccess": sandboxFieldNetworkAccess},
	"workspaceWrite":   {"networkAccess": sandboxFieldBool, "excludeSlashTmp": sandboxFieldBool, "excludeTmpdirEnvVar": sandboxFieldBool},
}

// sandboxPolicyThreadModes lists the thread_sandbox values each policy type may
// accompany. Codex applies sandboxPolicy as an override of the thread mode for
// that turn and every later one, so a policy requesting authority the thread
// mode lacks silently escalates the session. workspaceWrite must match exactly
// because the launcher applies its narrowed Git metadata grant only to
// workspace-write threads: permitting it elsewhere would forward a
// workspace-write policy carrying no narrowing at all. readOnly is never
// broader than any mode, and externalSandbox delegates confinement outside
// Codex entirely, so it is treated as full access.
var sandboxPolicyThreadModes = map[string][]string{
	"readOnly":         {"read-only", "workspace-write", "danger-full-access"},
	"workspaceWrite":   {"workspace-write"},
	"dangerFullAccess": {"danger-full-access"},
	"externalSandbox":  {"danger-full-access"},
}

// sandboxPolicy validates the optional per-turn sandbox policy against the
// Codex SandboxPolicy schema. The value is forwarded verbatim as turn/start's
// sandboxPolicy, so a malformed shape must fail configuration validation
// instead of surfacing as an opaque app-server rejection mid-run -- or, worse,
// being accepted with a field Codex silently ignores. Absence stays nil so the
// launcher's own narrowed workspace-write grant remains the effective policy
// (PMR-65, PMR-80). Values are matched exactly, never trimmed, because what is
// validated here is what is forwarded.
func sandboxPolicy(codex map[string]any, threadSandbox string) (any, error) {
	v, exists := codex["turn_sandbox_policy"]
	if !exists || v == nil {
		return nil, nil
	}
	policy, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("invalid configuration: codex.turn_sandbox_policy must be an object")
	}
	kind, ok := policy["type"].(string)
	if !ok {
		return nil, errors.New("invalid configuration: codex.turn_sandbox_policy.type must be a string")
	}
	fields, known := sandboxPolicyFields[kind]
	if !known {
		return nil, fmt.Errorf("invalid configuration: codex.turn_sandbox_policy.type must be one of %s, got %q", strings.Join(sortedKeys(sandboxPolicyFields), ", "), kind)
	}
	if _, exists := policy["writableRoots"]; exists {
		return nil, errors.New("invalid configuration: codex.turn_sandbox_policy.writableRoots must not be configured; Symphony grants the workspace plus only the narrow Git metadata roots a local commit needs")
	}
	var unsupported []string
	for key := range policy {
		if key == "type" {
			continue
		}
		if _, ok := fields[key]; !ok {
			unsupported = append(unsupported, fmt.Sprintf("%q", key))
		}
	}
	if len(unsupported) > 0 {
		sort.Strings(unsupported)
		return nil, fmt.Errorf("invalid configuration: codex.turn_sandbox_policy does not support %s for type %q", strings.Join(unsupported, ", "), kind)
	}
	for _, key := range sortedKeys(fields) {
		value, exists := policy[key]
		if !exists {
			continue
		}
		switch fields[key] {
		case sandboxFieldBool:
			if _, ok := value.(bool); !ok {
				return nil, fmt.Errorf("invalid configuration: codex.turn_sandbox_policy.%s must be a boolean for type %q", key, kind)
			}
		case sandboxFieldNetworkAccess:
			access, ok := value.(string)
			if !ok || (access != "restricted" && access != "enabled") {
				return nil, fmt.Errorf("invalid configuration: codex.turn_sandbox_policy.%s must be \"restricted\" or \"enabled\" for type %q", key, kind)
			}
		}
	}
	if modes := sandboxPolicyThreadModes[kind]; !contains(modes, threadSandbox) {
		return nil, fmt.Errorf("invalid configuration: codex.turn_sandbox_policy.type %q overrides the thread sandbox and requires codex.thread_sandbox to be one of %s, got %q", kind, strings.Join(modes, ", "), threadSandbox)
	}
	return policy, nil
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for key := range m {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
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

func newSourceSnapshot(workflow []byte, overlay map[string]string) *sourceSnapshot {
	environment := make(map[string]string)
	for _, assignment := range os.Environ() {
		name, value, found := strings.Cut(assignment, "=")
		if found {
			environment[name] = value
		}
	}
	for name, value := range overlay {
		environment[name] = value
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
