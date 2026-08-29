package coordinator

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/pmrrasmussen/symphony/internal/domain"
)

// immediateTimer fires every callback the moment it is armed, on its own
// goroutine, and can never cancel one.
//
// It is the adversarial version of the production Timer seam. In production a
// redispatch cannot realistically beat the handful of statements a finishing
// worker still has to run, because backoff() floors at ten seconds and
// landingRetryDelay at the GitHub poll interval -- but that is a coincidence
// of the delay constants, not an invariant, and this seam exists precisely so
// a test can remove it. With the delay gone, a redispatch's fresh reservation
// and the outgoing worker's own release overlap on every episode, which is the
// window PMR-121 described.
type immediateTimer struct {
	mu    sync.Mutex
	armed int
}

func (t *immediateTimer) AfterFunc(_ time.Duration, callback func()) TimerHandle {
	t.mu.Lock()
	t.armed++
	t.mu.Unlock()
	go callback()
	return immediateTimerHandle{}
}

func (t *immediateTimer) count() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.armed
}

type immediateTimerHandle struct{}

func (immediateTimerHandle) Stop() bool { return false }

// TestUnreserveReleasesOnlyTheReservationItOwns pins the ownership rule
// directly (PMR-121): launch's deferred release names the generation it
// reserved, so a release arriving after the same issue has been redispatched
// leaves the redispatch's own slot alone. Before the reservation carried an
// owner, this sequence deleted whatever occupied the slot, leaving the issue
// running with no reservation and the coordinator free to admit one worker
// over agent.max_concurrent_agents.
func TestUnreserveReleasesOnlyTheReservationItOwns(t *testing.T) {
	w := testSettings(t) // max_concurrent_agents: 1
	issue := testIssue()
	c := testCoordinator(w.Config, &fakeTracker{issue: issue}, &fakeAgent{}, &fakeWorkspace{})
	defer assertInvariants(t, c)

	if !claims(c, issue, w.Config) {
		t.Fatal("issue was not claimed")
	}
	c.mu.Lock()
	outgoing := c.reserveLocked(issue, w.Config)
	c.mu.Unlock()
	if outgoing == 0 {
		t.Fatal("first launch could not reserve the only slot")
	}

	// The landing-wait path: the worker frees its slot explicitly, the retry
	// timer fires and redispatches, and only then does the outgoing worker's
	// deferred release run.
	c.unreserve(issue.ID, outgoing)
	c.mu.Lock()
	redispatch := c.reserveLocked(issue, w.Config)
	c.mu.Unlock()
	if redispatch == 0 || redispatch == outgoing {
		t.Fatalf("redispatch reservation=%d, want a fresh generation distinct from %d", redispatch, outgoing)
	}
	c.unreserve(issue.ID, outgoing)

	if got := c.admittedTotal(); got != 1 {
		t.Fatalf("admitted=%d, want the redispatch's own reservation still held", got)
	}
	if got := c.admittedInState("todo"); got != 1 {
		t.Fatalf("admitted in todo=%d, want the redispatch still counted against the per-state limit", got)
	}
	other := testIssue()
	other.ID, other.Identifier = "other", "ENG-2"
	if claims(c, other, w.Config) {
		t.Fatal("a stale release freed a slot the redispatch still owned, admitting one worker over max_concurrent_agents")
	}

	// The redispatch's own release is the one that frees the slot, exactly once.
	c.unreserve(issue.ID, redispatch)
	if got := c.admittedTotal(); got != 0 {
		t.Fatalf("admitted=%d after the owning release, want the slot free", got)
	}
	c.release(issue.ID)
}

// churnAgent dispatches an issue `episodes` times without finishing it -- as a
// landing wait, or as a start failure -- and completes it on the next
// dispatch, marking it terminal in the tracker so the churn ends. It exists to
// drive many redispatches of the same issue through one coordinator quickly.
type churnAgent struct {
	mu        sync.Mutex
	tracker   *issueMapTracker
	episodes  int
	failStart bool
	starts    map[string]int
	total     int
}

func (a *churnAgent) Start(_ context.Context, r domain.AgentRequest) (domain.AgentSession, <-chan domain.Event, error) {
	a.mu.Lock()
	a.total++
	a.starts[r.Issue.ID]++
	attempt, total := a.starts[r.Issue.ID], a.total
	fail := a.failStart && attempt <= a.episodes
	a.mu.Unlock()
	if attempt > a.episodes {
		done := r.Issue
		done.State = "Done"
		a.tracker.setIssue(done)
		ch := make(chan domain.Event, 1)
		ch <- domain.Event{Kind: domain.EventCompleted, At: time.Now()}
		close(ch)
		return domain.AgentSession{ID: fmt.Sprintf("session-%d", total)}, ch, nil
	}
	if fail {
		return domain.AgentSession{}, nil, errors.New("agent binary not found")
	}
	ch := make(chan domain.Event, 1)
	ch <- domain.Event{Kind: domain.EventLandingWaiting, At: time.Now(), Message: "required checks are pending"}
	close(ch)
	return domain.AgentSession{ID: fmt.Sprintf("session-%d", total)}, ch, nil
}

func (a *churnAgent) Continue(context.Context, domain.AgentSession, string) (<-chan domain.Event, error) {
	return closedEvents(), nil
}

func (a *churnAgent) Cancel(context.Context, domain.AgentSession) error { return nil }

func (a *churnAgent) dispatched(id string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.starts[id]
}

// TestImmediateRedispatchNeverExceedsCapacity drives the same two paths
// PMR-121 named -- a landing-wait redispatch and a failure redispatch -- with
// every retry timer firing instantly, so each redispatch overlaps the release
// of the worker it replaces, and watches the capacity accounting throughout.
// Neither bound may ever be crossed: not agent.max_concurrent_agents, and not
// the per-state limit, which is the one an over-admission breaks first because
// it is the smaller of the two.
func TestImmediateRedispatchNeverExceedsCapacity(t *testing.T) {
	for _, tc := range []struct {
		name      string
		failStart bool
	}{
		{name: "landing wait redispatch"},
		{name: "failure redispatch", failStart: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const maxConcurrent, perState = 2, 1
			w := testSettings(t)
			w.Config.Agent.MaxConcurrent = maxConcurrent
			w.Config.Agent.ByState = map[string]int{"todo": perState}
			// High enough that no episode is abandoned mid-test: this is about
			// capacity accounting under redispatch, not the retry ceiling.
			w.Config.Agent.MaxAttempts = 100
			w.Config.GitHub.PollInterval = time.Second

			first, second := testIssue(), testIssue()
			first.ID, first.Identifier = "first", "ENG-1"
			second.ID, second.Identifier = "second", "ENG-2"
			tracker := &issueMapTracker{
				candidates: []domain.Issue{first, second},
				issues:     map[string]domain.Issue{first.ID: first, second.ID: second},
			}
			agent := &churnAgent{tracker: tracker, episodes: 24, failStart: tc.failStart, starts: map[string]int{}}
			c := testCoordinator(w.Config, tracker, agent, &fakeWorkspace{})
			defer assertInvariants(t, c)
			timer := &immediateTimer{}
			c.timer = timer

			// Sample the accounting continuously rather than only at rest: the
			// window this covers is open for a few statements at a time.
			stop := make(chan struct{})
			var sampler sync.WaitGroup
			var breach struct {
				sync.Mutex
				err error
			}
			record := func(err error) {
				breach.Lock()
				if breach.err == nil {
					breach.err = err
				}
				breach.Unlock()
			}
			sampler.Add(1)
			go func() {
				defer sampler.Done()
				for {
					select {
					case <-stop:
						return
					default:
					}
					if admitted := c.admittedTotal(); admitted > maxConcurrent {
						record(fmt.Errorf("admitted=%d exceeds max_concurrent_agents=%d", admitted, maxConcurrent))
					}
					if todo := c.admittedInState("todo"); todo > perState {
						record(fmt.Errorf("admitted in todo=%d exceeds the per-state limit=%d", todo, perState))
					}
					// Bounded by the global limit rather than the per-state one:
					// a run whose issue has moved on to another state is still
					// running, and is counted against that state instead.
					if running := c.runningCount(); running > maxConcurrent {
						record(fmt.Errorf("running=%d exceeds max_concurrent_agents=%d", running, maxConcurrent))
					}
					if claimed := c.Snapshot().Claimed; claimed > maxConcurrent {
						record(fmt.Errorf("claimed=%d exceeds max_concurrent_agents=%d", claimed, maxConcurrent))
					}
					if err := c.checkInvariants(); err != nil {
						record(err)
					}
				}
			}()

			deadline := time.Now().Add(15 * time.Second)
			for agent.dispatched(first.ID) <= agent.episodes || agent.dispatched(second.ID) <= agent.episodes {
				c.Tick(context.Background())
				if time.Now().After(deadline) {
					t.Fatalf("issues never finished their episodes: first=%d second=%d", agent.dispatched(first.ID), agent.dispatched(second.ID))
				}
			}
			waitForRelease(t, c, first.ID)
			waitForRelease(t, c, second.ID)
			close(stop)
			sampler.Wait()

			breach.Lock()
			err := breach.err
			breach.Unlock()
			if err != nil {
				t.Fatalf("capacity accounting broke under immediate redispatch: %v", err)
			}
			if timer.count() < agent.episodes {
				t.Fatalf("armed %d timers, want at least one redispatch per non-terminal episode", timer.count())
			}
			if err := c.Shutdown(context.Background()); err != nil {
				t.Fatal(err)
			}
			if snapshot := c.Snapshot(); snapshot.Claimed != 0 || len(snapshot.Running) != 0 {
				t.Fatalf("snapshot after shutdown=%+v, want nothing held", snapshot)
			}
		})
	}
}

// TestConfiguredDelaysKeepSlotReleaseAheadOfRedispatch pins the production
// timing PMR-121 relies on being unchanged: a worker's slot is freed as it
// exits, before the retry it armed can fire, so a healthy redispatch never has
// to wait out its predecessor's teardown for capacity.
func TestConfiguredDelaysKeepSlotReleaseAheadOfRedispatch(t *testing.T) {
	w := testSettings(t)
	w.Config.GitHub.PollInterval = 30 * time.Second
	issue := testIssue()
	tracker := &fakeTracker{issue: issue}
	agent := &fakeAgent{events: landingWaitingEvents, started: make(chan struct{}, 1)}
	ws := &fakeWorkspace{after: make(chan struct{}, 1)}
	c := testCoordinator(w.Config, tracker, agent, ws)
	defer assertInvariants(t, c)
	timer := &fakeTimer{signal: make(chan struct{}, 2)}
	c.timer = timer

	c.Tick(context.Background())
	<-agent.started
	<-ws.after
	<-timer.signal

	// The landing wait holds only the duplicate-prevention claim: the slot is
	// already free while the timer waits out the GitHub poll interval.
	if admitted := c.admittedTotal(); admitted != 0 {
		t.Fatalf("admitted=%d, want the slot released before the redispatch is armed", admitted)
	}
	if !c.claimHeld(issue.ID) {
		t.Fatal("landing wait dropped the duplicate-prevention claim")
	}
	if len(timer.delays) != 1 || timer.delays[0] != 30*time.Second {
		t.Fatalf("delays=%v, want the configured GitHub poll interval", timer.delays)
	}
	if _, ok := w.Config.Agent.ByState["todo"]; ok {
		t.Fatal("this test assumes no per-state limit on todo")
	}
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}
