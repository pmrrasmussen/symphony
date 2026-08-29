package tui

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/pmrrasmussen/symphony/internal/operator"
	"github.com/pmrrasmussen/symphony/internal/status"
)

func TestWindowKeepsTheSelectionVisible(t *testing.T) {
	items := []string{"a", "b", "c", "d", "e"}
	// A marker on a row the user cannot see would be worse than a short list,
	// so the window follows the selection rather than pinning to the top.
	shown, start, hidden := window(items, 4, 3)
	if strings.Join(shown, "") != "de" || start != 3 || hidden != 3 {
		t.Fatalf("shown=%v start=%d hidden=%d, want [d e], 3 and 3", shown, start, hidden)
	}
	shown, start, hidden = window(items, 0, 3)
	if strings.Join(shown, "") != "ab" || start != 0 || hidden != 3 {
		t.Fatalf("shown=%v start=%d hidden=%d, want [a b], 0 and 3", shown, start, hidden)
	}
	// An unclipped window starts where the slice does, so a caller that rebases
	// a position on start leaves it untouched.
	shown, start, hidden = window(items, 4, 0)
	if strings.Join(shown, "") != "abcde" || start != 0 || hidden != 0 {
		t.Fatalf("shown=%v start=%d hidden=%d, want the whole slice, 0 and 0", shown, start, hidden)
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

// TestServiceStateHasItsOwnStyle pins PMR-119: a draining daemon's
// "stopping" state must not read the same as "running" or "stopped" on the
// styled Status page, since that distinction is exactly what an operator
// opens the TUI during a restart to see.
func TestServiceStateHasItsOwnStyle(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	style := newTheme(true, true, 100, 0)
	seen := map[string]string{}
	for _, state := range []string{"running", "stopping", "stopped"} {
		rendered := style.serviceStyle(state).Render(state)
		if other, clash := seen[rendered]; clash {
			t.Fatalf("%q and %q render identically (%q), so the distinction carries no color", state, other, rendered)
		}
		seen[rendered] = state

		model := styledFixture([]operator.Instance{{
			ID:       "com.pmrrasmussen.symphony",
			Liveness: operator.LivenessRunning,
			Snapshot: &operator.Snapshot{Snapshot: status.Snapshot{State: status.ProcessState(state)}},
		}}, now)
		model.page = statusPage
		view := model.View(now)
		if !strings.Contains(view, state) {
			t.Fatalf("styled status page lost service state %q:\n%s", state, view)
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
