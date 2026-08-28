package coordinator

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/pmrrasmussen/symphony/internal/config"
)

func TestDispatchMovesTodoIssueToInProgress(t *testing.T) {
	s := startTransitionSettings(t)
	issue := testIssue()
	tracker := &fakeTracker{issue: issue}
	agent := &fakeAgent{events: completedEvents}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{}, 1)}
	c := testCoordinator(s, tracker, agent, ws)

	c.Tick(context.Background())
	<-ws.after
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}

	calls := tracker.transitionCalls()
	if len(calls) != 1 {
		t.Fatalf("start transitions=%+v, want exactly one dispatch transition", calls)
	}
	if calls[0].id != issue.ID || calls[0].to != "In Progress" {
		t.Fatalf("start transition=%+v, want issue %q moved to In Progress", calls[0], issue.ID)
	}
	if starts, _, _ := agent.counts(); starts != 1 {
		t.Fatalf("starts=%d, want the session to start after the transition", starts)
	}
}

func TestDispatchStartTransitionLogsOperationAndEdge(t *testing.T) {
	s := startTransitionSettings(t)
	issue := testIssue()
	tracker := &fakeTracker{issue: issue}
	agent := &fakeAgent{events: completedEvents}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{}, 1)}
	var logs bytes.Buffer
	c := New(tracker, agent, ws, func() config.Settings { return s }, slog.New(slog.NewJSONHandler(&logs, nil)))
	c.clock = fakeClock{now: time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)}

	c.Tick(context.Background())
	<-ws.after
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}

	output := logs.String()
	if !strings.Contains(output, `"msg":"issue moved to started state"`) ||
		!strings.Contains(output, `"operation":"start_transition"`) ||
		!strings.Contains(output, `"from_state":"todo"`) ||
		!strings.Contains(output, `"to_state":"in progress"`) {
		t.Fatalf("host-side start transition edge not reconstructable from log: %s", output)
	}
}

func TestDispatchDoesNotTransitionAlreadyStartedIssue(t *testing.T) {
	s := startTransitionSettings(t)
	// An issue re-observed already In Progress (a restart or turn-limit
	// re-dispatch) has no configured start edge for its state, so the
	// coordinator must not request any transition.
	started := testIssue()
	started.State = "In Progress"
	tracker := &fakeTracker{issue: started}
	agent := &fakeAgent{events: completedEvents}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{}, 1)}
	c := testCoordinator(s, tracker, agent, ws)

	c.Tick(context.Background())
	<-ws.after
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}

	if calls := tracker.transitionCalls(); len(calls) != 0 {
		t.Fatalf("start transitions=%+v, want none for an already-started issue", calls)
	}
	if starts, _, _ := agent.counts(); starts != 1 {
		t.Fatalf("starts=%d, want the session to start", starts)
	}
}

func TestDispatchTransitionFailureDoesNotBlockOrDoubleDispatch(t *testing.T) {
	s := startTransitionSettings(t)
	issue := testIssue()
	tracker := &fakeTracker{issue: issue, transitionErr: errors.New("tracker write failed")}
	agent := &fakeAgent{events: completedEvents}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{}, 1)}
	var logs bytes.Buffer
	c := New(tracker, agent, ws, func() config.Settings { return s }, slog.New(slog.NewJSONHandler(&logs, nil)))
	c.clock = fakeClock{now: time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)}

	c.Tick(context.Background())
	<-ws.after
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}

	if calls := tracker.transitionCalls(); len(calls) != 1 {
		t.Fatalf("start transitions=%+v, want exactly one attempt even on failure", calls)
	}
	// A failed transition must degrade gracefully: the session still starts
	// exactly once and the run is not double-dispatched.
	if starts, _, _ := agent.counts(); starts != 1 {
		t.Fatalf("starts=%d, want the run to proceed once despite the failed transition", starts)
	}
	if output := logs.String(); !strings.Contains(output, `"msg":"dispatch start transition failed"`) {
		t.Fatalf("failed transition was not logged: %s", output)
	}
}
