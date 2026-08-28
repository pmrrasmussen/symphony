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
	defer assertInvariants(t, c)

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
	defer assertInvariants(t, c)
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
	defer assertInvariants(t, c)

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

// TestDispatchStartTransitionSkipsIssueMovedDuringDispatch covers the window
// between the poll snapshot and the start move: the coordinator must assert the
// state its edge was keyed by, and report the state the adapter actually read
// rather than the snapshot's, so the log never claims a Todo -> In Progress
// move for an issue a human had already cancelled.
func TestDispatchStartTransitionSkipsIssueMovedDuringDispatch(t *testing.T) {
	s := startTransitionSettings(t)
	issue := testIssue()
	tracker := &fakeTracker{issue: issue, liveState: "Canceled"}
	agent := &fakeAgent{events: completedEvents}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{}, 1)}
	var logs bytes.Buffer
	c := New(tracker, agent, ws, func() config.Settings { return s }, slog.New(slog.NewJSONHandler(&logs, nil)))
	defer assertInvariants(t, c)
	c.clock = fakeClock{now: time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)}

	c.Tick(context.Background())
	<-ws.after
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}

	calls := tracker.transitionCalls()
	if len(calls) != 1 || calls[0].from != "Todo" || calls[0].to != "In Progress" {
		t.Fatalf("start transitions=%+v, want one Todo -> In Progress request naming its source", calls)
	}
	output := logs.String()
	if strings.Contains(output, `"msg":"issue moved to started state"`) {
		t.Fatalf("a concurrently cancelled issue was reported as started: %s", output)
	}
	if !strings.Contains(output, `"msg":"dispatch start transition skipped: issue left the start state"`) ||
		!strings.Contains(output, `"from_state":"canceled"`) ||
		!strings.Contains(output, `"expected_from_state":"todo"`) {
		t.Fatalf("skipped start transition not reconstructable from log: %s", output)
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
	defer assertInvariants(t, c)
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
