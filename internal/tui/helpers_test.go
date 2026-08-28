package tui

import (
	"fmt"
	"testing"
	"time"

	"github.com/pmrrasmussen/symphony/internal/operator"
	"github.com/pmrrasmussen/symphony/internal/status"
)

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
		Snapshot:  &operator.Snapshot{Snapshot: status.Snapshot{State: status.Running}},
		RecentLog: log,
	}}, now)
	model.height = height
	model.page = statusPage
	return model
}
