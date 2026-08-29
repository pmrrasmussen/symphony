package config

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

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

// decodeAgent validates the agent: block, including the closed agent.backend
// enum. It is used regardless of which backend is selected: only the launch
// contract for the selected backend (Codex or Claude) is validated as
// complete, in decode's cross-section validation.
func decodeAgent(raw map[string]any) (Agent, error) {
	agent, err := object(raw, "agent")
	if err != nil {
		return Agent{}, err
	}
	maxConcurrent, err := integer(agent, "max_concurrent_agents", 10)
	if err != nil {
		return Agent{}, err
	}
	maxTurns, err := integer(agent, "max_turns", 20)
	if err != nil {
		return Agent{}, err
	}
	maxAttempts, err := integer(agent, "max_attempts", 5)
	if err != nil {
		return Agent{}, err
	}
	maxRetryBackoff, err := durationMS(agent, "max_retry_backoff_ms", 300_000)
	if err != nil {
		return Agent{}, err
	}
	byState, err := stateLimits(agent["max_concurrent_agents_by_state"])
	if err != nil {
		return Agent{}, err
	}
	backend, err := stringDefault(agent, "backend", DefaultAgentBackend)
	if err != nil {
		return Agent{}, err
	}
	if !contains(agentBackends, backend) {
		return Agent{}, fmt.Errorf("invalid configuration: agent.backend must be one of %s, got %q", strings.Join(agentBackends, ", "), backend)
	}
	return Agent{Backend: backend, MaxConcurrent: maxConcurrent, MaxTurns: maxTurns, MaxAttempts: maxAttempts, MaxRetryBackoff: maxRetryBackoff, ByState: byState}, nil
}

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

func decodeClaude(raw map[string]any) (Claude, error) {
	block, err := object(raw, "claude")
	if err != nil {
		return Claude{}, err
	}
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

// codexKeys is the complete set of codex block keys, refused if unknown for the
// same reason claudeKeys are: `stall_timout_ms` would otherwise load clean and
// run the session on the 300s default. An unknown *top-level* workflow key is
// still an extension key kept in Workflow.Raw, but inside a block this package
// validates in full there is nothing an unknown key can be except a typo.
var codexKeys = []string{"command", "approval_policy", "thread_sandbox", "turn_sandbox_policy", "turn_timeout_ms", "read_timeout_ms", "start_timeout_ms", "stall_timeout_ms"}

// decodeCodex validates the codex: block: the launch command, approval
// policy, thread sandbox mode, optional per-turn sandbox policy override, and
// the four launch timeouts.
func decodeCodex(raw map[string]any) (Codex, error) {
	codex, err := object(raw, "codex")
	if err != nil {
		return Codex{}, err
	}
	for key := range codex {
		if !contains(codexKeys, key) {
			return Codex{}, fmt.Errorf("invalid configuration: unknown codex field %q", key)
		}
	}
	command, err := stringDefault(codex, "command", "codex app-server")
	if err != nil {
		return Codex{}, err
	}
	approvalPolicy, err := stringDefault(codex, "approval_policy", "never")
	if err != nil {
		return Codex{}, err
	}
	threadSandbox, err := stringDefault(codex, "thread_sandbox", "workspace-write")
	if err != nil {
		return Codex{}, err
	}
	if !contains(sandboxModes, threadSandbox) {
		return Codex{}, fmt.Errorf("invalid configuration: codex.thread_sandbox must be one of %s, got %q", strings.Join(sandboxModes, ", "), threadSandbox)
	}
	turnSandboxPolicy, err := sandboxPolicy(codex, threadSandbox)
	if err != nil {
		return Codex{}, err
	}
	turnTimeout, err := durationMS(codex, "turn_timeout_ms", 3_600_000)
	if err != nil {
		return Codex{}, err
	}
	readTimeout, err := durationMS(codex, "read_timeout_ms", 5_000)
	if err != nil {
		return Codex{}, err
	}
	// A cold codex app-server start (process spawn plus first model load) far
	// exceeds the small steady-state read_timeout_ms. This budget governs only
	// the initial handshake and thread/start; 120s gives generous headroom
	// above the ~60s that survived a real cold start live (PMR-57).
	startTimeout, err := durationMS(codex, "start_timeout_ms", 120_000)
	if err != nil {
		return Codex{}, err
	}
	stallTimeout, err := durationMS(codex, "stall_timeout_ms", 300_000)
	if err != nil {
		return Codex{}, err
	}
	return Codex{
		Command: command, ApprovalPolicy: approvalPolicy, ThreadSandbox: threadSandbox,
		TurnSandboxPolicy: turnSandboxPolicy, TurnTimeout: turnTimeout, ReadTimeout: readTimeout,
		StartTimeout: startTimeout, StallTimeout: stallTimeout,
	}, nil
}
