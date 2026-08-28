package coordinator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/domain"
)

func TestCleanupStatusClassifiesWorkspaceOutcome(t *testing.T) {
	tests := []struct {
		name    string
		outcome domain.CleanupOutcome
		err     error
		want    string
	}{
		{name: "clean", outcome: domain.CleanupClean, err: nil, want: "clean"},
		{name: "landed", outcome: domain.CleanupLanded, err: nil, want: "landed"},
		{name: "dirty", err: errors.New("refusing to remove Git workspace with uncommitted or untracked changes"), want: "dirty"},
		{name: "committed", err: fmt.Errorf("refusing to remove Git workspace whose HEAD %s differs from recorded base commit %s", "abc", "def"), want: "committed"},
		{name: "unverifiable landing stays committed", err: fmt.Errorf("refusing to remove Git workspace whose HEAD %s differs from recorded base commit %s; merged landing could not be verified", "abc", "def"), want: "committed"},
		{name: "blocked", err: errors.New("refusing to remove workspace without durable ownership state"), want: "blocked"},
		// A killed subprocess or another failure that never reached a refusal
		// decision is not a verified refusal, so it must not be reported as
		// blocked (PMR-130): it names no committed or dirty work to protect.
		{name: "failed before it could classify anything", err: errors.New("validate recorded source repository: classify workspace source repository: git rev-parse --path-format=absolute --show-toplevel: signal: killed: ; manual recovery is required"), want: "failed"},
		// A refused cleanup never reports a removal outcome, even if one leaks in.
		{name: "landed outcome never masks a refusal", outcome: domain.CleanupLanded, err: errors.New("refusing to remove Git workspace with uncommitted or untracked changes"), want: "dirty"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := cleanupStatus(test.outcome, test.err); got != test.want {
				t.Fatalf("cleanupStatus=%q, want %q", got, test.want)
			}
		})
	}
}

// TestRunEndCleanupSurvivesConcurrentReconciliationCancellation covers
// PMR-130's first cause: a resolved landing's own workspace cleanup raced
// reconciliation, which observed the same terminal issue and called stopRun
// concurrently. stopRun cancels the run's own context, and cleanupWorkspace
// used to inherit it, turning a healthy landing's git invocation into a
// killed subprocess reported as an operator-actionable "blocked". The
// run-end cleanup call must hold a context detached from that cancellation so
// a race with reconciliation cannot fail it.
func TestRunEndCleanupSurvivesConcurrentReconciliationCancellation(t *testing.T) {
	w := testSettings(t)
	issue := testIssue()
	terminal := issue
	terminal.State = "Done"
	terminal.Dispatchable = false
	tracker := &fakeTracker{issue: issue}
	tracker.setFresh(terminal)
	var log syncBuffer
	agent := &fakeAgent{events: completedEvents}
	gate := make(chan struct{})
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{}, 1), cleanupStarted: make(chan struct{}, 1), cleanupGate: gate}
	c := New(tracker, agent, ws, func() config.Settings { return w.Config }, slog.New(slog.NewJSONHandler(&log, nil)))
	c.clock = fakeClock{now: time.Date(2026, 8, 26, 7, 37, 0, 0, time.UTC)}
	c.timer = &fakeTimer{}

	c.Tick(context.Background())
	<-ws.cleanupStarted // the run's own cleanup is now blocked mid-attempt

	if !c.stopRun(issue.ID, stopTerminal) {
		t.Fatal("reconciliation could not stop a run whose own cleanup is still in flight")
	}
	close(gate) // let the blocked attempt observe whatever context it was actually given

	<-ws.after
	waitForRelease(t, c, issue.ID)

	if _, _, cleanups, _ := ws.counts(); cleanups != 1 {
		t.Fatalf("cleanups=%d, want exactly the one run-end attempt", cleanups)
	}
	lines := cleanupLogLines(log.String())
	if len(lines) != 1 {
		t.Fatalf("workspace cleanup records=%v, want exactly one", lines)
	}
	if strings.Contains(lines[0], `"level":"WARN"`) || strings.Contains(lines[0], `"status":"blocked"`) {
		t.Fatalf("run-end cleanup observed the concurrent cancellation it must be detached from: %s", lines[0])
	}
	if !strings.Contains(lines[0], `"status":"clean"`) {
		t.Fatalf("run-end cleanup did not report a successful removal: %s", lines[0])
	}
}

// TestRunEndCleanupFailureIsNotActionableWhenReconciliationSucceeds covers
// PMR-130's reporting fix directly: even when the run-end cleanup attempt
// fails for a reason that has nothing to do with context cancellation (here,
// a stand-in for the read-after-write race PMR-112's landing hit against
// GitHub), reconciliation's own concurrent, authoritative attempt succeeding
// right after it means the first failure named no real call to action and
// must not be logged at WARN.
func TestRunEndCleanupFailureIsNotActionableWhenReconciliationSucceeds(t *testing.T) {
	w := testSettings(t)
	issue := testIssue()
	terminal := issue
	terminal.State = "Done"
	terminal.Dispatchable = false
	tracker := &fakeTracker{issue: issue}
	tracker.setFresh(terminal)
	var log syncBuffer
	agent := &fakeAgent{events: completedEvents}
	gate := make(chan struct{})
	ws := &fakeWorkspace{
		shouldRun:      true,
		after:          make(chan struct{}, 1),
		cleanupStarted: make(chan struct{}, 1),
		cleanupGate:    gate,
		cleanupErr:     errors.New("refusing to remove Git workspace whose HEAD c6e8a98 differs from recorded base commit 54bccf5; merged landing could not be verified"),
	}
	c := New(tracker, agent, ws, func() config.Settings { return w.Config }, slog.New(slog.NewJSONHandler(&log, nil)))
	c.clock = fakeClock{now: time.Date(2026, 8, 26, 7, 41, 0, 0, time.UTC)}
	c.timer = &fakeTimer{}

	c.Tick(context.Background())
	<-ws.cleanupStarted // the run's own cleanup is now blocked before it fails

	// Reconciliation independently reaches the same terminal issue while the
	// run-end attempt is still in flight, stops the run, and runs its own
	// cleanup on a live context -- the authoritative retry.
	c.Tick(context.Background())
	close(gate) // only now let the blocked, doomed first attempt fail

	<-ws.after
	waitForRelease(t, c, issue.ID)

	if _, _, cleanups, _ := ws.counts(); cleanups != 2 {
		t.Fatalf("cleanups=%d, want the run-end attempt and reconciliation's authoritative retry", cleanups)
	}
	lines := cleanupLogLines(log.String())
	if len(lines) != 2 {
		t.Fatalf("workspace cleanup records=%v, want exactly two", lines)
	}
	for _, line := range lines {
		if strings.Contains(line, `"level":"WARN"`) {
			t.Fatalf("a superseded first attempt must never be reported as an operator call to action: %s", line)
		}
	}
	// Reconciliation's own cleanup runs synchronously within the second Tick
	// call and so is logged first; the run-end attempt was still blocked on
	// the gate at that point and only logs once it is released afterward.
	if !strings.Contains(lines[0], `"status":"clean"`) {
		t.Fatalf("reconciliation's retry record=%s, want a successful removal", lines[0])
	}
	if !strings.Contains(lines[1], `"status":"committed"`) {
		t.Fatalf("run-end cleanup record=%s, want the classified committed refusal", lines[1])
	}
}

// TestRunEndCleanupFailureStaysActionableForANonTerminalStopReason guards the
// narrower half of the same fix: reconciliation only ever re-cleans up a run
// it stopped for stopTerminal (coordinator.go:733), so a run-end failure
// raced by any other stop reason -- ineligible or stalled -- names a genuine
// leak that nothing will retry. Treating every non-empty r.stopped as
// superseded, as an earlier version of this fix did, would swallow that leak
// at Info and reintroduce exactly the silence PMR-130 exists to prevent.
func TestRunEndCleanupFailureStaysActionableForANonTerminalStopReason(t *testing.T) {
	for _, reason := range []stopReason{stopIneligible, stopStalled} {
		t.Run(string(reason), func(t *testing.T) {
			issue := testIssue()
			var log syncBuffer
			ws := &fakeWorkspace{
				cleanupErr: errors.New("refusing to remove Git workspace whose HEAD c6e8a98 differs from recorded base commit 54bccf5; merged landing could not be verified"),
			}
			c := New(&fakeTracker{issue: issue}, &fakeAgent{}, ws, func() config.Settings { return testSettings(t).Config }, slog.New(slog.NewJSONHandler(&log, nil)))
			r := &running{issue: issue, stopped: reason}

			c.cleanupWorkspaceAtRunEnd(context.Background(), r, issue)

			lines := cleanupLogLines(log.String())
			if len(lines) != 1 {
				t.Fatalf("workspace cleanup records=%v, want exactly one", lines)
			}
			if !strings.Contains(lines[0], `"level":"WARN"`) {
				t.Fatalf("a failure raced by stop reason %q has no guaranteed retry and must stay a call to action: %s", reason, lines[0])
			}
			if !strings.Contains(lines[0], `"status":"committed"`) {
				t.Fatalf("run-end cleanup record=%s, want the classified committed refusal", lines[0])
			}
		})
	}
}

func TestTerminalIssueIsForgottenByTheHostIntegration(t *testing.T) {
	w := testSettings(t)
	issue := testIssue()
	tracker := &fakeTracker{issue: issue}
	terminal := issue
	terminal.State = "Done"
	terminal.Dispatchable = false
	tracker.setFresh(terminal)
	agent := &fakeAgent{events: completedEvents}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{}, 1), cleaned: make(chan struct{}, 1)}
	c := testCoordinator(w.Config, tracker, agent, ws)
	forgetter := &stubForgetter{}
	c.SetIssueForgetter(forgetter)

	c.Tick(context.Background())
	<-ws.cleaned
	if got := forgetter.issues(); len(got) != 1 || got[0] != issue.ID {
		t.Fatalf("terminal issue releases=%v, want exactly %q", got, issue.ID)
	}
}

// A run that ends with its issue still active must not release anything: the
// pull request that issue published is exactly the one the poll loop still has
// a merge to observe on.
func TestActiveIssueIsNotForgotten(t *testing.T) {
	w := testSettings(t)
	issue := testIssue()
	agent := &fakeAgent{events: completedEvents}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{}, 1)}
	c := testCoordinator(w.Config, &fakeTracker{issue: issue}, agent, ws)
	forgetter := &stubForgetter{}
	c.SetIssueForgetter(forgetter)

	c.Tick(context.Background())
	<-ws.after
	if got := forgetter.issues(); len(got) != 0 {
		t.Fatalf("still-active issue released=%v, want none", got)
	}
}
