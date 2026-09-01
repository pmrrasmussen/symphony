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
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/pmrrasmussen/symphony/internal/agentstream"
	"github.com/pmrrasmussen/symphony/internal/agenttest"
	"github.com/pmrrasmussen/symphony/internal/capability"
	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/domain"
	githubhost "github.com/pmrrasmussen/symphony/internal/github"
	"github.com/pmrrasmussen/symphony/internal/hostenv"
	"github.com/pmrrasmussen/symphony/internal/linear"
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
printf '%s\n' '{"jsonrpc":"2.0","method":"thread/tokenUsage/updated","params":{"threadId":"thread-1","turnId":"turn-1","tokenUsage":{"last":{"cachedInputTokens":0,"cacheWriteInputTokens":0,"inputTokens":4,"outputTokens":6,"reasoningOutputTokens":0,"totalTokens":10},"total":{"cachedInputTokens":0,"cacheWriteInputTokens":0,"inputTokens":4,"outputTokens":6,"reasoningOutputTokens":0,"totalTokens":10},"modelContextWindow":128000}}}'
printf '%s\n' '{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed","items":[]}}}'
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
	var started, usage domain.Event
	for event := range events {
		seen[event.Kind] = true
		if event.Kind == domain.EventSessionStarted {
			started = event
		}
		if event.Kind == domain.EventUsage {
			usage = event
		}
	}
	if !seen[domain.EventSessionStarted] || !seen[domain.EventCompleted] || !seen[domain.EventUsage] {
		t.Fatalf("events=%v", seen)
	}
	if usage.Usage != (domain.Usage{InputTokens: 4, OutputTokens: 6, TotalTokens: 10}) {
		t.Fatalf("usage=%+v", usage.Usage)
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
	// This test is about stderr, not the environment, so it hands the child the
	// filter's own no-op result rather than nil: a nil Env would inherit the
	// test process's environment whole, which is never what a real launch does.
	c, err := start(context.Background(), request(dir, script), hostenv.Filter(os.Environ(), nil, config.Settings{}, nil), nil, realTimer{})
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
	c, events := streamingClient()
	c.pending[1] = responses
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
	case event := <-events.Events():
		if event.Kind != domain.EventRateLimit {
			t.Fatalf("event=%+v", event)
		}
	default:
		t.Fatal("colliding server request was consumed as a response")
	}
}

func TestItemLifecycleClassifiesSafeFieldsWithoutParsingCommandOrArguments(t *testing.T) {
	c, events := streamingClient()
	input := strings.NewReader(
		`{"jsonrpc":"2.0","method":"item/started","params":{"threadId":"thread-1","turnId":"turn-1","startedAtMs":1,"item":{"id":"item-1","type":"commandExecution","status":"inProgress","cwd":"/work","commandActions":[],"command":["bash","-lc","token=do-not-log-this"]}}}` + "\n" +
			`{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"thread-1","turnId":"turn-1","completedAtMs":2,"item":{"id":"item-1","type":"commandExecution","status":"failed","cwd":"/work","commandActions":[],"command":["bash","-lc","token=do-not-log-this"],"durationMs":250,"aggregatedOutput":"secret-output-value"}}}` + "\n" +
			`{"jsonrpc":"2.0","method":"item/started","params":{"threadId":"thread-1","turnId":"turn-1","startedAtMs":3,"item":{"id":"item-2","type":"mcpToolCall","status":"inProgress","server":"docs","tool":"read_file","arguments":{"path":"token=do-not-log-this"}}}}` + "\n" +
			`{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"thread-1","turnId":"turn-1","completedAtMs":4,"item":{"id":"item-2","type":"mcpToolCall","status":"completed","server":"docs","tool":"read_file","arguments":{"path":"token=do-not-log-this"},"durationMs":40,"result":{"content":"secret-result-value"}}}}` + "\n",
	)
	if err := c.read(input); err != nil {
		t.Fatal(err)
	}
	events.Close()
	var seen []domain.Event
	for event := range events.Events() {
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

// TestThreadTokenUsageUpdatedFoldsCacheAndReasoningTokens pins usage
// extraction against the app-server's own ThreadTokenUsageUpdatedNotification
// shape, confirmed against codex-cli 0.149.1's generated protocol schema
// (`codex app-server generate-json-schema`): a rename of any of these six
// TokenUsageBreakdown keys, or a change to the tokenUsage/total nesting,
// breaks this test instead of silently zeroing every Codex run's usage the
// way turn/completed's non-existent "usage" field did (PMR-155).
func TestThreadTokenUsageUpdatedFoldsCacheAndReasoningTokens(t *testing.T) {
	c, events := streamingClient()
	input := strings.NewReader(
		`{"jsonrpc":"2.0","method":"thread/tokenUsage/updated","params":{"threadId":"thread-1","turnId":"turn-1","tokenUsage":{"last":{"cachedInputTokens":2,"cacheWriteInputTokens":1,"inputTokens":4,"outputTokens":6,"reasoningOutputTokens":3,"totalTokens":16},"total":{"cachedInputTokens":2,"cacheWriteInputTokens":1,"inputTokens":4,"outputTokens":6,"reasoningOutputTokens":3,"totalTokens":16},"modelContextWindow":128000}}}` + "\n",
	)
	if err := c.read(input); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-events.Events():
		if event.Kind != domain.EventUsage || event.UsageAuthoritative {
			t.Fatalf("event=%+v", event)
		}
		if event.Usage != (domain.Usage{InputTokens: 7, OutputTokens: 9, TotalTokens: 16}) {
			t.Fatalf("usage=%+v", event.Usage)
		}
	default:
		t.Fatal("no usage event emitted")
	}
}

// TestThreadTokenUsageUpdatedWithNoUsageLogsOneDiagnosticPerSession asserts
// the miss is loud: an extraction that finds no usage in a notification that
// is supposed to carry it must produce a diagnostic, not silence, and exactly
// once per session so a chatty rolling notification cannot flood the log.
func TestThreadTokenUsageUpdatedWithNoUsageLogsOneDiagnosticPerSession(t *testing.T) {
	c := &client{pending: map[int]chan callResult{}, done: make(chan struct{})}
	zero := `{"jsonrpc":"2.0","method":"thread/tokenUsage/updated","params":{"threadId":"thread-1","turnId":"turn-1","tokenUsage":{"last":{"cachedInputTokens":0,"cacheWriteInputTokens":0,"inputTokens":0,"outputTokens":0,"reasoningOutputTokens":0,"totalTokens":0},"total":{"cachedInputTokens":0,"cacheWriteInputTokens":0,"inputTokens":0,"outputTokens":0,"reasoningOutputTokens":0,"totalTokens":0}}}}` + "\n"
	if err := c.read(strings.NewReader(zero + zero)); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	diagnostics := append([]domain.Event(nil), c.diagnostics...)
	c.mu.Unlock()
	count := 0
	for _, event := range diagnostics {
		if event.Kind == domain.EventDiagnostic && strings.Contains(event.Message, "thread/tokenUsage/updated reported no usage") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("diagnostic count=%d want 1, diagnostics=%+v", count, diagnostics)
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

// TestAnOversizedNotificationDoesNotEndTheSession is the whole of PMR-192. One
// item/completed carrying a command's aggregated output can exceed any line
// bound, and the scanner this loop used made that permanently fatal: the read
// returned, the client aborted, and a run that was progressing ended as "codex
// stdout scanner failed". The oversized line is skipped instead, the
// notifications around it are still classified, and the run continues.
func TestAnOversizedNotificationDoesNotEndTheSession(t *testing.T) {
	c, events := streamingClient()
	oversized := `{"jsonrpc":"2.0","method":"item/completed","params":{"item":{"id":"item-1","type":"commandExecution","status":"completed","output":"` +
		strings.Repeat("x", agentstream.MaxLine) + `"}}}`
	input := strings.NewReader(
		"not json at all\n" +
			oversized + "\n" +
			`{"jsonrpc":"2.0","method":"item/completed","params":{"item":{"id":"item-2","type":"commandExecution","status":"completed"}}}` + "\n")
	if err := c.read(input); err != nil {
		t.Fatalf("read ended the session: %v", err)
	}
	var items, diagnostics []domain.Event
	events.Close()
	for event := range events.Events() {
		switch event.Kind {
		case domain.EventItem:
			items = append(items, event)
		case domain.EventDiagnostic:
			diagnostics = append(diagnostics, event)
		}
	}
	if len(items) != 1 || items[0].ItemID != "item-2" {
		t.Fatalf("items=%+v", items)
	}
	// Both skips are reported once per session, not once per line: the flood
	// this tolerates is exactly what must not reach a log.
	if len(diagnostics) != 1 || !strings.Contains(diagnostics[0].Message, "skipped an unreadable") {
		t.Fatalf("diagnostics=%+v", diagnostics)
	}
}

// TestAResponseNamingNoPendingCallIsSkipped covers the other line this loop
// cannot use. It answers nothing this client is waiting for -- a call that has
// already timed out and removed itself, say -- so it is skipped like any other,
// rather than ending a session that has moved on.
func TestAResponseNamingNoPendingCallIsSkipped(t *testing.T) {
	c, _ := streamingClient()
	if err := c.read(strings.NewReader("{\"jsonrpc\":\"2.0\",\"id\":7,\"result\":{\"ok\":true}}\n")); err != nil {
		t.Fatalf("read ended the session: %v", err)
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
	c, events := streamingClient()
	c.activeDone = make(chan struct{})
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
	for event := range events.Events() {
		last = event
		count++
	}
	// Progress is dropped near the top of the buffer so the outcome always
	// fits; what must never happen is a blocking send, which would leak the
	// emitting goroutine and hang this test rather than fail it.
	if count != eventBuffer-agentstream.ReservedTerminalSlots+1 {
		t.Fatalf("event count=%d", count)
	}
	if last.Kind != domain.EventFailed || !strings.Contains(last.Message, "process exit") {
		t.Fatalf("last=%+v", last)
	}
}

func TestFinishDefersTerminalUntilTurnActivation(t *testing.T) {
	c, events := heldClient()
	c.finish(errors.New("codex process exited immediately after turn start"))
	select {
	case _, ok := <-events.Events():
		if !ok {
			t.Fatal("held event stream closed before session activation")
		}
		t.Fatal("held event stream emitted before session activation")
	default:
	}
	session := domain.AgentSession{ID: "thread-turn", ThreadID: "thread", TurnID: "turn"}
	c.activate(events, session, 123)
	var kinds []domain.EventKind
	for event := range events.Events() {
		kinds = append(kinds, event.Kind)
	}
	if len(kinds) != 2 || kinds[0] != domain.EventSessionStarted || kinds[1] != domain.EventFailed {
		t.Fatalf("events=%v", kinds)
	}
}

func TestCompletedBeforeProcessExitRemainsTerminal(t *testing.T) {
	c, events := heldClient()
	c.emit(domain.Event{Kind: domain.EventCompleted})
	c.finish(errors.New("codex process exited after completion"))
	session := domain.AgentSession{ID: "thread-turn", ThreadID: "thread", TurnID: "turn"}
	c.activate(events, session, 123)
	var kinds []domain.EventKind
	for event := range events.Events() {
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
	if len(messages) != 2 {
		t.Fatalf("messages=%q", messages)
	}
	if !strings.HasPrefix(messages[0], strings.Repeat("x", 10)) || !strings.Contains(messages[0], "…[truncated]") {
		t.Fatalf("oversized message not bounded and marked truncated: %q", messages[0])
	}
	if !strings.Contains(messages[1], "[REDACTED]") || strings.Contains(messages[1], "secret-value") {
		t.Fatalf("messages=%q", messages)
	}
}

func TestDrainOversizedLineIsMaskedForCredentials(t *testing.T) {
	var messages []string
	longSecretLine := "token=secret-value " + strings.Repeat("x", observability.MaxDiagnosticBytes*3)
	err := drain(strings.NewReader(longSecretLine+"\n"), func(message string) {
		messages = append(messages, observability.Text(message))
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 {
		t.Fatalf("messages=%q", messages)
	}
	if strings.Contains(messages[0], "secret-value") {
		t.Fatalf("credential leaked in oversized diagnostic: %q", messages[0])
	}
	if !strings.Contains(messages[0], "[REDACTED]") {
		t.Fatalf("expected masked token in oversized diagnostic: %q", messages[0])
	}
	if !strings.Contains(messages[0], "…[truncated]") {
		t.Fatalf("expected truncation marker in oversized diagnostic: %q", messages[0])
	}
}

// TestStartFailsPromptlyWhenProcessExitsWithPendingRequest pins that an
// app-server which exits with a request outstanding fails that request on its
// exit rather than leaving it to time out.
//
// "Promptly" is asserted by giving the session a timer that never elapses: with
// no budget available to expire, the exit path is the only thing that can end
// this call at all. It used to be asserted with a two-second context deadline,
// which made a loaded machine's slow child look like the bug (PMR-96).
func TestStartFailsPromptlyWhenProcessExitsWithPendingRequest(t *testing.T) {
	dir := t.TempDir()
	script := writeAppServer(t, dir, `
IFS= read -r line
exit 7
`)
	b := New()
	timedBackend(t, b)
	_, _, err := b.Start(context.Background(), request(dir, script))
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

// TestUsageSurvivesCancellationBeforeTurnCompletes reproduces PMR-155's actual
// failure: Symphony's own publish flow cancels the session the moment the
// landing capability resolves, which kills the app-server well before it
// would ever emit turn/completed on its own, so a fix that only read usage
// from turn/completed could never have reported anything for this run. The
// app-server's thread/tokenUsage/updated notification arrives during the
// turn, so it must survive a cancellation that pre-empts the turn's own end.
func TestUsageSurvivesCancellationBeforeTurnCompletes(t *testing.T) {
	dir := t.TempDir()
	script := writeAppServer(t, dir, `
IFS= read -r line
printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{}}'
IFS= read -r line
IFS= read -r line
printf '%s\n' '{"jsonrpc":"2.0","id":2,"result":{"thread":{"id":"thread-1"}}}'
IFS= read -r line
printf '%s\n' '{"jsonrpc":"2.0","id":3,"result":{"turn":{"id":"turn-1"}}}'
printf '%s\n' '{"jsonrpc":"2.0","method":"thread/tokenUsage/updated","params":{"threadId":"thread-1","turnId":"turn-1","tokenUsage":{"last":{"cachedInputTokens":0,"cacheWriteInputTokens":0,"inputTokens":4,"outputTokens":6,"reasoningOutputTokens":0,"totalTokens":10},"total":{"cachedInputTokens":0,"cacheWriteInputTokens":0,"inputTokens":4,"outputTokens":6,"reasoningOutputTokens":0,"totalTokens":10},"modelContextWindow":128000}}}'
sleep 30 &
wait
`)
	b := New()
	session, events, err := b.Start(context.Background(), request(dir, script))
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var usage domain.Event
	done := make(chan struct{})
	go func() {
		defer close(done)
		for event := range events {
			if event.Kind == domain.EventUsage {
				mu.Lock()
				usage = event
				mu.Unlock()
			}
		}
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		seen := usage.Kind == domain.EventUsage
		mu.Unlock()
		if seen {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("usage was not reported before cancellation")
		}
		time.Sleep(10 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := b.Cancel(ctx, session); err != nil {
		t.Fatal(err)
	}
	<-done
	mu.Lock()
	got := usage.Usage
	mu.Unlock()
	if got != (domain.Usage{InputTokens: 4, OutputTokens: 6, TotalTokens: 10}) {
		t.Fatalf("usage=%+v", got)
	}
}

// TestContinueReusesFrozenClientFieldsAcrossTurns pins Continue's documented
// choice (backend.go's Continue) to rebuild the second turn's request from
// the client's own fields, frozen once at Start, rather than from anything
// live: the settings callback returns a different value on every call here,
// yet the second turn/start still carries the workspace, approval policy, and
// sandbox policy the session started with, and the settings callback is never
// consulted again to produce them.
func TestContinueReusesFrozenClientFieldsAcrossTurns(t *testing.T) {
	dir := t.TempDir()
	captured := filepath.Join(dir, "second-turn.json")
	script := writeAppServer(t, dir, `
IFS= read -r line
printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{}}'
IFS= read -r line
IFS= read -r line
printf '%s\n' '{"jsonrpc":"2.0","id":2,"result":{"thread":{"id":"thread-1"}}}'
IFS= read -r line
printf '%s\n' '{"jsonrpc":"2.0","id":3,"result":{"turn":{"id":"turn-1"}}}'
printf '%s\n' '{"jsonrpc":"2.0","method":"turn/completed","params":{}}'
IFS= read -r line
printf '%s\n' "$line" > `+captured+`
printf '%s\n' '{"jsonrpc":"2.0","id":4,"result":{"turn":{"id":"turn-2"}}}'
printf '%s\n' '{"jsonrpc":"2.0","method":"turn/completed","params":{}}'
`)
	settingsCalls := 0
	settingsFn := func() config.Settings {
		settingsCalls++
		// A distinct value on every call: if Continue read this even once, it
		// would be observable in the captured params asserted below.
		return config.Settings{GitHub: config.GitHub{Owner: fmt.Sprintf("reload-%d", settingsCalls)}}
	}
	b := NewWithSettings(settingsFn)
	sandbox := map[string]any{"type": "workspaceWrite", "writableRoots": []string{"/frozen/root"}}
	session, events, err := b.Start(context.Background(), domain.AgentRequest{
		Workspace: dir, Prompt: "first", Command: "sh " + script,
		ApprovalPolicy: "on-request", ThreadSandbox: "workspace-write", TurnSandboxPolicy: sandbox,
		TurnTimeout: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}
	callsAfterStart := settingsCalls

	continued, err := b.Continue(context.Background(), session, "second")
	if err != nil {
		t.Fatal(err)
	}
	for range continued {
	}

	if settingsCalls != callsAfterStart {
		t.Fatalf("Continue consulted the settings callback: calls after Start=%d after Continue=%d", callsAfterStart, settingsCalls)
	}
	data, err := os.ReadFile(captured)
	if err != nil {
		t.Fatal(err)
	}
	var request struct {
		Params struct {
			Cwd            string         `json:"cwd"`
			ApprovalPolicy string         `json:"approvalPolicy"`
			SandboxPolicy  map[string]any `json:"sandboxPolicy"`
		} `json:"params"`
	}
	if err := json.Unmarshal(data, &request); err != nil {
		t.Fatalf("captured second turn/start not JSON: %s", data)
	}
	if request.Params.Cwd != dir || request.Params.ApprovalPolicy != "on-request" {
		t.Fatalf("second turn/start did not carry the frozen workspace/approval policy: %s", data)
	}
	want := map[string]any{"type": "workspaceWrite", "writableRoots": []any{"/frozen/root"}}
	if !reflect.DeepEqual(request.Params.SandboxPolicy, want) {
		t.Fatalf("second turn/start did not carry the frozen sandbox policy: got %v want %v", request.Params.SandboxPolicy, want)
	}
}

// TestTurnStartFailureClosesTheHeldStreamBeforeActivation covers the first of
// turn()'s two closeActive call sites: the app-server process exits with
// turn/start outstanding (an abort mid-turn), which fails that pending call
// before activate() ever ran. closeActive must still detach the turn -- closing
// a sink that is still holding what it never got to deliver -- and turn() must
// report the failure without ever handing back an events channel nobody can
// drain.
func TestTurnStartFailureClosesTheHeldStreamBeforeActivation(t *testing.T) {
	dir := t.TempDir()
	script := writeAppServer(t, dir, `
IFS= read -r line
printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{}}'
IFS= read -r line
IFS= read -r line
printf '%s\n' '{"jsonrpc":"2.0","id":2,"result":{"thread":{"id":"thread-1"}}}'
IFS= read -r line
exit 0
`)
	c, err := start(context.Background(), request(dir, script), hostenv.Filter(os.Environ(), nil, config.Settings{}, nil), nil, realTimer{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.callWithTimeout(context.Background(), "initialize", map[string]any{}, c.startTimeout); err != nil {
		t.Fatal(err)
	}
	if err := c.notify("initialized", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	res, err := c.callWithTimeout(context.Background(), "thread/start", map[string]any{"cwd": dir}, c.startTimeout)
	if err != nil {
		t.Fatal(err)
	}
	thread, ok := nestedString(res, "thread", "id")
	if !ok {
		t.Fatalf("thread/start response=%v", res)
	}

	_, events, turnErr := c.turn(context.Background(), thread, "work", domain.AgentRequest{Workspace: dir})
	if turnErr == nil || !strings.Contains(turnErr.Error(), "process exited") {
		t.Fatalf("turn error=%v, want the aborted process reported", turnErr)
	}
	if events != nil {
		t.Fatal("a closed turn must not hand back an events channel")
	}
	c.mu.Lock()
	active, activeDone := c.active, c.activeDone
	c.mu.Unlock()
	if active != nil || activeDone != nil {
		t.Fatalf("closeActive did not detach the turn: active=%v activeDone=%v", active, activeDone)
	}
}

// TestMalformedTurnStartResponseClosesTheHeldStreamBeforeActivation covers
// turn()'s second closeActive call site: a turn/start response that answers but
// omits turn.id, the teardown-before-the-stream-is-activated case, since
// activate() never runs on this path either.
func TestMalformedTurnStartResponseClosesTheHeldStreamBeforeActivation(t *testing.T) {
	dir := t.TempDir()
	script := writeAppServer(t, dir, `
IFS= read -r line
printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{}}'
IFS= read -r line
IFS= read -r line
printf '%s\n' '{"jsonrpc":"2.0","id":2,"result":{"thread":{"id":"thread-1"}}}'
IFS= read -r line
printf '%s\n' '{"jsonrpc":"2.0","id":3,"result":{}}'
`)
	c, err := start(context.Background(), request(dir, script), hostenv.Filter(os.Environ(), nil, config.Settings{}, nil), nil, realTimer{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.callWithTimeout(context.Background(), "initialize", map[string]any{}, c.startTimeout); err != nil {
		t.Fatal(err)
	}
	if err := c.notify("initialized", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	res, err := c.callWithTimeout(context.Background(), "thread/start", map[string]any{"cwd": dir}, c.startTimeout)
	if err != nil {
		t.Fatal(err)
	}
	thread, ok := nestedString(res, "thread", "id")
	if !ok {
		t.Fatalf("thread/start response=%v", res)
	}

	_, events, turnErr := c.turn(context.Background(), thread, "work", domain.AgentRequest{Workspace: dir})
	if turnErr == nil || !strings.Contains(turnErr.Error(), "malformed turn/start response") {
		t.Fatalf("turn error=%v, want the malformed response reported", turnErr)
	}
	if events != nil {
		t.Fatal("a closed turn must not hand back an events channel")
	}
	c.mu.Lock()
	active, activeDone := c.active, c.activeDone
	c.mu.Unlock()
	if active != nil || activeDone != nil {
		t.Fatalf("closeActive did not detach the turn: active=%v activeDone=%v", active, activeDone)
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
	settings := func() config.Settings { return config.Settings{} }
	b := NewWithSettings(settings)
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
		if !kind.Terminal() {
			t.Fatalf("event kind %q must be terminal for the run", kind)
		}
	}
	if domain.EventItem.Terminal() || domain.EventProgress.Terminal() {
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
case "$line" in *github_pr_context*github_land_pr*) ;; *) exit 20;; esac
case "$line" in *github_publish_pr*) exit 29;; esac
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
printf '%s\n' '{"jsonrpc":"2.0","id":106,"method":"item/tool/call","params":{"tool":"github_land_pr"}}'
IFS= read -r line
case "$line" in *'"success":false'*) ;; *) exit 28;; esac
printf '%s\n' '{"jsonrpc":"2.0","method":"turn/completed","params":{}}'
`)
	settings := config.Settings{
		Tracker: config.Tracker{Provider: map[string]any{"api_key": "linear-token", "project_slug_id": "project-1", "endpoint": server.URL}, ActiveStates: []string{"todo"}, HandoffState: "In Review"},
		GitHub:  config.GitHub{Enabled: true, Owner: "owner", Repository: "repo", BaseBranch: "main", Token: "github-token", Endpoint: server.URL, MergeState: "Merging", MergeMethod: "merge", RequiredChecks: []string{"ci"}},
	}
	snapshot := func() config.Settings { return settings }
	b := NewWithSettings(snapshot)
	_, events, err := b.Start(context.Background(), hostPrepared(t, snapshot, domain.AgentRequest{Issue: domain.Issue{ID: "active", Identifier: "PMR-5", State: "Merging"}, Workspace: dir, Prompt: "work", Command: "sh " + script, ApprovalPolicy: "never", ThreadSandbox: "workspace-write", TurnTimeout: time.Minute}))
	if err != nil {
		t.Fatal(err)
	}
	seenCompleted := false
	reported := map[string][]domain.Event{}
	for event := range events {
		if event.Kind == domain.EventBlocked {
			t.Fatalf("tool rejection blocked a non-interactive turn: %+v", event)
		}
		if event.ItemType == "dynamicToolCall" {
			reported[event.ToolName] = append(reported[event.ToolName], event)
		}
		seenCompleted = seenCompleted || event.Kind == domain.EventCompleted
	}
	if !seenCompleted {
		t.Fatal("tool rejection did not allow turn completion")
	}
	// The item records this transport hands to the shared dispatch, asserted
	// where a real capability is actually invoked over the app-server protocol.
	// Only the two calls whose arguments validated reached an invocation; every
	// other call above was refused before it, and a refusal that precedes a call
	// is never reported as one.
	//
	// github_land_pr is called above with no "arguments" member at all, so its
	// pair is also this transport's end of PMR-186: an app-server that omits the
	// member reaches a zero-argument capability, where before the normalization
	// moved into the shared dispatch it was refused as an unsupported tool and
	// produced no records here.
	for name := range reported {
		if name != "github_pr_context" && name != "github_land_pr" {
			t.Fatalf("a call that never reached an invocation was reported as %q: %+v", name, reported[name])
		}
	}
	for _, name := range []string{"github_pr_context", "github_land_pr"} {
		pair := reported[name]
		if len(pair) != 2 {
			t.Fatalf("%s produced %d item records, want a started/finished pair: %+v", name, len(pair), pair)
		}
		if pair[0].Outcome != domain.ItemStarted || pair[1].Outcome != domain.ItemFailed {
			t.Fatalf("%s outcomes = %q and %q, want started then failed", name, pair[0].Outcome, pair[1].Outcome)
		}
		if pair[0].ItemID == "" || pair[0].ItemID != pair[1].ItemID {
			t.Fatalf("%s reported IDs %q and %q, so no consumer can match them", name, pair[0].ItemID, pair[1].ItemID)
		}
	}
}

// gatedThreadStart is an app-server that answers the handshake at once and then
// holds thread/start -- and, after that, turn/start -- open until the test
// releases each in turn. Each hold announces itself with a marker file first, so
// a test knows which call is outstanding rather than guessing: initialize and
// thread/start share the start timeout, so "a start-timeout budget exists" does
// not on its own say which of the two it belongs to.
//
// Holding a call open is what makes the budget governing it observable, and the
// release is a handshake with the test, so nothing here is timed.
func gatedThreadStart(dir string) string {
	hold := func(call string) string {
		return "printf 'x\\n' > " + filepath.Join(dir, "at-"+call) + "\n" +
			"until [ -f " + filepath.Join(dir, "release-"+call) + " ]; do sleep 0.005; done\n"
	}
	return `
IFS= read -r line
printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{}}'
IFS= read -r line
IFS= read -r line
` + hold("thread-start") + `printf '%s\n' '{"jsonrpc":"2.0","id":2,"result":{"thread":{"id":"thread-1"}}}'
IFS= read -r line
` + hold("turn-start") + `printf '%s\n' '{"jsonrpc":"2.0","id":3,"result":{"turn":{"id":"turn-1"}}}'
printf '%s\n' '{"jsonrpc":"2.0","method":"turn/completed","params":{}}'
`
}

// awaitHeld blocks until the scripted app-server is holding the named call open,
// which is the point at which the only live budget is that call's.
func awaitHeld(t *testing.T, dir, call string) {
	t.Helper()
	agenttest.AwaitFile(t, filepath.Join(dir, "at-"+call), func(string) bool { return true },
		"the scripted app-server never reached "+call)
}

func release(t *testing.T, dir, call string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "release-"+call), []byte("go\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestStartTimeoutGovernsThreadStartDistinctlyFromReadTimeout proves the
// cold-start seam (PMR-57): thread/start is bounded by the generous start
// timeout and not by the small steady-state read timeout, so a cold app-server's
// first model load cannot trip mid-turn hang detection -- while every RPC after
// it keeps the read timeout.
//
// The assertion is which budget is live while a call is outstanding, which is
// the bound itself rather than a proxy for it. It used to be "a thread/start
// that slept two real seconds neither failed nor was rescued", and that made the
// outcome depend on how much CPU the test got: both halves failed reproducibly
// under concurrent load, because a handshake that lost the CPU for a second
// tripped the bound the test was not testing (PMR-96).
func TestStartTimeoutGovernsThreadStartDistinctlyFromReadTimeout(t *testing.T) {
	// A read timeout far smaller than the start timeout, so which of the two is
	// scheduled is unambiguous. Neither elapses unless a subtest says so.
	settings := func(dir, script string) domain.AgentRequest {
		req := request(dir, script)
		req.ReadTimeout = 200 * time.Millisecond
		req.StartTimeout = 10 * time.Second
		return req
	}
	t.Run("thread/start is governed by the start timeout and the next call by the read timeout", func(t *testing.T) {
		dir := t.TempDir()
		script := writeAppServer(t, dir, gatedThreadStart(dir))
		req := settings(dir, script)
		b := New()
		timer := timedBackend(t, b)
		type outcome struct {
			events <-chan domain.Event
			err    error
		}
		started := make(chan outcome, 1)
		go func() {
			_, events, err := b.Start(context.Background(), req)
			started <- outcome{events: events, err: err}
		}()
		// While thread/start is outstanding, the start timeout is the only live
		// budget: a read timeout among them would mean the small steady-state
		// bound governs a cold start.
		awaitHeld(t, dir, "thread-start")
		if live := timer.AwaitLive(t, req.StartTimeout); len(live) != 1 {
			t.Fatalf("live budgets while thread/start was outstanding=%v, want only the start timeout", live)
		}
		release(t, dir, "thread-start")
		// turn/start is a steady-state RPC, so it is the read timeout's.
		awaitHeld(t, dir, "turn-start")
		if live := timer.AwaitLive(t, req.ReadTimeout); len(live) != 1 {
			t.Fatalf("live budgets while turn/start was outstanding=%v, want only the read timeout", live)
		}
		release(t, dir, "turn-start")
		result := agenttest.Await(t, started, "the released thread/start never returned from Start")
		if result.err != nil {
			t.Fatalf("a cold start inside the start timeout failed: %v", result.err)
		}
		seenCompleted := false
		for _, event := range agenttest.DrainEvents(t, result.events) {
			if event.Kind == domain.EventFailed {
				t.Fatalf("a slow-but-in-budget thread/start produced failure: %+v", event)
			}
			seenCompleted = seenCompleted || event.Kind == domain.EventCompleted
		}
		if !seenCompleted {
			t.Fatal("a slow-but-in-budget thread/start did not complete")
		}
	})
	t.Run("the elapsed start timeout is what fails a thread/start", func(t *testing.T) {
		dir := t.TempDir()
		script := writeAppServer(t, dir, gatedThreadStart(dir))
		req := settings(dir, script)
		b := New()
		timer := timedBackend(t, b)
		failed := make(chan error, 1)
		go func() {
			_, _, err := b.Start(context.Background(), req)
			failed <- err
		}()
		// The release marker is never written: with thread/start held open, its
		// budget elapsing is the only thing that can end this call.
		awaitHeld(t, dir, "thread-start")
		timer.Elapse(t, req.StartTimeout)
		err := agenttest.Await(t, failed, "the elapsed start timeout did not end thread/start at all")
		if err == nil || !strings.Contains(err.Error(), "thread/start timed out") {
			t.Fatalf("the elapsed start timeout did not bound thread/start: err=%v", err)
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
	settingsFn := func() config.Settings {
		settingsCalls++
		return settings
	}
	b := NewWithSettings(settingsFn)
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
			snapshot := func() config.Settings { return settings }
			b := NewWithSettings(snapshot)
			_, events, err := b.Start(context.Background(), hostPrepared(t, snapshot, domain.AgentRequest{Issue: domain.Issue{ID: "active", Identifier: "PMR-37", State: test.issueState}, Workspace: dir, Prompt: "work", Command: "sh " + script, ApprovalPolicy: "never", ThreadSandbox: "workspace-write", TurnTimeout: time.Minute}))
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
			// The two are exclusive: a landing dispatch's delivery is the merge, so
			// it is served no publish tool, and a dispatch in any other state is
			// served publish and no landing tool (PMR-169).
			if gotPublish := strings.Contains(string(data), "github_publish_pr"); gotPublish == test.wantLand {
				t.Fatalf("issueState=%q github_publish_pr present=%v alongside github_land_pr present=%v: %s", test.issueState, gotPublish, test.wantLand, data)
			}
			// Trust boundary: the agent is never advertised a Linear-mutating tool.
			if strings.Contains(string(data), "linear_graphql") {
				t.Fatalf("issueState=%q advertised a Linear-mutating linear_graphql tool: %s", test.issueState, data)
			}
		})
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
	settingsFn := func() config.Settings { return settings }
	b := NewWithSettings(settingsFn, "LINEAR_API_KEY")
	_, events, err := b.Start(context.Background(), hostPrepared(t, settingsFn, domain.AgentRequest{Issue: domain.Issue{ID: "active"}, Workspace: dir, Prompt: "work", Command: "sh " + script, ApprovalPolicy: "never", ThreadSandbox: "workspace-write", TurnTimeout: time.Minute}))
	if err != nil {
		t.Fatal(err)
	}
	var dynamicToolCalls []domain.Event
	for event := range events {
		if event.ItemType == "dynamicToolCall" {
			dynamicToolCalls = append(dynamicToolCalls, event)
		}
	}
	// One read and one Backlog-state resolution bind the session, followed by
	// one re-read before mutation (ensureMutable) and the create mutation.
	if graphQLCalls != 4 {
		t.Fatalf("GraphQL calls=%d want prepare+state+ensure+create=4", graphQLCalls)
	}
	// create_followup_issue is deliberately not reported as a dynamicToolCall
	// (see docs/observability.md): a reported call would be tracked as an
	// outstanding operation and would surface in heartbeat and stall records.
	if len(dynamicToolCalls) != 0 {
		t.Fatalf("follow-up creation emitted %d dynamicToolCall events: %+v", len(dynamicToolCalls), dynamicToolCalls)
	}
}

// TestDynamicToolsWrapEveryDefinitionInTheAppServerEnvelope pins the wire shape
// this adapter owns. The definitions themselves are asserted in
// internal/capability; what is asserted here is that each one reaches the
// app-server as a function tool whose schema is carried under "inputSchema".
// Without this, renaming or dropping that key would advertise tools with no
// schema at all -- no additionalProperties:false, no length bound -- and leave
// only the provider-side parsers between a model and unbounded arguments.
func TestDynamicToolsWrapEveryDefinitionInTheAppServerEnvelope(t *testing.T) {
	settings := config.Settings{}
	settings.Tracker.FollowupIssueCreation = true
	settings.GitHub.MergeState = "Merging"
	registry := capability.Build(capability.Bindings{
		Settings: settings,
		Issue:    domain.Issue{Identifier: "PMR-1", State: "Merging"},
		Handoff:  &linear.HandoffSession{},
		GitHub:   &githubhost.Session{},
	})
	tools := dynamicTools(registry)
	// Four, not five: these bindings are a landing dispatch, which is served
	// github_land_pr in place of github_publish_pr rather than in addition to it.
	if len(tools) != 4 {
		t.Fatalf("wrapped %d tools, want 4", len(tools))
	}
	for _, tool := range tools {
		if tool["type"] != "function" {
			t.Fatalf("tool type=%#v, want \"function\": %#v", tool["type"], tool)
		}
		name, ok := tool["name"].(string)
		if !ok || name == "" {
			t.Fatalf("tool name=%#v", tool["name"])
		}
		if description, ok := tool["description"].(string); !ok || description == "" {
			t.Fatalf("%s description=%#v", name, tool["description"])
		}
		schema, ok := tool["inputSchema"].(map[string]any)
		if !ok {
			t.Fatalf("%s carries no inputSchema: %#v", name, tool)
		}
		if schema["type"] != "object" || schema["additionalProperties"] != false {
			t.Fatalf("%s schema=%#v", name, schema)
		}
		for key := range tool {
			switch key {
			case "type", "name", "description", "inputSchema":
			default:
				t.Fatalf("%s carries unexpected envelope field %q", name, key)
			}
		}
	}
}

func TestDynamicToolsIsEmptyWithoutCapabilities(t *testing.T) {
	if tools := dynamicTools(capability.Build(capability.Bindings{})); len(tools) != 0 {
		t.Fatalf("wrapped %d tools without any bound capability", len(tools))
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

// TestConfiguredNetworkAccessSurvivesAsEffectiveTurnPolicyWithoutCredentials
// covers the whole path PMR-80 made durable: the repository-owned
// codex.turn_sandbox_policy is loaded from WORKFLOW.md front matter, narrowed
// by the launcher, and sent as the turn's effective sandboxPolicy. Sockets are
// allowed so repository validation can bind a local loopback listener, while
// writes stay confined to the workspace plus the narrowed Git roots and the
// host's Linear and GitHub credentials never reach the child environment.
func TestConfiguredNetworkAccessSurvivesAsEffectiveTurnPolicyWithoutCredentials(t *testing.T) {
	dir := t.TempDir()
	gitMetadata := filepath.Join(t.TempDir(), "git-common")
	t.Setenv("PMR80_LINEAR_KEY", "linear-token")
	t.Setenv("PMR80_GITHUB_TOKEN", "github-token")
	t.Setenv("PMR80_INHERITED_COPY", "Bearer github-token")
	workflow := filepath.Join(dir, "WORKFLOW.md")
	front := "---\ntracker: {kind: linear, provider: {api_key: $PMR80_LINEAR_KEY}, active_states: [Todo], terminal_states: [Done]}\n" +
		"codex: {thread_sandbox: workspace-write, turn_sandbox_policy: {type: workspaceWrite, networkAccess: true}}\n---\nprompt"
	if err := os.WriteFile(workflow, []byte(front), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(workflow, "")
	if err != nil {
		t.Fatal(err)
	}
	settings := loaded.Config
	settings.HostSecretEnvNames = append(settings.HostSecretEnvNames, "PMR80_GITHUB_TOKEN")
	settings.HostSecretValues = append(settings.HostSecretValues, "github-token")
	script := writeAppServer(t, dir, `
env > environment
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
	settingsFn := func() config.Settings { return settings }
	b := NewWithSettings(settingsFn)
	_, events, err := b.Start(context.Background(), domain.AgentRequest{
		Workspace: dir, GitMetadataRoots: []string{gitMetadata}, Prompt: "work", Command: "sh " + script,
		ApprovalPolicy: settings.Codex.ApprovalPolicy, ThreadSandbox: settings.Codex.ThreadSandbox,
		TurnSandboxPolicy: settings.Codex.TurnSandboxPolicy, TurnTimeout: time.Minute,
		ReadTimeout: 30 * time.Second, StartTimeout: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}
	data, err := os.ReadFile(filepath.Join(dir, "turn.json"))
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
	policy := request.Params.SandboxPolicy
	if policy["type"] != "workspaceWrite" || policy["networkAccess"] != true {
		t.Fatalf("effective turn sandbox policy=%v want workspaceWrite with networkAccess enabled", policy)
	}
	roots, ok := policy["writableRoots"].([]any)
	if !ok || len(roots) != 1 || roots[0] != gitMetadata {
		t.Fatalf("writable roots=%v want only the narrowed Git metadata grant; network access must not widen write authority", policy["writableRoots"])
	}
	environment, err := os.ReadFile(filepath.Join(dir, "environment"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"PMR80_LINEAR_KEY=", "PMR80_GITHUB_TOKEN=", "PMR80_INHERITED_COPY="} {
		if strings.Contains(string(environment), name) {
			t.Fatalf("network-enabled child environment retained credential %q", name)
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
	return &client{in: in, timer: realTimer{}, pending: map[int]chan callResult{}, done: make(chan struct{})}
}

// streamingClient is a client whose turn is already streaming: an activated sink
// of the size a real turn gets, and nothing else a read loop needs.
func streamingClient() (*client, *agentstream.Sink) {
	c := bareClient(nopWriteCloser{Writer: io.Discard})
	c.active = agentstream.NewSink(eventBuffer)
	return c, c.active
}

// heldClient is a client whose turn exists but whose session identity does not
// yet, which is the window turn/start is outstanding in: everything emitted is
// held until activate opens the stream with that identity.
func heldClient() (*client, *agentstream.Sink) {
	c := bareClient(nopWriteCloser{Writer: io.Discard})
	c.active = agentstream.NewHeldSink(eventBuffer)
	c.activeDone = make(chan struct{})
	return c, c.active
}

// timedBackend is a backend whose every session bound -- the handshake and
// thread/start budgets, and the turn budget -- elapses when this test says so
// rather than when a real clock says so. See codex.Timer and agenttest.FakeTimer.
func timedBackend(t *testing.T, b *Backend) *agenttest.FakeTimer {
	t.Helper()
	timer := agenttest.NewFakeTimer()
	b.timer = timer
	return timer
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

// hostPrepared does the host's half of a dispatch, because these tests start
// sessions directly rather than through the scheduler: one Linear handoff and one
// GitHub manager, prepared into the one capability set the request carries. There
// is no production constructor that mints providers for a backend, so a test that
// needs them does the host's wiring itself; each test process gets its own pair,
// which is exactly the one-per-process rule.
func hostPrepared(t *testing.T, snapshot func() config.Settings, r domain.AgentRequest) domain.AgentRequest {
	t.Helper()
	carried, err := capability.NewPreparer(linear.NewHandoff(snapshot), githubhost.New(snapshot, nil)).
		Prepare(context.Background(), snapshot(), r.Issue, r.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	r.Capabilities = carried
	return r
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

// TestFileFormCredentialPathIsRemovedByName pins the name-based half of the
// environment blocklist. For the api_key_file form the canonical WORKFLOW.md
// actually uses, the variable holds a *path* rather than the secret, so
// no value filter ever matches it and settings.HostSecretEnvNames is the only
// control keeping it out of the child. That control is load-bearing now that
// the turn policy grants network access: reads outside the workspace are not
// sandboxed, so a worker that learned this path could read the credential and
// send it. Deleting settings.HostSecretEnvNames from Start left every other
// test in this package green.
func TestFileFormCredentialPathIsRemovedByName(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(t.TempDir(), "linear-api-key")
	if err := os.WriteFile(keyFile, []byte("linear-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PMR80_KEY_FILE", keyFile)
	workflow := filepath.Join(dir, "WORKFLOW.md")
	front := "---\ntracker: {kind: linear, provider: {api_key_file: $PMR80_KEY_FILE}, active_states: [Todo], terminal_states: [Done]}\n" +
		"codex: {thread_sandbox: workspace-write, turn_sandbox_policy: {type: workspaceWrite, networkAccess: true}}\n---\nprompt"
	if err := os.WriteFile(workflow, []byte(front), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(workflow, "")
	if err != nil {
		t.Fatal(err)
	}
	settings := loaded.Config
	if !slices.Contains(settings.HostSecretEnvNames, "PMR80_KEY_FILE") {
		t.Fatalf("host secret env names=%v want the api_key_file reference captured", settings.HostSecretEnvNames)
	}
	script := writeAppServer(t, dir, `
env > environment
IFS= read -r line
printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{}}'
IFS= read -r line
IFS= read -r line
printf '%s\n' '{"jsonrpc":"2.0","id":2,"result":{"thread":{"id":"thread-1"}}}'
IFS= read -r line
printf '%s\n' '{"jsonrpc":"2.0","id":3,"result":{"turn":{"id":"turn-1"}}}'
printf '%s\n' '{"jsonrpc":"2.0","method":"turn/completed","params":{}}'
`)
	settingsFn := func() config.Settings { return settings }
	b := NewWithSettings(settingsFn)
	_, events, err := b.Start(context.Background(), domain.AgentRequest{
		Workspace: dir, Prompt: "work", Command: "sh " + script,
		ApprovalPolicy: settings.Codex.ApprovalPolicy, ThreadSandbox: settings.Codex.ThreadSandbox,
		TurnSandboxPolicy: settings.Codex.TurnSandboxPolicy, TurnTimeout: time.Minute,
		ReadTimeout: 30 * time.Second, StartTimeout: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}
	environment, err := os.ReadFile(filepath.Join(dir, "environment"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(environment), "PMR80_KEY_FILE=") {
		t.Fatal("child environment retained the credential file path; only the name blocklist can remove it for the api_key_file form")
	}
}

// TestABackendHoldsNoProviderOfItsOwn pins the backend half of the ownership the
// host depends on: this backend constructs no provider and holds none, so it
// cannot be the source of a second githubhost.Manager and cannot prepare a
// registry the host did not (PMR-182). The manager holds the linked table Poll
// walks and the exactly-once Linear completion guard, so a process holding two
// would poll a table no session ever writes into -- an issue whose pull request
// merged would never reconcile to Done.
//
// The other two halves of the invariant are asserted where they now live: that
// the preparation reports the instance it binds, in internal/capability, and that
// the host polls that very instance, in cmd/symphony where the wiring is.
func TestABackendHoldsNoProviderOfItsOwn(t *testing.T) {
	// Settings that would advertise every capability if this backend still read
	// them: an enabled integration, a handoff state, and a Merging issue.
	settings := config.Settings{
		Tracker: config.Tracker{Provider: map[string]any{"api_key": "linear-token", "project_slug_id": "project-1"}, ActiveStates: []string{"Merging"}, HandoffState: "In Review"},
		GitHub: config.GitHub{Enabled: true, Owner: "owner", Repository: "repo", BaseBranch: "main",
			Token: "github-token", Endpoint: "https://api.github.com", MergeState: "Merging", MergeMethod: "merge"},
	}
	if _, ok := any(NewWithSettings(func() config.Settings { return settings })).(interface {
		GitHubManager() *githubhost.Manager
	}); ok {
		t.Fatal("this backend reports a GitHub manager, so it holds a provider of its own")
	}
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
	b := NewWithSettings(func() config.Settings { return settings })
	// The request carries no preparation, which is the whole test: nothing else
	// reaches a provider, and no live provider round trip happens below.
	r := request(dir, script)
	r.Issue = domain.Issue{ID: "issue-1", Identifier: "PMR-182", State: "Merging"}
	_, events, err := b.Start(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}
	params, err := os.ReadFile(captured)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(params), "dynamicTools") {
		t.Fatalf("a request carrying no prepared capabilities still advertised tools, so this backend built a registry of its own: %s", params)
	}
}

// TestThePreparedRequestDrivesWhatReachesTheAppServer is the behavioural half:
// the capability set the host prepared, carried on the request, is what decides
// which tools this transport advertises and which credentials its child is
// stripped of. Without this, a Start that accepted the preparation and then
// ignored it would still satisfy the identity assertions above.
func TestThePreparedRequestDrivesWhatReachesTheAppServer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		query, _ := request["query"].(string)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(query, "SymphonyLinearHandoffIssue"):
			_, _ = w.Write([]byte(`{"data":{"issue":{"id":"active","identifier":"PMR-93","title":"Own the providers","description":"safe","url":"https://linear.app/issue/PMR-93","project":{"slugId":"project-1"},"team":{"id":"team-1"},"state":{"id":"todo","name":"Todo"}}}}`))
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
	snapshot := func() config.Settings { return settings }
	dir := t.TempDir()
	captured := filepath.Join(dir, "thread-start.json")
	environment := filepath.Join(dir, "environment")
	script := writeAppServer(t, dir, `
env > `+environment+`
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
	b := NewWithSettings(snapshot, "PMR93_LINEAR_TOKEN")
	t.Setenv("PMR93_LINEAR_TOKEN", "linear-token")
	t.Setenv("PMR93_INHERITED_GITHUB_TOKEN", "prefix-github-token-suffix")
	_, events, err := b.Start(context.Background(), hostPrepared(t, snapshot, domain.AgentRequest{Issue: domain.Issue{ID: "active", Identifier: "PMR-93", State: "Merging"}, Workspace: dir, Prompt: "work", Command: "sh " + script, ApprovalPolicy: "never", ThreadSandbox: "workspace-write", TurnTimeout: time.Minute}))
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}
	params, err := os.ReadFile(captured)
	if err != nil {
		t.Fatal(err)
	}
	// The registry the host prepared reached the app-server: every GitHub tool a
	// landing dispatch is served, land included for the configured Merging state,
	// which is only true when the Linear handoff was prepared too -- without one
	// no GitHub session exists.
	for _, tool := range []string{"refresh_base_ref", "github_pr_context", "github_land_pr"} {
		if !strings.Contains(string(params), tool) {
			t.Fatalf("passed providers did not advertise %s: %s", tool, params)
		}
	}
	// Publish is the one GitHub capability a Merging dispatch is deliberately not
	// told about, so that landing is its only delivery (PMR-169).
	if strings.Contains(string(params), "github_publish_pr") {
		t.Fatalf("a landing dispatch was advertised github_publish_pr: %s", params)
	}
	if strings.Contains(string(params), "linear_graphql") {
		t.Fatalf("advertised a Linear-mutating tool: %s", params)
	}
	child, err := os.ReadFile(environment)
	if err != nil {
		t.Fatal(err)
	}
	// The same preparation owns secret filtering: the matcher carried beside the
	// registry strips both the named credential and an unrelated variable that
	// merely contains the GitHub token.
	for _, leaked := range []string{"PMR93_LINEAR_TOKEN=", "PMR93_INHERITED_GITHUB_TOKEN="} {
		if strings.Contains(string(child), leaked) {
			t.Fatalf("child environment retained %q", leaked)
		}
	}
}

// TestNoHostCredentialReachesTheChildEnvironment is this backend's whole-boundary
// proof of the guarantee config.ReservedSecretEnvNames documents. Its counterpart
// is internal/claude's TestHostSecretsNeverReachTheChild, so a filter cannot hold
// on one transport and not the other.
//
// The reserved names are written out here rather than read from
// config.ReservedSecretEnvNames, deliberately: a test that iterates the list
// asserts nothing about its contents, and dropping an entry would leave it
// green. Before this test, dropping any of the five left this whole package
// green -- LINEAR_API_KEY included, because the one test that set it gave it a
// value that a configured HostSecretValues entry also matched, so the name
// filter was never what removed it.
//
// The provider case is the acceptance criterion in full: PMR94_INHERITED_FORGE
// and PMR94_INHERITED_TRACKER carry the bound GitHub and Linear sessions' own
// credentials under names no list mentions, and neither credential appears in
// HostSecretEnvNames or HostSecretValues, so only the session's provider matcher
// can see them.
func TestNoHostCredentialReachesTheChildEnvironment(t *testing.T) {
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
			_, _ = w.Write([]byte(`{"data":{"issue":{"id":"issue-1","identifier":"PMR-94","title":"Filter","description":"safe",` +
				`"url":"https://linear.app/issue/PMR-94","project":{"slugId":"project-1"},"team":{"id":"team-1"},` +
				`"state":{"id":"todo","name":"Todo"}}}}`))
		case strings.Contains(query.Query, "SymphonyLinearHandoffStates"):
			_, _ = w.Write([]byte(`{"data":{"team":{"id":"team-1","states":{"nodes":[{"id":"review","name":"In Review"}]}}}}`))
		default:
			t.Errorf("unexpected query: %s", query.Query)
		}
	}))
	defer tracker.Close()

	// Filter 1: the reserved names, whatever the workflow says. Each value is
	// unique and is matched by no other filter, so only the name can remove it.
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
	// Filter 2: a configured name. Filter 3: a configured value under a name no
	// list mentions. Filter 4: the bound provider session's own credential,
	// under a name no list mentions and with no configured value at all.
	t.Setenv("PMR94_CONFIGURED_NAME", "configured-name-value")
	t.Setenv("PMR94_PADDED_NAME", "padded-name-value")
	t.Setenv("PMR94_INHERITED_CONFIGURED", "Bearer configured-secret-value")
	t.Setenv("PMR94_INHERITED_FORGE", "prefix-provider-forge-token-suffix")
	t.Setenv("PMR94_INHERITED_TRACKER", "wraps provider-tracker-key inside")
	t.Setenv("PMR94_KEPT", "ordinary-value")

	settings := config.Settings{
		Tracker: config.Tracker{
			Provider:     map[string]any{"api_key": "provider-tracker-key", "project_slug_id": "project-1", "endpoint": tracker.URL},
			ActiveStates: []string{"Todo"}, HandoffState: "In Review",
		},
		GitHub: config.GitHub{Enabled: true, Owner: "owner", Repository: "repo", BaseBranch: "main",
			Token: "provider-forge-token", Endpoint: tracker.URL, MergeState: "Merging", MergeMethod: "merge"},
		// The padded and blank names are hostenv.Filter's, and are here because
		// this test launches the real backend: a Settings that never went
		// through config.Load can carry either, and the launcher must reach the
		// filter that handles them.
		HostSecretEnvNames: []string{"PMR94_CONFIGURED_NAME", "  PMR94_PADDED_NAME  ", "   "},
		HostSecretValues:   []string{"configured-secret-value"},
	}
	dir := t.TempDir()
	environment := filepath.Join(dir, "environment")
	script := writeAppServer(t, dir, `
env > `+environment+`
IFS= read -r line
printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{}}'
IFS= read -r line
IFS= read -r line
printf '%s\n' '{"jsonrpc":"2.0","id":2,"result":{"thread":{"id":"thread-1"}}}'
IFS= read -r line
printf '%s\n' '{"jsonrpc":"2.0","id":3,"result":{"turn":{"id":"turn-1"}}}'
printf '%s\n' '{"jsonrpc":"2.0","method":"turn/completed","params":{}}'
`)
	snapshot := func() config.Settings { return settings }
	b := NewWithSettings(snapshot)
	r := request(dir, script)
	r.Issue = domain.Issue{ID: "issue-1", Identifier: "PMR-94", State: "Todo"}
	if _, events, err := b.Start(context.Background(), hostPrepared(t, snapshot, r)); err != nil {
		t.Fatal(err)
	} else {
		for range events {
		}
	}
	child := readChildEnvironment(t, environment)
	names := childEnvironmentNames(child)
	for name, value := range reserved {
		if slices.Contains(names, name) {
			t.Fatalf("child environment retained reserved variable %s", name)
		}
		if strings.Contains(child, value) {
			t.Fatalf("child environment retained the value of reserved variable %s", name)
		}
	}
	for _, leaked := range []string{"configured-name-value", "padded-name-value",
		"configured-secret-value", "provider-forge-token", "provider-tracker-key"} {
		if strings.Contains(child, leaked) {
			t.Fatalf("child environment retained %q", leaked)
		}
	}
	if !strings.Contains(child, "ordinary-value") {
		t.Fatal("the host credential filter removed unrelated variables")
	}
}

// TestAnOperatorProfileReachesNeitherTheChildEnvironmentNorItsStream covers the
// launch form rather than the filter: the app-server runs under `bash -c`, so
// the operator's profile is never sourced and the filtered environment is the
// environment the child actually gets. Under `bash -lc` this one fixture broke
// the boundary twice -- it re-exported LINEAR_API_KEY, a reserved name
// hostenv.Filter had just removed, and its greeting landed on the stdout read()
// decodes as JSON-RPC, failing every session at start (PMR-172).
//
// HOME is redirected at this test's own process, so the fixture is the profile
// a login shell would read and no operator file is involved.
func TestAnOperatorProfileReachesNeitherTheChildEnvironmentNorItsStream(t *testing.T) {
	home := t.TempDir()
	profile := "echo 'nvm: a profile that prints on stdout'\nexport LINEAR_API_KEY=profile-re-exported-key\n"
	if err := os.WriteFile(filepath.Join(home, ".bash_profile"), []byte(profile), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("LINEAR_API_KEY", "reserved-linear-key-value")

	dir := t.TempDir()
	environment := filepath.Join(dir, "environment")
	script := writeAppServer(t, dir, `
env > `+environment+`
IFS= read -r line
printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{}}'
IFS= read -r line
IFS= read -r line
printf '%s\n' '{"jsonrpc":"2.0","id":2,"result":{"thread":{"id":"thread-1"}}}'
IFS= read -r line
printf '%s\n' '{"jsonrpc":"2.0","id":3,"result":{"turn":{"id":"turn-1"}}}'
printf '%s\n' '{"jsonrpc":"2.0","method":"turn/completed","params":{}}'
`)
	_, events, err := New().Start(context.Background(), request(dir, script))
	if err != nil {
		t.Fatalf("the profile's stdout reached the JSON-RPC stream: %v", err)
	}
	for range events {
	}
	child := readChildEnvironment(t, environment)
	if slices.Contains(childEnvironmentNames(child), "LINEAR_API_KEY") {
		t.Fatalf("the profile re-exported the reserved name the filter removed: %s", child)
	}
	if strings.Contains(child, "profile-re-exported-key") {
		t.Fatalf("the profile's value reached the child environment: %s", child)
	}
}

// childEnvironmentNames reads the variable names out of `env` output, so an
// absence assertion on one name cannot be satisfied by another name that merely
// ends with it -- GITHUB_TOKEN inside SYMPHONY_GITHUB_TOKEN, for example.
//
// Only a line whose prefix is shaped like a variable name counts, because a
// developer or CI machine can hold a multi-line value whose continuation lines
// would otherwise read as names and produce a confusing failure.
func childEnvironmentNames(child string) []string {
	var names []string
	for _, line := range strings.Split(child, "\n") {
		if name, _, found := strings.Cut(line, "="); found && environmentName(name) {
			names = append(names, name)
		}
	}
	return names
}

func environmentName(candidate string) bool {
	if candidate == "" {
		return false
	}
	for i, r := range candidate {
		digit := r >= '0' && r <= '9'
		if !(r == '_' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || digit && i > 0) {
			return false
		}
	}
	return true
}

func readChildEnvironment(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// TestABoundGitHubManagerWithoutAHandoffStillStripsItsToken closes a hole the
// provider matcher had from the day it was written, in both backends
// identically. githubhost.Manager.PrepareWithSettings returns nil when no Linear
// handoff was prepared, so a workflow with github.enabled but no handoff_state
// and no followup_issue_creation leaves a bound manager, no session -- and, until
// capability.SecretMatcher took the manager as its fallback, a matcher that was
// live and answered false for every candidate, the forge token included.
//
// It was never a leak through a loaded workflow, because HostSecretValues covers
// the same token there. It is a leak for any Settings not produced by Load,
// which is the case filter 4 exists for, so leaving it would have made the
// documented reason false.
func TestABoundGitHubManagerWithoutAHandoffStillStripsItsToken(t *testing.T) {
	settings := config.Settings{
		// No handoff_state and no followup_issue_creation: nothing prepares a
		// Linear handoff, so nothing prepares a GitHub session either.
		Tracker: config.Tracker{Provider: map[string]any{"api_key": "provider-tracker-key"}, ActiveStates: []string{"Todo"}},
		GitHub: config.GitHub{Enabled: true, Owner: "owner", Repository: "repo", BaseBranch: "main",
			Token: "provider-forge-token", Endpoint: "https://api.github.com", MergeState: "Merging", MergeMethod: "merge"},
	}
	t.Setenv("PMR94_INHERITED_FORGE", "prefix-provider-forge-token-suffix")
	t.Setenv("PMR94_KEPT", "ordinary-value")
	dir := t.TempDir()
	environment := filepath.Join(dir, "environment")
	script := writeAppServer(t, dir, `
env > `+environment+`
IFS= read -r line
printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{}}'
IFS= read -r line
IFS= read -r line
printf '%s\n' '{"jsonrpc":"2.0","id":2,"result":{"thread":{"id":"thread-1"}}}'
IFS= read -r line
printf '%s\n' '{"jsonrpc":"2.0","id":3,"result":{"turn":{"id":"turn-1"}}}'
printf '%s\n' '{"jsonrpc":"2.0","method":"turn/completed","params":{}}'
`)
	snapshot := func() config.Settings { return settings }
	b := NewWithSettings(snapshot)
	r := request(dir, script)
	r.Issue = domain.Issue{ID: "issue-1", Identifier: "PMR-94", State: "Todo"}
	if _, events, err := b.Start(context.Background(), hostPrepared(t, snapshot, r)); err != nil {
		t.Fatal(err)
	} else {
		for range events {
		}
	}
	child := readChildEnvironment(t, environment)
	if strings.Contains(child, "provider-forge-token") {
		t.Fatal("a bound GitHub manager's token reached the child because no session was prepared to match it")
	}
	if !strings.Contains(child, "ordinary-value") {
		t.Fatal("the manager fallback removed unrelated variables")
	}
}

// TestTheConstructorsExtraSecretNamesAreBlocked is this backend's whole share of
// the environment filter now that hostenv.Filter owns the loop: the names New
// was given are the only input Start contributes that no other caller does, and
// nothing is added after filtering. Everything the filter itself does is proven
// once, in internal/hostenv.
func TestTheConstructorsExtraSecretNamesAreBlocked(t *testing.T) {
	t.Setenv("PMR99_CONSTRUCTOR_NAME", "constructor-secret")
	t.Setenv("PMR99_KEPT", "ordinary-value")
	dir := t.TempDir()
	environment := filepath.Join(dir, "environment")
	script := writeAppServer(t, dir, `
env > `+environment+`
IFS= read -r line
printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{}}'
IFS= read -r line
IFS= read -r line
printf '%s\n' '{"jsonrpc":"2.0","id":2,"result":{"thread":{"id":"thread-1"}}}'
IFS= read -r line
printf '%s\n' '{"jsonrpc":"2.0","id":3,"result":{"turn":{"id":"turn-1"}}}'
printf '%s\n' '{"jsonrpc":"2.0","method":"turn/completed","params":{}}'
`)
	b := New("PMR99_CONSTRUCTOR_NAME")
	if _, events, err := b.Start(context.Background(), request(dir, script)); err != nil {
		t.Fatal(err)
	} else {
		for range events {
		}
	}
	child := readChildEnvironment(t, environment)
	if strings.Contains(child, "constructor-secret") {
		t.Fatal("a name this backend's constructor was given reached the child")
	}
	if !strings.Contains(child, "ordinary-value") {
		t.Fatal("the host credential filter removed unrelated variables")
	}
}

// The tool cache grant reaches the Codex workspace-write policy too, so the
// same workflow field means the same thing on either backend. An unset
// CacheRoot must leave writableRoots exactly as it was.
func TestLocalCommitSandboxGrantsCacheRoot(t *testing.T) {
	objects, cache := "/tmp/src/.git/objects", "/tmp/cache"
	base := domain.AgentRequest{GitMetadataRoots: []string{objects}, ThreadSandbox: "workspace-write"}

	without, ok := localCommitSandbox(base).(map[string]any)
	if !ok {
		t.Fatalf("policy=%T, want a workspaceWrite map", localCommitSandbox(base))
	}
	if roots, _ := without["writableRoots"].([]string); len(roots) != 1 || roots[0] != objects {
		t.Fatalf("writableRoots=%v, want only the git object store when no cache is configured", without["writableRoots"])
	}

	withCache := base
	withCache.CacheRoot = cache
	granted, ok := localCommitSandbox(withCache).(map[string]any)
	if !ok {
		t.Fatalf("policy=%T, want a workspaceWrite map", localCommitSandbox(withCache))
	}
	roots, _ := granted["writableRoots"].([]string)
	var found bool
	for _, root := range roots {
		if root == cache {
			found = true
		}
	}
	if !found {
		t.Fatalf("writableRoots=%v, want the cache root granted", roots)
	}
	if len(roots) != 2 {
		t.Fatalf("writableRoots=%v, want exactly the object store and the cache root", roots)
	}
}
