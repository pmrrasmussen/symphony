package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pmrrasmussen/symphony/internal/coordinator"
	"github.com/pmrrasmussen/symphony/internal/domain"
	"github.com/pmrrasmussen/symphony/internal/operator"
	"github.com/pmrrasmussen/symphony/internal/status"
)

func TestStatusViewRendersOnlySafeRuntimeFields(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	instance := operator.Instance{
		ID:       "com.pmrrasmussen.symphony.repo",
		Liveness: operator.LivenessRunning,
		Launchd:  operator.LaunchdStatus{Loaded: true, Process: true, PID: 45},
		Config:   &operator.EffectiveConfig{MaxTurns: 20},
		Snapshot: &operator.Snapshot{
			UpdatedAt: now.Add(-time.Second),
			Snapshot: status.Snapshot{
				State: "running", StartedAt: now.Add(-2 * time.Hour),
				Coordinator: coordinator.Snapshot{
					Claimed: 1,
					Running: []coordinator.RunningSnapshot{{
						IssueIdentifier: "PMR-75", IssueState: "In Progress", StartedAt: now.Add(-time.Hour), LastEventAt: now.Add(-time.Minute), TurnCount: 4,
						Usage: domain.Usage{InputTokens: 12, OutputTokens: 3, TotalTokens: 15}, RateLimit: map[string]int64{"remaining": 9},
						OutstandingOperation: &coordinator.OutstandingOperationSnapshot{Type: "mcpToolCall", Name: "github_pr_context", AgeMS: 3000},
					}},
					Retrying: []coordinator.RetrySnapshot{{IssueIdentifier: "PMR-76", Attempt: 2, Kind: "retry", Reason: "timeout", Due: now.Add(time.Minute)}},
					Waiting: []coordinator.WaitingSnapshot{
						{IssueIdentifier: "PMR-77", IssueState: "Merging", Reason: "at_capacity", Since: now.Add(-10 * time.Minute), WaitingMS: 600000},
						{IssueIdentifier: "PMR-90", IssueState: "Todo", Reason: "blocked_by_relation", BlockedBy: []string{"PMR-70"}, Since: now.Add(-5 * time.Minute), WaitingMS: 300000},
					},
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

func TestConfigViewRendersOnlyTheSelectedBackendSettings(t *testing.T) {
	now := time.Now()
	for _, testCase := range []struct {
		backend, want string
		config        operator.EffectiveConfig
		absent        []string
	}{
		{backend: "codex", want: "\nCodex:\nCommand: codex app-server\nTimeouts: turn 1h0m0s; read 5s; start 2m0s; stall 5m0s\nApproval policy: never\nThread sandbox: workspace-write\n", config: operator.EffectiveConfig{AgentBackend: "codex", CodexCommand: "codex app-server", CodexApprovalPolicy: "never", CodexThreadSandbox: "workspace-write", TurnTimeout: time.Hour, ReadTimeout: 5 * time.Second, StartTimeout: 2 * time.Minute, StallTimeout: 5 * time.Minute}, absent: []string{"Model:"}},
		{backend: "claude", want: "\nClaude:\nCommand: claude\nModel: sonnet\nTimeouts: turn 30m0s; stall 5m0s\n", config: operator.EffectiveConfig{AgentBackend: "claude", ClaudeCommand: "claude", ClaudeModel: "sonnet", TurnTimeout: 30 * time.Minute, StallTimeout: 5 * time.Minute}, absent: []string{"Approval policy:", "Thread sandbox:", "read 0s", "start 0s"}},
		// A snapshot written before backend selection existed reports no
		// backend, which must still render a label.
		{backend: "", want: "\nAgent:\nCommand: codex app-server\n", config: operator.EffectiveConfig{CodexCommand: "codex app-server"}},
	} {
		instance := operator.Instance{ID: "safe", Config: &testCase.config}
		model := New([]operator.Instance{instance}, now)
		model, _ = model.Update("enter")
		model, _ = model.Update("c")
		view := model.View(now)
		if !strings.Contains(view, testCase.want) {
			t.Fatalf("backend %q view missing %q:\n%s", testCase.backend, testCase.want, view)
		}
		for _, forbidden := range testCase.absent {
			if strings.Contains(view, forbidden) {
				t.Fatalf("backend %q view exposed %q:\n%s", testCase.backend, forbidden, view)
			}
		}
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
