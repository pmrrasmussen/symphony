package coordinator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
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

func TestIneligibleReasonCategorizesEachRejection(t *testing.T) {
	tests := []struct {
		name   string
		issue  domain.Issue
		s      config.Settings
		reason string
	}{
		{name: "missing identity", issue: domain.Issue{}, s: config.Settings{}, reason: "missing_identity"},
		{
			name:   "not active",
			issue:  domain.Issue{ID: "a", Identifier: "X-1", Title: "t", State: "Backlog", Dispatchable: true},
			s:      config.Settings{Tracker: config.Tracker{ActiveStates: []string{"Todo"}}},
			reason: "not_active",
		},
		{
			name:   "terminal",
			issue:  domain.Issue{ID: "a", Identifier: "X-1", Title: "t", State: "Done", Dispatchable: true},
			s:      config.Settings{Tracker: config.Tracker{ActiveStates: []string{"Done"}, TerminalStates: []string{"Done"}}},
			reason: "terminal",
		},
		{
			name:   "not routable",
			issue:  domain.Issue{ID: "a", Identifier: "X-1", Title: "t", State: "Todo", Dispatchable: true},
			s:      config.Settings{Tracker: config.Tracker{ActiveStates: []string{"Todo"}, RequiredLabels: []string{"ready"}}},
			reason: "not_routable",
		},
		{
			name:   "eligible",
			issue:  domain.Issue{ID: "a", Identifier: "X-1", Title: "t", State: "Todo", Dispatchable: true},
			s:      config.Settings{Tracker: config.Tracker{ActiveStates: []string{"Todo"}}},
			reason: "",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := ineligibleReason(test.issue, test.s); got != test.reason {
				t.Fatalf("ineligibleReason=%q, want %q", got, test.reason)
			}
		})
	}
}

func TestPollSummaryReportsNoCandidatesAtDebugLevel(t *testing.T) {
	w := testSettings(t)
	tracker := &issueMapTracker{issues: map[string]domain.Issue{}}
	var logs bytes.Buffer
	c := New(tracker, &fakeAgent{}, &fakeWorkspace{}, func() config.Settings { return w.Config }, slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	c.Tick(context.Background())
	output := logs.String()
	if !strings.Contains(output, `"msg":"poll summary"`) || !strings.Contains(output, `"candidates":0`) || !strings.Contains(output, `"eligible":0`) || !strings.Contains(output, `"admitted":0`) {
		t.Fatalf("no-candidate poll summary missing expected counts: %s", output)
	}
}

func TestPollSummaryCategorizesRejectionsAndOmitsAtInfoLevel(t *testing.T) {
	w := testSettings(t)
	ready := testIssue()
	ready.ID, ready.Identifier = "ready", "ENG-3"
	claimed := testIssue()
	claimed.ID, claimed.Identifier = "claimed", "ENG-4"
	tracker := &issueMapTracker{candidates: []domain.Issue{ready, claimed}, issues: map[string]domain.Issue{ready.ID: ready, claimed.ID: claimed}}
	agent := &fakeAgent{events: closedEvents}
	ws := &fakeWorkspace{after: make(chan struct{}, 1)}
	var infoLogs, debugLogs bytes.Buffer
	c := New(tracker, agent, ws, func() config.Settings { return w.Config }, slog.New(slog.NewJSONHandler(&infoLogs, nil)))
	timer := &fakeTimer{signal: make(chan struct{}, 1)}
	c.timer = timer
	if !c.claim(claimed, w.Config) {
		t.Fatal("pre-claim failed")
	}
	c.Tick(context.Background())
	<-timer.signal
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-ws.after
	if output := infoLogs.String(); strings.Contains(output, `"msg":"poll summary"`) {
		t.Fatalf("poll summary debug detail leaked into the default info log: %s", output)
	}

	ready2 := testIssue()
	ready2.ID, ready2.Identifier = "ready2", "ENG-5"
	claimed2 := testIssue()
	claimed2.ID, claimed2.Identifier = "claimed2", "ENG-6"
	tracker2 := &issueMapTracker{candidates: []domain.Issue{ready2, claimed2}, issues: map[string]domain.Issue{ready2.ID: ready2, claimed2.ID: claimed2}}
	agent2 := &fakeAgent{events: closedEvents}
	ws2 := &fakeWorkspace{after: make(chan struct{}, 1)}
	c2 := New(tracker2, agent2, ws2, func() config.Settings { return w.Config }, slog.New(slog.NewJSONHandler(&debugLogs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	timer2 := &fakeTimer{signal: make(chan struct{}, 1)}
	c2.timer = timer2
	if !c2.claim(claimed2, w.Config) {
		t.Fatal("pre-claim failed")
	}
	c2.Tick(context.Background())
	<-timer2.signal
	if err := c2.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-ws2.after
	output := debugLogs.String()
	if !strings.Contains(output, `"candidates":2`) || !strings.Contains(output, `"eligible":2`) || !strings.Contains(output, `"admitted":1`) {
		t.Fatalf("poll summary counts=%s", output)
	}
	if !strings.Contains(output, `"already_claimed":1`) {
		t.Fatalf("poll summary missing categorized rejection: %s", output)
	}
	if !strings.Contains(output, `"issue_identifier":"ENG-6"`) || !strings.Contains(output, `"reason":"already_claimed"`) {
		t.Fatalf("per-issue rejection record missing: %s", output)
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

func TestCleanupStatusClassifiesWorkspaceOutcome(t *testing.T) {
	tests := []struct {
		name    string
		outcome domain.CleanupOutcome
		err     error
		want    string
	}{
		{name: "clean", outcome: domain.CleanupClean, err: nil, want: "clean"},
		{name: "landed", outcome: domain.CleanupLanded, err: nil, want: "landed"},
		{name: "dirty", err: errors.New("refusing to remove Git workspace with uncommitted or untracked changes"), want: "dirty"},
		{name: "committed", err: fmt.Errorf("refusing to remove Git workspace whose HEAD %s differs from recorded base commit %s", "abc", "def"), want: "committed"},
		{name: "unverifiable landing stays committed", err: fmt.Errorf("refusing to remove Git workspace whose HEAD %s differs from recorded base commit %s; merged landing could not be verified", "abc", "def"), want: "committed"},
		{name: "blocked", err: errors.New("refusing to remove workspace without durable ownership state"), want: "blocked"},
		// A refused cleanup never reports a removal outcome, even if one leaks in.
		{name: "landed outcome never masks a refusal", outcome: domain.CleanupLanded, err: errors.New("refusing to remove Git workspace with uncommitted or untracked changes"), want: "dirty"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := cleanupStatus(test.outcome, test.err); got != test.want {
				t.Fatalf("cleanupStatus=%q, want %q", got, test.want)
			}
		})
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

// syncBuffer is a concurrency-safe io.Writer/String() pair used to poll a log
// sink that a background coordinator goroutine is still writing to.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}
func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func waitForSubstring(t *testing.T, buf *syncBuffer, substr string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		output := buf.String()
		if strings.Contains(output, substr) {
			return output
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for log containing %q; got: %s", substr, output)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// waitForRunning blocks until identifier appears in c's running snapshot.
// agent.started only confirms Start was called; the launch goroutine still
// needs to record the session in c.running afterward, and a test that
// advances the clock and re-ticks before that happens will see reconcile
// find no running sessions at all, so a stall or eligibility change it
// expects to observe is silently missed and any following blocking receive
// (e.g. <-ws.after) hangs until the test binary's own timeout.
func waitForRunning(t *testing.T, c *Coordinator, identifier string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		for _, r := range c.Snapshot().Running {
			if r.IssueIdentifier == identifier {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("issue %s never appeared in the running snapshot", identifier)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestRenderExplainsHostAndManualDeliveryModes(t *testing.T) {
	settings := config.Settings{Prompt: "Work on {{.issue.identifier}}"}
	issue := domain.Issue{Identifier: "PMR-40"}
	manual, err := render(settings, issue, 0, config.DefaultAgentBackend)
	if err != nil || !strings.Contains(manual, "Delivery mode: manual") || !strings.Contains(manual, "Do not run gh, git push") {
		t.Fatalf("manual prompt=%q err=%v", manual, err)
	}
	settings.GitHub.Enabled = true
	settings.Tracker.HandoffState = "In Review"
	host, err := render(settings, issue, 0, config.DefaultAgentBackend)
	if err != nil || !strings.Contains(host, "Delivery mode: host-side publish") || !strings.Contains(host, "github_publish_pr with why, what_changed, and on_call") || !strings.Contains(host, "github_pr_context") {
		t.Fatalf("host prompt=%q err=%v", host, err)
	}
	// The same settings under the MCP-framed backend must name the tools the CLI
	// will actually serve. render is the only caller of DeliveryInstructions, so
	// this is where a dropped backend argument becomes observable: it would leave
	// the prompt naming Codex tool names for a Claude session.
	claude, err := render(settings, issue, 0, config.ClaudeAgentBackend)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(claude, config.MCPToolPrefix+"github_publish_pr with why, what_changed, and on_call") {
		t.Fatalf("claude prompt did not name the MCP publish tool: %q", claude)
	}
	if strings.Count(claude, "github_pr_context") != strings.Count(claude, config.MCPToolPrefix+"github_pr_context") {
		t.Fatalf("claude prompt named a bare tool: %q", claude)
	}
	if !strings.HasPrefix(claude, "Work on PMR-40\n\n") {
		t.Fatalf("host guidance displaced the repository prompt: %q", claude)
	}
}

// TestADispatchedPromptNamesTheToolsItsOwnBackendWillServe is the assertion the
// unit-level render tests cannot make. They call render with a backend a test
// chose; only a real dispatch shows which backend the call site passes, and the
// failure it catches is invisible everywhere else -- a prompt naming Codex tool
// names for a Claude session is a valid prompt, and the session it starts passes
// every launch check it has.
func TestADispatchedPromptNamesTheToolsItsOwnBackendWillServe(t *testing.T) {
	d := t.TempDir()
	path := filepath.Join(d, "WORKFLOW.md")
	body := "---\n" +
		"tracker: {kind: linear, provider: {api_key: k, handoff_state: In Review}, active_states: [Todo], terminal_states: [Done]}\n" +
		"github: {owner: pmrrasmussen, repository: symphony, token: $PMR52_COORDINATOR_TOKEN}\n" +
		"agent: {backend: claude, max_concurrent_agents: 1, max_turns: 1}\n" +
		"workspace: {root: " + filepath.Join(d, "work") + "}\n" +
		// A body that names Symphony's tools bare, as this repository's own
		// WORKFLOW.md does. A dispatch has to survive that: the mapping rule is
		// what makes it safe, and a launch guard that refused any bare mention
		// would refuse every real run.
		"---\nWork on {{.issue.identifier}}. Call github_publish_pr when clean and read github_pr_context."
	t.Setenv("PMR52_COORDINATOR_TOKEN", "github-secret")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := config.Load(path, "")
	if err != nil {
		t.Fatal(err)
	}
	if !w.Config.HostSidePublishPromised() || w.Config.Agent.Backend != config.ClaudeAgentBackend {
		t.Fatalf("fixture does not exercise the claude host-publish path: %+v", w.Config.Agent)
	}
	agent := &fakeAgent{events: completedEvents}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{})}
	c := New(&fakeTracker{issue: testIssue()}, agent, ws, func() config.Settings { return w.Config }, nil)
	c.Tick(context.Background())
	<-ws.after
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	requests := agent.requests()
	if len(requests) != 1 {
		t.Fatalf("dispatched %d requests, want 1", len(requests))
	}
	r := requests[0]
	if r.Backend != config.ClaudeAgentBackend {
		t.Fatalf("dispatched backend=%q", r.Backend)
	}
	if !strings.Contains(r.Prompt, config.MCPToolPrefix+"github_publish_pr") {
		t.Fatalf("the dispatched prompt named no MCP publish tool: %q", r.Prompt)
	}
	// The repository body's bare names survive verbatim, and the rule that maps
	// them travels with them. Both halves matter: the first is why WORKFLOW.md
	// needs no per-backend wording, the second is what the launch guard checks.
	if !strings.Contains(r.Prompt, "Call github_publish_pr when clean") {
		t.Fatalf("the repository body was rewritten rather than mapped: %q", r.Prompt)
	}
	if !strings.Contains(r.Prompt, config.MCPNamingRuleMarker) {
		t.Fatalf("the dispatched prompt names tools bare with no mapping rule: %q", r.Prompt)
	}
}

// TestContinuationGuidanceIsBackendNeutral covers the belt-and-braces half. The
// only assertion the existing continuation test makes about this text is that the
// coordinator sends what this function returns, which is true of any wording at
// all.
func TestContinuationGuidanceIsBackendNeutral(t *testing.T) {
	guidance := continuationGuidance(2, 3)
	// Both backends' vocabulary is banned, and the second half is the half that
	// was missing: this text is shared, so MCP wording leaks into every Codex
	// continuation turn, where there is no prefix to speak of. That is the same
	// mistake this change fixes, pointed the other way.
	for _, forbidden := range []string{"Codex", "codex", "workpad", "thread", "mcp", "MCP", "prefix"} {
		if strings.Contains(guidance, forbidden) {
			t.Fatalf("continuation guidance names %q, which is one backend's vocabulary: %q", forbidden, guidance)
		}
	}
	if !strings.Contains(guidance, "continuation turn #2 of 3") {
		t.Fatalf("continuation guidance lost its turn counter: %q", guidance)
	}
}

func TestBlankRequiredLabelFailsClosed(t *testing.T) {
	issue := domain.Issue{Dispatchable: true, Labels: []string{"ready"}}
	settings := config.Settings{Tracker: config.Tracker{RequiredLabels: []string{"ready", ""}}}
	if routable(issue, settings) {
		t.Fatal("blank required label allowed an issue to be routed")
	}
}

func TestSnapshotCopiesOnlySafeOperationalMetadata(t *testing.T) {
	c := New(&fakeTracker{}, &fakeAgent{}, &fakeWorkspace{}, func() config.Settings { return config.Settings{} }, nil)
	now := time.Now()
	c.claimed["provider-id"] = true
	c.stopping = true
	c.running["provider-id"] = &running{issue: domain.Issue{ID: "provider-id", Identifier: "PMR-6", State: "In Progress", Description: "must-not-appear"}, session: domain.AgentSession{ID: "session", ThreadID: "thread", TurnID: "turn"}, last: now, run: domain.Run{Attempt: 2, TurnCount: 1, StartedAt: now, Usage: domain.Usage{InputTokens: 1}}, rateLimit: map[string]int64{"remaining": 2}, outstanding: &outstandingOp{ItemID: "must-not-appear", ItemType: "dynamicToolCall", ToolName: "github_publish_pr", Since: now.Add(-time.Second)}}
	c.retries["retry-id"] = retryState{issue: domain.Issue{ID: "retry-id", Identifier: "PMR-9", Description: "must-not-appear"}, attempt: 3, kind: retryAgent, reason: "agent_event", due: now}
	snapshot := c.Snapshot()
	if snapshot.Claimed != 1 || !snapshot.Stopping || len(snapshot.Running) != 1 || len(snapshot.Retrying) != 1 {
		t.Fatalf("snapshot=%+v", snapshot)
	}
	if snapshot.Running[0].IssueIdentifier != "PMR-6" || snapshot.Running[0].IssueState != "In Progress" || snapshot.Running[0].RateLimit["remaining"] != 2 || snapshot.Running[0].OutstandingOperation == nil || snapshot.Running[0].OutstandingOperation.Type != "dynamicToolCall" || snapshot.Running[0].OutstandingOperation.Name != "github_publish_pr" || snapshot.Retrying[0].IssueIdentifier != "PMR-9" {
		t.Fatalf("snapshot=%+v", snapshot)
	}
	snapshot.Running[0].RateLimit["remaining"] = 99
	if c.running["provider-id"].rateLimit["remaining"] != 2 {
		t.Fatal("snapshot mutated live coordinator state")
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	for _, prohibited := range []string{"must-not-appear", "item_id"} {
		if strings.Contains(string(encoded), prohibited) {
			t.Fatalf("snapshot leaked %q: %s", prohibited, encoded)
		}
	}
}

func TestSortIssuesUsesTotalDeterministicOrder(t *testing.T) {
	older := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	newer := older.Add(time.Hour)
	tests := []struct {
		name      string
		createdAt map[string]*time.Time
		want      []string
	}{
		{name: "both nil", createdAt: map[string]*time.Time{}, want: []string{"PMR-1", "PMR-2"}},
		{name: "one nil", createdAt: map[string]*time.Time{"PMR-2": &older}, want: []string{"PMR-2", "PMR-1"}},
		{name: "equal", createdAt: map[string]*time.Time{"PMR-1": &older, "PMR-2": &older}, want: []string{"PMR-1", "PMR-2"}},
		{name: "distinct", createdAt: map[string]*time.Time{"PMR-1": &newer, "PMR-2": &older}, want: []string{"PMR-2", "PMR-1"}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for _, input := range [][]string{{"PMR-1", "PMR-2"}, {"PMR-2", "PMR-1"}} {
				issues := []domain.Issue{
					{Identifier: input[0], CreatedAt: test.createdAt[input[0]]},
					{Identifier: input[1], CreatedAt: test.createdAt[input[1]]},
				}

				sortIssues(issues)

				got := []string{issues[0].Identifier, issues[1].Identifier}
				if !reflect.DeepEqual(got, test.want) {
					t.Fatalf("sortIssues(%v) = %v, want %v", input, got, test.want)
				}
			}
		})
	}
}

type fakeTracker struct {
	mu            sync.Mutex
	issue         domain.Issue
	fresh         domain.Issue
	hasFresh      bool
	gets          int
	getErr        error
	transitions   []trackerTransition
	transitionErr error
}

// trackerTransition records one host-side dispatch transition request so tests
// can assert the coordinator moved an issue into its started state (or did not).
type trackerTransition struct{ id, from, to string }

func (f *fakeTracker) ListCandidates(context.Context, []string) ([]domain.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return []domain.Issue{f.issue}, nil
}
func (f *fakeTracker) GetIssues(context.Context, []string) ([]domain.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets++
	if f.getErr != nil {
		return nil, f.getErr
	}
	if f.hasFresh {
		return []domain.Issue{f.fresh}, nil
	}
	return []domain.Issue{f.issue}, nil
}

func (f *fakeTracker) getCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gets
}
func (f *fakeTracker) ListTerminal(context.Context, []string) ([]domain.Issue, error) {
	return nil, nil
}
func (f *fakeTracker) Transition(_ context.Context, issue domain.Issue, toState string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.transitions = append(f.transitions, trackerTransition{id: issue.ID, from: issue.State, to: toState})
	return f.transitionErr
}
func (f *fakeTracker) transitionCalls() []trackerTransition {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]trackerTransition(nil), f.transitions...)
}
func (f *fakeTracker) setIssue(issue domain.Issue) {
	f.mu.Lock()
	f.issue = issue
	f.mu.Unlock()
}
func (f *fakeTracker) setFresh(issue domain.Issue) {
	f.mu.Lock()
	f.fresh = issue
	f.hasFresh = true
	f.mu.Unlock()
}

// issueMapTracker makes capacity and reconciliation tests deterministic when
// more than one issue is eligible at the same time.
type issueMapTracker struct {
	mu         sync.Mutex
	candidates []domain.Issue
	issues     map[string]domain.Issue
	getErr     error
}

func (t *issueMapTracker) ListCandidates(context.Context, []string) ([]domain.Issue, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]domain.Issue(nil), t.candidates...), nil
}

func (t *issueMapTracker) GetIssues(_ context.Context, ids []string) ([]domain.Issue, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.getErr != nil {
		return nil, t.getErr
	}
	issues := make([]domain.Issue, 0, len(ids))
	for _, id := range ids {
		if issue, ok := t.issues[id]; ok {
			issues = append(issues, issue)
		}
	}
	return issues, nil
}

func (*issueMapTracker) ListTerminal(context.Context, []string) ([]domain.Issue, error) {
	return nil, nil
}

func (*issueMapTracker) Transition(context.Context, domain.Issue, string) error {
	return nil
}

func (t *issueMapTracker) setIssue(issue domain.Issue) {
	t.mu.Lock()
	t.issues[issue.ID] = issue
	for index, candidate := range t.candidates {
		if candidate.ID == issue.ID {
			t.candidates[index] = issue
		}
	}
	t.mu.Unlock()
}

type fakeAgent struct {
	mu                 sync.Mutex
	starts             int
	continues          int
	cancels            int
	started            chan struct{}
	events             func() <-chan domain.Event
	continuationEvents []func() <-chan domain.Event
	// startErr models a boundary that fails identically on every dispatch --
	// an unreachable agent binary is the canonical one -- so a test can drive
	// the retry ladder to its ceiling without any per-attempt bookkeeping.
	startErr         error
	continueErr      error
	continueSessions []domain.AgentSession
	continuePrompts  []string
	onContinue       func(int)
	// startRequests is every request the coordinator dispatched. The prompt and
	// the backend on it are one decision made at the call site, and this is the
	// only place a test can see the pair the router was actually handed.
	startRequests []domain.AgentRequest
}

func (f *fakeAgent) Start(_ context.Context, r domain.AgentRequest) (domain.AgentSession, <-chan domain.Event, error) {
	f.mu.Lock()
	f.starts++
	f.startRequests = append(f.startRequests, r)
	started := f.started
	err := f.startErr
	f.mu.Unlock()
	if started != nil {
		started <- struct{}{}
	}
	if err != nil {
		return domain.AgentSession{}, nil, err
	}
	return domain.AgentSession{ID: "t-u", ThreadID: "t", TurnID: "u"}, f.events(), nil
}
func (f *fakeAgent) Continue(_ context.Context, session domain.AgentSession, prompt string) (<-chan domain.Event, error) {
	f.mu.Lock()
	index := f.continues
	f.continues++
	f.continueSessions = append(f.continueSessions, session)
	f.continuePrompts = append(f.continuePrompts, prompt)
	events := f.continuationEvents
	err := f.continueErr
	onContinue := f.onContinue
	f.mu.Unlock()
	if onContinue != nil {
		onContinue(index)
	}
	if err != nil {
		return nil, err
	}
	if index >= len(events) {
		return closedEvents(), nil
	}
	return events[index](), nil
}
func (f *fakeAgent) Cancel(context.Context, domain.AgentSession) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancels++
	return nil
}
func (f *fakeAgent) counts() (starts, continues, cancels int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.starts, f.continues, f.cancels
}

func (f *fakeAgent) requests() []domain.AgentRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]domain.AgentRequest(nil), f.startRequests...)
}

func (f *fakeAgent) continuations() ([]domain.AgentSession, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]domain.AgentSession(nil), f.continueSessions...), append([]string(nil), f.continuePrompts...)
}

type fakeWorkspace struct {
	mu             sync.Mutex
	shouldRun      bool
	shouldRunCalls int
	prepares       int
	marks          int
	cleanups       int
	cleaned        chan struct{}
	marked         chan struct{}
	markErr        error
	after          chan struct{}
	prepareStarted chan struct{}
	prepareGate    <-chan struct{}
}

func (f *fakeWorkspace) Prepare(ctx context.Context, _ domain.Issue) (domain.Workspace, error) {
	f.mu.Lock()
	f.prepares++
	started := f.prepareStarted
	gate := f.prepareGate
	f.mu.Unlock()
	if started != nil {
		started <- struct{}{}
	}
	if gate != nil {
		select {
		case <-ctx.Done():
			return domain.Workspace{}, ctx.Err()
		case <-gate:
		}
	}
	return domain.Workspace{Path: "/tmp/work"}, nil
}
func (f *fakeWorkspace) BeforeRun(context.Context, domain.Workspace, domain.Issue) error { return nil }
func (f *fakeWorkspace) AfterRun(context.Context, domain.Workspace, domain.Issue) {
	if f.after != nil {
		f.after <- struct{}{}
	}
}
func (f *fakeWorkspace) Cleanup(context.Context, domain.Issue) (domain.CleanupOutcome, error) {
	f.mu.Lock()
	f.cleanups++
	cleaned := f.cleaned
	f.mu.Unlock()
	if cleaned != nil {
		cleaned <- struct{}{}
	}
	return domain.CleanupClean, nil
}
func (f *fakeWorkspace) Execute(context.Context, domain.Workspace, string, []string) ([]byte, error) {
	return nil, nil
}
func (f *fakeWorkspace) counts() (prepares, marks, cleanups, checks int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.prepares, f.marks, f.cleanups, f.shouldRunCalls
}

type fakeTimer struct {
	mu      sync.Mutex
	delays  []time.Duration
	entries []fakeTimerEntry
	signal  chan struct{}
}
type fakeTimerEntry struct {
	callback func()
	stopped  bool
}
type fakeTimerHandle struct {
	timer *fakeTimer
	index int
}

func (f *fakeTimer) AfterFunc(d time.Duration, callback func()) TimerHandle {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.delays = append(f.delays, d)
	f.entries = append(f.entries, fakeTimerEntry{callback: callback})
	if f.signal != nil {
		f.signal <- struct{}{}
	}
	return fakeTimerHandle{timer: f, index: len(f.entries) - 1}
}
func (h fakeTimerHandle) Stop() bool {
	h.timer.mu.Lock()
	defer h.timer.mu.Unlock()
	if h.index >= len(h.timer.entries) || h.timer.entries[h.index].stopped {
		return false
	}
	h.timer.entries[h.index].stopped = true
	return true
}
func (f *fakeTimer) fire(index int) {
	f.mu.Lock()
	entry := f.entries[index]
	f.mu.Unlock()
	if !entry.stopped {
		entry.callback()
	}
}

// fireStale simulates a callback that was already queued when Stop raced with
// it. The coordinator must still reject it by generation.
func (f *fakeTimer) fireStale(index int) {
	f.mu.Lock()
	callback := f.entries[index].callback
	f.mu.Unlock()
	callback()
}
func (f *fakeTimer) scheduled() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.entries)
}

type fakeClock struct{ now time.Time }

func (f fakeClock) Now() time.Time { return f.now }

type mutableClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *mutableClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *mutableClock) set(now time.Time) {
	c.mu.Lock()
	c.now = now
	c.mu.Unlock()
}

type retryPollError struct{ delay time.Duration }

func (e retryPollError) Error() string             { return "rate limited" }
func (e retryPollError) RetryDelay() time.Duration { return e.delay }

type rateLimitPollTracker struct {
	mu    sync.Mutex
	calls int
}

func (t *rateLimitPollTracker) ListCandidates(context.Context, []string) ([]domain.Issue, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.calls++
	if t.calls == 1 {
		return nil, retryPollError{delay: 2 * time.Minute}
	}
	return nil, nil
}
func (*rateLimitPollTracker) GetIssues(context.Context, []string) ([]domain.Issue, error) {
	return nil, nil
}
func (*rateLimitPollTracker) ListTerminal(context.Context, []string) ([]domain.Issue, error) {
	return nil, nil
}
func (*rateLimitPollTracker) Transition(context.Context, domain.Issue, string) error {
	return nil
}

func TestStartSchedulesRateLimitRecoveryWithInjectedTimer(t *testing.T) {
	w := testSettings(t)
	w.Config.Polling.Interval = 30 * time.Second
	tracker := &rateLimitPollTracker{}
	c := testCoordinator(w.Config, tracker, &fakeAgent{}, &fakeWorkspace{})
	timer := &fakeTimer{signal: make(chan struct{}, 2)}
	c.timer = timer
	ctx, cancel := context.WithCancel(context.Background())
	c.Start(ctx)
	<-timer.signal
	timer.mu.Lock()
	firstDelay := timer.delays[0]
	timer.mu.Unlock()
	if firstDelay != 2*time.Minute {
		t.Fatalf("rate-limit delay=%v want 2m", firstDelay)
	}
	timer.fire(0)
	<-timer.signal
	timer.mu.Lock()
	secondDelay := timer.delays[1]
	timer.mu.Unlock()
	if secondDelay != 30*time.Second {
		t.Fatalf("recovery delay=%v want 30s", secondDelay)
	}
	cancel()
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestActiveIssueAtTurnLimitIsBlockedAndRetriedWithoutCompletionMarker(t *testing.T) {
	w := testSettings(t)
	issue := testIssue()
	agent := &fakeAgent{events: completedEvents}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{}, 1)}
	c := testCoordinator(w.Config, &fakeTracker{issue: issue}, agent, ws)
	timer := &fakeTimer{signal: make(chan struct{}, 1)}
	c.timer = timer

	c.Tick(context.Background())
	<-ws.after
	<-timer.signal

	starts, continues, cancels := agent.counts()
	if starts != 1 || continues != 0 || cancels != 1 {
		t.Fatalf("starts=%d continues=%d cancels=%d", starts, continues, cancels)
	}
	_, marks, _, _ := ws.counts()
	if marks != 0 {
		t.Fatalf("completion markers=%d, want 0 for an active exhausted issue", marks)
	}
	c.mu.Lock()
	retry := c.retries[issue.ID]
	c.mu.Unlock()
	if retry.kind != retryAgent || retry.reason != "turn_limit_exhausted" || retry.attempt != 1 {
		t.Fatalf("retry=%+v", retry)
	}
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestLandingWaitEndsRunWithoutTurnsAndSchedulesBoundedLandingRetry reproduces
// the PMR-77 defect: a non-terminal landing wait must end the run at once
// instead of spending the remaining turns on repeated landing calls and then a
// turn-limit agent retry. The run instead ends as a wait, releases its
// orchestrator slot, keeps the duplicate-prevention claim, and schedules one
// delayed landing retry at the configured GitHub poll interval.
func TestLandingWaitEndsRunWithoutTurnsAndSchedulesBoundedLandingRetry(t *testing.T) {
	w := testSettings(t)
	w.Config.Agent.MaxTurns = 20
	w.Config.Agent.MaxRetryBackoff = 10 * time.Minute
	w.Config.GitHub.PollInterval = 30 * time.Second
	issue := testIssue()
	var log syncBuffer
	agent := &fakeAgent{events: landingWaitingEvents}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{}, 1)}
	c := New(&fakeTracker{issue: issue}, agent, ws, func() config.Settings { return w.Config }, slog.New(slog.NewJSONHandler(&log, nil)))
	c.clock = fakeClock{now: time.Date(2026, 8, 25, 9, 41, 0, 0, time.UTC)}
	timer := &fakeTimer{signal: make(chan struct{}, 1)}
	c.timer = timer

	c.Tick(context.Background())
	<-ws.after
	<-timer.signal

	starts, continues, cancels := agent.counts()
	if starts != 1 || continues != 0 || cancels != 1 {
		t.Fatalf("starts=%d continues=%d cancels=%d, want one start, no continuation turn, one cancel", starts, continues, cancels)
	}
	c.mu.Lock()
	retry := c.retries[issue.ID]
	claimed, admitted, running := c.claimed[issue.ID], len(c.admitted), len(c.running)
	c.mu.Unlock()
	if retry.kind != retryLanding || retry.reason != "landing_waiting" || retry.attempt != 0 {
		t.Fatalf("retry=%+v, want an unescalated landing retry", retry)
	}
	if !claimed || admitted != 0 || running != 0 {
		t.Fatalf("claimed=%v admitted=%d running=%d, want the claim held with no slot occupied", claimed, admitted, running)
	}
	if timer.scheduled() != 1 || timer.delays[0] != 30*time.Second {
		t.Fatalf("landing retry delays=%v, want one github poll interval", timer.delays)
	}
	if _, marks, cleanups, _ := ws.counts(); marks != 0 || cleanups != 0 {
		t.Fatalf("landing wait marks=%d cleanups=%d, want neither completion nor cleanup", marks, cleanups)
	}
	for _, want := range []string{`"msg":"agent landing waiting"`, `"operation":"landing_waiting"`, `"reason":"required checks are pending"`, `"msg":"landing wait retry scheduled"`, `"wait_attempt":1`, `"status":"waiting"`, `"retry_kind":"landing"`} {
		waitForSubstring(t, &log, want, time.Second)
	}
	if records := log.String(); strings.Contains(records, "turn_limit_exhausted") || strings.Contains(records, `"msg":"agent run retry scheduled"`) {
		t.Fatalf("landing wait logged an agent failure: %s", records)
	}
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestLandingResolvedEndsRunWithoutAnotherTurn covers the PMR-77 duplicate
// terminal call: once landing merged the pull request and reconciled the issue,
// the run ends immediately — even when this tracker refresh still reports the
// issue active — so no later turn can call the landing tool again.
func TestLandingResolvedEndsRunWithoutAnotherTurn(t *testing.T) {
	w := testSettings(t)
	w.Config.Agent.MaxTurns = 20
	issue := testIssue()
	var log syncBuffer
	agent := &fakeAgent{events: landingResolvedEvents}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{}, 1)}
	c := New(&fakeTracker{issue: issue}, agent, ws, func() config.Settings { return w.Config }, slog.New(slog.NewJSONHandler(&log, nil)))
	c.clock = fakeClock{now: time.Date(2026, 8, 25, 9, 41, 0, 0, time.UTC)}
	timer := &fakeTimer{}
	c.timer = timer

	c.Tick(context.Background())
	<-ws.after
	waitForRelease(t, c, issue.ID)

	starts, continues, cancels := agent.counts()
	if starts != 1 || continues != 0 || cancels != 1 {
		t.Fatalf("starts=%d continues=%d cancels=%d, want one start, no continuation turn, one cancel", starts, continues, cancels)
	}
	if timer.scheduled() != 0 {
		t.Fatalf("terminal landing scheduled %d retries, want none", timer.scheduled())
	}
	if _, _, cleanups, _ := ws.counts(); cleanups != 1 {
		t.Fatalf("cleanups=%d, want the workspace released even though the refresh still reports the issue active", cleanups)
	}
	records := log.String()
	if !strings.Contains(records, `"msg":"agent landing resolved"`) || !strings.Contains(records, `"operation":"landing_resolved"`) {
		t.Fatalf("log missing the landing resolution record: %s", records)
	}
	if !strings.Contains(records, `"status":"succeeded"`) {
		t.Fatalf("terminal landing did not finish the run successfully: %s", records)
	}
}

// waitForRelease waits until a finished run has released its claim, which the
// launch goroutine does after the workspace after_run hook.
func waitForRelease(t *testing.T, c *Coordinator, id string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		c.mu.Lock()
		claimed, running := c.claimed[id], len(c.running)
		c.mu.Unlock()
		if !claimed && running == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("run for %s never released its claim (claimed=%v running=%d)", id, claimed, running)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestLandingWaitRedispatchesWithEscalatingDelay drives the mechanism itself:
// the delayed landing retry relaunches the same attempt in a fresh session, and
// a gate that stays unsettled backs off instead of respawning at a fixed
// cadence forever. Adapted from the PMR-78 review probe.
func TestLandingWaitRedispatchesWithEscalatingDelay(t *testing.T) {
	w := testSettings(t)
	w.Config.Agent.MaxTurns = 20
	w.Config.Agent.MaxRetryBackoff = 10 * time.Minute
	// A poll interval below the first backoff step so escalation is visible.
	w.Config.GitHub.PollInterval = 5 * time.Second
	issue := testIssue()
	var log syncBuffer
	agent := &fakeAgent{events: landingWaitingEvents}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{}, 4)}
	c := New(&fakeTracker{issue: issue}, agent, ws, func() config.Settings { return w.Config }, slog.New(slog.NewJSONHandler(&log, nil)))
	c.clock = fakeClock{now: time.Date(2026, 8, 25, 9, 41, 0, 0, time.UTC)}
	timer := &fakeTimer{signal: make(chan struct{}, 4)}
	c.timer = timer

	c.Tick(context.Background())
	<-ws.after
	<-timer.signal

	// Firing the landing timer must relaunch landing, not fail the issue.
	timer.fire(0)
	<-ws.after
	<-timer.signal

	starts, continues, cancels := agent.counts()
	if starts != 2 || continues != 0 || cancels != 2 {
		t.Fatalf("starts=%d continues=%d cancels=%d, want two dispatches, no continuation turn, one cancel each", starts, continues, cancels)
	}
	c.mu.Lock()
	retry, ok := c.retries[issue.ID]
	waits, claimed, admitted := c.landingWaits[issue.ID], c.claimed[issue.ID], len(c.admitted)
	c.mu.Unlock()
	if !ok || retry.kind != retryLanding || retry.reason != "landing_waiting" || retry.attempt != 0 {
		t.Fatalf("retry=%+v ok=%v, want a second landing retry on the same attempt", retry, ok)
	}
	if waits != 2 || !claimed || admitted != 0 {
		t.Fatalf("waits=%d claimed=%v admitted=%d", waits, claimed, admitted)
	}
	if len(timer.delays) != 2 || timer.delays[0] != 10*time.Second || timer.delays[1] != 20*time.Second {
		t.Fatalf("landing retry delays=%v, want an escalating sequence", timer.delays)
	}
	waitForSubstring(t, &log, `"wait_attempt":2`, time.Second)
	if records := log.String(); strings.Contains(records, "turn_limit_exhausted") {
		t.Fatalf("repeated landing waits escalated into an agent failure: %s", records)
	}
	if snapshot := c.Snapshot(); len(snapshot.Retrying) != 1 || snapshot.Retrying[0].WaitAttempt != 2 || snapshot.Retrying[0].Attempt != 0 {
		t.Fatalf("snapshot retrying=%+v, want wait_attempt 2 on attempt 0", snapshot.Retrying)
	}
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	leaked, leakedWaits := c.claimed[issue.ID], len(c.landingWaits)
	c.mu.Unlock()
	if leaked || leakedWaits != 0 {
		t.Fatalf("shutdown leaked claim=%v landing waits=%d", leaked, leakedWaits)
	}
}

// TestCapacityBlockedLandingRetryKeepsItsCadence covers the redispatch that
// loses the state's single landing slot: it stays a landing retry on the same
// attempt at the landing cadence, instead of becoming a faster failure-backoff
// retry that would poll GitHub harder than configured. Adapted from the PMR-78
// review probe.
func TestCapacityBlockedLandingRetryKeepsItsCadence(t *testing.T) {
	w := testSettings(t)
	w.Config.Agent.MaxTurns = 20
	w.Config.Agent.MaxConcurrent = 1
	w.Config.Agent.MaxRetryBackoff = 10 * time.Minute
	w.Config.GitHub.PollInterval = 30 * time.Second
	issue := testIssue()
	agent := &fakeAgent{events: landingWaitingEvents}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{}, 4)}
	c := testCoordinator(w.Config, &fakeTracker{issue: issue}, agent, ws)
	timer := &fakeTimer{signal: make(chan struct{}, 4)}
	c.timer = timer

	c.Tick(context.Background())
	<-ws.after
	<-timer.signal

	// Another issue takes the only orchestrator slot before the timer fires.
	c.mu.Lock()
	c.admitted["other"] = "todo"
	c.mu.Unlock()

	timer.fire(0)
	<-timer.signal

	c.mu.Lock()
	retry := c.retries[issue.ID]
	claimed := c.claimed[issue.ID]
	c.mu.Unlock()
	if !claimed {
		t.Fatal("capacity-blocked landing retry dropped its claim")
	}
	if retry.kind != retryLanding || retry.reason != "landing_slot_unavailable" || retry.attempt != 0 {
		t.Fatalf("retry=%+v, want a landing retry on the same attempt", retry)
	}
	if len(timer.delays) != 2 || timer.delays[1] != 30*time.Second {
		t.Fatalf("delays=%v, want the landing cadence rather than a failure backoff", timer.delays)
	}
	if starts, _, _ := agent.counts(); starts != 1 {
		t.Fatalf("starts=%d, want no dispatch while the slot is taken", starts)
	}
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestPermanentDispatchFailureStopsAtMaxAttempts covers the PMR-111 defect: a
// boundary that fails identically on every dispatch used to reschedule itself
// forever at the backoff ceiling, holding its claim and leaving nothing in the
// log but a warning that reads like progress. The ladder now stops at exactly
// agent.max_attempts dispatches, arms no further timer, drops the claim, and
// says so once at error level.
func TestPermanentDispatchFailureStopsAtMaxAttempts(t *testing.T) {
	w := testSettings(t)
	w.Config.Agent.MaxAttempts = 3
	w.Config.Agent.MaxRetryBackoff = time.Minute
	issue := testIssue()
	var log syncBuffer
	agent := &fakeAgent{startErr: errors.New("agent binary not found")}
	ws := &fakeWorkspace{after: make(chan struct{}, 4)}
	c := New(&fakeTracker{issue: issue}, agent, ws, func() config.Settings { return w.Config }, slog.New(slog.NewJSONHandler(&log, nil)))
	c.clock = fakeClock{now: time.Date(2026, 8, 26, 9, 41, 0, 0, time.UTC)}
	timer := &fakeTimer{signal: make(chan struct{}, 4)}
	c.timer = timer

	c.Tick(context.Background())
	<-ws.after
	<-timer.signal
	// Two armed retries, and no more: the third dispatch reaches the ceiling.
	timer.fire(0)
	<-ws.after
	<-timer.signal
	timer.fire(1)
	<-ws.after
	waitForRelease(t, c, issue.ID)

	if starts, _, _ := agent.counts(); starts != 3 {
		t.Fatalf("starts=%d, want exactly max_attempts dispatches", starts)
	}
	if timer.scheduled() != 2 {
		t.Fatalf("armed %d retries, want one fewer than max_attempts", timer.scheduled())
	}
	c.mu.Lock()
	claimed, retries, admitted := c.claimed[issue.ID], len(c.retries), len(c.admitted)
	c.mu.Unlock()
	if claimed || retries != 0 || admitted != 0 {
		t.Fatalf("claimed=%v retries=%d admitted=%d, want the abandoned dispatch to hold nothing", claimed, retries, admitted)
	}
	record := waitForSubstring(t, &log, `"msg":"dispatch abandoned after max attempts"`, time.Second)
	for _, want := range []string{`"level":"ERROR"`, `"operation":"dispatch_abandoned"`, `"issue_identifier":"ENG-1"`, `"reason":"session_start"`, `"attempt":3`, `"max_attempts":3`} {
		if !strings.Contains(record, want) {
			t.Fatalf("abandonment record missing %s: %s", want, record)
		}
	}
	records := log.String()
	if count := strings.Count(records, `"msg":"dispatch abandoned after max attempts"`); count != 1 {
		t.Fatalf("abandonment was logged %d times, want exactly one: %s", count, records)
	}
	// The two dispatches below the ceiling keep their ordinary retry warning,
	// so the abandonment is the only new record on this path.
	if count := strings.Count(records, `"msg":"agent run retry scheduled"`); count != 2 {
		t.Fatalf("retry warnings=%d, want one per dispatch below the ceiling: %s", count, records)
	}
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestUnclassifiedAgentEventNeverAbandonsIssue covers the correction from
// review round 3: agentFailureReason's fallback, "agent_event", means the
// coordinator does not know why a run ended -- most commonly, in practice, a
// Claude quota rejection reported as domain.EventFailed carrying model or
// provider text (PMR-131). That is not the deterministic, classified failure
// the ceiling was built for, so it must keep climbing the ordinary
// escalating backoff ladder without ever arming abandonment, however many
// times it repeats -- unlike workspace_prepare, before_run, prompt_render,
// and session_start, which are issue-attributable and still consume the
// ceiling. See systemicFailureReasons for the three PMR-115 added
// alongside it (stream_closed, issue_refresh, session_continue) and why none
// of them consumes the ceiling either.
func TestUnclassifiedAgentEventNeverAbandonsIssue(t *testing.T) {
	w := testSettings(t)
	w.Config.Agent.MaxAttempts = 2
	w.Config.Agent.MaxRetryBackoff = time.Minute
	issue := testIssue()
	var log syncBuffer
	agent := &fakeAgent{events: failedEvents("model reported a failure")}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{}, 8)}
	c := New(&fakeTracker{issue: issue}, agent, ws, func() config.Settings { return w.Config }, slog.New(slog.NewJSONHandler(&log, nil)))
	c.clock = fakeClock{now: time.Date(2026, 8, 26, 9, 41, 0, 0, time.UTC)}
	timer := &fakeTimer{signal: make(chan struct{}, 8)}
	c.timer = timer

	c.Tick(context.Background())
	<-ws.after
	<-timer.signal

	const repeats = 6 // well past max_attempts=2, which an unclassified cause must never consume
	for i := 0; i < repeats; i++ {
		timer.fire(i)
		<-ws.after
		<-timer.signal
	}

	if starts, _, _ := agent.counts(); starts != repeats+1 {
		t.Fatalf("starts=%d, want one dispatch per fire plus the initial one", starts)
	}
	c.mu.Lock()
	claimed := c.claimed[issue.ID]
	retry, stillRetrying := c.retries[issue.ID]
	c.mu.Unlock()
	if !claimed || !stillRetrying {
		t.Fatal("an unclassified agent_event abandoned the issue before any classified failure occurred")
	}
	if retry.reason != "agent_event" {
		t.Fatalf("reason=%q, want agent_event", retry.reason)
	}
	if retry.attempt <= w.Config.Agent.MaxAttempts {
		t.Fatalf("attempt=%d, want it to keep climbing the ordinary ladder past max_attempts", retry.attempt)
	}
	if strings.Contains(log.String(), `"msg":"dispatch abandoned after max attempts"`) {
		t.Fatalf("an unclassified agent_event armed an abandonment record: %s", log.String())
	}
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestClosedEventStreamNeverAbandonsIssue pins the review-round-4 decision in
// systemicFailureReasons for "stream_closed": by construction (see
// errStreamClosed) it is never a repository- or issue-specific outcome, only
// ever a host bug in the coordinator's own event plumbing, so -- like
// agent_event -- it must keep climbing the ordinary escalating backoff
// ladder without ever arming abandonment, however many times it repeats. It
// drives finishFailure directly, the same way TestRetryAtCapacityNeverAbandons
// drives scheduleRetry directly, rather than replaying a full dispatch for
// every repeat: the reason and the ceiling interaction are what is under
// test, not the dispatch machinery TestClosedEventStreamSchedulesDeterministicAgentRetry
// already covers for a single occurrence.
func TestClosedEventStreamNeverAbandonsIssue(t *testing.T) {
	w := testSettings(t)
	w.Config.Agent.MaxAttempts = 2
	issue := testIssue()
	var log syncBuffer
	c := New(&fakeTracker{issue: issue}, &fakeAgent{}, &fakeWorkspace{}, func() config.Settings { return w.Config }, slog.New(slog.NewJSONHandler(&log, nil)))
	c.timer = &fakeTimer{}
	if !c.claim(issue, w.Config) {
		t.Fatal("issue was not claimed")
	}

	attempt := 0
	const repeats = 6 // well past max_attempts=2, which a host-generated cause must never consume
	for i := 0; i < repeats; i++ {
		c.finishFailure(context.Background(), issue, attempt, agentFailureReason(errStreamClosed), errStreamClosed)
		c.mu.Lock()
		retry, stillRetrying := c.retries[issue.ID]
		c.mu.Unlock()
		if !stillRetrying {
			t.Fatalf("stream_closed abandoned the issue on repeat %d", i)
		}
		if retry.reason != "stream_closed" {
			t.Fatalf("reason=%q, want stream_closed", retry.reason)
		}
		attempt = retry.attempt
	}

	if attempt <= w.Config.Agent.MaxAttempts {
		t.Fatalf("attempt=%d, want it to keep climbing the ordinary ladder past max_attempts", attempt)
	}
	if strings.Contains(log.String(), `"msg":"dispatch abandoned after max attempts"`) {
		t.Fatalf("a closed event stream armed an abandonment record: %s", log.String())
	}
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestPostTurnRefreshFailureNeverAbandonsIssue is the ceiling-interaction
// test review round 4 asked for: PMR-115's confirmed-live failure mode -- a
// Linear timeout on the post-turn GetIssues refresh -- is this codebase's
// shared tracker infrastructure, not this issue, so repeating it past
// agent.max_attempts must keep retrying rather than abandon the issue (see
// systemicFailureReasons). It drives finishFailure directly rather than
// replaying a full dispatch (see TestClosedEventStreamNeverAbandonsIssue):
// a permanently failing tracker would also fail runRetry's own pre-dispatch
// refresh, reclassifying every retry after the first as "retry_refresh" (a
// separate, already-covered ceiling interaction -- TestRetryRefreshFailureNeverAbandonsIssue,
// PMR-142) instead of exercising "issue_refresh" a second time.
func TestPostTurnRefreshFailureNeverAbandonsIssue(t *testing.T) {
	w := testSettings(t)
	w.Config.Agent.MaxAttempts = 2
	issue := testIssue()
	var log syncBuffer
	c := New(&fakeTracker{issue: issue}, &fakeAgent{}, &fakeWorkspace{}, func() config.Settings { return w.Config }, slog.New(slog.NewJSONHandler(&log, nil)))
	c.timer = &fakeTimer{}
	if !c.claim(issue, w.Config) {
		t.Fatal("issue was not claimed")
	}

	refreshErr := issueRefreshError{err: errors.New("linear tracker_request: Linear request failed")}
	attempt := 0
	const repeats = 6 // well past max_attempts=2, which a Linear-infrastructure cause must never consume
	for i := 0; i < repeats; i++ {
		c.finishFailure(context.Background(), issue, attempt, agentFailureReason(refreshErr), refreshErr)
		c.mu.Lock()
		retry, stillRetrying := c.retries[issue.ID]
		c.mu.Unlock()
		if !stillRetrying {
			t.Fatalf("issue_refresh abandoned the issue on repeat %d", i)
		}
		if retry.reason != "issue_refresh" {
			t.Fatalf("reason=%q, want issue_refresh", retry.reason)
		}
		attempt = retry.attempt
	}

	if attempt <= w.Config.Agent.MaxAttempts {
		t.Fatalf("attempt=%d, want it to keep climbing the ordinary ladder past max_attempts", attempt)
	}
	if strings.Contains(log.String(), `"msg":"dispatch abandoned after max attempts"`) {
		t.Fatalf("issue_refresh armed an abandonment record: %s", log.String())
	}
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestContinuationFailureNeverAbandonsIssue pins the same decision for
// "session_continue": it is Symphony's own backend adapter (agent.Continue)
// failing to resume a session, so a broken `claude` binary or lapsed backend
// auth would fail every running issue's next turn identically -- the same
// account-wide shape as the quota rejection that motivates agent_event's
// exemption, just raised by Symphony's own code instead of the model's (see
// systemicFailureReasons).
func TestContinuationFailureNeverAbandonsIssue(t *testing.T) {
	w := testSettings(t)
	w.Config.Agent.MaxAttempts = 2
	issue := testIssue()
	var log syncBuffer
	c := New(&fakeTracker{issue: issue}, &fakeAgent{}, &fakeWorkspace{}, func() config.Settings { return w.Config }, slog.New(slog.NewJSONHandler(&log, nil)))
	c.timer = &fakeTimer{}
	if !c.claim(issue, w.Config) {
		t.Fatal("issue was not claimed")
	}

	continueErr := sessionContinueError{err: errors.New("continuation unavailable")}
	attempt := 0
	const repeats = 6 // well past max_attempts=2, which a backend-adapter cause must never consume
	for i := 0; i < repeats; i++ {
		c.finishFailure(context.Background(), issue, attempt, agentFailureReason(continueErr), continueErr)
		c.mu.Lock()
		retry, stillRetrying := c.retries[issue.ID]
		c.mu.Unlock()
		if !stillRetrying {
			t.Fatalf("session_continue abandoned the issue on repeat %d", i)
		}
		if retry.reason != "session_continue" {
			t.Fatalf("reason=%q, want session_continue", retry.reason)
		}
		attempt = retry.attempt
	}

	if attempt <= w.Config.Agent.MaxAttempts {
		t.Fatalf("attempt=%d, want it to keep climbing the ordinary ladder past max_attempts", attempt)
	}
	if strings.Contains(log.String(), `"msg":"dispatch abandoned after max attempts"`) {
		t.Fatalf("session_continue armed an abandonment record: %s", log.String())
	}
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestLandingWaitRedispatchesPastMaxAttempts pins the exemption the ceiling
// must not swallow: a non-terminal landing wait is not an agent failure, so it
// keeps its unbounded redispatch and its unescalated attempt even after more
// dispatches than agent.max_attempts. Bounding it here would give up on a
// pull request whose checks are merely slow.
func TestLandingWaitRedispatchesPastMaxAttempts(t *testing.T) {
	w := testSettings(t)
	w.Config.Agent.MaxTurns = 20
	w.Config.Agent.MaxAttempts = 2
	w.Config.Agent.MaxRetryBackoff = 10 * time.Minute
	w.Config.GitHub.PollInterval = 30 * time.Second
	issue := testIssue()
	var log syncBuffer
	agent := &fakeAgent{events: landingWaitingEvents}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{}, 4)}
	c := New(&fakeTracker{issue: issue}, agent, ws, func() config.Settings { return w.Config }, slog.New(slog.NewJSONHandler(&log, nil)))
	c.clock = fakeClock{now: time.Date(2026, 8, 26, 9, 41, 0, 0, time.UTC)}
	timer := &fakeTimer{signal: make(chan struct{}, 4)}
	c.timer = timer

	c.Tick(context.Background())
	<-ws.after
	<-timer.signal
	for fired := 0; fired < 2; fired++ {
		timer.fire(fired)
		<-ws.after
		<-timer.signal
	}

	if starts, _, _ := agent.counts(); starts != 3 {
		t.Fatalf("starts=%d, want landing to keep redispatching past max_attempts", starts)
	}
	c.mu.Lock()
	retry, ok := c.retries[issue.ID]
	claimed, waits := c.claimed[issue.ID], c.landingWaits[issue.ID]
	c.mu.Unlock()
	if !ok || retry.kind != retryLanding || retry.attempt != 0 {
		t.Fatalf("retry=%+v ok=%v, want a further landing retry on the same attempt", retry, ok)
	}
	if !claimed || waits != 3 {
		t.Fatalf("claimed=%v wait_attempt=%d, want the claim held and only the wait count climbing", claimed, waits)
	}
	if records := log.String(); strings.Contains(records, "dispatch_abandoned") {
		t.Fatalf("a landing wait was abandoned at the agent ceiling: %s", records)
	}
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestLandingRetryDelayFloorsEscalatesAndCaps(t *testing.T) {
	settings := config.Settings{
		Polling: config.Polling{Interval: time.Minute},
		Agent:   config.Agent{MaxRetryBackoff: 90 * time.Second},
		GitHub:  config.GitHub{PollInterval: 30 * time.Second},
	}
	for _, test := range []struct {
		name     string
		settings config.Settings
		waits    int
		want     time.Duration
	}{
		{name: "first wait uses the github poll floor", settings: settings, waits: 1, want: 30 * time.Second},
		{name: "escalates past the floor", settings: settings, waits: 3, want: 40 * time.Second},
		{name: "capped by the retry ceiling", settings: settings, waits: 9, want: 90 * time.Second},
		{
			name:     "the poll floor is never undercut by a small ceiling",
			settings: config.Settings{Agent: config.Agent{MaxRetryBackoff: 5 * time.Second}, GitHub: config.GitHub{PollInterval: 30 * time.Second}},
			waits:    4,
			want:     30 * time.Second,
		},
		{
			name:     "falls back to the tracker poll interval",
			settings: config.Settings{Polling: config.Polling{Interval: 2 * time.Minute}, Agent: config.Agent{MaxRetryBackoff: 10 * time.Minute}},
			waits:    1,
			want:     2 * time.Minute,
		},
		{name: "unconfigured intervals use the documented default", settings: config.Settings{}, waits: 1, want: 30 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := landingRetryDelay(test.settings, test.waits); got != test.want {
				t.Fatalf("landingRetryDelay(waits=%d)=%s, want %s", test.waits, got, test.want)
			}
		})
	}
}

func landingWaitingEvents() <-chan domain.Event {
	ch := make(chan domain.Event, 2)
	ch <- domain.Event{Kind: domain.EventItem, At: time.Now(), ItemID: "1", ItemType: "dynamicToolCall", ToolName: "github_land_pr", Outcome: domain.ItemCompleted}
	ch <- domain.Event{Kind: domain.EventLandingWaiting, At: time.Now(), SessionID: "t-u", Message: "required checks are pending"}
	close(ch)
	return ch
}

func landingResolvedEvents() <-chan domain.Event {
	ch := make(chan domain.Event, 2)
	ch <- domain.Event{Kind: domain.EventItem, At: time.Now(), ItemID: "1", ItemType: "dynamicToolCall", ToolName: "github_land_pr", Outcome: domain.ItemCompleted}
	ch <- domain.Event{Kind: domain.EventLandingResolved, At: time.Now(), SessionID: "t-u"}
	close(ch)
	return ch
}

func TestBoundedRunRefreshesAndContinuesSameSessionToExactMaxTurns(t *testing.T) {
	w := testSettings(t)
	w.Config.Agent.MaxTurns = 3
	issue := testIssue()
	tracker := &fakeTracker{issue: issue}
	observedRefreshes := make([]int, 0, 2)
	var observedMu sync.Mutex
	agent := &fakeAgent{
		events:             completedEvents,
		continuationEvents: []func() <-chan domain.Event{completedEvents, completedEvents},
		onContinue: func(_ int) {
			observedMu.Lock()
			observedRefreshes = append(observedRefreshes, tracker.getCount())
			observedMu.Unlock()
		},
	}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{}, 1)}
	c := testCoordinator(w.Config, tracker, agent, ws)
	timer := &fakeTimer{signal: make(chan struct{}, 3)}
	c.timer = timer

	c.Tick(context.Background())
	for index := 0; index < 2; index++ {
		<-timer.signal
		timer.fire(index)
	}
	<-ws.after
	<-timer.signal

	starts, continues, cancels := agent.counts()
	if starts != 1 || continues != 2 || cancels != 1 {
		t.Fatalf("starts=%d continues=%d cancels=%d", starts, continues, cancels)
	}
	sessions, prompts := agent.continuations()
	wantSession := (domain.AgentSession{ID: "t-u", ThreadID: "t", TurnID: "u"})
	if len(sessions) != 2 || sessions[0] != wantSession || sessions[1] != wantSession {
		t.Fatalf("continuation sessions=%+v, want same initial session twice", sessions)
	}
	for index, prompt := range prompts {
		want := continuationGuidance(index+2, 3)
		if prompt != want {
			t.Fatalf("continuation prompt %d=%q, want configured guidance %q", index+2, prompt, want)
		}
	}
	observedMu.Lock()
	refreshes := append([]int(nil), observedRefreshes...)
	observedMu.Unlock()
	if !reflect.DeepEqual(refreshes, []int{1, 2}) {
		t.Fatalf("tracker refresh counts at continuation=%v, want [1 2]", refreshes)
	}
	if tracker.getCount() != 3 {
		t.Fatalf("tracker refreshes=%d, want one after each turn", tracker.getCount())
	}
	_, marks, _, _ := ws.counts()
	if marks != 0 {
		t.Fatalf("completion markers=%d, want 0 for an active exhausted issue", marks)
	}
	c.mu.Lock()
	retry := c.retries[issue.ID]
	c.mu.Unlock()
	if retry.kind != retryAgent || retry.reason != "turn_limit_exhausted" || retry.attempt != 1 {
		t.Fatalf("retry=%+v", retry)
	}
}

func TestBlockedEventStopsContinuationAndLogsSafeRetryReason(t *testing.T) {
	w := testSettings(t)
	w.Config.Agent.MaxTurns = 3
	issue := testIssue()
	events := make(chan domain.Event, 1)
	events <- domain.Event{Kind: domain.EventBlocked, At: time.Now(), Message: "Codex GitHub publication request was rejected"}
	close(events)
	agent := &fakeAgent{events: func() <-chan domain.Event { return events }}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{}, 1)}
	var logs bytes.Buffer
	c := New(&fakeTracker{issue: issue}, agent, ws, func() config.Settings { return w.Config }, slog.New(slog.NewJSONHandler(&logs, nil)))
	c.clock = fakeClock{now: time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)}
	timer := &fakeTimer{signal: make(chan struct{}, 1)}
	c.timer = timer

	c.Tick(context.Background())
	<-ws.after
	<-timer.signal

	starts, continues, cancels := agent.counts()
	if starts != 1 || continues != 0 || cancels != 1 {
		t.Fatalf("starts=%d continues=%d cancels=%d", starts, continues, cancels)
	}
	c.mu.Lock()
	retry := c.retries[issue.ID]
	c.mu.Unlock()
	if retry.kind != retryAgent || retry.reason != "agent_blocked" || retry.attempt != 1 {
		t.Fatalf("retry=%+v", retry)
	}
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	output := logs.String()
	if !strings.Contains(output, `"blocker":"github_publication"`) {
		t.Fatalf("blocked retry did not identify its safe category: %s", output)
	}
	if strings.Contains(output, "Codex GitHub publication request was rejected") {
		t.Fatalf("blocked retry logged raw event text: %s", output)
	}
}

func TestBoundedRunStopsAtHandoffWithoutContinuationOrMarker(t *testing.T) {
	w := testSettings(t)
	w.Config.Agent.MaxTurns = 3
	w.Config.Tracker.HandoffState = "Review"
	issue := testIssue()
	handoff := issue
	handoff.State = "Review"
	handoff.Dispatchable = false
	tracker := &fakeTracker{issue: issue}
	tracker.setFresh(handoff)
	agent := &fakeAgent{events: completedEvents}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{}, 1)}
	c := testCoordinator(w.Config, tracker, agent, ws)
	timer := &fakeTimer{}
	c.timer = timer

	c.Tick(context.Background())
	<-ws.after

	starts, continues, cancels := agent.counts()
	if starts != 1 || continues != 0 || cancels != 1 {
		t.Fatalf("handoff starts=%d continues=%d cancels=%d", starts, continues, cancels)
	}
	_, marks, _, _ := ws.counts()
	if marks != 0 || timer.scheduled() != 0 {
		t.Fatalf("handoff markers=%d timers=%d, want neither", marks, timer.scheduled())
	}
}

func TestBoundedRunCancellationDuringContinuationDelayStopsCleanly(t *testing.T) {
	w := testSettings(t)
	w.Config.Agent.MaxTurns = 2
	issue := testIssue()
	agent := &fakeAgent{events: completedEvents, continuationEvents: []func() <-chan domain.Event{completedEvents}}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{}, 1)}
	c := testCoordinator(w.Config, &fakeTracker{issue: issue}, agent, ws)
	timer := &fakeTimer{signal: make(chan struct{}, 1)}
	c.timer = timer

	c.Tick(context.Background())
	<-timer.signal
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-ws.after

	starts, continues, cancels := agent.counts()
	if starts != 1 || continues != 0 || cancels != 1 {
		t.Fatalf("cancelled starts=%d continues=%d cancels=%d", starts, continues, cancels)
	}
	_, marks, _, _ := ws.counts()
	if marks != 0 {
		t.Fatalf("cancelled completion markers=%d, want 0", marks)
	}
}

func TestBoundedRunContinuationFailureStopsSessionAndUsesFailureRetry(t *testing.T) {
	w := testSettings(t)
	w.Config.Agent.MaxTurns = 2
	issue := testIssue()
	agent := &fakeAgent{events: completedEvents, continueErr: errors.New("continuation unavailable")}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{}, 1)}
	var log syncBuffer
	c := New(&fakeTracker{issue: issue}, agent, ws, func() config.Settings { return w.Config }, slog.New(slog.NewJSONHandler(&log, nil)))
	c.clock = fakeClock{now: time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)}
	timer := &fakeTimer{signal: make(chan struct{}, 2)}
	c.timer = timer

	c.Tick(context.Background())
	<-timer.signal
	timer.fire(0)
	<-ws.after
	<-timer.signal

	starts, continues, cancels := agent.counts()
	if starts != 1 || continues != 1 || cancels != 1 {
		t.Fatalf("failed continuation starts=%d continues=%d cancels=%d", starts, continues, cancels)
	}
	_, marks, _, _ := ws.counts()
	if marks != 0 {
		t.Fatalf("failed continuation markers=%d, want 0", marks)
	}
	c.mu.Lock()
	retry := c.retries[issue.ID]
	c.mu.Unlock()
	if retry.kind != retryAgent || retry.reason != "session_continue" || retry.attempt != 1 {
		t.Fatalf("failed continuation retry=%+v", retry)
	}
	if records := log.String(); !strings.Contains(records, "continuation unavailable") {
		t.Fatalf("failed continuation dropped the backend error: %s", records)
	}
}

func TestClosedEventStreamSchedulesDeterministicAgentRetry(t *testing.T) {
	w := testSettings(t)
	issue := testIssue()
	agent := &fakeAgent{events: closedEvents}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{}, 1)}
	c := testCoordinator(w.Config, &fakeTracker{issue: issue}, agent, ws)
	timer := &fakeTimer{signal: make(chan struct{}, 1)}
	c.timer = timer

	c.Tick(context.Background())
	<-timer.signal
	if timer.scheduled() != 1 || timer.delays[0] != 10*time.Second {
		t.Fatalf("retries=%v", timer.delays)
	}
	c.mu.Lock()
	retry := c.retries[issue.ID]
	c.mu.Unlock()
	if retry.kind != retryAgent || retry.reason != "stream_closed" || retry.attempt != 1 {
		t.Fatalf("retry=%+v", retry)
	}
}

// TestPostTurnRefreshFailureSchedulesDistinctAgentRetry pins the PMR-115 fix:
// a tracker error from runTurns' post-turn GetIssues -- confirmed live as a
// Linear request timeout following a turn the agent completed successfully
// -- is named "issue_refresh" rather than collapsing into "agent_event", and
// the underlying tracker error text is not discarded.
func TestPostTurnRefreshFailureSchedulesDistinctAgentRetry(t *testing.T) {
	w := testSettings(t)
	issue := testIssue()
	var log syncBuffer
	agent := &fakeAgent{events: completedEvents}
	tracker := &fakeTracker{issue: issue, getErr: errors.New("linear tracker_request: Linear request failed")}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{}, 1)}
	c := New(tracker, agent, ws, func() config.Settings { return w.Config }, slog.New(slog.NewJSONHandler(&log, nil)))
	timer := &fakeTimer{signal: make(chan struct{}, 1)}
	c.timer = timer

	c.Tick(context.Background())
	<-timer.signal

	c.mu.Lock()
	retry := c.retries[issue.ID]
	c.mu.Unlock()
	if retry.kind != retryAgent || retry.reason != "issue_refresh" || retry.attempt != 1 {
		t.Fatalf("retry=%+v", retry)
	}
	if records := log.String(); !strings.Contains(records, "linear tracker_request: Linear request failed") {
		t.Fatalf("post-turn refresh failure dropped the tracker error: %s", records)
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

func TestRetryRechecksCurrentIssueEligibility(t *testing.T) {
	w := testSettings(t)
	issue := testIssue()
	tracker := &fakeTracker{issue: issue}
	agent := &fakeAgent{events: closedEvents}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{}, 1)}
	c := testCoordinator(w.Config, tracker, agent, ws)
	timer := &fakeTimer{signal: make(chan struct{}, 1)}
	c.timer = timer

	c.Tick(context.Background())
	<-timer.signal
	terminal := issue
	terminal.State = "Done"
	terminal.Dispatchable = false
	tracker.setIssue(terminal)
	timer.fire(0)

	starts, _, _ := agent.counts()
	if starts != 1 {
		t.Fatalf("starts=%d, retry ran stale issue", starts)
	}
	_, _, cleanups, _ := ws.counts()
	if cleanups != 1 {
		t.Fatalf("terminal retry cleanups=%d, want 1", cleanups)
	}
}

func TestStoppedRetryCallbackCannotReclaimIssue(t *testing.T) {
	w := testSettings(t)
	issue := testIssue()
	agent := &fakeAgent{events: completedEvents}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{}, 1)}
	c := testCoordinator(w.Config, &fakeTracker{issue: issue}, agent, ws)
	timer := &fakeTimer{}
	c.timer = timer
	if !c.claim(issue, w.Config) {
		t.Fatal("claim failed")
	}
	c.scheduleRetry(context.Background(), issue, domain.Workspace{}, 1, retryAgent, "test", time.Second)
	c.release(issue.ID)
	timer.fireStale(0)

	starts, _, _ := agent.counts()
	if starts != 0 {
		t.Fatalf("stale callback started %d sessions", starts)
	}
}

func TestReconciliationCancellationDoesNotRetry(t *testing.T) {
	w := testSettings(t)
	issue := testIssue()
	tracker := &fakeTracker{issue: issue}
	block := make(chan domain.Event)
	agent := &fakeAgent{events: func() <-chan domain.Event { return block }, started: make(chan struct{}, 1)}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{}, 1)}
	c := testCoordinator(w.Config, tracker, agent, ws)
	timer := &fakeTimer{}
	c.timer = timer

	c.Tick(context.Background())
	<-agent.started
	waitForRunning(t, c, issue.Identifier)
	ineligible := issue
	ineligible.Dispatchable = false
	tracker.setIssue(ineligible)
	c.Tick(context.Background())
	<-ws.after

	if timer.scheduled() != 0 {
		t.Fatalf("reconciliation cancellation scheduled retries=%d", timer.scheduled())
	}
	starts, _, cancels := agent.counts()
	if starts != 1 || cancels != 1 {
		t.Fatalf("starts=%d cancels=%d", starts, cancels)
	}
}

func TestCompletedEventAfterReconciliationCancellationDoesNotComplete(t *testing.T) {
	w := testSettings(t)
	issue := testIssue()
	agent := &fakeAgent{events: completedEvents}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{}, 1)}
	c := testCoordinator(w.Config, &fakeTracker{issue: issue}, agent, ws)
	r := &running{issue: issue, stopped: stopTerminal}
	events := make(chan domain.Event, 1)
	events <- domain.Event{Kind: domain.EventCompleted, SessionID: "t-u"}
	close(events)

	completed, err := c.consume(context.Background(), r, events)
	if completed {
		t.Fatal("completed event after reconciliation cancellation was accepted")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v, want context cancellation", err)
	}
}

// stubForgetter records the issues the coordinator reported as finished. It
// stands in for the host GitHub manager's linked-pull-request table, which is
// the thing that would otherwise poll a Done issue for the life of the process
// (PMR-112).
type stubForgetter struct {
	mu        sync.Mutex
	forgotten []string
}

func (s *stubForgetter) Forget(issueID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.forgotten = append(s.forgotten, issueID)
}

func (s *stubForgetter) issues() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.forgotten...)
}

func TestTerminalIssueIsForgottenByTheHostIntegration(t *testing.T) {
	w := testSettings(t)
	issue := testIssue()
	tracker := &fakeTracker{issue: issue}
	terminal := issue
	terminal.State = "Done"
	terminal.Dispatchable = false
	tracker.setFresh(terminal)
	agent := &fakeAgent{events: completedEvents}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{}, 1), cleaned: make(chan struct{}, 1)}
	c := testCoordinator(w.Config, tracker, agent, ws)
	forgetter := &stubForgetter{}
	c.SetIssueForgetter(forgetter)

	c.Tick(context.Background())
	<-ws.cleaned
	if got := forgetter.issues(); len(got) != 1 || got[0] != issue.ID {
		t.Fatalf("terminal issue releases=%v, want exactly %q", got, issue.ID)
	}
}

// A run that ends with its issue still active must not release anything: the
// pull request that issue published is exactly the one the poll loop still has
// a merge to observe on.
func TestActiveIssueIsNotForgotten(t *testing.T) {
	w := testSettings(t)
	issue := testIssue()
	agent := &fakeAgent{events: completedEvents}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{}, 1)}
	c := testCoordinator(w.Config, &fakeTracker{issue: issue}, agent, ws)
	forgetter := &stubForgetter{}
	c.SetIssueForgetter(forgetter)

	c.Tick(context.Background())
	<-ws.after
	if got := forgetter.issues(); len(got) != 0 {
		t.Fatalf("still-active issue released=%v, want none", got)
	}
}

func TestCompletionRevalidatesTerminalIssueBeforeMarker(t *testing.T) {
	w := testSettings(t)
	issue := testIssue()
	tracker := &fakeTracker{issue: issue}
	terminal := issue
	terminal.State = "Done"
	terminal.Dispatchable = false
	tracker.setFresh(terminal)
	agent := &fakeAgent{events: completedEvents}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{}, 1), cleaned: make(chan struct{}, 1)}
	c := testCoordinator(w.Config, tracker, agent, ws)

	c.Tick(context.Background())
	<-ws.cleaned
	_, marks, cleanups, _ := ws.counts()
	if marks != 0 {
		t.Fatalf("completion marker writes=%d, want 0 for terminal issue", marks)
	}
	if cleanups != 1 {
		t.Fatalf("terminal workspace cleanups=%d, want 1", cleanups)
	}
}

func TestShutdownCancellationDoesNotRetry(t *testing.T) {
	w := testSettings(t)
	issue := testIssue()
	block := make(chan domain.Event)
	agent := &fakeAgent{events: func() <-chan domain.Event { return block }, started: make(chan struct{}, 1)}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{}, 1)}
	c := testCoordinator(w.Config, &fakeTracker{issue: issue}, agent, ws)
	timer := &fakeTimer{}
	c.timer = timer

	c.Tick(context.Background())
	<-agent.started
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-ws.after
	if timer.scheduled() != 0 {
		t.Fatalf("shutdown cancellation scheduled retries=%d", timer.scheduled())
	}
}

func TestClaimPreventsDuplicateConcurrentLaunches(t *testing.T) {
	w := testSettings(t)
	issue := testIssue()
	block := make(chan domain.Event)
	agent := &fakeAgent{events: func() <-chan domain.Event { return block }, started: make(chan struct{}, 1)}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{}, 1)}
	c := testCoordinator(w.Config, &fakeTracker{issue: issue}, agent, ws)

	c.Tick(context.Background())
	<-agent.started
	c.Tick(context.Background())
	starts, _, _ := agent.counts()
	if starts != 1 {
		t.Fatalf("starts=%d, want one owner", starts)
	}
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-ws.after
}

func TestQueuedRetryDoesNotConsumeAnOrchestratorSlot(t *testing.T) {
	w := testSettings(t)
	retrying := testIssue()
	retrying.ID, retrying.Identifier = "retrying", "ENG-2"
	ready := testIssue()
	ready.ID, ready.Identifier = "ready", "ENG-3"
	tracker := &issueMapTracker{candidates: []domain.Issue{ready}, issues: map[string]domain.Issue{retrying.ID: retrying, ready.ID: ready}}
	block := make(chan domain.Event)
	agent := &fakeAgent{events: func() <-chan domain.Event { return block }, started: make(chan struct{}, 1)}
	ws := &fakeWorkspace{after: make(chan struct{}, 1)}
	c := testCoordinator(w.Config, tracker, agent, ws)
	timer := &fakeTimer{}
	c.timer = timer

	if !c.claim(retrying, w.Config) {
		t.Fatal("retrying issue was not claimed")
	}
	c.scheduleRetry(context.Background(), retrying, domain.Workspace{}, 1, retryAgent, "test", time.Minute)
	c.Tick(context.Background())
	<-agent.started

	starts, _, _ := agent.counts()
	if starts != 1 {
		t.Fatalf("starts=%d, want unrelated ready issue to use the slot", starts)
	}
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-ws.after
}

func TestLaunchReservationPreventsOversubscriptionBeforeSessionStart(t *testing.T) {
	w := testSettings(t)
	first := testIssue()
	second := testIssue()
	second.ID, second.Identifier = "second", "ENG-2"
	gate := make(chan struct{})
	ws := &fakeWorkspace{prepareStarted: make(chan struct{}, 1), prepareGate: gate, after: make(chan struct{}, 1)}
	block := make(chan domain.Event)
	agent := &fakeAgent{events: func() <-chan domain.Event { return block }, started: make(chan struct{}, 1)}
	c := testCoordinator(w.Config, &fakeTracker{issue: first}, agent, ws)

	if !c.claim(first, w.Config) || !c.launch(context.Background(), first, 0) {
		t.Fatal("first launch was not admitted")
	}
	<-ws.prepareStarted
	if c.claim(second, w.Config) {
		t.Fatal("second issue claimed a slot while first preparation had reserved it")
	}
	close(gate)
	<-agent.started
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-ws.after
}

// TestRetryAtCapacityRequeuesOnFixedCadence covers runRetry's contended-slot
// branch for retryAgent: losing the race for an orchestrator slot is capacity
// contention, not a dispatch failure, so it must keep the attempt fixed and
// retry on agentSlotRetryDelay's fixed poll-interval cadence rather than
// attempt+1 and the escalating failure backoff.
func TestRetryAtCapacityRequeuesOnFixedCadence(t *testing.T) {
	w := testSettings(t)
	w.Config.Agent.MaxRetryBackoff = 15 * time.Second
	retrying := testIssue()
	retrying.ID, retrying.Identifier = "retrying", "ENG-2"
	running := testIssue()
	running.ID, running.Identifier = "running", "ENG-3"
	tracker := &issueMapTracker{issues: map[string]domain.Issue{retrying.ID: retrying, running.ID: running}}
	block := make(chan domain.Event)
	agent := &fakeAgent{events: func() <-chan domain.Event { return block }, started: make(chan struct{}, 1)}
	ws := &fakeWorkspace{after: make(chan struct{}, 1)}
	c := testCoordinator(w.Config, tracker, agent, ws)
	timer := &fakeTimer{}
	c.timer = timer

	if !c.claim(retrying, w.Config) {
		t.Fatal("retrying issue was not claimed")
	}
	c.scheduleRetry(context.Background(), retrying, domain.Workspace{}, 1, retryAgent, "test", time.Second)
	if !c.claim(running, w.Config) || !c.launch(context.Background(), running, 0) {
		t.Fatal("running issue was not admitted")
	}
	<-agent.started
	timer.fire(0)

	c.mu.Lock()
	retry := c.retries[retrying.ID]
	c.mu.Unlock()
	if retry.reason != "agent_slot_unavailable" || retry.attempt != 1 {
		t.Fatalf("retry=%+v, want attempt unchanged and reason agent_slot_unavailable", retry)
	}
	if len(timer.delays) != 2 || timer.delays[1] != w.Config.Polling.Interval {
		t.Fatalf("retry delays=%v, want second retry at the poll interval (not the 15s failure backoff)", timer.delays)
	}
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-ws.after
}

// TestRetryAtCapacityNeverAbandons pins the invariant review settled
// empirically on this repository: PMR-100 completed successfully on attempt
// 11 after eleven straight lost slot races, never once having failed a
// dispatch. A contended orchestrator slot must never consume
// agent.max_attempts, however many times the slot is lost, or a healthy but
// busy queue would abandon issues that were never broken.
func TestRetryAtCapacityNeverAbandons(t *testing.T) {
	w := testSettings(t)
	w.Config.Agent.MaxAttempts = 2
	w.Config.Agent.MaxRetryBackoff = 15 * time.Second
	retrying := testIssue()
	retrying.ID, retrying.Identifier = "retrying", "ENG-2"
	running := testIssue()
	running.ID, running.Identifier = "running", "ENG-3"
	tracker := &issueMapTracker{issues: map[string]domain.Issue{retrying.ID: retrying, running.ID: running}}
	block := make(chan domain.Event)
	agent := &fakeAgent{events: func() <-chan domain.Event { return block }, started: make(chan struct{}, 1)}
	ws := &fakeWorkspace{after: make(chan struct{}, 1)}
	var log syncBuffer
	c := New(tracker, agent, ws, func() config.Settings { return w.Config }, slog.New(slog.NewJSONHandler(&log, nil)))
	c.clock = fakeClock{now: time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)}
	timer := &fakeTimer{}
	c.timer = timer

	if !c.claim(retrying, w.Config) {
		t.Fatal("retrying issue was not claimed")
	}
	c.scheduleRetry(context.Background(), retrying, domain.Workspace{}, 1, retryAgent, "test", time.Second)
	if !c.claim(running, w.Config) || !c.launch(context.Background(), running, 0) {
		t.Fatal("running issue was not admitted")
	}
	<-agent.started

	const lostRaces = 5 // well past max_attempts=2, which a contended slot must never consume
	for i := 0; i < lostRaces; i++ {
		timer.fire(i)
	}

	c.mu.Lock()
	claimed := c.claimed[retrying.ID]
	retry, stillRetrying := c.retries[retrying.ID]
	c.mu.Unlock()
	if !claimed || !stillRetrying {
		t.Fatal("contended slot abandoned the retry before a real dispatch failure ever occurred")
	}
	if retry.attempt != 1 {
		t.Fatalf("attempt=%d, want unchanged across %d lost slot races", retry.attempt, lostRaces)
	}
	if retry.reason != "agent_slot_unavailable" {
		t.Fatalf("reason=%q, want agent_slot_unavailable", retry.reason)
	}
	if strings.Contains(log.String(), `"msg":"dispatch abandoned after max attempts"`) {
		t.Fatalf("contended slot armed an abandonment record: %s", log.String())
	}
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-ws.after
}

func TestRetryRefreshFailureIncrementsAttemptAndRetries(t *testing.T) {
	w := testSettings(t)
	w.Config.Agent.MaxRetryBackoff = 15 * time.Second
	issue := testIssue()
	tracker := &issueMapTracker{issues: map[string]domain.Issue{issue.ID: issue}, getErr: errors.New("temporary tracker failure")}
	c := testCoordinator(w.Config, tracker, &fakeAgent{}, &fakeWorkspace{})
	timer := &fakeTimer{}
	c.timer = timer

	if !c.claim(issue, w.Config) {
		t.Fatal("issue was not claimed")
	}
	c.scheduleRetry(context.Background(), issue, domain.Workspace{}, 1, retryAgent, "test", time.Second)
	timer.fire(0)

	c.mu.Lock()
	retry := c.retries[issue.ID]
	c.mu.Unlock()
	if retry.reason != "retry_refresh" || retry.attempt != 2 {
		t.Fatalf("retry=%+v", retry)
	}
	if len(timer.delays) != 2 || timer.delays[1] != 15*time.Second {
		t.Fatalf("retry delays=%v, want capped 15s refresh retry", timer.delays)
	}
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestRetryRefreshFailureNeverAbandonsIssue pins the PMR-142 correction:
// runRetry's pre-dispatch refresh wraps the same tracker.GetIssues failure
// as the post-turn refresh covered by TestPostTurnRefreshFailureNeverAbandonsIssue
// (see systemicFailureReasons), just observed at a different moment -- the
// moment an issue is waiting to redispatch rather than the moment one just
// finished a turn. A sustained Linear outage drives every retrying issue
// through exactly this site, so it must keep climbing the ordinary
// escalating backoff ladder past agent.max_attempts, the same as
// "issue_refresh", instead of abandoning the issue on infrastructure that
// says nothing about whether its work is workable.
func TestRetryRefreshFailureNeverAbandonsIssue(t *testing.T) {
	w := testSettings(t)
	w.Config.Agent.MaxAttempts = 2
	w.Config.Agent.MaxRetryBackoff = 15 * time.Second
	issue := testIssue()
	tracker := &issueMapTracker{issues: map[string]domain.Issue{issue.ID: issue}, getErr: errors.New("temporary tracker failure")}
	var log syncBuffer
	c := New(tracker, &fakeAgent{}, &fakeWorkspace{}, func() config.Settings { return w.Config }, slog.New(slog.NewJSONHandler(&log, nil)))
	c.clock = fakeClock{now: time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)}
	timer := &fakeTimer{}
	c.timer = timer

	if !c.claim(issue, w.Config) {
		t.Fatal("issue was not claimed")
	}
	c.scheduleRetry(context.Background(), issue, domain.Workspace{}, 1, retryAgent, "test", time.Second)

	const repeats = 6 // well past max_attempts=2, which a Linear-infrastructure cause must never consume
	for i := 0; i < repeats; i++ {
		timer.fire(i)
	}

	c.mu.Lock()
	claimed := c.claimed[issue.ID]
	retry, stillRetrying := c.retries[issue.ID]
	c.mu.Unlock()
	if !claimed || !stillRetrying {
		t.Fatal("retry_refresh abandoned the issue before any classified failure occurred")
	}
	if retry.reason != "retry_refresh" {
		t.Fatalf("reason=%q, want retry_refresh", retry.reason)
	}
	if retry.attempt <= w.Config.Agent.MaxAttempts {
		t.Fatalf("attempt=%d, want it to keep climbing the ordinary ladder past max_attempts", retry.attempt)
	}
	if strings.Contains(log.String(), `"msg":"dispatch abandoned after max attempts"`) {
		t.Fatalf("retry_refresh armed an abandonment record: %s", log.String())
	}
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestLandingRetryRefreshFailureIgnoresMaxAttempts pins the same exemption
// TestLandingWaitRedispatchesPastMaxAttempts pins for the wait itself: a
// landing retry that fails to refresh its issue is still not an agent
// failure, so it must keep redispatching past agent.max_attempts rather than
// being abandoned by the ceiling that only retryAgent consumes — and, like the
// slot-contention escalation in runRetry, it must not inflate the attempt
// that feeds the rendered prompt either.
func TestLandingRetryRefreshFailureIgnoresMaxAttempts(t *testing.T) {
	w := testSettings(t)
	w.Config.Agent.MaxAttempts = 1
	w.Config.Agent.MaxRetryBackoff = 15 * time.Second
	issue := testIssue()
	tracker := &issueMapTracker{issues: map[string]domain.Issue{issue.ID: issue}, getErr: errors.New("temporary tracker failure")}
	c := testCoordinator(w.Config, tracker, &fakeAgent{}, &fakeWorkspace{})
	timer := &fakeTimer{}
	c.timer = timer

	if !c.claim(issue, w.Config) {
		t.Fatal("issue was not claimed")
	}
	c.scheduleRetry(context.Background(), issue, domain.Workspace{}, 3, retryLanding, "landing_waiting", time.Second)
	timer.fire(0)

	c.mu.Lock()
	retry, ok := c.retries[issue.ID]
	claimed := c.claimed[issue.ID]
	c.mu.Unlock()
	if !claimed {
		t.Fatal("landing retry refresh failure dropped its claim below max_attempts=1")
	}
	if !ok || retry.kind != retryLanding || retry.reason != "retry_refresh" || retry.attempt != 3 {
		t.Fatalf("retry=%+v ok=%v, want a further landing retry past the ceiling with its attempt unchanged", retry, ok)
	}
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestStalledRunCancelsAndSchedulesRetry(t *testing.T) {
	w := testSettings(t)
	w.Config.Codex.StallTimeout = time.Second
	issue := testIssue()
	tracker := &fakeTracker{issue: issue}
	block := make(chan domain.Event)
	agent := &fakeAgent{events: func() <-chan domain.Event { return block }, started: make(chan struct{}, 1)}
	ws := &fakeWorkspace{after: make(chan struct{}, 1)}
	c := testCoordinator(w.Config, tracker, agent, ws)
	clock := &mutableClock{now: time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)}
	c.clock = clock
	timer := &fakeTimer{signal: make(chan struct{}, 1)}
	c.timer = timer

	c.Tick(context.Background())
	<-agent.started
	waitForRunning(t, c, issue.Identifier)
	clock.set(time.Date(2026, 7, 18, 12, 0, 2, 0, time.UTC))
	c.Tick(context.Background())
	<-ws.after
	<-timer.signal

	starts, _, cancels := agent.counts()
	if starts != 1 || cancels != 1 {
		t.Fatalf("starts=%d cancels=%d, want stalled session cancelled once", starts, cancels)
	}
	c.mu.Lock()
	retry := c.retries[issue.ID]
	c.mu.Unlock()
	if retry.reason != "stalled" || retry.attempt != 1 {
		t.Fatalf("retry=%+v", retry)
	}
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestStallBudgetIsResolvedUnderTheRunsBackendNotTheConfiguredOne pins the
// pinning decision this scheduler makes: reload keeps publishing live policy,
// but the stall budget for an in-flight run is read under the backend that
// started it. Selecting a different backend mid-run must not silently disable
// stall detection for a run the previous backend owns.
func TestStallBudgetIsResolvedUnderTheRunsBackendNotTheConfiguredOne(t *testing.T) {
	w := testSettings(t)
	w.Config.Codex.StallTimeout = time.Second
	current := w.Config
	issue := testIssue()
	tracker := &fakeTracker{issue: issue}
	block := make(chan domain.Event)
	agent := &fakeAgent{events: func() <-chan domain.Event { return block }, started: make(chan struct{}, 1)}
	ws := &fakeWorkspace{after: make(chan struct{}, 1)}
	c := New(tracker, agent, ws, func() config.Settings { return current }, nil)
	clock := &mutableClock{now: time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)}
	c.clock = clock
	timer := &fakeTimer{signal: make(chan struct{}, 1)}
	c.timer = timer

	c.Tick(context.Background())
	<-agent.started
	waitForRunning(t, c, issue.Identifier)

	// A reload now selects a backend that carries no stall budget of its own.
	// Reading the budget under the current selection would leave this run
	// unsupervised forever; reading it under the run's backend still stalls it.
	changed := current
	changed.Agent.Backend = "some-other-backend"
	current = changed

	clock.set(time.Date(2026, 7, 18, 12, 0, 2, 0, time.UTC))
	c.Tick(context.Background())
	// Bounded waits on purpose: resolving the budget under the configured
	// backend instead of the run's leaves this run unsupervised, which shows up
	// as nothing ever happening. Fail with that diagnosis instead of hanging the
	// package until the test binary's own timeout.
	select {
	case <-ws.after:
	case <-time.After(5 * time.Second):
		t.Fatal("the stalled run was never reconciled: the stall budget was not resolved under the run's backend")
	}
	select {
	case <-timer.signal:
	case <-time.After(5 * time.Second):
		t.Fatal("no retry was scheduled for the stalled run")
	}

	starts, _, cancels := agent.counts()
	if starts != 1 || cancels != 1 {
		t.Fatalf("starts=%d cancels=%d, want the stalled session cancelled once", starts, cancels)
	}
	c.mu.Lock()
	retry := c.retries[issue.ID]
	c.mu.Unlock()
	if retry.reason != "stalled" {
		t.Fatalf("retry=%+v, want a stalled retry", retry)
	}
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestReconciliationRefreshesStateCapacityForLaterAdmissions(t *testing.T) {
	w := testSettings(t)
	w.Config.Agent.MaxConcurrent = 2
	w.Config.Tracker.ActiveStates = []string{"Todo", "Doing"}
	w.Config.Agent.ByState = map[string]int{"todo": 1, "doing": 1}
	first := testIssue()
	second := testIssue()
	second.ID, second.Identifier = "second", "ENG-2"
	tracker := &issueMapTracker{candidates: []domain.Issue{first, second}, issues: map[string]domain.Issue{first.ID: first, second.ID: second}}
	block := make(chan domain.Event)
	agent := &fakeAgent{events: func() <-chan domain.Event { return block }, started: make(chan struct{}, 2)}
	ws := &fakeWorkspace{after: make(chan struct{}, 2)}
	c := testCoordinator(w.Config, tracker, agent, ws)

	c.Tick(context.Background())
	<-agent.started
	waitForRunning(t, c, first.Identifier)
	fresh := first
	fresh.State = "Doing"
	tracker.setIssue(fresh)
	c.Tick(context.Background())
	<-agent.started

	c.mu.Lock()
	state := c.admitted[first.ID]
	c.mu.Unlock()
	if state != "doing" {
		t.Fatalf("first admitted state=%q, want refreshed doing", state)
	}
	starts, _, _ := agent.counts()
	if starts != 2 {
		t.Fatalf("starts=%d, want Todo admission after first moved to Doing", starts)
	}
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-ws.after
	<-ws.after
}

func TestFourImplementationAndReworkIssuesRunConcurrently(t *testing.T) {
	w := testSettings(t)
	w.Config.Agent.MaxConcurrent = 4
	w.Config.Tracker.ActiveStates = []string{"Todo", "In Progress", "Rework"}

	issues := make([]domain.Issue, 5)
	states := []string{"Todo", "In Progress", "Rework", "Rework", "In Progress"}
	issueMap := make(map[string]domain.Issue, len(issues))
	for index := range issues {
		issues[index] = testIssue()
		issues[index].ID = fmt.Sprintf("issue-%d", index+1)
		issues[index].Identifier = fmt.Sprintf("ENG-%d", index+1)
		issues[index].State = states[index]
		issueMap[issues[index].ID] = issues[index]
	}

	tracker := &issueMapTracker{candidates: issues, issues: issueMap}
	block := make(chan domain.Event)
	agent := &fakeAgent{events: func() <-chan domain.Event { return block }, started: make(chan struct{}, 4)}
	ws := &fakeWorkspace{after: make(chan struct{}, 4)}
	c := testCoordinator(w.Config, tracker, agent, ws)

	c.Tick(context.Background())
	for range 4 {
		<-agent.started
	}
	starts, _, _ := agent.counts()
	if starts != 4 {
		t.Fatalf("starts=%d, want four concurrent implementation/rework agents", starts)
	}
	c.mu.Lock()
	admitted := len(c.admitted)
	c.mu.Unlock()
	if admitted != 4 {
		t.Fatalf("admitted=%d, want the global four-agent capacity fully occupied", admitted)
	}
	if c.claim(issues[4], w.Config) {
		t.Fatal("a fifth implementation issue exceeded the global four-agent capacity")
	}

	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	for range 4 {
		<-ws.after
	}
}

// TestMergingAndUnrelatedImplementationRunConcurrentlyUnderByStateCapacity
// exercises the active four-agent policy end to end at the coordinator level:
// one Merging landing agent and unrelated implementation agents admit and run
// at the same time, a queued retry timer never occupies a concurrency slot
// while it waits, and max_concurrent_agents_by_state still refuses a second
// concurrent Merging issue even though overall capacity has spare room.
func TestMergingAndUnrelatedImplementationRunConcurrentlyUnderByStateCapacity(t *testing.T) {
	w := testSettings(t)
	w.Config.Agent.MaxConcurrent = 4
	w.Config.Tracker.ActiveStates = []string{"In Progress", "Merging"}
	w.Config.Agent.ByState = map[string]int{"merging": 1}

	implementation := testIssue()
	implementation.ID, implementation.Identifier, implementation.State = "impl", "ENG-1", "In Progress"
	landing := testIssue()
	landing.ID, landing.Identifier, landing.State = "landing", "ENG-2", "Merging"
	secondLanding := testIssue()
	secondLanding.ID, secondLanding.Identifier, secondLanding.State = "landing-2", "ENG-3", "Merging"
	retryable := testIssue()
	retryable.ID, retryable.Identifier, retryable.State = "retryable", "ENG-4", "In Progress"
	extra := testIssue()
	extra.ID, extra.Identifier, extra.State = "extra", "ENG-5", "In Progress"
	fourth := testIssue()
	fourth.ID, fourth.Identifier, fourth.State = "fourth", "ENG-6", "In Progress"

	tracker := &issueMapTracker{
		candidates: []domain.Issue{implementation, landing},
		issues: map[string]domain.Issue{
			implementation.ID: implementation, landing.ID: landing, secondLanding.ID: secondLanding,
			retryable.ID: retryable, extra.ID: extra, fourth.ID: fourth,
		},
	}
	block := make(chan domain.Event)
	agent := &fakeAgent{events: func() <-chan domain.Event { return block }, started: make(chan struct{}, 4)}
	ws := &fakeWorkspace{after: make(chan struct{}, 4)}
	c := testCoordinator(w.Config, tracker, agent, ws)
	timer := &fakeTimer{}
	c.timer = timer

	// One unrelated implementation issue and one Merging landing issue admit
	// and launch together in the same poll.
	c.Tick(context.Background())
	<-agent.started
	<-agent.started
	if starts, _, _ := agent.counts(); starts != 2 {
		t.Fatalf("starts=%d, want the unrelated implementation and Merging issues both admitted", starts)
	}

	// A queued retry for a third, unrelated issue must not occupy a
	// concurrency slot while it waits.
	if !c.claim(retryable, w.Config) {
		t.Fatal("retryable issue was not claimed")
	}
	c.scheduleRetry(context.Background(), retryable, domain.Workspace{}, 1, retryAgent, "test", time.Minute)
	c.mu.Lock()
	admitted := len(c.admitted)
	c.mu.Unlock()
	if admitted != 2 {
		t.Fatalf("admitted=%d, want a queued retry timer to consume no concurrency slot", admitted)
	}

	// A second concurrent Merging issue is refused by the per-state cap even
	// though overall capacity (2 of 4) still has room.
	if c.claim(secondLanding, w.Config) {
		t.Fatal("a second concurrent Merging issue must be refused by max_concurrent_agents_by_state")
	}

	// The retry's reserved claim must not itself block a genuinely free
	// general-capacity slot from admitting a new, unrelated candidate.
	tracker.mu.Lock()
	tracker.candidates = append(tracker.candidates, extra, fourth)
	tracker.mu.Unlock()
	c.Tick(context.Background())
	<-agent.started
	<-agent.started
	if starts, _, _ := agent.counts(); starts != 4 {
		t.Fatalf("starts=%d, want both free general-capacity slots admitted despite the queued retry", starts)
	}

	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-ws.after
	<-ws.after
	<-ws.after
	<-ws.after
}

// startTransitionSettings adds a Todo -> In Progress dispatch-time start
// transition to the base test workflow so the coordinator's host-side move is
// exercised. In Progress is added as an active state so the moved issue stays
// eligible for reconciliation exactly as production configuration requires.
func startTransitionSettings(t *testing.T) config.Settings {
	t.Helper()
	w := testSettings(t)
	s := w.Config
	s.Tracker.ActiveStates = []string{"Todo", "In Progress"}
	s.Tracker.HostTransitions = config.HostTransitions{Start: map[string]string{"todo": "In Progress"}}
	return s
}

func TestDispatchMovesTodoIssueToInProgress(t *testing.T) {
	s := startTransitionSettings(t)
	issue := testIssue()
	tracker := &fakeTracker{issue: issue}
	agent := &fakeAgent{events: completedEvents}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{}, 1)}
	c := testCoordinator(s, tracker, agent, ws)

	c.Tick(context.Background())
	<-ws.after
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}

	calls := tracker.transitionCalls()
	if len(calls) != 1 {
		t.Fatalf("start transitions=%+v, want exactly one dispatch transition", calls)
	}
	if calls[0].id != issue.ID || calls[0].to != "In Progress" {
		t.Fatalf("start transition=%+v, want issue %q moved to In Progress", calls[0], issue.ID)
	}
	if starts, _, _ := agent.counts(); starts != 1 {
		t.Fatalf("starts=%d, want the session to start after the transition", starts)
	}
}

func TestDispatchStartTransitionLogsOperationAndEdge(t *testing.T) {
	s := startTransitionSettings(t)
	issue := testIssue()
	tracker := &fakeTracker{issue: issue}
	agent := &fakeAgent{events: completedEvents}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{}, 1)}
	var logs bytes.Buffer
	c := New(tracker, agent, ws, func() config.Settings { return s }, slog.New(slog.NewJSONHandler(&logs, nil)))
	c.clock = fakeClock{now: time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)}

	c.Tick(context.Background())
	<-ws.after
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}

	output := logs.String()
	if !strings.Contains(output, `"msg":"issue moved to started state"`) ||
		!strings.Contains(output, `"operation":"start_transition"`) ||
		!strings.Contains(output, `"from_state":"todo"`) ||
		!strings.Contains(output, `"to_state":"in progress"`) {
		t.Fatalf("host-side start transition edge not reconstructable from log: %s", output)
	}
}

func TestDispatchDoesNotTransitionAlreadyStartedIssue(t *testing.T) {
	s := startTransitionSettings(t)
	// An issue re-observed already In Progress (a restart or turn-limit
	// re-dispatch) has no configured start edge for its state, so the
	// coordinator must not request any transition.
	started := testIssue()
	started.State = "In Progress"
	tracker := &fakeTracker{issue: started}
	agent := &fakeAgent{events: completedEvents}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{}, 1)}
	c := testCoordinator(s, tracker, agent, ws)

	c.Tick(context.Background())
	<-ws.after
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}

	if calls := tracker.transitionCalls(); len(calls) != 0 {
		t.Fatalf("start transitions=%+v, want none for an already-started issue", calls)
	}
	if starts, _, _ := agent.counts(); starts != 1 {
		t.Fatalf("starts=%d, want the session to start", starts)
	}
}

func TestDispatchTransitionFailureDoesNotBlockOrDoubleDispatch(t *testing.T) {
	s := startTransitionSettings(t)
	issue := testIssue()
	tracker := &fakeTracker{issue: issue, transitionErr: errors.New("tracker write failed")}
	agent := &fakeAgent{events: completedEvents}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{}, 1)}
	var logs bytes.Buffer
	c := New(tracker, agent, ws, func() config.Settings { return s }, slog.New(slog.NewJSONHandler(&logs, nil)))
	c.clock = fakeClock{now: time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)}

	c.Tick(context.Background())
	<-ws.after
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}

	if calls := tracker.transitionCalls(); len(calls) != 1 {
		t.Fatalf("start transitions=%+v, want exactly one attempt even on failure", calls)
	}
	// A failed transition must degrade gracefully: the session still starts
	// exactly once and the run is not double-dispatched.
	if starts, _, _ := agent.counts(); starts != 1 {
		t.Fatalf("starts=%d, want the run to proceed once despite the failed transition", starts)
	}
	if output := logs.String(); !strings.Contains(output, `"msg":"dispatch start transition failed"`) {
		t.Fatalf("failed transition was not logged: %s", output)
	}
}

// TestHandoffRunRecordsObservationForRevertDetection proves a completed run
// that ends because its issue reached the review handoff state is remembered,
// so a later external revert of that handoff can be attributed at poll time.
func TestHandoffRunRecordsObservationForRevertDetection(t *testing.T) {
	w := testSettings(t)
	w.Config.Tracker.HandoffState = "In Review"
	w.Config.Tracker.ActiveStates = []string{"Todo", "In Progress"}
	issue := testIssue()
	issue.State = "In Progress"
	handoff := issue
	handoff.State = "In Review"
	handoff.Dispatchable = false
	tracker := &fakeTracker{issue: issue}
	tracker.setFresh(handoff)
	agent := &fakeAgent{events: completedEvents}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{}, 1)}
	c := testCoordinator(w.Config, tracker, agent, ws)

	c.Tick(context.Background())
	<-ws.after

	deadline := time.Now().Add(time.Second)
	for {
		c.mu.Lock()
		observation, ok := c.handoffs[issue.ID]
		c.mu.Unlock()
		if ok && observation.state == "in review" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("completed handoff run did not record a review-state observation")
		}
		time.Sleep(time.Millisecond)
	}
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestExternalHandoffRevertIsObservedAtPoll proves the PMR-63 flap is now
// visible in the log: an active candidate that Symphony itself just handed off
// to the review state was reverted by an external actor, so the poll loop logs
// the external delta exactly once and never re-logs or mutates the tracker.
func TestExternalHandoffRevertIsObservedAtPoll(t *testing.T) {
	w := testSettings(t)
	w.Config.Tracker.HandoffState = "In Review"
	w.Config.Tracker.ActiveStates = []string{"Todo", "In Progress"}
	// In Progress is a start-policy endpoint, so the revert warns because it
	// reactivated a pre-review implementation state — not merely because the
	// destination was unclassifiable.
	w.Config.Tracker.HostTransitions.Start = map[string]string{"todo": "In Progress"}
	reverted := testIssue()
	reverted.State = "In Progress"
	tracker := &fakeTracker{issue: reverted}
	var logs bytes.Buffer
	c := New(tracker, &fakeAgent{}, &fakeWorkspace{}, func() config.Settings { return w.Config }, slog.New(slog.NewJSONHandler(&logs, nil)))
	c.clock = fakeClock{now: time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)}

	// Pre-claim the reverted issue so the poll does not also launch a run, then
	// record Symphony's own prior handoff into the review state (claiming clears
	// any prior memory, so the observation must be set afterward).
	if !c.claim(reverted, w.Config) {
		t.Fatal("pre-claim failed")
	}
	c.noteHandoffObservation(domain.Issue{ID: reverted.ID, Identifier: reverted.Identifier, State: "In Review"}, w.Config, c.clock.Now())

	c.Tick(context.Background())

	output := logs.String()
	if !strings.Contains(output, `"msg":"external tracker state change observed"`) ||
		!strings.Contains(output, `"operation":"external_reversion"`) ||
		!strings.Contains(output, `"from_state":"in review"`) ||
		!strings.Contains(output, `"to_state":"in progress"`) ||
		!strings.Contains(output, `"issue_identifier":"ENG-1"`) {
		t.Fatalf("external handoff revert was not logged from the poll loop: %s", output)
	}
	c.mu.Lock()
	_, still := c.handoffs[reverted.ID]
	c.mu.Unlock()
	if still {
		t.Fatal("handoff observation was not consumed after the revert was logged")
	}

	logs.Reset()
	c.Tick(context.Background())
	if strings.Contains(logs.String(), "external_reversion") {
		t.Fatalf("a single external revert was re-logged on a later poll: %s", logs.String())
	}
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestHealthyHandoffIsSweptWithoutExternalRevertLog proves the common case — an
// issue that stays in review and is never reverted — neither logs a spurious
// external delta nor leaks its handoff memory: the retention window bounds the
// map even when the issue never reappears as a candidate.
func TestHealthyHandoffIsSweptWithoutExternalRevertLog(t *testing.T) {
	w := testSettings(t)
	w.Config.Tracker.HandoffState = "In Review"
	w.Config.Polling.Interval = 30 * time.Second
	tracker := &issueMapTracker{issues: map[string]domain.Issue{}}
	var logs bytes.Buffer
	clock := &mutableClock{now: time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)}
	c := New(tracker, &fakeAgent{}, &fakeWorkspace{}, func() config.Settings { return w.Config }, slog.New(slog.NewJSONHandler(&logs, nil)))
	c.clock = clock
	c.noteHandoffObservation(domain.Issue{ID: "id", Identifier: "ENG-1", State: "In Review"}, w.Config, clock.Now())

	c.Tick(context.Background())
	c.mu.Lock()
	_, present := c.handoffs["id"]
	c.mu.Unlock()
	if !present {
		t.Fatal("handoff memory was dropped inside the retention window")
	}

	clock.set(clock.now.Add(3 * time.Minute))
	c.Tick(context.Background())
	c.mu.Lock()
	_, stillThere := c.handoffs["id"]
	c.mu.Unlock()
	if stillThere {
		t.Fatal("stale handoff memory was not swept after the retention window")
	}
	if strings.Contains(logs.String(), "external_reversion") {
		t.Fatalf("a healthy handoff wrongly logged an external revert: %s", logs.String())
	}
}

// TestPostHandoffStateChangeIsClassified proves the human-controlled review
// state's outbound edges are told apart in the log: moving the issue to the
// merge state is the human approval that authorizes landing and moving it to
// the rework state is a human review decision — both expected, both info —
// while anything Symphony cannot name from the configured lifecycle, including
// a reactivation into a pre-review implementation state, stays an actionable
// warning.
func TestPostHandoffStateChangeIsClassified(t *testing.T) {
	canonical := []string{"Todo", "In Progress", "Rework", "Merging"}
	fromTodo := map[string]string{"todo": "In Progress"}
	const (
		expectedMessage = "human review state change observed"
		warnedMessage   = "external tracker state change observed"
	)
	for _, tc := range []struct {
		name                             string
		activeStates                     []string
		start                            map[string]string
		mergeState                       string
		state, operation, message, level string
	}{
		{
			name: "approval into the merge state", activeStates: canonical, start: fromTodo, mergeState: "Merging",
			state: "Merging", operation: "review_approved", message: expectedMessage, level: "INFO",
		},
		{
			name: "changes requested into the single remaining state", activeStates: canonical, start: fromTodo, mergeState: "Merging",
			state: "Rework", operation: "rework_requested", message: expectedMessage, level: "INFO",
		},
		{
			name: "reactivation into the start policy's target", activeStates: canonical, start: fromTodo, mergeState: "Merging",
			state: "In Progress", operation: "external_reversion", message: warnedMessage, level: "WARN",
		},
		{
			name: "reactivation into the start policy's source", activeStates: canonical, start: fromTodo, mergeState: "Merging",
			state: "Todo", operation: "external_reversion", message: warnedMessage, level: "WARN",
		},
		{
			// A second unaccounted-for active state makes the rework state
			// unnameable, so work parked in it is never passed off as expected.
			name:         "parked in an extra active state",
			activeStates: []string{"Todo", "In Progress", "Rework", "Merging", "Blocked"},
			start:        fromTodo, mergeState: "Merging",
			state: "Blocked", operation: "external_reversion", message: warnedMessage, level: "WARN",
		},
		{
			// The same ambiguity suppresses the rework naming itself: Symphony
			// warns rather than guessing which of the two states is Rework.
			name:         "ambiguous candidates suppress the rework naming",
			activeStates: []string{"Todo", "In Progress", "Rework", "Merging", "Blocked"},
			start:        fromTodo, mergeState: "Merging",
			state: "Rework", operation: "external_reversion", message: warnedMessage, level: "WARN",
		},
		{
			name:         "thrown back to a dispatchable backlog",
			activeStates: []string{"Backlog", "Todo", "In Progress", "Rework", "Merging"},
			start:        fromTodo, mergeState: "Merging",
			state: "Backlog", operation: "external_reversion", message: warnedMessage, level: "WARN",
		},
		{
			// A dispatch entry state the start policy does not name is still a
			// pre-review implementation state; reactivating into it must warn.
			name:         "reactivation into an unnamed entry state",
			activeStates: []string{"Todo", "Ready", "In Progress", "Rework", "Merging"},
			start:        map[string]string{"ready": "In Progress"}, mergeState: "Merging",
			state: "Todo", operation: "external_reversion", message: warnedMessage, level: "WARN",
		},
		{
			name: "no start policy leaves rework unnameable", activeStates: canonical, mergeState: "Merging",
			state: "Rework", operation: "external_reversion", message: warnedMessage, level: "WARN",
		},
		{
			// The approval edge is identified by github.merge_state alone, so it
			// survives a missing start policy.
			name: "approval without a start policy", activeStates: canonical, mergeState: "Merging",
			state: "Merging", operation: "review_approved", message: expectedMessage, level: "INFO",
		},
		{
			// With landing unconfigured there is no merge state to recognize, so
			// the same edge is unnameable and warns instead.
			name: "approval with landing unconfigured", activeStates: canonical, start: fromTodo,
			state: "Merging", operation: "external_reversion", message: warnedMessage, level: "WARN",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := testSettings(t)
			w.Config.Tracker.HandoffState = "In Review"
			w.Config.Tracker.ActiveStates = tc.activeStates
			w.Config.Tracker.HostTransitions.Start = tc.start
			w.Config.GitHub.MergeState = tc.mergeState
			moved := testIssue()
			moved.State = tc.state
			tracker := &fakeTracker{issue: moved}
			var logs bytes.Buffer
			c := New(tracker, &fakeAgent{}, &fakeWorkspace{}, func() config.Settings { return w.Config }, slog.New(slog.NewJSONHandler(&logs, nil)))
			c.clock = fakeClock{now: time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)}

			// Pre-claim so the poll only classifies the change instead of also
			// launching a run, then record Symphony's own prior handoff (claiming
			// clears any prior memory, so the observation must be set afterward).
			if !c.claim(moved, w.Config) {
				t.Fatal("pre-claim failed")
			}
			c.noteHandoffObservation(domain.Issue{ID: moved.ID, Identifier: moved.Identifier, State: "In Review"}, w.Config, c.clock.Now())

			c.Tick(context.Background())

			output := logs.String()
			if !strings.Contains(output, `"msg":"`+tc.message+`"`) ||
				!strings.Contains(output, `"operation":"`+tc.operation+`"`) ||
				!strings.Contains(output, `"level":"`+tc.level+`"`) ||
				!strings.Contains(output, `"from_state":"in review"`) ||
				!strings.Contains(output, `"to_state":"`+config.Norm(tc.state)+`"`) ||
				!strings.Contains(output, `"issue_identifier":"ENG-1"`) {
				t.Fatalf("post-handoff change to %s was not classified as %s: %s", tc.state, tc.operation, output)
			}
			if tc.operation != "external_reversion" && strings.Contains(output, "external_reversion") {
				t.Fatalf("an expected human review decision was also logged as a reversion: %s", output)
			}
			if err := c.Shutdown(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func testCoordinator(settings config.Settings, tracker domain.Tracker, agent domain.AgentBackend, ws domain.WorkspaceExecutor) *Coordinator {
	c := New(tracker, agent, ws, func() config.Settings { return settings }, nil)
	c.clock = fakeClock{now: time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)}
	return c
}

func testSettings(t *testing.T) config.Workflow {
	t.Helper()
	d := t.TempDir()
	workflow := filepath.Join(d, "WORKFLOW.md")
	if err := os.WriteFile(workflow, []byte("---\ntracker: {kind: linear, active_states: [Todo], terminal_states: [Done]}\nagent: {max_concurrent_agents: 1, max_turns: 1}\nworkspace: {root: /tmp/work}\n---\nWork on {{.issue.identifier}}"), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := config.Load(workflow, "")
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func testIssue() domain.Issue {
	return domain.Issue{ID: "id", Identifier: "ENG-1", Title: "Work", State: "Todo", Dispatchable: true}
}

func completedEvents() <-chan domain.Event {
	ch := make(chan domain.Event, 1)
	ch <- domain.Event{Kind: domain.EventCompleted, At: time.Now(), SessionID: "t-u"}
	close(ch)
	return ch
}
func closedEvents() <-chan domain.Event {
	ch := make(chan domain.Event)
	close(ch)
	return ch
}

// failedEvents returns a domain.EventFailed carrying message, the shape a
// real backend uses for model/provider-reported text (see
// claude/backend.go's emitTerminal calls) -- unlike closedEvents, which is
// the host's own event plumbing giving up with no verdict at all.
func failedEvents(message string) func() <-chan domain.Event {
	return func() <-chan domain.Event {
		ch := make(chan domain.Event, 1)
		ch <- domain.Event{Kind: domain.EventFailed, At: time.Now(), Message: message}
		close(ch)
		return ch
	}
}
