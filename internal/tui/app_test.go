package tui

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/pmrrasmussen/symphony/internal/operator"
	"github.com/pmrrasmussen/symphony/internal/status"
)

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

// TestTickSkipsASweepThatIsStillInFlight covers the pile-up an unguarded timer
// causes: a sweep slower than the refresh interval would otherwise have a
// second, third, and fourth sweep started on top of it, each repeating the
// probes the first is already stuck on.
func TestTickSkipsASweepThatIsStillInFlight(t *testing.T) {
	var calls int
	discover := func(context.Context, operator.Options) ([]operator.Instance, error) {
		calls++
		return []operator.Instance{{ID: "alpha"}}, nil
	}
	view := newTestApp(nil, discover)
	_, command := view.Update(tickMsg(time.Now()))
	batch, ok := command().(tea.BatchMsg)
	if !ok {
		t.Fatalf("first tick produced %T, want a batch of discovery and the next tick", command())
	}
	if _, second := view.Update(tickMsg(time.Now())); second == nil {
		t.Fatal("the skipped tick did not re-arm the timer")
	}
	if view.dispatched != 1 {
		t.Fatalf("%d sweeps dispatched, want the second tick skipped while one was in flight", view.dispatched)
	}
	// Once the outstanding sweep lands, the next tick sweeps again.
	view.Update(batch[0]())
	if calls != 1 {
		t.Fatalf("discovery calls=%d, want 1", calls)
	}
	if _, third := view.Update(tickMsg(time.Now())); third == nil {
		t.Fatal("the tick after the sweep landed produced no command")
	}
	if view.dispatched != 2 {
		t.Fatalf("%d sweeps dispatched after the first landed, want 2", view.dispatched)
	}
}

// TestStaleSweepCannotOverwriteANewerFrame covers results arriving out of order,
// which an explicit refresh over a slow timed sweep can produce. The older
// result must be dropped rather than replace the frame and be stamped with the
// time it happened to land.
func TestStaleSweepCannotOverwriteANewerFrame(t *testing.T) {
	results := [][]operator.Instance{{{ID: "old"}}, {{ID: "new-one"}, {ID: "new-two"}}}
	var call int
	discover := func(context.Context, operator.Options) ([]operator.Instance, error) {
		instances := results[call]
		call++
		return instances, nil
	}
	view := newTestApp(nil, discover)
	_, first := view.Update(tea.KeyPressMsg{Code: 'r', Text: "r"})
	_, second := view.Update(tea.KeyPressMsg{Code: 'r', Text: "r"})
	older, newer := first().(discoveredMsg), second().(discoveredMsg)
	if older.sweep >= newer.sweep {
		t.Fatalf("sweep stamps are not ordered: %d then %d", older.sweep, newer.sweep)
	}
	view.Update(newer)
	updatedAt := view.model.updatedAt
	view.Update(older)
	if len(view.model.instances) != 2 || view.model.instances[0].ID != "new-one" {
		t.Fatalf("the older sweep replaced the frame: %#v", view.model.instances)
	}
	if !view.model.updatedAt.Equal(updatedAt) {
		t.Fatal("the older sweep restamped the frame as freshly read")
	}
}

// TestOnlyAnExplicitRefreshReprobes pins which sweep pays for the agent CLI
// probes: the timed one reuses the cache the view holds for its whole lifetime,
// and only `r` asks for the probes again.
func TestOnlyAnExplicitRefreshReprobes(t *testing.T) {
	var seen []operator.Options
	discover := func(_ context.Context, options operator.Options) ([]operator.Instance, error) {
		seen = append(seen, options)
		return nil, nil
	}
	view := newTestApp(nil, discover)
	view.options = operator.Options{Preflight: &operator.PreflightCache{}}
	_, tick := view.Update(tickMsg(time.Now()))
	tick().(tea.BatchMsg)[0]()
	view.Update(discoveredMsg{sweep: view.dispatched})
	_, refresh := view.Update(tea.KeyPressMsg{Code: 'r', Text: "r"})
	refresh()
	if len(seen) != 2 {
		t.Fatalf("%d sweeps ran, want the tick and the refresh", len(seen))
	}
	if seen[0].RefreshPreflight {
		t.Fatal("the timed sweep asked for the agent CLI probes again")
	}
	if !seen[1].RefreshPreflight {
		t.Fatal("the explicit refresh did not ask for the agent CLI probes again")
	}
	if seen[0].Preflight == nil || seen[0].Preflight != seen[1].Preflight {
		t.Fatalf("sweeps did not share one probe cache: %p and %p", seen[0].Preflight, seen[1].Preflight)
	}
}

// TestRunCancelsSweepsOnItsWayOut covers the context discovery now threads into
// the probes: a sweep outlives the view that dispatched it, and must be told the
// view is gone rather than left holding a keychain prompt for nobody.
func TestRunCancelsSweepsOnItsWayOut(t *testing.T) {
	var sweepCtx context.Context
	discover := func(ctx context.Context, options operator.Options) ([]operator.Instance, error) {
		sweepCtx = ctx
		if options.Preflight == nil {
			t.Error("sweep ran without a probe cache")
		}
		return nil, nil
	}
	if err := Run(context.Background(), strings.NewReader("q\n"), &bytes.Buffer{}, discover); err != nil {
		t.Fatal(err)
	}
	if sweepCtx == nil {
		t.Fatal("no sweep ran")
	}
	if sweepCtx.Err() == nil {
		t.Fatal("the view returned without cancelling its sweeps")
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

func TestDriverClampsAScrollThatRanPastTheEnd(t *testing.T) {
	// G sets an offset past any content; the frame knows the real length, so the
	// driver corrects it and scrolling back responds on the first keypress.
	view := newTestApp([]operator.Instance{{ID: "one", Liveness: operator.LivenessRunning,
		Snapshot: &operator.Snapshot{Snapshot: status.Snapshot{State: status.Running}},
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
