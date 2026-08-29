package claude

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pmrrasmussen/symphony/internal/domain"
)

// TestOnlyOneTerminalEventIsReported keeps the documented contract true: a
// refused init ends the turn, and a result arriving afterwards must not add a
// second terminal event -- which also reported the wrong reason.
func TestOnlyOneTerminalEventIsReported(t *testing.T) {
	dir := t.TempDir()
	script := writeFakeClaude(t, dir, "cat <<'EOF'\n"+refusedInitLine(dir)+"\n"+resultLine(false, "")+"\nEOF\n")
	backend := New(settingsFunc())
	_, events, err := backend.Start(context.Background(), request(t, dir, script))
	if err != nil {
		t.Fatal(err)
	}
	collected := drain(t, events)
	terminals := terminalEvents(collected)
	if len(terminals) != 1 {
		t.Fatalf("%d terminal events: %v", len(terminals), kinds(collected))
	}
	if !strings.Contains(terminals[0].Message, "permission mode") {
		t.Fatalf("terminal event misreports the reason: %+v", terminals[0])
	}
}

// TestEmitsFromOtherGoroutinesSurviveTheTurnShutdown is the reason the sink
// exists. Nothing structurally confines emitting to the read loop, and a send
// landing after close(events) panics -- unrecoverably and process-wide, so one
// late event from one turn would kill every parallel session.
func TestEmitsFromOtherGoroutinesSurviveTheTurnShutdown(t *testing.T) {
	dir := t.TempDir()
	script := writeFakeClaude(t, dir, waitForGate(dir)+"cat <<'EOF'\n"+
		initLine(dir, allCodingTools)+"\n"+resultLine(false, "")+"\nEOF\n")

	backend := New(settingsFunc())
	session, events, err := backend.Start(context.Background(), request(t, dir, script))
	if err != nil {
		t.Fatal(err)
	}
	live := liveTurn(t, backend, session.ID)

	// The emitters keep going across the shutdown they cannot see coming, which
	// is what puts a send in the window the guard has to cover.
	stop := make(chan struct{})
	var emitters sync.WaitGroup
	for i := 0; i < 32; i++ {
		emitters.Add(1)
		go func() {
			defer emitters.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				live.sink.Emit(domain.Event{Kind: domain.EventDiagnostic, At: time.Now(), Message: "out of band"})
			}
		}()
	}

	openGate(t, dir)
	collected := drain(t, events)
	close(stop)
	emitters.Wait()

	// drain returned, so the channel is closed: these two emits are certainly
	// post-close rather than incidentally so, and must be dropped.
	live.sink.Emit(domain.Event{Kind: domain.EventDiagnostic, At: time.Now(), Message: "after close"})
	if live.sink.EmitTerminal(domain.Event{Kind: domain.EventFailed, At: time.Now(), Message: "after close"}) {
		t.Fatal("an outcome was accepted after the stream closed")
	}
	if _, open := <-events; open {
		t.Fatal("an emit after the stream closed reached the channel")
	}

	// The flood must not have crowded out the turn's own outcome: that is what
	// the reserved slots are for.
	if terminals := terminalEvents(collected); len(terminals) != 1 || terminals[0].Kind != domain.EventCompleted {
		t.Fatalf("terminal events=%v", kinds(terminals))
	}
}

// TestATerminalEventClaimedOffTheReadLoopSuppressesTheResult covers the second
// half of the same problem: with the latch on the turn rather than in stream's
// locals, a terminal event raised elsewhere is seen by the read loop, which
// would otherwise report the arriving result as a second outcome -- and the
// coordinator, which returns on the first, would record the run as settled while
// the child kept running.
func TestATerminalEventClaimedOffTheReadLoopSuppressesTheResult(t *testing.T) {
	dir := t.TempDir()
	script := writeFakeClaude(t, dir, "cat <<'EOF'\n"+initLine(dir, allCodingTools)+"\nEOF\n"+
		waitForGate(dir)+"cat <<'EOF'\n"+resultLine(false, "")+"\nEOF\n")

	backend := New(settingsFunc())
	session, events, err := backend.Start(context.Background(), request(t, dir, script))
	if err != nil {
		t.Fatal(err)
	}
	live := liveTurn(t, backend, session.ID)

	// Waiting for the session start proves the read loop is running, so the
	// claim below races nothing and the result really does arrive afterwards.
	var collected []domain.Event
	for event := range events {
		collected = append(collected, event)
		if event.Kind != domain.EventSessionStarted {
			continue
		}
		// EventLandingResolved stands in for what the capability bridge will
		// report; this backend produces no such event on its own today. Claiming
		// and emitting are one operation, so an out-of-band outcome cannot settle
		// the turn and then fail to deliver -- which would close the stream with
		// no outcome at all and lose the reason entirely.
		if !live.sink.EmitTerminal(domain.Event{Kind: domain.EventLandingResolved, At: time.Now(), Message: "landed out of band"}) {
			t.Fatal("the turn was already settled before anything ended it")
		}
		openGate(t, dir)
	}

	terminals := terminalEvents(collected)
	if len(terminals) != 1 {
		t.Fatalf("%d terminal events: %v", len(terminals), kinds(collected))
	}
	if terminals[0].Kind != domain.EventLandingResolved || terminals[0].Message != "landed out of band" {
		t.Fatalf("terminal event=%+v", terminals[0])
	}
}

// TestTwoRefusedInitLinesReportOneFailure covers a double terminal event that
// needed no second goroutine at all. The refused-init branch used to emit
// unconditionally -- only the result branch was guarded -- and both lines arrive
// in one read, which t.kill() cannot take back because closing the pipe does not
// discard bytes the reader already holds. So a doubly refused init reported two
// EventFailed, and Coordinator.consume acted on the first while the child was
// still being killed.
func TestTwoRefusedInitLinesReportOneFailure(t *testing.T) {
	dir := t.TempDir()
	script := writeFakeClaude(t, dir, "cat <<'EOF'\n"+refusedInitLine(dir)+"\n"+refusedInitLine(dir)+"\n"+
		resultLine(false, "")+"\nEOF\n")

	backend := New(settingsFunc())
	_, events, err := backend.Start(context.Background(), request(t, dir, script))
	if err != nil {
		t.Fatal(err)
	}
	collected := drain(t, events)
	terminals := terminalEvents(collected)
	if len(terminals) != 1 {
		t.Fatalf("%d terminal events: %v", len(terminals), kinds(collected))
	}
	if terminals[0].Kind != domain.EventFailed || !strings.Contains(terminals[0].Message, "permission mode") {
		t.Fatalf("terminal event=%+v", terminals[0])
	}
}
