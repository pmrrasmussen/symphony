package agentstream

import (
	"sync"

	"github.com/pmrrasmussen/symphony/internal/domain"
)

// ReservedTerminalSlots keeps space for the terminal event by dropping ordinary
// progress once the buffer is nearly full. One slot is now provably enough,
// because Sink.EmitTerminal admits exactly one terminal event per turn, but the
// second is kept: it costs one buffered progress event and it is what makes the
// reservation hold even if a turn ever gains a second outcome to report.
const ReservedTerminalSlots = 2

// Sink owns a turn's event channel outright: every send, the terminal latch, and
// the close itself happen under one mutex. Nothing about a turn's event stream is
// left to an invariant about which goroutine does what.
//
// That the mutex is the guard, and not the select/default idiom the sends use, is
// the whole point: default covers a full channel, not a closed one, and a send on
// a closed channel panics unrecoverably and process-wide -- one late event would
// kill every parallel session, not just its own turn. Because the close happens
// here too, there is no state in which the channel is closed and the sink is not.
//
// No send ever blocks. A consumer stops reading as soon as it sees a terminal
// event, so ordinary progress is dropped once the buffer is nearly full and the
// terminal event keeps the reserved room.
type Sink struct {
	mu       sync.Mutex
	events   chan domain.Event
	closed   bool
	terminal bool
	// held and pending are how a sink built by NewHeldSink keeps a stream whose
	// opening event is not known yet: everything emitted is buffered, in order,
	// until Activate delivers it. Nothing else about the sink changes -- the
	// latch still admits one outcome, and a held outcome is still that turn's.
	held    bool
	pending []domain.Event
}

// NewSink returns a sink over a fresh channel of the given capacity, delivering
// from the first emit.
func NewSink(buffer int) *Sink {
	return &Sink{events: make(chan domain.Event, buffer)}
}

// NewHeldSink returns a sink that delivers nothing until Activate, for a caller
// whose stream must open with an event it cannot name yet -- the identity of a
// session the child has not reported. Emitting into it is otherwise the same
// operation, so a caller with something to report before then, up to and
// including the turn's outcome, neither blocks nor loses it.
func NewHeldSink(buffer int) *Sink {
	return &Sink{events: make(chan domain.Event, buffer), held: true}
}

// Events is the consumer's end of the stream. It is closed by Close and by
// nothing else.
func (s *Sink) Events() <-chan domain.Event { return s.events }

// Emit reports progress. A terminal event handed to it still goes through the
// latch, so there is no path to an unclaimed outcome.
func (s *Sink) Emit(event domain.Event) {
	if event.Kind.Terminal() {
		s.EmitTerminal(event)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	if s.held {
		// The bound leaves room for Activate's own opening events and for the
		// outcome, so held progress can crowd out neither. Progress after a held
		// outcome is dropped rather than queued behind it: a consumer stops at
		// the terminal event, so delivering it would only reorder the stream.
		if s.terminal || len(s.pending) >= cap(s.events)-ReservedTerminalSlots-1 {
			return
		}
		s.pending = append(s.pending, event)
		return
	}
	if len(s.events) >= cap(s.events)-ReservedTerminalSlots {
		return
	}
	s.sendLocked(event)
}

// EmitTerminal reports the turn's one outcome and reports whether this caller is
// the one that settled it. Claiming and sending are a single operation on
// purpose: a claim that could not deliver its event would leave the turn settled
// with nothing to show for it, and the consumer would then see the stream close
// with no outcome and report "closed before completion" instead of the real
// reason. A second outcome is no better -- Coordinator.consume returns on the
// first, so the run would be recorded as finished under that reason while the
// child kept burning tokens until a cancellation arrived.
//
// On a held sink the outcome is claimed and buffered rather than delivered, and
// Activate is what delivers it. A caller that ends its stream on the terminal
// event must therefore end it on Activate's report too, not on this one alone.
func (s *Sink) EmitTerminal(event domain.Event) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.terminal {
		return false
	}
	s.terminal = true
	if s.held {
		s.pending = append(s.pending, event)
		return true
	}
	s.sendLocked(event)
	return true
}

// Activate releases a held sink: first is delivered ahead of everything held,
// then everything held, in the order it was emitted. It reports whether the
// turn's outcome was among what it delivered, so a caller that ends its stream
// on the terminal event ends it for an outcome claimed before activation too.
//
// first is delivered unconditionally. It is the caller's own opening event on a
// channel nothing has been sent to yet, and the held bound is what keeps the
// room for it.
func (s *Sink) Activate(first ...domain.Event) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.held || s.closed {
		return false
	}
	s.held = false
	pending := s.pending
	s.pending = nil
	for _, event := range first {
		s.sendLocked(event)
	}
	delivered := false
	for _, event := range pending {
		if event.Kind.Terminal() {
			s.sendLocked(event)
			delivered = true
			continue
		}
		if len(s.events) >= cap(s.events)-ReservedTerminalSlots {
			continue
		}
		s.sendLocked(event)
	}
	return delivered
}

// Activated reports whether the sink is delivering rather than holding. It is
// how a caller that ends its stream on the terminal event tells an outcome
// EmitTerminal has delivered from one it has only claimed, and it answers for a
// sink built by NewSink too.
func (s *Sink) Activated() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.held
}

// Settled reports whether the turn's outcome is already spoken for. It is only
// ever a shortcut -- EmitTerminal is what enforces the latch -- so a caller may
// act on it, but must not treat a false as a reservation.
func (s *Sink) Settled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.terminal
}

// Close ends the stream. It closes the channel under the mutex, so an emit is
// either already done or sees closed; neither can be mid-send.
//
// Closing a still-held sink discards what it holds. That is a caller abandoning
// a stream it never opened -- a turn that failed before it could be activated --
// and there is nobody left to flush to.
func (s *Sink) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	s.pending = nil
	close(s.events)
}

func (s *Sink) sendLocked(event domain.Event) {
	select {
	case s.events <- event:
	default:
	}
}
