package coordinator

import (
	"context"
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
	starts int
	mu     sync.Mutex
}

func (f *fakeAgent) Start(context.Context, domain.AgentRequest) (domain.AgentSession, <-chan domain.Event, error) {
	f.mu.Lock()
	f.starts++
	f.mu.Unlock()
	ch := make(chan domain.Event, 1)
	ch <- domain.Event{Kind: domain.EventCompleted, At: time.Now(), SessionID: "t-u"}
	close(ch)
	return domain.AgentSession{ID: "t-u", ThreadID: "t", TurnID: "u"}, ch, nil
}
func (f *fakeAgent) Continue(context.Context, domain.AgentSession, string) (<-chan domain.Event, error) {
	return nil, nil
}
func (f *fakeAgent) Cancel(context.Context, domain.AgentSession) error { return nil }

type fakeWorkspace struct{ after chan struct{} }

func (f fakeWorkspace) Prepare(context.Context, domain.Issue) (domain.Workspace, error) {
	return domain.Workspace{Path: "/tmp/work"}, nil
}
func (f fakeWorkspace) BeforeRun(context.Context, domain.Workspace, domain.Issue) error { return nil }
func (f fakeWorkspace) AfterRun(context.Context, domain.Workspace, domain.Issue)        { close(f.after) }
func (f fakeWorkspace) Cleanup(context.Context, domain.Issue) error                     { return nil }
func (f fakeWorkspace) Execute(context.Context, domain.Workspace, string, []string) ([]byte, error) {
	return nil, nil
}

type fakeTimer struct {
	mu     sync.Mutex
	delays []time.Duration
}

func (f *fakeTimer) AfterFunc(d time.Duration, _ func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.delays = append(f.delays, d)
}
func TestCompleteIssueSchedulesContinuationWithoutSleep(t *testing.T) {
	d := t.TempDir()
	workflow := filepath.Join(d, "WORKFLOW.md")
	if err := os.WriteFile(workflow, []byte("---\ntracker: {kind: linear, active_states: [Todo], terminal_states: [Done]}\nagent: {max_turns: 1, max_concurrent_agents: 1}\nworkspace: {root: /tmp/work}\n---\nWork on {{.Issue.Identifier}}"), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := config.Load(workflow, "")
	if err != nil {
		t.Fatal(err)
	}
	issue := domain.Issue{ID: "id", Identifier: "ENG-1", Title: "Work", State: "Todo", Dispatchable: true}
	agent := &fakeAgent{}
	ws := fakeWorkspace{after: make(chan struct{})}
	c := New(fakeTracker{issue}, agent, ws, func() config.Settings { return w.Config }, nil)
	timer := &fakeTimer{}
	c.timer = timer
	c.Tick(context.Background())
	<-ws.after
	agent.mu.Lock()
	starts := agent.starts
	agent.mu.Unlock()
	if starts != 1 {
		t.Fatalf("starts=%d", starts)
	}
	timer.mu.Lock()
	defer timer.mu.Unlock()
	if len(timer.delays) != 1 || timer.delays[0] != continuationDelay {
		t.Fatalf("retries=%v", timer.delays)
	}
}
