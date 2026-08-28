package coordinator

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/domain"
)

func TestObservabilityNormalizesEventsAndProtectsSensitiveMessages(t *testing.T) {
	w := testSettings(t)
	issue := testIssue()
	events := make(chan domain.Event, 6)
	events <- domain.Event{Kind: domain.EventSessionStarted, At: time.Now(), SessionID: "session", ThreadID: "thread", TurnID: "turn", PID: 123}
	events <- domain.Event{Kind: domain.EventUsage, At: time.Now(), Usage: domain.Usage{InputTokens: 4, OutputTokens: 6}}
	events <- domain.Event{Kind: domain.EventUsage, At: time.Now(), Usage: domain.Usage{InputTokens: 4, OutputTokens: 6, TotalTokens: 10}}
	events <- domain.Event{Kind: domain.EventRateLimit, At: time.Now(), RateLimit: map[string]any{"remaining": float64(9), "token": "do-not-log-this"}}
	events <- domain.Event{Kind: domain.EventDiagnostic, At: time.Now(), Message: "token=do-not-log-this"}
	events <- domain.Event{Kind: domain.EventCompleted, At: time.Now(), Message: "prompt=do-not-log-this"}
	close(events)
	agent := &fakeAgent{events: func() <-chan domain.Event { return events }}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{})}
	var logs bytes.Buffer
	c := New(&fakeTracker{issue: issue}, agent, ws, func() config.Settings { return w.Config }, slog.New(slog.NewJSONHandler(&logs, nil)))
	c.Tick(context.Background())
	<-ws.after
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	output := logs.String()
	for _, secret := range []string{"do-not-log-this", "prompt=do-not-log-this", issue.Description} {
		if secret != "" && strings.Contains(output, secret) {
			t.Fatalf("operator log leaked %q: %s", secret, output)
		}
	}
	for _, field := range []string{`"thread_id":"thread"`, `"turn_id":"turn"`, `"pid":123`, `"input_tokens":4`, `"output_tokens":6`, `"total_tokens":10`, `"remaining":9`, `"runtime_ms"`} {
		if !strings.Contains(output, field) {
			t.Fatalf("operator log missing %s: %s", field, output)
		}
	}
}

func TestEmptyRateLimitSnapshotIsOmittedFromTheLog(t *testing.T) {
	w := testSettings(t)
	issue := testIssue()
	events := make(chan domain.Event, 2)
	events <- domain.Event{Kind: domain.EventRateLimit, At: time.Now(), RateLimit: map[string]any{"token": "do-not-log-this"}}
	events <- domain.Event{Kind: domain.EventCompleted, At: time.Now()}
	close(events)
	agent := &fakeAgent{events: func() <-chan domain.Event { return events }}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{})}
	var logs bytes.Buffer
	c := New(&fakeTracker{issue: issue}, agent, ws, func() config.Settings { return w.Config }, slog.New(slog.NewJSONHandler(&logs, nil)))
	c.Tick(context.Background())
	<-ws.after
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if output := logs.String(); strings.Contains(output, "agent rate limit") {
		t.Fatalf("empty rate-limit snapshot was logged: %s", output)
	}
}

func TestGenericProgressEventsAreDebugOnlyAndCoalesceRepeats(t *testing.T) {
	w := testSettings(t)
	issue := testIssue()
	events := make(chan domain.Event, 25)
	for i := 0; i < 22; i++ {
		events <- domain.Event{Kind: domain.EventProgress, At: time.Now(), Message: "thread/tokenUsage/updated"}
	}
	events <- domain.Event{Kind: domain.EventCompleted, At: time.Now()}
	close(events)
	agent := &fakeAgent{events: func() <-chan domain.Event { return events }}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{})}
	var infoLogs, debugLogs bytes.Buffer
	c := New(&fakeTracker{issue: issue}, agent, ws, func() config.Settings { return w.Config }, slog.New(slog.NewJSONHandler(&infoLogs, nil)))
	c.Tick(context.Background())
	<-ws.after
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if output := infoLogs.String(); strings.Contains(output, `"msg":"agent event"`) {
		t.Fatalf("generic progress flooded the default info log: %s", output)
	}

	events = make(chan domain.Event, 25)
	for i := 0; i < 22; i++ {
		events <- domain.Event{Kind: domain.EventProgress, At: time.Now(), Message: "thread/tokenUsage/updated"}
	}
	events <- domain.Event{Kind: domain.EventCompleted, At: time.Now()}
	close(events)
	agent = &fakeAgent{events: func() <-chan domain.Event { return events }}
	ws = &fakeWorkspace{shouldRun: true, after: make(chan struct{})}
	c = New(&fakeTracker{issue: issue}, agent, ws, func() config.Settings { return w.Config }, slog.New(slog.NewJSONHandler(&debugLogs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	c.Tick(context.Background())
	<-ws.after
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	count := strings.Count(debugLogs.String(), `"msg":"agent event"`)
	if count == 0 || count >= 22 {
		t.Fatalf("repeated generic progress was not coalesced at debug level: count=%d log=%s", count, debugLogs.String())
	}
}

func TestHeartbeatAndStallRecordOutstandingOperation(t *testing.T) {
	w := testSettings(t)
	w.Config.Codex.StallTimeout = time.Second
	issue := testIssue()
	tracker := &fakeTracker{issue: issue}
	events := make(chan domain.Event, 1)
	events <- domain.Event{Kind: domain.EventItem, ItemID: "item-1", ItemType: "commandExecution", Outcome: domain.ItemStarted}
	agent := &fakeAgent{events: func() <-chan domain.Event { return events }, started: make(chan struct{}, 1)}
	ws := &fakeWorkspace{after: make(chan struct{}, 1)}
	logs := &syncBuffer{}
	c := New(tracker, agent, ws, func() config.Settings { return w.Config }, slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	clock := &mutableClock{now: time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)}
	c.clock = clock
	timer := &fakeTimer{signal: make(chan struct{}, 1)}
	c.timer = timer

	c.Tick(context.Background())
	<-agent.started
	waitForSubstring(t, logs, `"msg":"agent item event"`, time.Second)

	clock.set(clock.now.Add(200 * time.Millisecond))
	c.Tick(context.Background())
	heartbeat := waitForSubstring(t, logs, `"msg":"agent heartbeat"`, time.Second)
	if !strings.Contains(heartbeat, `"outstanding_item_type":"commandExecution"`) || !strings.Contains(heartbeat, `"outstanding_item_id":"item-1"`) {
		t.Fatalf("heartbeat missing outstanding operation: %s", heartbeat)
	}

	clock.set(clock.now.Add(2 * time.Second))
	c.Tick(context.Background())
	<-ws.after
	<-timer.signal
	stalled := waitForSubstring(t, logs, `"reason":"stalled"`, time.Second)
	if !strings.Contains(stalled, `"outstanding_item_id":"item-1"`) || !strings.Contains(stalled, `"outstanding_item_type":"commandExecution"`) || !strings.Contains(stalled, `"last_activity_age_ms"`) {
		t.Fatalf("stall record missing outstanding operation: %s", stalled)
	}
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestNewDebugRecordsNeverCarryToolInputsOrSecrets exercises every new debug
// and info log path added for actionable diagnostics (poll summaries, item
// lifecycle, heartbeat/stall records, claim/preparation records, cleanup
// status) with representative secret-shaped values in the fields an operator
// cannot control, and asserts none of them appear in the emitted log.
func TestNewDebugRecordsNeverCarryToolInputsOrSecrets(t *testing.T) {
	w := testSettings(t)
	issue := testIssue()
	issue.Description = "issue description must-not-appear"
	tracker := &fakeTracker{issue: issue}
	events := make(chan domain.Event, 4)
	events <- domain.Event{Kind: domain.EventItem, ItemID: "item-1", ItemType: "commandExecution", ToolName: "token=do-not-log-this", Outcome: domain.ItemStarted}
	events <- domain.Event{Kind: domain.EventItem, ItemID: "item-1", ItemType: "commandExecution", ToolName: "token=do-not-log-this", Outcome: "failed", DurationMs: 5}
	events <- domain.Event{Kind: domain.EventProgress, Message: "prompt=do-not-log-this"}
	events <- domain.Event{Kind: domain.EventCompleted}
	close(events)
	agent := &fakeAgent{events: func() <-chan domain.Event { return events }}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{})}
	var logs bytes.Buffer
	c := New(tracker, agent, ws, func() config.Settings { return w.Config }, slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	c.Tick(context.Background())
	<-ws.after
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	output := logs.String()
	for _, secret := range []string{"do-not-log-this", "must-not-appear"} {
		if strings.Contains(output, secret) {
			t.Fatalf("new debug/info record leaked %q: %s", secret, output)
		}
	}
	if !strings.Contains(output, `"msg":"agent item event"`) || !strings.Contains(output, `"item_type":"commandExecution"`) {
		t.Fatalf("item event record missing: %s", output)
	}
}

// TestCapabilityToolCallIsIdentifiableByNameInTheDebugLog pins PMR-163: an
// operator debugging a run that spent its turn budget calling one bound
// capability repeatedly must be able to name which one from the debug log
// alone, rather than seeing only a generic "toolCall"/"mcpToolCall" item
// type. internal/claude already carries the CLI's own MCP-framed tool name
// onto domain.Event.ToolName for every item it emits (see
// internal/claude.TestMCPCapabilityToolCallIsIdentifiableByNameInTheEvent);
// this pins the other half, that this coordinator's existing item_name
// attribute (internal/coordinator/events.go) surfaces it once it arrives.
func TestCapabilityToolCallIsIdentifiableByNameInTheDebugLog(t *testing.T) {
	w := testSettings(t)
	issue := testIssue()
	const capabilityTool = "mcp__symphony__github_publish_pr"
	events := make(chan domain.Event, 3)
	events <- domain.Event{Kind: domain.EventItem, ItemID: "call-1", ItemType: "mcpToolCall", ToolName: capabilityTool, Outcome: domain.ItemStarted}
	events <- domain.Event{Kind: domain.EventItem, ItemID: "call-1", ItemType: "mcpToolCall", ToolName: capabilityTool, Outcome: domain.ItemCompleted, DurationMs: 5}
	events <- domain.Event{Kind: domain.EventCompleted}
	close(events)
	agent := &fakeAgent{events: func() <-chan domain.Event { return events }}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{})}
	var logs bytes.Buffer
	c := New(&fakeTracker{issue: issue}, agent, ws, func() config.Settings { return w.Config }, slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	c.Tick(context.Background())
	<-ws.after
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	output := logs.String()
	if !strings.Contains(output, `"msg":"agent item event"`) || !strings.Contains(output, `"item_name":"`+capabilityTool+`"`) {
		t.Fatalf("capability call not identifiable by name in the debug log: %s", output)
	}
}

// TestUpdateUsageAuthoritativeReplacesInflatedProvisionalPeak pins the fix for
// PMR-153: Claude's mid-turn provisional figure is this host's own running
// sum of per-API-call deltas, not the CLI's turn total, so it can overshoot
// the authoritative end-of-turn result for the very same turn. A
// component-wise max() would let that overshoot latch permanently even after
// the authoritative source corrects itself down; the recorded usage must end
// at the authoritative figure instead.
func TestUpdateUsageAuthoritativeReplacesInflatedProvisionalPeak(t *testing.T) {
	w := testSettings(t)
	issue := testIssue()
	events := make(chan domain.Event, 4)
	// A mid-turn provisional estimate, inflated above what the turn actually
	// spends -- exactly the failure mode PMR-153 describes.
	events <- domain.Event{Kind: domain.EventUsage, At: time.Now(), Usage: domain.Usage{InputTokens: 9000, OutputTokens: 9000, TotalTokens: 18000}}
	// The CLI's own authoritative end-of-turn total for that same turn, lower
	// than the provisional estimate above.
	events <- domain.Event{Kind: domain.EventUsage, At: time.Now(), Usage: domain.Usage{InputTokens: 1000, OutputTokens: 500, TotalTokens: 1500}, UsageAuthoritative: true}
	events <- domain.Event{Kind: domain.EventCompleted, At: time.Now()}
	close(events)
	agent := &fakeAgent{events: func() <-chan domain.Event { return events }}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{})}
	var logs bytes.Buffer
	c := New(&fakeTracker{issue: issue}, agent, ws, func() config.Settings { return w.Config }, slog.New(slog.NewJSONHandler(&logs, nil)))
	c.Tick(context.Background())
	<-ws.after
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	output := logs.String()
	summary := findLine(t, output, `"msg":"agent turn completed"`)
	if strings.Contains(summary, `"total_tokens":18000`) {
		t.Fatalf("authoritative usage did not replace the inflated provisional peak in the terminal summary: %s", summary)
	}
	for _, field := range []string{`"input_tokens":1000`, `"output_tokens":500`, `"total_tokens":1500`} {
		if !strings.Contains(summary, field) {
			t.Fatalf("terminal summary missing authoritative %s: %s", field, summary)
		}
	}
}

// TestUpdateUsageNonAuthoritativeNotificationsAccumulate pins the behavior
// updateUsage's component-wise max() was written for: Codex's
// thread/tokenUsage/updated notifications are genuinely cumulative and
// monotonically increasing, so repeated or successive notifications merge
// into a running total rather than ever regressing (PMR-153).
func TestUpdateUsageNonAuthoritativeNotificationsAccumulate(t *testing.T) {
	w := testSettings(t)
	issue := testIssue()
	events := make(chan domain.Event, 4)
	events <- domain.Event{Kind: domain.EventUsage, At: time.Now(), Usage: domain.Usage{InputTokens: 100, OutputTokens: 50, TotalTokens: 150}}
	events <- domain.Event{Kind: domain.EventUsage, At: time.Now(), Usage: domain.Usage{InputTokens: 400, OutputTokens: 200, TotalTokens: 600}}
	events <- domain.Event{Kind: domain.EventCompleted, At: time.Now()}
	close(events)
	agent := &fakeAgent{events: func() <-chan domain.Event { return events }}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{})}
	var logs bytes.Buffer
	c := New(&fakeTracker{issue: issue}, agent, ws, func() config.Settings { return w.Config }, slog.New(slog.NewJSONHandler(&logs, nil)))
	c.Tick(context.Background())
	<-ws.after
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	summary := findLine(t, logs.String(), `"msg":"agent turn completed"`)
	for _, field := range []string{`"input_tokens":400`, `"output_tokens":200`, `"total_tokens":600`} {
		if !strings.Contains(summary, field) {
			t.Fatalf("terminal summary missing accumulated %s: %s", field, summary)
		}
	}
}

// TestEventFailedStaysAgentEventAndPassesThroughObservabilityText covers the
// one case "agent_event" still names after PMR-115: domain.EventFailed
// carrying model or provider text. That text is attached to the retry log
// like every other reason's error, but only because observability.safeAttr
// routes an "error" attribute through observability.Text for every log call
// regardless of reason -- so it is masked and bounded exactly like any other
// diagnostic, never verbatim.
func TestEventFailedStaysAgentEventAndPassesThroughObservabilityText(t *testing.T) {
	w := testSettings(t)
	issue := testIssue()
	var log syncBuffer
	agent := &fakeAgent{events: failedEvents("claude turn failed: token=super-secret-value unspecified")}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{}, 1)}
	c := New(&fakeTracker{issue: issue}, agent, ws, func() config.Settings { return w.Config }, slog.New(slog.NewJSONHandler(&log, nil)))
	timer := &fakeTimer{signal: make(chan struct{}, 1)}
	c.timer = timer

	c.Tick(context.Background())
	<-timer.signal

	c.mu.Lock()
	retry := c.retries[issue.ID]
	c.mu.Unlock()
	if retry.kind != retryAgent || retry.reason != "agent_event" || retry.attempt != 1 {
		t.Fatalf("retry=%+v, want agent_event", retry)
	}
	records := log.String()
	if strings.Contains(records, "super-secret-value") {
		t.Fatalf("model text reached the log unredacted: %s", records)
	}
	if !strings.Contains(records, "token=[REDACTED]") {
		t.Fatalf("model text was not passed through observability.Text: %s", records)
	}
}

// TestRateLimitStatusUsesFixedLogVocabulary keeps coordinator logging on the
// backend's closed rate-limit vocabulary. The backend test covers conversion
// from the arbitrary CLI wire value; this test pins the operator-visible
// fallback and the registered operation on the non-terminal warning record.
func TestRateLimitStatusUsesFixedLogVocabulary(t *testing.T) {
	w := testSettings(t)
	issue := testIssue()
	events := make(chan domain.Event, 2)
	events <- domain.Event{Kind: domain.EventDiagnostic, At: time.Now(), Message: "claude reported a rate limit: unrecognized (five_hour)", RateLimitStatus: "unrecognized"}
	events <- domain.Event{Kind: domain.EventCompleted, At: time.Now()}
	close(events)
	agent := &fakeAgent{events: func() <-chan domain.Event { return events }}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{})}
	var logs bytes.Buffer
	c := New(&fakeTracker{issue: issue}, agent, ws, func() config.Settings { return w.Config }, slog.New(slog.NewJSONHandler(&logs, nil)))
	c.Tick(context.Background())
	<-ws.after
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	output := logs.String()
	if strings.Contains(output, "do-not-log-this") {
		t.Fatalf("rate-limit status leaked wire text: %s", output)
	}
	if !strings.Contains(output, `"status":"unrecognized"`) {
		t.Fatalf("rejection status was not normalized to the fallback: %s", output)
	}
	if !strings.Contains(output, `"operation":"rate_limit"`) {
		t.Fatalf("rejection rate-limit operation was not logged: %s", output)
	}
	if strings.Contains(output, "[REDACTED]") {
		t.Fatalf("fallback status was redacted instead of normalized: %s", output)
	}
}
