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
	log        *observability.Logger
	// forget is the optional host integration told that an issue is finished.
	// It is installed once at startup, before Start, and never replaced, so the
	// scheduling goroutines that read it need no lock of their own.
	forget domain.IssueForgetter

	mu         sync.Mutex
	running    map[string]*running
	claimed    map[string]bool
	claimState map[string]string
	// admitted contains work that has reserved an orchestrator slot. Unlike a
	// claim, it deliberately excludes delayed retry timers: a timer still owns
	// its issue to prevent duplicate dispatch, but it must not idle a worker
	// slot while it waits.
	admitted map[string]string
	retries  map[string]retryState
	// handoffs records issues Symphony itself drove into the configured review
	// handoff state, keyed by issue ID, so the poll loop can recognize and log
	// an external actor (for example Linear's native GitHub PR automation)
	// reverting that handoff to an active state instead of silently
	// re-dispatching it. It is in-process only and discarded safely on restart.
	handoffs map[string]handoffObservation
	// landingWaits counts consecutive non-terminal landing waits for a claimed
	// issue. It escalates the delayed landing redispatch so a gate that never
	// settles (a genuinely long check run, or a required_checks name that does
	// not match any GitHub job) backs off toward agent.max_retry_backoff_ms
	// instead of respawning a session at the GitHub poll cadence forever. It is
	// cleared with the claim, so any other landing outcome resets it (PMR-78).
	landingWaits map[string]int
	// landingEscalated records, per claimed issue, whether the "landing wait
	// retry scheduled" log has already been raised to Warn once landingWaits
	// crossed the point where landingRetryDelay's backoff saturates at
	// agent.max_retry_backoff_ms (see landingWaitEscalated). It keeps that
	// escalation a one-time signal -- naming a stuck landing once it stops
	// being distinguishable from a slow one -- rather than a Warn on every
	// subsequent poll-cadence wait. Cleared with the claim alongside
	// landingWaits (PMR-116).
	landingEscalated map[string]bool
	// waiting records a Todo issue that is sitting idle for a reason that earns
	// neither a claim nor a retry timer, so no other tracking remembers it
	// (PMR-139, PMR-152): a candidate rejected only for capacity, or one held
	// ineligible by an open blocker relation, is re-evaluated fresh from
	// ListCandidates on the next poll, with nothing else tracking it in the
	// interim. It is keyed by issue ID and cleared the moment the issue is no
	// longer seen in either state -- admitted, unblocked, turned ineligible for
	// some other reason, or dropped from the tracker's candidate list.
	waiting map[string]waitingState
	// waitingEscalated marks, per waiting issue, that the "still waiting" Warn
	// has already fired once, mirroring landingEscalated: an issue that stays
	// stuck keeps logging Info only on the poll it first entered (or changed
	// reason for) the waiting set, not a Warn every poll.
	waitingEscalated map[string]bool
	nextRetry        uint64
	stopping         bool
	ctx              context.Context
	cancel           context.CancelFunc
	wg               sync.WaitGroup
}

func New(t domain.Tracker, a domain.AgentBackend, w domain.WorkspaceExecutor, settings func() config.Settings, logger *slog.Logger) *Coordinator {
	if logger == nil {
		logger = slog.Default()
	}
	return &Coordinator{
		tracker: t, agent: a, workspaces: w, settings: settings,
		timer: realTimer{}, clock: realClock{}, log: observability.FromSlog(logger),
		running: map[string]*running{}, claimed: map[string]bool{},
		claimState: map[string]string{}, admitted: map[string]string{}, retries: map[string]retryState{},
		handoffs: map[string]handoffObservation{}, landingWaits: map[string]int{},
		landingEscalated: map[string]bool{},
		waiting:          map[string]waitingState{}, waitingEscalated: map[string]bool{},
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
	runs := make([]*running, 0, len(c.running))
	for _, r := range c.running {
		if r.stopped == "" {
			r.stopped = stopShutdown
			r.cancel()
			runs = append(runs, r)
		}
	}
	for id, retry := range c.retries {
		if retry.timer != nil {
			retry.timer.Stop()
		}
		delete(c.retries, id)
		delete(c.claimed, id)
		delete(c.claimState, id)
		delete(c.landingWaits, id)
		delete(c.landingEscalated, id)
	}
	for id := range c.waiting {
		delete(c.waiting, id)
		delete(c.waitingEscalated, id)
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
