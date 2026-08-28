package tui

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/pmrrasmussen/symphony/internal/coordinator"
	"github.com/pmrrasmussen/symphony/internal/operator"
	"github.com/pmrrasmussen/symphony/internal/status"
)

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
		Snapshot: &operator.Snapshot{Snapshot: status.Snapshot{Coordinator: coordinator.Snapshot{Running: []coordinator.RunningSnapshot{{
			IssueIdentifier: "PMR-75", IssueState: "In Progress", TurnCount: 1,
		}}}}},
	}}, now)
	model.page = statusPage
	if view := model.View(now); strings.Contains(view, "2562047h") {
		t.Fatalf("status page rendered a zero timestamp as a duration:\n%s", view)
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
