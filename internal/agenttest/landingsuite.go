package agenttest

import (
	"context"
	"testing"

	"github.com/pmrrasmussen/symphony/internal/domain"
)

// LandingEnding is how a shared case's turn ends. It names only what the two
// transports genuinely share: a turn that ends by itself, and a turn that stays
// open until the run is stopped. Everything else a turn can do to end -- Codex's
// three protocol notifications, Claude's stream close and its turn budget -- is
// that backend's own finalize trigger and is asserted in its own package.
type LandingEnding int

const (
	// EndsCompleted ends the turn the ordinary successful way for the transport.
	EndsCompleted LandingEnding = iota
	// EndsOpen holds the turn open until the caller stops the run.
	EndsOpen
)

// LandingSpec is the whole of what a shared case asks a backend fixture for.
type LandingSpec struct {
	// Calls is the outcome the child's landing calls must come back with, in
	// order: false is the retryable gate, true is a landing that resolves. Empty
	// means the child never calls the landing capability at all.
	//
	// The outcome is asserted by the child itself, not just arranged by the
	// fixture: a case that means to hit a gate and instead lands would otherwise
	// pass through this suite asserting the wrong thing.
	Calls []bool
	// Ending is how the turn ends.
	Ending LandingEnding
}

// LandingRun is one started session a shared case drives.
type LandingRun struct {
	// Events is the turn's event stream.
	Events <-chan domain.Event
	// AwaitLandingCalls blocks until every call the spec named has come back,
	// which is what puts the session in the state a turn end has work to do in.
	// It fails the test rather than returning if the child never got there.
	AwaitLandingCalls func(t *testing.T)
	// Stop ends the run the way the coordinator does, by cancelling the session,
	// and waits for the child to be reaped.
	Stop func(t *testing.T)
}

// LandingBackend is what a backend package supplies to run this suite: start a
// session against the fixture's remotes, drive its child through the landing
// calls the spec names, and end the turn as the spec says. Nothing else about
// the backend is substituted -- the registry, the providers, the launch, and
// every turn-end path are the production ones.
type LandingBackend interface {
	StartLandingSession(t *testing.T, ctx context.Context, remote *LandingRemote, dir string, spec LandingSpec) LandingRun
}

// RunLandingSuite asserts the host-side half of the deferred landing fallback
// (PMR-46, PMR-78, PMR-86, PMR-95) against one backend fixture.
//
// Everything here is backend-neutral by construction: it lives in
// internal/github (FinalizeLanding, fireDeferredRefusal, the resolved-landing
// latch) and internal/capability, and a backend contributes only the turn end
// that reaches it. internal/github asserts the fallback itself by calling
// FinalizeLanding directly; what is only true of a real session -- and what this
// suite covers -- is that a turn ending reaches that call, with the state a real
// landing attempt left behind it.
func RunLandingSuite(t *testing.T, backend LandingBackend) {
	t.Helper()

	// A turn that hit a retryable landing gate and then ended without landing
	// must return the issue to human review. Without it the issue sits in
	// Merging holding a state-aware capacity slot with nothing scheduled to move
	// it (PMR-86).
	t.Run("a gate and then a turn end returns the issue to review", func(t *testing.T) {
		remote, run := startLanding(t, backend, context.Background(), LandingSpec{Calls: []bool{false}})
		run.AwaitLandingCalls(t)
		DrainEvents(t, run.Events)
		run.Stop(t)
		remote.RequireDeferredRefusal(t)
	})

	// The first negative: the deferred fallback exists for a turn that ended
	// *without* landing, so a turn that merged must never be walked back to
	// review. Getting this wrong would move a Done issue to In Review with its
	// pull request already merged.
	t.Run("a turn end after a resolved landing leaves the issue done", func(t *testing.T) {
		dir := t.TempDir()
		remote := NewLandingRemote(t)
		remote.ReadyToLand()
		run := backend.StartLandingSession(t, context.Background(), remote, dir, LandingSpec{Calls: []bool{true}})
		run.AwaitLandingCalls(t)
		DrainEvents(t, run.Events)
		run.Stop(t)
		transitions, comments, merges := remote.Observed()
		if merges != 1 {
			t.Fatalf("merges=%d, want exactly one", merges)
		}
		if len(transitions) != 1 || transitions[0] != "Done" {
			t.Fatalf("Linear transitions=%v, want exactly one to Done", transitions)
		}
		if len(comments) != 0 {
			t.Fatalf("a resolved landing posted a refusal comment: %v", comments)
		}
	})

	// The second negative. With the bounded-fix feature off, a failing required
	// check is not a retryable gate at all: landing refuses inline, applies the
	// fallback there, and leaves the turn end with nothing to do. The refusal
	// comment is the deferred path's own fingerprint, so its absence is what says
	// the deferred path did not run.
	t.Run("a turn end with the bounded fix off defers nothing", func(t *testing.T) {
		dir := t.TempDir()
		remote := NewLandingRemote(t)
		remote.DisableLandFix()
		run := backend.StartLandingSession(t, context.Background(), remote, dir, LandingSpec{Calls: []bool{false}})
		run.AwaitLandingCalls(t)
		DrainEvents(t, run.Events)
		run.Stop(t)
		transitions, comments, _ := remote.Observed()
		if len(transitions) != 1 || transitions[0] != "In Review" {
			t.Fatalf("Linear transitions=%v, want exactly the one inline refusal", transitions)
		}
		if len(comments) != 0 {
			t.Fatalf("the bounded-fix feature is off, yet the deferred comment was posted: %v", comments)
		}
	})

	// The third negative, and the one that says the deferred transition is a
	// landing fallback rather than a turn-end habit: a session that never
	// attempted a landing must leave the tracker exactly as it found it.
	t.Run("a turn end without a landing attempt transitions nothing", func(t *testing.T) {
		remote, run := startLanding(t, backend, context.Background(), LandingSpec{})
		collected := DrainEvents(t, run.Events)
		run.Stop(t)
		if len(collected) == 0 || collected[len(collected)-1].Kind != domain.EventCompleted {
			t.Fatalf("the turn did not complete: %v", collected)
		}
		transitions, comments, merges := remote.Observed()
		if len(transitions) != 0 || len(comments) != 0 || merges != 0 {
			t.Fatalf("a turn that never attempted a landing produced transitions=%v comments=%v merges=%d", transitions, comments, merges)
		}
	})

	// PMR-95. The coordinator stops a run by cancelling the very context Start
	// was given and only then cancelling the session (Coordinator.stopRun, and
	// Shutdown), so by the time the turn-ended finalizer runs, the run context is
	// already done. A finalizer that inherits it cannot issue the transition at
	// all, and the issue stays in Merging holding a capacity slot with nothing
	// scheduled to move it.
	//
	// The cancellation order here is stopRun's, exactly: cancel the run context,
	// then stop the session on a fresh one.
	t.Run("the deferred transition survives run cancellation", func(t *testing.T) {
		runCtx, cancelRun := context.WithCancel(context.Background())
		remote, run := startLanding(t, backend, runCtx, LandingSpec{Calls: []bool{false}, Ending: EndsOpen})
		run.AwaitLandingCalls(t)
		cancelRun()
		run.Stop(t)
		DrainEvents(t, run.Events)
		remote.RequireDeferredRefusal(t)
	})

	// The negative the landed guard actually exists for, and the one the
	// resolved-landing case above cannot reach: there, no gate is ever hit, so
	// the finalizer short-circuits on retryableGateHit and never reads landed at
	// all.
	//
	// Here the gate is hit, the fix turn's retry merges, and the turn ends with
	// both facts on record. Getting it wrong is the worst outcome in this area: a
	// merged, Done issue walked back to In Review carrying a comment that says
	// landing fix attempts were exhausted.
	t.Run("a turn end after a gate and then a merge leaves the issue done", func(t *testing.T) {
		dir := t.TempDir()
		remote := NewLandingRemote(t)
		remote.RetryableGateThenReady()
		run := backend.StartLandingSession(t, context.Background(), remote, dir, LandingSpec{Calls: []bool{false, true}})
		run.AwaitLandingCalls(t)
		DrainEvents(t, run.Events)
		run.Stop(t)
		transitions, comments, merges := remote.Observed()
		if merges != 1 {
			t.Fatalf("merges=%d, want exactly one", merges)
		}
		if len(transitions) != 1 || transitions[0] != "Done" {
			t.Fatalf("Linear transitions=%v, want exactly one to Done", transitions)
		}
		if len(comments) != 0 {
			t.Fatalf("a merged landing was commented on as a refusal: %v", comments)
		}
	})
}

// startLanding is the two lines every case above shares: a temporary directory
// of its own, a fixture whose required check has already failed, and one started
// session.
func startLanding(t *testing.T, backend LandingBackend, ctx context.Context, spec LandingSpec) (*LandingRemote, LandingRun) {
	t.Helper()
	dir := t.TempDir()
	remote := NewLandingRemote(t)
	return remote, backend.StartLandingSession(t, ctx, remote, dir, spec)
}
