package claude

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/pmrrasmussen/symphony/internal/domain"
)

// codingTools is the fixed tool surface a Symphony Claude session gets. It is
// deliberately not configurable: the default surface includes delegation,
// scheduling, and outbound capabilities that would step outside Symphony's
// bounded-capability model, and --tools is what actually removes them (a
// permission allowlist alone still advertises them).
var codingTools = []string{"Bash", "Edit", "Glob", "Grep", "Read", "Write"}

// deniedTools are refused in addition to being absent from the tool surface, so
// a future change to codingTools cannot quietly reintroduce outbound access.
var deniedTools = []string{"WebFetch", "WebSearch"}

// permissionMode is the only fail-closed non-interactive mode: it denies
// anything not explicitly allowed and tells the model, instead of prompting.
// "manual"/"default" can block on stdin, which is also where the prompt is
// written, and "bypassPermissions" is the opposite of fail-closed.
const permissionMode = "dontAsk"

// policy is the settings payload handed to the CLI. It is marshaled from these
// structs rather than assembled as text because the CLI silently ignores a
// settings payload it cannot parse: a hand-built string with one typo would
// leave the session running with no policy at all and no diagnostic.
type policy struct {
	Sandbox     sandboxPolicy     `json:"sandbox"`
	Permissions permissionsPolicy `json:"permissions"`
}

type sandboxPolicy struct {
	Enabled bool `json:"enabled"`
	// FailIfUnavailable turns the CLI's own fail-open degradation into a refusal
	// to start. Without it a sandbox that cannot initialize disables itself for
	// the rest of the session, reporting that only in tool-result text, and the
	// turn continues unconfined.
	FailIfUnavailable bool `json:"failIfUnavailable"`
	// AllowUnsandboxedCommands closes the per-command escape hatch.
	AllowUnsandboxedCommands bool             `json:"allowUnsandboxedCommands"`
	Filesystem               filesystemPolicy `json:"filesystem"`
	Network                  networkPolicy    `json:"network"`
}

type filesystemPolicy struct {
	AllowWrite []string `json:"allowWrite"`
}

type networkPolicy struct {
	AllowedDomains []string `json:"allowedDomains"`
}

type permissionsPolicy struct {
	DefaultMode string   `json:"defaultMode"`
	Allow       []string `json:"allow"`
	Deny        []string `json:"deny"`
}

// buildPolicy bounds Bash writes to the worktree plus the two narrow Git
// metadata roots Symphony grants for a local commit -- the same grant the Codex
// profile makes -- and leaves outbound network unrestricted, matching the Codex
// profile's deliberate networkAccess: true. Reads are not confined, exactly as
// for Codex.
func buildPolicy(r domain.AgentRequest) (policy, error) {
	roots, err := writeRoots(r)
	if err != nil {
		return policy{}, err
	}
	return policy{
		Sandbox: sandboxPolicy{
			Enabled:                  true,
			FailIfUnavailable:        true,
			AllowUnsandboxedCommands: false,
			Filesystem:               filesystemPolicy{AllowWrite: roots},
			// "*" is unrestricted egress. Repository test suites bind loopback
			// listeners and fetch modules, which is why the Codex profile grants
			// the same; a narrower list belongs to whoever owns that policy, not
			// to this launcher.
			Network: networkPolicy{AllowedDomains: []string{"*"}},
		},
		Permissions: permissionsPolicy{
			DefaultMode: permissionMode,
			Allow:       append([]string(nil), codingTools...),
			Deny:        append([]string(nil), deniedTools...),
		},
	}, nil
}

// writeRoots is the workspace plus its Git metadata roots, deduplicated and
// ordered so the rendered policy is deterministic.
func writeRoots(r domain.AgentRequest) ([]string, error) {
	workspace := strings.TrimSpace(r.Workspace)
	if workspace == "" {
		return nil, fmt.Errorf("claude launch requires a workspace directory")
	}
	seen := map[string]bool{workspace: true}
	roots := []string{workspace}
	for _, root := range r.GitMetadataRoots {
		root = strings.TrimSpace(root)
		if root == "" || seen[root] {
			continue
		}
		seen[root] = true
		roots = append(roots, root)
	}
	sort.Strings(roots[1:])
	return roots, nil
}

// launchArgs renders the fixed launch contract. Every policy decision is a flag
// or a marshaled settings payload passed on the command line, never a file: a
// settings file that is unreadable or half-written is silently ignored by the
// CLI, which would drop the policy without a diagnostic.
//
// The prompt is never an argument. --tools, --allowedTools, and --mcp-config are
// variadic, so a trailing positional prompt is consumed as a flag value; it goes
// on stdin instead.
func launchArgs(r domain.AgentRequest, sessionID string, resume bool) ([]string, error) {
	rendered, err := buildPolicy(r)
	if err != nil {
		return nil, err
	}
	settings, err := json.Marshal(rendered)
	if err != nil {
		return nil, fmt.Errorf("render claude policy: %w", err)
	}
	tools := strings.Join(codingTools, ",")
	args := []string{
		"--print",
		"--output-format", "stream-json",
		// stream-json without --verbose is a hard error, not a downgrade.
		"--verbose",
		// An empty source list excludes user, project, and local settings. The
		// worktree is a checkout of a repository that may ship its own
		// .claude/settings.json, CLAUDE.md, skills, plugins, and hooks -- hooks
		// run arbitrary commands -- so leaving discovery on would let repository
		// content widen the boundary this launcher fixes.
		"--setting-sources", "",
		"--settings", string(settings),
		"--tools", tools,
		"--allowedTools", tools,
		"--disallowedTools", strings.Join(deniedTools, ","),
		"--permission-mode", permissionMode,
		// No capability bridge exists yet, so the session must get no MCP server
		// at all. Without this flag the child inherits the operator's own
		// user-level MCP servers, credentials included.
		"--strict-mcp-config",
	}
	if resume {
		args = append(args, "--resume", sessionID)
	} else {
		args = append(args, "--session-id", sessionID)
	}
	if model := strings.TrimSpace(r.Model); model != "" {
		args = append(args, "--model", model)
	}
	return args, nil
}
