package claude

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pmrrasmussen/symphony/internal/agentstream"
	"github.com/pmrrasmussen/symphony/internal/capability"
	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/domain"
	githubhost "github.com/pmrrasmussen/symphony/internal/github"
	"github.com/pmrrasmussen/symphony/internal/linear"
	"github.com/pmrrasmussen/symphony/internal/mcpbridge"
)

// The capability names this package renders into a launch contract. They are the
// registry's own constants, so a rename there breaks these tests rather than
// silently changing what a Claude session is told exists.
var allCapabilityNames = []string{
	capability.NameCreateFollowupIssue,
	capability.NameGitHubRefreshBaseRef,
	capability.NameGitHubPublishPR,
	capability.NameGitHubPRContext,
	capability.NameGitHubLandPR,
}

// TestTheLaunchContractWithoutACapabilityIsUnchanged is the Codex-parity
// statement. A workflow that configures no Symphony capability still takes this
// path, and the argument vector it produces has to be exactly the one it
// produced before the endpoint existed -- not merely equivalent. A byte-level
// golden is the only assertion that says so: an extra flag, a reordering, or an
// empty --mcp-config would all pass a set comparison.
//
// It is also what gives "no MCP server at all" in the init echo a single
// meaning. Configuration is the other half: it refuses a Claude workflow that
// configures a capability no session could advertise, so this argv cannot stand
// for a capability that was asked for and silently not granted.
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
		// Edit and Write are scoped to the write roots -- see scopedAllow --
		// so --allowedTools is no longer the bare tool surface.
		"--allowedTools", strings.Join(scopedAllow(rootsOf(t, r), codingTools), ","),
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
// session can actually be built with: none, all five, the Merging-state landing
// set, and a tracker-only follow-up set. Flag order is asserted along with the
// values because the contract is a fixed argument vector, and because the
// resume/session-id and model flags must stay last however many tools there are.
func TestTheLaunchContractPinsExactlyTheAdvertisedCapabilities(t *testing.T) {
	for name, names := range map[string][]string{
		"nothing advertised": nil,
		"all five":           allCapabilityNames,
		"landing only":       {capability.NameGitHubLandPR},
		"follow-up only":     {capability.NameCreateFollowupIssue},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			r := request(t, dir, "unused")
			endpoint := &capabilityEndpoint{url: "http://127.0.0.1:54321/mcp", token: "token-value", names: names}
			contract, err := launchArgs(r, "session-1", true, endpoint)
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
				// --tools is what decides a tool exists; --allowedTools is a
				// permission rule, like the payload's own Allow list, so Edit
				// and Write are scoped to the write roots there instead of
				// bare -- see scopedAllow.
				"--tools", tools,
				"--allowedTools", strings.Join(scopedAllow(rootsOf(t, r), wantTools), ","),
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
			var mcp mcpConfig
			if err := json.Unmarshal([]byte(flagValue(t, contract.args, "--mcp-config")), &mcp); err != nil {
				t.Fatalf("MCP configuration is not valid JSON: %v", err)
			}
			server, ok := mcp.MCPServers[mcpServerName]
			if !ok || len(mcp.MCPServers) != 1 {
				t.Fatalf("MCP configuration=%+v", mcp)
			}
			if server.Type != "http" || server.URL != endpoint.url {
				t.Fatalf("server=%+v", server)
			}
			// The payload's own permission allowlist must be the same rules
			// --allowedTools names. Under dontAsk anything not allowed is
			// refused, so two disagreeing statements of what is permitted
			// leave a reader unable to tell which one is authoritative.
			var rendered policy
			if err := json.Unmarshal([]byte(flagValue(t, contract.args, "--settings")), &rendered); err != nil {
				t.Fatal(err)
			}
			if strings.Join(rendered.Permissions.Allow, ",") != flagValue(t, contract.args, "--allowedTools") {
				t.Fatalf("permissions.allow=%v, want the same rules as --allowedTools %q", rendered.Permissions.Allow, flagValue(t, contract.args, "--allowedTools"))
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

// TestScopedAllowConfinesOnlyEditAndWrite is the unit-level statement of the
// PMR-156 fix: sandbox.filesystem.allowWrite governs Bash and its children
// only, so a bare "Edit"/"Write" permission rule would let either tool write
// anywhere under defaultMode dontAsk. scopedAllow is what closes that gap, and
// it must do so without touching any other tool's rule -- Bash's own
// confinement already comes from allowWrite, not from its permission rule, and
// a capability tool name is opaque to the write-root boundary entirely.
func TestScopedAllowConfinesOnlyEditAndWrite(t *testing.T) {
	roots := []string{"/work/tree", "/work/git/objects"}
	got := scopedAllow(roots, []string{"Bash", "Edit", "Glob", "Grep", "Read", "Write", "mcp__symphony__github_publish_pr"})
	want := []string{
		"Bash",
		"Edit(//work/tree/**)",
		"Edit(//work/git/objects/**)",
		"Glob",
		"Grep",
		"Read",
		"Write(//work/tree/**)",
		"Write(//work/git/objects/**)",
		"mcp__symphony__github_publish_pr",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("scopedAllow=%v, want %v", got, want)
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
				//
				// What this pins is that a returned Cancel has already retired
				// the session's authority. It does not pin that Cancel's own
				// revocation is what did it: the turn's shutdown reaches its
				// retirement microseconds after the kill, so this still passes
				// with Cancel's call deleted. That call is kept as the guard on
				// the path where the wait below does not complete -- the
				// coordinator bounds Cancel at five seconds -- and no test can
				// distinguish it.
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
		if event.Kind.Terminal() {
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
	// entered and release let a test hold an invocation in flight, which is what
	// a revocation's drain waits for.
	entered chan<- struct{}
	release <-chan struct{}
}

func (c scriptedCapability) Definition() capability.Definition { return c.definition }
func (c scriptedCapability) Lifecycle() bool                   { return false }
func (c scriptedCapability) Prepare(json.RawMessage) (capability.Invocation, *capability.Failure) {
	return func(context.Context) (capability.Result, *capability.Failure) {
		if c.entered != nil {
			c.entered <- struct{}{}
		}
		if c.release != nil {
			<-c.release
		}
		return c.result, nil
	}, nil
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
	// Bounded, because Close waits for in-flight requests: a test that leaves an
	// invocation blocked would otherwise hang the whole binary at cleanup
	// instead of reporting its own failure.
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = endpoint.Close(closeCtx)
	})
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

// awaitRetiring blocks until a turn's token stops authenticating, which is the
// observable edge of a revocation that has begun and not finished: Revoke clears
// the registration before it drains the invocation still in flight. It is how a
// test orders itself against a drain it cannot see, and the deadline is a hang
// guard rather than an assertion -- describe names what did not happen, so a
// wedge is reported against the waiting test.
func awaitRetiring(t *testing.T, url, token, describe string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for probeEndpoint(t, url, token) != http.StatusUnauthorized {
		if time.Now().After(deadline) {
			t.Fatalf("%s", describe)
		}
		time.Sleep(10 * time.Millisecond)
	}
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

// TestStartBindsTheHostProvidersAndTheirSecrets covers the block every other
// test in this file steps over. The capability tests build a session by hand so
// they can substitute a registry, which leaves Start's own preparation -- the
// handoff, the GitHub session, the secret matcher, and the Bindings the registry
// is built from -- asserted by nothing at all.
//
// That is the one function the whole wiring rests on, and its failure is silent
// by construction: drop the bindings and advertisedNames returns nil, so no
// --mcp-config is rendered, so verifyInit expects zero MCP servers and finds
// zero, so the session is approved and the turn ends completed with committed,
// unpublished work. Every gate passes. Only this test does not.
func TestStartBindsTheHostProvidersAndTheirSecrets(t *testing.T) {
	tracker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var query struct {
			Query string `json:"query"`
		}
		if err := json.NewDecoder(r.Body).Decode(&query); err != nil {
			t.Error(err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(query.Query, "SymphonyLinearHandoffIssue"):
			_, _ = w.Write([]byte(`{"data":{"issue":{"id":"issue-1","identifier":"PMR-52","title":"Parity","description":"safe",` +
				`"url":"https://linear.app/issue/PMR-52","project":{"id":"project-uuid","slugId":"project-1"},` +
				`"team":{"id":"team-1"},"state":{"id":"merging","name":"Merging"}}}}`))
		case strings.Contains(query.Query, "SymphonyLinearHandoffStates"):
			_, _ = w.Write([]byte(`{"data":{"team":{"id":"team-1","states":{"nodes":[{"id":"review","name":"In Review"},{"id":"backlog","name":"Backlog"}]}}}}`))
		default:
			t.Errorf("unexpected query: %s", query.Query)
		}
	}))
	defer tracker.Close()
	settings := config.Settings{
		Tracker: config.Tracker{
			Provider:              map[string]any{"api_key": "linear-api-secret", "project_slug_id": "project-1", "endpoint": tracker.URL},
			ActiveStates:          []string{"Merging"},
			HandoffState:          "In Review",
			FollowupIssueCreation: true,
		},
		GitHub: config.GitHub{Enabled: true, Owner: "owner", Repository: "repo", BaseBranch: "main",
			Token: "github-token-secret", Endpoint: tracker.URL, MergeState: "Merging", MergeMethod: "merge"},
	}
	snapshot := func() config.Settings { return settings }

	dir := t.TempDir()
	// The init echo has to name all five capabilities, which is itself part of
	// the assertion: a contract built from a registry missing its bindings would
	// refuse this turn.
	tools := allCodingTools
	for _, name := range allCapabilityNames {
		tools += `,"` + mcpToolName(name) + `"`
	}
	script := writeFakeClaude(t, dir, "cat <<'EOF'\n"+
		`{"type":"system","subtype":"init","cwd":"`+workspaceOf(dir)+`","permissionMode":"dontAsk","tools":[`+tools+
		`],"mcp_servers":[{"name":"symphony","status":"connected"}]}`+"\n"+resultLine(false, "")+"\nEOF\n")
	t.Setenv("PMR52_INHERITS_THE_FORGE_TOKEN", "prefix-github-token-secret-suffix")

	mcpEndpoint, err := mcpbridge.Listen()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mcpEndpoint.Close(context.Background()) })
	backend := NewWithProviders(snapshot, linear.NewHandoff(snapshot), githubhost.New(snapshot, nil), mcpEndpoint)

	r := request(t, dir, script)
	r.Issue = domain.Issue{ID: "issue-1", Identifier: "PMR-52", State: "Merging"}
	agentSession, events, err := backend.Start(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	collected := drain(t, events)

	backend.mu.Lock()
	s := backend.sessions[agentSession.ID]
	backend.mu.Unlock()
	if s == nil {
		t.Fatal("the session was forgotten")
	}
	// The advertised set is the registry's own, so this is also what --tools was
	// pinned to and what tools/list serves.
	if strings.Join(s.advertised, ",") != strings.Join(allCapabilityNames, ",") {
		t.Fatalf("advertised=%v, want %v", s.advertised, allCapabilityNames)
	}
	if s.secretMatcher == nil {
		t.Fatal("Start bound providers without a secret matcher, so their resolved credentials reach the child")
	}
	// Both providers' credentials must be recognized: only the matcher can see
	// them, because neither has a configured name or a configured value.
	for _, secret := range []string{"prefix-github-token-secret-suffix", "carries linear-api-secret inside"} {
		if !s.secretMatcher(secret) {
			t.Fatalf("the session secret matcher does not recognize %q", secret)
		}
	}
	if s.secretMatcher("ordinary-value") {
		t.Fatal("the session secret matcher matches unrelated values")
	}
	if environment := readFile(t, filepath.Join(dir, "env.txt")); strings.Contains(environment, "github-token-secret") {
		t.Fatal("a provider-resolved credential reached the child environment")
	}
	if lastKind(t, collected).Kind != domain.EventCompleted {
		t.Fatalf("a fully bound session did not complete: %v", kinds(collected))
	}
}

// TestTheAdvertisedSetIsTheRegistrysOwn is the parity assertion PMR-52 asked
// for, in the only form this package can make it: the same bindings that give
// internal/codex its five dynamic tools give this backend the same five names,
// in the same order, with the mcp__symphony__ prefix applied to each and nothing
// added, dropped, or reordered. internal/codex asserts the app-server half of
// the same statement over the same bindings, so neither transport can quietly
// grow a capability set of its own.
func TestTheAdvertisedSetIsTheRegistrysOwn(t *testing.T) {
	settings := config.Settings{}
	settings.Tracker.FollowupIssueCreation = true
	settings.GitHub.MergeState = "Merging"
	registry := capability.Build(capability.Bindings{
		Settings: settings,
		Issue:    domain.Issue{Identifier: "PMR-52", State: "Merging"},
		Handoff:  &linear.HandoffSession{},
		GitHub:   &githubhost.Session{},
	})
	definitions := registry.Definitions()
	names := advertisedNames(registry)
	if len(names) != len(definitions) {
		t.Fatalf("advertised %d names for %d definitions", len(names), len(definitions))
	}
	for i, definition := range definitions {
		if names[i] != definition.Name {
			t.Fatalf("name %d = %q, want the registry's %q", i, names[i], definition.Name)
		}
	}
	if strings.Join(names, ",") != strings.Join(allCapabilityNames, ",") {
		t.Fatalf("advertised=%v, want %v", names, allCapabilityNames)
	}
	contract, err := launchArgs(request(t, t.TempDir(), "unused"), "session-1", false,
		&capabilityEndpoint{url: "http://127.0.0.1:1/mcp", token: "t", names: names})
	if err != nil {
		t.Fatal(err)
	}
	want := append(append([]string(nil), codingTools...), prefixed(definitions2names(definitions))...)
	if strings.Join(contract.tools, ",") != strings.Join(want, ",") {
		t.Fatalf("tool surface=%v, want %v", contract.tools, want)
	}
}

func definitions2names(definitions []capability.Definition) []string {
	names := make([]string, 0, len(definitions))
	for _, definition := range definitions {
		names = append(names, definition.Name)
	}
	return names
}

// TestTheNextTurnCannotStartUntilThePreviousRegistrationIsFullyRetired is the
// falsifiable form of the ordering every document in this repository asserts.
//
// The stale-token test above catches the next-turn revocation being deleted
// outright, but not the two mutations that actually threaten the invariant:
// moving it to after spawn, and releasing the session's registration slot before
// the revocation it started has finished. Neither is visible to a probe from the
// child, because the parent wins that race against a starting child every time.
//
// This transcript removes the race in both directions. Turn one makes a
// capability call that blocks inside the invocation and then exits, so its own
// stream goroutine begins retiring the registration and parks in the drain --
// which the test waits for by polling until turn one's token stops
// authenticating, the observable edge of a revocation that has started and not
// finished. Only then is a continuation requested. With the ordering right, that
// launch finds the slot still occupied and blocks on the same latch, so turn
// two's child provably cannot exist until this test releases the call. Move the
// revocation after spawn, or clear the slot before the revocation completes, and
// turn two's child starts immediately -- concurrently with a capability call from
// the previous turn against the same provider session, which is the hazard.
func TestTheNextTurnCannotStartUntilThePreviousRegistrationIsFullyRetired(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl is needed for the child to hold a capability call open")
	}
	dir := t.TempDir()
	entered, release := make(chan struct{}, 1), make(chan struct{})
	registry := &scriptedRegistry{served: []capability.Capability{scriptedCapability{
		definition: capability.Definition{Name: capability.NameGitHubPRContext, Description: "read review context",
			InputSchema: map[string]any{"type": "object", "additionalProperties": false}},
		result:  capability.Result{Success: true, Payload: map[string]any{"state": "open"}},
		entered: entered, release: release,
	}}}
	init, marker, first := capabilityInitLine(dir), filepath.Join(dir, "second-turn-started"), filepath.Join(dir, "first-turn-ran")
	script := writeFakeClaude(t, dir, ""+
		"if [ ! -f "+first+" ]; then\n"+
		"  : > "+first+"\n"+
		"  url=$(grep -o 'http://[0-9.]*:[0-9]*/mcp' "+filepath.Join(dir, "args.txt")+" | head -1)\n"+
		"  printf '%s' '{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\",\"params\":{\"name\":\""+
		capability.NameGitHubPRContext+"\",\"arguments\":{}}}' > "+filepath.Join(dir, "held.body")+"\n"+
		// The call is detached from the inherited stdout and stderr. A curl
		// holding those keeps this turn's read loop from ever seeing EOF, so
		// the turn could not end and nothing would retire its registration.
		"  curl -sS -X POST -H \"Authorization: Bearer $"+endpointTokenEnvName+"\" -H 'Content-Type: application/json'"+
		" --data-binary @"+filepath.Join(dir, "held.body")+" -o /dev/null \"$url\" >/dev/null 2>&1 &\n"+
		"  cat <<'EOF'\n"+init+"\n"+resultLine(false, "")+"\nEOF\n"+
		// Held open until the test has seen the invocation start, so this turn
		// cannot end -- and its registration cannot be drained -- before there
		// is something in flight for the drain to wait on.
		"  while [ ! -f "+filepath.Join(dir, "in-flight")+" ]; do sleep 0.02; done\n"+
		"else\n"+
		"  : > "+marker+"\n"+
		"  cat <<'EOF'\n"+init+"\n"+resultLine(false, "")+"\nEOF\n"+
		"fi\n")

	backend, _ := backendWithEndpoint(t)
	session, events, err := startWithRegistry(t, backend, context.Background(), request(t, dir, script), registry)
	if err != nil {
		t.Fatal(err)
	}
	for event := range events {
		if event.Kind.Terminal() {
			break
		}
	}
	select {
	case <-entered:
	case <-time.After(20 * time.Second):
		t.Fatal("turn one never reached the capability invocation")
	}
	// The invocation is in flight, so turn one may now end. Its stream then
	// retires the registration and parks in the drain; waiting for the token to
	// stop authenticating is the observable edge of a revocation that has begun
	// and cannot finish, which is what the next launch has to race.
	url, token := endpointFromChild(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "in-flight"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	awaitRetiring(t, url, token, "turn one's stream never began retiring its registration")

	continued := make(chan error, 1)
	var second <-chan domain.Event
	go func() {
		var err error
		second, err = backend.Continue(context.Background(), session, "second turn")
		continued <- err
	}()
	// The invocation is in flight, so the launch must be parked behind it.
	// Anything that got past started a turn concurrently with a live capability
	// call from the previous one.
	select {
	case err := <-continued:
		t.Fatalf("the next turn launched while a capability call from the previous turn was still in flight (err=%v)", err)
	case <-time.After(750 * time.Millisecond):
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("turn two's child ran before turn one's registration was fully retired")
	}

	close(release)
	if err := <-continued; err != nil {
		t.Fatal(err)
	}
	drain(t, second)
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("turn two never ran: %v", err)
	}
}

// TestAnExpiredRevocationIsReportedOnceHoweverManyPathsRetireIt covers the latch
// and the reporting rule around it. An expired drain or finalizer is the code
// knowingly abandoning an ordering invariant; nothing in mcpbridge logs, so an
// unreported expiry is invisible, and a doubly reported one reads as two.
func TestAnExpiredRevocationIsReportedOnceHoweverManyPathsRetireIt(t *testing.T) {
	bridge := &failingRegistration{err: mcpbridge.ErrDrainExpired}
	held := &registration{bridge: bridge}
	s := &session{id: "s", ctx: context.Background(), endpoint: held}

	if err := s.retireEndpoint(nil); !errors.Is(err, mcpbridge.ErrDrainExpired) {
		t.Fatalf("the retiring path reported %v, want the drain expiry", err)
	}
	s.mu.Lock()
	cleared := s.endpoint == nil
	s.mu.Unlock()
	if !cleared {
		t.Fatal("a retired registration is still the session's live one")
	}
	// The turn's own shutdown arrives second and must neither revoke again nor
	// report the same expiry a second time.
	if err := s.retireEndpoint(held); err != nil {
		t.Fatalf("the losing path reported %v, want nothing", err)
	}
	if calls := bridge.revocations(); calls != 1 {
		t.Fatalf("the registration was revoked %d times, want exactly 1", calls)
	}
}

// TestAnExpiredRevocationReachesTheNextTurnsStream covers the emission site the
// previous turn's stream cannot serve: by the time this expiry is known, that
// stream is closed.
func TestAnExpiredRevocationReachesTheNextTurnsStream(t *testing.T) {
	dir := t.TempDir()
	registry := oneCapabilityRegistry()
	script := writeFakeClaude(t, dir, "cat <<'EOF'\n"+capabilityInitLine(dir)+"\n"+resultLine(false, "")+"\nEOF\n")
	backend, _ := backendWithEndpoint(t)
	id, err := newSessionID()
	if err != nil {
		t.Fatal(err)
	}
	s := &session{id: id, ctx: context.Background(), registry: registry, advertised: advertisedNames(registry),
		endpoint: &registration{bridge: &failingRegistration{err: errors.Join(mcpbridge.ErrDrainExpired, mcpbridge.ErrFinalizerExpired)}}}
	backend.mu.Lock()
	backend.sessions[id] = s
	backend.mu.Unlock()
	events, err := backend.run(context.Background(), s, request(t, dir, script), true)
	if err != nil {
		t.Fatal(err)
	}
	collected := drain(t, events)
	var reported int
	for _, event := range collected {
		if event.Kind != domain.EventDiagnostic || !strings.Contains(event.Message, "capability endpoint revocation") {
			continue
		}
		reported++
		for _, reason := range []string{"in flight", "finalizer"} {
			if !strings.Contains(event.Message, reason) {
				t.Fatalf("the diagnostic lost a reason: %q", event.Message)
			}
		}
	}
	if reported != 1 {
		t.Fatalf("an expired revocation produced %d diagnostics, want exactly 1: %v", reported, kinds(collected))
	}
	if lastKind(t, collected).Kind != domain.EventCompleted {
		t.Fatal("an expired revocation of the previous turn failed this one")
	}
}

// TestReportRetirementIsSilentOnASuccessfulRevocation pins the shared emission
// helper both retirement sites use, including the one inside stream's defers,
// which cannot be reached with a substituted registration.
func TestReportRetirementIsSilentOnASuccessfulRevocation(t *testing.T) {
	events := agentstream.NewSink(eventBuffer)
	reportRetirement(events, nil)
	if len(events.Events()) != 0 {
		t.Fatalf("a clean revocation reported %d events", len(events.Events()))
	}
	reportRetirement(events, mcpbridge.ErrFinalizerExpired)
	events.Close()
	collected := drain(t, events.Events())
	if len(collected) != 1 || collected[0].Kind != domain.EventDiagnostic {
		t.Fatalf("collected=%v", kinds(collected))
	}
	if !strings.Contains(collected[0].Message, "finalizer") {
		t.Fatalf("diagnostic=%q", collected[0].Message)
	}
}

// TestTheEndpointNeverReachesADiagnostic covers the one path where raw child
// output becomes an event message. observability.Text redacts credential-shaped
// text, and a loopback URL is not credential-shaped, so a CLI that prints its
// MCP configuration or a connect failure to stderr would put the endpoint in a
// diagnostic and from there into the log.
func TestTheEndpointNeverReachesADiagnostic(t *testing.T) {
	dir := t.TempDir()
	registry := oneCapabilityRegistry()
	script := writeFakeClaude(t, dir, "cat <<'EOF'\n"+capabilityInitLine(dir)+"\nEOF\n"+
		"url=$(grep -o 'http://[0-9.]*:[0-9]*/mcp' "+filepath.Join(dir, "args.txt")+" | head -1)\n"+
		"echo \"mcp connect failed: $url token $"+endpointTokenEnvName+"\" >&2\n"+
		"exit 3\n")
	backend, _ := backendWithEndpoint(t)
	_, events, err := startWithRegistry(t, backend, context.Background(), request(t, dir, script), registry)
	if err != nil {
		t.Fatal(err)
	}
	collected := drain(t, events)
	url, token := endpointFromChild(t, dir)
	var diagnostics int
	for _, event := range collected {
		if strings.Contains(event.Message, url) || strings.Contains(event.Message, token) {
			t.Fatalf("an event carried the endpoint: %q", event.Message)
		}
		if event.Kind == domain.EventDiagnostic && strings.Contains(event.Message, "mcp connect failed") {
			diagnostics++
		}
	}
	// The diagnostic itself must survive: redaction, not suppression.
	if diagnostics != 1 {
		t.Fatalf("the child's stderr produced %d diagnostics, want exactly 1: %v", diagnostics, kinds(collected))
	}
}

// failingRegistration is a registration whose revocation reports an abandoned
// invariant. It exists because a real one cannot: mcpbridge's drain and
// finalizer bounds are private and are only ever the production constants, so
// reaching ErrDrainExpired for real costs two minutes of a wedged provider call.
type failingRegistration struct {
	err error

	mu    sync.Mutex
	calls int
}

func (f *failingRegistration) URL() string   { return "http://127.0.0.1:1/mcp" }
func (f *failingRegistration) Token() string { return "substituted" }

func (f *failingRegistration) Revoke(context.Context) error {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	return f.err
}

func (f *failingRegistration) revocations() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// TestTheRenderedNameMatchesTheNameTheCLIWillServe pins the one duplicated fact
// in this change. config.MCPToolPrefix is what the rendered prompt tells the
// model to call; mcpToolName is what --tools actually pins and what the CLI
// echoes back. They are in different packages because importing this launcher
// from internal/config is a cycle, so the only thing that can keep them equal is
// this assertion. Break either side and the prompt names a tool the session does
// not serve, with no diagnostic anywhere: the launch contract is still
// self-consistent, so verifyInit approves the turn.
func TestTheRenderedNameMatchesTheNameTheCLIWillServe(t *testing.T) {
	for _, name := range allCapabilityNames {
		if got, want := mcpToolName(name), config.MCPToolPrefix+name; got != want {
			t.Fatalf("the CLI will serve %q but the prompt names %q", got, want)
		}
	}
}

// TestStartRefusesToRunAPromiseTheSessionCannotKeep is the launch-time
// consistency guard, in the form that reaches it without a hand-built session.
//
// The divergence is real rather than contrived: an issue whose identifier
// contains no branch-safe character has no deterministic branch, so
// github.Manager prepares no session for it and the registry advertises no
// GitHub capability -- while configuration still promises host-side publish and
// the coordinator has already rendered a prompt saying so. Without the guard the
// turn runs, the model finds no publish tool, and the run ends completed with
// committed, unpublished work; nothing in the launch contract is inconsistent, so
// verifyInit approves it.
func TestStartRefusesToRunAPromiseTheSessionCannotKeep(t *testing.T) {
	tracker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var query struct {
			Query string `json:"query"`
		}
		if err := json.NewDecoder(r.Body).Decode(&query); err != nil {
			t.Error(err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(query.Query, "SymphonyLinearHandoffIssue"):
			_, _ = w.Write([]byte(`{"data":{"issue":{"id":"issue-1","identifier":"###","title":"Parity","description":"safe",` +
				`"url":"https://linear.app/issue/x","project":{"id":"project-uuid","slugId":"project-1"},` +
				`"team":{"id":"team-1"},"state":{"id":"progress","name":"In Progress"}}}}`))
		case strings.Contains(query.Query, "SymphonyLinearHandoffStates"):
			_, _ = w.Write([]byte(`{"data":{"team":{"id":"team-1","states":{"nodes":[{"id":"review","name":"In Review"}]}}}}`))
		default:
			t.Errorf("unexpected query: %s", query.Query)
		}
	}))
	defer tracker.Close()
	settings := config.Settings{
		Tracker: config.Tracker{
			Provider:     map[string]any{"api_key": "linear-api-secret", "project_slug_id": "project-1", "endpoint": tracker.URL},
			ActiveStates: []string{"In Progress"},
			HandoffState: "In Review",
		},
		GitHub: config.GitHub{Enabled: true, Owner: "owner", Repository: "repo", BaseBranch: "main",
			Token: "github-token-secret", Endpoint: tracker.URL},
	}
	snapshot := func() config.Settings { return settings }
	if !settings.HostSidePublishPromised() {
		t.Fatal("these settings do not promise host-side publish, so the guard is not under test")
	}

	dir := t.TempDir()
	script := writeFakeClaude(t, dir, "cat <<'EOF'\n"+initLine(dir, allCodingTools)+"\n"+resultLine(false, "")+"\nEOF\n")
	mcpEndpoint, err := mcpbridge.Listen()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mcpEndpoint.Close(context.Background()) })
	backend := NewWithProviders(snapshot, linear.NewHandoff(snapshot), githubhost.New(snapshot, nil), mcpEndpoint)

	r := request(t, dir, script)
	r.Issue = domain.Issue{ID: "issue-1", Identifier: "###", State: "In Progress"}
	agentSession, events, err := backend.Start(context.Background(), r)
	if err == nil {
		t.Fatalf("a session that cannot publish was started anyway: %+v", agentSession)
	}
	if !strings.Contains(err.Error(), capability.NameGitHubPublishPR) {
		t.Fatalf("the refusal does not name the missing capability: %v", err)
	}
	if events != nil {
		t.Fatal("a refused launch returned an event stream")
	}
	// No child may run: the guard exists precisely because a turn that starts is
	// a turn that ends completed with unpublished work.
	if _, statErr := os.Stat(filepath.Join(dir, "args.txt")); statErr == nil {
		t.Fatal("the refused launch still spawned the CLI")
	}
	backend.mu.Lock()
	live := len(backend.sessions)
	backend.mu.Unlock()
	if live != 0 {
		t.Fatalf("a refused launch left %d sessions registered", live)
	}
}

// TestTheGuardRefusesEachDivergenceItClaimsToCover exercises verifyPromises
// directly, over the three failures it distinguishes and the sessions it must
// not refuse.
//
// It replaces a test that asserted two independent facts and never evaluated the
// guard at all: mutating the guard into a blanket refusal of every promised
// publish left that test passing, and only a pre-existing test caught it. The
// acceptance rows here are what make a blanket refusal fail.
func TestTheGuardRefusesEachDivergenceItClaimsToCover(t *testing.T) {
	publish := capability.NameGitHubPublishPR
	bound := config.Settings{
		Tracker: config.Tracker{HandoffState: "In Review"},
		GitHub:  config.GitHub{Enabled: true},
	}
	prefixedPrompt := "task\n\n" + bound.DeliveryInstructions(config.ClaudeAgentBackend)
	barePrompt := "task\n\n" + bound.DeliveryInstructions(config.DefaultAgentBackend)

	for name, tc := range map[string]struct {
		settings   config.Settings
		prompt     string
		advertised []string
		want       string
	}{
		"a fully bound session is accepted": {
			settings: bound, prompt: prefixedPrompt, advertised: []string{publish, capability.NameGitHubPRContext},
		},
		"a manual run with nothing advertised is accepted": {
			settings: config.Settings{}, prompt: "task\n\n" + (config.Settings{}).DeliveryInstructions(config.ClaudeAgentBackend),
		},
		"a follow-up-only session is accepted": {
			settings:   config.Settings{Tracker: config.Tracker{FollowupIssueCreation: true}},
			prompt:     "task\n\n" + config.Settings{Tracker: config.Tracker{FollowupIssueCreation: true}}.DeliveryInstructions(config.ClaudeAgentBackend),
			advertised: []string{capability.NameCreateFollowupIssue},
		},
		// The settings term: this snapshot promises publish and the session
		// serves none, which is the degenerate-identifier route.
		"settings promise publish with nothing advertised": {
			settings: bound, prompt: prefixedPrompt, want: "advertises no " + publish,
		},
		// The prompt term, and the reload this guard exists for: the prompt was
		// rendered while github was enabled, the snapshot read here has it
		// disabled, so the settings term is false and only the prompt sees it.
		"a reload disabled github after the prompt was rendered": {
			settings: config.Settings{}, prompt: prefixedPrompt, want: "advertises no " + publish,
		},
		// The reverse direction: publish reachable with no state to hand off to.
		"publish advertised with no handoff state": {
			settings: config.Settings{GitHub: config.GitHub{Enabled: true}}, prompt: barePrompt,
			advertised: []string{publish}, want: "no tracker.provider.handoff_state",
		},
		// The naming term, which is the defect this pull request exists to fix:
		// a prompt rendered for the wrong backend names the bare tool while the
		// CLI is pinned to the prefixed one.
		// The row that matters most, and the one whose absence let a guard that
		// refused any bare mention look correct: a real dispatch prompt is a
		// repository-owned body that names Symphony's tools bare, followed by the
		// guidance that maps them. This must be accepted -- this repository's own
		// WORKFLOW.md body names them seven times -- or every claude dispatch
		// refuses at session_start.
		"a repository body naming tools bare under the mapping rule is accepted": {
			settings: bound,
			prompt: "Call github_publish_pr once the worktree is clean, read github_pr_context for\n" +
				"feedback, call github_land_pr in Merging, and use create_followup_issue for\n" +
				"out-of-scope work.\n\n" + bound.DeliveryInstructions(config.ClaudeAgentBackend),
			advertised: []string{publish, capability.NameGitHubPRContext},
		},
		// The same body with the guidance rendered for the wrong backend: the
		// bare names are now unmapped, which is the whole failure.
		"a repository body naming tools bare with no mapping rule is refused": {
			settings: bound,
			prompt: "Call github_publish_pr once the worktree is clean.\n\n" +
				bound.DeliveryInstructions(config.DefaultAgentBackend),
			advertised: []string{publish, capability.NameGitHubPRContext},
			want:       "naming rule to map it",
		},
		// A whitespace-only handoff state promises nothing, prepares nothing, and
		// must therefore not be refused. Without the TrimSpace in
		// HostSidePublishPromised the promise is true, the session serves nothing,
		// and every launch refuses at session_start with retry and backoff.
		"a whitespace handoff state neither promises nor refuses": {
			settings: config.Settings{GitHub: config.GitHub{Enabled: true}, Tracker: config.Tracker{HandoffState: "   "}},
			prompt:   "task",
		},
		"the prompt names an advertised tool bare": {
			settings: bound, prompt: barePrompt, advertised: []string{publish, capability.NameGitHubPRContext},
			want: "naming rule to map it",
		},
		"a bare name with no mapping rule at all is refused": {
			settings:   config.Settings{Tracker: config.Tracker{FollowupIssueCreation: true}},
			prompt:     "capture leftovers with create_followup_issue",
			advertised: []string{capability.NameCreateFollowupIssue},
			want:       "naming rule to map it",
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := verifyPromises(tc.settings, tc.prompt, tc.advertised)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("a consistent session was refused: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("a divergent session was accepted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("refusal %q does not report %q", err, tc.want)
			}
		})
	}
}

// TestStartRefusesAPromptRenderedForTheWrongBackend is the naming refusal through
// Start rather than through verifyPromises, on a session whose providers are all
// really bound. It is the launch-time half of what the coordinator's dispatch
// test asserts at the other end: between them, a prompt rendered for the wrong
// backend fails at the call site and at the launch.
func TestStartRefusesAPromptRenderedForTheWrongBackend(t *testing.T) {
	backend, dir, settings := boundBackend(t)
	// This session advertises refresh, publish, and context, so the init echo
	// has to name all three prefixed tools and the connected server, or
	// verifyInit refuses the accepted launch below for an unrelated reason.
	tools := allCodingTools
	for _, name := range []string{capability.NameGitHubRefreshBaseRef, capability.NameGitHubPublishPR, capability.NameGitHubPRContext} {
		tools += `,"` + mcpToolName(name) + `"`
	}
	script := writeFakeClaude(t, dir, "cat <<'EOF'\n"+
		`{"type":"system","subtype":"init","cwd":"`+workspaceOf(dir)+`","permissionMode":"dontAsk","tools":[`+tools+
		`],"mcp_servers":[{"name":"symphony","status":"connected"}]}`+"\n"+resultLine(false, "")+"\nEOF\n")
	r := request(t, dir, script)
	r.Issue = domain.Issue{ID: "issue-1", Identifier: "PMR-52", State: "In Progress"}
	// Exactly what the coordinator would hand this session if it resolved the
	// backend wrongly: valid guidance, bare tool names.
	r.Prompt = "task\n\n" + settings.DeliveryInstructions(config.DefaultAgentBackend)

	agentSession, events, err := backend.Start(context.Background(), r)
	if err == nil {
		t.Fatalf("a prompt naming tools this session does not serve was accepted: %+v", agentSession)
	}
	if !strings.Contains(err.Error(), config.MCPToolPrefix) {
		t.Fatalf("the refusal does not name the missing prefix: %v", err)
	}
	if events != nil {
		t.Fatal("a refused launch returned an event stream")
	}
	if _, statErr := os.Stat(filepath.Join(dir, "args.txt")); statErr == nil {
		t.Fatal("the refused launch still spawned the CLI")
	}

	// And the same session with the prompt it should have been given starts and
	// completes, so the refusal above is about the naming and not about the
	// session.
	r.Prompt = "task\n\n" + settings.DeliveryInstructions(config.ClaudeAgentBackend)
	if _, events, err = backend.Start(context.Background(), r); err != nil {
		t.Fatalf("a correctly rendered prompt was refused: %v", err)
	}
	if lastKind(t, drain(t, events)).Kind != domain.EventCompleted {
		t.Fatal("a consistent session did not complete")
	}
}

// TestRefreshBaseRefAdvertisedDoesNotWidenSandboxWriteGrants pins the
// acceptance criterion the whole design rationale for refresh_base_ref rests
// on: a session advertising the capability gets exactly the same sandbox
// write grant as one that does not -- the workspace plus its two narrow Git
// metadata roots, never the shared Git common directory those roots live in.
// That common directory holds refs/remotes/origin/<base> and packed-refs;
// granting it directly would reopen the source-repository write access
// PMR-65 closed, which is the whole reason this fetch is host-mediated rather
// than a sandbox change (PMR-141).
func TestRefreshBaseRefAdvertisedDoesNotWidenSandboxWriteGrants(t *testing.T) {
	backend, dir, settings := boundBackend(t)
	tools := allCodingTools
	for _, name := range []string{capability.NameGitHubRefreshBaseRef, capability.NameGitHubPublishPR, capability.NameGitHubPRContext} {
		tools += `,"` + mcpToolName(name) + `"`
	}
	script := writeFakeClaude(t, dir, "cat <<'EOF'\n"+
		`{"type":"system","subtype":"init","cwd":"`+workspaceOf(dir)+`","permissionMode":"dontAsk","tools":[`+tools+
		`],"mcp_servers":[{"name":"symphony","status":"connected"}]}`+"\n"+resultLine(false, "")+"\nEOF\n")
	r := request(t, dir, script)
	r.Issue = domain.Issue{ID: "issue-1", Identifier: "PMR-52", State: "In Progress"}
	r.Prompt = "task\n\n" + settings.DeliveryInstructions(config.ClaudeAgentBackend)

	_, events, err := backend.Start(context.Background(), r)
	if err != nil {
		t.Fatalf("Start refused a session advertising refresh_base_ref: %v", err)
	}
	drain(t, events)

	var rendered policy
	if err := json.Unmarshal([]byte(flagValue(t, readArgs(t, dir), "--settings")), &rendered); err != nil {
		t.Fatalf("settings payload is not valid JSON: %v", err)
	}
	want := map[string]bool{r.Workspace: true}
	for _, root := range r.GitMetadataRoots {
		want[root] = true
	}
	if len(rendered.Sandbox.Filesystem.AllowWrite) != len(want) {
		t.Fatalf("allowWrite=%v, want exactly %v", rendered.Sandbox.Filesystem.AllowWrite, want)
	}
	commonDir := filepath.Dir(r.GitMetadataRoots[0])
	for _, root := range rendered.Sandbox.Filesystem.AllowWrite {
		if !want[root] {
			t.Fatalf("allowWrite granted an unexpected root %q", root)
		}
		if root == commonDir {
			t.Fatal("allowWrite granted the shared Git common directory itself, reopening PMR-65")
		}
	}
}

// boundBackend is a Claude backend whose Linear and GitHub providers really do
// prepare sessions, against a scripted tracker. It is the fixture for asserting
// what Start does with a session that has every capability available to it.
func boundBackend(t *testing.T) (*Backend, string, config.Settings) {
	t.Helper()
	tracker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var query struct {
			Query string `json:"query"`
		}
		if err := json.NewDecoder(r.Body).Decode(&query); err != nil {
			t.Error(err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(query.Query, "SymphonyLinearHandoffIssue"):
			_, _ = w.Write([]byte(`{"data":{"issue":{"id":"issue-1","identifier":"PMR-52","title":"Parity","description":"safe",` +
				`"url":"https://linear.app/issue/PMR-52","project":{"id":"project-uuid","slugId":"project-1"},` +
				`"team":{"id":"team-1"},"state":{"id":"progress","name":"In Progress"}}}}`))
		case strings.Contains(query.Query, "SymphonyLinearHandoffStates"):
			_, _ = w.Write([]byte(`{"data":{"team":{"id":"team-1","states":{"nodes":[{"id":"review","name":"In Review"}]}}}}`))
		default:
			t.Errorf("unexpected query: %s", query.Query)
		}
	}))
	t.Cleanup(tracker.Close)
	settings := config.Settings{
		Tracker: config.Tracker{
			Provider:     map[string]any{"api_key": "linear-api-secret", "project_slug_id": "project-1", "endpoint": tracker.URL},
			ActiveStates: []string{"In Progress"},
			HandoffState: "In Review",
		},
		GitHub: config.GitHub{Enabled: true, Owner: "owner", Repository: "repo", BaseBranch: "main",
			Token: "github-token-secret", Endpoint: tracker.URL},
	}
	snapshot := func() config.Settings { return settings }
	endpoint, err := mcpbridge.Listen()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = endpoint.Close(context.Background()) })
	return NewWithProviders(snapshot, linear.NewHandoff(snapshot), githubhost.New(snapshot, nil), endpoint), t.TempDir(), settings
}

// TestABoundGitHubManagerWithoutAHandoffStillStripsItsToken is the counterpart
// to internal/codex's test of the same name, and the same hole: a workflow with
// github.enabled but no handoff_state prepares no Linear handoff, so
// githubhost.Manager.PrepareWithSettings returns no session, so the session's
// matcher had nothing to consult and answered false for every candidate --
// including the forge token the bound manager holds. Both backends built that
// matcher themselves, identically, which is why capability.SecretMatcher now
// owns it and takes the manager as its fallback.
func TestABoundGitHubManagerWithoutAHandoffStillStripsItsToken(t *testing.T) {
	settings := config.Settings{
		Tracker: config.Tracker{Provider: map[string]any{"api_key": "provider-tracker-key"}, ActiveStates: []string{"Todo"}},
		GitHub: config.GitHub{Enabled: true, Owner: "owner", Repository: "repo", BaseBranch: "main",
			Token: "provider-forge-token", Endpoint: "https://api.github.com", MergeState: "Merging", MergeMethod: "merge"},
	}
	snapshot := func() config.Settings { return settings }
	dir := t.TempDir()
	script := writeFakeClaude(t, dir, "cat <<'EOF'\n"+initLine(dir, allCodingTools)+"\n"+resultLine(false, "")+"\nEOF\n")
	t.Setenv("PMR94_INHERITED_FORGE", "prefix-provider-forge-token-suffix")
	t.Setenv("PMR94_KEPT", "ordinary-value")

	// No handoff provider at all, so nothing can prepare a GitHub session: the
	// manager is the only thing that knows this token.
	backend := NewWithProviders(snapshot, nil, githubhost.New(snapshot, nil), nil)
	r := request(t, dir, script)
	r.Issue = domain.Issue{ID: "issue-1", Identifier: "PMR-94", State: "Todo"}
	_, events, err := backend.Start(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if lastKind(t, drain(t, events)).Kind != domain.EventCompleted {
		t.Fatal("the session did not complete")
	}
	environment := readFile(t, filepath.Join(dir, "env.txt"))
	if strings.Contains(environment, "provider-forge-token") {
		t.Fatal("a bound GitHub manager's token reached the child because no session was prepared to match it")
	}
	if !strings.Contains(environment, "ordinary-value") {
		t.Fatal("the manager fallback removed unrelated variables")
	}
}
