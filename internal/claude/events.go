package claude

import (
	"encoding/json"
	"path/filepath"
	"strings"

	"github.com/pmrrasmussen/symphony/internal/domain"
)

// Every struct in this file is deliberately narrow. The CLI's stream carries
// full model output, full tool arguments, and full tool results
// (assistant.message.content[].text, tool_use.input, tool_result.content, and
// result.result); a field with no matching struct member is silently discarded
// by json.Unmarshal, so it can never reach an event or a log. Do not decode any
// of this into map[string]any.

// streamEvent is the envelope shared by every line on stdout.
type streamEvent struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	SessionID string `json:"session_id"`
}

// initEvent is the session announcement. It is the only place the CLI reports
// back what policy it actually applied, which makes it the verification hook for
// a launch contract that is otherwise unobservable.
type initEvent struct {
	CWD            string   `json:"cwd"`
	PermissionMode string   `json:"permissionMode"`
	Tools          []string `json:"tools"`
	MCPServers     []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	} `json:"mcp_servers"`
}

// toolUse and toolResult are the only tool-lifecycle signal the CLI emits: there
// are no discrete start/complete notifications and no protocol-supplied
// durations, so calls are paired by ID and timed by this backend.
type assistantMessage struct {
	Message struct {
		Content []struct {
			Type string `json:"type"`
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"content"`
		// Usage is the Anthropic Messages API usage for this one underlying API
		// call. A turn's agentic loop can make several of these before the CLI's
		// closing result line, which is the only place usage was previously
		// read, so reading it there alone leaves a turn that is actively
		// spending tokens reporting zero until it ends -- or, if it is killed by
		// the turn timeout, reporting nothing at all.
		Usage usage `json:"usage"`
	} `json:"message"`
}

type userMessage struct {
	Message struct {
		Content []struct {
			Type      string          `json:"type"`
			ToolUseID string          `json:"tool_use_id"`
			IsError   bool            `json:"is_error"`
			Content   json.RawMessage `json:"content"`
		} `json:"content"`
	} `json:"message"`
}

// sandboxBindDeniedMarker is the exact substring Go's net package produces
// when the sandbox refuses a loopback bind ("listen tcp 127.0.0.1:0: bind:
// operation not permitted"), which is what blocks every repository test
// suite that stands up an httptest server or an mcpbridge listener.
const sandboxBindDeniedMarker = "bind: operation not permitted"

// toolResultDeniedLoopbackBind reports whether a failed tool result's content
// contains the fixed sandbox-denial marker, without ever forwarding that
// content itself into an event: only this boolean crosses the boundary, so a
// tool result full of arbitrary or sensitive output is still never decoded
// into a log.
func toolResultDeniedLoopbackBind(raw json.RawMessage) bool {
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return strings.Contains(text, sandboxBindDeniedMarker)
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		for _, block := range blocks {
			if strings.Contains(block.Text, sandboxBindDeniedMarker) {
				return true
			}
		}
	}
	return false
}

// resultEvent ends a turn. There is no turn_success or error_reason field: the
// authoritative signals are is_error and terminal_reason. Note that an
// authentication failure arrives as subtype "success" with is_error true, so
// subtype must never be read as a success signal.
type resultEvent struct {
	IsError        bool   `json:"is_error"`
	Subtype        string `json:"subtype"`
	StopReason     string `json:"stop_reason"`
	TerminalReason string `json:"terminal_reason"`
	APIErrorStatus string `json:"api_error_status"`
	NumTurns       int    `json:"num_turns"`
	Usage          usage  `json:"usage"`
	// PermissionDenials records refused tool calls. Only the tool name is
	// decoded; the denied arguments are not.
	PermissionDenials []struct {
		ToolName string `json:"tool_name"`
	} `json:"permission_denials"`
}

// usage is per turn, unlike the Codex app-server's cumulative notifications.
// Reporting InputTokens alone would understate real input by orders of magnitude
// because almost all of it arrives as cache reads and cache creation.
type usage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
}

// totals folds the cache components into the input count, so the reported input
// reflects what the model actually processed.
func (u usage) totals() domain.Usage {
	input := u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
	return domain.Usage{InputTokens: input, OutputTokens: u.OutputTokens, TotalTokens: input + u.OutputTokens}
}

// rateLimitEvent carries the actionable fields as strings, which a numeric-only
// normalization would drop entirely.
type rateLimitEvent struct {
	RateLimitInfo struct {
		Status        string  `json:"status"`
		RateLimitType string  `json:"rateLimitType"`
		Utilization   float64 `json:"utilization"`
		// ResetsAt is Unix epoch seconds naming when the reported window
		// reopens. It is absent for a status the CLI reports with no reset at
		// all (in practice, a fresh "allowed").
		ResetsAt int64 `json:"resetsAt"`
	} `json:"rate_limit_info"`
}

// permissionDeniedEvent names the tool whose call was refused.
type permissionDeniedEvent struct {
	ToolName  string `json:"tool_name"`
	ToolUseID string `json:"tool_use_id"`
}

// itemType classifies a tool call for the existing item vocabulary, so an
// operator reading logs sees the same categories the Codex backend reports.
func itemType(tool string) string {
	switch {
	case tool == "Bash":
		return "commandExecution"
	case tool == "Edit" || tool == "Write" || tool == "NotebookEdit" || tool == "MultiEdit":
		return "fileChange"
	case strings.HasPrefix(tool, "mcp__"):
		return "mcpToolCall"
	default:
		return "toolCall"
	}
}

// verifyInit checks that the CLI applied the launch contract this backend asked
// for. The settings payload is accepted silently even when it cannot be parsed,
// the MCP configuration is accepted silently too, and the sandbox reports its own
// state nowhere in the stream, so this echo is the only confirmation available
// that the policy is in force.
//
// It takes the contract the turn was actually launched with rather than
// recomputing what it should have been: see launchContract.
//
// It deliberately does not check the sandbox: the init event does not report it.
// That limitation is stated in the documentation rather than papered over.
func verifyInit(event initEvent, workspace string, contract launchContract) string {
	if event.PermissionMode != permissionMode {
		return "claude session refused: permission mode was not applied"
	}
	if refusal := verifyMCPServers(event, contract.mcpServers); refusal != "" {
		return refusal
	}
	if !sameDirectory(event.CWD, workspace) {
		// A turn running somewhere other than this issue's worktree would write
		// outside the boundary the sandbox was built around.
		return "claude session refused: the reported working directory is not this issue's workspace"
	}
	expected := map[string]bool{}
	for _, tool := range contract.tools {
		expected[tool] = true
	}
	for _, tool := range event.Tools {
		if !expected[tool] {
			return "claude session refused: an unexpected tool was available"
		}
		delete(expected, tool)
	}
	if len(expected) != 0 {
		return "claude session refused: the expected tool surface was not applied"
	}
	return ""
}

// verifyMCPServers requires exactly the servers the contract asked for, each
// reporting itself connected.
//
// A session with no capability endpoint requires zero servers, which is what
// --strict-mcp-config is there to produce and what every session gets today; an
// extra server means the child reached a configuration this launcher did not
// write, credentials included.
//
// A session with an endpoint requires "connected" and not "pending", which is
// what makes this fail closed. A pending or failed server is a session whose
// capability tools are advertised in --tools -- so the model will be told they
// exist and will call them -- while every call returns a client-level MCP
// failure. Accepting that state would produce precisely the silent-breakage
// shape this wiring exists to prevent: a turn that commits work, cannot publish
// it, and reports EventCompleted.
func verifyMCPServers(event initEvent, expected []string) string {
	remaining := map[string]bool{}
	for _, name := range expected {
		remaining[name] = true
	}
	for _, server := range event.MCPServers {
		if !remaining[server.Name] {
			return "claude session refused: an MCP server was attached"
		}
		delete(remaining, server.Name)
		if server.Status != "connected" {
			return "claude session refused: the capability endpoint did not connect"
		}
	}
	if len(remaining) != 0 {
		return "claude session refused: the capability endpoint was not attached"
	}
	return ""
}

// decode reads one stdout line. A line that is not JSON, or not an object, is
// reported as undecodable rather than failing the run: the stream is the child's
// output, not a protocol Symphony controls.
func decode(line []byte) (streamEvent, bool) {
	trimmed := strings.TrimSpace(string(line))
	if !strings.HasPrefix(trimmed, "{") {
		return streamEvent{}, false
	}
	var envelope streamEvent
	if err := json.Unmarshal([]byte(trimmed), &envelope); err != nil || envelope.Type == "" {
		return streamEvent{}, false
	}
	return envelope, true
}

// sameDirectory compares two paths after resolving symlinks, because the CLI
// reports a resolved working directory (/private/var/... on macOS for a
// /var/... workspace) and a literal comparison would reject correct launches.
func sameDirectory(reported, want string) bool {
	reported, want = strings.TrimSpace(reported), strings.TrimSpace(want)
	if reported == "" || want == "" {
		return false
	}
	if reported == want {
		return true
	}
	resolvedReported, err := filepath.EvalSymlinks(reported)
	if err != nil {
		return false
	}
	resolvedWant, err := filepath.EvalSymlinks(want)
	if err != nil {
		return false
	}
	return resolvedReported == resolvedWant
}
