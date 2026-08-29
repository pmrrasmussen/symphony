package coordinator

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
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

func (*lifecycleTracker) Transition(_ context.Context, _ domain.Issue, fromState, _ string) (domain.TransitionResult, error) {
	return domain.TransitionResult{FromState: fromState, Applied: true}, nil
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
	afterRun chan struct{}
}

func (w *observingLocalWorkspace) Prepare(ctx context.Context, issue domain.Issue) (domain.Workspace, error) {
	return w.local.Prepare(ctx, issue)
}

func (w *observingLocalWorkspace) BeforeRun(ctx context.Context, workspace domain.Workspace, issue domain.Issue) error {
	return w.local.BeforeRun(ctx, workspace, issue)
}

func (w *observingLocalWorkspace) AfterRun(ctx context.Context, workspace domain.Workspace, issue domain.Issue) error {
	err := w.local.AfterRun(ctx, workspace, issue)
	w.afterRun <- struct{}{}
	return err
}

func (w *observingLocalWorkspace) Cleanup(ctx context.Context, issue domain.Issue) (domain.CleanupOutcome, error) {
	return w.local.Cleanup(ctx, issue)
}

func TestLocalWorkspaceActiveTurnLimitRemainsEligibleAfterRestart(t *testing.T) {
	root := t.TempDir()
	updated := time.Date(2026, time.July, 18, 12, 0, 0, 0, time.UTC)
	issue := lifecycleIssue(updated)
	tracker := &lifecycleTracker{issue: issue}
	agent := &lifecycleAgent{requests: make(chan domain.AgentRequest, 2)}
	settings := lifecycleSettings(root, "printf after-run > .after-run")
	local := localworkspace.New(func() config.Settings { return settings })
	workspaces := &observingLocalWorkspace{local: local, afterRun: make(chan struct{}, 2)}
	coordinator := New(tracker, agent, workspaces, func() config.Settings { return settings }, nil)
	defer assertInvariants(t, coordinator)
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
	<-workspaces.afterRun
	if err := coordinator.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(workspacePath, ".after-run")); err != nil {
		t.Fatalf("after_run hook did not use prepared workspace: %v", err)
	}
	if _, err := local.Prepare(context.Background(), issue); err != nil {
		t.Fatalf("active exhausted issue should remain preparable: %v", err)
	}
	if starts, cancels, sent := agent.counts(); starts != 1 || cancels != 1 {
		t.Fatalf("first lifecycle starts=%d cancels=%d, want 1 each", starts, cancels)
	} else if want := []domain.EventKind{domain.EventSessionStarted, domain.EventProgress, domain.EventUsage, domain.EventRateLimit, domain.EventCompleted}; !reflect.DeepEqual(sent, want) {
		t.Fatalf("event sequence=%v, want %v", sent, want)
	}

	// Restarting with the exact same active issue must dispatch it again because
	// turn-limit exhaustion keeps active work eligible for a future run.
	restarted := New(tracker, agent, workspaces, func() config.Settings { return settings }, nil)
	defer assertInvariants(t, restarted)
	restarted.Tick(context.Background())
	<-agent.requests
	if err := restarted.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
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
	defer assertInvariants(t, coordinator)
	timer := &fakeTimer{signal: make(chan struct{}, 2)}
	coordinator.timer = timer

	coordinator.Tick(context.Background())
	<-agent.requests
	<-timer.signal
	if _, err := local.Prepare(context.Background(), issue); err != nil {
		t.Fatalf("first bounded turn should leave workspace preparable: %v", err)
	}
	timer.fire(0)
	<-workspaces.afterRun
	<-timer.signal
	if got := agent.continuationCount(); got != 1 {
		t.Fatalf("continuations=%d, want 1", got)
	}
	if _, err := local.Prepare(context.Background(), issue); err != nil {
		t.Fatalf("turn-limit exhaustion should leave the workspace preparable: %v", err)
	}
}

func TestCorruptLocalCompletionStateNeverStartsAgent(t *testing.T) {
	root := t.TempDir()
	updated := time.Date(2026, time.July, 18, 12, 0, 0, 0, time.UTC)
	issue := lifecycleIssue(updated)
	settings := lifecycleSettings(root, "")
	local := localworkspace.New(func() config.Settings { return settings })
	if _, err := local.Prepare(context.Background(), issue); err != nil {
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
	defer assertInvariants(t, coordinator)
	coordinator.Tick(context.Background())
	// Prepare deliberately schedules a retry when it sees the corrupt marker.
	// Stop that background retry before TempDir cleanup removes its workspace.
	if err := coordinator.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
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
	workspaces := &observingLocalWorkspace{local: local, afterRun: make(chan struct{}, 1)}
	coordinator := New(tracker, agent, workspaces, func() config.Settings { return settings }, nil)
	defer assertInvariants(t, coordinator)
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
	snapshot := coordinator.Snapshot()
	if len(snapshot.Retrying) != 0 || snapshot.Claimed != 0 {
		t.Fatalf("shutdown retained retries=%d claims=%d", len(snapshot.Retrying), snapshot.Claimed)
	}
}

func lifecycleSettings(root, afterRun string) config.Settings {
	return config.Settings{
		Tracker:   config.Tracker{ActiveStates: []string{"todo"}, TerminalStates: []string{"done"}},
		Polling:   config.Polling{Interval: time.Hour},
		Workspace: config.Workspace{Root: root},
		Hooks:     config.Hooks{AfterRun: afterRun, Timeout: time.Second},
		Agent:     config.Agent{MaxConcurrent: 1, MaxTurns: 1, MaxAttempts: 5, MaxRetryBackoff: time.Second},
		Codex:     config.Codex{Command: "test", TurnTimeout: time.Second, ReadTimeout: time.Second},
		Prompt:    "Work on {{.issue.identifier}}",
	}
}

func lifecycleIssue(updated time.Time) domain.Issue {
	return domain.Issue{ID: "issue-1", Identifier: "PMR-9", Title: "Lifecycle", State: "Todo", Dispatchable: true, UpdatedAt: &updated}
}

// stubLandingVerifier stands in for the host GitHub merge verification the
// production wiring installs on the local workspace executor. It records the
// commit terminal cleanup asked about so the test can prove cleanup verified
// the worktree's own HEAD.
type stubLandingVerifier struct {
	mu     sync.Mutex
	landed bool
	calls  []string
}

func (s *stubLandingVerifier) VerifyLanded(_ context.Context, _ domain.Issue, commit string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, commit)
	return s.landed, nil
}

func (s *stubLandingVerifier) verified() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

// TestTerminalReconciliationRemovesOnlyVerifiedLandedWorktrees drives the
// reconciliation path an issue takes once the host has landed its pull request
// and the tracker reports Done: the run stops as terminal and cleanup runs
// against a real Git worktree that carries one local commit.
func TestTerminalReconciliationRemovesOnlyVerifiedLandedWorktrees(t *testing.T) {
	tests := []struct {
		name        string
		landed      bool
		wantStatus  string
		wantRemoved bool
	}{
		{name: "verified landing", landed: true, wantStatus: `"status":"landed"`, wantRemoved: true},
		{name: "unpublished commits", wantStatus: `"status":"committed"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			source := newLifecycleGitRepository(t)
			settings := lifecycleSettings(root, "")
			settings.Workspace.SourceRoot = source
			issue := lifecycleIssue(time.Date(2026, time.August, 25, 12, 0, 0, 0, time.UTC))
			tracker := &lifecycleTracker{issue: issue}
			agent := &lifecycleAgent{block: true, requests: make(chan domain.AgentRequest, 1)}
			local := localworkspace.New(func() config.Settings { return settings })
			verifier := &stubLandingVerifier{landed: test.landed}
			local.SetLandingVerifier(verifier)
			workspaces := &observingLocalWorkspace{local: local, afterRun: make(chan struct{}, 1)}
			logs := &syncBuffer{}
			c := New(tracker, agent, workspaces, func() config.Settings { return settings }, slog.New(slog.NewJSONHandler(logs, nil)))
			defer assertInvariants(t, c)

			c.Tick(context.Background())
			request := <-agent.requests
			waitForRunning(t, c, issue.Identifier)
			runLifecycleGit(t, request.Workspace, "commit", "--allow-empty", "-m", "landed work")
			head := lifecycleGitHead(t, request.Workspace)

			landed := issue
			landed.State = "Done"
			tracker.setIssue(landed)
			c.Tick(context.Background())
			<-workspaces.afterRun

			record := waitForSubstring(t, logs, `"msg":"workspace cleanup"`, 2*time.Second)
			if !strings.Contains(record, test.wantStatus) {
				t.Fatalf("cleanup record missing %s: %s", test.wantStatus, record)
			}
			if verified := verifier.verified(); len(verified) != 1 || verified[0] != head {
				t.Fatalf("verified commits=%v, want the worktree HEAD %s once", verified, head)
			}
			_, statErr := os.Stat(request.Workspace)
			if removed := os.IsNotExist(statErr); removed != test.wantRemoved {
				t.Fatalf("workspace removed=%t, want %t (stat=%v)", removed, test.wantRemoved, statErr)
			}
			if err := c.Shutdown(context.Background()); err != nil {
				t.Fatal(err)
			}
			if !test.wantRemoved {
				runLifecycleGit(t, source, "worktree", "remove", "--force", request.Workspace)
			}
		})
	}
}

// newLifecycleGitRepository builds the minimal source repository shape
// LocalWorkspaceExecutor requires: a checkout with an "origin" remote whose
// main branch it can refresh before adding a detached worktree.
func newLifecycleGitRepository(t *testing.T) string {
	t.Helper()
	remote := filepath.Join(t.TempDir(), "remote.git")
	runLifecycleGit(t, filepath.Dir(remote), "init", "--bare", remote)
	source := filepath.Join(t.TempDir(), "source")
	if err := os.Mkdir(source, 0o755); err != nil {
		t.Fatal(err)
	}
	runLifecycleGit(t, source, "init")
	runLifecycleGit(t, source, "config", "user.email", "test@example.invalid")
	runLifecycleGit(t, source, "config", "user.name", "Test")
	runLifecycleGit(t, source, "commit", "--allow-empty", "-m", "initial")
	runLifecycleGit(t, source, "branch", "-M", "main")
	runLifecycleGit(t, source, "remote", "add", "origin", remote)
	runLifecycleGit(t, source, "push", "-u", "origin", "main")
	return source
}

func runLifecycleGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func lifecycleGitHead(t *testing.T, dir string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v: %s", err, out)
	}
	return strings.TrimSpace(string(out))
}
