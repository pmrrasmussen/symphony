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

func TestEnabledLinearHandoffIsAdvertisedAndUsesOnlyBoundIssue(t *testing.T) {
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
			_, _ = w.Write([]byte(`{"data":{"issue":{"id":"active","identifier":"PMR-5","title":"Handoff","description":"safe","url":"https://linear.app/issue/PMR-5","project":{"slugId":"project-1"},"team":{"id":"team-1"},"state":{"id":"todo","name":"Todo"}}}}`))
		case strings.Contains(query, "SymphonyLinearHandoffStates"):
			_, _ = w.Write([]byte(`{"data":{"team":{"id":"team-1","states":{"nodes":[{"id":"review","name":"In Review"}]}}}}`))
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
case "$line" in *linear_graphql*) ;; *) exit 20;; esac
printf '%s\n' '{"jsonrpc":"2.0","id":2,"result":{"thread":{"id":"thread-1"}}}'
IFS= read -r line
printf '%s\n' '{"jsonrpc":"2.0","id":3,"result":{"turn":{"id":"turn-1"}}}'
printf '%s\n' '{"jsonrpc":"2.0","id":99,"method":"item/tool/call","params":{"threadId":"thread-1","turnId":"turn-1","callId":"call-1","tool":"linear_graphql","arguments":{"operation":"read"}}}'
IFS= read -r line
case "$line" in *'"success":true'*PMR-5*) ;; *) exit 21;; esac
printf '%s\n' '{"jsonrpc":"2.0","method":"turn/completed","params":{}}'
`
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	settings := config.Settings{Tracker: config.Tracker{
		Provider:     map[string]any{"api_key": "test-token", "project_slug_id": "project-1", "endpoint": server.URL},
		ActiveStates: []string{"todo"}, HandoffState: "In Review",
	}}
	b := NewWithLinearHandoff(func() config.Settings { return settings }, "LINEAR_API_KEY")
	_, events, err := b.Start(context.Background(), domain.AgentRequest{Issue: domain.Issue{ID: "active"}, Workspace: dir, Prompt: "work", Command: "sh " + script, ApprovalPolicy: "never", ThreadSandbox: "workspace-write", TurnTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}
	if graphQLCalls != 2 {
		t.Fatalf("GraphQL calls=%d want prepare-only 2", graphQLCalls)
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

func TestRejectedLinearAndGitHubToolsDoNotBlockTheTurn(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		query := request["query"].(string)
		w.Header().Set("Content-Type", "application/json")
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
case "$line" in *linear_graphql*github_publish_pr*) ;; *) exit 20;; esac
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
printf '%s\n' '{"jsonrpc":"2.0","method":"turn/completed","params":{}}'
`)
	settings := config.Settings{
		Tracker: config.Tracker{Provider: map[string]any{"api_key": "linear-token", "project_slug_id": "project-1", "endpoint": server.URL}, ActiveStates: []string{"todo"}, HandoffState: "In Review"},
		GitHub:  config.GitHub{Enabled: true, Owner: "owner", Repository: "repo", BaseBranch: "main", Token: "github-token", Endpoint: server.URL},
	}
	b, _ := NewWithIntegrations(func() config.Settings { return settings }, nil)
	_, events, err := b.Start(context.Background(), domain.AgentRequest{Issue: domain.Issue{ID: "active", Identifier: "PMR-5"}, Workspace: dir, Prompt: "work", Command: "sh " + script, ApprovalPolicy: "never", ThreadSandbox: "workspace-write", TurnTimeout: time.Minute})
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

func TestGitHubToolHasNoCallerControlledScopeOrCredentialFields(t *testing.T) {
	definition := githubToolDefinition()
	schema, ok := definition["inputSchema"].(map[string]any)
	if !ok || schema["type"] != "object" || schema["additionalProperties"] != false {
		t.Fatalf("schema=%#v", definition["inputSchema"])
	}
	if _, hasProperties := schema["properties"]; hasProperties {
		t.Fatalf("GitHub tool unexpectedly accepts caller-controlled input: %#v", schema)
	}
	encoded, err := json.Marshal(definition)
	if err != nil || strings.Contains(string(encoded), "token") || strings.Contains(string(encoded), "owner") || strings.Contains(string(encoded), "repository") {
		t.Fatalf("tool definition exposed host scope: %s err=%v", encoded, err)
	}
}

func TestLinearTransitionToolHasOnlyBoundDestinationInput(t *testing.T) {
	definition := linearGraphQLToolDefinition()
	schema, ok := definition["inputSchema"].(map[string]any)
	if !ok || schema["type"] != "object" || schema["additionalProperties"] != false {
		t.Fatalf("schema=%#v", definition["inputSchema"])
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties=%#v", schema["properties"])
	}
	for _, forbidden := range []string{"issue", "project", "team", "endpoint", "credential", "token", "state_id"} {
		if _, exists := properties[forbidden]; exists {
			t.Fatalf("linear tool exposed caller-controlled %q: %#v", forbidden, properties)
		}
	}
	operation, ok := properties["operation"].(map[string]any)
	if !ok {
		t.Fatalf("operation schema=%#v", properties["operation"])
	}
	encoded, _ := json.Marshal(operation["enum"])
	if !strings.Contains(string(encoded), "transition") {
		t.Fatalf("transition operation is not advertised: %s", encoded)
	}
}

func TestCreateChildIssueToolHasNoCallerControlledScopeFields(t *testing.T) {
	definition := createChildIssueToolDefinition()
	schema, ok := definition["inputSchema"].(map[string]any)
	if !ok || schema["type"] != "object" || schema["additionalProperties"] != false {
		t.Fatalf("schema=%#v", definition["inputSchema"])
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties=%#v", schema["properties"])
	}
	for _, forbidden := range []string{"issue", "issue_id", "project", "project_id", "team", "team_id", "endpoint", "credential", "token", "parent_id"} {
		if _, exists := properties[forbidden]; exists {
			t.Fatalf("create_child_issue tool exposed caller-controlled %q: %#v", forbidden, properties)
		}
	}
	for _, allowed := range []string{"title", "description", "priority", "labels", "depends_on"} {
		if _, exists := properties[allowed]; !exists {
			t.Fatalf("create_child_issue tool is missing bounded field %q: %#v", allowed, properties)
		}
	}
	required, _ := schema["required"].([]string)
	if len(required) != 1 || required[0] != "title" {
		t.Fatalf("required=%#v", schema["required"])
	}
}

// TestChildIssueCreationIsGatedIndependentlyOfHandoffAndCreatesABoundChild
// enables only tracker.provider.child_issue_creation (no handoff_state or
// agent_transitions) and verifies: the linear_graphql tool is not advertised,
// the create_child_issue tool is, and a call is bound to the active issue's
// project/team/parent without any caller-supplied scope.
func TestChildIssueCreationIsGatedIndependentlyOfHandoffAndCreatesABoundChild(t *testing.T) {
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
		case strings.Contains(query, "SymphonyLinearCreateChildIssue"):
			variables, _ := request["variables"].(map[string]any)
			if variables["teamID"] != "team-1" || variables["projectID"] != "project-id-1" || variables["parentID"] != "active" {
				t.Fatalf("unexpected create variables: %#v", variables)
			}
			_, _ = w.Write([]byte(`{"data":{"issueCreate":{"success":true,"issue":{"id":"child-1","identifier":"PMR-41-1","url":"https://linear.app/issue/child-1"}}}}`))
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
case "$line" in *create_child_issue*) ;; *) exit 20;; esac
case "$line" in *linear_graphql*) exit 22;; *) ;; esac
printf '%s\n' '{"jsonrpc":"2.0","id":2,"result":{"thread":{"id":"thread-1"}}}'
IFS= read -r line
printf '%s\n' '{"jsonrpc":"2.0","id":3,"result":{"turn":{"id":"turn-1"}}}'
printf '%s\n' '{"jsonrpc":"2.0","id":99,"method":"item/tool/call","params":{"threadId":"thread-1","turnId":"turn-1","callId":"call-1","tool":"create_child_issue","arguments":{"title":"Split off the client change"}}}'
IFS= read -r line
case "$line" in *'"success":true'*PMR-41-1*) ;; *) exit 21;; esac
printf '%s\n' '{"jsonrpc":"2.0","method":"turn/completed","params":{}}'
`
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	settings := config.Settings{Tracker: config.Tracker{
		Provider:           map[string]any{"api_key": "test-token", "project_slug_id": "project-1", "endpoint": server.URL},
		ActiveStates:       []string{"todo"},
		ChildIssueCreation: true,
	}}
	b := NewWithLinearHandoff(func() config.Settings { return settings }, "LINEAR_API_KEY")
	_, events, err := b.Start(context.Background(), domain.AgentRequest{Issue: domain.Issue{ID: "active"}, Workspace: dir, Prompt: "work", Command: "sh " + script, ApprovalPolicy: "never", ThreadSandbox: "workspace-write", TurnTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}
	// One read to bind the session, one re-read before the mutation
	// (ensureMutable), and the create mutation itself.
	if graphQLCalls != 3 {
		t.Fatalf("GraphQL calls=%d want prepare+ensure+create=3", graphQLCalls)
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
	_, events, err := b.Start(context.Background(), domain.AgentRequest{Workspace: dir, GitMetadataRoot: gitMetadata, Prompt: "work", Command: "sh " + script, ApprovalPolicy: "never", ThreadSandbox: "workspace-write", TurnTimeout: time.Minute})
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
	if got := localCommitSandbox(domain.AgentRequest{GitMetadataRoot: root, ThreadSandbox: "workspace-write", TurnSandboxPolicy: readOnly}); !reflect.DeepEqual(got, readOnly) {
		t.Fatalf("read-only policy changed to %#v", got)
	}
	configured := map[string]any{"type": "workspaceWrite", "writableRoots": []any{"/extra", root}}
	got, ok := localCommitSandbox(domain.AgentRequest{GitMetadataRoot: root, ThreadSandbox: "workspace-write", TurnSandboxPolicy: configured}).(map[string]any)
	if !ok {
		t.Fatalf("policy type=%T", got)
	}
	roots, ok := got["writableRoots"].([]string)
	if !ok || !reflect.DeepEqual(roots, []string{"/extra", root}) {
		t.Fatalf("roots=%#v", got["writableRoots"])
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
		TurnTimeout: time.Minute, ReadTimeout: time.Second,
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
