// Package config loads and validates the repository-owned WORKFLOW.md contract.
package config

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

const fallbackPrompt = "Work on {{.issue.identifier}}: {{.issue.title}}\n\n{{.issue.description}}"

type Workflow struct {
	// Raw preserves the complete front-matter object, including extension keys.
	Raw    map[string]any
	Prompt string
	Config Settings
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

// decode validates one front-matter object into Settings. Each top-level
// block is delegated to its own per-section decoder; what remains here is
// composing their results and the validation that genuinely spans more than
// one section (which backend's launch contract must be complete, and the
// github.merge_state / tracker.provider.handoff_state pairing).
func decode(raw map[string]any, base, path, logRoot string, sources *sourceSnapshot) (Settings, error) {
	tracker, rawProvider, warnings, err := decodeTracker(raw, base, sources)
	if err != nil {
		return Settings{}, err
	}
	github, githubObjectValid := githubBlock(raw)
	landing, err := githubLandingPolicy(github, tracker.ActiveStates, tracker.TerminalStates, tracker.HandoffState)
	if err != nil {
		return Settings{}, err
	}
	polling, err := decodePolling(raw)
	if err != nil {
		return Settings{}, err
	}
	workspace, err := decodeWorkspace(raw, base, sources)
	if err != nil {
		return Settings{}, err
	}
	hooks, err := decodeHooks(raw)
	if err != nil {
		return Settings{}, err
	}
	agent, err := decodeAgent(raw)
	if err != nil {
		return Settings{}, err
	}
	codex, err := decodeCodex(raw)
	if err != nil {
		return Settings{}, err
	}
	claude, err := decodeClaude(raw)
	if err != nil {
		return Settings{}, err
	}
	decodedGitHub, githubWarnings := decodeGitHub(github, githubObjectValid, base, sources)
	warnings = append(warnings, githubWarnings...)
	githubSettings := applyLandingPolicy(decodedGitHub, landing)
	if githubSettings.MergeState != "" && !githubSettings.Enabled {
		return Settings{}, errors.New("invalid configuration: github.merge_state requires a fully configured github integration")
	}

	s := Settings{
		WorkflowPath: path,
		LogRoot:      normalizePath(logRootOrDefault(logRoot), base),
		Tracker:      tracker,
		Polling:      polling,
		Workspace:    workspace,
		Hooks:        hooks,
		Claude:       claude,
		Agent:        agent,
		Codex:        codex,
		GitHub:       githubSettings,
		// Keep only the names of environment variables that carry host
		// credentials. The Codex launcher uses this metadata to prevent those
		// variables from crossing the child-process boundary.
		HostSecretEnvNames: hostSecretEnvNames(rawProvider, github),
		HostSecretValues:   hostSecretValues(tracker.Provider, github, base, sources),
		Warnings:           warnings,
	}
	return validateSettings(s)
}

// validateSettings holds the validation that genuinely spans more than one
// section decoder: it cannot run until every section above has decoded, and
// none of it belongs to any one of them.
func validateSettings(s Settings) (Settings, error) {
	if s.Tracker.Kind != "linear" {
		return s, fmt.Errorf("invalid configuration: tracker.kind must be linear")
	}
	if len(s.Tracker.ActiveStates) == 0 || len(s.Tracker.TerminalStates) == 0 {
		return s, errors.New("invalid configuration: tracker active_states and terminal_states are required")
	}
	// The coordinator's admission check reads the two lists through independent
	// predicates (active and issueTerminal), so a state named in both makes one
	// issue a dispatch candidate and a terminal stop-and-clean-up target at the
	// same time, and the two act on it in turn on every poll. Neither predicate
	// can resolve that on its own, so the overlap is refused here -- compared
	// through the same Norm both of them match with, so what is refused is
	// exactly what they would collide over.
	for _, active := range s.Tracker.ActiveStates {
		for _, terminal := range s.Tracker.TerminalStates {
			if Norm(active) == Norm(terminal) {
				return s, fmt.Errorf("invalid configuration: tracker active_states %q and terminal_states %q name the same state; the two lists must be disjoint", active, terminal)
			}
		}
	}
	if s.Polling.Interval <= 0 || s.Hooks.Timeout <= 0 || s.Agent.MaxConcurrent <= 0 || s.Agent.MaxTurns <= 0 || s.Agent.MaxAttempts <= 0 || s.Agent.MaxRetryBackoff <= 0 {
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
		// Two residual configuration rules, both refused only for this backend.
		//
		// Rule one: an enabled github integration requires handoff_state. Without
		// one, the configuration either grants nothing or -- with follow-up issues
		// on -- advertises github_publish_pr while the rendered guidance says
		// publishing is unavailable. A worker acting on the advertised tool reaches
		// LinkAndHandoff with an empty targetStateID, which comments the pull
		// request onto the issue and then transitions it to no state at all, so the
		// refusal arrives after an irreversible GitHub mutation.
		//
		// Rule two: a configured capability that no session could advertise is
		// refused rather than silently degraded.
		//
		// Both stay accepted under codex, where they behave identically and always
		// have. docs/architecture.md's opening section states why narrowing them
		// there would reject workflows already in the field, and what "no MCP server
		// at all" in the init echo is allowed to mean under claude.
		if s.GitHub.Enabled && strings.TrimSpace(s.Tracker.HandoffState) == "" {
			return s, errors.New("invalid configuration: agent.backend claude requires tracker.provider.handoff_state for an enabled github integration: without it the scoped publish capability either cannot be prepared at all or is advertised while the run is told host-side publishing is unavailable")
		}
		if (s.LinearSessionCapabilityEnabled() || s.GitHub.Enabled) && !s.SessionCapabilityAdvertisable() {
			return s, errors.New("invalid configuration: agent.backend claude configures a Symphony session capability that no session could advertise: pair tracker.provider.handoff_state with an enabled github integration, or enable tracker.provider.followup_issue_creation")
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

func decodePolling(raw map[string]any) (Polling, error) {
	polling, err := object(raw, "polling")
	if err != nil {
		return Polling{}, err
	}
	interval, err := durationMS(polling, "interval_ms", 30_000)
	if err != nil {
		return Polling{}, err
	}
	return Polling{Interval: interval}, nil
}

func decodeWorkspace(raw map[string]any, base string, sources *sourceSnapshot) (Workspace, error) {
	workspace, err := object(raw, "workspace")
	if err != nil {
		return Workspace{}, err
	}
	root, err := pathValue(workspace, "root", filepath.Join(os.TempDir(), "symphony_workspaces"), base, sources)
	if err != nil {
		return Workspace{}, err
	}
	sourceRoot, err := optionalPathValue(workspace, "source_root", base, sources)
	if err != nil {
		return Workspace{}, err
	}
	return Workspace{Root: root, SourceRoot: sourceRoot}, nil
}

func decodeHooks(raw map[string]any) (Hooks, error) {
	hooks, err := object(raw, "hooks")
	if err != nil {
		return Hooks{}, err
	}
	timeout, err := durationMS(hooks, "timeout_ms", 60_000)
	if err != nil {
		return Hooks{}, err
	}
	afterCreate, err := script(hooks, "after_create")
	if err != nil {
		return Hooks{}, err
	}
	beforeRun, err := script(hooks, "before_run")
	if err != nil {
		return Hooks{}, err
	}
	afterRun, err := script(hooks, "after_run")
	if err != nil {
		return Hooks{}, err
	}
	beforeRemove, err := script(hooks, "before_remove")
	if err != nil {
		return Hooks{}, err
	}
	return Hooks{AfterCreate: afterCreate, BeforeRun: beforeRun, AfterRun: afterRun, BeforeRemove: beforeRemove, Timeout: timeout}, nil
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
	w.Raw = CloneMap(w.Raw)
	w.Config.Tracker.Provider = CloneMap(w.Config.Tracker.Provider)
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

// CloneMap deep-copies a decoded configuration map so a caller holding one
// cannot mutate the snapshot another caller reads.
func CloneMap(source map[string]any) map[string]any {
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
		return CloneMap(typed)
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
