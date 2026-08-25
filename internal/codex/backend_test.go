package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/domain"
	"github.com/pmrrasmussen/symphony/internal/observability"
)

func TestStartNormalizesAppServerLifecycle(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-app-server.sh")
	body := `#!/bin/sh
IFS= read -r line
printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{}}'
IFS= read -r line
IFS= read -r line
printf '%s\n' '{"jsonrpc":"2.0","id":2,"result":{"thread":{"id":"thread-1"}}}'
IFS= read -r line
printf '%s\n' '{"jsonrpc":"2.0","id":3,"result":{"turn":{"id":"turn-1"}}}'
printf '%s\n' '{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"thread-1","turn":{"id":"turn-1","usage":{"inputTokens":4,"outputTokens":6,"totalTokens":10}}}}'
`
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	b := New()
	session, events, err := b.Start(context.Background(), domain.AgentRequest{Workspace: dir, Prompt: "work", Command: "sh " + script, ApprovalPolicy: "never", ThreadSandbox: "workspace-write", TurnTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if session.ID != "thread-1-turn-1" {
		t.Fatalf("session=%+v", session)
	}
	seen := map[domain.EventKind]bool{}
	var started domain.Event
	for event := range events {
		seen[event.Kind] = true
		if event.Kind == domain.EventSessionStarted {
			started = event
		}
	}
	if !seen[domain.EventSessionStarted] || !seen[domain.EventCompleted] {
		t.Fatalf("events=%v", seen)
	}
	if started.SessionID != session.ID || started.ThreadID != session.ThreadID || started.TurnID != session.TurnID || started.PID <= 0 {
		t.Fatalf("session-start event=%+v session=%+v", started, session)
	}
	deadline := time.Now().Add(time.Second)
	for {
		b.mu.Lock()
		_, retained := b.sessions[session.ID]
		b.mu.Unlock()
		if !retained {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("exited app-server session was retained")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestStartDrainsStderrBeforeProcessFinalization(t *testing.T) {
	dir := t.TempDir()
	script := writeAppServer(t, dir, `
printf '%s\n' 'token=do-not-log-this' >&2
`)
	c, err := start(context.Background(), request(dir, script), nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	<-c.done
	c.mu.Lock()
	diagnostics := append([]domain.Event(nil), c.diagnostics...)
	c.mu.Unlock()
	seenDiagnostic := false
	for _, event := range diagnostics {
		seenDiagnostic = seenDiagnostic || redactedDiagnostic(event)
	}
	if !seenDiagnostic {
		t.Fatalf("retained diagnostics=%+v", diagnostics)
	}
}

func TestReadRoutesServerRequestBeforeCollidingResponseID(t *testing.T) {
	responses := make(chan callResult, 1)
	events := make(chan domain.Event, 32)
	c := &client{pending: map[int]chan callResult{1: responses}, active: events, activeReady: true, done: make(chan struct{})}
	input := strings.NewReader("{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"account/rateLimits/updated\",\"params\":{\"remaining\":9}}\n" +
		"{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"ok\":true}}\n")
	if err := c.read(input); err != nil {
		t.Fatal(err)
	}
	result := <-responses
	if result.err != nil || !bytes.Contains(result.rpc.Result, []byte(`"ok":true`)) {
		t.Fatalf("response=%+v", result)
	}
	select {
	case event := <-events:
		if event.Kind != domain.EventRateLimit {
			t.Fatalf("event=%+v", event)
		}
	default:
		t.Fatal("colliding server request was consumed as a response")
	}
}

func TestItemLifecycleClassifiesSafeFieldsWithoutParsingCommandOrArguments(t *testing.T) {
	events := make(chan domain.Event, 8)
	c := &client{pending: map[int]chan callResult{}, active: events, activeReady: true, done: make(chan struct{})}
	input := strings.NewReader(
		`{"jsonrpc":"2.0","method":"item/started","params":{"threadId":"thread-1","turnId":"turn-1","startedAtMs":1,"item":{"id":"item-1","type":"commandExecution","status":"inProgress","cwd":"/work","commandActions":[],"command":["bash","-lc","token=do-not-log-this"]}}}` + "\n" +
			`{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"thread-1","turnId":"turn-1","completedAtMs":2,"item":{"id":"item-1","type":"commandExecution","status":"failed","cwd":"/work","commandActions":[],"command":["bash","-lc","token=do-not-log-this"],"durationMs":250,"aggregatedOutput":"secret-output-value"}}}` + "\n" +
			`{"jsonrpc":"2.0","method":"item/started","params":{"threadId":"thread-1","turnId":"turn-1","startedAtMs":3,"item":{"id":"item-2","type":"mcpToolCall","status":"inProgress","server":"docs","tool":"read_file","arguments":{"path":"token=do-not-log-this"}}}}` + "\n" +
			`{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"thread-1","turnId":"turn-1","completedAtMs":4,"item":{"id":"item-2","type":"mcpToolCall","status":"completed","server":"docs","tool":"read_file","arguments":{"path":"token=do-not-log-this"},"durationMs":40,"result":{"content":"secret-result-value"}}}}` + "\n",
	)
	if err := c.read(input); err != nil {
		t.Fatal(err)
	}
	close(events)
	var seen []domain.Event
	for event := range events {
		seen = append(seen, event)
	}
	if len(seen) != 4 {
		t.Fatalf("events=%+v", seen)
	}
	if seen[0].Kind != domain.EventItem || seen[0].ItemID != "item-1" || seen[0].ItemType != "commandExecution" || seen[0].Outcome != domain.ItemStarted || seen[0].ToolName != "" {
		t.Fatalf("command started=%+v", seen[0])
	}
	if seen[1].Outcome != "failed" || seen[1].DurationMs != 250 || seen[1].ItemID != "item-1" {
		t.Fatalf("command completed=%+v", seen[1])
	}
	if seen[2].ItemID != "item-2" || seen[2].ItemType != "mcpToolCall" || seen[2].ToolName != "read_file" || seen[2].Outcome != domain.ItemStarted {
		t.Fatalf("mcp started=%+v", seen[2])
	}
	if seen[3].Outcome != "completed" || seen[3].DurationMs != 40 || seen[3].ToolName != "read_file" {
		t.Fatalf("mcp completed=%+v", seen[3])
	}
	for _, event := range seen {
		blob := fmt.Sprintf("%+v", event)
		for _, secret := range []string{"do-not-log-this", "secret-output-value", "secret-result-value", "bash", "/work", "docs"} {
			if strings.Contains(blob, secret) {
				t.Fatalf("item event leaked command/argument/output content %q: %s", secret, blob)
			}
		}
	}
}

func TestCallRemovesPendingRequestOnTimeoutAndWriteFailure(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		c := bareClient(nopWriteCloser{Writer: io.Discard})
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		_, err := c.call(ctx, "initialize", map[string]any{})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("error=%v", err)
		}
		if got := pendingCount(c); got != 0 {
			t.Fatalf("pending=%d want 0", got)
		}
	})
	t.Run("write failure fails all", func(t *testing.T) {
		c := bareClient(failingWriteCloser{})
		other := make(chan callResult, 1)
		c.pending[99] = other
		_, err := c.call(context.Background(), "initialize", map[string]any{})
		if err == nil || !strings.Contains(err.Error(), "write failed") {
			t.Fatalf("error=%v", err)
		}
		if got := pendingCount(c); got != 0 {
			t.Fatalf("pending=%d want 0", got)
		}
		if result := <-other; result.err == nil {
			t.Fatal("concurrent pending request was not failed")
		}
	})
}

func TestCallPrefersDeliveredResponseWhenProcessExitIsReady(t *testing.T) {
	for range 20 {
		c := bareClient(nil)
		c.in = responseThenExitWriteCloser{client: c}
		result, err := c.call(context.Background(), "turn/start", map[string]any{})
		if err != nil {
			t.Fatal(err)
		}
		if ok, _ := result["ok"].(bool); !ok {
			t.Fatalf("result=%v", result)
		}
	}
}

func TestReadClassifiesMalformedAndOversizedStdout(t *testing.T) {
	c := bareClient(nopWriteCloser{Writer: io.Discard})
	if err := c.read(strings.NewReader("not-json\n")); err == nil || !strings.Contains(err.Error(), "malformed app-server message") {
		t.Fatalf("malformed error=%v", err)
	}
	oversized := strings.NewReader(strings.Repeat("x", (1<<20)+1))
	if err := c.read(oversized); err == nil || !strings.Contains(err.Error(), "stdout scanner failed") {
		t.Fatalf("scanner error=%v", err)
	}
}

func TestMalformedResponseFailsPendingRequest(t *testing.T) {
	response := make(chan callResult, 1)
	c := bareClient(nopWriteCloser{Writer: io.Discard})
	c.pending[1] = response
	err := c.read(strings.NewReader("{\"jsonrpc\":\"2.0\",\"id\":1}\n"))
	if err == nil || !strings.Contains(err.Error(), "missing result and error") {
		t.Fatalf("read error=%v", err)
	}
	if result := <-response; result.err == nil || !strings.Contains(result.err.Error(), "missing result and error") {
		t.Fatalf("pending result=%+v", result)
	}
}

func TestMalformedResultFailsAllPendingRequests(t *testing.T) {
	c := bareClient(nopWriteCloser{Writer: io.Discard})
	other := make(chan callResult, 1)
	c.pending[99] = other
	callDone := make(chan error, 1)
	go func() {
		_, err := c.call(context.Background(), "initialize", map[string]any{})
		callDone <- err
	}()
	deadline := time.Now().Add(time.Second)
	var response chan callResult
	for response == nil {
		c.mu.Lock()
		response = c.pending[1]
		if response != nil {
			delete(c.pending, 1)
		}
		c.mu.Unlock()
		if response == nil {
			if time.Now().After(deadline) {
				t.Fatal("request was not registered")
			}
			time.Sleep(time.Millisecond)
		}
	}
	response <- callResult{rpc: rpc{Result: json.RawMessage(`[]`)}}
	if err := <-callDone; err == nil || !strings.Contains(err.Error(), "malformed initialize response") {
		t.Fatalf("call error=%v", err)
	}
	if result := <-other; result.err == nil {
		t.Fatal("other pending request was not failed")
	}
}

func TestFinishFailsPendingAndDeliversTerminalIntoFullBuffer(t *testing.T) {
	c := bareClient(nopWriteCloser{Writer: io.Discard})
	events := make(chan domain.Event, 32)
	c.active = events
	c.activeDone = make(chan struct{})
	c.activeReady = true
	pending := make(chan callResult, 1)
	c.pending[1] = pending
	for i := 0; i < 100; i++ {
		c.emit(domain.Event{Kind: domain.EventProgress, Message: fmt.Sprintf("progress-%d", i)})
	}
	c.finish(errors.New("codex test process exit"))
	if result := <-pending; result.err == nil {
		t.Fatal("pending request was not failed")
	}
	var last domain.Event
	count := 0
	for event := range events {
		last = event
		count++
	}
	if count != 32 || last.Kind != domain.EventFailed || !strings.Contains(last.Message, "process exit") {
		t.Fatalf("event count=%d last=%+v", count, last)
	}
}

func TestFinishDefersTerminalUntilTurnActivation(t *testing.T) {
	c := bareClient(nopWriteCloser{Writer: io.Discard})
	events := make(chan domain.Event, 32)
	c.active = events
	c.activeDone = make(chan struct{})
	c.finish(errors.New("codex process exited immediately after turn start"))
	select {
	case _, ok := <-events:
		if !ok {
			t.Fatal("pre-ready event stream closed before session activation")
		}
		t.Fatal("pre-ready event stream emitted before session activation")
	default:
	}
	session := domain.AgentSession{ID: "thread-turn", ThreadID: "thread", TurnID: "turn"}
	c.activate(events, session, 123)
	var kinds []domain.EventKind
	for event := range events {
		kinds = append(kinds, event.Kind)
	}
	if len(kinds) != 2 || kinds[0] != domain.EventSessionStarted || kinds[1] != domain.EventFailed {
		t.Fatalf("events=%v", kinds)
	}
}

func TestCompletedBeforeProcessExitRemainsTerminal(t *testing.T) {
	c := bareClient(nopWriteCloser{Writer: io.Discard})
	events := make(chan domain.Event, 32)
	c.active = events
	c.activeDone = make(chan struct{})
	c.emit(domain.Event{Kind: domain.EventCompleted})
	c.finish(errors.New("codex process exited after completion"))
	session := domain.AgentSession{ID: "thread-turn", ThreadID: "thread", TurnID: "turn"}
	c.activate(events, session, 123)
	var kinds []domain.EventKind
	for event := range events {
		kinds = append(kinds, event.Kind)
	}
	if len(kinds) != 2 || kinds[0] != domain.EventSessionStarted || kinds[1] != domain.EventCompleted {
		t.Fatalf("events=%v", kinds)
	}
}

func TestDrainContinuesAfterOversizedDiagnostic(t *testing.T) {
	var messages []string
	err := drain(strings.NewReader(strings.Repeat("x", observability.MaxDiagnosticBytes*3)+"\ntoken=secret-value\n"), func(message string) {
		messages = append(messages, observability.Text(message))
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || messages[0] != "stderr diagnostic exceeded limit" || !strings.Contains(messages[1], "[REDACTED]") || strings.Contains(messages[1], "secret-value") {
		t.Fatalf("messages=%q", messages)
	}
}

func TestStartFailsPromptlyWhenProcessExitsWithPendingRequest(t *testing.T) {
	dir := t.TempDir()
	script := writeAppServer(t, dir, `
IFS= read -r line
exit 7
`)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _, err := New().Start(ctx, request(dir, script))
	if err == nil || !strings.Contains(err.Error(), "process exited") {
		t.Fatalf("error=%v", err)
	}
}

func TestProcessExitAndTurnTimeoutDeliverTerminalFailure(t *testing.T) {
	for _, test := range []struct {
		name        string
		afterStart  string
		turnTimeout time.Duration
		want        string
	}{
		{name: "process exit", afterStart: "exit 9", turnTimeout: time.Minute, want: "process exited"},
		{name: "turn timeout", afterStart: "sleep 30", turnTimeout: 30 * time.Millisecond, want: "turn timeout"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			script := writeAppServer(t, dir, `
IFS= read -r line
printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{}}'
IFS= read -r line
IFS= read -r line
printf '%s\n' '{"jsonrpc":"2.0","id":2,"result":{"thread":{"id":"thread-1"}}}'
IFS= read -r line
printf '%s\n' '{"jsonrpc":"2.0","id":3,"result":{"turn":{"id":"turn-1"}}}'
`+test.afterStart+"\n")
			req := request(dir, script)
			req.TurnTimeout = test.turnTimeout
			_, events, err := New().Start(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			var terminal domain.Event
			for event := range events {
				if event.Kind == domain.EventFailed {
					terminal = event
				}
			}
			if terminal.Kind != domain.EventFailed || !strings.Contains(strings.ToLower(terminal.Message), test.want) {
				t.Fatalf("terminal=%+v", terminal)
			}
		})
	}
}

func TestCancelTerminatesAppServerProcessGroup(t *testing.T) {
	dir := t.TempDir()
	childPIDPath := filepath.Join(dir, "child.pid")
	script := writeAppServer(t, dir, `
IFS= read -r line
printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{}}'
IFS= read -r line
IFS= read -r line
printf '%s\n' '{"jsonrpc":"2.0","id":2,"result":{"thread":{"id":"thread-1"}}}'
IFS= read -r line
printf '%s\n' '{"jsonrpc":"2.0","id":3,"result":{"turn":{"id":"turn-1"}}}'
sleep 30 &
printf '%s' "$!" > child.pid
wait
`)
	b := New()
	session, _, err := b.Start(context.Background(), request(dir, script))
	if err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	c := b.sessions[session.ID]
	b.mu.Unlock()
	childPID := waitForPID(t, childPIDPath)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := b.Cancel(ctx, session); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for syscall.Kill(childPID, 0) == nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if err := syscall.Kill(childPID, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("child process %d survived cancellation: %v", childPID, err)
	}
	if got := pendingCount(c); got != 0 {
		t.Fatalf("pending requests after cancellation=%d", got)
	}
}

func TestDrainReportsBoundedRedactedStderrBeforeTurn(t *testing.T) {
	c := &client{}
	drain(strings.NewReader("token=do-not-log-this\n"), c.diagnostic)
	if len(c.diagnostics) != 1 {
		t.Fatalf("diagnostic count=%d want 1", len(c.diagnostics))
	}
	diagnostic := c.diagnostics[0]
	if diagnostic.Kind != domain.EventDiagnostic || strings.Contains(diagnostic.Message, "do-not-log-this") || !strings.Contains(diagnostic.Message, "[REDACTED]") || len(diagnostic.Message) > observability.MaxDiagnosticBytes {
		t.Fatalf("stderr diagnostic=%q", diagnostic.Message)
	}
}

func TestUnsupportedToolIsRejectedWithoutBlockingTheTurn(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-app-server.sh")
	body := `#!/bin/sh
IFS= read -r line
printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{}}'
IFS= read -r line
IFS= read -r line
case "$line" in *linear_graphql*) exit 20;; *) ;; esac
printf '%s\n' '{"jsonrpc":"2.0","id":2,"result":{"thread":{"id":"thread-1"}}}'
IFS= read -r line
printf '%s\n' '{"jsonrpc":"2.0","id":3,"result":{"turn":{"id":"turn-1"}}}'
printf '%s\n' '{"jsonrpc":"2.0","id":99,"method":"item/tool/call","params":{"tool":"linear_graphql","arguments":{"operation":"read"}}}'
IFS= read -r line
case "$line" in *'"success":false'*) ;; *) exit 21;; esac
printf '%s\n' '{"jsonrpc":"2.0","method":"turn/completed","params":{}}'
`
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	b := NewWithLinearHandoff(func() config.Settings { return config.Settings{} })
	_, events, err := b.Start(context.Background(), domain.AgentRequest{Workspace: dir, Prompt: "work", Command: "sh " + script, ApprovalPolicy: "never", ThreadSandbox: "workspace-write", TurnTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	seenCompleted := false
	for event := range events {
		if event.Kind == domain.EventBlocked {
			t.Fatalf("unsupported tool blocked a non-interactive turn: %+v", event)
		}
		seenCompleted = seenCompleted || event.Kind == domain.EventCompleted
	}
	if !seenCompleted {
		t.Fatal("unsupported tool did not allow turn completion")
	}
}

// TestLandingDecisionsAreTerminalEventsForTheRun pins the PMR-78 contract the
// landing tool handler relies on: a settled landing decision must close the
// active event stream, so the coordinator ends the run instead of starting
// another turn that could only call github_land_pr again.
func TestLandingDecisionsAreTerminalEventsForTheRun(t *testing.T) {
	for _, kind := range []domain.EventKind{domain.EventLandingWaiting, domain.EventLandingResolved} {
		if !terminal(kind) {
			t.Fatalf("event kind %q must be terminal for the run", kind)
		}
	}
	if terminal(domain.EventItem) || terminal(domain.EventProgress) {
		t.Fatal("non-terminal event kinds must not end the run")
	}
}

func TestRejectedLinearAndGitHubToolsDoNotBlockTheTurn(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, "/repos/") {
			// github_pr_context resolves the bound pull request over REST
			// before this test's tool call arrives; none has been published.
			if r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/pulls" {
				_, _ = w.Write([]byte(`[]`))
				return
			}
			t.Fatalf("unexpected GitHub REST request %s %s", r.Method, r.URL.Path)
		}
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		query := request["query"].(string)
		switch {
		case strings.Contains(query, "SymphonyLinearHandoffIssue"):
			_, _ = w.Write([]byte(`{"data":{"issue":{"id":"active","identifier":"PMR-5","title":"Handoff","description":"safe","url":"https://linear.app/issue/PMR-5","project":{"slugId":"project-1"},"team":{"id":"team-1"},"state":{"id":"todo","name":"Todo"}}}}`))
		case strings.Contains(query, "SymphonyLinearHandoffStates"):
			_, _ = w.Write([]byte(`{"data":{"team":{"id":"team-1","states":{"nodes":[{"id":"review","name":"In Review"}]}}}}`))
		default:
			t.Fatalf("unexpected query: %s", query)
		}
	}))
	defer server.Close()
	dir := t.TempDir()
	script := writeAppServer(t, dir, `
IFS= read -r line
printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{}}'
IFS= read -r line
IFS= read -r line
case "$line" in *github_publish_pr*github_pr_context*github_land_pr*) ;; *) exit 20;; esac
printf '%s\n' '{"jsonrpc":"2.0","id":2,"result":{"thread":{"id":"thread-1"}}}'
IFS= read -r line
printf '%s\n' '{"jsonrpc":"2.0","id":3,"result":{"turn":{"id":"turn-1"}}}'
printf '%s\n' '{"jsonrpc":"2.0","id":99,"method":"item/tool/call","params":{"tool":"unsupported","arguments":{}}}'
IFS= read -r line
case "$line" in *'"success":false'*) ;; *) exit 21;; esac
printf '%s\n' '{"jsonrpc":"2.0","id":100,"method":"item/tool/call","params":{"tool":"linear_graphql","arguments":{"operation":"invalid"}}}'
IFS= read -r line
case "$line" in *'"success":false'*) ;; *) exit 22;; esac
printf '%s\n' '{"jsonrpc":"2.0","id":101,"method":"item/tool/call","params":{"tool":"github_publish_pr","arguments":{}}}'
IFS= read -r line
case "$line" in *'"success":false'*) ;; *) exit 23;; esac
printf '%s\n' '{"jsonrpc":"2.0","id":102,"method":"item/tool/call","params":{"tool":"github_publish_pr","arguments":{"why":"fix","what_changed":"code","on_call":"none","extra":"nope"}}}'
IFS= read -r line
case "$line" in *'"success":false'*) ;; *) exit 24;; esac
printf '%s\n' '{"jsonrpc":"2.0","id":103,"method":"item/tool/call","params":{"tool":"github_pr_context","arguments":{}}}'
IFS= read -r line
case "$line" in *'"success":false'*) ;; *) exit 25;; esac
printf '%s\n' '{"jsonrpc":"2.0","id":104,"method":"item/tool/call","params":{"tool":"github_pr_context","arguments":{"number":7}}}'
IFS= read -r line
case "$line" in *'"success":false'*) ;; *) exit 26;; esac
printf '%s\n' '{"jsonrpc":"2.0","id":105,"method":"item/tool/call","params":{"tool":"github_land_pr","arguments":{"reason":"nope"}}}'
IFS= read -r line
case "$line" in *'"success":false'*) ;; *) exit 27;; esac
printf '%s\n' '{"jsonrpc":"2.0","id":106,"method":"item/tool/call","params":{"tool":"github_land_pr","arguments":{}}}'
IFS= read -r line
case "$line" in *'"success":false'*) ;; *) exit 28;; esac
printf '%s\n' '{"jsonrpc":"2.0","method":"turn/completed","params":{}}'
`)
	settings := config.Settings{
		Tracker: config.Tracker{Provider: map[string]any{"api_key": "linear-token", "project_slug_id": "project-1", "endpoint": server.URL}, ActiveStates: []string{"todo"}, HandoffState: "In Review"},
		GitHub:  config.GitHub{Enabled: true, Owner: "owner", Repository: "repo", BaseBranch: "main", Token: "github-token", Endpoint: server.URL, MergeState: "Merging", MergeMethod: "merge", RequiredChecks: []string{"ci"}},
	}
	b, _ := NewWithIntegrations(func() config.Settings { return settings }, nil)
	_, events, err := b.Start(context.Background(), domain.AgentRequest{Issue: domain.Issue{ID: "active", Identifier: "PMR-5", State: "Merging"}, Workspace: dir, Prompt: "work", Command: "sh " + script, ApprovalPolicy: "never", ThreadSandbox: "workspace-write", TurnTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	seenCompleted := false
	for event := range events {
		if event.Kind == domain.EventBlocked {
			t.Fatalf("tool rejection blocked a non-interactive turn: %+v", event)
		}
		seenCompleted = seenCompleted || event.Kind == domain.EventCompleted
	}
	if !seenCompleted {
		t.Fatal("tool rejection did not allow turn completion")
	}
}

// TestStartTimeoutGovernsThreadStartDistinctlyFromReadTimeout proves the
// cold-start seam (PMR-57): a thread/start that takes longer than the small
// steady-state read timeout still succeeds when it stays within the generous
// start timeout, and a thread/start is bounded by the start timeout rather
// than the read timeout (a large read timeout cannot rescue it).
func TestStartTimeoutGovernsThreadStartDistinctlyFromReadTimeout(t *testing.T) {
	// The handshake responds immediately; only thread/start is deliberately slow
	// (2s), well beyond a small read timeout.
	slowThreadStart := `
IFS= read -r line
printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{}}'
IFS= read -r line
IFS= read -r line
sleep 2
printf '%s\n' '{"jsonrpc":"2.0","id":2,"result":{"thread":{"id":"thread-1"}}}'
IFS= read -r line
printf '%s\n' '{"jsonrpc":"2.0","id":3,"result":{"turn":{"id":"turn-1"}}}'
printf '%s\n' '{"jsonrpc":"2.0","method":"turn/completed","params":{}}'
`
	t.Run("start timeout covers a slow thread/start beyond the read timeout", func(t *testing.T) {
		dir := t.TempDir()
		script := writeAppServer(t, dir, slowThreadStart)
		req := request(dir, script)
		// The 2s thread/start exceeds this read timeout but stays within the
		// generous start timeout, so a cold start must still succeed.
		req.ReadTimeout = 200 * time.Millisecond
		req.StartTimeout = 10 * time.Second
		_, events, err := New().Start(context.Background(), req)
		if err != nil {
			t.Fatalf("cold thread/start within start timeout failed: %v", err)
		}
		seenCompleted := false
		for event := range events {
			if event.Kind == domain.EventFailed {
				t.Fatalf("slow-but-in-budget thread/start produced failure: %+v", event)
			}
			seenCompleted = seenCompleted || event.Kind == domain.EventCompleted
		}
		if !seenCompleted {
			t.Fatal("slow-but-in-budget thread/start did not complete")
		}
	})
	t.Run("start timeout bounds thread/start regardless of a large read timeout", func(t *testing.T) {
		dir := t.TempDir()
		script := writeAppServer(t, dir, slowThreadStart)
		req := request(dir, script)
		// A large read timeout cannot rescue thread/start: it is governed by the
		// start timeout, which the 2s delay exceeds. The handshake responds well
		// within this 1s budget, so the bound that fires is thread/start's.
		req.ReadTimeout = 10 * time.Second
		req.StartTimeout = time.Second
		_, _, err := New().Start(context.Background(), req)
		if err == nil || !strings.Contains(err.Error(), "thread/start timed out") {
			t.Fatalf("start timeout did not bound thread/start: err=%v", err)
		}
	})
}

func TestStartUsesBashForConfiguredCommands(t *testing.T) {
	dir := t.TempDir()
	command := `function app_server {
IFS= read -r line
printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{}}'
IFS= read -r line
IFS= read -r line
printf '%s\n' '{"jsonrpc":"2.0","id":2,"result":{"thread":{"id":"thread-1"}}}'
IFS= read -r line
printf '%s\n' '{"jsonrpc":"2.0","id":3,"result":{"turn":{"id":"turn-1"}}}'
printf '%s\n' '{"jsonrpc":"2.0","method":"turn/completed","params":{}}'
}
app_server`
	_, events, err := New().Start(context.Background(), domain.AgentRequest{Workspace: dir, Prompt: "work", Command: command, ApprovalPolicy: "never", ThreadSandbox: "workspace-write", TurnTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	for event := range events {
		if event.Kind == domain.EventCompleted {
			return
		}
	}
	t.Fatal("Bash-specific configured command did not complete")
}

func TestStartFiltersSettingsSnapshotSecrets(t *testing.T) {
	dir := t.TempDir()
	environment := filepath.Join(dir, "environment")
	t.Setenv("PMR33_LINEAR_TOKEN", "linear-token")
	t.Setenv("PMR33_OTHER_NAME", "Bearer linear-token")
	t.Setenv("PMR33_GITHUB_TOKEN", "github-token")
	t.Setenv("LINEAR_API_KEY", "reserved-linear-token")
	command := `env > environment
IFS= read -r line
printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{}}'
IFS= read -r line
IFS= read -r line
printf '%s\n' '{"jsonrpc":"2.0","id":2,"result":{"thread":{"id":"thread-1"}}}'
IFS= read -r line
printf '%s\n' '{"jsonrpc":"2.0","id":3,"result":{"turn":{"id":"turn-1"}}}'
printf '%s\n' '{"jsonrpc":"2.0","method":"turn/completed","params":{}}'`
	settings := config.Settings{HostSecretEnvNames: []string{"PMR33_LINEAR_TOKEN", "PMR33_GITHUB_TOKEN"}, HostSecretValues: []string{"linear-token", "github-token"}}
	settingsCalls := 0
	b := NewWithLinearHandoff(func() config.Settings {
		settingsCalls++
		return settings
	})
	_, events, err := b.Start(context.Background(), domain.AgentRequest{Workspace: dir, Prompt: "work", Command: command, ApprovalPolicy: "never", ThreadSandbox: "workspace-write", TurnTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}
	if settingsCalls != 1 {
		t.Fatalf("settings callback calls=%d want one snapshot", settingsCalls)
	}
	data, err := os.ReadFile(environment)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"PMR33_LINEAR_TOKEN=", "PMR33_GITHUB_TOKEN=", "PMR33_OTHER_NAME=", "LINEAR_API_KEY="} {
		if strings.Contains(string(data), value) {
			t.Fatalf("child environment retained %q: %s", value, data)
		}
	}
}

func TestGitHubToolHasOnlyStructuredHandoffFieldsNoScopeOrCredentialInput(t *testing.T) {
	definition := githubToolDefinition()
	schema, ok := definition["inputSchema"].(map[string]any)
	if !ok || schema["type"] != "object" || schema["additionalProperties"] != false {
		t.Fatalf("schema=%#v", definition["inputSchema"])
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties=%#v", schema["properties"])
	}
	allowed := map[string]bool{"why": true, "what_changed": true, "on_call": true}
	for name := range properties {
		if !allowed[name] {
			t.Fatalf("GitHub tool unexpectedly accepts field %q: %#v", name, schema)
		}
	}
	required, ok := schema["required"].([]string)
	if !ok || len(required) != 3 {
		t.Fatalf("required=%#v", schema["required"])
	}
	for _, name := range required {
		if !allowed[name] {
			t.Fatalf("required field %q is not a structured handoff field", name)
		}
	}
	// The schema (not the free-text description) must never expose a
	// scope-selection or credential field.
	encoded, err := json.Marshal(schema)
	if err != nil || strings.Contains(string(encoded), "token") || strings.Contains(string(encoded), "owner") || strings.Contains(string(encoded), "repository") || strings.Contains(string(encoded), "branch") || strings.Contains(string(encoded), "pull_number") {
		t.Fatalf("tool schema exposed host scope: %s err=%v", encoded, err)
	}
}

func TestGitHubContextToolHasNoInput(t *testing.T) {
	definition := githubContextToolDefinition()
	schema, ok := definition["inputSchema"].(map[string]any)
	if !ok || schema["type"] != "object" || schema["additionalProperties"] != false {
		t.Fatalf("schema=%#v", definition["inputSchema"])
	}
	if _, hasProperties := schema["properties"]; hasProperties {
		t.Fatalf("GitHub context tool unexpectedly accepts caller-controlled input: %#v", schema)
	}
	encoded, err := json.Marshal(definition)
	if err != nil || strings.Contains(string(encoded), "token") || strings.Contains(string(encoded), "\"owner\"") || strings.Contains(string(encoded), "\"repository\"") {
		t.Fatalf("tool definition exposed host scope: %s err=%v", encoded, err)
	}
}

func TestGitHubLandToolHasNoInput(t *testing.T) {
	definition := githubLandToolDefinition()
	schema, ok := definition["inputSchema"].(map[string]any)
	if !ok || schema["type"] != "object" || schema["additionalProperties"] != false {
		t.Fatalf("schema=%#v", definition["inputSchema"])
	}
	if _, hasProperties := schema["properties"]; hasProperties {
		t.Fatalf("GitHub land tool unexpectedly accepts caller-controlled input: %#v", schema)
	}
	encoded, err := json.Marshal(definition)
	if err != nil || strings.Contains(string(encoded), "token") || strings.Contains(string(encoded), "\"owner\"") || strings.Contains(string(encoded), "\"repository\"") || strings.Contains(string(encoded), "\"method\"") {
		t.Fatalf("tool definition exposed host scope: %s err=%v", encoded, err)
	}
}

// TestGitHubLandToolAdvertisedOnlyForConfiguredMergeState exercises the
// dispatch-time filter added in backend.go's Start: github_land_pr is
// offered only when the session's issue is currently (per AgentRequest.Issue,
// the coordinator's own dispatch snapshot) in the exact configured Merging
// state. This is a coarse admission filter; Land itself re-validates Linear
// state before any mutation, which is exercised at the github package level.
func TestGitHubLandToolAdvertisedOnlyForConfiguredMergeState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, "/repos/") {
			t.Fatalf("unexpected GitHub REST request during tool advertisement: %s %s", r.Method, r.URL.Path)
		}
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		query := request["query"].(string)
		switch {
		case strings.Contains(query, "SymphonyLinearHandoffIssue"):
			_, _ = w.Write([]byte(`{"data":{"issue":{"id":"active","identifier":"PMR-37","title":"Land","description":"safe","url":"https://linear.app/issue/PMR-37","project":{"slugId":"project-1"},"team":{"id":"team-1"},"state":{"id":"todo","name":"Todo"}}}}`))
		case strings.Contains(query, "SymphonyLinearHandoffStates"):
			_, _ = w.Write([]byte(`{"data":{"team":{"id":"team-1","states":{"nodes":[{"id":"review","name":"In Review"}]}}}}`))
		default:
			t.Fatalf("unexpected query: %s", query)
		}
	}))
	defer server.Close()
	settings := config.Settings{
		Tracker: config.Tracker{Provider: map[string]any{"api_key": "linear-token", "project_slug_id": "project-1", "endpoint": server.URL}, ActiveStates: []string{"todo"}, HandoffState: "In Review"},
		GitHub:  config.GitHub{Enabled: true, Owner: "owner", Repository: "repo", BaseBranch: "main", Token: "github-token", Endpoint: server.URL, MergeState: "Merging", MergeMethod: "merge", RequiredChecks: []string{"ci"}},
	}
	for _, test := range []struct {
		name       string
		issueState string
		wantLand   bool
	}{
		{name: "matching Merging state", issueState: "Merging", wantLand: true},
		{name: "non-matching state", issueState: "In Progress", wantLand: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			captured := filepath.Join(dir, "thread-start.json")
			script := writeAppServer(t, dir, `
IFS= read -r line
printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{}}'
IFS= read -r line
IFS= read -r line
printf '%s\n' "$line" > `+captured+`
printf '%s\n' '{"jsonrpc":"2.0","id":2,"result":{"thread":{"id":"thread-1"}}}'
IFS= read -r line
printf '%s\n' '{"jsonrpc":"2.0","id":3,"result":{"turn":{"id":"turn-1"}}}'
printf '%s\n' '{"jsonrpc":"2.0","method":"turn/completed","params":{}}'
`)
			b, _ := NewWithIntegrations(func() config.Settings { return settings }, nil)
			_, events, err := b.Start(context.Background(), domain.AgentRequest{Issue: domain.Issue{ID: "active", Identifier: "PMR-37", State: test.issueState}, Workspace: dir, Prompt: "work", Command: "sh " + script, ApprovalPolicy: "never", ThreadSandbox: "workspace-write", TurnTimeout: time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			for range events {
			}
			data, err := os.ReadFile(captured)
			if err != nil {
				t.Fatal(err)
			}
			if gotLand := strings.Contains(string(data), "github_land_pr"); gotLand != test.wantLand {
				t.Fatalf("issueState=%q github_land_pr present=%v want=%v params=%s", test.issueState, gotLand, test.wantLand, data)
			}
			// Trust boundary: the agent is never advertised a Linear-mutating
			// tool. github_publish_pr must remain; linear_graphql must be absent.
			if !strings.Contains(string(data), "github_publish_pr") {
				t.Fatalf("issueState=%q github_publish_pr missing: %s", test.issueState, data)
			}
			if strings.Contains(string(data), "linear_graphql") {
				t.Fatalf("issueState=%q advertised a Linear-mutating linear_graphql tool: %s", test.issueState, data)
			}
		})
	}
}

func TestCreateFollowupIssueToolHasNoCallerControlledScopeFields(t *testing.T) {
	definition := createFollowupIssueToolDefinition()
	schema, ok := definition["inputSchema"].(map[string]any)
	if !ok || schema["type"] != "object" || schema["additionalProperties"] != false {
		t.Fatalf("schema=%#v", definition["inputSchema"])
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties=%#v", schema["properties"])
	}
	for _, forbidden := range []string{"issue", "issue_id", "project", "project_id", "team", "team_id", "state", "state_id", "endpoint", "credential", "token", "parent_id"} {
		if _, exists := properties[forbidden]; exists {
			t.Fatalf("create_followup_issue tool exposed caller-controlled %q: %#v", forbidden, properties)
		}
	}
	for _, allowed := range []string{"title", "description", "acceptance_criteria", "relationship"} {
		if _, exists := properties[allowed]; !exists {
			t.Fatalf("create_followup_issue tool is missing bounded field %q: %#v", allowed, properties)
		}
	}
	required, _ := schema["required"].([]string)
	if len(required) != 3 || required[0] != "title" || required[1] != "description" || required[2] != "acceptance_criteria" {
		t.Fatalf("required=%#v", schema["required"])
	}
}

// TestFollowupIssueCreationIsGatedIndependentlyOfHandoffAndCreatesInBacklog
// enables only tracker.provider.followup_issue_creation (no handoff_state or
// agent_transitions) and verifies: the linear_graphql tool is not advertised,
// the create_followup_issue tool is, and a call is bound to the active issue's
// project/team plus the resolved Backlog state without caller-supplied scope.
func TestFollowupIssueCreationIsGatedIndependentlyOfHandoffAndCreatesInBacklog(t *testing.T) {
	var graphQLCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		graphQLCalls++
		query := request["query"].(string)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(query, "SymphonyLinearHandoffIssue"):
			_, _ = w.Write([]byte(`{"data":{"issue":{"id":"active","identifier":"PMR-41","title":"Decompose","description":"safe","url":"https://linear.app/issue/PMR-41","project":{"id":"project-id-1","slugId":"project-1"},"team":{"id":"team-1"},"state":{"id":"todo","name":"Todo"}}}}`))
		case strings.Contains(query, "SymphonyLinearHandoffStates"):
			_, _ = w.Write([]byte(`{"data":{"team":{"id":"team-1","states":{"nodes":[{"id":"backlog","name":"Backlog"}]}}}}`))
		case strings.Contains(query, "SymphonyLinearCreateFollowupIssue"):
			variables, _ := request["variables"].(map[string]any)
			if variables["teamID"] != "team-1" || variables["projectID"] != "project-id-1" || variables["stateID"] != "backlog" {
				t.Fatalf("unexpected create variables: %#v", variables)
			}
			if _, exists := variables["parentID"]; exists {
				t.Fatalf("follow-up was assigned a parent: %#v", variables)
			}
			_, _ = w.Write([]byte(`{"data":{"issueCreate":{"success":true,"issue":{"id":"followup-1","identifier":"PMR-42","url":"https://linear.app/issue/PMR-42","project":{"id":"project-id-1"},"team":{"id":"team-1"},"state":{"id":"backlog","name":"Backlog"},"parent":null}}}}`))
		default:
			t.Fatalf("unexpected query: %s", query)
		}
	}))
	defer server.Close()
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-app-server.sh")
	body := `#!/bin/sh
IFS= read -r line
printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{}}'
IFS= read -r line
IFS= read -r line
case "$line" in *create_followup_issue*) ;; *) exit 20;; esac
case "$line" in *linear_graphql*) exit 22;; *) ;; esac
printf '%s\n' '{"jsonrpc":"2.0","id":2,"result":{"thread":{"id":"thread-1"}}}'
IFS= read -r line
printf '%s\n' '{"jsonrpc":"2.0","id":3,"result":{"turn":{"id":"turn-1"}}}'
printf '%s\n' '{"jsonrpc":"2.0","id":99,"method":"item/tool/call","params":{"threadId":"thread-1","turnId":"turn-1","callId":"call-1","tool":"create_followup_issue","arguments":{"title":"Split off the client change","description":"The current issue exposed separate client work.","acceptance_criteria":"The client change has focused tests."}}}'
IFS= read -r line
case "$line" in *'"success":true'*PMR-42*Backlog*) ;; *) exit 21;; esac
printf '%s\n' '{"jsonrpc":"2.0","method":"turn/completed","params":{}}'
`
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	settings := config.Settings{Tracker: config.Tracker{
		Provider:              map[string]any{"api_key": "test-token", "project_slug_id": "project-1", "endpoint": server.URL},
		ActiveStates:          []string{"todo"},
		FollowupIssueCreation: true,
	}}
	b := NewWithLinearHandoff(func() config.Settings { return settings }, "LINEAR_API_KEY")
	_, events, err := b.Start(context.Background(), domain.AgentRequest{Issue: domain.Issue{ID: "active"}, Workspace: dir, Prompt: "work", Command: "sh " + script, ApprovalPolicy: "never", ThreadSandbox: "workspace-write", TurnTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}
	// One read and one Backlog-state resolution bind the session, followed by
	// one re-read before mutation (ensureMutable) and the create mutation.
	if graphQLCalls != 4 {
		t.Fatalf("GraphQL calls=%d want prepare+state+ensure+create=4", graphQLCalls)
	}
}

func TestStartGrantsLinkedWorktreeMetadataOnlyToWorkspaceWriteTurns(t *testing.T) {
	dir := t.TempDir()
	gitMetadata := filepath.Join(t.TempDir(), "git-common")
	policyPath := filepath.Join(dir, "turn.json")
	script := writeAppServer(t, dir, `
IFS= read -r line
printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{}}'
IFS= read -r line
IFS= read -r line
printf '%s\n' '{"jsonrpc":"2.0","id":2,"result":{"thread":{"id":"thread-1"}}}'
IFS= read -r line
printf '%s' "$line" > turn.json
printf '%s\n' '{"jsonrpc":"2.0","id":3,"result":{"turn":{"id":"turn-1"}}}'
printf '%s\n' '{"jsonrpc":"2.0","method":"turn/completed","params":{}}'
`)
	b := New()
	_, events, err := b.Start(context.Background(), domain.AgentRequest{Workspace: dir, GitMetadataRoots: []string{gitMetadata}, Prompt: "work", Command: "sh " + script, ApprovalPolicy: "never", ThreadSandbox: "workspace-write", TurnTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}
	data, err := os.ReadFile(policyPath)
	if err != nil {
		t.Fatal(err)
	}
	var request struct {
		Params struct {
			SandboxPolicy map[string]any `json:"sandboxPolicy"`
		} `json:"params"`
	}
	if err := json.Unmarshal(data, &request); err != nil {
		t.Fatal(err)
	}
	if request.Params.SandboxPolicy["type"] != "workspaceWrite" {
		t.Fatalf("sandbox policy=%v", request.Params.SandboxPolicy)
	}
	roots, ok := request.Params.SandboxPolicy["writableRoots"].([]any)
	if !ok || len(roots) != 1 || roots[0] != gitMetadata {
		t.Fatalf("writable roots=%v", request.Params.SandboxPolicy["writableRoots"])
	}
}

func TestLocalCommitSandboxPreservesStricterConfiguredPolicies(t *testing.T) {
	root := "/trusted/git-common"
	readOnly := map[string]any{"type": "readOnly"}
	if got := localCommitSandbox(domain.AgentRequest{GitMetadataRoots: []string{root}, ThreadSandbox: "workspace-write", TurnSandboxPolicy: readOnly}); !reflect.DeepEqual(got, readOnly) {
		t.Fatalf("read-only policy changed to %#v", got)
	}
	configured := map[string]any{"type": "workspaceWrite", "writableRoots": []any{"/extra", root}}
	got, ok := localCommitSandbox(domain.AgentRequest{GitMetadataRoots: []string{root}, ThreadSandbox: "workspace-write", TurnSandboxPolicy: configured}).(map[string]any)
	if !ok {
		t.Fatalf("policy type=%T", got)
	}
	roots, ok := got["writableRoots"].([]string)
	if !ok || !reflect.DeepEqual(roots, []string{"/extra", root}) {
		t.Fatalf("roots=%#v", got["writableRoots"])
	}
}

// TestLocalCommitSandboxGrantsOnlyNarrowedGitRoots proves the narrowed grant
// (PMR-65): a workspace-write turn is granted exactly the object store and the
// per-worktree metadata directory Symphony validated, and never the source
// common directory, its refs/heads, or the primary index.
func TestLocalCommitSandboxGrantsOnlyNarrowedGitRoots(t *testing.T) {
	commonDir := "/src/.git"
	objects := commonDir + "/objects"
	worktreeDir := commonDir + "/worktrees/PMR-1"
	got, ok := localCommitSandbox(domain.AgentRequest{GitMetadataRoots: []string{objects, worktreeDir}, ThreadSandbox: "workspace-write"}).(map[string]any)
	if !ok || got["type"] != "workspaceWrite" {
		t.Fatalf("policy=%#v", got)
	}
	roots, ok := got["writableRoots"].([]string)
	if !ok {
		t.Fatalf("writable roots type=%T", got["writableRoots"])
	}
	if !reflect.DeepEqual(roots, []string{objects, worktreeDir}) {
		t.Fatalf("writable roots=%#v want exactly the object store and worktree metadata dir", roots)
	}
	for _, forbidden := range []string{commonDir, commonDir + "/refs/heads", commonDir + "/index", commonDir + "/packed-refs"} {
		for _, granted := range roots {
			if granted == forbidden {
				t.Fatalf("narrowed grant unexpectedly included %q", forbidden)
			}
		}
	}
}

func TestFilteredEnvRemovesConfiguredSecretByNameAndValue(t *testing.T) {
	t.Setenv("PMR5_TOKEN_BY_NAME", "visible-if-broken")
	t.Setenv("PMR5_TOKEN_BY_VALUE", "linear-secret")
	t.Setenv("PMR5_TOKEN_WITH_PREFIX", "Bearer linear-secret")
	t.Setenv("PMR5_TOKEN_WITH_SUFFIX", "linear-secret:suffix")
	t.Setenv("SYMPHONY_LINEAR_API_KEY_FILE", "/private/linear-key")
	t.Setenv("SYMPHONY_GITHUB_TOKEN_FILE", "/private/github-key")
	blockedNames := []string{"PMR5_TOKEN_BY_NAME", "SYMPHONY_LINEAR_API_KEY_FILE", "SYMPHONY_GITHUB_TOKEN_FILE"}
	for _, value := range filteredEnv(blockedNames, func(candidate string) bool { return strings.Contains(candidate, "linear-secret") }) {
		if strings.HasPrefix(value, "PMR5_TOKEN_BY_NAME=") || strings.HasPrefix(value, "PMR5_TOKEN_BY_VALUE=") || strings.HasPrefix(value, "SYMPHONY_LINEAR_API_KEY_FILE=") || strings.HasPrefix(value, "SYMPHONY_GITHUB_TOKEN_FILE=") {
			t.Fatalf("child environment retained a configured credential variable: %q", value)
		}
		if strings.HasPrefix(value, "PMR5_TOKEN_WITH_PREFIX=") || strings.HasPrefix(value, "PMR5_TOKEN_WITH_SUFFIX=") {
			t.Fatalf("child environment retained embedded Linear secret: %q", value)
		}
	}
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

type failingWriteCloser struct{}

func (failingWriteCloser) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }
func (failingWriteCloser) Close() error              { return nil }

type responseThenExitWriteCloser struct{ client *client }

func (w responseThenExitWriteCloser) Write(p []byte) (int, error) {
	w.client.mu.Lock()
	response := w.client.pending[1]
	delete(w.client.pending, 1)
	w.client.mu.Unlock()
	response <- callResult{rpc: rpc{Result: json.RawMessage(`{"ok":true}`)}}
	close(w.client.done)
	return len(p), nil
}
func (responseThenExitWriteCloser) Close() error { return nil }

func bareClient(in io.WriteCloser) *client {
	return &client{in: in, pending: map[int]chan callResult{}, done: make(chan struct{})}
}

func pendingCount(c *client) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.pending)
}

func request(dir, script string) domain.AgentRequest {
	return domain.AgentRequest{
		Workspace: dir, Prompt: "work", Command: "sh " + script,
		ApprovalPolicy: "never", ThreadSandbox: "workspace-write",
		TurnTimeout: time.Minute, ReadTimeout: time.Second, StartTimeout: time.Minute,
	}
}

func writeAppServer(t *testing.T, dir, body string) string {
	t.Helper()
	script := filepath.Join(dir, "fake-app-server.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
	return script
}

func waitForPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var lastErr error
	for {
		text, err := os.ReadFile(path)
		if err == nil {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(text)))
			if parseErr == nil {
				return pid
			}
			// The shell creates/truncates the file before writing the PID. Under
			// the race detector, a read can observe that short empty window.
			lastErr = parseErr
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("child pid was not written to %s: %v", path, lastErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func redactedDiagnostic(event domain.Event) bool {
	return event.Kind == domain.EventDiagnostic && strings.Contains(event.Message, "[REDACTED]") && !strings.Contains(event.Message, "do-not-log-this")
}
