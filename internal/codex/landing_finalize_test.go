package codex

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pmrrasmussen/symphony/internal/agenttest"
	"github.com/pmrrasmussen/symphony/internal/domain"
)

// This file covers the wiring from a turn ending to the deferred
// Merging -> In Review landing transition (PMR-46, PMR-78) on the app-server
// transport: the fallback that returns an issue to human review when a turn
// ended after a retryable landing gate without a landing.
//
// What is host-side about that -- the fallback itself, the resolved-landing
// latch, the negatives -- is asserted once, in internal/agenttest's shared
// landing suite, which this file runs against a fixture of this transport's.
// What is left here is this transport's own finalize trigger: client.turn's
// three protocol turn endings, and its turn timeout, which is not a turn end at
// all -- and, because that timeout is the one path here that can observe a
// capability call in flight, where a drained call's outcome ranks against the
// reason the budget would otherwise have reported (PMR-177).
//
// Nothing is substituted but the two remotes (one httptest server) and the two
// child processes (the scripted app-server, and a scripted git). The backend
// builds its own registry from a real linear.Handoff and a real github.Manager.

// landingScript is the app-server transcript a landing session runs: it
// completes the handshake, makes one github_land_pr call per requested outcome,
// asserts each came back with the outcome the case arranged, records where the
// test can wait for it, and then ends the turn however the caller asked.
//
// Asserting the outcome inside the child is what keeps a case honest: one that
// means to hit a retryable gate and instead lands would otherwise run its whole
// assertion set against the wrong session.
func landingScript(dir string, calls []bool, ending string) string {
	script := `
IFS= read -r line
printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{}}'
IFS= read -r line
IFS= read -r line
`
	if len(calls) > 0 {
		script += "case \"$line\" in *github_land_pr*) ;; *) exit 30;; esac\n"
	}
	script += `printf '%s\n' '{"jsonrpc":"2.0","id":2,"result":{"thread":{"id":"thread-1"}}}'
IFS= read -r line
printf '%s\n' '{"jsonrpc":"2.0","id":3,"result":{"turn":{"id":"turn-1"}}}'
`
	for i, wantSuccess := range calls {
		id := strconv.Itoa(99 + i)
		script += `printf '%s\n' '{"jsonrpc":"2.0","id":` + id + `,"method":"item/tool/call","params":{"tool":"github_land_pr","arguments":{}}}'
IFS= read -r line
case "$line" in *'"success":` + strconv.FormatBool(wantSuccess) + `'*) ;; *) exit 31;; esac
printf '%s\n' "$line" > ` + landingMarker(dir, i) + "\n"
	}
	return script + ending
}

// landingMarker is where the child records the outcome of one landing call.
func landingMarker(dir string, call int) string {
	return filepath.Join(dir, "landed-"+strconv.Itoa(call))
}

const (
	turnCompleted = `printf '%s\n' '{"jsonrpc":"2.0","method":"turn/completed","params":{}}'` + "\n"
	turnFailed    = `printf '%s\n' '{"jsonrpc":"2.0","method":"turn/failed","params":{}}'` + "\n"
	turnCancelled = `printf '%s\n' '{"jsonrpc":"2.0","method":"turn/cancelled","params":{}}'` + "\n"
	turnHangs     = "sleep 120\n"
)

// landingBackend is this transport's fixture for the shared landing suite: it
// starts a real session whose scripted app-server makes the landing calls the
// spec asks for and ends the turn as it asks.
type landingBackend struct{}

func (landingBackend) StartLandingSession(t *testing.T, ctx context.Context, remote *agenttest.LandingRemote, dir string, spec agenttest.LandingSpec) agenttest.LandingRun {
	t.Helper()
	ending := turnCompleted
	if spec.Ending == agenttest.EndsOpen {
		ending = turnHangs
	}
	b, session, events := landingSession(t, ctx, remote, dir, landingScript(dir, spec.Calls, ending))
	return agenttest.LandingRun{
		Events:            events,
		AwaitLandingCalls: func(t *testing.T) { awaitLandingCalls(t, dir, len(spec.Calls)) },
		Stop:              func(t *testing.T) { settle(t, b, session) },
	}
}

// landingSession starts one session against the fixture's remotes with the given
// app-server transcript.
func landingSession(t *testing.T, ctx context.Context, remote *agenttest.LandingRemote, dir, transcript string) (*Backend, domain.AgentSession, <-chan domain.Event) {
	t.Helper()
	agenttest.WriteFakeGit(t, dir)
	script := writeAppServer(t, dir, transcript)
	b := integratedBackend(remote.SettingsFunc())
	r := request(dir, script)
	r.Issue = remote.Issue()
	session, events, err := b.Start(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	return b, session, events
}

// awaitLandingCalls blocks until the child has had every github_land_pr call
// answered, which is what puts the session in the only state a turn end has work
// to do in.
func awaitLandingCalls(t *testing.T, dir string, calls int) {
	t.Helper()
	for i := range calls {
		agenttest.AwaitFile(t, landingMarker(dir, i), func(body string) bool { return strings.Contains(body, `"success"`) },
			"the child never reported an answered github_land_pr call")
	}
}

// settle ends the session the way the coordinator always does, and waits for the
// child to be reaped so a test's temporary directory outlives it.
//
// On a path that has already cancelled the session it is a pure no-op -- Cancel
// deleted the session from the backend's map and the second call returns on the
// nil client -- so it asserts nothing there. Where it does assert something is
// after a protocol turn end: Cancel is a second turn-end path over the same
// session, and every count these tests make is still "exactly one" after it.
func settle(t *testing.T, b *Backend, session domain.AgentSession) {
	t.Helper()
	if err := b.Cancel(context.Background(), session); err != nil {
		t.Fatalf("cancel: %v", err)
	}
}

// TestTheHostSideLandingBehaviourHoldsOverTheAppServer runs the shared landing
// suite against this transport. The suite is what asserts the host-side half
// once; this test is what says a real app-server session reaches it.
func TestTheHostSideLandingBehaviourHoldsOverTheAppServer(t *testing.T) {
	agenttest.RunLandingSuite(t, landingBackend{})
}

// TestEveryTurnEndPathFiresTheDeferredLandingTransition is this transport's own
// finalize trigger, and the coverage PMR-86 exists for: client.finalizeLanding
// has three call sites, and a turn that hit a retryable landing gate must return
// the issue to human review through every one of them.
//
// The case set is this transport's own and is deliberately not the Claude file's:
// the app-server ends a turn with one of three protocol notifications, which that
// transport has no equivalent of, and a hard Cancel is the one ending both share.
// The turn timeout is covered separately, because here it is not a turn end at
// all -- see TestATimedOutTurnFinalizesOnlyWhenTheRunIsStopped.
func TestEveryTurnEndPathFiresTheDeferredLandingTransition(t *testing.T) {
	for name, test := range map[string]struct {
		ending string
		cancel bool
	}{
		"turn/completed": {ending: turnCompleted},
		"turn/failed":    {ending: turnFailed},
		"turn/cancelled": {ending: turnCancelled},
		"hard cancel":    {ending: turnHangs, cancel: true},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			remote := agenttest.NewLandingRemote(t)
			b, session, events := landingSession(t, context.Background(), remote, dir, landingScript(dir, []bool{false}, test.ending))
			awaitLandingCalls(t, dir, 1)
			if test.cancel {
				// Cancel is what the coordinator calls for a run it is stopping.
				// It must have finalized the landing by the time it returns: the
				// coordinator has no other handle on the session left afterwards.
				settle(t, b, session)
				transitions, _, _ := remote.Observed()
				if len(transitions) != 1 || transitions[0] != "In Review" {
					t.Fatalf("cancel returned with Linear transitions=%v, want exactly one to In Review", transitions)
				}
			}
			agenttest.DrainEvents(t, events)
			settle(t, b, session)
			remote.RequireDeferredRefusal(t)
		})
	}
}

// TestATimedOutTurnFinalizesOnlyWhenTheRunIsStopped is this transport's turn
// timeout, and it is a separate test because the timeout is not a turn end here.
// client.turn's budget emits a terminal failure and kills the child; it does not
// finalize. So the timeout leaves the deferred transition owed, and the Cancel
// the coordinator issues for that terminal event is what pays it -- against a
// session whose process is already gone, which is the state no other case here
// produces.
//
// Both halves are asserted, because only the pair says where the transition came
// from: nothing after the timeout, everything after the Cancel.
//
// The budget is elapsed rather than waited out, and that is what makes the
// ordering exact rather than merely likely. The landing call has to have come
// back before the timeout fires -- a timeout that fired mid-call would leave no
// gate hit and nothing to assert -- and the old test bought that ordering with a
// ten-second real budget it then sat through, which a loaded machine could still
// have beaten.
func TestATimedOutTurnFinalizesOnlyWhenTheRunIsStopped(t *testing.T) {
	dir := t.TempDir()
	remote := agenttest.NewLandingRemote(t)
	agenttest.WriteFakeGit(t, dir)
	script := writeAppServer(t, dir, landingScript(dir, []bool{false}, turnHangs))
	b := integratedBackend(remote.SettingsFunc())
	timer := timedBackend(t, b)
	r := request(dir, script)
	r.Issue = remote.Issue()
	r.TurnTimeout = 90 * time.Second
	session, events, err := b.Start(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	awaitLandingCalls(t, dir, 1)

	timer.Elapse(t, r.TurnTimeout)
	// The stream closes when the turn timeout ends the turn, so draining it is
	// how this waits for that outcome to be reported.
	collected := agenttest.DrainEvents(t, events)
	if len(collected) == 0 || collected[len(collected)-1].Kind != domain.EventFailed {
		t.Fatalf("the turn timeout did not end the turn: %v", collected)
	}
	if transitions, comments, _ := remote.Observed(); len(transitions) != 0 || len(comments) != 0 {
		t.Fatalf("the timeout itself finalized: transitions=%v comments=%v", transitions, comments)
	}

	settle(t, b, session)
	remote.RequireDeferredRefusal(t)
}

// TestATimedOutTurnReportsAnInFlightLandingsOutcomeInsteadOfTheTimeout is
// PMR-177: the turn budget expires while github_land_pr is mid-merge, and the
// merge succeeds while the budget's own callback is draining that call.
//
// Both terminal events are real, and the first one detaches the stream, so which
// one gets there decides what the coordinator records. The landing's has to win:
// it is the run's actual outcome, and the timeout is only what to say when the
// drained call produced no outcome at all. Emit the timeout first and the run is
// recorded as failed against a pull request that merged, and the issue is
// redispatched into a landing attempt with nothing left to land.
//
// The ordering is exact rather than likely in both places it has to be. The
// fixture parks the merge, so the call is provably in flight when the budget is
// elapsed; and the budget is elapsed on a goroutine of its own so that the merge
// is released only once the drain's own bound is scheduled, which is the
// observable edge of a callback that has reached the drain and not left it.
func TestATimedOutTurnReportsAnInFlightLandingsOutcomeInsteadOfTheTimeout(t *testing.T) {
	dir := t.TempDir()
	remote := agenttest.NewLandingRemote(t)
	remote.ReadyToLand()
	reached, release := remote.PauseAtMerge()
	agenttest.WriteFakeGit(t, dir)
	// The child holds the turn open once its landing call is answered, so the
	// budget is the only thing that can end this turn.
	script := writeAppServer(t, dir, landingScript(dir, []bool{true}, turnHangs))
	b := integratedBackend(remote.SettingsFunc())
	timer := timedBackend(t, b)
	r := request(dir, script)
	r.Issue = remote.Issue()
	r.TurnTimeout = 90 * time.Second
	session, events, err := b.Start(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	agenttest.Await(t, reached, "the landing never reached its merge")

	fired := timer.ElapseAsync(t, r.TurnTimeout)
	timer.AwaitLive(t, invocationDrain)
	release()
	agenttest.Await(t, fired, "the elapsed turn budget never returned")

	collected := agenttest.DrainEvents(t, events)
	outcome := collected[len(collected)-1]
	if outcome.Kind != domain.EventLandingResolved {
		t.Fatalf("the turn's outcome was %+v, want the drained landing's resolution", outcome)
	}
	// The budget really did expire, so the fallback it would have reported is
	// left behind rather than merely reordered: one terminal event per turn.
	for _, event := range collected {
		if event.Kind == domain.EventFailed {
			t.Fatalf("the turn also reported a failure: %+v", event)
		}
	}
	settle(t, b, session)
	transitions, comments, merges := remote.Observed()
	if merges != 1 {
		t.Fatalf("merges=%d, want exactly one", merges)
	}
	if len(transitions) != 1 || transitions[0] != "Done" {
		t.Fatalf("Linear transitions=%v, want exactly one to Done", transitions)
	}
	if len(comments) != 0 {
		t.Fatalf("the timed-out turn commented on a merged landing as a refusal: %v", comments)
	}
}
