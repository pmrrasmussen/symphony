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
	"text/template"
	"time"
	"unicode"

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
	Tracker      Tracker
	Polling      Polling
	Workspace    Workspace
	Hooks        Hooks
	Agent        Agent
	Codex        Codex
	WorkflowPath string
	LogRoot      string
	Prompt       string
}

type Tracker struct {
	Kind                                         string
	Provider                                     map[string]any
	RequiredLabels, ActiveStates, TerminalStates []string
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
	b, err := os.ReadFile(abs)
	if err != nil {
		return Workflow{}, fmt.Errorf("missing_workflow_file: %w", err)
	}
	raw, body, err := parse(b)
	if err != nil {
		return Workflow{}, err
	}
	s, err := decode(raw, filepath.Dir(abs), abs, logRoot)
	if err != nil {
		return Workflow{}, err
	}
	s.Prompt = strings.TrimSpace(body)
	if s.Prompt == "" {
		s.Prompt = fallbackPrompt
	}
	return Workflow{Raw: raw, Prompt: s.Prompt, Config: s}, nil
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

func decode(raw map[string]any, base, path, logRoot string) (Settings, error) {
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

	trackerKind, err := stringValue(tr, "kind")
	if err != nil {
		return Settings{}, err
	}
	requiredLabels, err := stringList(tr, "required_labels")
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
	resolvedProvider, err := resolveProvider(provider, base)
	if err != nil {
		return Settings{}, err
	}

	pollInterval, err := durationMS(polling, "interval_ms", 30_000)
	if err != nil {
		return Settings{}, err
	}
	workspaceRoot, err := pathValue(workspace, "root", filepath.Join(os.TempDir(), "symphony_workspaces"), base)
	if err != nil {
		return Settings{}, err
	}
	sourceRoot, err := optionalPathValue(workspace, "source_root", base)
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

	s := Settings{
		WorkflowPath: path,
		LogRoot:      normalizePath(logRootOrDefault(logRoot), base),
		Tracker: Tracker{
			Kind:           strings.TrimSpace(trackerKind),
			Provider:       resolvedProvider,
			RequiredLabels: stringsLower(requiredLabels),
			ActiveStates:   stringsLower(activeStates),
			TerminalStates: stringsLower(terminalStates),
		},
		Polling:   Polling{Interval: pollInterval},
		Workspace: Workspace{Root: workspaceRoot, SourceRoot: sourceRoot},
		Hooks:     Hooks{AfterCreate: afterCreate, BeforeRun: beforeRun, AfterRun: afterRun, BeforeRemove: beforeRemove, Timeout: hookTimeout},
		Agent:     Agent{MaxConcurrent: maxConcurrent, MaxTurns: maxTurns, MaxRetryBackoff: maxRetryBackoff, ByState: byState},
		Codex:     Codex{Command: command, ApprovalPolicy: approvalPolicy, ThreadSandbox: threadSandbox, TurnSandboxPolicy: codex["turn_sandbox_policy"], TurnTimeout: turnTimeout, ReadTimeout: readTimeout, StallTimeout: stallTimeout},
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
	path, logRoot string
	current       Workflow
	digest        [sha256.Size]byte
	rejected      [sha256.Size]byte
	hasRejected   bool
}

func NewStore(path, logRoot string) (*Store, error) {
	w, err := Load(path, logRoot)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(w.Config.WorkflowPath)
	if err != nil {
		return nil, err
	}
	return &Store{path: w.Config.WorkflowPath, logRoot: logRoot, current: w, digest: sha256.Sum256(b)}, nil
}

func (s *Store) Current() Workflow {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.current
}

// Reload retains the last valid workflow when the new file is malformed. A
// repeated invalid byte-for-byte version is ignored until it changes again.
func (s *Store) Reload() error {
	b, err := os.ReadFile(s.path)
	if err != nil {
		return fmt.Errorf("missing_workflow_file: %w", err)
	}
	digest := sha256.Sum256(b)
	s.mu.RLock()
	unchanged := digest == s.digest || (s.hasRejected && digest == s.rejected)
	s.mu.RUnlock()
	if unchanged {
		return nil
	}
	w, err := Load(s.path, s.logRoot)
	if err != nil {
		s.mu.Lock()
		s.rejected, s.hasRejected = digest, true
		s.mu.Unlock()
		return err
	}
	s.mu.Lock()
	s.current = w
	s.digest = digest
	s.hasRejected = false
	s.mu.Unlock()
	return nil
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

func resolveProvider(m map[string]any, base string) (map[string]any, error) {
	out := make(map[string]any, len(m)+1)
	for key, value := range m {
		out[key] = value
	}
	apiKey, hasAPIKey := out["api_key"]
	if hasAPIKey {
		if _, ok := apiKey.(string); !ok {
			return nil, errors.New("invalid configuration: tracker.provider.api_key must be a string")
		}
	}
	v, exists := out["api_key_file"]
	if exists {
		file, ok := v.(string)
		if !ok {
			return nil, errors.New("invalid configuration: tracker.provider.api_key_file must be a string")
		}
		file = resolveEnvReference(file)
		if strings.TrimSpace(file) == "" {
			return nil, errors.New("invalid linear api_key_file: empty path")
		}
		b, err := os.ReadFile(normalizePath(file, base))
		if err != nil {
			return nil, fmt.Errorf("invalid linear api_key_file: %w", err)
		}
		if value := strings.TrimSpace(string(b)); value == "" {
			return nil, errors.New("invalid linear api_key_file: empty secret")
		} else {
			// The explicitly configured secret file takes precedence over an
			// inline reference, including an unset inline $VAR reference.
			out["api_key"] = value
		}
		return out, nil
	}
	if !hasAPIKey {
		return out, nil
	}
	resolved := resolveEnvReference(apiKey.(string))
	if strings.TrimSpace(resolved) == "" {
		return nil, errors.New("invalid linear api_key: resolved secret is empty")
	}
	out["api_key"] = resolved
	return out, nil
}

func pathValue(m map[string]any, key, fallback, base string) (string, error) {
	v, exists := m[key]
	if !exists {
		return normalizePath(fallback, base), nil
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("invalid configuration: %s must be a path string", key)
	}
	s = resolveEnvReference(s)
	if strings.TrimSpace(s) == "" {
		return "", fmt.Errorf("invalid configuration: %s must not be empty", key)
	}
	return normalizePath(s, base), nil
}

func optionalPathValue(m map[string]any, key, base string) (string, error) {
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
	s = resolveEnvReference(s)
	if strings.TrimSpace(s) == "" {
		return "", nil
	}
	return normalizePath(s, base), nil
}

func resolveEnvReference(value string) string {
	if !strings.HasPrefix(value, "$") || len(value) == 1 {
		return value
	}
	name := value[1:]
	for index, r := range name {
		if !(r == '_' || unicode.IsLetter(r) || (index > 0 && unicode.IsDigit(r))) {
			return value
		}
	}
	return os.Getenv(name)
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
