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
	defer assertInvariants(t, c)
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
	if ws.overlappedCleanups() {
		t.Fatal("two cleanup attempts ran against one worktree at once")
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
// GitHub), reconciliation's own authoritative attempt succeeding right after it
// means the first failure named no real call to action and must not be logged
// at WARN.
//
// The two attempts are what PMR-160 made sequential rather than concurrent, and
// this is the half of that serialization which must keep running twice: an
// attempt that failed removed nothing, so the workspace is still there and the
// authoritative caller still has a removal to attempt.
func TestRunEndCleanupFailureIsNotActionableWhenReconciliationSucceeds(t *testing.T) {
	issue := testIssue()
	var log syncBuffer
	ws := &fakeWorkspace{cleanupErr: errors.New("refusing to remove Git workspace whose HEAD c6e8a98 differs from recorded base commit 54bccf5; merged landing could not be verified")}
	c := New(&fakeTracker{issue: issue}, &fakeAgent{}, ws, func() config.Settings { return testSettings(t).Config }, slog.New(slog.NewJSONHandler(&log, nil)))
	defer assertInvariants(t, c)
	// stopTerminal is the one stop reason that guarantees reconciliation holds
	// an authoritative attempt of its own right after stopRun returns.
	r := &running{issue: issue, stopped: stopTerminal}

	c.cleanupWorkspaceAtRunEnd(context.Background(), r, issue)
	c.cleanupWorkspaceForRun(context.Background(), r, issue)

	if _, _, cleanups, _ := ws.counts(); cleanups != 2 {
		t.Fatalf("cleanups=%d, want the run-end attempt and reconciliation's authoritative retry", cleanups)
	}
	if ws.overlappedCleanups() {
		t.Fatal("two cleanup attempts ran against one worktree at once")
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
	if !strings.Contains(lines[0], `"status":"committed"`) {
		t.Fatalf("run-end cleanup record=%s, want the classified committed refusal", lines[0])
	}
	if !strings.Contains(lines[1], `"status":"clean"`) {
		t.Fatalf("reconciliation's retry record=%s, want a successful removal", lines[1])
	}
}

// TestSucceededCleanupIsNotAttemptedAgain is the other half of PMR-160's
// serialization, and the reason the observed WARN is gone: an attempt holds the
// run's gate for as long as it is in flight, so the second caller cannot reach
// Cleanup while the first is still removing the worktree, and once the first
// has removed it there is nothing left for the second to do -- no Cleanup call,
// no second record, and no second Forget of an issue the host already dropped.
func TestSucceededCleanupIsNotAttemptedAgain(t *testing.T) {
	issue := testIssue()
	var log syncBuffer
	gate := make(chan struct{})
	ws := &fakeWorkspace{cleanupStarted: make(chan struct{}, 1), cleanupGate: gate}
	c := New(&fakeTracker{issue: issue}, &fakeAgent{}, ws, func() config.Settings { return testSettings(t).Config }, slog.New(slog.NewJSONHandler(&log, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer assertInvariants(t, c)
	forgetter := &stubForgetter{}
	c.SetIssueForgetter(forgetter)
	r := &running{issue: issue}

	finished := make(chan struct{})
	go func() {
		defer close(finished)
		c.cleanupWorkspaceAtRunEnd(context.Background(), r, issue)
	}()
	<-ws.cleanupStarted
	if r.cleanup.TryLock() {
		r.cleanup.Unlock()
		t.Fatal("an in-flight cleanup does not hold the run's gate, so a second caller could remove the same worktree concurrently")
	}
	close(gate)
	<-finished

	c.cleanupWorkspaceForRun(context.Background(), r, issue)

	if _, _, cleanups, _ := ws.counts(); cleanups != 1 {
		t.Fatalf("cleanups=%d, want only the attempt that had something to remove", cleanups)
	}
	if got := forgetter.issues(); len(got) != 1 {
		t.Fatalf("host forget calls=%v, want exactly one", got)
	}
	if lines := cleanupLogLines(log.String()); len(lines) != 1 {
		t.Fatalf("workspace cleanup records=%v, want exactly one", lines)
	}
	if !strings.Contains(log.String(), `"msg":"workspace cleanup skipped"`) || !strings.Contains(log.String(), `"reason":"already_finalized"`) {
		t.Fatalf("the skipped attempt left no debug-level trace: %s", log.String())
	}
}

// TestReconciliationCleanupAndRunEndCleanupShareOneAttempt is PMR-160's
// observed pair of call sites, from reconciliation's end: the poll loop's
// stopTerminal branch must clean up *through the run it just stopped*, so its
// attempt and that run's own run-end attempt are one serialized pair. This is
// the direction the observed logs showed inverted -- reconciliation reached the
// worktree the landing's own cleanup was already removing, logged a second
// "GitHub landing verified for workspace cleanup", and then warned that `git
// worktree remove` had failed with "is not a working tree" on a landing that
// had in fact succeeded.
func TestReconciliationCleanupAndRunEndCleanupShareOneAttempt(t *testing.T) {
	w := testSettings(t)
	issue := testIssue()
	terminal := issue
	terminal.State = "Done"
	terminal.Dispatchable = false
	tracker := &fakeTracker{issue: issue}
	tracker.setFresh(terminal)
	var log syncBuffer
	gate := make(chan struct{})
	ws := &fakeWorkspace{cleanupStarted: make(chan struct{}, 1), cleanupGate: gate}
	c := New(tracker, &fakeAgent{}, ws, func() config.Settings { return w.Config }, slog.New(slog.NewJSONHandler(&log, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer assertInvariants(t, c)
	r := &running{issue: issue, session: domain.AgentSession{ID: "s"}, last: c.clock.Now(), cancel: func() {}}
	c.seedRunning(issue, r)

	reconciled := make(chan struct{})
	go func() {
		defer close(reconciled)
		if err := c.reconcile(context.Background()); err != nil {
			t.Errorf("reconcile: %v", err)
		}
	}()
	<-ws.cleanupStarted // reconciliation's own attempt is now in flight

	// It has to be holding the stopped run's gate, or the run-end attempt that
	// runs a moment later inside runTurns would remove the same worktree
	// concurrently instead of waiting for this one.
	if r.cleanup.TryLock() {
		r.cleanup.Unlock()
		t.Fatal("reconciliation cleaned up without the run's cleanup gate, so the run-end attempt could race it")
	}
	close(gate)
	<-reconciled

	// runTurns reaches its own terminal branch for the same issue right after.
	c.cleanupWorkspaceAtRunEnd(context.Background(), r, terminal)

	if _, _, cleanups, _ := ws.counts(); cleanups != 1 {
		t.Fatalf("cleanups=%d, want exactly one attempt for one finished run", cleanups)
	}
	if ws.overlappedCleanups() {
		t.Fatal("two cleanup attempts ran against one worktree at once")
	}
	lines := cleanupLogLines(log.String())
	if len(lines) != 1 {
		t.Fatalf("workspace cleanup records=%v, want exactly one", lines)
	}
	if strings.Contains(lines[0], `"level":"WARN"`) {
		t.Fatalf("a successful removal must not report a cleanup failure: %s", lines[0])
	}
	if !strings.Contains(lines[0], `"status":"clean"`) {
		t.Fatalf("cleanup record=%s, want a successful removal", lines[0])
	}
}

// TestDocumentedCleanupRefusalsStayActionable holds the other half of PMR-160
// in place: making an already-removed workspace a silent success must not make
// a workspace Symphony is deliberately keeping silent too. Every refusal
// docs/dogfooding.md section 7 tells an operator to expect -- uncommitted
// changes, a HEAD moved off the recorded base commit, and a recorded source
// path that no longer identifies the same repository -- must still reach WARN
// carrying the workspace package's own reason, because each names work only a
// human can decide the fate of.
func TestDocumentedCleanupRefusalsStayActionable(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus string
	}{
		{
			name:       "uncommitted or untracked changes",
			err:        errors.New("refusing to remove Git workspace with uncommitted or untracked changes"),
			wantStatus: "dirty",
		},
		{
			name:       "HEAD moved off the recorded base commit",
			err:        errors.New("refusing to remove Git workspace whose HEAD c6e8a98 differs from recorded base commit 54bccf5"),
			wantStatus: "committed",
		},
		{
			name:       "source repository is gone, so local changes cannot be verified",
			err:        errors.New("recorded source and Git common directory are unavailable; refusing to remove a worktree whose local changes cannot be verified; preserve it outside the managed root for manual recovery"),
			wantStatus: "blocked",
		},
		{
			// This one names no "refusing", so cleanupStatus reports it in the
			// failed bucket rather than as blocked. The distinction is only how
			// the status reads: the record is still a WARN carrying the reason.
			name:       "recorded source path identifies a different repository",
			err:        errors.New("recorded source path now identifies a different Git repository; manual recovery is required"),
			wantStatus: "failed",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			issue := testIssue()
			var log syncBuffer
			ws := &fakeWorkspace{cleanupErr: test.err}
			c := New(&fakeTracker{issue: issue}, &fakeAgent{}, ws, func() config.Settings { return testSettings(t).Config }, slog.New(slog.NewJSONHandler(&log, nil)))
			defer assertInvariants(t, c)

			c.cleanupWorkspace(context.Background(), issue)

			lines := cleanupLogLines(log.String())
			if len(lines) != 1 {
				t.Fatalf("workspace cleanup records=%v, want exactly one", lines)
			}
			if !strings.Contains(lines[0], `"level":"WARN"`) {
				t.Fatalf("a refused cleanup must stay an operator call to action: %s", lines[0])
			}
			if !strings.Contains(lines[0], `"status":"`+test.wantStatus+`"`) {
				t.Fatalf("workspace cleanup record=%s, want status %q", lines[0], test.wantStatus)
			}
			if !strings.Contains(lines[0], test.err.Error()) {
				t.Fatalf("workspace cleanup record=%s, want the refusal reason %q", lines[0], test.err)
			}
		})
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
			defer assertInvariants(t, c)
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

// TestReconcileCompletesDespiteAWedgedTerminalCleanup is PMR-180. Reconcile's
// stopTerminal branch cleans up on the poll loop's own context, which has no
// deadline, and internal/workspace bounds its git subprocesses by the caller's
// context and nothing else. So one hung git call -- a filesystem hang, lock
// contention -- used to freeze polling, reconciliation, and stall detection for
// every other issue indefinitely, with the daemon still looking alive while it
// scheduled nothing. The pass must instead give up on workspaceCleanupTimeout
// and report the failure it gave up on.
func TestReconcileCompletesDespiteAWedgedTerminalCleanup(t *testing.T) {
	w := testSettings(t)
	issue := testIssue()
	terminal := issue
	terminal.State = "Done"
	terminal.Dispatchable = false
	tracker := &fakeTracker{issue: issue}
	tracker.setFresh(terminal)
	var log syncBuffer
	ws := &fakeWorkspace{cleanupWedged: true}
	c := New(tracker, &fakeAgent{}, ws, func() config.Settings { return w.Config }, slog.New(slog.NewJSONHandler(&log, nil)))
	defer assertInvariants(t, c)
	// The production bound is fifteen seconds; assert it by electing a short one
	// rather than waiting that out. Only its presence is under test.
	c.cleanupTimeout = 20 * time.Millisecond
	r := &running{issue: issue, session: domain.AgentSession{ID: "s"}, last: c.clock.Now(), cancel: func() {}}
	c.seedRunning(issue, r)

	done := make(chan error, 1)
	go func() { done <- c.reconcile(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a wedged cleanup held the poll goroutine: reconcile never returned, so nothing else would be polled, reconciled, or found stalled")
	}

	lines := cleanupLogLines(log.String())
	if len(lines) != 1 {
		t.Fatalf("workspace cleanup records=%v, want exactly one", lines)
	}
	if !strings.Contains(lines[0], `"level":"WARN"`) || !strings.Contains(lines[0], `"status":"failed"`) {
		t.Fatalf("an abandoned cleanup leaves a workspace behind and must say so: %s", lines[0])
	}
	if !strings.Contains(lines[0], context.DeadlineExceeded.Error()) {
		t.Fatalf("cleanup record=%s, want the bound as the reason it gave up", lines[0])
	}
}

// TestRetryCleanupIsBoundedForAWedgedWorkspace is the same bound on the lesser
// call site: a landing retry whose refresh found the issue already terminal
// (retry.go). A wedge there holds only that timer's goroutine, not the poll
// loop, but the context it inherits is just as deadline-free, so the attempt
// must still end on its own.
func TestRetryCleanupIsBoundedForAWedgedWorkspace(t *testing.T) {
	issue := testIssue()
	var log syncBuffer
	ws := &fakeWorkspace{cleanupWedged: true}
	c := New(&fakeTracker{issue: issue}, &fakeAgent{}, ws, func() config.Settings { return testSettings(t).Config }, slog.New(slog.NewJSONHandler(&log, nil)))
	defer assertInvariants(t, c)
	c.cleanupTimeout = 20 * time.Millisecond

	done := make(chan struct{})
	go func() {
		defer close(done)
		c.cleanupWorkspace(context.Background(), issue)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a wedged cleanup held the retry goroutine forever")
	}

	lines := cleanupLogLines(log.String())
	if len(lines) != 1 || !strings.Contains(lines[0], context.DeadlineExceeded.Error()) {
		t.Fatalf("workspace cleanup records=%v, want one naming the bound it gave up on", lines)
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
	defer assertInvariants(t, c)
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
	defer assertInvariants(t, c)
	forgetter := &stubForgetter{}
	c.SetIssueForgetter(forgetter)

	c.Tick(context.Background())
	<-ws.after
	if got := forgetter.issues(); len(got) != 0 {
		t.Fatalf("still-active issue released=%v, want none", got)
	}
}
