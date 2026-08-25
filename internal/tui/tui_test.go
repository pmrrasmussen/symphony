package tui

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

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
		ProjectSelector: "project", WorkspaceSource: "/repo", WorkspaceRoot: "/work", CodexCommand: "codex app-server", CodexApprovalPolicy: "never", CodexThreadSandbox: "workspace-write",
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
