package tui

import (
	"bytes"
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"

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
				Waiting: []operator.WaitingSnapshot{
					{IssueIdentifier: "PMR-77", IssueState: "Merging", Reason: "at_capacity", Since: now.Add(-10 * time.Minute), WaitingMS: 600000},
					{IssueIdentifier: "PMR-90", IssueState: "Todo", Reason: "blocked_by_relation", BlockedBy: []string{"PMR-70"}, Since: now.Add(-5 * time.Minute), WaitingMS: 300000},
				},
			},
		},
		RecentLog: []operator.LogEvent{{Time: now, Level: "INFO", Message: "issue claimed"}},
	}
	model := New([]operator.Instance{instance}, now)
	model, _ = model.Update("enter")
	view := model.View(now)
	for _, want := range []string{"PMR-75 (In Progress)", "turns 4/20", "tokens: input 12, output 3, total 15", "waiting: mcpToolCall github_pr_context", "Retry PMR-76", "Waiting PMR-77 (Merging)", "Waiting PMR-90 (Todo): blocked by PMR-70", "Recent redacted lifecycle activity", "issue claimed"} {
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
	if view.model.width != 120 || view.model.height != 40 {
		t.Fatalf("recorded %dx%d, want 120x40", view.model.width, view.model.height)
	}
}

// styledFixture builds a styled model wide enough that no panel wraps, so these
// tests exercise styling rather than line breaking. It stays below splitWidth so
// that they exercise the drill-down layout; splitFixture covers the other side.
func styledFixture(instances []operator.Instance, now time.Time) Model {
	model := New(instances, now)
	model.layout = true
	model.color = true
	model.width = 100
	return model
}

// splitFixture builds a styled model wide enough for the side-by-side layout.
func splitFixture(instances []operator.Instance, now time.Time) Model {
	model := styledFixture(instances, now)
	model.width = 130
	return model
}

// The dashboard lays the same fields out as tables, so a cell boundary now
// separates values the prose ran together. The plain renderer keeps the old
// wording and its own tests still guard it; what must not change here is which
// fields appear and which never do.
func TestStyledDetailPagesTabulateTheSameFields(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	instance := operator.Instance{
		ID:       "com.pmrrasmussen.symphony",
		Liveness: operator.LivenessRunning,
		Config:   &operator.EffectiveConfig{MaxTurns: 20, Credentials: operator.Credentials{Tracker: operator.CredentialPresence{Configured: true, EnvironmentNames: []string{"SECRET_ENV"}}}},
		Snapshot: &operator.Snapshot{Coordinator: operator.RuntimeSnapshot{Running: []operator.RunningSnapshot{{
			IssueIdentifier: "PMR-75",
			IssueState:      "In Progress",
			TurnCount:       4,
			Usage:           operator.Usage{InputTokens: 12, OutputTokens: 3, TotalTokens: 15},
		}}}},
	}
	model := styledFixture([]operator.Instance{instance}, now)
	model.page = statusPage
	status := model.View(now)
	for _, want := range []string{"ISSUE", "STATE", "TURNS", "TOKENS", "PMR-75", "In Progress", "4/20", "15"} {
		if !strings.Contains(status, want) {
			t.Fatalf("styled status page lost %q:\n%s", want, status)
		}
	}

	model.page = configPage
	config := model.View(now)
	for _, want := range []string{"Credentials", "Linear", "GitHub", "configured", "not configured"} {
		if !strings.Contains(config, want) {
			t.Fatalf("styled config page lost %q:\n%s", want, config)
		}
	}
	// Tabulating the credentials must not become a new way to leak the
	// reference behind them.
	if strings.Contains(config, "SECRET_ENV") {
		t.Fatalf("styled config page exposed a credential reference:\n%s", config)
	}
	if !strings.Contains(config, "\x1b") {
		t.Fatal("styled frame carries no styling at all")
	}
}

func TestSplitLayoutAppearsOnlyFromItsThreshold(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	instances := []operator.Instance{{ID: "one", Liveness: operator.LivenessRunning}}
	for _, testCase := range []struct {
		width int
		split bool
	}{
		{width: 0, split: false},
		{width: 100, split: false},
		{width: splitWidth - 1, split: false},
		{width: splitWidth, split: true},
		{width: 200, split: true},
	} {
		model := styledFixture(instances, now)
		model.width = testCase.width
		if model.splitLayout() != testCase.split {
			t.Fatalf("width %d: split=%v, want %v", testCase.width, model.splitLayout(), testCase.split)
		}
		// The hint bar is the honest signal: the split layout quits outright,
		// the drill-down backs out first.
		view := model.View(now)
		wantHint := "q quit"
		if !testCase.split && model.page != overviewPage {
			wantHint = "q back"
		}
		if !strings.Contains(view, wantHint) {
			t.Fatalf("width %d: hint bar missing %q:\n%s", testCase.width, wantHint, view)
		}
	}
}

func TestSplitLayoutQuitsWithoutBackingOutFirst(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	model := splitFixture([]operator.Instance{{ID: "one", Liveness: operator.LivenessRunning}}, now)

	// Enter has nothing to open: the page it would drill into is already beside
	// the list.
	moved, quit := model.Update("enter")
	if quit || moved.page != overviewPage {
		t.Fatalf("enter changed the split layout: page=%v quit=%v", moved.page, quit)
	}

	// The detail keys still work without drilling in first.
	moved, _ = model.Update("c")
	if moved.page != configPage {
		t.Fatalf("c did not reach Config in the split layout: page=%v", moved.page)
	}
	moved, _ = moved.Update("tab")
	if moved.page != validationPage {
		t.Fatalf("Tab did not advance in the split layout: page=%v", moved.page)
	}
	// One q, not two: there is no overview to return to.
	if _, quit = moved.Update("q"); !quit {
		t.Fatal("q on a split detail page did not quit")
	}
	if _, quit = model.Update("q"); !quit {
		t.Fatal("q on the split layout did not quit")
	}
}

func TestSplitLayoutShowsTheListBesideTheSelectedInstance(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	model := splitFixture([]operator.Instance{
		{ID: "com.pmrrasmussen.symphony", Liveness: operator.LivenessRunning},
		{ID: "com.pmrrasmussen.symphony.testing-grounds", Liveness: operator.LivenessStopped},
	}, now)
	model.selected = 1
	view := model.View(now)
	// Both halves, on the same rows: the list on the left and the selected
	// instance's Status on the right.
	for _, want := range []string{"INSTANCE", "▸", "│", "com.pmrrasmussen.symphony.testing-grounds"} {
		if !strings.Contains(view, want) {
			t.Fatalf("split frame missing %q:\n%s", want, view)
		}
	}
	// The frame must still fit the window it was given.
	for _, line := range strings.Split(strings.TrimRight(view, "\n"), "\n") {
		if lipgloss.Width(line) > model.width {
			t.Fatalf("split frame line is %d columns wide in a %d-column window:\n%s",
				lipgloss.Width(line), model.width, view)
		}
	}
}

func TestValidationPageWordsSeverityAsWellAsColoringIt(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	model := styledFixture([]operator.Instance{{
		ID:       "one",
		Liveness: operator.LivenessInvalid,
		Findings: []operator.Finding{
			{Severity: operator.SeverityError, Code: "workflow", Message: "unreadable"},
			{Severity: operator.SeverityWarning, Code: "logs", Message: "no log directory yet"},
		},
	}}, now)
	model.page = validationPage
	view := model.View(now)
	// Color alone would be invisible to a red-green colorblind reader, so the
	// severity is spelled out in its own column.
	for _, want := range []string{"SEVERITY", "CODE", "MESSAGE", "ERROR", "WARNING", "workflow", "unreadable", "INVALID"} {
		if !strings.Contains(view, want) {
			t.Fatalf("validation page lost %q:\n%s", want, view)
		}
	}
}

func TestZeroTimestampsSayUnknownRatherThanMillionsOfHours(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	if got := formatSince(now, time.Time{}); got != "unknown" {
		t.Fatalf("formatSince on a zero time returned %q", got)
	}
	if got := formatSince(now, now.Add(-90*time.Second)); got != "1m" {
		t.Fatalf("formatSince returned %q, want 1m", got)
	}
	// A snapshot that carried no start time used to render 2562047h47m.
	model := styledFixture([]operator.Instance{{
		ID:       "one",
		Liveness: operator.LivenessRunning,
		Snapshot: &operator.Snapshot{Coordinator: operator.RuntimeSnapshot{Running: []operator.RunningSnapshot{{
			IssueIdentifier: "PMR-75", IssueState: "In Progress", TurnCount: 1,
		}}}},
	}}, now)
	model.page = statusPage
	if view := model.View(now); strings.Contains(view, "2562047h") {
		t.Fatalf("status page rendered a zero timestamp as a duration:\n%s", view)
	}
}

func TestStyledOverviewMarksSelectionAndShortensStale(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	model := styledFixture([]operator.Instance{
		{ID: "alpha", Liveness: operator.LivenessRunning},
		{ID: "beta", Liveness: operator.LivenessStale},
	}, now)
	model.selected = 1
	view := model.View(now)
	if !strings.Contains(view, "▸") {
		t.Fatalf("no selection marker:\n%s", view)
	}
	// The raw value is fourteen characters and used to overflow the column.
	if strings.Contains(view, string(operator.LivenessStale)) {
		t.Fatalf("overview printed the raw stale value:\n%s", view)
	}
	if !strings.Contains(view, "stale") {
		t.Fatalf("overview lost the stale state:\n%s", view)
	}
}

func TestNarrowWindowKeepsEveryColumnHeader(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	model := styledFixture([]operator.Instance{
		{ID: "com.pmrrasmussen.symphony", Liveness: operator.LivenessRunning},
		{ID: "com.pmrrasmussen.symphony.acme-web", Liveness: operator.LivenessStopped},
	}, now)
	model.width = 80
	view := model.View(now)
	for _, header := range []string{"INSTANCE", "STATE", "AGENTS", "RETRIES", "CHECKS"} {
		if !strings.Contains(view, header) {
			t.Fatalf("80 columns truncated the %s header:\n%s", header, view)
		}
	}
}

func TestTruncateCutsOnRuneBoundaries(t *testing.T) {
	// Byte slicing split the last character and mismeasured the width.
	if got := truncate("aaaa", 3); got != "aa…" {
		t.Fatalf("truncate(ascii)=%q, want %q", got, "aa…")
	}
	got := truncate("ααααα", 3)
	if got != "αα…" {
		t.Fatalf("truncate(multibyte)=%q, want %q", got, "αα…")
	}
	if !utf8.ValidString(got) {
		t.Fatalf("truncate produced invalid UTF-8: %q", got)
	}
	if got := truncate("short", 10); got != "short" {
		t.Fatalf("truncate widened a short value to %q", got)
	}
}

func TestWindowKeepsTheSelectionVisible(t *testing.T) {
	items := []string{"a", "b", "c", "d", "e"}
	// A marker on a row the user cannot see would be worse than a short list,
	// so the window follows the selection rather than pinning to the top.
	shown, hidden := window(items, 4, 3)
	if strings.Join(shown, "") != "de" || hidden != 3 {
		t.Fatalf("shown=%v hidden=%d, want [d e] and 3", shown, hidden)
	}
	shown, hidden = window(items, 0, 3)
	if strings.Join(shown, "") != "ab" || hidden != 3 {
		t.Fatalf("shown=%v hidden=%d, want [a b] and 3", shown, hidden)
	}
}

func TestWidthBandsDropTheNumericColumnsFirst(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	instances := []operator.Instance{
		{ID: "com.pmrrasmussen.symphony", Liveness: operator.LivenessRunning},
		{ID: "com.pmrrasmussen.symphony.acme-web", Liveness: operator.LivenessStopped},
	}
	for _, testCase := range []struct {
		width   int
		numeric bool
	}{
		{width: 119, numeric: true},
		{width: 100, numeric: true},
		{width: 80, numeric: true},
		{width: 70, numeric: false},
		{width: 60, numeric: false},
	} {
		model := styledFixture(instances, now)
		model.width = testCase.width
		view := model.View(now)
		for _, header := range []string{"INSTANCE", "STATE", "CHECKS"} {
			if !strings.Contains(view, header) {
				t.Fatalf("width %d dropped the load-bearing %s column:\n%s", testCase.width, header, view)
			}
		}
		for _, header := range []string{"AGENTS", "RETRIES"} {
			if strings.Contains(view, header) != testCase.numeric {
				t.Fatalf("width %d: %s present=%v, want %v:\n%s", testCase.width, header, !testCase.numeric, testCase.numeric, view)
			}
		}
	}
}

func TestTooSmallWindowNamesWhatItNeeds(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	for _, testCase := range []struct{ width, height int }{
		{width: 58, height: 40},
		{width: 100, height: 12},
		{width: 40, height: 10},
	} {
		model := styledFixture([]operator.Instance{{ID: "one"}}, now)
		model.width, model.height = testCase.width, testCase.height
		view := model.View(now)
		if !strings.Contains(view, "terminal too small: need 60x14") {
			t.Fatalf("%dx%d drew a frame instead of naming its minimum:\n%s", testCase.width, testCase.height, view)
		}
	}
	// At the declared minimum the dashboard must actually render.
	model := styledFixture([]operator.Instance{{ID: "one"}}, now)
	model.width, model.height = 60, 14
	if view := model.View(now); strings.Contains(view, "terminal too small") {
		t.Fatalf("60x14 is the declared minimum but refused to draw:\n%s", view)
	}
}

// colorSequence matches an SGR foreground-color parameter. Weight and dimming
// are not color and are expected to survive NO_COLOR.
var colorSequence = regexp.MustCompile(`\x1b\[[0-9;]*(3[0-7]|9[0-7])(;[0-9]+)*m`)

func TestNoColorKeepsTheDashboardWithoutHue(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	instances := []operator.Instance{{ID: "one", Liveness: operator.LivenessRunning}}

	colored := styledFixture(instances, now).View(now)
	if !colorSequence.MatchString(colored) {
		t.Fatalf("the colored frame carries no color at all:\n%q", colored)
	}

	model := styledFixture(instances, now)
	model.color = false
	mono := model.View(now)
	if colorSequence.MatchString(mono) {
		t.Fatalf("NO_COLOR frame still emits color:\n%q", mono)
	}
	// NO_COLOR gives up hue, not the dashboard: the rules, the table and the
	// state glyph all have to survive, because they are what carries the
	// meaning once color is gone.
	for _, want := range []string{"─", "╭", "▸", "● running", "INSTANCE"} {
		if !strings.Contains(mono, want) {
			t.Fatalf("NO_COLOR frame lost %q:\n%s", want, mono)
		}
	}
}

func TestEachLivenessHasItsOwnGlyph(t *testing.T) {
	seen := map[string]operator.Liveness{}
	for _, state := range []operator.Liveness{
		operator.LivenessRunning,
		operator.LivenessStopped,
		operator.LivenessStale,
		operator.LivenessInvalid,
	} {
		glyph := livenessGlyph(state)
		if other, clash := seen[glyph]; clash {
			t.Fatalf("%s and %s share the glyph %q, so the shape marks nothing", state, other, glyph)
		}
		seen[glyph] = state
	}
}

// logFixture is a styled model whose Status page is taller than its window,
// which is the case PMR-88 is about.
func logFixture(t *testing.T, entries int, height int) Model {
	t.Helper()
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	log := make([]operator.LogEvent, 0, entries)
	for index := range entries {
		log = append(log, operator.LogEvent{Time: now, Level: "INFO", Message: fmt.Sprintf("event %d", index)})
	}
	model := styledFixture([]operator.Instance{{
		ID:        "com.pmrrasmussen.symphony",
		Liveness:  operator.LivenessRunning,
		Snapshot:  &operator.Snapshot{State: "running"},
		RecentLog: log,
	}}, now)
	model.height = height
	model.page = statusPage
	return model
}

func TestTallDetailBodyScrollsInsteadOfBeingCutOff(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	model := logFixture(t, 20, 20)
	view := model.View(now)

	// A position, not a bare count: the reader has to be able to tell whether
	// there is more and which way.
	if !strings.Contains(view, "of") || !strings.Contains(view, "▾") {
		t.Fatalf("no position indicator on an overflowing page:\n%s", view)
	}
	if strings.Contains(view, "▴") {
		t.Fatalf("indicator claims content above while at the top:\n%s", view)
	}
	// The frame must still fit the window it was given.
	if height := lipgloss.Height(view); height > model.height+1 {
		t.Fatalf("frame is %d rows in a %d-row window:\n%s", height, model.height, view)
	}
	if !strings.Contains(view, "q back") {
		t.Fatalf("scrolled frame pushed the hint bar off screen:\n%s", view)
	}
}

func TestScrollingReachesTheLastLine(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	model := logFixture(t, 20, 20)

	// The whole point of the issue: the alternate screen has no scrollback, so
	// the final entry has to be reachable from inside the view.
	if strings.Contains(model.View(now), "event 19") {
		t.Fatal("fixture is not tall enough to exercise scrolling")
	}
	bottom, _ := model.Update("G")
	view := bottom.View(now)
	if !strings.Contains(view, "event 19") {
		t.Fatalf("G did not reach the last line:\n%s", view)
	}
	if !strings.Contains(view, "▴") {
		t.Fatalf("indicator does not report content above at the bottom:\n%s", view)
	}
	if strings.Contains(view, "▾") {
		t.Fatalf("indicator still claims content below at the bottom:\n%s", view)
	}
	back, _ := bottom.Update("g")
	if back.offset != 0 {
		t.Fatalf("g did not return to the top: offset=%d", back.offset)
	}
}

func TestScrollKeysStepHalfAScreenAndStopAtTheTop(t *testing.T) {
	model := logFixture(t, 40, 21)
	step := model.scrollStep()
	if step < 1 {
		t.Fatalf("scroll step is %d", step)
	}
	down, _ := model.Update("ctrl+d")
	if down.offset != step {
		t.Fatalf("ctrl+d moved to %d, want %d", down.offset, step)
	}
	paged, _ := down.Update("pgdown")
	if paged.offset != 2*step {
		t.Fatalf("pgdown moved to %d, want %d", paged.offset, 2*step)
	}
	up, _ := paged.Update("ctrl+u")
	if up.offset != step {
		t.Fatalf("ctrl+u moved to %d, want %d", up.offset, step)
	}
	// Scrolling up at the top must not go negative.
	top, _ := up.Update("ctrl+u")
	top, _ = top.Update("ctrl+u")
	if top.offset != 0 {
		t.Fatalf("scrolling past the top reached %d", top.offset)
	}
}

func TestDriverClampsAScrollThatRanPastTheEnd(t *testing.T) {
	// G sets an offset past any content; the frame knows the real length, so the
	// driver corrects it and scrolling back responds on the first keypress.
	view := newTestApp([]operator.Instance{{ID: "one", Liveness: operator.LivenessRunning,
		Snapshot: &operator.Snapshot{State: "running"},
		RecentLog: []operator.LogEvent{
			{Time: time.Now(), Level: "INFO", Message: "one"},
			{Time: time.Now(), Level: "INFO", Message: "two"},
		}}}, nil)
	view.model.layout, view.model.color = true, true
	view.model.width, view.model.height = 100, 14
	view.model.page = statusPage
	view.model.offset = scrollToEnd
	view.View()
	if view.model.offset >= scrollToEnd {
		t.Fatalf("driver left the offset past the end: %d", view.model.offset)
	}
}

func TestOffsetResetsWhenTheContentBehindItChanges(t *testing.T) {
	model := logFixture(t, 40, 20)
	scrolled, _ := model.Update("ctrl+d")
	if scrolled.offset == 0 {
		t.Fatal("fixture did not scroll")
	}
	// A different page and a different instance are different content, so the
	// viewport starts at the top rather than part way down someone else's page.
	if moved, _ := scrolled.Update("c"); moved.offset != 0 {
		t.Fatalf("changing page kept offset %d", moved.offset)
	}
	// A second instance, so that j has somewhere to move to.
	twoUp := scrolled
	twoUp.instances = append(append([]operator.Instance(nil), scrolled.instances...),
		operator.Instance{ID: "com.pmrrasmussen.symphony.other", Liveness: operator.LivenessStopped})
	if moved, _ := twoUp.Update("j"); moved.selected != 1 || moved.offset != 0 {
		t.Fatalf("changing instance left selected=%d offset=%d", moved.selected, moved.offset)
	}
	// Refreshing in place must not throw away the reader's position.
	held := scrolled
	held.Refresh(held.instances, nil, time.Now())
	if held.offset != scrolled.offset {
		t.Fatalf("refresh reset the offset from %d to %d", scrolled.offset, held.offset)
	}
}

func TestScrollKeysAreInertOnTheOverview(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	model := styledFixture([]operator.Instance{{ID: "one", Liveness: operator.LivenessRunning}}, now)
	model.height = 20
	for _, key := range []string{"ctrl+d", "ctrl+u", "g", "G", "pgdown", "pgup"} {
		if moved, quit := model.Update(key); moved.offset != 0 || quit {
			t.Fatalf("%s scrolled the overview: offset=%d quit=%v", key, moved.offset, quit)
		}
	}
}

func TestRedirectedDetailIsNeverScrolled(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	model := logFixture(t, 20, 20)
	// The redirected surface has no window to fit and its frames are read by
	// pipes, so it prints every line and no indicator.
	model.layout, model.color = false, false
	view := model.View(now)
	for index := range 20 {
		if want := fmt.Sprintf("event %d", index); !strings.Contains(view, want) {
			t.Fatalf("redirected detail dropped %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "▾") || strings.Contains(view, "ctrl+d") {
		t.Fatalf("redirected detail carries a scroll indicator:\n%s", view)
	}
}
