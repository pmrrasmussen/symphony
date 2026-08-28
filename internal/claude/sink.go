package claude

import (
	"errors"
	"io"
	"os/exec"
	"strconv"
	"sync"

	"github.com/pmrrasmussen/symphony/internal/domain"
	"github.com/pmrrasmussen/symphony/internal/observability"
)

// reservedTerminalSlots keeps space for the terminal event by dropping ordinary
// progress once the buffer is nearly full. One slot is now provably enough,
// because sink.emitTerminal admits exactly one terminal event per turn, but the
// second is kept: it costs one buffered progress event and it is what makes the
// reservation hold even if a turn ever gains a second outcome to report.
const reservedTerminalSlots = 2

// exitText reports an exit status without the child's own output.
func exitText(err error) string {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return "exit status " + strconv.Itoa(exit.ExitCode())
	}
	return "process error"
}

// sink owns a turn's event channel outright: every send, the terminal latch, and
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
type sink struct {
	mu       sync.Mutex
	events   chan domain.Event
	closed   bool
	terminal bool
}

// emit reports progress. A terminal event handed to it still goes through the
// latch, so there is no path to an unclaimed outcome.
func (s *sink) emit(event domain.Event) {
	if event.Kind.Terminal() {
		s.emitTerminal(event)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || len(s.events) >= cap(s.events)-reservedTerminalSlots {
		return
	}
	s.sendLocked(event)
}

// emitTerminal reports the turn's one outcome and reports whether this caller is
// the one that settled it. Claiming and sending are a single operation on
// purpose: a claim that could not deliver its event would leave the turn settled
// with nothing to show for it, and the consumer would then see the stream close
// with no outcome and report "closed before completion" instead of the real
// reason. A second outcome is no better -- Coordinator.consume returns on the
// first, so the run would be recorded as finished under that reason while the
// child kept burning tokens until a cancellation arrived.
func (s *sink) emitTerminal(event domain.Event) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.terminal {
		return false
	}
	s.terminal = true
	s.sendLocked(event)
	return true
}

// settled reports whether the turn's outcome is already spoken for. It is only
// ever a shortcut -- emitTerminal is what enforces the latch -- so a caller may
// act on it, but must not treat a false as a reservation.
func (s *sink) settled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.terminal
}

// close ends the stream. It closes the channel under the mutex, so an emit is
// either already done or sees closed; neither can be mid-send.
func (s *sink) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	close(s.events)
}

func (s *sink) sendLocked(event domain.Event) {
	select {
	case s.events <- event:
	default:
	}
}

// boundedTail keeps only the last bounded, redacted slice of stderr, so a noisy
// child cannot flood a log and no unbounded child output is retained.
type boundedTail struct {
	mu   sync.Mutex
	tail []byte
}

func (b *boundedTail) readFrom(r io.Reader) {
	buf := make([]byte, 4<<10)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			b.mu.Lock()
			b.tail = append(b.tail, buf[:n]...)
			if len(b.tail) > observability.MaxDiagnosticBytes {
				b.tail = b.tail[len(b.tail)-observability.MaxDiagnosticBytes:]
			}
			b.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

func (b *boundedTail) text() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.tail) == 0 {
		return ""
	}
	return observability.Text(string(b.tail))
}
