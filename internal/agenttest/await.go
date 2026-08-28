package agenttest

import (
	"os"
	"testing"
	"time"

	"github.com/pmrrasmussen/symphony/internal/domain"
)

// This file holds the three ways a test here waits for something a real session
// does on its own schedule: a value on a channel, a marker file a scripted child
// writes, and a turn's event stream closing.
//
// Every one of them is bounded, and none of the bounds is an assertion. A bare
// receive or a bare `for range events` turns a wedge into a package timeout with
// no failing test named, which is the one failure mode a wiring test must not
// have -- and the mutation that removes the bound under test is exactly what
// produces such a wedge. The deadlines are hang guards only: nothing here is
// timed, so a slow machine merely gets there later.

// hangGuard is the patience every wait in this file allows. It is generous
// because a turn end does real work before its stream closes -- a revocation
// that drains an invocation still in flight, then up to three tracker round
// trips -- and because what these tests assert is what the tracker was told,
// never how quickly. The real cost of the slowest wait here is well under a
// second, so this is two orders of magnitude of headroom.
//
// It is not larger than this because these suites are run under `go test
// -timeout 5m`: a change that wedges several waits at once must still report
// each against its own test rather than adding up to a package timeout, which
// names nothing.
const hangGuard = 30 * time.Second

// Await receives one value from ch under the hang guard. describe names what the
// test was waiting for, so a wedge is reported against this test rather than as
// a package timeout.
func Await[T any](t *testing.T, ch <-chan T, describe string) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(hangGuard):
		t.Fatalf("%s", describe)
		var zero T
		return zero
	}
}

// DrainEvents drains a turn's event stream until it closes, and reports every
// event it saw. A closed stream is how a backend says the turn is over, so this
// is also how a test waits for that.
func DrainEvents(t *testing.T, events <-chan domain.Event) []domain.Event {
	t.Helper()
	var collected []domain.Event
	timeout := time.After(hangGuard)
	for {
		select {
		case event, ok := <-events:
			if !ok {
				return collected
			}
			collected = append(collected, event)
		case <-timeout:
			t.Fatalf("event stream did not close; collected %d events", len(collected))
		}
	}
}

// AwaitFile blocks until path holds contents that satisfy ok, which is how a
// fixture waits for a marker its scripted child writes.
func AwaitFile(t *testing.T, path string, ok func(string) bool, describe string) {
	t.Helper()
	deadline := time.Now().Add(hangGuard)
	for {
		body, err := os.ReadFile(path)
		if err == nil && len(body) > 0 && ok(string(body)) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s (marker %s: err=%v contents=%q)", describe, path, err, body)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
