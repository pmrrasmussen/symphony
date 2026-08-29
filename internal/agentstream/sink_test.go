package agentstream

import (
	"testing"

	"github.com/pmrrasmussen/symphony/internal/domain"
)

const buffer = 64

// TestTheSinkReservesRoomForTheTerminalEvent pins the buffer policy: a consumer
// stops reading at the terminal event, so progress is dropped near the top of the
// buffer, the terminal event still fits, and no emit ever blocks -- a blocked
// emit would leak the emitting goroutine and orphan the child, so this test hangs
// rather than fails if one ever does.
func TestTheSinkReservesRoomForTheTerminalEvent(t *testing.T) {
	s := NewSink(buffer)
	for i := 0; i < 4*buffer; i++ {
		s.Emit(domain.Event{Kind: domain.EventDiagnostic})
	}
	if len(s.Events()) != buffer-ReservedTerminalSlots {
		t.Fatalf("buffered %d progress events, want %d", len(s.Events()), buffer-ReservedTerminalSlots)
	}
	if !s.EmitTerminal(domain.Event{Kind: domain.EventCompleted}) {
		t.Fatal("the terminal event did not fit the room reserved for it")
	}
	// One outcome per turn, by whichever route it is offered.
	if s.EmitTerminal(domain.Event{Kind: domain.EventFailed}) {
		t.Fatal("a second outcome was accepted")
	}
	s.Emit(domain.Event{Kind: domain.EventFailed})
	buffered := len(s.Events())

	// Dropping post-close emits must not disturb what the consumer has yet to
	// read, so the buffer is checked after the close as well.
	s.Close()
	s.Emit(domain.Event{Kind: domain.EventDiagnostic})
	if s.EmitTerminal(domain.Event{Kind: domain.EventFailed}) {
		t.Fatal("an outcome was accepted after the sink closed")
	}
	collected := drain(s)
	if len(collected) != buffered || len(collected) != buffer-ReservedTerminalSlots+1 {
		t.Fatalf("collected %d events, buffered %d: %v", len(collected), buffered, kinds(collected))
	}
	if collected[len(collected)-1].Kind != domain.EventCompleted {
		t.Fatalf("the terminal event did not fit: %v", kinds(collected))
	}
}

// TestAHeldSinkDeliversNothingUntilActivation covers the pre-activation buffer a
// backend needs when its stream must open with an event it cannot name yet: the
// session identity. Everything emitted before then is delivered afterwards, in
// order, behind the opening event.
func TestAHeldSinkDeliversNothingUntilActivation(t *testing.T) {
	s := NewHeldSink(buffer)
	s.Emit(domain.Event{Kind: domain.EventProgress, Message: "before"})
	if s.Activated() {
		t.Fatal("a held sink reported itself activated")
	}
	if len(s.Events()) != 0 {
		t.Fatalf("a held sink delivered %d events", len(s.Events()))
	}
	if s.Activate(domain.Event{Kind: domain.EventSessionStarted}) {
		t.Fatal("activation reported an outcome nothing had emitted")
	}
	s.Emit(domain.Event{Kind: domain.EventProgress, Message: "after"})
	s.Close()
	collected := drain(s)
	if len(collected) != 3 || collected[0].Kind != domain.EventSessionStarted ||
		collected[1].Message != "before" || collected[2].Message != "after" {
		t.Fatalf("collected=%v", collected)
	}
}

// TestAnOutcomeClaimedBeforeActivationIsDeliveredByIt is what keeps a turn whose
// child died between the launch and the session identity from closing its stream
// with no outcome at all: EmitTerminal claims it, and activation is what reports
// that it has now been delivered, so the caller ends the stream then rather than
// before the opening event was ever sent.
func TestAnOutcomeClaimedBeforeActivationIsDeliveredByIt(t *testing.T) {
	s := NewHeldSink(buffer)
	if !s.EmitTerminal(domain.Event{Kind: domain.EventFailed, Message: "exited"}) {
		t.Fatal("a held sink refused the turn's only outcome")
	}
	if !s.Settled() {
		t.Fatal("an outcome claimed before activation left the turn unsettled")
	}
	// Progress emitted behind a claimed outcome would only reorder a stream the
	// consumer stops reading at that outcome.
	s.Emit(domain.Event{Kind: domain.EventProgress, Message: "too late"})
	if len(s.Events()) != 0 {
		t.Fatalf("a held sink delivered %d events", len(s.Events()))
	}
	if !s.Activate(domain.Event{Kind: domain.EventSessionStarted}) {
		t.Fatal("activation did not report delivering the outcome it held")
	}
	s.Close()
	collected := drain(s)
	if len(collected) != 2 || collected[0].Kind != domain.EventSessionStarted ||
		collected[1].Kind != domain.EventFailed {
		t.Fatalf("collected=%v", kinds(collected))
	}
}

// TestAFloodedHeldSinkStillFitsItsOpeningEventAndItsOutcome pins the held bound:
// it is one slot tighter than the delivered one, because activation's own
// opening event has to fit ahead of everything held.
func TestAFloodedHeldSinkStillFitsItsOpeningEventAndItsOutcome(t *testing.T) {
	s := NewHeldSink(buffer)
	for i := 0; i < 4*buffer; i++ {
		s.Emit(domain.Event{Kind: domain.EventDiagnostic})
	}
	if !s.EmitTerminal(domain.Event{Kind: domain.EventCompleted}) {
		t.Fatal("a flooded held sink refused the turn's outcome")
	}
	s.Activate(domain.Event{Kind: domain.EventSessionStarted})
	s.Close()
	collected := drain(s)
	if len(collected) != buffer-ReservedTerminalSlots+1 {
		t.Fatalf("collected %d events: %v", len(collected), kinds(collected))
	}
	if collected[0].Kind != domain.EventSessionStarted {
		t.Fatalf("the opening event was crowded out: %v", kinds(collected))
	}
	if collected[len(collected)-1].Kind != domain.EventCompleted {
		t.Fatalf("the outcome was crowded out: %v", kinds(collected))
	}
}

func drain(s *Sink) []domain.Event {
	var collected []domain.Event
	for event := range s.Events() {
		collected = append(collected, event)
	}
	return collected
}

func kinds(events []domain.Event) []domain.EventKind {
	out := make([]domain.EventKind, 0, len(events))
	for _, event := range events {
		out = append(out, event.Kind)
	}
	return out
}
