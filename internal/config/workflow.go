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

	"gopkg.in/yaml.v3"
)

type Workflow struct {
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
type Workspace struct {
	Root, SourceRoot string
}
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

func Load(path, logRoot string) (Workflow, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return Workflow{}, err
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
	if _, err := template.New("workflow").Option("missingkey=error").Parse(body); err != nil {
		return Workflow{}, fmt.Errorf("template_parse_error: %w", err)
	}
	s.Prompt = strings.TrimSpace(body)
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
	var raw map[string]any
	if err := yaml.Unmarshal([]byte(strings.Join(lines[1:end], "\n")), &raw); err != nil {
		return nil, "", fmt.Errorf("workflow_parse_error: %w", err)
	}
	if raw == nil {
		raw = map[string]any{}
	}
	return raw, strings.Join(lines[end+1:], "\n"), nil
}
func decode(raw map[string]any, base, path, logRoot string) (Settings, error) {
	getMap := func(k string) map[string]any {
		if m, ok := raw[k].(map[string]any); ok {
			return m
		}
		return map[string]any{}
	}
	tr := getMap("tracker")
	pr := getMapFrom(tr, "provider")
	p := getMap("polling")
	w := getMap("workspace")
	h := getMap("hooks")
	a := getMap("agent")
	c := getMap("codex")
	provider, err := resolveProvider(pr, base)
	if err != nil {
		return Settings{}, err
	}
	sourceRoot := str(w["source_root"])
	if sourceRoot != "" {
		sourceRoot = normalizePath(expandEnv(sourceRoot), base)
	}
	s := Settings{WorkflowPath: path, LogRoot: normalizePath(defaultString(logRoot, "./.symphony/logs"), base), Tracker: Tracker{Kind: str(tr["kind"]), Provider: provider, RequiredLabels: stringsLower(list(tr["required_labels"])), ActiveStates: stringsLower(list(tr["active_states"])), TerminalStates: stringsLower(list(tr["terminal_states"]))}, Polling: Polling{Interval: ms(p["interval_ms"], 30000)}, Workspace: Workspace{Root: normalizePath(expandEnv(defaultString(str(w["root"]), "/symphony_workspaces")), base), SourceRoot: sourceRoot}, Hooks: Hooks{AfterCreate: str(h["after_create"]), BeforeRun: str(h["before_run"]), AfterRun: str(h["after_run"]), BeforeRemove: str(h["before_remove"]), Timeout: ms(h["timeout_ms"], 60000)}, Agent: Agent{MaxConcurrent: num(a["max_concurrent_agents"], 10), MaxTurns: num(a["max_turns"], 20), MaxRetryBackoff: ms(a["max_retry_backoff_ms"], 300000), ByState: stateLimits(a["max_concurrent_agents_by_state"])}, Codex: Codex{Command: defaultString(str(c["command"]), "codex app-server"), ApprovalPolicy: defaultString(str(c["approval_policy"]), "never"), ThreadSandbox: defaultString(str(c["thread_sandbox"]), "workspace-write"), TurnSandboxPolicy: c["turn_sandbox_policy"], TurnTimeout: ms(c["turn_timeout_ms"], 3600000), ReadTimeout: ms(c["read_timeout_ms"], 5000), StallTimeout: ms(c["stall_timeout_ms"], 300000)}}
	if s.Tracker.Kind != "linear" {
		return s, fmt.Errorf("invalid configuration: tracker.kind must be linear")
	}
	if len(s.Tracker.ActiveStates) == 0 || len(s.Tracker.TerminalStates) == 0 {
		return s, errors.New("invalid configuration: tracker active_states and terminal_states are required")
	}
	if s.Polling.Interval <= 0 || s.Hooks.Timeout <= 0 || s.Agent.MaxConcurrent <= 0 || s.Agent.MaxTurns <= 0 || s.Agent.MaxRetryBackoff <= 0 || s.Codex.Command == "" || s.Codex.TurnTimeout <= 0 || s.Codex.ReadTimeout <= 0 {
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
func (s Settings) Render(issue any, attempt int) (string, error) {
	var out bytes.Buffer
	err := template.Must(template.New("workflow").Option("missingkey=error").Parse(s.Prompt)).Execute(&out, map[string]any{"Issue": issue, "Attempt": attempt})
	if err != nil {
		return "", fmt.Errorf("template_render_error: %w", err)
	}
	return out.String(), nil
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
	w, e := Load(path, logRoot)
	if e != nil {
		return nil, e
	}
	b, e := os.ReadFile(w.Config.WorkflowPath)
	if e != nil {
		return nil, e
	}
	return &Store{path: path, logRoot: logRoot, current: w, digest: sha256.Sum256(b)}, nil
}
func (s *Store) Current() Workflow { s.mu.RLock(); defer s.mu.RUnlock(); return s.current }
func (s *Store) Reload() error {
	b, e := os.ReadFile(s.path)
	if e != nil {
		return e
	}
	digest := sha256.Sum256(b)
	s.mu.RLock()
	unchanged := digest == s.digest || (s.hasRejected && digest == s.rejected)
	s.mu.RUnlock()
	if unchanged {
		return nil
	}
	w, e := Load(s.path, s.logRoot)
	if e != nil {
		s.mu.Lock()
		s.rejected, s.hasRejected = digest, true
		s.mu.Unlock()
		return e
	}
	s.mu.Lock()
	s.current = w
	s.digest = digest
	s.hasRejected = false
	s.mu.Unlock()
	return nil
}
func getMapFrom(m map[string]any, k string) map[string]any {
	if x, ok := m[k].(map[string]any); ok {
		return x
	}
	return map[string]any{}
}
func str(v any) string { x, _ := v.(string); return strings.TrimSpace(x) }
func defaultString(v, d string) string {
	if v == "" {
		return d
	}
	return v
}
func num(v any, d int) int {
	switch x := v.(type) {
	case int:
		return x
	case int64:
		return int(x)
	case float64:
		return int(x)
	}
	return d
}
func ms(v any, d int) time.Duration { return time.Duration(num(v, d)) * time.Millisecond }
func list(v any) []string {
	xs, _ := v.([]any)
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		if y := str(x); y != "" {
			out = append(out, y)
		}
	}
	return out
}
func stringsLower(v []string) []string {
	for i := range v {
		v[i] = strings.ToLower(strings.TrimSpace(v[i]))
	}
	return v
}
func stateLimits(v any) map[string]int {
	out := map[string]int{}
	m, _ := v.(map[string]any)
	for k, x := range m {
		if n := num(x, 0); n > 0 {
			out[strings.ToLower(strings.TrimSpace(k))] = n
		}
	}
	return out
}
func expandEnv(v string) string {
	if strings.HasPrefix(v, "$") && len(v) > 1 {
		return os.Getenv(strings.TrimPrefix(v, "$"))
	}
	return os.ExpandEnv(v)
}
func resolveMap(m map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		if x, ok := v.(string); ok {
			out[k] = expandEnv(x)
		} else {
			out[k] = v
		}
	}
	return out
}
func resolveProvider(m map[string]any, base string) (map[string]any, error) {
	out := resolveMap(m)
	file, _ := out["api_key_file"].(string)
	if file == "" {
		return out, nil
	}
	b, err := os.ReadFile(normalizePath(expandEnv(file), base))
	if err != nil {
		return nil, fmt.Errorf("invalid linear api_key_file: %w", err)
	}
	if value := strings.TrimSpace(string(b)); value == "" {
		return nil, errors.New("invalid linear api_key_file: empty secret")
	} else {
		out["api_key"] = value
	}
	return out, nil
}
func normalizePath(v, base string) string {
	if strings.HasPrefix(v, "~/") {
		h, _ := os.UserHomeDir()
		v = filepath.Join(h, v[2:])
	}
	if !filepath.IsAbs(v) {
		v = filepath.Join(base, v)
	}
	x, _ := filepath.Abs(filepath.Clean(v))
	return x
}
