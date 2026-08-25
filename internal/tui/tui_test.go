package tui

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/pmrrasmussen/symphony/internal/operator"
)

func TestOverviewSelectionAndDetailNavigation(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	model := New([]operator.Instance{{ID: "com.pmrrasmussen.symphony.alpha", Liveness: operator.LivenessRunning}, {ID: "com.pmrrasmussen.symphony.beta", Liveness: operator.LivenessStopped}}, now)

	model, quit := model.Update("j")
	if quit || model.selected != 1 {
		t.Fatalf("down selection = %d, quit=%v", model.selected, quit)
	}
	model, quit = model.Update("enter")
	if quit || model.page != statusPage {
		t.Fatalf("open detail page=%v quit=%v", model.page, quit)
	}
	model, _ = model.Update("c")
	if model.page != configPage {
		t.Fatalf("config page=%v", model.page)
	}
	model, quit = model.Update("q")
	if quit || model.page != overviewPage {
		t.Fatalf("back page=%v quit=%v", model.page, quit)
	}
	_, quit = model.Update("q")
	if !quit {
		t.Fatal("overview q did not quit")
	}
}

func TestStatusViewRendersOnlySafeRuntimeFields(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	instance := operator.Instance{
		ID:       "com.pmrrasmussen.symphony.repo",
		Liveness: operator.LivenessRunning,
		Launchd:  operator.LaunchdStatus{Loaded: true, Process: true, PID: 45},
		Config:   &operator.EffectiveConfig{MaxTurns: 20},
		Snapshot: &operator.Snapshot{
			State: "running", StartedAt: now.Add(-2 * time.Hour), UpdatedAt: now.Add(-time.Second),
			Coordinator: operator.RuntimeSnapshot{
				Claimed: 1,
				Running: []operator.RunningSnapshot{{
					IssueIdentifier: "PMR-75", IssueState: "In Progress", StartedAt: now.Add(-time.Hour), LastActivityAt: now.Add(-time.Minute), TurnCount: 4,
					Usage: operator.Usage{InputTokens: 12, OutputTokens: 3, TotalTokens: 15}, RateLimit: map[string]int64{"remaining": 9},
					OutstandingOperation: &operator.OutstandingOperation{Type: "mcpToolCall", Name: "github_pr_context", AgeMS: 3000},
				}},
				Retrying: []operator.RetrySnapshot{{IssueIdentifier: "PMR-76", Attempt: 2, Kind: "retry", Reason: "timeout", Due: now.Add(time.Minute)}},
			},
		},
		RecentLog: []operator.LogEvent{{Time: now, Level: "INFO", Message: "issue claimed"}},
	}
	model := New([]operator.Instance{instance}, now)
	model, _ = model.Update("enter")
	view := model.View(now)
	for _, want := range []string{"PMR-75 (In Progress)", "turns 4/20", "tokens: input 12, output 3, total 15", "waiting: mcpToolCall github_pr_context", "Retry PMR-76", "Recent redacted lifecycle activity", "issue claimed"} {
		if !strings.Contains(view, want) {
			t.Fatalf("status view missing %q:\n%s", want, view)
		}
	}
	for _, forbidden := range []string{"prompt", "tool arguments", "credential-secret"} {
		if strings.Contains(view, forbidden) {
			t.Fatalf("status view exposed %q:\n%s", forbidden, view)
		}
	}
}

func TestConfigViewShowsCredentialPresenceNotReferences(t *testing.T) {
	now := time.Now()
	instance := operator.Instance{ID: "safe", Config: &operator.EffectiveConfig{
		ProjectSelector: "project", WorkspaceSource: "/repo", WorkspaceRoot: "/work", AgentBackend: "codex", CodexCommand: "codex app-server", CodexApprovalPolicy: "never", CodexThreadSandbox: "workspace-write",
		Credentials: operator.Credentials{Tracker: operator.CredentialPresence{Configured: true, EnvironmentNames: []string{"SECRET_ENV"}, FileReferences: []string{"/secret/path"}}, GitHub: operator.CredentialPresence{}},
	}}
	model := New([]operator.Instance{instance}, now)
	model, _ = model.Update("enter")
	model, _ = model.Update("c")
	view := model.View(now)
	if !strings.Contains(view, "Credentials: Linear configured; GitHub not configured") {
		t.Fatalf("credential presence missing:\n%s", view)
	}
	for _, forbidden := range []string{"SECRET_ENV", "/secret/path"} {
		if strings.Contains(view, forbidden) {
			t.Fatalf("config displayed credential reference %q:\n%s", forbidden, view)
		}
	}
}

func TestConfigViewLabelsAgentLineWithSelectedBackend(t *testing.T) {
	now := time.Now()
	for _, testCase := range []struct{ backend, want string }{
		{backend: "codex", want: "\nCodex: codex app-server\n"},
		// A snapshot written before backend selection existed reports no
		// backend, which must still render a label.
		{backend: "", want: "\nAgent: codex app-server\n"},
	} {
		instance := operator.Instance{ID: "safe", Config: &operator.EffectiveConfig{AgentBackend: testCase.backend, CodexCommand: "codex app-server"}}
		model := New([]operator.Instance{instance}, now)
		model, _ = model.Update("enter")
		model, _ = model.Update("c")
		view := model.View(now)
		if !strings.Contains(view, testCase.want) {
			t.Fatalf("backend %q view missing %q:\n%s", testCase.backend, testCase.want, view)
		}
	}
}

func TestRefreshHandlesInstancesAppearingAndDisappearing(t *testing.T) {
	now := time.Now()
	model := New([]operator.Instance{{ID: "first"}, {ID: "second"}}, now)
	model.selected = 1
	model.Refresh(nil, nil, now)
	if model.selected != 0 || model.page != overviewPage || len(model.instances) != 0 {
		t.Fatalf("empty refresh model=%#v", model)
	}
	model.Refresh(nil, context.DeadlineExceeded, now)
	if !strings.Contains(model.message, "refresh failed") {
		t.Fatalf("refresh error message=%q", model.message)
	}
}

func TestRunRefreshesWithoutPersistentConnection(t *testing.T) {
	var calls int
	discover := func(context.Context, operator.Options) ([]operator.Instance, error) {
		calls++
		return []operator.Instance{{ID: "instance", Liveness: operator.LivenessRunning}}, nil
	}
	var output bytes.Buffer
	if err := Run(context.Background(), strings.NewReader("r\nq\n"), &output, discover); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("discovery calls=%d, want initial + refresh", calls)
	}
	if !strings.Contains(output.String(), "refreshed") {
		t.Fatalf("refresh frame missing:\n%s", output.String())
	}
}

func TestRedirectedOutputCarriesNoControlSequences(t *testing.T) {
	discover := func(context.Context, operator.Options) ([]operator.Instance, error) {
		return []operator.Instance{{ID: "instance", Liveness: operator.LivenessRunning}}, nil
	}
	var output bytes.Buffer
	if err := Run(context.Background(), strings.NewReader("q\n"), &output, discover); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "\x1b") {
		t.Fatalf("redirected frame contains an escape sequence:\n%q", output.String())
	}
	if !strings.Contains(output.String(), "Symphony operator view") {
		t.Fatalf("redirected frame missing the view:\n%s", output.String())
	}
}

func newTestApp(instances []operator.Instance, discover Discover) *app {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	return &app{model: New(instances, now), discover: discover, ctx: context.Background()}
}

func TestSingleKeypressesMoveSelectionWithoutEnter(t *testing.T) {
	view := newTestApp([]operator.Instance{{ID: "alpha"}, {ID: "beta"}}, nil)
	if _, command := view.Update(tea.KeyPressMsg{Code: 'j', Text: "j"}); command != nil {
		t.Fatalf("j produced a command: %#v", command)
	}
	if view.model.selected != 1 {
		t.Fatalf("selected=%d after j, want 1", view.model.selected)
	}
	if _, command := view.Update(tea.KeyPressMsg{Code: 'k', Text: "k"}); command != nil {
		t.Fatalf("k produced a command: %#v", command)
	}
	if view.model.selected != 0 {
		t.Fatalf("selected=%d after k, want 0", view.model.selected)
	}
}

func TestQuitKeysEndTheProgram(t *testing.T) {
	for name, message := range map[string]tea.KeyPressMsg{
		"q":      {Code: 'q', Text: "q"},
		"ctrl+c": {Code: 'c', Mod: tea.ModCtrl},
	} {
		t.Run(name, func(t *testing.T) {
			view := newTestApp([]operator.Instance{{ID: "alpha"}}, nil)
			_, command := view.Update(message)
			if command == nil {
				t.Fatal("no command returned")
			}
			if _, quit := command().(tea.QuitMsg); !quit {
				t.Fatalf("%s did not quit", name)
			}
		})
	}
}

func TestScheduledTickRefreshesAndRearmsItself(t *testing.T) {
	var calls int
	discover := func(context.Context, operator.Options) ([]operator.Instance, error) {
		calls++
		return []operator.Instance{{ID: "alpha", Liveness: operator.LivenessRunning}}, nil
	}
	view := newTestApp(nil, discover)
	_, command := view.Update(tickMsg(time.Now()))
	if command == nil {
		t.Fatal("tick returned no command")
	}
	batch, ok := command().(tea.BatchMsg)
	if !ok {
		t.Fatalf("tick command produced %T, want a batch of discovery and the next tick", command())
	}
	// Discovery first, then the re-arm. The second command is deliberately not
	// invoked here: it sleeps for the whole refresh interval by design.
	if len(batch) != 2 {
		t.Fatalf("batch has %d commands, want discovery and the next tick", len(batch))
	}
	// Discovery must run inside the command, not while the tick was handled, so
	// that a slow launchctl sweep cannot block the view.
	if calls != 0 {
		t.Fatalf("discovery ran %d times before its command was invoked", calls)
	}
	message, ok := batch[0]().(discoveredMsg)
	if !ok {
		t.Fatalf("first batched command produced %T, want a discovery result", batch[0]())
	}
	if message.err != nil {
		t.Fatal(message.err)
	}
	if calls != 1 {
		t.Fatalf("discovery calls=%d, want 1", calls)
	}
}

func TestRefreshKeyRunsDiscoveryAsACommand(t *testing.T) {
	var calls int
	discover := func(context.Context, operator.Options) ([]operator.Instance, error) {
		calls++
		return []operator.Instance{{ID: "alpha"}, {ID: "beta"}}, nil
	}
	view := newTestApp(nil, discover)
	_, command := view.Update(tea.KeyPressMsg{Code: 'r', Text: "r"})
	if command == nil {
		t.Fatal("r returned no command")
	}
	if calls != 0 {
		t.Fatalf("discovery ran %d times on the UI goroutine", calls)
	}
	message, ok := command().(discoveredMsg)
	if !ok {
		t.Fatalf("r produced %T, want a discovery result", command())
	}
	if _, next := view.Update(message); next != nil {
		t.Fatalf("applying discovery produced a command: %#v", next)
	}
	if len(view.model.instances) != 2 {
		t.Fatalf("model has %d instances after refresh, want 2", len(view.model.instances))
	}
	if !strings.Contains(view.View().Content, "refreshed") {
		t.Fatalf("refreshed frame missing the message:\n%s", view.View().Content)
	}
}

func TestWindowSizeIsRecordedForTheRenderer(t *testing.T) {
	view := newTestApp(nil, nil)
	if _, command := view.Update(tea.WindowSizeMsg{Width: 120, Height: 40}); command != nil {
		t.Fatalf("resize produced a command: %#v", command)
	}
	if view.width != 120 || view.height != 40 {
		t.Fatalf("recorded %dx%d, want 120x40", view.width, view.height)
	}
}
