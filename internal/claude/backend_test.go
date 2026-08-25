package claude

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/domain"
)

// A scripted executable stands in for the CLI: it writes its own arguments and
// environment where a test can read them, then emits a stream-json transcript.
// Nothing here talks to a real model.
func writeFakeClaude(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, "fake-claude.sh")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > " + filepath.Join(dir, "args.txt") + "\n" +
		"env > " + filepath.Join(dir, "env.txt") + "\n" +
		"cat > " + filepath.Join(dir, "prompt.txt") + "\n" +
		body
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func workspaceOf(dir string) string { return filepath.Join(dir, "worktree") }

func initLine(dir, tools string) string {
	return `{"type":"system","subtype":"init","session_id":"x","cwd":"` + workspaceOf(dir) +
		`","permissionMode":"dontAsk","tools":[` + tools + `],"mcp_servers":[]}`
}

const allCodingTools = `"Bash","Edit","Glob","Grep","Read","Write"`

// refusedInitLine announces a permission mode the launch contract forbids, which
// is what makes verifyInit fail the turn closed.
func refusedInitLine(dir string) string {
	return `{"type":"system","subtype":"init","cwd":"` + workspaceOf(dir) +
		`","permissionMode":"acceptEdits","tools":[` + allCodingTools + `],"mcp_servers":[]}`
}

func resultLine(isError bool, extra string) string {
	body := `{"type":"result","subtype":"success","is_error":` + strconv.FormatBool(isError) +
		`,"usage":{"input_tokens":2,"output_tokens":11,"cache_creation_input_tokens":5325,"cache_read_input_tokens":3289}`
	if extra != "" {
		body += "," + extra
	}
	return body + "}"
}

func request(t *testing.T, dir, script string) domain.AgentRequest {
	t.Helper()
	workspace := workspaceOf(dir)
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	return domain.AgentRequest{
		Issue:            domain.Issue{ID: "id", Identifier: "PMR-1", State: "Todo"},
		Backend:          "claude",
		Workspace:        workspace,
		GitMetadataRoots: []string{filepath.Join(dir, "objects"), filepath.Join(dir, "worktrees", "pmr-1")},
		Prompt:           "do the work",
		Command:          "sh " + script,
		TurnTimeout:      30 * time.Second,
	}
}

func drain(t *testing.T, events <-chan domain.Event) []domain.Event {
	t.Helper()
	var collected []domain.Event
	timeout := time.After(20 * time.Second)
	for {
		select {
		case event, ok := <-events:
			if !ok {
				return collected
			}
			collected = append(collected, event)
		case <-timeout:
			t.Fatalf("event stream did not close; collected %d events", len(collected))
		}
	}
}

func kinds(events []domain.Event) []domain.EventKind {
	out := make([]domain.EventKind, 0, len(events))
	for _, event := range events {
		out = append(out, event.Kind)
	}
	return out
}

func lastKind(t *testing.T, events []domain.Event) domain.Event {
	t.Helper()
	if len(events) == 0 {
		t.Fatal("no events")
	}
	return events[len(events)-1]
}

func settingsFunc() func() config.Settings {
	return func() config.Settings { return config.Settings{} }
}

// TestStartRunsATurnAndNormalizesItsLifecycle is the happy path: a turn reports
// its session, pairs a tool call with its result, accumulates usage, and ends
// with exactly one terminal event before the stream closes.
func TestStartRunsATurnAndNormalizesItsLifecycle(t *testing.T) {
	dir := t.TempDir()
	script := writeFakeClaude(t, dir, "cat <<'EOF'\n"+
		initLine(dir, allCodingTools)+"\n"+
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"call-1","name":"Bash"}]}}`+"\n"+
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"call-1","is_error":false}]}}`+"\n"+
		resultLine(false, "")+"\n"+
		"EOF\n")

	backend := New(settingsFunc())
	session, events, err := backend.Start(context.Background(), request(t, dir, script))
	if err != nil {
		t.Fatal(err)
	}
	if session.ID == "" || session.ThreadID != session.ID || session.TurnID != "1" {
		t.Fatalf("session=%+v", session)
	}
	collected := drain(t, events)

	var started, item, usage, completed int
	for _, event := range collected {
		switch event.Kind {
		case domain.EventSessionStarted:
			started++
			if event.PID == 0 {
				t.Fatal("session start reported no pid")
			}
		case domain.EventItem:
			item++
		case domain.EventUsage:
			usage++
			// Input must fold the cache components: reporting input_tokens alone
			// would understate real input by orders of magnitude.
			if event.Usage.InputTokens != 2+5325+3289 || event.Usage.OutputTokens != 11 {
				t.Fatalf("usage=%+v", event.Usage)
			}
			if event.Usage.TotalTokens != event.Usage.InputTokens+event.Usage.OutputTokens {
				t.Fatalf("total=%+v", event.Usage)
			}
		case domain.EventCompleted:
			completed++
		case domain.EventFailed, domain.EventBlocked:
			t.Fatalf("unexpected terminal event: %+v", event)
		}
	}
	if started != 1 || item != 2 || usage != 1 || completed != 1 {
		t.Fatalf("started=%d item=%d usage=%d completed=%d (%v)", started, item, usage, completed, kinds(collected))
	}
	if lastKind(t, collected).Kind != domain.EventCompleted {
		t.Fatalf("terminal event was not last: %v", kinds(collected))
	}

	// The started and completed item records must pair by ID and be timed here,
	// because the CLI supplies neither discrete lifecycle events nor durations.
	var startedItem, finishedItem domain.Event
	for _, event := range collected {
		if event.Kind != domain.EventItem {
			continue
		}
		if event.Outcome == domain.ItemStarted {
			startedItem = event
		} else {
			finishedItem = event
		}
	}
	if startedItem.ItemID != "call-1" || finishedItem.ItemID != "call-1" {
		t.Fatalf("item ids: %q / %q", startedItem.ItemID, finishedItem.ItemID)
	}
	if startedItem.ItemType != "commandExecution" || startedItem.ToolName != "Bash" {
		t.Fatalf("item classification=%+v", startedItem)
	}
	if finishedItem.Outcome != domain.ItemCompleted {
		t.Fatalf("finished outcome=%q", finishedItem.Outcome)
	}
}

// TestTheLaunchContractIsFixedAndReappliedOnEveryTurn is the core boundary test.
// The CLI restores none of the policy flags on resume, so a contract applied only
// on the first turn would silently vanish for every turn after it.
func TestTheLaunchContractIsFixedAndReappliedOnEveryTurn(t *testing.T) {
	dir := t.TempDir()
	script := writeFakeClaude(t, dir, "cat <<'EOF'\n"+initLine(dir, allCodingTools)+"\n"+resultLine(false, "")+"\nEOF\n")
	r := request(t, dir, script)

	backend := New(settingsFunc())
	session, events, err := backend.Start(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	drain(t, events)
	first := readArgs(t, dir)
	assertFixedPolicy(t, first, r)
	if got := flagValue(t, first, "--session-id"); got != session.ID {
		t.Fatalf("--session-id=%q, want the assigned %q", got, session.ID)
	}
	if hasFlag(first, "--resume") {
		t.Fatal("a first turn must not resume")
	}
	// The prompt is never an argument: several launch flags are variadic and
	// would swallow a trailing positional.
	for _, arg := range first {
		if strings.Contains(arg, r.Prompt) {
			t.Fatalf("prompt appeared in arguments: %q", arg)
		}
	}
	if prompt := readFile(t, filepath.Join(dir, "prompt.txt")); prompt != r.Prompt {
		t.Fatalf("prompt on stdin=%q, want %q", prompt, r.Prompt)
	}

	events, err = backend.Continue(context.Background(), session, "second turn")
	if err != nil {
		t.Fatal(err)
	}
	drain(t, events)
	second := readArgs(t, dir)
	assertFixedPolicy(t, second, r)
	if got := flagValue(t, second, "--resume"); got != session.ID {
		t.Fatalf("--resume=%q, want %q", got, session.ID)
	}
	if hasFlag(second, "--session-id") {
		t.Fatal("a resumed turn must not also assign a session id")
	}
	if prompt := readFile(t, filepath.Join(dir, "prompt.txt")); prompt != "second turn" {
		t.Fatalf("continuation prompt=%q", prompt)
	}
}

// assertFixedPolicy checks every element of the boundary that must hold on every
// single turn.
func assertFixedPolicy(t *testing.T, args []string, r domain.AgentRequest) {
	t.Helper()
	for _, expected := range [][2]string{
		{"--output-format", "stream-json"},
		// stream-json without --verbose is a hard CLI error.
		{"--permission-mode", permissionMode},
		// An empty source list keeps repository-supplied settings, CLAUDE.md,
		// skills, plugins, and hooks from widening the boundary.
		{"--setting-sources", ""},
		{"--tools", strings.Join(codingTools, ",")},
		{"--allowedTools", strings.Join(codingTools, ",")},
	} {
		if got := flagValue(t, args, expected[0]); got != expected[1] {
			t.Fatalf("%s=%q, want %q", expected[0], got, expected[1])
		}
	}
	for _, flag := range []string{"--print", "--verbose", "--strict-mcp-config"} {
		if !hasFlag(args, flag) {
			t.Fatalf("%s missing from %v", flag, args)
		}
	}

	var rendered policy
	if err := json.Unmarshal([]byte(flagValue(t, args, "--settings")), &rendered); err != nil {
		t.Fatalf("settings payload is not valid JSON: %v", err)
	}
	if !rendered.Sandbox.Enabled || !rendered.Sandbox.FailIfUnavailable || rendered.Sandbox.AllowUnsandboxedCommands {
		t.Fatalf("sandbox=%+v", rendered.Sandbox)
	}
	// Writes are bounded to the worktree plus exactly the Git metadata roots the
	// workspace layer granted -- no wider.
	want := map[string]bool{r.Workspace: true}
	for _, root := range r.GitMetadataRoots {
		want[root] = true
	}
	if len(rendered.Sandbox.Filesystem.AllowWrite) != len(want) {
		t.Fatalf("allowWrite=%v, want exactly %v", rendered.Sandbox.Filesystem.AllowWrite, want)
	}
	for _, root := range rendered.Sandbox.Filesystem.AllowWrite {
		if !want[root] {
			t.Fatalf("allowWrite granted an unexpected root %q", root)
		}
	}
	if rendered.Permissions.DefaultMode != permissionMode {
		t.Fatalf("permissions=%+v", rendered.Permissions)
	}
	for _, denied := range deniedTools {
		if !contains(rendered.Permissions.Deny, denied) {
			t.Fatalf("%s is not denied: %+v", denied, rendered.Permissions)
		}
	}
}

// TestAPolicyThatDidNotApplyFailsTheTurnClosed covers the only confirmation the
// CLI offers. A settings payload it cannot parse is ignored silently, so the init
// echo is the sole evidence the boundary is in force -- and a mismatch must end
// the turn rather than run under an unknown boundary.
func TestAPolicyThatDidNotApplyFailsTheTurnClosed(t *testing.T) {
	for name, build := range map[string]func(dir string) string{
		"permission mode was ignored": func(dir string) string {
			return `{"type":"system","subtype":"init","cwd":"` + workspaceOf(dir) + `","permissionMode":"acceptEdits","tools":[` + allCodingTools + `],"mcp_servers":[]}`
		},
		"an extra tool is available": func(dir string) string {
			return `{"type":"system","subtype":"init","cwd":"` + workspaceOf(dir) + `","permissionMode":"dontAsk","tools":[` + allCodingTools + `,"WebFetch"],"mcp_servers":[]}`
		},
		"the tool surface shrank": func(dir string) string {
			return `{"type":"system","subtype":"init","cwd":"` + workspaceOf(dir) + `","permissionMode":"dontAsk","tools":["Read"],"mcp_servers":[]}`
		},
		"an MCP server is attached": func(dir string) string {
			return `{"type":"system","subtype":"init","cwd":"` + workspaceOf(dir) + `","permissionMode":"dontAsk","tools":[` + allCodingTools + `],"mcp_servers":[{"name":"other","status":"connected"}]}`
		},
		"no working directory": func(string) string {
			return `{"type":"system","subtype":"init","cwd":"","permissionMode":"dontAsk","tools":[` + allCodingTools + `],"mcp_servers":[]}`
		},
		// A turn running somewhere other than this issue's worktree would write
		// outside the boundary the sandbox was built around.
		"a different working directory": func(string) string {
			return `{"type":"system","subtype":"init","cwd":"/somewhere/else","permissionMode":"dontAsk","tools":[` + allCodingTools + `],"mcp_servers":[]}`
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			script := writeFakeClaude(t, dir, "cat <<'EOF'\n"+build(dir)+"\n"+resultLine(false, "")+"\nEOF\n")
			backend := New(settingsFunc())
			_, events, err := backend.Start(context.Background(), request(t, dir, script))
			if err != nil {
				t.Fatal(err)
			}
			collected := drain(t, events)
			for _, event := range collected {
				if event.Kind == domain.EventCompleted {
					t.Fatalf("a turn whose policy did not apply completed: %v", kinds(collected))
				}
			}
			failure := lastKind(t, collected)
			if failure.Kind != domain.EventFailed || !strings.Contains(failure.Message, "refused") {
				t.Fatalf("terminal event=%+v", failure)
			}
		})
	}
}

// TestAMissingInitEventFailsTheTurn covers a turn that reports a result without
// ever announcing its policy.
func TestAMissingInitEventFailsTheTurn(t *testing.T) {
	dir := t.TempDir()
	script := writeFakeClaude(t, dir, "cat <<'EOF'\n"+resultLine(false, "")+"\nEOF\n")
	backend := New(settingsFunc())
	_, events, err := backend.Start(context.Background(), request(t, dir, script))
	if err != nil {
		t.Fatal(err)
	}
	failure := lastKind(t, drain(t, events))
	if failure.Kind != domain.EventFailed || !strings.Contains(failure.Message, "no init event") {
		t.Fatalf("terminal event=%+v", failure)
	}
}

// TestAnErrorResultFailsTheTurnEvenWhenTheSubtypeSaysSuccess is the
// authentication case: the CLI reports subtype "success" with is_error true, so
// reading subtype as the success signal would record a failed turn as completed.
func TestAnErrorResultFailsTheTurnEvenWhenTheSubtypeSaysSuccess(t *testing.T) {
	dir := t.TempDir()
	script := writeFakeClaude(t, dir, "cat <<'EOF'\n"+initLine(dir, allCodingTools)+"\n"+
		resultLine(true, `"terminal_reason":"api_error","api_error_status":"401"`)+"\nEOF\n")
	backend := New(settingsFunc())
	_, events, err := backend.Start(context.Background(), request(t, dir, script))
	if err != nil {
		t.Fatal(err)
	}
	collected := drain(t, events)
	failure := lastKind(t, collected)
	if failure.Kind != domain.EventFailed || !strings.Contains(failure.Message, "api_error") {
		t.Fatalf("terminal event=%+v", failure)
	}
	for _, event := range collected {
		if event.Kind == domain.EventCompleted {
			t.Fatal("an error result was reported as a completed turn")
		}
	}
}

// TestMalformedAndOversizedOutputIsSkippedNotFatal keeps one bad line from
// ending a run that is otherwise progressing. An oversized line is normal
// traffic here: a single assistant message or tool result is one line.
func TestMalformedAndOversizedOutputIsSkippedNotFatal(t *testing.T) {
	dir := t.TempDir()
	script := writeFakeClaude(t, dir, ""+
		"cat <<'EOF'\n"+initLine(dir, allCodingTools)+"\nnot json at all\n{\"type\":}\n[]\n\nEOF\n"+
		// A line past the scanner bound, emitted without a trailing newline
		// problem by padding a valid envelope.
		"printf '{\"type\":\"assistant\",\"pad\":\"'; head -c 9000000 /dev/zero | tr '\\0' 'x'; printf '\"}\\n'\n"+
		"cat <<'EOF'\n"+resultLine(false, "")+"\nEOF\n")
	backend := New(settingsFunc())
	_, events, err := backend.Start(context.Background(), request(t, dir, script))
	if err != nil {
		t.Fatal(err)
	}
	collected := drain(t, events)
	if lastKind(t, collected).Kind != domain.EventCompleted {
		t.Fatalf("run did not complete past malformed output: %v", kinds(collected))
	}
}

// TestTurnTimeoutIsReportedAndKillsTheProcessGroup bounds a turn that never
// produces a result.
func TestTurnTimeoutIsReportedAndKillsTheProcessGroup(t *testing.T) {
	dir := t.TempDir()
	script := writeFakeClaude(t, dir, "cat <<'EOF'\n"+initLine(dir, allCodingTools)+"\nEOF\nsleep 120\n")
	r := request(t, dir, script)
	r.TurnTimeout = 300 * time.Millisecond
	backend := New(settingsFunc())
	_, events, err := backend.Start(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	failure := lastKind(t, drain(t, events))
	if failure.Kind != domain.EventFailed || !strings.Contains(failure.Message, "timeout") {
		t.Fatalf("terminal event=%+v", failure)
	}
}

// TestCancelStopsTheTurnAndIsSafeWhenNothingIsRunning covers both cancellation
// shapes. Between turns there is no process at all, which is the difference from
// a long-lived app-server.
func TestCancelStopsTheTurnAndIsSafeWhenNothingIsRunning(t *testing.T) {
	dir := t.TempDir()
	script := writeFakeClaude(t, dir, "cat <<'EOF'\n"+initLine(dir, allCodingTools)+"\nEOF\nsleep 120\n")
	backend := New(settingsFunc())
	session, events, err := backend.Start(context.Background(), request(t, dir, script))
	if err != nil {
		t.Fatal(err)
	}
	// Wait for the session announcement so the child is definitely running.
	for event := range events {
		if event.Kind == domain.EventSessionStarted {
			break
		}
	}
	if err := backend.Cancel(context.Background(), session); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	// Cancelling again, and cancelling a session that never existed, are both
	// no-ops rather than errors.
	if err := backend.Cancel(context.Background(), session); err != nil {
		t.Fatalf("second cancel: %v", err)
	}
	if err := backend.Cancel(context.Background(), domain.AgentSession{ID: "never"}); err != nil {
		t.Fatalf("unknown cancel: %v", err)
	}
	if _, err := backend.Continue(context.Background(), session, "after cancel"); err == nil {
		t.Fatal("a cancelled session must not be continuable")
	}
}

// TestAnExitWithoutAResultFailsTheTurn keeps a silent child from looking like a
// completed turn.
func TestAnExitWithoutAResultFailsTheTurn(t *testing.T) {
	dir := t.TempDir()
	script := writeFakeClaude(t, dir, "cat <<'EOF'\n"+initLine(dir, allCodingTools)+"\nEOF\necho 'boom' >&2\nexit 3\n")
	backend := New(settingsFunc())
	_, events, err := backend.Start(context.Background(), request(t, dir, script))
	if err != nil {
		t.Fatal(err)
	}
	collected := drain(t, events)
	failure := lastKind(t, collected)
	if failure.Kind != domain.EventFailed || !strings.Contains(failure.Message, "exit status 3") {
		t.Fatalf("terminal event=%+v", failure)
	}
}

// TestUsageAccumulatesAcrossTurns matters because the CLI reports usage per turn
// while the scheduler keeps a component-wise maximum across a run: reporting the
// per-turn figure would make a resumed run under-report.
func TestUsageAccumulatesAcrossTurns(t *testing.T) {
	dir := t.TempDir()
	script := writeFakeClaude(t, dir, "cat <<'EOF'\n"+initLine(dir, allCodingTools)+"\n"+resultLine(false, "")+"\nEOF\n")
	backend := New(settingsFunc())
	session, events, err := backend.Start(context.Background(), request(t, dir, script))
	if err != nil {
		t.Fatal(err)
	}
	firstUsage := usageOf(t, drain(t, events))
	events, err = backend.Continue(context.Background(), session, "again")
	if err != nil {
		t.Fatal(err)
	}
	secondUsage := usageOf(t, drain(t, events))
	if secondUsage.OutputTokens != 2*firstUsage.OutputTokens || secondUsage.InputTokens != 2*firstUsage.InputTokens {
		t.Fatalf("usage did not accumulate: first=%+v second=%+v", firstUsage, secondUsage)
	}
}

// TestHostSecretsNeverReachTheChild covers the filters that need no bound
// provider: every reserved name, the configured names, and the configured
// values, because an inherited variable under any other name can still carry a
// configured credential. The fourth filter, the session's provider secret
// matcher, needs prepared providers and is proven by
// TestStartBindsTheHostProvidersAndTheirSecrets.
// internal/codex's TestNoHostCredentialReachesTheChildEnvironment is the
// counterpart, so a filter cannot hold on one transport and not the other.
//
// The reserved names are written out here rather than read from
// config.ReservedSecretEnvNames, deliberately: a test that iterates the list
// asserts nothing about its contents, and dropping an entry would leave it
// green. Before this test, only two of the five were covered by anything.
func TestHostSecretsNeverReachTheChild(t *testing.T) {
	dir := t.TempDir()
	script := writeFakeClaude(t, dir, "cat <<'EOF'\n"+initLine(dir, allCodingTools)+"\n"+resultLine(false, "")+"\nEOF\n")

	// Each reserved value is unique and is matched by no other filter, so only
	// the name can remove it.
	reserved := map[string]string{
		"LINEAR_API_KEY":               "reserved-linear-key-value",
		"SYMPHONY_LINEAR_API_KEY_FILE": "/private/reserved-linear-key-path",
		"GITHUB_TOKEN":                 "reserved-forge-token-value",
		"SYMPHONY_GITHUB_TOKEN":        "reserved-symphony-forge-token-value",
		"SYMPHONY_GITHUB_TOKEN_FILE":   "/private/reserved-forge-token-path",
	}
	for name, value := range reserved {
		t.Setenv(name, value)
	}
	t.Setenv("HOST_CONFIGURED_NAME", "configured-name-secret")
	t.Setenv("INNOCENT_LOOKING", "prefix-configured-value-secret-suffix")
	t.Setenv("KEPT", "ordinary-value")

	settings := func() config.Settings {
		return config.Settings{
			HostSecretEnvNames: []string{"HOST_CONFIGURED_NAME"},
			HostSecretValues:   []string{"configured-value-secret"},
		}
	}
	backend := New(settings, "EXTRA_SECRET_NAME")
	t.Setenv("EXTRA_SECRET_NAME", "extra-secret")

	_, events, err := backend.Start(context.Background(), request(t, dir, script))
	if err != nil {
		t.Fatal(err)
	}
	drain(t, events)

	environment := readFile(t, filepath.Join(dir, "env.txt"))
	// The names are compared exactly, so an absence assertion on one name cannot
	// be satisfied by another that merely ends with it -- GITHUB_TOKEN inside
	// SYMPHONY_GITHUB_TOKEN, for example.
	var names []string
	for _, line := range strings.Split(environment, "\n") {
		if name, _, found := strings.Cut(line, "="); found {
			names = append(names, name)
		}
	}
	for name, value := range reserved {
		if slices.Contains(names, name) {
			t.Fatalf("child environment retained reserved variable %s", name)
		}
		if strings.Contains(environment, value) {
			t.Fatalf("child environment retained the value of reserved variable %s", name)
		}
	}
	for _, forbidden := range []string{
		"configured-name-secret", "configured-value-secret", "extra-secret",
	} {
		if strings.Contains(environment, forbidden) {
			t.Fatalf("child environment retained %q", forbidden)
		}
	}
	// Unrelated variables must survive: the CLI authenticates through the
	// operator's own login, which lives in the environment it inherits.
	if !strings.Contains(environment, "ordinary-value") {
		t.Fatal("child environment lost unrelated variables")
	}
}

// TestADeniedToolCallIsReportedWithoutItsArguments keeps a refusal observable
// while the denied arguments stay out of every event.
func TestADeniedToolCallIsReportedWithoutItsArguments(t *testing.T) {
	dir := t.TempDir()
	script := writeFakeClaude(t, dir, "cat <<'EOF'\n"+initLine(dir, allCodingTools)+"\n"+
		`{"type":"system","subtype":"permission_denied","tool_name":"Bash","tool_use_id":"call-9","tool_input":{"command":"curl http://secret.example"}}`+"\n"+
		resultLine(false, `"permission_denials":[{"tool_name":"Bash","tool_use_id":"call-9","tool_input":{"command":"curl http://secret.example"}}]`)+"\nEOF\n")
	backend := New(settingsFunc())
	_, events, err := backend.Start(context.Background(), request(t, dir, script))
	if err != nil {
		t.Fatal(err)
	}
	collected := drain(t, events)
	var declined bool
	for _, event := range collected {
		if event.Kind == domain.EventItem && event.Outcome == domain.ItemDeclined {
			declined = true
			if event.ToolName != "Bash" || event.ItemID != "call-9" {
				t.Fatalf("declined item=%+v", event)
			}
		}
		// No event may carry the denied command.
		if strings.Contains(event.Message, "secret.example") || strings.Contains(event.ItemID, "secret.example") {
			t.Fatalf("event leaked denied tool arguments: %+v", event)
		}
	}
	if !declined {
		t.Fatalf("a denied call was not reported: %v", kinds(collected))
	}
}

// TestContinuingAnUnknownSessionFails guards the one-process-per-turn model: a
// resume needs the captured launch contract, which only a known session has.
func TestContinuingAnUnknownSessionFails(t *testing.T) {
	backend := New(settingsFunc())
	if _, err := backend.Continue(context.Background(), domain.AgentSession{ID: "nope"}, "prompt"); err == nil {
		t.Fatal("continuing an unknown session must fail")
	}
}

func usageOf(t *testing.T, events []domain.Event) domain.Usage {
	t.Helper()
	for _, event := range events {
		if event.Kind == domain.EventUsage {
			return event.Usage
		}
	}
	t.Fatalf("no usage event: %v", kinds(events))
	return domain.Usage{}
}

func readArgs(t *testing.T, dir string) []string {
	t.Helper()
	raw := readFile(t, filepath.Join(dir, "args.txt"))
	var args []string
	for _, line := range strings.Split(raw, "\n") {
		args = append(args, line)
	}
	// A trailing newline yields one empty element; keep it, because an empty
	// flag value is itself meaningful here (--setting-sources "").
	return args
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSuffix(string(raw), "\n")
}

func flagValue(t *testing.T, args []string, flag string) string {
	t.Helper()
	for i, arg := range args {
		if arg == flag {
			if i+1 >= len(args) {
				return ""
			}
			return args[i+1]
		}
	}
	t.Fatalf("%s missing from %v", flag, args)
	return ""
}

func hasFlag(args []string, flag string) bool {
	for _, arg := range args {
		if arg == flag {
			return true
		}
	}
	return false
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// TestATurnEndsEvenWhenADescendantEscapesTheProcessGroup is the regression that
// matters most. A group kill cannot reach a process that left the group -- via
// setsid, nohup, or any double fork -- and such a process keeps the inherited
// stdout write end open. Reading would then never see EOF, so the turn would
// hang forever with no terminal event and no closed channel, and the timeout
// would be unenforceable. Closing the parent's pipe ends is what ends the read.
func TestATurnEndsEvenWhenADescendantEscapesTheProcessGroup(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is needed to detach a descendant from the process group")
	}
	dir := t.TempDir()
	script := writeFakeClaude(t, dir, "cat <<'EOF'\n"+initLine(dir, allCodingTools)+"\nEOF\n"+
		// The grandchild leaves the process group and holds the inherited stdout
		// open well past the turn timeout.
		"python3 -c 'import os,time; os.setsid(); time.sleep(60)' &\n"+
		"sleep 60\n")
	r := request(t, dir, script)
	r.TurnTimeout = 400 * time.Millisecond

	backend := New(settingsFunc())
	_, events, err := backend.Start(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	// drain fails the test if the stream never closes, which is exactly the
	// defect: before the fix this blocked until the grandchild exited.
	collected := drain(t, events)
	failure := lastKind(t, collected)
	if failure.Kind != domain.EventFailed || !strings.Contains(failure.Message, "timeout") {
		t.Fatalf("terminal event=%+v", failure)
	}
}

// TestCancelAfterATurnFinishesSignalsNothing covers the pid-reuse hazard: the
// group kill deliberately bypasses Go's post-Wait guard, so a session that kept
// pointing at a reaped turn could signal an unrelated process group once the pid
// was recycled.
func TestCancelAfterATurnFinishesSignalsNothing(t *testing.T) {
	dir := t.TempDir()
	script := writeFakeClaude(t, dir, "cat <<'EOF'\n"+initLine(dir, allCodingTools)+"\n"+resultLine(false, "")+"\nEOF\n")
	backend := New(settingsFunc())
	session, events, err := backend.Start(context.Background(), request(t, dir, script))
	if err != nil {
		t.Fatal(err)
	}
	drain(t, events)

	backend.mu.Lock()
	live := backend.sessions[session.ID]
	backend.mu.Unlock()
	if live == nil {
		t.Fatal("session was forgotten while it was still resumable")
	}
	live.mu.Lock()
	running := live.running
	live.mu.Unlock()
	if running != nil {
		t.Fatal("a finished turn is still the session's live process; cancelling it would signal a reaped pid")
	}
	if err := backend.Cancel(context.Background(), session); err != nil {
		t.Fatalf("cancel after a finished turn: %v", err)
	}
}

// TestOnlyOneTerminalEventIsReported keeps the documented contract true: a
// refused init ends the turn, and a result arriving afterwards must not add a
// second terminal event -- which also reported the wrong reason.
func TestOnlyOneTerminalEventIsReported(t *testing.T) {
	dir := t.TempDir()
	script := writeFakeClaude(t, dir, "cat <<'EOF'\n"+refusedInitLine(dir)+"\n"+resultLine(false, "")+"\nEOF\n")
	backend := New(settingsFunc())
	_, events, err := backend.Start(context.Background(), request(t, dir, script))
	if err != nil {
		t.Fatal(err)
	}
	collected := drain(t, events)
	terminals := terminalEvents(collected)
	if len(terminals) != 1 {
		t.Fatalf("%d terminal events: %v", len(terminals), kinds(collected))
	}
	if !strings.Contains(terminals[0].Message, "permission mode") {
		t.Fatalf("terminal event misreports the reason: %+v", terminals[0])
	}
}

// TestARateLimitReportIsObservable guards against the signal being emitted in a
// shape the scheduler discards.
func TestARateLimitReportIsObservable(t *testing.T) {
	dir := t.TempDir()
	script := writeFakeClaude(t, dir, "cat <<'EOF'\n"+initLine(dir, allCodingTools)+"\n"+
		`{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","rateLimitType":"output_tokens","utilization":0.97}}`+"\n"+
		resultLine(false, "")+"\nEOF\n")
	backend := New(settingsFunc())
	_, events, err := backend.Start(context.Background(), request(t, dir, script))
	if err != nil {
		t.Fatal(err)
	}
	var reported bool
	for _, event := range drain(t, events) {
		if event.Kind == domain.EventDiagnostic && strings.Contains(event.Message, "rate limit") {
			reported = true
			if !strings.Contains(event.Message, "rejected") || !strings.Contains(event.Message, "output_tokens") {
				t.Fatalf("rate limit report lost its detail: %q", event.Message)
			}
		}
		// EventRateLimit would be dropped by the scheduler's numeric allowlist,
		// so emitting it here would make the signal invisible.
		if event.Kind == domain.EventRateLimit {
			t.Fatalf("rate limit emitted in a shape the scheduler discards: %+v", event)
		}
	}
	if !reported {
		t.Fatal("a rate limit report never reached an event")
	}
}

// TestTheModelAndDenyFlagsArePassed covers two flags the documentation
// enumerates that no other test constrained.
func TestTheModelAndDenyFlagsArePassed(t *testing.T) {
	dir := t.TempDir()
	script := writeFakeClaude(t, dir, "cat <<'EOF'\n"+initLine(dir, allCodingTools)+"\n"+resultLine(false, "")+"\nEOF\n")
	r := request(t, dir, script)
	r.Model = "sonnet"

	backend := New(settingsFunc())
	session, events, err := backend.Start(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	drain(t, events)
	first := readArgs(t, dir)
	if got := flagValue(t, first, "--model"); got != "sonnet" {
		t.Fatalf("--model=%q", got)
	}
	if got := flagValue(t, first, "--disallowedTools"); got != strings.Join(deniedTools, ",") {
		t.Fatalf("--disallowedTools=%q", got)
	}
	// Both must survive a resume, like every other part of the contract.
	events, err = backend.Continue(context.Background(), session, "again")
	if err != nil {
		t.Fatal(err)
	}
	drain(t, events)
	second := readArgs(t, dir)
	if got := flagValue(t, second, "--model"); got != "sonnet" {
		t.Fatalf("resumed --model=%q", got)
	}
	if got := flagValue(t, second, "--disallowedTools"); got != strings.Join(deniedTools, ",") {
		t.Fatalf("resumed --disallowedTools=%q", got)
	}
}

// TestNoModelFlagWithoutAConfiguredModel keeps an unset model from becoming an
// empty flag value.
func TestNoModelFlagWithoutAConfiguredModel(t *testing.T) {
	dir := t.TempDir()
	script := writeFakeClaude(t, dir, "cat <<'EOF'\n"+initLine(dir, allCodingTools)+"\n"+resultLine(false, "")+"\nEOF\n")
	backend := New(settingsFunc())
	_, events, err := backend.Start(context.Background(), request(t, dir, script))
	if err != nil {
		t.Fatal(err)
	}
	drain(t, events)
	if hasFlag(readArgs(t, dir), "--model") {
		t.Fatal("an unset model still passed --model")
	}
}

// gate makes a scripted turn pause mid-transcript until the test releases it, so
// a test can act on a turn that is provably still live.
func gatePath(dir string) string { return filepath.Join(dir, "gate") }

func waitForGate(dir string) string {
	return "while [ ! -f " + gatePath(dir) + " ]; do sleep 0.02; done\n"
}

func openGate(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(gatePath(dir), nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

// liveTurn reaches the turn the session is currently running, which is what an
// out-of-band emitter -- anything holding the turn rather than driving its read
// loop -- has a handle on.
func liveTurn(t *testing.T, backend *Backend, sessionID string) *turn {
	t.Helper()
	backend.mu.Lock()
	s := backend.sessions[sessionID]
	backend.mu.Unlock()
	if s == nil {
		t.Fatalf("session %q is not known to the backend", sessionID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running == nil {
		t.Fatal("the turn finished before the test could reach it")
	}
	return s.running
}

func terminalEvents(events []domain.Event) []domain.Event {
	var out []domain.Event
	for _, event := range events {
		if terminal(event.Kind) {
			out = append(out, event)
		}
	}
	return out
}

// TestEmitsFromOtherGoroutinesSurviveTheTurnShutdown is the reason the sink
// exists. Nothing structurally confines emitting to the read loop, and a send
// landing after close(events) panics -- unrecoverably and process-wide, so one
// late event from one turn would kill every parallel session.
func TestEmitsFromOtherGoroutinesSurviveTheTurnShutdown(t *testing.T) {
	dir := t.TempDir()
	script := writeFakeClaude(t, dir, waitForGate(dir)+"cat <<'EOF'\n"+
		initLine(dir, allCodingTools)+"\n"+resultLine(false, "")+"\nEOF\n")

	backend := New(settingsFunc())
	session, events, err := backend.Start(context.Background(), request(t, dir, script))
	if err != nil {
		t.Fatal(err)
	}
	live := liveTurn(t, backend, session.ID)

	// The emitters keep going across the shutdown they cannot see coming, which
	// is what puts a send in the window the guard has to cover.
	stop := make(chan struct{})
	var emitters sync.WaitGroup
	for i := 0; i < 32; i++ {
		emitters.Add(1)
		go func() {
			defer emitters.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				live.sink.emit(domain.Event{Kind: domain.EventDiagnostic, At: time.Now(), Message: "out of band"})
			}
		}()
	}

	openGate(t, dir)
	collected := drain(t, events)
	close(stop)
	emitters.Wait()

	// drain returned, so the channel is closed: these two emits are certainly
	// post-close rather than incidentally so, and must be dropped.
	live.sink.emit(domain.Event{Kind: domain.EventDiagnostic, At: time.Now(), Message: "after close"})
	if live.sink.emitTerminal(domain.Event{Kind: domain.EventFailed, At: time.Now(), Message: "after close"}) {
		t.Fatal("an outcome was accepted after the stream closed")
	}
	if _, open := <-events; open {
		t.Fatal("an emit after the stream closed reached the channel")
	}

	// The flood must not have crowded out the turn's own outcome: that is what
	// the reserved slots are for.
	if terminals := terminalEvents(collected); len(terminals) != 1 || terminals[0].Kind != domain.EventCompleted {
		t.Fatalf("terminal events=%v", kinds(terminals))
	}
}

// TestATerminalEventClaimedOffTheReadLoopSuppressesTheResult covers the second
// half of the same problem: with the latch on the turn rather than in stream's
// locals, a terminal event raised elsewhere is seen by the read loop, which
// would otherwise report the arriving result as a second outcome -- and the
// coordinator, which returns on the first, would record the run as settled while
// the child kept running.
func TestATerminalEventClaimedOffTheReadLoopSuppressesTheResult(t *testing.T) {
	dir := t.TempDir()
	script := writeFakeClaude(t, dir, "cat <<'EOF'\n"+initLine(dir, allCodingTools)+"\nEOF\n"+
		waitForGate(dir)+"cat <<'EOF'\n"+resultLine(false, "")+"\nEOF\n")

	backend := New(settingsFunc())
	session, events, err := backend.Start(context.Background(), request(t, dir, script))
	if err != nil {
		t.Fatal(err)
	}
	live := liveTurn(t, backend, session.ID)

	// Waiting for the session start proves the read loop is running, so the
	// claim below races nothing and the result really does arrive afterwards.
	var collected []domain.Event
	for event := range events {
		collected = append(collected, event)
		if event.Kind != domain.EventSessionStarted {
			continue
		}
		// EventLandingResolved stands in for what the capability bridge will
		// report; this backend produces no such event on its own today. Claiming
		// and emitting are one operation, so an out-of-band outcome cannot settle
		// the turn and then fail to deliver -- which would close the stream with
		// no outcome at all and lose the reason entirely.
		if !live.sink.emitTerminal(domain.Event{Kind: domain.EventLandingResolved, At: time.Now(), Message: "landed out of band"}) {
			t.Fatal("the turn was already settled before anything ended it")
		}
		openGate(t, dir)
	}

	terminals := terminalEvents(collected)
	if len(terminals) != 1 {
		t.Fatalf("%d terminal events: %v", len(terminals), kinds(collected))
	}
	if terminals[0].Kind != domain.EventLandingResolved || terminals[0].Message != "landed out of band" {
		t.Fatalf("terminal event=%+v", terminals[0])
	}
}

// TestTheSinkReservesRoomForTheTerminalEvent pins the buffer policy the sink
// inherited: a consumer stops reading at the terminal event, so progress is
// dropped near the top of the buffer, the terminal event still fits, and no emit
// ever blocks -- a blocked emit would leak the stream goroutine and orphan the
// child, so this test hangs rather than fails if one ever does.
func TestTheSinkReservesRoomForTheTerminalEvent(t *testing.T) {
	s := &sink{events: make(chan domain.Event, eventBuffer)}
	for i := 0; i < 4*eventBuffer; i++ {
		s.emit(domain.Event{Kind: domain.EventDiagnostic})
	}
	if len(s.events) != eventBuffer-reservedTerminalSlots {
		t.Fatalf("buffered %d progress events, want %d", len(s.events), eventBuffer-reservedTerminalSlots)
	}
	if !s.emitTerminal(domain.Event{Kind: domain.EventCompleted}) {
		t.Fatal("the terminal event did not fit the room reserved for it")
	}
	// One outcome per turn, by whichever route it is offered.
	if s.emitTerminal(domain.Event{Kind: domain.EventFailed}) {
		t.Fatal("a second outcome was accepted")
	}
	s.emit(domain.Event{Kind: domain.EventFailed})
	buffered := len(s.events)

	// Dropping post-close emits must not disturb what the consumer has yet to
	// read, so the buffer is checked after the close as well.
	s.close()
	s.emit(domain.Event{Kind: domain.EventDiagnostic})
	if s.emitTerminal(domain.Event{Kind: domain.EventFailed}) {
		t.Fatal("an outcome was accepted after the sink closed")
	}
	collected := drain(t, s.events)
	if len(collected) != buffered || len(collected) != eventBuffer-reservedTerminalSlots+1 {
		t.Fatalf("collected %d events, buffered %d: %v", len(collected), buffered, kinds(collected))
	}
	if last := lastKind(t, collected); last.Kind != domain.EventCompleted {
		t.Fatalf("the terminal event did not fit: %v", kinds(collected))
	}
}

// TestTwoRefusedInitLinesReportOneFailure covers a double terminal event that
// needed no second goroutine at all. The refused-init branch used to emit
// unconditionally -- only the result branch was guarded -- and both lines arrive
// in one read, which t.kill() cannot take back because closing the pipe does not
// discard bytes the reader already holds. So a doubly refused init reported two
// EventFailed, and Coordinator.consume acted on the first while the child was
// still being killed.
func TestTwoRefusedInitLinesReportOneFailure(t *testing.T) {
	dir := t.TempDir()
	script := writeFakeClaude(t, dir, "cat <<'EOF'\n"+refusedInitLine(dir)+"\n"+refusedInitLine(dir)+"\n"+
		resultLine(false, "")+"\nEOF\n")

	backend := New(settingsFunc())
	_, events, err := backend.Start(context.Background(), request(t, dir, script))
	if err != nil {
		t.Fatal(err)
	}
	collected := drain(t, events)
	terminals := terminalEvents(collected)
	if len(terminals) != 1 {
		t.Fatalf("%d terminal events: %v", len(terminals), kinds(collected))
	}
	if terminals[0].Kind != domain.EventFailed || !strings.Contains(terminals[0].Message, "permission mode") {
		t.Fatalf("terminal event=%+v", terminals[0])
	}
}
