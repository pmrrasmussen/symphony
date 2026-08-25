package claude

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pmrrasmussen/symphony/internal/capability"
	"github.com/pmrrasmussen/symphony/internal/domain"
	"github.com/pmrrasmussen/symphony/internal/mcpbridge"
)

// The capability names this package renders into a launch contract. They are the
// registry's own constants, so a rename there breaks these tests rather than
// silently changing what a Claude session is told exists.
var allCapabilityNames = []string{
	capability.NameCreateFollowupIssue,
	capability.NameGitHubPublishPR,
	capability.NameGitHubPRContext,
	capability.NameGitHubLandPR,
}

// TestTheLaunchContractWithoutACapabilityIsUnchanged is the test that makes the
// rest of this file safe to land. Configuration still refuses a Claude workflow
// that enables a Symphony capability, so every real session takes this path, and
// the argument vector it produces has to be exactly the one it produced before
// the endpoint existed -- not merely equivalent. A byte-level golden is the only
// assertion that says so: an extra flag, a reordering, or an empty --mcp-config
// would all pass a set comparison.
func TestTheLaunchContractWithoutACapabilityIsUnchanged(t *testing.T) {
	dir := t.TempDir()
	r := request(t, dir, "unused")
	r.Model = "sonnet"
	contract, err := launchArgs(r, "session-1", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	settings := flagValue(t, contract.args, "--settings")
	want := []string{
		"--print",
		"--output-format", "stream-json",
		"--verbose",
		"--setting-sources", "",
		"--settings", settings,
		"--tools", "Bash,Edit,Glob,Grep,Read,Write",
		"--allowedTools", "Bash,Edit,Glob,Grep,Read,Write",
		"--disallowedTools", "WebFetch,WebSearch",
		"--permission-mode", "dontAsk",
		"--strict-mcp-config",
		"--session-id", "session-1",
		"--model", "sonnet",
	}
	assertArgs(t, contract.args, want)
	if hasFlag(contract.args, "--mcp-config") {
		t.Fatalf("a session with no capability was handed an MCP configuration: %v", contract.args)
	}
	// An empty expected server set is what verifyInit turns into "no MCP server
	// at all", which is the behaviour every session still gets.
	if len(contract.mcpServers) != 0 {
		t.Fatalf("mcpServers=%v", contract.mcpServers)
	}
	if strings.Join(contract.tools, ",") != strings.Join(codingTools, ",") {
		t.Fatalf("tools=%v, want exactly the coding tools", contract.tools)
	}
}

// TestTheLaunchContractPinsExactlyTheAdvertisedCapabilities covers the sets a
// session can actually be built with: none, all four, the Merging-state landing
// set, and a tracker-only follow-up set. Flag order is asserted along with the
// values because the contract is a fixed argument vector, and because the
// resume/session-id and model flags must stay last however many tools there are.
func TestTheLaunchContractPinsExactlyTheAdvertisedCapabilities(t *testing.T) {
	for name, names := range map[string][]string{
		"nothing advertised": nil,
		"all four":           allCapabilityNames,
		"landing only":       {capability.NameGitHubLandPR},
		"follow-up only":     {capability.NameCreateFollowupIssue},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			endpoint := &capabilityEndpoint{url: "http://127.0.0.1:54321/mcp", token: "token-value", names: names}
			contract, err := launchArgs(request(t, dir, "unused"), "session-1", true, endpoint)
			if err != nil {
				t.Fatal(err)
			}
			wantTools := append(append([]string(nil), codingTools...), prefixed(names)...)
			if strings.Join(contract.tools, ",") != strings.Join(wantTools, ",") {
				t.Fatalf("tools=%v, want %v", contract.tools, wantTools)
			}
			tools := strings.Join(wantTools, ",")
			want := []string{
				"--print",
				"--output-format", "stream-json",
				"--verbose",
				"--setting-sources", "",
				"--settings", flagValue(t, contract.args, "--settings"),
				// Both flags carry the same explicit list: --tools is what
				// decides a tool exists, --allowedTools is what keeps
				// dontAsk from denying it, and a session needs both.
				"--tools", tools,
				"--allowedTools", tools,
				"--disallowedTools", "WebFetch,WebSearch",
				"--permission-mode", "dontAsk",
				"--strict-mcp-config",
			}
			if len(names) > 0 {
				want = append(want, "--mcp-config", flagValue(t, contract.args, "--mcp-config"))
			}
			want = append(want, "--resume", "session-1")
			assertArgs(t, contract.args, want)
			if len(names) == 0 {
				if hasFlag(contract.args, "--mcp-config") || len(contract.mcpServers) != 0 {
					t.Fatalf("an unadvertised session was still given an endpoint: %v", contract.args)
				}
				return
			}
			if strings.Join(contract.mcpServers, ",") != mcpServerName {
				t.Fatalf("mcpServers=%v, want exactly %q", contract.mcpServers, mcpServerName)
			}
			var rendered mcpConfig
			if err := json.Unmarshal([]byte(flagValue(t, contract.args, "--mcp-config")), &rendered); err != nil {
				t.Fatalf("MCP configuration is not valid JSON: %v", err)
			}
			server, ok := rendered.MCPServers[mcpServerName]
			if !ok || len(rendered.MCPServers) != 1 {
				t.Fatalf("MCP configuration=%+v", rendered)
			}
			if server.Type != "http" || server.URL != endpoint.url {
				t.Fatalf("server=%+v", server)
			}
			// The credential travels by ${VAR} reference. A rendered token
			// would put a bearer credential in argv, which is world-readable
			// on Linux for as long as the turn runs.
			if server.Headers["Authorization"] != "Bearer ${SYMPHONY_MCP_TOKEN}" {
				t.Fatalf("headers=%+v", server.Headers)
			}
			for _, arg := range contract.args {
				if strings.Contains(arg, endpoint.token) {
					t.Fatalf("the endpoint token appeared in an argument: %q", arg)
				}
			}
			// Explicit names, never the mcp__symphony__* glob: the init echo is
			// checked for set equality, and a glob would let the CLI advertise a
			// capability Symphony never asked for and still pass.
			if strings.Contains(tools, "*") {
				t.Fatalf("the tool surface was pinned with a glob: %q", tools)
			}
		})
	}
}

// TestVerifyInitAcceptsExactlyTheContractItWasLaunchedWith is the fail-closed
// half. The CLI silently ignores an MCP configuration it cannot use, so a
// dropped --mcp-config is indistinguishable from a session that never had one --
// except in this echo, which is the only place it is observable at all.
func TestVerifyInitAcceptsExactlyTheContractItWasLaunchedWith(t *testing.T) {
	dir := t.TempDir()
	workspace := workspaceOf(dir)
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	bare := launchContract{tools: codingTools}
	bound := launchContract{
		tools:      append(append([]string(nil), codingTools...), prefixed(allCapabilityNames)...),
		mcpServers: []string{mcpServerName},
	}
	connected := []initServer{{Name: mcpServerName, Status: "connected"}}

	for name, test := range map[string]struct {
		contract launchContract
		event    initEvent
		refusal  string
	}{
		"no capability, no server": {bare, initEvent{CWD: workspace, PermissionMode: permissionMode, Tools: codingTools}, ""},
		"every capability connected": {bound, initEvent{CWD: workspace, PermissionMode: permissionMode,
			Tools: bound.tools, MCPServers: connected}, ""},
		"the endpoint was not attached": {bound, initEvent{CWD: workspace, PermissionMode: permissionMode,
			Tools: bound.tools}, "the capability endpoint was not attached"},
		"the endpoint failed": {bound, initEvent{CWD: workspace, PermissionMode: permissionMode,
			Tools: bound.tools, MCPServers: []initServer{{Name: mcpServerName, Status: "failed"}}}, "the capability endpoint did not connect"},
		// Pending is the case that makes this fail closed rather than
		// optimistic: the tools are advertised, so the model is told they
		// exist, while every call returns a client-level MCP failure.
		"the endpoint is still pending": {bound, initEvent{CWD: workspace, PermissionMode: permissionMode,
			Tools: bound.tools, MCPServers: []initServer{{Name: mcpServerName, Status: "pending"}}}, "the capability endpoint did not connect"},
		"another server was attached too": {bound, initEvent{CWD: workspace, PermissionMode: permissionMode,
			Tools: bound.tools, MCPServers: append([]initServer{{Name: "operator", Status: "connected"}}, connected...)}, "an MCP server was attached"},
		"a server reached a session with no capability": {bare, initEvent{CWD: workspace, PermissionMode: permissionMode,
			Tools: codingTools, MCPServers: connected}, "an MCP server was attached"},
		"an extra capability tool is available": {bound, initEvent{CWD: workspace, PermissionMode: permissionMode,
			Tools: append(append([]string(nil), bound.tools...), mcpToolName("something_else")), MCPServers: connected}, "an unexpected tool was available"},
		"an advertised capability tool is missing": {bound, initEvent{CWD: workspace, PermissionMode: permissionMode,
			Tools: bound.tools[:len(bound.tools)-1], MCPServers: connected}, "the expected tool surface was not applied"},
	} {
		t.Run(name, func(t *testing.T) {
			refusal := verifyInit(test.event, workspace, test.contract)
			if test.refusal == "" {
				if refusal != "" {
					t.Fatalf("a conforming init was refused: %q", refusal)
				}
				return
			}
			if !strings.Contains(refusal, test.refusal) {
				t.Fatalf("refusal=%q, want it to name %q", refusal, test.refusal)
			}
		})
	}
}

// TestTheCapabilityEndpointReachesTheChildAsARealMCPClientCanUseIt is the test
// this whole layer exists for. internal/mcpbridge's own suite exercises the
// protocol exhaustively against in-process requests; what is unproven until here
// is that the endpoint reaches the child at all -- that the URL survives into an
// argument, the token survives into the environment, and a client that has only
// those two can complete a handshake, list the session's capabilities, and invoke
// one over real loopback HTTP.
//
// The fake binary is a real HTTP client for that reason. A test that reached into
// the registration and called the handler directly would pass with the endpoint
// wired to nothing.
func TestTheCapabilityEndpointReachesTheChildAsARealMCPClientCanUseIt(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl is needed for the child to act as a real MCP client over loopback HTTP")
	}
	dir := t.TempDir()
	registry := &scriptedRegistry{served: []capability.Capability{scriptedCapability{
		definition: capability.Definition{Name: capability.NameGitHubPRContext, Description: "read review context",
			InputSchema: map[string]any{"type": "object", "additionalProperties": false}},
		result: capability.Result{Success: true, Payload: map[string]any{"state": "open"}},
	}}}
	script := writeFakeClaude(t, dir, mcpClientBody(dir)+"cat <<'EOF'\n"+
		capabilityInitLine(dir)+"\n"+resultLine(false, "")+"\nEOF\n")

	backend, _ := backendWithEndpoint(t)
	_, events, err := startWithRegistry(t, backend, context.Background(), request(t, dir, script), registry)
	if err != nil {
		t.Fatal(err)
	}
	collected := drain(t, events)
	// The turn completing is itself an assertion: verifyInit required exactly
	// {symphony: connected} and exactly the seven tools this contract asked for.
	if lastKind(t, collected).Kind != domain.EventCompleted {
		t.Fatalf("the turn did not complete: %v", kinds(collected))
	}

	for _, step := range []string{"initialize", "tools-list", "tools-call"} {
		if status := readFile(t, filepath.Join(dir, step+".status")); status != "200" {
			t.Fatalf("%s returned HTTP %s: %s", step, status, readFile(t, filepath.Join(dir, step+".json")))
		}
	}
	var handshake struct {
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
			ServerInfo      struct {
				Name string `json:"name"`
			} `json:"serverInfo"`
		} `json:"result"`
	}
	decodeJSONFile(t, filepath.Join(dir, "initialize.json"), &handshake)
	if handshake.Result.ProtocolVersion == "" || handshake.Result.ServerInfo.Name != mcpServerName {
		t.Fatalf("handshake=%+v", handshake.Result)
	}
	var listing struct {
		Result struct {
			Tools []struct {
				Name        string         `json:"name"`
				Description string         `json:"description"`
				InputSchema map[string]any `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	decodeJSONFile(t, filepath.Join(dir, "tools-list.json"), &listing)
	// tools/list and --tools are derived from the same stored registry, so the
	// listing names exactly the capability the tool surface was pinned to --
	// unprefixed, because the prefix is the CLI's own.
	if len(listing.Result.Tools) != 1 || listing.Result.Tools[0].Name != capability.NameGitHubPRContext {
		t.Fatalf("tools/list=%+v", listing.Result.Tools)
	}
	if listing.Result.Tools[0].Description == "" || listing.Result.Tools[0].InputSchema == nil {
		t.Fatalf("advertised tool lost its schema: %+v", listing.Result.Tools[0])
	}
	var call struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	decodeJSONFile(t, filepath.Join(dir, "tools-call.json"), &call)
	if call.Result.IsError || len(call.Result.Content) != 1 || !strings.Contains(call.Result.Content[0].Text, `"state":"open"`) {
		t.Fatalf("tools/call=%+v", call.Result)
	}

	// The endpoint the child used is dead the moment the turn ended, and the
	// registry's turn-ended finalizer ran exactly once.
	url, token := endpointFromChild(t, dir)
	if status := probeEndpoint(t, url, token); status != http.StatusUnauthorized {
		t.Fatalf("the child's token still authenticates after the turn ended: HTTP %d", status)
	}
	if ended := registry.ended(); ended != 1 {
		t.Fatalf("turn-ended finalizer ran %d times, want exactly 1", ended)
	}
}

// TestEveryTurnEndPathRetiresTheRegistrationExactlyOnce covers the four ways a
// turn can end. Each one has to run the registry's turn-ended finalizer, because
// that is what performs the deferred Merging -> In Review transition after a
// retryable landing gate, and each one has to kill the registration, because
// leaving it live is a credential lifetime leak rather than a leaked struct: the
// registration holds the GitHub session, so a stale one keeps a
// loopback-reachable, token-bearing capability set alive for the daemon's
// lifetime.
//
// Exactly once matters as much as at least once: two finalizer runs are two
// attempts at the same deferred transition.
func TestEveryTurnEndPathRetiresTheRegistrationExactlyOnce(t *testing.T) {
	for name, test := range map[string]struct {
		transcript func(init string) string
		timeout    time.Duration
		cancel     bool
		want       domain.EventKind
	}{
		"completion": {transcript: func(init string) string {
			return "cat <<'EOF'\n" + init + "\n" + resultLine(false, "") + "\nEOF\n"
		}, want: domain.EventCompleted},
		"failure": {transcript: func(init string) string {
			return "cat <<'EOF'\n" + init + "\n" + resultLine(true, `"terminal_reason":"api_error"`) + "\nEOF\n"
		}, want: domain.EventFailed},
		"turn timeout": {transcript: func(init string) string {
			return "cat <<'EOF'\n" + init + "\nEOF\nsleep 120\n"
		}, timeout: 400 * time.Millisecond, want: domain.EventFailed},
		"hard cancel": {transcript: func(init string) string {
			return "cat <<'EOF'\n" + init + "\nEOF\nsleep 120\n"
		}, cancel: true, want: domain.EventFailed},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			registry := oneCapabilityRegistry()
			script := writeFakeClaude(t, dir, test.transcript(capabilityInitLine(dir)))
			r := request(t, dir, script)
			if test.timeout > 0 {
				r.TurnTimeout = test.timeout
			}
			backend, _ := backendWithEndpoint(t)
			session, events, err := startWithRegistry(t, backend, context.Background(), r, registry)
			if err != nil {
				t.Fatal(err)
			}
			if test.cancel {
				// Cancelling once the session has announced itself proves the
				// child is really running, so this is a hard cancel of a live
				// turn rather than of a turn that had already ended.
				for event := range events {
					if event.Kind == domain.EventSessionStarted {
						break
					}
				}
				if err := backend.Cancel(context.Background(), session); err != nil {
					t.Fatalf("cancel: %v", err)
				}
				// Asserted before the stream is drained, because Cancel's own
				// guarantee is that a returned Cancel has already retired the
				// session's authority -- not that the turn's shutdown will get
				// round to it. A caller that has cancelled a session has no
				// other handle on it left to wait for.
				if ended := registry.ended(); ended != 1 {
					t.Fatalf("cancel returned with the turn-ended finalizer having run %d times, want exactly 1", ended)
				}
				if url, token := endpointFromChild(t, dir); probeEndpoint(t, url, token) != http.StatusUnauthorized {
					t.Fatal("cancel returned while the turn's token still authenticated")
				}
			}
			collected := drain(t, events)
			if last := lastKind(t, collected); last.Kind != test.want {
				t.Fatalf("terminal event=%+v", last)
			}
			if ended := registry.ended(); ended != 1 {
				t.Fatalf("turn-ended finalizer ran %d times, want exactly 1", ended)
			}
			url, token := endpointFromChild(t, dir)
			if status := probeEndpoint(t, url, token); status != http.StatusUnauthorized {
				t.Fatalf("the turn's token still authenticates after the turn ended: HTTP %d", status)
			}
		})
	}
}

// TestASessionWithNothingAdvertisedRunsAsIfTheEndpointDidNotExist is the other
// half of the byte-identical claim, and the half an argument-vector golden
// cannot make: the child's environment must be untouched too. A registration is
// still created -- it is what runs the registry's turn-ended finalizer, which the
// Codex transport also runs unconditionally -- but it must be unreachable by
// construction, with no MCP configuration and no token anywhere the child can
// see it.
func TestASessionWithNothingAdvertisedRunsAsIfTheEndpointDidNotExist(t *testing.T) {
	dir := t.TempDir()
	registry := &scriptedRegistry{}
	script := writeFakeClaude(t, dir, "cat <<'EOF'\n"+initLine(dir, allCodingTools)+"\n"+
		resultLine(false, "")+"\nEOF\n")

	backend, endpoint := backendWithEndpoint(t)
	_, events, err := startWithRegistry(t, backend, context.Background(), request(t, dir, script), registry)
	if err != nil {
		t.Fatal(err)
	}
	if lastKind(t, drain(t, events)).Kind != domain.EventCompleted {
		t.Fatal("a session with nothing advertised did not complete")
	}
	args := readArgs(t, dir)
	if hasFlag(args, "--mcp-config") {
		t.Fatalf("an unadvertised session was handed an MCP configuration: %v", args)
	}
	if got := flagValue(t, args, "--tools"); got != strings.Join(codingTools, ",") {
		t.Fatalf("--tools=%q, want exactly the coding tools", got)
	}
	for _, entry := range strings.Split(readFile(t, filepath.Join(dir, "env.txt")), "\n") {
		if strings.HasPrefix(entry, endpointTokenEnvName+"=") {
			t.Fatalf("an unadvertised session handed the child an endpoint token: %q", entry)
		}
	}
	// The registration exists and is retired all the same, which is what keeps
	// the turn-ended finalizer firing on a session whose capabilities are all
	// unadvertised.
	if ended := registry.ended(); ended != 1 {
		t.Fatalf("turn-ended finalizer ran %d times, want exactly 1", ended)
	}
	if err := endpoint.Close(context.Background()); err != nil {
		t.Fatalf("the turn left a registration behind: %v", err)
	}
}

// TestARegistrationIsRetiredWhenTheChildNeverStarts covers the early return no
// turn-end path can reach. Nothing will read that turn's stream and no stream
// goroutine exists to retire its registration, so a registration left behind
// here survives for the daemon's lifetime with the GitHub session inside it.
func TestARegistrationIsRetiredWhenTheChildNeverStarts(t *testing.T) {
	dir := t.TempDir()
	registry := oneCapabilityRegistry()
	backend, endpoint := backendWithEndpoint(t)
	r := request(t, dir, "unused")
	r.Command = filepath.Join(dir, "no-such-binary")
	if _, _, err := startWithRegistry(t, backend, context.Background(), r, registry); err == nil {
		t.Fatal("a launch with no binary reported success")
	}
	if ended := registry.ended(); ended != 1 {
		t.Fatalf("turn-ended finalizer ran %d times after a failed launch, want exactly 1", ended)
	}
	// Closing reports registrations no session revoked, which is the only signal
	// a leak of this shape produces at all.
	if err := endpoint.Close(context.Background()); err != nil {
		t.Fatalf("the failed launch left a registration behind: %v", err)
	}
}

// TestThePreviousTurnsRegistrationIsDeadBeforeTheNextTurnRuns is the ordering
// this wiring turns on, and the transcript is built to reproduce the exact race
// the ordering exists for. Turn one emits its result -- so the coordinator sees a
// terminal event and calls Continue -- and then leaves a descendant holding the
// inherited pipes, so turn one's own goroutine is still blocked on stderr when
// turn two launches. "Revoked at turn end" is therefore not yet true, and the
// only thing that can have retired turn one's authority is turn two's launch.
//
// Turn two proves it from where it matters: as the child, with turn one's token,
// over the same loopback endpoint.
func TestThePreviousTurnsRegistrationIsDeadBeforeTheNextTurnRuns(t *testing.T) {
	for _, tool := range []string{"curl", "python3"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is needed to hold turn one's pipes open while turn two probes its token", tool)
		}
	}
	dir := t.TempDir()
	registry := oneCapabilityRegistry()
	init := capabilityInitLine(dir)
	firstToken := filepath.Join(dir, "first-token")
	// Turn one records its own token, reports a result, and then detaches a
	// descendant that keeps the inherited stdout and stderr open past the
	// continuation. That descendant is what pins turn one's stream goroutine
	// before its own revocation.
	script := writeFakeClaude(t, dir, ""+
		"if [ ! -f "+firstToken+" ]; then\n"+
		"  printf '%s' \"$SYMPHONY_MCP_TOKEN\" > "+firstToken+"\n"+
		"  cat <<'EOF'\n"+init+"\n"+resultLine(false, "")+"\nEOF\n"+
		"  python3 -c 'import os,time; os.setsid(); time.sleep(30)' &\n"+
		"  sleep 30\n"+
		"else\n"+
		"  url=$(grep -o 'http://[0-9.]*:[0-9]*/mcp' "+filepath.Join(dir, "args.txt")+" | head -1)\n"+
		"  printf '%s' '{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/list\"}' > "+filepath.Join(dir, "stale.body")+"\n"+
		"  curl -sS -X POST -H \"Authorization: Bearer $(cat "+firstToken+")\" -H 'Content-Type: application/json'"+
		" --data-binary @"+filepath.Join(dir, "stale.body")+" -o /dev/null -w '%{http_code}' \"$url\" > "+filepath.Join(dir, "stale.status")+"\n"+
		"  cat <<'EOF'\n"+init+"\n"+resultLine(false, "")+"\nEOF\n"+
		"fi\n")

	backend, _ := backendWithEndpoint(t)
	session, events, err := startWithRegistry(t, backend, context.Background(), request(t, dir, script), registry)
	if err != nil {
		t.Fatal(err)
	}
	// Reading only as far as the terminal event is what the coordinator does, and
	// it is what leaves turn one's goroutine still running.
	for event := range events {
		if terminal(event.Kind) {
			break
		}
	}
	events, err = backend.Continue(context.Background(), session, "second turn")
	if err != nil {
		t.Fatal(err)
	}
	drain(t, events)
	if status := readFile(t, filepath.Join(dir, "stale.status")); status != "401" {
		t.Fatalf("turn one's token was still live inside turn two: HTTP %s", status)
	}
}

// TestNoProviderCredentialReachesTheChildEnvironment closes the gap binding
// providers to this backend opened. A provider-resolved credential -- a GitHub
// token frozen into the session at build -- has no configured name and no
// configured value, so neither of the environment's other two filters can see
// it; only the provider secret matcher can, and this backend was not passing one.
// The Codex backend always has.
func TestNoProviderCredentialReachesTheChildEnvironment(t *testing.T) {
	dir := t.TempDir()
	registry := oneCapabilityRegistry()
	script := writeFakeClaude(t, dir, "cat <<'EOF'\n"+capabilityInitLine(dir)+"\n"+
		resultLine(false, "")+"\nEOF\n")
	t.Setenv("PMR52_INHERITED_GITHUB_TOKEN", "prefix-provider-github-token-suffix")
	t.Setenv("PMR52_KEPT", "ordinary-value")

	backend, _ := backendWithEndpoint(t)
	id, err := newSessionID()
	if err != nil {
		t.Fatal(err)
	}
	s := &session{id: id, ctx: context.Background(), registry: registry, advertised: advertisedNames(registry),
		secretMatcher: func(candidate string) bool { return strings.Contains(candidate, "provider-github-token") }}
	backend.mu.Lock()
	backend.sessions[id] = s
	backend.mu.Unlock()
	events, err := backend.run(context.Background(), s, request(t, dir, script), false)
	if err != nil {
		t.Fatal(err)
	}
	drain(t, events)

	environment := readFile(t, filepath.Join(dir, "env.txt"))
	if strings.Contains(environment, "provider-github-token") {
		t.Fatal("a provider-resolved credential reached the child environment")
	}
	if !strings.Contains(environment, "ordinary-value") {
		t.Fatal("the provider secret matcher removed unrelated variables")
	}
	// The one credential the child is meant to hold survives every filter,
	// because it is added after them.
	if _, token := endpointFromChild(t, dir); token == "" {
		t.Fatal("the endpoint token was filtered out of the child environment")
	}
}

// TestAStaleEndpointTokenIsNeverInherited keeps the endpoint token's only source
// this launcher. Without the unconditional name block, a session with no
// capability at all would hand the child whatever SYMPHONY_MCP_TOKEN the
// operator's own shell happened to export.
func TestAStaleEndpointTokenIsNeverInherited(t *testing.T) {
	t.Setenv(endpointTokenEnvName, "operator-stale-token")
	for name, endpoint := range map[string]*capabilityEndpoint{
		"no capability endpoint": nil,
		"a live endpoint":        {url: "http://127.0.0.1:1/mcp", token: "this-turns-token"},
	} {
		t.Run(name, func(t *testing.T) {
			environment := filteredEnv(nil, settingsFunc(), nil, endpoint)
			for _, entry := range environment {
				if strings.Contains(entry, "operator-stale-token") {
					t.Fatalf("the child inherited a stale endpoint token: %q", entry)
				}
			}
			want := endpointTokenEnvName + "=this-turns-token"
			if contains(environment, want) != (endpoint != nil) {
				t.Fatalf("the child environment %v carried this turn's token unexpectedly", endpoint != nil)
			}
		})
	}
}

// --- fixtures ---

// initServer is the init event's MCP server shape, named so a table can build
// one. The decoded struct is deliberately anonymous in events.go.
type initServer = struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

// capabilityInitLine is the init echo a session with one advertised capability
// must produce: the endpoint connected, and exactly the six coding tools plus
// that capability's prefixed name. Anything else is a refusal, which is what the
// verifyInit table above enumerates.
func capabilityInitLine(dir string) string {
	return `{"type":"system","subtype":"init","session_id":"x","cwd":"` + workspaceOf(dir) +
		`","permissionMode":"dontAsk","tools":[` + allCodingTools + `,"` + mcpToolName(capability.NameGitHubPRContext) +
		`"],"mcp_servers":[{"name":"` + mcpServerName + `","status":"connected"}]}`
}

// scriptedRegistry stands in for a session's capability registry. What it fakes
// is only what it serves; what it records -- how many times a turn end ran the
// finalizer -- is the behaviour under test, and a real *capability.Registry
// answers that only through a live provider session against a live remote.
type scriptedRegistry struct {
	served []capability.Capability

	mu    sync.Mutex
	turns int
}

func (r *scriptedRegistry) Definitions() []capability.Definition {
	definitions := make([]capability.Definition, 0, len(r.served))
	for _, served := range r.served {
		definitions = append(definitions, served.Definition())
	}
	return definitions
}

func (r *scriptedRegistry) Lookup(name string) (capability.Capability, bool) {
	for _, served := range r.served {
		if served.Definition().Name == name {
			return served, true
		}
	}
	return nil, false
}

func (r *scriptedRegistry) TurnEnded(context.Context) {
	r.mu.Lock()
	r.turns++
	r.mu.Unlock()
}

func (r *scriptedRegistry) ended() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.turns
}

type scriptedCapability struct {
	definition capability.Definition
	result     capability.Result
}

func (c scriptedCapability) Definition() capability.Definition { return c.definition }
func (c scriptedCapability) Lifecycle() bool                   { return false }
func (c scriptedCapability) Prepare(json.RawMessage) (capability.Invocation, *capability.Failure) {
	return func(context.Context) (capability.Result, *capability.Failure) { return c.result, nil }, nil
}

func oneCapabilityRegistry() *scriptedRegistry {
	return &scriptedRegistry{served: []capability.Capability{scriptedCapability{
		definition: capability.Definition{Name: capability.NameGitHubPRContext, Description: "read review context",
			InputSchema: map[string]any{"type": "object", "additionalProperties": false}},
		result: capability.Result{Success: true, Payload: map[string]any{"state": "open"}},
	}}}
}

// backendWithEndpoint wires a backend to a real loopback endpoint, closed with
// the test. The endpoint is real rather than substituted because everything this
// file asserts -- that a token authenticates, that it stops authenticating, that
// a child can complete a handshake against it -- is only true of a live listener.
func backendWithEndpoint(t *testing.T) (*Backend, *mcpbridge.Server) {
	t.Helper()
	endpoint, err := mcpbridge.Listen()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = endpoint.Close(context.Background()) })
	return NewWithProviders(settingsFunc(), nil, nil, endpoint), endpoint
}

// startWithRegistry runs a first turn against a substituted registry. Start
// builds its own from the host providers, which is right in production and
// unusable here: the turn-end lifecycle under test is observable only through the
// registry. Everything after the registry -- the registration, the launch
// contract, the environment, the spawn, and every turn-end path -- is the
// production code path.
func startWithRegistry(t *testing.T, b *Backend, ctx context.Context, r domain.AgentRequest, registry mcpbridge.Capabilities) (domain.AgentSession, <-chan domain.Event, error) {
	t.Helper()
	id, err := newSessionID()
	if err != nil {
		t.Fatal(err)
	}
	s := &session{id: id, ctx: ctx, registry: registry, advertised: advertisedNames(registry)}
	b.mu.Lock()
	b.sessions[id] = s
	b.mu.Unlock()
	events, err := b.run(ctx, s, r, false)
	if err != nil {
		b.forget(id)
		return domain.AgentSession{}, nil, err
	}
	return domain.AgentSession{ID: id, ThreadID: id, TurnID: "1"}, events, nil
}

// mcpClientBody makes the fake binary a real MCP client: it takes the endpoint
// URL out of its own argument vector and the bearer token out of its own
// environment -- the only two places a real client can find them -- and speaks
// plain JSON over loopback HTTP.
func mcpClientBody(dir string) string {
	at := func(name string) string { return filepath.Join(dir, name) }
	post := func(step, body string) string {
		return "printf '%s' '" + body + "' > " + at(step+".body") + "\n" +
			"curl -sS -X POST -H \"Authorization: Bearer $" + endpointTokenEnvName + "\"" +
			" -H 'Content-Type: application/json' --data-binary @" + at(step+".body") +
			" -o " + at(step+".json") + " -w '%{http_code}' \"$url\" > " + at(step+".status") + "\n"
	}
	return "url=$(grep -o 'http://[0-9.]*:[0-9]*/mcp' " + at("args.txt") + " | head -1)\n" +
		post("initialize", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"fake-claude","version":"1"}}}`) +
		"printf '%s' '{\"jsonrpc\":\"2.0\",\"method\":\"notifications/initialized\"}' > " + at("initialized.body") + "\n" +
		post("tools-list", `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`) +
		post("tools-call", `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"`+capability.NameGitHubPRContext+`","arguments":{}}}`)
}

// endpointFromChild reads back the endpoint the child was actually given: the URL
// out of its arguments, the token out of its environment. Reading them from the
// child rather than from the registration is the point -- it is the same evidence
// a real client would have had.
func endpointFromChild(t *testing.T, dir string) (string, string) {
	t.Helper()
	var url, token string
	for _, arg := range readArgs(t, dir) {
		if start := strings.Index(arg, "http://"); start >= 0 && strings.Contains(arg, "/mcp") {
			url = arg[start : strings.Index(arg[start:], "/mcp")+start+len("/mcp")]
		}
	}
	for _, entry := range strings.Split(readFile(t, filepath.Join(dir, "env.txt")), "\n") {
		if name, value, found := strings.Cut(entry, "="); found && name == endpointTokenEnvName {
			token = value
		}
	}
	if url == "" || token == "" {
		t.Fatalf("the child was given url=%q token present=%v", url, token != "")
	}
	return url, token
}

// probeEndpoint asks the endpoint whether a token still authorizes anything.
func probeEndpoint(t *testing.T, url, token string) int {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, url, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	return response.StatusCode
}

func decodeJSONFile(t *testing.T, path string, into any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatalf("%s is not valid JSON (%v): %s", path, err, raw)
	}
}

func prefixed(names []string) []string {
	out := make([]string, 0, len(names))
	for _, name := range names {
		out = append(out, mcpToolName(name))
	}
	return out
}

// assertArgs compares an argument vector element by element, so a reordering or
// an inserted flag fails as loudly as a wrong value.
func assertArgs(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("argument vector has %d elements, want %d:\n got %q\nwant %q", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("argument %d = %q, want %q:\n got %q\nwant %q", i, got[i], want[i], got, want)
		}
	}
}
