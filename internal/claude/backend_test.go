package claude

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
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
		// Edit and Write are scoped to the write roots -- see scopedAllow -- so
		// --allowedTools is no longer the bare tool surface.
		{"--allowedTools", strings.Join(scopedAllow(rootsOf(t, r), codingTools), ",")},
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
	// AllowLocalBinding is the separate grant, alongside "*" egress, that lets a
	// session bind the loopback listeners its own test suites need.
	if !rendered.Sandbox.Network.AllowLocalBinding {
		t.Fatalf("network=%+v, want AllowLocalBinding", rendered.Sandbox.Network)
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

// assistantUsageLine is one Anthropic API call inside a turn's agentic loop --
// the closing result line reports the turn's total only once the turn ends, so
// this is the CLI's only in-flight signal of what a running turn has spent.
func assistantUsageLine(input, output, cacheCreate, cacheRead int) string {
	return `{"type":"assistant","message":{"content":[],"usage":{"input_tokens":` + strconv.Itoa(input) +
		`,"output_tokens":` + strconv.Itoa(output) +
		`,"cache_creation_input_tokens":` + strconv.Itoa(cacheCreate) +
		`,"cache_read_input_tokens":` + strconv.Itoa(cacheRead) + `}}}`
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
	t.Setenv("HOST_PADDED_NAME", "padded-name-secret")
	t.Setenv("INNOCENT_LOOKING", "prefix-configured-value-secret-suffix")
	t.Setenv("KEPT", "ordinary-value")

	settings := func() config.Settings {
		return config.Settings{
			// The padded and blank names are hostenv.Filter's, and are here
			// because this test launches the real backend: what matters is that
			// the launcher reaches the filter that handles them.
			HostSecretEnvNames: []string{"HOST_CONFIGURED_NAME", "  HOST_PADDED_NAME  ", "   "},
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
	// SYMPHONY_GITHUB_TOKEN, for example. Only a line whose prefix is shaped
	// like a variable name counts: a multi-line value on a developer or CI
	// machine would otherwise read as a name and fail confusingly.
	var names []string
	for _, line := range strings.Split(environment, "\n") {
		name, _, found := strings.Cut(line, "=")
		if !found || strings.TrimFunc(name, func(r rune) bool {
			return r == '_' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9'
		}) != "" {
			continue
		}
		names = append(names, name)
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
		"configured-name-secret", "padded-name-secret", "configured-value-secret", "extra-secret",
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
	args = append(args, strings.Split(raw, "\n")...)
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

// rootsOf mirrors the write roots buildPolicy derives from r, so a test's
// expectation and the production computation cannot silently drift apart.
func rootsOf(t *testing.T, r domain.AgentRequest) []string {
	t.Helper()
	roots, err := writeRoots(r)
	if err != nil {
		t.Fatal(err)
	}
	return roots
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
		if event.Kind.Terminal() {
			out = append(out, event)
		}
	}
	return out
}
