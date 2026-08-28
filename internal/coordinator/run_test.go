package coordinator

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/domain"
)

// TestPromptRenderedDebugRecordCarriesByteCountsNotText pins the Debug-level
// "prompt rendered" record (added for PMR-136) to actually being emitted with
// its two byte-count fields, and to those fields surviving redaction as
// plain numbers rather than being treated as opaque text: prompt_bytes and
// delivery_instruction_bytes are ints, so safeAttr passes them through
// unmodified and opaqueKey does not match either name. The rendered prompt
// text itself must never appear in the log.
func TestPromptRenderedDebugRecordCarriesByteCountsNotText(t *testing.T) {
	w := testSettings(t)
	issue := testIssue()
	agent := &fakeAgent{events: closedEvents, started: make(chan struct{}, 1)}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{}, 1)}
	var logs bytes.Buffer
	c := New(&fakeTracker{issue: issue}, agent, ws, func() config.Settings { return w.Config }, slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer assertInvariants(t, c)
	c.Tick(context.Background())
	<-ws.after
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	line := findLine(t, logs.String(), `"msg":"prompt rendered"`)
	if !strings.Contains(line, `"issue_identifier":"ENG-1"`) {
		t.Fatalf("prompt rendered record missing issue_identifier: %s", line)
	}
	for _, field := range []string{`"prompt_bytes":`, `"delivery_instruction_bytes":`} {
		if !strings.Contains(line, field) {
			t.Fatalf("prompt rendered record missing %s: %s", field, line)
		}
	}
	if strings.Contains(line, `"prompt_bytes":"`) || strings.Contains(line, `"delivery_instruction_bytes":"`) {
		t.Fatalf("prompt rendered byte counts were redacted to strings instead of passing through as numbers: %s", line)
	}
	if strings.Contains(logs.String(), "Work on ENG-1") {
		t.Fatalf("rendered prompt text leaked into the log: %s", logs.String())
	}
}

func TestRenderExplainsHostAndManualDeliveryModes(t *testing.T) {
	settings := config.Settings{Prompt: "Work on {{.issue.identifier}}"}
	issue := domain.Issue{Identifier: "PMR-40"}
	manual, _, err := render(settings, issue, 0, config.DefaultAgentBackend)
	if err != nil || !strings.Contains(manual, "Delivery mode: manual") || !strings.Contains(manual, "Do not run gh, git push") {
		t.Fatalf("manual prompt=%q err=%v", manual, err)
	}
	settings.GitHub.Enabled = true
	settings.Tracker.HandoffState = "In Review"
	host, _, err := render(settings, issue, 0, config.DefaultAgentBackend)
	if err != nil || !strings.Contains(host, "Delivery mode: host-side publish") || !strings.Contains(host, "github_publish_pr with why, what_changed, and on_call") || !strings.Contains(host, "github_pr_context") {
		t.Fatalf("host prompt=%q err=%v", host, err)
	}
	// The same settings under the MCP-framed backend must name the tools the CLI
	// will actually serve. render is the only caller of DeliveryInstructions, so
	// this is where a dropped backend argument becomes observable: it would leave
	// the prompt naming Codex tool names for a Claude session.
	claude, deliveryBytes, err := render(settings, issue, 0, config.ClaudeAgentBackend)
	if err != nil {
		t.Fatal(err)
	}
	if deliveryBytes <= 0 || deliveryBytes >= len(claude) {
		t.Fatalf("delivery instruction byte count=%d prompt bytes=%d", deliveryBytes, len(claude))
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
	defer assertInvariants(t, c)
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

func TestReconciliationCancellationDoesNotRetry(t *testing.T) {
	w := testSettings(t)
	issue := testIssue()
	tracker := &fakeTracker{issue: issue}
	block := make(chan domain.Event)
	agent := &fakeAgent{events: func() <-chan domain.Event { return block }, started: make(chan struct{}, 1)}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{}, 1)}
	c := testCoordinator(w.Config, tracker, agent, ws)
	defer assertInvariants(t, c)
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

// TestReconcileRefreshFailureLogsWarnOnlyWhenNotCancelled pins the PMR-128
// fix that a run-scoped refresh racing its own context's cancellation is
// routine, not an operator-facing problem: reconcile still surfaces a live
// tracker failure at Warn, but a failure discovered after ctx is already done
// logs at Debug instead.
func TestReconcileRefreshFailureLogsWarnOnlyWhenNotCancelled(t *testing.T) {
	w := testSettings(t)
	issue := testIssue()
	refreshErr := errors.New("linear tracker_transport: Linear request failed")

	t.Run("live context", func(t *testing.T) {
		var logs bytes.Buffer
		tracker := &fakeTracker{issue: issue, getIssuesErr: refreshErr}
		c := New(tracker, &fakeAgent{}, &fakeWorkspace{}, func() config.Settings { return w.Config }, slog.New(slog.NewJSONHandler(&logs, nil)))
		defer assertInvariants(t, c)
		c.seedRunning(issue, &running{issue: issue})

		if err := c.reconcile(context.Background()); err == nil {
			t.Fatal("expected reconcile to surface the refresh failure")
		}
		if !strings.Contains(logs.String(), "running issue refresh failed") {
			t.Fatalf("missing warn log: %s", logs.String())
		}
	})

	t.Run("cancelled context", func(t *testing.T) {
		var logs bytes.Buffer
		tracker := &fakeTracker{issue: issue, getIssuesErr: refreshErr}
		c := New(tracker, &fakeAgent{}, &fakeWorkspace{}, func() config.Settings { return w.Config }, slog.New(slog.NewJSONHandler(&logs, nil)))
		defer assertInvariants(t, c)
		c.seedRunning(issue, &running{issue: issue})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		if err := c.reconcile(ctx); err == nil {
			t.Fatal("expected reconcile to surface the refresh failure")
		}
		if strings.Contains(logs.String(), "running issue refresh failed") {
			t.Fatalf("cancelled refresh produced a log record at the default (Info+) level: %s", logs.String())
		}
	})
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
	defer assertInvariants(t, c)

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

func TestStalledRunCancelsAndSchedulesRetry(t *testing.T) {
	w := testSettings(t)
	w.Config.Codex.StallTimeout = time.Second
	issue := testIssue()
	tracker := &fakeTracker{issue: issue}
	block := make(chan domain.Event)
	agent := &fakeAgent{events: func() <-chan domain.Event { return block }, started: make(chan struct{}, 1)}
	ws := &fakeWorkspace{after: make(chan struct{}, 1)}
	c := testCoordinator(w.Config, tracker, agent, ws)
	defer assertInvariants(t, c)
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
	retry, _ := c.armedRetry(issue.ID)
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
	defer assertInvariants(t, c)
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
	retry, _ := c.armedRetry(issue.ID)
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
	defer assertInvariants(t, c)

	c.Tick(context.Background())
	<-agent.started
	waitForRunning(t, c, first.Identifier)
	fresh := first
	fresh.State = "Doing"
	tracker.setIssue(fresh)
	c.Tick(context.Background())
	<-agent.started

	state := c.admittedState(first.ID)
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
