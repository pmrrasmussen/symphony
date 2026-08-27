package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCodexStartTimeoutDefaultsGenerouslyAndParsesIndependently(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "WORKFLOW.md")
	defaults := "---\ntracker: {kind: linear, active_states: [Todo], terminal_states: [Done]}\n---\nprompt"
	if err := os.WriteFile(p, []byte(defaults), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := Load(p, "")
	if err != nil {
		t.Fatal(err)
	}
	if w.Config.Codex.StartTimeout != 120_000*time.Millisecond {
		t.Fatalf("default start timeout=%v want 120s", w.Config.Codex.StartTimeout)
	}
	if w.Config.Codex.ReadTimeout != 5_000*time.Millisecond {
		t.Fatalf("default read timeout=%v want 5s", w.Config.Codex.ReadTimeout)
	}
	if w.Config.Codex.StartTimeout <= w.Config.Codex.ReadTimeout {
		t.Fatalf("start timeout %v must exceed read timeout %v", w.Config.Codex.StartTimeout, w.Config.Codex.ReadTimeout)
	}
	explicit := "---\ntracker: {kind: linear, active_states: [Todo], terminal_states: [Done]}\ncodex: {read_timeout_ms: 5000, start_timeout_ms: 90000}\n---\nprompt"
	if err := os.WriteFile(p, []byte(explicit), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err = Load(p, "")
	if err != nil {
		t.Fatal(err)
	}
	if w.Config.Codex.StartTimeout != 90_000*time.Millisecond || w.Config.Codex.ReadTimeout != 5_000*time.Millisecond {
		t.Fatalf("explicit start=%v read=%v", w.Config.Codex.StartTimeout, w.Config.Codex.ReadTimeout)
	}
}

func TestRepositoryWorkflowGrantsLoopbackWithinWorkspaceWrite(t *testing.T) {
	dir := t.TempDir()
	for variable, name := range map[string]string{"SYMPHONY_LINEAR_API_KEY_FILE": "linear-key", "SYMPHONY_GITHUB_TOKEN_FILE": "github-token"} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("test-secret"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv(variable, path)
	}
	w, err := Load(filepath.Join("..", "..", "WORKFLOW.md"), "")
	if err != nil {
		t.Fatal(err)
	}
	if w.Config.Codex.ThreadSandbox != "workspace-write" {
		t.Fatalf("thread sandbox=%q want workspace-write", w.Config.Codex.ThreadSandbox)
	}
	policy, ok := w.Config.Codex.TurnSandboxPolicy.(map[string]any)
	if !ok {
		t.Fatalf("turn sandbox policy type=%T want an object", w.Config.Codex.TurnSandboxPolicy)
	}
	if policy["type"] != "workspaceWrite" || policy["networkAccess"] != true {
		t.Fatalf("turn sandbox policy=%#v want workspaceWrite with networkAccess enabled", policy)
	}
	// Network access must not come bundled with broader filesystem authority:
	// the launcher owns writableRoots and grants only the narrowed Git roots.
	if roots, exists := policy["writableRoots"]; exists {
		t.Fatalf("canonical workflow configures writable roots %#v; filesystem authority must stay with the launcher's narrowed grant", roots)
	}
}

// TestTurnSandboxPolicyShapeIsValidated covers the shapes Codex would either
// reject at turn/start on every dispatch or, worse, silently accept with a
// field it ignores. The field sets come from the app-server SandboxPolicy
// schema (codex-cli 0.149.1).
func TestTurnSandboxPolicyShapeIsValidated(t *testing.T) {
	for name, test := range map[string]struct{ codex, want string }{
		"not an object":   {"{turn_sandbox_policy: workspaceWrite}", "codex.turn_sandbox_policy must be an object"},
		"missing type":    {"{turn_sandbox_policy: {networkAccess: true}}", "codex.turn_sandbox_policy.type must be a string"},
		"non-string type": {"{turn_sandbox_policy: {type: 7}}", "codex.turn_sandbox_policy.type must be a string"},
		"blank type":      {"{turn_sandbox_policy: {type: '   '}}", "codex.turn_sandbox_policy.type must be one of"},
		"unknown type":    {"{turn_sandbox_policy: {type: sandboxed}}", "codex.turn_sandbox_policy.type must be one of"},
		// The kebab spelling thread_sandbox uses two lines above in the same
		// YAML block is not a SandboxPolicy type. Left unvalidated it passes
		// Load and --dry-run, then skips the narrowed Git grant and is refused
		// by the app-server, so every dispatch fails.
		"thread_sandbox spelling of the type":       {"{thread_sandbox: workspace-write, turn_sandbox_policy: {type: workspace-write}}", `codex.turn_sandbox_policy.type must be one of dangerFullAccess, externalSandbox, readOnly, workspaceWrite, got "workspace-write"`},
		"misspelled network access":                 {"{turn_sandbox_policy: {type: workspaceWrite, networkAcces: true}}", `codex.turn_sandbox_policy does not support "networkAcces" for type "workspaceWrite"`},
		"non-boolean network access":                {"{turn_sandbox_policy: {type: workspaceWrite, networkAccess: 'true'}}", "codex.turn_sandbox_policy.networkAccess must be a boolean"},
		"boolean network access on externalSandbox": {"{thread_sandbox: danger-full-access, turn_sandbox_policy: {type: externalSandbox, networkAccess: true}}", `codex.turn_sandbox_policy.networkAccess must be "restricted" or "enabled"`},
		"unknown network access enum value":         {"{thread_sandbox: danger-full-access, turn_sandbox_policy: {type: externalSandbox, networkAccess: unrestricted}}", `codex.turn_sandbox_policy.networkAccess must be "restricted" or "enabled"`},
		"network access on dangerFullAccess":        {"{thread_sandbox: danger-full-access, turn_sandbox_policy: {type: dangerFullAccess, networkAccess: true}}", `codex.turn_sandbox_policy does not support "networkAccess" for type "dangerFullAccess"`},
		"non-boolean tmp exclusion":                 {"{turn_sandbox_policy: {type: workspaceWrite, excludeSlashTmp: yes-please}}", "codex.turn_sandbox_policy.excludeSlashTmp must be a boolean"},
		// writableRoots is rejected outright rather than validated: the
		// launcher merges its narrowed roots into whatever is configured, so
		// even a well-formed absolute root widens write authority past what the
		// documentation promises. 'nope' is not even an array, and ['/'] is the
		// worst case -- write access to the whole filesystem.
		"unparseable writable roots":  {"{turn_sandbox_policy: {type: workspaceWrite, writableRoots: 'nope'}}", "codex.turn_sandbox_policy.writableRoots must not be configured"},
		"filesystem root as writable": {"{turn_sandbox_policy: {type: workspaceWrite, writableRoots: ['/']}}", "codex.turn_sandbox_policy.writableRoots must not be configured"},
		"relative writable root":      {"{turn_sandbox_policy: {type: workspaceWrite, writableRoots: ['../elsewhere']}}", "codex.turn_sandbox_policy.writableRoots must not be configured"},
		// The turn policy overrides the thread mode for this and every later
		// turn, so a mismatch silently escalates the session and skips the
		// narrowed Git grant the launcher applies only to workspace-write.
		"write authority the thread mode lacks":        {"{thread_sandbox: read-only, turn_sandbox_policy: {type: workspaceWrite, networkAccess: true}}", `requires codex.thread_sandbox to be one of workspace-write, got "read-only"`},
		"full access on a workspace-write thread":      {"{thread_sandbox: workspace-write, turn_sandbox_policy: {type: dangerFullAccess}}", `requires codex.thread_sandbox to be one of danger-full-access, got "workspace-write"`},
		"external sandbox on a workspace-write thread": {"{thread_sandbox: workspace-write, turn_sandbox_policy: {type: externalSandbox, networkAccess: enabled}}", `requires codex.thread_sandbox to be one of danger-full-access, got "workspace-write"`},
		"unknown thread sandbox mode":                  {"{thread_sandbox: workspaceWrite}", `codex.thread_sandbox must be one of read-only, workspace-write, danger-full-access, got "workspaceWrite"`},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("LINEAR_API_KEY", "secret")
			p := filepath.Join(t.TempDir(), "WORKFLOW.md")
			content := "---\ntracker: {kind: linear, provider: {api_key: $LINEAR_API_KEY}, active_states: [Todo], terminal_states: [Done]}\ncodex: " + test.codex + "\n---\nprompt"
			if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			w, err := Load(p, "")
			if err == nil {
				t.Fatalf("accepted invalid sandbox configuration as %#v", w.Config.Codex)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%q want it to contain %q", err, test.want)
			}
		})
	}
}

// TestValidTurnSandboxPolicyVariantsAreAccepted keeps the validator from
// narrowing past the Codex schema: every field the protocol accepts for a
// variant must still load, or a legitimate operator policy becomes
// unconfigurable.
func TestValidTurnSandboxPolicyVariantsAreAccepted(t *testing.T) {
	for name, codex := range map[string]string{
		"loopback-capable workspace write":           "{thread_sandbox: workspace-write, turn_sandbox_policy: {type: workspaceWrite, networkAccess: true}}",
		"workspace write with tmp exclusions":        "{thread_sandbox: workspace-write, turn_sandbox_policy: {type: workspaceWrite, networkAccess: false, excludeSlashTmp: true, excludeTmpdirEnvVar: true}}",
		"read-only turn on a workspace-write thread": "{thread_sandbox: workspace-write, turn_sandbox_policy: {type: readOnly, networkAccess: false}}",
		"external sandbox with the enum form":        "{thread_sandbox: danger-full-access, turn_sandbox_policy: {type: externalSandbox, networkAccess: enabled}}",
		"full access matching its thread mode":       "{thread_sandbox: danger-full-access, turn_sandbox_policy: {type: dangerFullAccess}}",
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("LINEAR_API_KEY", "secret")
			p := filepath.Join(t.TempDir(), "WORKFLOW.md")
			content := "---\ntracker: {kind: linear, provider: {api_key: $LINEAR_API_KEY}, active_states: [Todo], terminal_states: [Done]}\ncodex: " + codex + "\n---\nprompt"
			if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(p, ""); err != nil {
				t.Fatalf("rejected a schema-valid policy: %v", err)
			}
		})
	}
}

// TestOmittedTurnSandboxPolicyStaysNil keeps absence distinguishable from an
// empty object: a nil policy is what lets the launcher substitute its own
// narrowed workspace-write grant instead of forwarding a meaningless one.
func TestOmittedTurnSandboxPolicyStaysNil(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "secret")
	p := filepath.Join(t.TempDir(), "WORKFLOW.md")
	content := "---\ntracker: {kind: linear, provider: {api_key: $LINEAR_API_KEY}, active_states: [Todo], terminal_states: [Done]}\ncodex: {thread_sandbox: workspace-write}\n---\nprompt"
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := Load(p, "")
	if err != nil {
		t.Fatal(err)
	}
	if w.Config.Codex.TurnSandboxPolicy != nil {
		t.Fatalf("turn sandbox policy=%#v want nil when the key is omitted", w.Config.Codex.TurnSandboxPolicy)
	}
}

// TestAgentBackendDefaultsToCodexAndFailsClosed covers the selection field: an
// absent value must behave exactly as every workflow written before it existed,
// and an unknown or wrongly typed value must fail the whole candidate rather
// than fall back to a default.
func TestAgentBackendDefaultsToCodexAndFailsClosed(t *testing.T) {
	d := t.TempDir()
	write := func(t *testing.T, agentBlock string) string {
		t.Helper()
		path := filepath.Join(d, strings.ReplaceAll(t.Name(), "/", "_")+".md")
		body := "---\ntracker: {kind: linear, provider: {api_key: secret-key}, active_states: [Todo], terminal_states: [Done]}\npolling: {interval_ms: 100}\nworkspace: {root: work}\nhooks: {timeout_ms: 100}\nagent: {" + agentBlock + "}\ncodex: {command: codex app-server}\n---\nbody"
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	t.Run("absent defaults to codex", func(t *testing.T) {
		w, err := Load(write(t, "max_turns: 2"), "logs")
		if err != nil {
			t.Fatal(err)
		}
		if w.Config.Agent.Backend != "codex" {
			t.Fatalf("backend=%q, want codex", w.Config.Agent.Backend)
		}
	})

	t.Run("explicit codex is accepted", func(t *testing.T) {
		w, err := Load(write(t, "backend: codex"), "logs")
		if err != nil {
			t.Fatal(err)
		}
		if w.Config.Agent.Backend != "codex" {
			t.Fatalf("backend=%q", w.Config.Agent.Backend)
		}
	})

	for _, value := range []string{"Codex", "codex ", "", "docker", "Claude"} {
		t.Run("rejects "+value, func(t *testing.T) {
			_, err := Load(write(t, "backend: '"+value+"'"), "logs")
			if err == nil {
				t.Fatalf("backend %q was accepted", value)
			}
			if !strings.Contains(err.Error(), "invalid configuration: agent.backend must be one of codex") {
				t.Fatalf("error=%v", err)
			}
			// The rejection must name the offending value and nothing else from
			// the configuration.
			for _, leaked := range []string{"api_key", "secret-key", "Todo", "Done", "codex app-server", "/tmp/work"} {
				if strings.Contains(err.Error(), leaked) {
					t.Fatalf("error leaked configured value %q: %v", leaked, err)
				}
			}
		})
	}

	t.Run("rejects a non-string", func(t *testing.T) {
		if _, err := Load(write(t, "backend: 3"), "logs"); err == nil {
			t.Fatal("a non-string backend was accepted")
		}
	})
}

// TestAgentMaxAttemptsDefaultsAndIsValidatedLikeTheOtherAgentLimits covers the
// PMR-111 dispatch ceiling as a schema field: a workflow written before it
// existed still loads with a usable bound, and a value that would restore the
// unbounded retry loop (zero or negative) or is not an integer at all fails the
// whole candidate instead of silently disabling the ceiling.
func TestAgentMaxAttemptsDefaultsAndIsValidatedLikeTheOtherAgentLimits(t *testing.T) {
	d := t.TempDir()
	write := func(t *testing.T, agentBlock string) string {
		t.Helper()
		path := filepath.Join(d, strings.ReplaceAll(t.Name(), "/", "_")+".md")
		body := "---\ntracker: {kind: linear, provider: {api_key: secret-key}, active_states: [Todo], terminal_states: [Done]}\npolling: {interval_ms: 100}\nworkspace: {root: work}\nhooks: {timeout_ms: 100}\nagent: {" + agentBlock + "}\ncodex: {command: codex app-server}\n---\nbody"
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	t.Run("absent defaults to a bounded ceiling", func(t *testing.T) {
		w, err := Load(write(t, "max_turns: 2"), "logs")
		if err != nil {
			t.Fatal(err)
		}
		if w.Config.Agent.MaxAttempts != 5 {
			t.Fatalf("max_attempts=%d, want the default 5", w.Config.Agent.MaxAttempts)
		}
	})

	t.Run("an explicit value is loaded", func(t *testing.T) {
		w, err := Load(write(t, "max_attempts: 2"), "logs")
		if err != nil {
			t.Fatal(err)
		}
		if w.Config.Agent.MaxAttempts != 2 {
			t.Fatalf("max_attempts=%d", w.Config.Agent.MaxAttempts)
		}
	})

	for _, value := range []string{"0", "-1"} {
		t.Run("rejects "+value, func(t *testing.T) {
			_, err := Load(write(t, "max_attempts: "+value), "logs")
			if err == nil {
				t.Fatalf("max_attempts %s was accepted", value)
			}
			if !strings.Contains(err.Error(), "non-positive duration or agent limit") {
				t.Fatalf("error=%v", err)
			}
		})
	}

	t.Run("rejects a non-integer", func(t *testing.T) {
		_, err := Load(write(t, "max_attempts: many"), "logs")
		if err == nil {
			t.Fatal("a non-integer max_attempts was accepted")
		}
		if !strings.Contains(err.Error(), "max_attempts must be an integer") {
			t.Fatalf("error=%v", err)
		}
	})
}

// TestAgentLaunchResolvesTheSelectedBackendsContract pins the neutral accessor
// coordination and preflight read instead of a backend's own settings block.
func TestAgentLaunchResolvesTheSelectedBackendsContract(t *testing.T) {
	s := Settings{Codex: Codex{
		Command: "codex app-server", ApprovalPolicy: "never", ThreadSandbox: "workspace-write",
		TurnSandboxPolicy: map[string]any{"type": "workspaceWrite"},
		TurnTimeout:       time.Hour, ReadTimeout: 5 * time.Second,
		StartTimeout: 2 * time.Minute, StallTimeout: 5 * time.Minute,
	}}
	s.Agent.Backend = "codex"
	launch := s.AgentLaunch()
	if launch.Backend != "codex" || launch.Command != "codex app-server" || launch.ApprovalPolicy != "never" || launch.ThreadSandbox != "workspace-write" {
		t.Fatalf("launch=%+v", launch)
	}
	// All four timeout budgets must be routed, not three: start_timeout_ms is
	// distinct from read_timeout_ms on purpose.
	if launch.TurnTimeout != time.Hour || launch.ReadTimeout != 5*time.Second || launch.StartTimeout != 2*time.Minute || launch.StallTimeout != 5*time.Minute {
		t.Fatalf("timeouts=%+v", launch)
	}
	if policy, ok := launch.TurnSandboxPolicy.(map[string]any); !ok || policy["type"] != "workspaceWrite" {
		t.Fatalf("turn sandbox policy=%#v", launch.TurnSandboxPolicy)
	}

	// An unset selection resolves as codex, so a pre-existing workflow keeps its
	// exact launch contract.
	unset := s
	unset.Agent.Backend = ""
	// AgentLaunch carries an interface-typed sandbox policy, so compare the
	// comparable fields rather than the struct: == on a launch holding a map
	// panics at run time.
	got, want := unset.AgentLaunch(), s.AgentLaunch()
	if got.Backend != want.Backend || got.Command != want.Command || got.ApprovalPolicy != want.ApprovalPolicy ||
		got.ThreadSandbox != want.ThreadSandbox || got.TurnTimeout != want.TurnTimeout ||
		got.ReadTimeout != want.ReadTimeout || got.StartTimeout != want.StartTimeout || got.StallTimeout != want.StallTimeout {
		t.Fatalf("unset backend resolved differently: %+v", got)
	}

	// An unknown name yields no launch parameters rather than another backend's,
	// which is what makes a stale or wrong lookup fail loudly instead of running
	// something unintended.
	unknown, known := s.AgentLaunchFor("docker")
	if known {
		t.Fatal("an unknown backend reported a known launch contract")
	}
	if unknown.Command != "" || unknown.TurnTimeout != 0 || unknown.StallTimeout != 0 || unknown.Backend != "docker" {
		t.Fatalf("unknown backend launch=%+v", unknown)
	}
	if _, known := s.AgentLaunchFor(""); !known {
		t.Fatal("an unset backend must resolve to the default contract")
	}
}

// TestClaudeBackendConfiguration covers the claude block: its defaults, that a
// typo is refused rather than silently defaulted, that only the selected
// backend's launch contract has to be complete, and the residual capability
// rule -- a Claude workflow may enable a Symphony session capability, but not one
// no session could ever advertise.
func TestClaudeBackendConfiguration(t *testing.T) {
	d := t.TempDir()
	write := func(t *testing.T, body string) string {
		t.Helper()
		path := filepath.Join(d, strings.ReplaceAll(t.Name(), "/", "_")+".md")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	const head = "---\ntracker: {kind: linear, provider: {api_key: k}, active_states: [Todo], terminal_states: [Done]}\npolling: {interval_ms: 100}\nworkspace: {root: work}\nhooks: {timeout_ms: 100}\n"

	t.Run("defaults and no codex block required", func(t *testing.T) {
		// A Claude workflow omits codex entirely; the codex requirements must not
		// reject it.
		w, err := Load(write(t, head+"agent: {backend: claude}\n---\nbody"), "logs")
		if err != nil {
			t.Fatal(err)
		}
		if w.Config.Claude.Command != "claude" || w.Config.Claude.Model != "" {
			t.Fatalf("claude=%+v", w.Config.Claude)
		}
		if w.Config.Claude.TurnTimeout != time.Hour || w.Config.Claude.StallTimeout != 5*time.Minute {
			t.Fatalf("claude timeouts=%+v", w.Config.Claude)
		}
		launch := w.Config.AgentLaunch()
		if launch.Backend != "claude" || launch.Command != "claude" || launch.TurnTimeout != time.Hour || launch.StallTimeout != 5*time.Minute {
			t.Fatalf("launch=%+v", launch)
		}
		// Codex-only launch fields have no Claude analogue and must stay empty
		// rather than leaking another backend's values.
		if launch.ApprovalPolicy != "" || launch.ThreadSandbox != "" || launch.TurnSandboxPolicy != nil || launch.ReadTimeout != 0 || launch.StartTimeout != 0 {
			t.Fatalf("claude launch carried codex fields: %+v", launch)
		}
	})

	t.Run("explicit values", func(t *testing.T) {
		w, err := Load(write(t, head+"agent: {backend: claude}\nclaude: {command: claude-next, model: sonnet, turn_timeout_ms: 1000, stall_timeout_ms: 200}\n---\nbody"), "logs")
		if err != nil {
			t.Fatal(err)
		}
		if w.Config.Claude != (Claude{Command: "claude-next", Model: "sonnet", TurnTimeout: time.Second, StallTimeout: 200 * time.Millisecond}) {
			t.Fatalf("claude=%+v", w.Config.Claude)
		}
	})

	t.Run("unknown claude field is refused", func(t *testing.T) {
		_, err := Load(write(t, head+"agent: {backend: claude}\nclaude: {commnad: claude}\n---\nbody"), "logs")
		if err == nil || !strings.Contains(err.Error(), `unknown claude field "commnad"`) {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("empty command is refused", func(t *testing.T) {
		if _, err := Load(write(t, head+"agent: {backend: claude}\nclaude: {command: '  '}\n---\nbody"), "logs"); err == nil {
			t.Fatal("a blank claude command was accepted")
		}
	})

	// The refusal these two subtests replace was blanket: any session capability
	// with agent.backend claude. What is left is the residual rule, so the
	// accepted and refused halves are asserted separately and by the same route.
	t.Run("a capability a session can advertise is accepted", func(t *testing.T) {
		t.Setenv("PMR52_GITHUB_TOKEN", "github-secret")
		for name, front := range map[string]string{
			"follow-up issues": "tracker: {kind: linear, provider: {api_key: k, followup_issue_creation: true}, active_states: [Todo], terminal_states: [Done]}\n",
			"host-side publish": "tracker: {kind: linear, provider: {api_key: k, handoff_state: In Review}, active_states: [Todo], terminal_states: [Done]}\n" +
				"github: {owner: pmrrasmussen, repository: symphony, token: $PMR52_GITHUB_TOKEN}\n",
		} {
			t.Run(name, func(t *testing.T) {
				body := "---\n" + front + "polling: {interval_ms: 100}\nworkspace: {root: work}\nhooks: {timeout_ms: 100}\nagent: {backend: claude}\n---\nbody"
				w, err := Load(write(t, body), "logs")
				if err != nil {
					t.Fatalf("a Claude workflow with a reachable capability was rejected: %v", err)
				}
				if !w.Config.SessionCapabilityAdvertisable() {
					t.Fatalf("accepted a capability no session could advertise: %+v", w.Config.Tracker)
				}
			})
		}
	})

	t.Run("a capability no session could advertise is refused", func(t *testing.T) {
		// After the handoff_state rule below, the only way left to write one: the
		// handoff object is prepared and nothing model-facing uses it.
		body := "---\ntracker: {kind: linear, provider: {api_key: k, handoff_state: In Review}, active_states: [Todo], terminal_states: [Done]}\n" +
			"polling: {interval_ms: 100}\nworkspace: {root: work}\nhooks: {timeout_ms: 100}\nagent: {backend: claude}\n---\nbody"
		_, err := Load(write(t, body), "logs")
		if err == nil || !strings.Contains(err.Error(), "configures a Symphony session capability that no session could advertise") {
			t.Fatalf("err=%v", err)
		}
	})

	// TestClaudeBackendConfiguration's other subtests describe configurations that
	// grant the model nothing. This one describes the opposite, and is the reason
	// the rule is unconditional rather than a special case of "advertises
	// nothing": with follow-up issues on, followup_issue_creation alone satisfies
	// LinearSessionCapabilityEnabled, so a Linear handoff session exists, so a
	// GitHub session is built on top of it, so github_publish_pr IS advertised --
	// while DeliveryInstructions branches on HostSidePublishPromised and tells the
	// run that publishing is unavailable. A worker that believes its tool list
	// over the prompt reaches LinkAndHandoff with no target state, which comments
	// the pull request onto the issue and then transitions it to nothing. The
	// refusal would arrive after the pull request exists.
	t.Run("an enabled github integration requires a handoff state", func(t *testing.T) {
		t.Setenv("PMR52_GITHUB_TOKEN", "github-secret")
		for name, provider := range map[string]string{
			"no other capability":      "{api_key: k}",
			"with follow-up issues on": "{api_key: k, followup_issue_creation: true}",
		} {
			t.Run(name, func(t *testing.T) {
				body := "---\ntracker: {kind: linear, provider: " + provider + ", active_states: [Todo], terminal_states: [Done]}\n" +
					"github: {owner: pmrrasmussen, repository: symphony, token: $PMR52_GITHUB_TOKEN}\n" +
					"polling: {interval_ms: 100}\nworkspace: {root: work}\nhooks: {timeout_ms: 100}\nagent: {backend: claude}\n---\nbody"
				_, err := Load(write(t, body), "logs")
				if err == nil || !strings.Contains(err.Error(), "requires tracker.provider.handoff_state for an enabled github integration") {
					t.Fatalf("err=%v", err)
				}
			})
		}
	})

	// The same configuration stays valid for codex. The prompt/advertisement
	// mismatch above is pre-existing there and is deliberately not fixed by this
	// rule: narrowing codex would reject workflows already in the field.
	t.Run("codex still accepts github without a handoff state", func(t *testing.T) {
		t.Setenv("PMR52_GITHUB_TOKEN", "github-secret")
		body := "---\ntracker: {kind: linear, provider: {api_key: k, followup_issue_creation: true}, active_states: [Todo], terminal_states: [Done]}\n" +
			"github: {owner: pmrrasmussen, repository: symphony, token: $PMR52_GITHUB_TOKEN}\n" +
			"polling: {interval_ms: 100}\nworkspace: {root: work}\nhooks: {timeout_ms: 100}\nagent: {backend: codex}\n---\nbody"
		w, err := Load(write(t, body), "logs")
		if err != nil {
			t.Fatalf("codex workflow was rejected: %v", err)
		}
		if !w.Config.GitHub.Enabled || w.Config.Tracker.HandoffState != "" {
			t.Fatalf("fixture does not exercise the combination: %+v", w.Config.GitHub.Enabled)
		}
	})

	t.Run("the same capabilities stay valid for codex", func(t *testing.T) {
		body := "---\ntracker: {kind: linear, provider: {api_key: k, handoff_state: In Review}, active_states: [Todo], terminal_states: [Done]}\npolling: {interval_ms: 100}\nworkspace: {root: work}\nhooks: {timeout_ms: 100}\nagent: {backend: codex}\n---\nbody"
		if _, err := Load(write(t, body), "logs"); err != nil {
			t.Fatalf("codex workflow with capabilities was rejected: %v", err)
		}
	})

	t.Run("a github block that does not resolve leaves the integration disabled", func(t *testing.T) {
		// decodeGitHub disables the integration on any invalid value, so a present
		// but unresolvable block is not a configured capability and does not reach
		// the refusal. Nothing is silently granted: the capability is unavailable
		// either way.
		body := head + "github: {owner: pmrrasmussen, repository: symphony, token_file: absent-github-token}\nagent: {backend: claude}\n---\nbody"
		w, err := Load(write(t, body), "logs")
		if err != nil {
			t.Fatalf("err=%v", err)
		}
		if w.Config.GitHub.Enabled {
			t.Fatalf("github=%+v", w.Config.GitHub)
		}
	})
}

// TestEverySelectableBackendHasAValidatedLaunchContract fails when a name is
// added to agentBackends without giving decode's launch-contract switch an arm
// of its own. That switch defaulted to the Codex requirements, so a new backend
// would have been validated against codex.command and the codex timeouts.
func TestEverySelectableBackendHasAValidatedLaunchContract(t *testing.T) {
	path := filepath.Join(t.TempDir(), "WORKFLOW.md")
	for _, backend := range AgentBackends() {
		content := "---\ntracker: {kind: linear, provider: {api_key: k}, active_states: [Todo], terminal_states: [Done]}\nworkspace: {root: work}\nagent: {backend: " + backend + "}\n---\nbody"
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		w, err := Load(path, "logs")
		if err != nil {
			t.Fatalf("backend %q: %v", backend, err)
		}
		launch, known := w.Config.AgentLaunchFor(backend)
		if !known || strings.TrimSpace(launch.Command) == "" || launch.TurnTimeout <= 0 || launch.StallTimeout <= 0 {
			t.Fatalf("backend %q launch=%+v known=%v", backend, launch, known)
		}
	}
}

// TestReservedSecretEnvNamesReturnsAnIndependentCopy pins the contract every
// caller relies on: both launchers append their own configured names to the
// result, so a returned slice that aliased the package's own would let one
// backend's constructor overwrite the shared policy for the whole process. The
// aliasing is invisible while len == cap, which it is for five names -- append
// reallocates -- so this is exactly the invariant that breaks silently when a
// sixth name is added.
