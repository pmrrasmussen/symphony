// Package coordinator owns all mutable scheduling state.
package coordinator

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/domain"
	"github.com/pmrrasmussen/symphony/internal/observability"
)

// Timer is intentionally small so retries can be driven deterministically in
// tests. Stop must prevent a callback that has not started from running.
type Timer interface {
	AfterFunc(time.Duration, func()) TimerHandle
}

type TimerHandle interface{ Stop() bool }

type realTimer struct{}

func (realTimer) AfterFunc(d time.Duration, f func()) TimerHandle { return time.AfterFunc(d, f) }

type Clock interface{ Now() time.Time }

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

type stopReason string

const (
	stopShutdown     stopReason = "shutdown"
	stopIneligible   stopReason = "ineligible"
	stopTerminal     stopReason = "terminal"
	stopStalled      stopReason = "stalled"
	stopParentCancel stopReason = "parent_cancelled"
)

type Coordinator struct {
	tracker    domain.Tracker
	agent      domain.AgentBackend
	workspaces domain.WorkspaceExecutor
	settings   func() config.Settings
	timer      Timer
	clock      Clock
	log        *slog.Logger
	// cleanupTimeout bounds every workspace cleanup attempt; see
	// workspaceCleanupTimeout, which is the only value production ever gives it.
	// It is a field rather than a plain constant for the same reason Timer is a
	// seam: a test asserts the bound by electing a short one and observing the
	// caller return, not by waiting fifteen seconds out.
	cleanupTimeout time.Duration
	// forget is the optional host integration told that an issue is finished.
	// It is installed once at startup, before Start, and never replaced, so the
	// scheduling goroutines that read it need no lock of their own.
	forget domain.IssueForgetter

	mu sync.Mutex
	// states is the coordinator's single per-issue record, keyed by issue ID.
	// Everything it knows about an issue lives in one owned value, so the
	// relations between a claim, its reservation, its run, its retry timer and
	// its waiting or handoff memory are properties of that value rather than
	// rules re-applied at each call site (PMR-123). An entry exists only while
	// it remembers something; see issueState and pruneLocked.
	states map[string]*issueState
	// admittedCount and admittedByState are the reservation tallies kept in
	// step with states, so both halves of the capacity check --
	// agent.max_concurrent_agents and the per-state limit -- are O(1) reads
	// that cannot drift from the records they describe.
	admittedCount   int
	admittedByState map[string]int
	// nextRetry and nextReservation mint the generations that make a retry
	// timer and an orchestrator reservation identifiable by their owner rather
	// than by the slot they happen to occupy.
	nextRetry       uint64
	nextReservation uint64
	stopping        bool
	ctx             context.Context
	cancel          context.CancelFunc
	wg              sync.WaitGroup
}

func New(t domain.Tracker, a domain.AgentBackend, w domain.WorkspaceExecutor, settings func() config.Settings, logger *slog.Logger) *Coordinator {
	return &Coordinator{
		tracker: t, agent: a, workspaces: w, settings: settings,
		timer: realTimer{}, clock: realClock{}, log: observability.Logger(logger),
		cleanupTimeout: workspaceCleanupTimeout,
		states:         map[string]*issueState{}, admittedByState: map[string]int{},
	}
}

// SetIssueForgetter installs the host integration notified when an issue
// reaches its terminal tracker state. Call it before Start; with none
// installed the coordinator simply reports nothing, exactly as before.
func (c *Coordinator) SetIssueForgetter(f domain.IssueForgetter) { c.forget = f }

func (c *Coordinator) Start(parent context.Context) {
	c.mu.Lock()
	if c.ctx != nil {
		c.mu.Unlock()
		return
	}
	if c.stopping {
		c.mu.Unlock()
		return
	}
	c.ctx, c.cancel = context.WithCancel(parent)
	ctx := c.ctx
	c.wg.Add(1)
	c.mu.Unlock()
	go func() {
		defer c.wg.Done()
		for {
			err := c.tick(ctx)
			d := pollDelay(c.settings().Polling.Interval, err)
			fired := make(chan struct{})
			timer := c.timer.AfterFunc(d, func() { close(fired) })
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-fired:
			}
		}
	}()
}

// Shutdown prevents queued retries, tells every active session to stop, and
// waits only for workers. A non-conforming backend cannot keep the coordinator
// alive indefinitely because cancellation is itself bounded.
func (c *Coordinator) Shutdown(ctx context.Context) error {
	c.mu.Lock()
	c.stopping = true
	if c.cancel != nil {
		c.cancel()
	}
	runs := make([]*running, 0, len(c.states))
	for id, st := range c.states {
		if r := st.run; r != nil && r.stopped == "" {
			r.stopped = stopShutdown
			r.cancel()
			runs = append(runs, r)
		}
		// A pending retry owns nothing but its claim, so shutdown drops the
		// whole claim with it; a running issue keeps its claim until its own
		// worker goroutine releases it. Waiting memory describes an unclaimed
		// candidate and is simply forgotten.
		if st.retry != nil {
			c.releaseLocked(id)
		}
		st.waiting = nil
		st.waitingEscalated = false
		c.pruneLocked(id)
	}
	c.mu.Unlock()
	c.cancelAll(ctx, runs)
	done := make(chan struct{})
	go func() { c.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Coordinator) Tick(ctx context.Context) {
	_ = c.tick(ctx)
}

func (c *Coordinator) context() context.Context {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ctx
}

func (c *Coordinator) isStopping() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stopping
}

func cancellationReason(stopped stopReason, ctx context.Context) stopReason {
	if stopped != "" {
		return stopped
	}
	if ctx.Err() != nil {
		return stopParentCancel
	}
	return ""
}
