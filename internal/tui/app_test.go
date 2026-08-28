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
