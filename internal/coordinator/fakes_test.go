package coordinator

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/pmrrasmussen/symphony/internal/domain"
)

type fakeTracker struct {
	mu            sync.Mutex
	issue         domain.Issue
	fresh         domain.Issue
	hasFresh      bool
	gets          int
	getErr        error
	transitions   []trackerTransition
	transitionErr error
	// liveState, when set, is the state a concurrent writer (a human) left the
	// issue in. Like the real adapter, the fake then refuses the write and
	// reports that freshly read state back instead.
	liveState    string
	getIssuesErr error
}

// trackerTransition records one host-side dispatch transition request so tests
// can assert the coordinator moved an issue into its started state (or did not).
// from is the source state the coordinator asserted, not its snapshot's.
type trackerTransition struct{ id, from, to string }

func (f *fakeTracker) ListCandidates(context.Context, []string) ([]domain.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return []domain.Issue{f.issue}, nil
}
func (f *fakeTracker) GetIssues(context.Context, []string) ([]domain.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets++
	if f.getIssuesErr != nil {
		return nil, f.getIssuesErr
	}
	if f.getErr != nil {
		return nil, f.getErr
	}
	if f.hasFresh {
		return []domain.Issue{f.fresh}, nil
	}
	return []domain.Issue{f.issue}, nil
}

func (f *fakeTracker) getCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gets
}
func (f *fakeTracker) ListTerminal(context.Context, []string) ([]domain.Issue, error) {
	return nil, nil
}
func (f *fakeTracker) Transition(_ context.Context, issue domain.Issue, fromState, toState string) (domain.TransitionResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.transitions = append(f.transitions, trackerTransition{id: issue.ID, from: fromState, to: toState})
	if f.transitionErr != nil {
		return domain.TransitionResult{}, f.transitionErr
	}
	if f.liveState != "" && !strings.EqualFold(f.liveState, fromState) {
		return domain.TransitionResult{FromState: f.liveState}, nil
	}
	return domain.TransitionResult{FromState: fromState, Applied: true}, nil
}
func (f *fakeTracker) transitionCalls() []trackerTransition {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]trackerTransition(nil), f.transitions...)
}
func (f *fakeTracker) setIssue(issue domain.Issue) {
	f.mu.Lock()
	f.issue = issue
	f.mu.Unlock()
}
func (f *fakeTracker) setFresh(issue domain.Issue) {
	f.mu.Lock()
	f.fresh = issue
	f.hasFresh = true
	f.mu.Unlock()
}

// issueMapTracker makes capacity and reconciliation tests deterministic when
// more than one issue is eligible at the same time.
type issueMapTracker struct {
	mu         sync.Mutex
	candidates []domain.Issue
	issues     map[string]domain.Issue
	getErr     error
}

func (t *issueMapTracker) ListCandidates(context.Context, []string) ([]domain.Issue, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]domain.Issue(nil), t.candidates...), nil
}

func (t *issueMapTracker) GetIssues(_ context.Context, ids []string) ([]domain.Issue, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.getErr != nil {
		return nil, t.getErr
	}
	issues := make([]domain.Issue, 0, len(ids))
	for _, id := range ids {
		if issue, ok := t.issues[id]; ok {
			issues = append(issues, issue)
		}
	}
	return issues, nil
}

func (*issueMapTracker) ListTerminal(context.Context, []string) ([]domain.Issue, error) {
	return nil, nil
}

func (*issueMapTracker) Transition(_ context.Context, _ domain.Issue, fromState, _ string) (domain.TransitionResult, error) {
	return domain.TransitionResult{FromState: fromState, Applied: true}, nil
}

func (t *issueMapTracker) setIssue(issue domain.Issue) {
	t.mu.Lock()
	t.issues[issue.ID] = issue
	for index, candidate := range t.candidates {
		if candidate.ID == issue.ID {
			t.candidates[index] = issue
		}
	}
	t.mu.Unlock()
}

type fakeAgent struct {
	mu                 sync.Mutex
	starts             int
	continues          int
	cancels            int
	started            chan struct{}
	events             func() <-chan domain.Event
	continuationEvents []func() <-chan domain.Event
	// startErr models a boundary that fails identically on every dispatch --
	// an unreachable agent binary is the canonical one -- so a test can drive
	// the retry ladder to its ceiling without any per-attempt bookkeeping.
	startErr         error
	continueErr      error
	continueSessions []domain.AgentSession
	continuePrompts  []string
	onContinue       func(int)
	// startRequests is every request the coordinator dispatched. The prompt and
	// the backend on it are one decision made at the call site, and this is the
	// only place a test can see the pair the router was actually handed.
	startRequests []domain.AgentRequest
}

func (f *fakeAgent) Start(_ context.Context, r domain.AgentRequest) (domain.AgentSession, <-chan domain.Event, error) {
	f.mu.Lock()
	f.starts++
	f.startRequests = append(f.startRequests, r)
	started := f.started
	err := f.startErr
	f.mu.Unlock()
	if started != nil {
		started <- struct{}{}
	}
	if err != nil {
		return domain.AgentSession{}, nil, err
	}
	return domain.AgentSession{ID: "t-u", ThreadID: "t", TurnID: "u"}, f.events(), nil
}
func (f *fakeAgent) Continue(_ context.Context, session domain.AgentSession, prompt string) (<-chan domain.Event, error) {
	f.mu.Lock()
	index := f.continues
	f.continues++
	f.continueSessions = append(f.continueSessions, session)
	f.continuePrompts = append(f.continuePrompts, prompt)
	events := f.continuationEvents
	err := f.continueErr
	onContinue := f.onContinue
	f.mu.Unlock()
	if onContinue != nil {
		onContinue(index)
	}
	if err != nil {
		return nil, err
	}
	if index >= len(events) {
		return closedEvents(), nil
	}
	return events[index](), nil
}
func (f *fakeAgent) Cancel(context.Context, domain.AgentSession) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancels++
	return nil
}
func (f *fakeAgent) counts() (starts, continues, cancels int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.starts, f.continues, f.cancels
}

func (f *fakeAgent) requests() []domain.AgentRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]domain.AgentRequest(nil), f.startRequests...)
}

func (f *fakeAgent) continuations() ([]domain.AgentSession, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]domain.AgentSession(nil), f.continueSessions...), append([]string(nil), f.continuePrompts...)
}

type fakeWorkspace struct {
	mu             sync.Mutex
	shouldRun      bool
	shouldRunCalls int
	prepares       int
	marks          int
	cleanups       int
	cleaned        chan struct{}
	after          chan struct{}
	prepareStarted chan struct{}
	prepareGate    <-chan struct{}
	// afterErr is the source-integrity verdict AfterRun reports, the one error
	// that boundary returns (domain.WorkspaceExecutor).
	afterErr error
	// cleanupStarted and cleanupGate let a test pause the first Cleanup call
	// until it has arranged a race against it (a concurrent stopRun, or a
	// second Cleanup call), and cleanupErr makes that first call fail so the
	// test can observe how the failure is reported. Every later call proceeds
	// immediately and succeeds, standing in for reconciliation's own
	// authoritative retry.
	cleanupStarted chan struct{}
	cleanupGate    <-chan struct{}
	cleanupErr     error
	// cleanupsInFlight and cleanupOverlap record whether two Cleanup calls were
	// ever in flight at once, which is the corruption PMR-160 fixed: two
	// attempts removing one worktree, the loser reporting the winner's completed
	// removal as a git failure. No test needs to arrange the overlap to assert
	// it never happens, so every test that pauses a cleanup checks this.
	cleanupsInFlight int
	cleanupOverlap   bool
}

func (f *fakeWorkspace) Prepare(ctx context.Context, _ domain.Issue) (domain.Workspace, error) {
	f.mu.Lock()
	f.prepares++
	started := f.prepareStarted
	gate := f.prepareGate
	f.mu.Unlock()
	if started != nil {
		started <- struct{}{}
	}
	if gate != nil {
		select {
		case <-ctx.Done():
			return domain.Workspace{}, ctx.Err()
		case <-gate:
		}
	}
	return domain.Workspace{Path: "/tmp/work"}, nil
}
func (f *fakeWorkspace) BeforeRun(context.Context, domain.Workspace, domain.Issue) error { return nil }
func (f *fakeWorkspace) AfterRun(context.Context, domain.Workspace, domain.Issue) error {
	if f.after != nil {
		f.after <- struct{}{}
	}
	return f.afterErr
}
func (f *fakeWorkspace) Cleanup(ctx context.Context, _ domain.Issue) (domain.CleanupOutcome, error) {
	f.mu.Lock()
	f.cleanups++
	f.cleanupsInFlight++
	if f.cleanupsInFlight > 1 {
		f.cleanupOverlap = true
	}
	first := f.cleanups == 1
	cleaned := f.cleaned
	started := f.cleanupStarted
	gate := f.cleanupGate
	err := f.cleanupErr
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.cleanupsInFlight--
		f.mu.Unlock()
	}()
	if first {
		if started != nil {
			started <- struct{}{}
		}
		if gate != nil {
			<-gate
		}
		if err != nil {
			return domain.CleanupClean, err
		}
		if ctx.Err() != nil {
			return domain.CleanupClean, ctx.Err()
		}
	}
	if cleaned != nil {
		cleaned <- struct{}{}
	}
	return domain.CleanupClean, nil
}
func (f *fakeWorkspace) Execute(context.Context, domain.Workspace, string, []string) ([]byte, error) {
	return nil, nil
}
func (f *fakeWorkspace) counts() (prepares, marks, cleanups, checks int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.prepares, f.marks, f.cleanups, f.shouldRunCalls
}

// overlappedCleanups reports whether two Cleanup calls were ever in flight at
// the same time.
func (f *fakeWorkspace) overlappedCleanups() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cleanupOverlap
}

type fakeTimer struct {
	mu      sync.Mutex
	delays  []time.Duration
	entries []fakeTimerEntry
	signal  chan struct{}
}
type fakeTimerEntry struct {
	callback func()
	stopped  bool
}
type fakeTimerHandle struct {
	timer *fakeTimer
	index int
}

func (f *fakeTimer) AfterFunc(d time.Duration, callback func()) TimerHandle {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.delays = append(f.delays, d)
	f.entries = append(f.entries, fakeTimerEntry{callback: callback})
	if f.signal != nil {
		f.signal <- struct{}{}
	}
	return fakeTimerHandle{timer: f, index: len(f.entries) - 1}
}
func (h fakeTimerHandle) Stop() bool {
	h.timer.mu.Lock()
	defer h.timer.mu.Unlock()
	if h.index >= len(h.timer.entries) || h.timer.entries[h.index].stopped {
		return false
	}
	h.timer.entries[h.index].stopped = true
	return true
}
func (f *fakeTimer) fire(index int) {
	f.mu.Lock()
	entry := f.entries[index]
	f.mu.Unlock()
	if !entry.stopped {
		entry.callback()
	}
}

// fireStale simulates a callback that was already queued when Stop raced with
// it. The coordinator must still reject it by generation.
func (f *fakeTimer) fireStale(index int) {
	f.mu.Lock()
	callback := f.entries[index].callback
	f.mu.Unlock()
	callback()
}
func (f *fakeTimer) scheduled() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.entries)
}

type fakeClock struct{ now time.Time }

func (f fakeClock) Now() time.Time { return f.now }

type mutableClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *mutableClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *mutableClock) set(now time.Time) {
	c.mu.Lock()
	c.now = now
	c.mu.Unlock()
}

type retryPollError struct{ delay time.Duration }

func (e retryPollError) Error() string             { return "rate limited" }
func (e retryPollError) RetryDelay() time.Duration { return e.delay }

type rateLimitPollTracker struct {
	mu    sync.Mutex
	calls int
}

func (t *rateLimitPollTracker) ListCandidates(context.Context, []string) ([]domain.Issue, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.calls++
	if t.calls == 1 {
		return nil, retryPollError{delay: 2 * time.Minute}
	}
	return nil, nil
}
func (*rateLimitPollTracker) GetIssues(context.Context, []string) ([]domain.Issue, error) {
	return nil, nil
}
func (*rateLimitPollTracker) ListTerminal(context.Context, []string) ([]domain.Issue, error) {
	return nil, nil
}
func (*rateLimitPollTracker) Transition(_ context.Context, _ domain.Issue, fromState, _ string) (domain.TransitionResult, error) {
	return domain.TransitionResult{FromState: fromState, Applied: true}, nil
}

// stubForgetter records the issues the coordinator reported as finished. It
// stands in for the host GitHub manager's linked-pull-request table, which is
// the thing that would otherwise poll a Done issue for the life of the process
// (PMR-112).
type stubForgetter struct {
	mu        sync.Mutex
	forgotten []string
}

func (s *stubForgetter) Forget(issueID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.forgotten = append(s.forgotten, issueID)
}

func (s *stubForgetter) issues() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.forgotten...)
}
