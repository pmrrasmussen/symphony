package agenttest

import (
	"sync"
	"testing"
	"time"
)

// FakeTimer is the timing seam both agent backends accept in place of their real
// timers, so a test can decide when a configured budget elapses instead of
// waiting one out. It replaced the tests that proved a turn timeout by sleeping
// through ten real seconds -- and with them the load-dependent failures those
// tests had, where the assertion depended on how much CPU the test got (PMR-96).
//
// One fake serves both backends because each backend's Timer interface returns
// the builtin func() bool as its stop, rather than a named handle type of its
// own: internal/codex.Timer and internal/claude.Timer are structurally identical,
// and neither package imports this one.
type FakeTimer struct {
	mu     sync.Mutex
	budget []*fakeBudget
}

type fakeBudget struct {
	delay   time.Duration
	fire    func()
	stopped bool
	fired   bool
}

// NewFakeTimer returns a timer that schedules nothing until a test says so.
func NewFakeTimer() *FakeTimer { return &FakeTimer{} }

// AfterFunc records a budget instead of starting a clock. The returned stop
// reports whether it prevented a callback that had not run, exactly as
// time.Timer.Stop does, so a caller that relies on Stop's answer sees the same
// thing it would from a real timer.
func (f *FakeTimer) AfterFunc(d time.Duration, fire func()) func() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	budget := &fakeBudget{delay: d, fire: fire}
	f.budget = append(f.budget, budget)
	return func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		if budget.stopped || budget.fired {
			return false
		}
		budget.stopped = true
		return true
	}
}

// Live reports every budget that is still scheduled -- neither stopped nor fired
// -- in the order it was scheduled. It is what lets a test assert *which* bound
// governs the work in flight, rather than only that some bound eventually fired.
func (f *FakeTimer) Live() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	var live []time.Duration
	for _, budget := range f.budget {
		if !budget.stopped && !budget.fired {
			live = append(live, budget.delay)
		}
	}
	return live
}

// AwaitLive blocks until a budget for exactly d is scheduled and reports every
// live budget at that moment. A backend schedules its budgets from its own
// goroutines, so a test that inspected them without waiting would race the
// schedule rather than the clock. The bound is the same hang guard every other
// wait here uses, and is not an assertion: see await.go.
func (f *FakeTimer) AwaitLive(t *testing.T, d time.Duration) []time.Duration {
	t.Helper()
	deadline := time.Now().Add(hangGuard)
	for {
		live := f.Live()
		for _, delay := range live {
			if delay == d {
				return live
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no %s budget was scheduled; live budgets=%v", d, live)
		}
		time.Sleep(time.Millisecond)
	}
}

// Elapse fires the oldest live budget for exactly d, waiting for one to be
// scheduled first. It runs the callback on the calling goroutine, which a real
// time.AfterFunc runs on one of its own; every callback either of these backends
// schedules is written for a goroutine it does not own, so the difference is not
// one the code under test can observe.
func (f *FakeTimer) Elapse(t *testing.T, d time.Duration) {
	t.Helper()
	f.AwaitLive(t, d)
	f.mu.Lock()
	var elapsed *fakeBudget
	for _, budget := range f.budget {
		if budget.delay == d && !budget.stopped && !budget.fired {
			elapsed = budget
			break
		}
	}
	if elapsed != nil {
		elapsed.fired = true
	}
	f.mu.Unlock()
	if elapsed == nil {
		t.Fatalf("the %s budget was stopped before it could elapse; live budgets=%v", d, f.Live())
	}
	elapsed.fire()
}
