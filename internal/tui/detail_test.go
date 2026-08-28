package tui

import (
	"strings"
	"testing"
	"time"

	lipgloss "charm.land/lipgloss/v2"

	"github.com/pmrrasmussen/symphony/internal/coordinator"
	"github.com/pmrrasmussen/symphony/internal/domain"
	"github.com/pmrrasmussen/symphony/internal/operator"
	"github.com/pmrrasmussen/symphony/internal/status"
)

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
		Snapshot: &operator.Snapshot{Snapshot: status.Snapshot{Coordinator: coordinator.Snapshot{Running: []coordinator.RunningSnapshot{{
			IssueIdentifier: "PMR-75",
			IssueState:      "In Progress",
			TurnCount:       4,
			Usage:           domain.Usage{InputTokens: 12, OutputTokens: 3, TotalTokens: 15},
		}}}}},
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

// TestDetailPagesShowPerIssueCostBesideTheRunFigure covers PMR-151's operator
// surface. The two figures are deliberately different here: a run that has
// spent 15 tokens on an issue that has spent 750 across its attempts is the
// whole point -- the per-run number alone makes an issue on its thirty-eighth
// attempt look as cheap as one on its first. Both renderers are checked,
// because the plain one is the scriptable surface and a field that exists on
// only one of them is the drift this dashboard keeps having to undo.
func TestDetailPagesShowPerIssueCostBesideTheRunFigure(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	snapshot := &operator.Snapshot{Snapshot: status.Snapshot{Coordinator: coordinator.Snapshot{
		Running: []coordinator.RunningSnapshot{{
			IssueIdentifier: "PMR-75",
			IssueState:      "In Progress",
			TurnCount:       4,
			Usage:           domain.Usage{InputTokens: 12, OutputTokens: 3, TotalTokens: 15},
			IssueUsage:      domain.Usage{InputTokens: 600, OutputTokens: 150, TotalTokens: 750},
		}},
		// A queued retry holds no run at all, so its per-issue total is the
		// only cost the row can carry.
		Retrying: []coordinator.RetrySnapshot{{
			IssueIdentifier: "PMR-76", Attempt: 38, Kind: "agent", Reason: "agent_event",
			Due: now.Add(time.Minute), IssueUsage: domain.Usage{TotalTokens: 4200},
		}},
	}}}
	instance := operator.Instance{
		ID:       "com.pmrrasmussen.symphony",
		Liveness: operator.LivenessRunning,
		Config:   &operator.EffectiveConfig{MaxTurns: 20},
		Snapshot: snapshot,
	}

	styled := styledFixture([]operator.Instance{instance}, now)
	styled.page = statusPage
	view := styled.View(now)
	for _, want := range []string{"ISSUE TOKENS", "750", "4200"} {
		if !strings.Contains(view, want) {
			t.Fatalf("styled status page lost %q:\n%s", want, view)
		}
	}

	plain := New([]operator.Instance{instance}, now)
	plain, _ = plain.Update("enter")
	text := plain.View(now)
	for _, want := range []string{"tokens: input 12, output 3, total 15", "issue tokens: input 600, output 150, total 750", "issue tokens 4200"} {
		if !strings.Contains(text, want) {
			t.Fatalf("plain status view missing %q:\n%s", want, text)
		}
	}
}
