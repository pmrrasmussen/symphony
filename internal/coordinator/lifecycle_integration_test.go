package coordinator

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/domain"
	localworkspace "github.com/pmrrasmussen/symphony/internal/workspace"
)

// These tests deliberately use the production local workspace executor. They
// cover the durable state boundary that coordinator unit-test doubles cannot.
type lifecycleTracker struct {
	mu    sync.Mutex
	issue domain.Issue
}

func (t *lifecycleTracker) ListCandidates(context.Context, []string) ([]domain.Issue, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return []domain.Issue{t.issue}, nil
}

func (t *lifecycleTracker) GetIssues(_ context.Context, ids []string) ([]domain.Issue, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, id := range ids {
		if id == t.issue.ID {
			return []domain.Issue{t.issue}, nil
		}
	}
	return nil, nil
}

func (*lifecycleTracker) ListTerminal(context.Context, []string) ([]domain.Issue, error) {
	return nil, nil
}

func (t *lifecycleTracker) setIssue(issue domain.Issue) {
	t.mu.Lock()
	t.issue = issue
	t.mu.Unlock()
}

type lifecycleAgent struct {
	mu        sync.Mutex
	starts    int
	continues int
	cancels   int
	block     bool
	requests  chan domain.AgentRequest
	sent      []domain.EventKind
}

func (a *lifecycleAgent) Start(_ context.Context, request domain.AgentRequest) (domain.AgentSession, <-chan domain.Event, error) {
	a.mu.Lock()
	a.starts++
	block := a.block
	a.mu.Unlock()
	a.requests <- request

	events := make(chan domain.Event, 5)
	if !block {
		for _, kind := range []domain.EventKind{
			domain.EventSessionStarted,
			domain.EventProgress,
			domain.EventUsage,
			domain.EventRateLimit,
			domain.EventCompleted,
		} {
			events <- domain.Event{Kind: kind, SessionID: "session"}
			a.mu.Lock()
			a.sent = append(a.sent, kind)
			a.mu.Unlock()
		}
		close(events)
	}
	return domain.AgentSession{ID: "session", ThreadID: "thread", TurnID: "turn"}, events, nil
}

func (a *lifecycleAgent) Continue(context.Context, domain.AgentSession, string) (<-chan domain.Event, error) {
	a.mu.Lock()
	a.continues++
	a.mu.Unlock()
	events := make(chan domain.Event, 1)
	events <- domain.Event{Kind: domain.EventCompleted, SessionID: "session"}
	close(events)
	return events, nil
}

func (a *lifecycleAgent) Cancel(context.Context, domain.AgentSession) error {
	a.mu.Lock()
	a.cancels++
	a.mu.Unlock()
	return nil
}

func (a *lifecycleAgent) counts() (starts, cancels int, sent []domain.EventKind) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.starts, a.cancels, append([]domain.EventKind(nil), a.sent...)
}

func (a *lifecycleAgent) continuationCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.continues
}

type observingLocalWorkspace struct {
	local    *localworkspace.Local
	marked   chan struct{}
	afterRun chan struct{}
}

func (w *observingLocalWorkspace) ShouldRun(ctx context.Context, issue domain.Issue) (bool, error) {
	return w.local.ShouldRun(ctx, issue)
}

func (w *observingLocalWorkspace) Prepare(ctx context.Context, issue domain.Issue) (domain.Workspace, error) {
	return w.local.Prepare(ctx, issue)
}

func (w *observingLocalWorkspace) BeforeRun(ctx context.Context, workspace domain.Workspace, issue domain.Issue) error {
	return w.local.BeforeRun(ctx, workspace, issue)
}

func (w *observingLocalWorkspace) AfterRun(ctx context.Context, workspace domain.Workspace, issue domain.Issue) {
	w.local.AfterRun(ctx, workspace, issue)
	w.afterRun <- struct{}{}
}

func (w *observingLocalWorkspace) MarkCompleted(ctx context.Context, workspace domain.Workspace, issue domain.Issue) error {
	err := w.local.MarkCompleted(ctx, workspace, issue)
	if err == nil {
		w.marked <- struct{}{}
	}
	return err
}

func (w *observingLocalWorkspace) Cleanup(ctx context.Context, issue domain.Issue) error {
	return w.local.Cleanup(ctx, issue)
}

func (w *observingLocalWorkspace) Execute(ctx context.Context, workspace domain.Workspace, command string, args []string) ([]byte, error) {
	return w.local.Execute(ctx, workspace, command, args)
}

func TestLocalWorkspaceCompletionLifecycleIsDurable(t *testing.T) {
	root := t.TempDir()
	updated := time.Date(2026, time.July, 18, 12, 0, 0, 0, time.UTC)
	issue := lifecycleIssue(updated)
	tracker := &lifecycleTracker{issue: issue}
	agent := &lifecycleAgent{requests: make(chan domain.AgentRequest, 2)}
	settings := lifecycleSettings(root, "printf after-run > .after-run")
	local := localworkspace.New(func() config.Settings { return settings })
	workspaces := &observingLocalWorkspace{local: local, marked: make(chan struct{}, 2), afterRun: make(chan struct{}, 2)}
	coordinator := New(tracker, agent, workspaces, func() config.Settings { return settings }, nil)
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("canonicalize workspace root: %v", err)
	}
	workspacePath := filepath.Join(canonicalRoot, localworkspace.Key(issue.Identifier))

	coordinator.Tick(context.Background())
	request := <-agent.requests
	if request.Workspace != workspacePath {
		t.Fatalf("agent workspace=%q, want prepared workspace %q", request.Workspace, workspacePath)
	}
	<-workspaces.marked
	<-workspaces.afterRun
	if _, err := os.Stat(filepath.Join(workspacePath, ".after-run")); err != nil {
		t.Fatalf("after_run hook did not use prepared workspace: %v", err)
	}
	if shouldRun, err := local.ShouldRun(context.Background(), issue); err != nil || shouldRun {
		t.Fatalf("completed issue should be durably skipped: shouldRun=%t err=%v", shouldRun, err)
	}
	if starts, cancels, sent := agent.counts(); starts != 1 || cancels != 1 {
		t.Fatalf("first lifecycle starts=%d cancels=%d, want 1 each", starts, cancels)
	} else if want := []domain.EventKind{domain.EventSessionStarted, domain.EventProgress, domain.EventUsage, domain.EventRateLimit, domain.EventCompleted}; !reflect.DeepEqual(sent, want) {
		t.Fatalf("success event sequence=%v, want %v", sent, want)
	}

	// The persisted marker suppresses the exact completed Linear version after
	// an orchestrator restart, when in-memory claims are empty again.
	restarted := New(tracker, agent, workspaces, func() config.Settings { return settings }, nil)
	restarted.Tick(context.Background())
	if starts, _, _ := agent.counts(); starts != 1 {
		t.Fatalf("unchanged completed issue restarted %d times", starts)
	}

	// A material Linear update is a new lifecycle and must run once more.
	changed := issue
	changedAt := updated.Add(time.Second)
	changed.UpdatedAt = &changedAt
	tracker.setIssue(changed)
	restarted.Tick(context.Background())
	<-agent.requests
	<-workspaces.marked
	<-workspaces.afterRun
	if shouldRun, err := local.ShouldRun(context.Background(), changed); err != nil || shouldRun {
		t.Fatalf("updated completion should be durably recorded: shouldRun=%t err=%v", shouldRun, err)
	}
	if starts, cancels, _ := agent.counts(); starts != 2 || cancels != 2 {
		t.Fatalf("updated lifecycle starts=%d cancels=%d, want 2 each", starts, cancels)
	}
}

func TestLocalWorkspaceRemainsEligibleAfterTurnLimitExhaustion(t *testing.T) {
	root := t.TempDir()
	updated := time.Date(2026, time.July, 18, 12, 0, 0, 0, time.UTC)
	issue := lifecycleIssue(updated)
	tracker := &lifecycleTracker{issue: issue}
	agent := &lifecycleAgent{requests: make(chan domain.AgentRequest, 1)}
	settings := lifecycleSettings(root, "")
	settings.Agent.MaxTurns = 2
	local := localworkspace.New(func() config.Settings { return settings })
	workspaces := &observingLocalWorkspace{local: local, afterRun: make(chan struct{}, 1)}
	coordinator := New(tracker, agent, workspaces, func() config.Settings { return settings }, nil)
	timer := &fakeTimer{signal: make(chan struct{}, 2)}
	coordinator.timer = timer

	coordinator.Tick(context.Background())
	<-agent.requests
	<-timer.signal
	if shouldRun, err := local.ShouldRun(context.Background(), issue); err != nil || !shouldRun {
		t.Fatalf("first bounded turn wrote marker early: shouldRun=%t err=%v", shouldRun, err)
	}
	timer.fire(0)
	<-workspaces.afterRun
	<-timer.signal
	if got := agent.continuationCount(); got != 1 {
		t.Fatalf("continuations=%d, want 1", got)
	}
	if shouldRun, err := local.ShouldRun(context.Background(), issue); err != nil || !shouldRun {
		t.Fatalf("turn-limit exhaustion should leave the active issue eligible: shouldRun=%t err=%v", shouldRun, err)
	}
}

func TestCorruptLocalCompletionStateNeverStartsAgent(t *testing.T) {
	root := t.TempDir()
	updated := time.Date(2026, time.July, 18, 12, 0, 0, 0, time.UTC)
	issue := lifecycleIssue(updated)
	settings := lifecycleSettings(root, "")
	local := localworkspace.New(func() config.Settings { return settings })
	ws, err := local.Prepare(context.Background(), issue)
	if err != nil {
		t.Fatal(err)
	}
	if err := local.MarkCompleted(context.Background(), ws, issue); err != nil {
		t.Fatal(err)
	}
	markers, err := filepath.Glob(filepath.Join(root, ".symphony-state", "*.json"))
	if err != nil || len(markers) != 1 {
		t.Fatalf("markers=%v err=%v", markers, err)
	}
	if err := os.WriteFile(markers[0], []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	tracker := &lifecycleTracker{issue: issue}
	agent := &lifecycleAgent{requests: make(chan domain.AgentRequest, 1)}
	coordinator := New(tracker, agent, local, func() config.Settings { return settings }, nil)
	coordinator.Tick(context.Background())
	if starts, _, _ := agent.counts(); starts != 0 {
		t.Fatalf("corrupt marker started agent %d times", starts)
	}
	select {
	case request := <-agent.requests:
		t.Fatalf("corrupt marker dispatched request for %s", request.Issue.Identifier)
	default:
	}
}

func TestLocalWorkspaceActiveRunShutdownCancelsAndDoesNotRetry(t *testing.T) {
	root := t.TempDir()
	issue := lifecycleIssue(time.Date(2026, time.July, 18, 12, 0, 0, 0, time.UTC))
	tracker := &lifecycleTracker{issue: issue}
	agent := &lifecycleAgent{block: true, requests: make(chan domain.AgentRequest, 1)}
	settings := lifecycleSettings(root, "printf shutdown > .after-run")
	local := localworkspace.New(func() config.Settings { return settings })
	workspaces := &observingLocalWorkspace{local: local, marked: make(chan struct{}, 1), afterRun: make(chan struct{}, 1)}
	coordinator := New(tracker, agent, workspaces, func() config.Settings { return settings }, nil)
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("canonicalize workspace root: %v", err)
	}
	workspacePath := filepath.Join(canonicalRoot, localworkspace.Key(issue.Identifier))

	coordinator.Tick(context.Background())
	<-agent.requests
	if err := coordinator.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	<-workspaces.afterRun
	if _, err := os.Stat(filepath.Join(workspacePath, ".after-run")); err != nil {
		t.Fatalf("after_run hook did not run during shutdown: %v", err)
	}
	if starts, cancels, _ := agent.counts(); starts != 1 || cancels != 1 {
		t.Fatalf("shutdown starts=%d cancels=%d, want 1 each", starts, cancels)
	}
	coordinator.mu.Lock()
	retries, claims := len(coordinator.retries), len(coordinator.claimed)
	coordinator.mu.Unlock()
	if retries != 0 || claims != 0 {
		t.Fatalf("shutdown retained retries=%d claims=%d", retries, claims)
	}
}

func lifecycleSettings(root, afterRun string) config.Settings {
	return config.Settings{
		Tracker:   config.Tracker{ActiveStates: []string{"todo"}, TerminalStates: []string{"done"}},
		Polling:   config.Polling{Interval: time.Hour},
		Workspace: config.Workspace{Root: root},
		Hooks:     config.Hooks{AfterRun: afterRun, Timeout: time.Second},
		Agent:     config.Agent{MaxConcurrent: 1, MaxTurns: 1, MaxRetryBackoff: time.Second},
		Codex:     config.Codex{Command: "test", TurnTimeout: time.Second, ReadTimeout: time.Second},
		Prompt:    "Work on {{.issue.identifier}}",
	}
}

func lifecycleIssue(updated time.Time) domain.Issue {
	return domain.Issue{ID: "issue-1", Identifier: "PMR-9", Title: "Lifecycle", State: "Todo", Dispatchable: true, UpdatedAt: &updated}
}
