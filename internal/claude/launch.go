package claude

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/pmrrasmussen/symphony/internal/domain"
)

// codingTools is the fixed tool surface a Symphony Claude session gets before
// any bounded capability is added to it. It is deliberately not configurable:
// the default surface includes delegation, scheduling, and outbound capabilities
// that would step outside Symphony's bounded-capability model, and --tools is
// what actually removes them (a permission allowlist alone still advertises
// them).
var codingTools = []string{"Bash", "Edit", "Glob", "Grep", "Read", "Write"}

// mcpServerName is the one MCP server a Symphony session may ever have. The CLI
// derives every tool name it serves from it, so this constant is simultaneously
// the name checked in the init echo and the prefix checked in the tool surface.
const mcpServerName = "symphony"

// endpointTokenEnvName carries the capability endpoint's per-registration bearer
// token to the child.
//
// It is an environment variable and not a command-line argument because argv is
// world-readable on Linux (/proc/<pid>/cmdline) while a process's environment is
// owner-only, so a token in argv would be readable by any local account for as
// long as the turn runs. Nothing has to substitute it into the rendered
// configuration either: --mcp-config expands ${VAR} references inside headers --
// verified against claude 2.1.245 -- so the secret exists in exactly one place
// the child can reach and in no file, log, event, or argument.
const endpointTokenEnvName = "SYMPHONY_MCP_TOKEN"

// mcpToolName is the tool name the CLI derives for one capability served by this
// endpoint. It is the authority for that name; config.MCPToolPrefix is the
// mirror the rendered prompt has to carry, and a test in this package asserts
// the two agree.
func mcpToolName(capability string) string { return "mcp__" + mcpServerName + "__" + capability }

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
	// AllowLocalBinding grants bind(2) on loopback in addition to the outbound
	// connect that AllowedDomains alone covers. The two are separate grants in
	// the CLI's sandbox: without this, "*" egress still leaves net.Listen on
	// 127.0.0.1 failing with "bind: operation not permitted", which is what
	// blocks every repository test suite that stands up an httptest server or
	// an mcpbridge listener.
	AllowLocalBinding bool `json:"allowLocalBinding"`
}

type permissionsPolicy struct {
	DefaultMode string   `json:"defaultMode"`
	Allow       []string `json:"allow"`
	Deny        []string `json:"deny"`
}

// buildPolicy bounds Bash writes to the worktree plus the two narrow Git
// metadata roots Symphony grants for a local commit -- the same grant the Codex
// profile makes -- and leaves outbound network unrestricted, matching the Codex
// profile's deliberate networkAccess: true. It also grants AllowLocalBinding,
// without which a session cannot bind the loopback listeners several of this
// repository's own test suites require to run at all. Reads are not confined,
// exactly as for Codex.
//
// allowed is the session's whole tool surface, capability tools included, and it
// is passed in rather than read from codingTools so the payload cannot describe
// a narrower permission set than the flags do. This file's doctrine is that
// policy is pinned in the payload: two disagreeing representations of "what is
// permitted" would leave a reader unable to tell which one is authoritative.
//
// sandbox.filesystem.allowWrite governs Bash and its children only, so Allow
// expands "Edit" and "Write" into one path-scoped rule per write root instead of
// the bare name (PMR-156). Every other tool, Bash included, keeps the bare name.
// docs/architecture.md's "Sandbox ownership decision" section states what that
// closed, what it did not, and where the rule syntax was verified.
func buildPolicy(r domain.AgentRequest, allowed []string) (policy, error) {
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
			// "*" is unrestricted egress, matching the Codex profile's
			// networkAccess: true. AllowLocalBinding is the separate grant
			// repository test suites need to bind loopback listeners; a
			// narrower policy belongs to whoever owns that policy, not to this
			// launcher.
			Network: networkPolicy{AllowedDomains: []string{"*"}, AllowLocalBinding: true},
		},
		Permissions: permissionsPolicy{
			DefaultMode: permissionMode,
			Allow:       scopedAllow(roots, allowed),
			Deny:        append([]string(nil), deniedTools...),
		},
	}, nil
}

// editWriteTools are the tool names sandbox.filesystem.allowWrite does not
// govern. scopedAllow is what confines them instead.
var editWriteTools = map[string]bool{"Edit": true, "Write": true}

// scopedAllow expands each of editWriteTools into one path-scoped permission
// rule per write root, and leaves every other tool name in allowed unchanged.
//
// The path syntax is the CLI's own: a rule argument beginning with "//" is an
// absolute path, so root -- always absolute, per writeRoots -- becomes
// "//"+root+"/**". "/**" matches anything under the root, never the root path
// itself, which is exactly right here since a root is always a directory.
func scopedAllow(roots []string, allowed []string) []string {
	rules := make([]string, 0, len(allowed))
	for _, tool := range allowed {
		if !editWriteTools[tool] {
			rules = append(rules, tool)
			continue
		}
		for _, root := range roots {
			rules = append(rules, tool+"(/"+root+"/**)")
		}
	}
	return rules
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

// capabilityEndpoint is one turn's reachable capability set: where the private
// loopback MCP endpoint lives, the bearer token that authorizes this turn
// against it, and the capability names the session's registry advertised.
//
// The names are frozen when the session's registry is built, exactly as they are
// on the Codex transport, and are deliberately not recomputed here from current
// settings: settings are live-reloadable, the registry is not, and a --tools
// list computed from a newer configuration than the registry that serves
// tools/list would either permission-deny a tool the endpoint offers or refuse
// the whole session for advertising a tool Symphony itself asked for.
type capabilityEndpoint struct {
	url   string
	token string
	names []string
}

// mcpConfig is the --mcp-config payload. Like the settings payload it is
// marshaled from structs and passed inline rather than written to a file: a
// configuration file that is unreadable or half-written at launch leaves the
// child with no MCP server and no diagnostic, which under --strict-mcp-config is
// indistinguishable from a session that was never meant to have one -- except
// that verifyInit then refuses the turn, which is the fail-closed half of the
// same doctrine.
type mcpConfig struct {
	MCPServers map[string]mcpServerConfig `json:"mcpServers"`
}

type mcpServerConfig struct {
	Type string `json:"type"`
	URL  string `json:"url"`
	// Headers carries the bearer token by ${VAR} reference, so the token itself
	// never enters argv. See endpointTokenEnvName.
	Headers map[string]string `json:"headers"`
}

// launchContract is one turn's launch: the argument vector, and the init echo
// that argument vector must produce.
//
// The two travel together because they are one decision. verifyInit used to
// recompute the expected tool surface from the package-level codingTools, which
// was safe only while that surface was a constant; with a per-session capability
// set they would be two independent computations of the same thing, and the
// drift would surface as a session refused for advertising exactly the tool
// Symphony asked the CLI to advertise. Handing the echo to the verifier makes
// that class of mismatch unrepresentable.
type launchContract struct {
	args []string
	// tools is the exact tool surface the CLI must report back -- the fixed
	// coding tools plus one mcp__symphony__<name> per advertised capability.
	tools []string
	// mcpServers is the exact set of MCP servers the CLI must report, each of
	// which must also report itself connected. Empty means no MCP server at all.
	mcpServers []string
}

// launchArgs renders the fixed launch contract. Every policy decision is a flag
// or a marshaled payload passed on the command line, never a file: a settings or
// MCP configuration file that is unreadable or half-written is silently ignored
// by the CLI, which would drop the policy without a diagnostic.
//
// The prompt is never an argument. --tools, --allowedTools, and --mcp-config are
// variadic, so a trailing positional prompt is consumed as a flag value; it goes
// on stdin instead.
//
// endpoint is nil, or advertises nothing, for a session with no reachable
// capability -- a workflow that configures none. That path must render
// byte-identically to what it rendered before this endpoint existed: it is the
// Codex-parity path, and it is what makes "no MCP server at all" in the init
// echo mean exactly one thing. Configuration is what keeps that meaning single,
// by refusing a Claude workflow that configures a capability no session could
// advertise.
func launchArgs(r domain.AgentRequest, sessionID string, resume bool, endpoint *capabilityEndpoint) (launchContract, error) {
	contract := launchContract{tools: append([]string(nil), codingTools...)}
	var mcp []byte
	var err error
	if endpoint != nil && len(endpoint.names) > 0 {
		for _, name := range endpoint.names {
			// Explicit names, never the mcp__symphony__* glob the CLI also
			// accepts: the init echo is checked for set equality, and a glob
			// would let the CLI advertise a capability Symphony never asked for
			// and still pass every check.
			contract.tools = append(contract.tools, mcpToolName(name))
		}
		contract.mcpServers = []string{mcpServerName}
		mcp, err = json.Marshal(mcpConfig{MCPServers: map[string]mcpServerConfig{
			mcpServerName: {Type: "http", URL: endpoint.url,
				Headers: map[string]string{"Authorization": "Bearer ${" + endpointTokenEnvName + "}"}},
		}})
		if err != nil {
			return launchContract{}, fmt.Errorf("render claude capability endpoint: %w", err)
		}
	}
	// The settings payload is rendered after the tool surface is known, because
	// its permission allowlist is that surface.
	rendered, err := buildPolicy(r, contract.tools)
	if err != nil {
		return launchContract{}, err
	}
	settings, err := json.Marshal(rendered)
	if err != nil {
		return launchContract{}, fmt.Errorf("render claude policy: %w", err)
	}
	tools := strings.Join(contract.tools, ",")
	contract.args = []string{
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
		// --tools is the tool surface itself -- the bare names verifyInit checks
		// the init echo against -- while --allowedTools is a permission rule, like
		// the payload's own Allow list, so it carries rendered.Permissions.Allow's
		// path-scoped Edit/Write rules rather than the bare tool names. The two
		// necessarily differ now: --tools cannot express a scoped rule and still
		// mean "this tool exists", and rendered.Permissions.Allow already is the
		// canonical list to keep --allowedTools in step with -- computing it twice
		// would let the flag and the payload drift apart.
		"--tools", tools,
		"--allowedTools", strings.Join(rendered.Permissions.Allow, ","),
		"--disallowedTools", strings.Join(deniedTools, ","),
		"--permission-mode", permissionMode,
		// --strict-mcp-config confines the session to the MCP configuration on
		// this command line, and it is load-bearing in both directions. With no
		// --mcp-config it leaves the session with no MCP server at all; with one
		// it guarantees Symphony's own endpoint is the only server the child can
		// reach. Without it the child additionally inherits the operator's own
		// user-level MCP servers, credentials included.
		"--strict-mcp-config",
	}
	if len(mcp) > 0 {
		contract.args = append(contract.args, "--mcp-config", string(mcp))
	}
	if resume {
		contract.args = append(contract.args, "--resume", sessionID)
	} else {
		contract.args = append(contract.args, "--session-id", sessionID)
	}
	if model := strings.TrimSpace(r.Model); model != "" {
		contract.args = append(contract.args, "--model", model)
	}
	return contract, nil
}
