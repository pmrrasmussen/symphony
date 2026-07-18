package coordinator

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/domain"
)

type fakeTracker struct{ issue domain.Issue }

func (f fakeTracker) ListCandidates(context.Context, []string) ([]domain.Issue, error) {
	return []domain.Issue{f.issue}, nil
}
func (f fakeTracker) GetIssues(context.Context, []string) ([]domain.Issue, error) {
	return []domain.Issue{f.issue}, nil
}
func (f fakeTracker) ListTerminal(context.Context, []string) ([]domain.Issue, error) { return nil, nil }

type fakeAgent struct {
	mu        sync.Mutex
	starts    int
	continues int
	cancels   int
	events    func() <-chan domain.Event
}

func (f *fakeAgent) Start(context.Context, domain.AgentRequest) (domain.AgentSession, <-chan domain.Event, error) {
	f.mu.Lock()
	f.starts++
	f.mu.Unlock()
	return domain.AgentSession{ID: "t-u", ThreadID: "t", TurnID: "u"}, f.events(), nil
}
func (f *fakeAgent) Continue(context.Context, domain.AgentSession, string) (<-chan domain.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.continues++
	return nil, nil
}
func (f *fakeAgent) Cancel(context.Context, domain.AgentSession) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancels++
	return nil
}

type fakeWorkspace struct {
	mu             sync.Mutex
	shouldRun      bool
	shouldRunCalls int
	prepares       int
	marks          int
	markErr        error
	after          chan struct{}
}

func (f *fakeWorkspace) ShouldRun(context.Context, domain.Issue) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.shouldRunCalls++
	return f.shouldRun, nil
}
func (f *fakeWorkspace) Prepare(context.Context, domain.Issue) (domain.Workspace, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prepares++
	return domain.Workspace{Path: "/tmp/work"}, nil
}
func (f *fakeWorkspace) BeforeRun(context.Context, domain.Workspace, domain.Issue) error { return nil }
func (f *fakeWorkspace) AfterRun(context.Context, domain.Workspace, domain.Issue) {
	close(f.after)
}
func (f *fakeWorkspace) MarkCompleted(context.Context, domain.Workspace, domain.Issue) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.marks++
	return f.markErr
}
func (f *fakeWorkspace) Cleanup(context.Context, domain.Issue) error { return nil }
func (f *fakeWorkspace) Execute(context.Context, domain.Workspace, string, []string) ([]byte, error) {
	return nil, nil
}

type fakeTimer struct {
	mu     sync.Mutex
	delays []time.Duration
	called chan struct{}
}

func (f *fakeTimer) AfterFunc(d time.Duration, _ func()) {
	f.mu.Lock()
	f.delays = append(f.delays, d)
	called := f.called
	f.mu.Unlock()
	if called != nil {
		close(called)
	}
}

func TestCompletionIsRecordedAndDoesNotContinueOrRetry(t *testing.T) {
	w := testSettings(t)
	issue := testIssue()
	agent := &fakeAgent{events: completedEvents}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{})}
	c := New(fakeTracker{issue}, agent, ws, func() config.Settings { return w.Config }, nil)
	timer := &fakeTimer{}
	c.timer = timer

	c.Tick(context.Background())
	<-ws.after

	agent.mu.Lock()
	starts, continues, cancels := agent.starts, agent.continues, agent.cancels
	agent.mu.Unlock()
	if starts != 1 || continues != 0 || cancels != 1 {
		t.Fatalf("starts=%d continues=%d cancels=%d", starts, continues, cancels)
	}
	ws.mu.Lock()
	marks := ws.marks
	ws.mu.Unlock()
	if marks != 1 {
		t.Fatalf("completed work marks=%d, want 1", marks)
	}
	timer.mu.Lock()
	defer timer.mu.Unlock()
	if len(timer.delays) != 0 {
		t.Fatalf("completed run scheduled retries=%v", timer.delays)
	}
}

func TestClosedEventStreamBeforeCompletionRetries(t *testing.T) {
	w := testSettings(t)
	issue := testIssue()
	agent := &fakeAgent{events: closedEvents}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{})}
	c := New(fakeTracker{issue}, agent, ws, func() config.Settings { return w.Config }, nil)
	timer := &fakeTimer{called: make(chan struct{})}
	c.timer = timer

	c.Tick(context.Background())
	<-timer.called

	ws.mu.Lock()
	marks := ws.marks
	ws.mu.Unlock()
	if marks != 0 {
		t.Fatalf("closed stream marked completion %d times", marks)
	}
	timer.mu.Lock()
	defer timer.mu.Unlock()
	if len(timer.delays) != 1 || timer.delays[0] != 10*time.Second {
		t.Fatalf("retries=%v", timer.delays)
	}
}

func TestCompletionMarkerFailureDoesNotRerunCodex(t *testing.T) {
	w := testSettings(t)
	issue := testIssue()
	agent := &fakeAgent{events: completedEvents}
	ws := &fakeWorkspace{shouldRun: true, markErr: errors.New("disk full"), after: make(chan struct{})}
	c := New(fakeTracker{issue}, agent, ws, func() config.Settings { return w.Config }, nil)
	timer := &fakeTimer{called: make(chan struct{})}
	c.timer = timer

	c.Tick(context.Background())
	<-timer.called

	agent.mu.Lock()
	starts := agent.starts
	agent.mu.Unlock()
	if starts != 1 {
		t.Fatalf("starts=%d, want one completed Codex turn", starts)
	}
	timer.mu.Lock()
	defer timer.mu.Unlock()
	if len(timer.delays) != 1 || timer.delays[0] != 10*time.Second {
		t.Fatalf("completion marker retry=%v", timer.delays)
	}
}

func TestShouldRunPreventsClaimAndLaunch(t *testing.T) {
	w := testSettings(t)
	issue := testIssue()
	agent := &fakeAgent{events: completedEvents}
	ws := &fakeWorkspace{shouldRun: false, after: make(chan struct{})}
	c := New(fakeTracker{issue}, agent, ws, func() config.Settings { return w.Config }, nil)

	c.Tick(context.Background())

	agent.mu.Lock()
	starts := agent.starts
	agent.mu.Unlock()
	ws.mu.Lock()
	prepares, checks := ws.prepares, ws.shouldRunCalls
	ws.mu.Unlock()
	if starts != 0 || prepares != 0 || checks != 1 {
		t.Fatalf("starts=%d prepares=%d should-run-checks=%d", starts, prepares, checks)
	}
	c.mu.Lock()
	claimed := c.claimed[issue.ID]
	c.mu.Unlock()
	if claimed {
		t.Fatal("issue was claimed despite workspace refusing to run")
	}
}

func testSettings(t *testing.T) config.Workflow {
	t.Helper()
	d := t.TempDir()
	workflow := filepath.Join(d, "WORKFLOW.md")
	if err := os.WriteFile(workflow, []byte("---\ntracker: {kind: linear, active_states: [Todo], terminal_states: [Done]}\nagent: {max_concurrent_agents: 1}\nworkspace: {root: /tmp/work}\n---\nWork on {{.Issue.Identifier}}"), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := config.Load(workflow, "")
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func testIssue() domain.Issue {
	return domain.Issue{ID: "id", Identifier: "ENG-1", Title: "Work", State: "Todo", Dispatchable: true}
}

func completedEvents() <-chan domain.Event {
	ch := make(chan domain.Event, 1)
	ch <- domain.Event{Kind: domain.EventCompleted, At: time.Now(), SessionID: "t-u"}
	close(ch)
	return ch
}

func closedEvents() <-chan domain.Event {
	ch := make(chan domain.Event)
	close(ch)
	return ch
}
