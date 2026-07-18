// Package coordinator owns all mutable scheduling state.
package coordinator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/domain"
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

type retryKind string

const (
	retryAgent      retryKind = "agent"
	retryCompletion retryKind = "completion_marker"
)

// retryState is the coordinator's complete durable-in-process intent for a
// claimed issue. Durable completion itself remains in the workspace; this
// state only governs a live process and is discarded safely on restart.
type retryState struct {
	ctx        context.Context
	issue      domain.Issue
	workspace  domain.Workspace
	attempt    int
	kind       retryKind
	reason     string
	due        time.Time
	generation uint64
	timer      TimerHandle
}

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

	mu         sync.Mutex
	running    map[string]*running
	claimed    map[string]bool
	claimState map[string]string
	retries    map[string]retryState
	nextRetry  uint64
	stopping   bool
	ctx        context.Context
	cancel     context.CancelFunc
	wg         sync.WaitGroup
}

type running struct {
	issue     domain.Issue
	session   domain.AgentSession
	started   time.Time
	last      time.Time
	cancel    context.CancelFunc
	attempt   int
	workspace domain.Workspace
	stopped   stopReason
}

func New(t domain.Tracker, a domain.AgentBackend, w domain.WorkspaceExecutor, settings func() config.Settings, logger *slog.Logger) *Coordinator {
	if logger == nil {
		logger = slog.Default()
	}
	return &Coordinator{
		tracker: t, agent: a, workspaces: w, settings: settings,
		timer: realTimer{}, clock: realClock{}, log: logger,
		running: map[string]*running{}, claimed: map[string]bool{},
		claimState: map[string]string{}, retries: map[string]retryState{},
	}
}

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
			c.Tick(ctx)
			d := c.settings().Polling.Interval
			timer := time.NewTimer(d)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
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
	if ctx.Err() != nil || c.isStopping() {
		return
	}
	c.reconcile(ctx)
	if ctx.Err() != nil {
		return
	}
	s := c.settings()
	issues, err := c.tracker.ListCandidates(ctx, s.Tracker.ActiveStates)
	if err != nil {
		c.log.Error("candidate poll failed", "error", err)
		return
	}
	sortIssues(issues)
	for _, i := range issues {
		if ctx.Err() != nil || c.isStopping() {
			return
		}
		if !eligible(i, s) || !c.shouldRun(ctx, i) || !c.claim(i, s) {
			continue
		}
		c.launch(ctx, i, 0)
	}
}

func (c *Coordinator) reconcile(ctx context.Context) {
	c.mu.Lock()
	runs := make([]*running, 0, len(c.running))
	for _, r := range c.running {
		runs = append(runs, r)
	}
	c.mu.Unlock()
	if len(runs) == 0 {
		return
	}
	ids := make([]string, len(runs))
	for i, r := range runs {
		ids[i] = r.issue.ID
	}
	issues, err := c.tracker.GetIssues(ctx, ids)
	if err != nil {
		c.log.Warn("running issue refresh failed", "error", err)
		return
	}
	byID := map[string]domain.Issue{}
	for _, i := range issues {
		byID[i.ID] = i
	}
	s := c.settings()
	now := c.clock.Now()
	for _, r := range runs {
		fresh, found := byID[r.issue.ID]
		reason := stopReason("")
		if !found || !eligible(fresh, s) {
			reason = stopIneligible
			if found && terminal(fresh, s) {
				reason = stopTerminal
			}
		}
		c.mu.Lock()
		last := r.last
		c.mu.Unlock()
		if reason == "" && s.Codex.StallTimeout > 0 && now.Sub(last) > s.Codex.StallTimeout {
			reason = stopStalled
		}
		if reason == "" || !c.stopRun(r.issue.ID, reason) {
			continue
		}
		if reason == stopTerminal {
			if err := c.workspaces.Cleanup(ctx, fresh); err != nil {
				c.log.Warn("terminal workspace cleanup failed", "issue_id", fresh.ID, "issue_identifier", fresh.Identifier, "error", err)
			}
		}
		c.log.Info("agent reconciled", "issue_id", r.issue.ID, "issue_identifier", r.issue.Identifier, "session_id", r.session.ID, "reason", reason)
	}
}

func (c *Coordinator) claim(i domain.Issue, s config.Settings) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopping || c.claimed[i.ID] || len(c.claimed) >= s.Agent.MaxConcurrent {
		return false
	}
	limit, ok := s.Agent.ByState[norm(i.State)]
	if ok {
		n := 0
		for _, state := range c.claimState {
			if state == norm(i.State) {
				n++
			}
		}
		if n >= limit {
			return false
		}
	}
	c.claimed[i.ID] = true
	c.claimState[i.ID] = norm(i.State)
	return true
}

func (c *Coordinator) launch(parent context.Context, i domain.Issue, attempt int) {
	c.mu.Lock()
	if c.stopping {
		c.mu.Unlock()
		c.release(i.ID)
		return
	}
	c.wg.Add(1)
	c.mu.Unlock()
	go func() {
		defer c.wg.Done()
		ctx, cancel := context.WithCancel(parent)
		defer cancel()
		if !c.shouldRun(ctx, i) {
			c.release(i.ID)
			return
		}
		ws, err := c.workspaces.Prepare(ctx, i)
		if err != nil {
			c.finishFailure(parent, i, attempt, "workspace_prepare", err)
			return
		}
		if err = c.workspaces.BeforeRun(ctx, ws, i); err != nil {
			c.workspaces.AfterRun(context.Background(), ws, i)
			c.finishFailure(parent, i, attempt, "before_run", err)
			return
		}
		s := c.settings()
		prompt, err := render(s, i, attempt)
		if err != nil {
			c.workspaces.AfterRun(context.Background(), ws, i)
			c.finishFailure(parent, i, attempt, "prompt_render", err)
			return
		}
		session, events, err := c.agent.Start(ctx, domain.AgentRequest{Issue: i, Workspace: ws.Path, Prompt: prompt, Command: s.Codex.Command, ApprovalPolicy: s.Codex.ApprovalPolicy, ThreadSandbox: s.Codex.ThreadSandbox, TurnSandboxPolicy: s.Codex.TurnSandboxPolicy, TurnTimeout: s.Codex.TurnTimeout, ReadTimeout: s.Codex.ReadTimeout})
		if err != nil {
			c.workspaces.AfterRun(context.Background(), ws, i)
			c.finishFailure(parent, i, attempt, "session_start", err)
			return
		}
		if ctx.Err() != nil {
			c.cancelSession(context.Background(), session)
			c.workspaces.AfterRun(context.Background(), ws, i)
			c.release(i.ID)
			return
		}
		now := c.clock.Now()
		r := &running{issue: i, session: session, started: now, last: now, cancel: cancel, attempt: attempt, workspace: ws}
		c.mu.Lock()
		c.running[i.ID] = r
		c.mu.Unlock()
		completed, consumeErr := c.consume(ctx, r, events)
		c.mu.Lock()
		delete(c.running, i.ID)
		stopped := r.stopped
		completionAllowed := completed && !c.stopping && stopped == "" && parent.Err() == nil
		c.mu.Unlock()
		if completed && !completionAllowed {
			completed = false
			if consumeErr == nil {
				consumeErr = context.Canceled
			}
		}
		if completed {
			c.cancelSession(context.Background(), r.session)
		}
		c.workspaces.AfterRun(context.Background(), ws, i)
		if completed {
			if err := c.workspaces.MarkCompleted(parent, ws, i); err != nil {
				c.log.Error("record completed work failed", "issue_id", i.ID, "issue_identifier", i.Identifier, "session_id", session.ID, "error", err)
				c.scheduleRetry(parent, i, ws, attempt+1, retryCompletion, "completion_marker", backoff(attempt+1, s.Agent.MaxRetryBackoff))
				return
			}
			c.release(i.ID)
			return
		}
		if stopped != "" || ctx.Err() != nil {
			c.log.Info("agent run cancelled", "issue_id", i.ID, "issue_identifier", i.Identifier, "session_id", session.ID, "reason", cancellationReason(stopped, ctx))
			c.release(i.ID)
			return
		}
		c.finishFailure(parent, i, attempt, "agent_event", consumeErr)
	}()
}

func (c *Coordinator) consume(ctx context.Context, r *running, events <-chan domain.Event) (bool, error) {
	for {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case e, ok := <-events:
			if !ok {
				return false, errors.New("agent event stream closed before completion")
			}
			at := e.At
			if at.IsZero() {
				at = c.clock.Now()
			}
			c.mu.Lock()
			r.last = at
			c.mu.Unlock()
			c.log.Info("agent event", "issue_id", r.issue.ID, "issue_identifier", r.issue.Identifier, "session_id", e.SessionID, "event", e.Kind)
			switch e.Kind {
			case domain.EventBlocked:
				return false, fmt.Errorf("agent blocked: %s", e.Message)
			case domain.EventFailed:
				return false, fmt.Errorf("agent failed: %s", e.Message)
			case domain.EventCompleted:
				// Reconciliation and event delivery can race. An event that
				// arrives after reconciliation has canceled this run must never
				// turn into a durable completion marker.
				c.mu.Lock()
				stopped := r.stopped
				stopping := c.stopping
				c.mu.Unlock()
				if stopped != "" || stopping || ctx.Err() != nil {
					if err := ctx.Err(); err != nil {
						return false, err
					}
					return false, context.Canceled
				}
				return true, nil
			}
		}
	}
}

func (c *Coordinator) finishFailure(ctx context.Context, i domain.Issue, attempt int, reason string, err error) {
	if ctx.Err() != nil {
		c.log.Info("agent run cancelled", "issue_id", i.ID, "issue_identifier", i.Identifier, "reason", cancellationReason("", ctx))
		c.release(i.ID)
		return
	}
	c.scheduleRetry(ctx, i, domain.Workspace{}, attempt+1, retryAgent, reason, backoff(attempt+1, c.settings().Agent.MaxRetryBackoff))
	if err != nil {
		c.log.Warn("agent run retry scheduled", "issue_id", i.ID, "issue_identifier", i.Identifier, "reason", reason, "error", err)
	}
}

func (c *Coordinator) scheduleRetry(ctx context.Context, i domain.Issue, ws domain.Workspace, attempt int, kind retryKind, reason string, delay time.Duration) {
	c.mu.Lock()
	if c.stopping || !c.claimed[i.ID] {
		c.mu.Unlock()
		return
	}
	if previous, ok := c.retries[i.ID]; ok && previous.timer != nil {
		previous.timer.Stop()
	}
	c.nextRetry++
	generation := c.nextRetry
	state := retryState{ctx: ctx, issue: i, workspace: ws, attempt: attempt, kind: kind, reason: reason, due: c.clock.Now().Add(delay), generation: generation}
	c.retries[i.ID] = state
	c.mu.Unlock()
	handle := c.timer.AfterFunc(delay, func() { c.runRetry(i.ID, generation) })
	c.mu.Lock()
	current, ok := c.retries[i.ID]
	if ok && current.generation == generation {
		current.timer = handle
		c.retries[i.ID] = current
	} else if handle != nil {
		handle.Stop()
	}
	c.mu.Unlock()
	c.log.Info("agent retry scheduled", "issue_id", i.ID, "issue_identifier", i.Identifier, "retry_kind", kind, "reason", reason, "attempt", attempt, "due", state.due)
}

func (c *Coordinator) runRetry(id string, generation uint64) {
	c.mu.Lock()
	retry, ok := c.retries[id]
	stopping := c.stopping
	if !ok || retry.generation != generation || stopping {
		c.mu.Unlock()
		if ok && stopping {
			c.release(id)
		}
		return // a stopped or superseded timer is stale
	}
	delete(c.retries, id)
	c.mu.Unlock()
	ctx := retry.ctx
	if managed := c.context(); managed != nil {
		ctx = managed
	}
	if ctx == nil || ctx.Err() != nil {
		c.release(id)
		return
	}
	fresh, err := c.tracker.GetIssues(ctx, []string{id})
	if err != nil || len(fresh) != 1 || fresh[0].ID != id {
		if err != nil {
			c.log.Warn("retry issue refresh failed", "issue_id", id, "reason", retry.reason, "error", err)
		}
		c.release(id)
		return
	}
	s := c.settings()
	issue := fresh[0]
	if !eligible(issue, s) {
		if terminal(issue, s) {
			if err := c.workspaces.Cleanup(ctx, issue); err != nil {
				c.log.Warn("terminal workspace cleanup failed", "issue_id", issue.ID, "issue_identifier", issue.Identifier, "error", err)
			}
		}
		c.release(id)
		return
	}
	if !c.shouldRun(ctx, issue) {
		c.release(id)
		return
	}
	if retry.kind == retryCompletion {
		// A completion marker may only be written for the exact issue version
		// that Codex completed. A later update is a new run, not a marker retry.
		if !sameIssueVersion(retry.issue, issue) {
			c.release(id)
			return
		}
		if err := c.workspaces.MarkCompleted(ctx, retry.workspace, issue); err != nil {
			c.log.Error("record completed work retry failed", "issue_id", issue.ID, "issue_identifier", issue.Identifier, "error", err)
			c.scheduleRetry(ctx, issue, retry.workspace, retry.attempt+1, retryCompletion, "completion_marker", backoff(retry.attempt+1, s.Agent.MaxRetryBackoff))
			return
		}
		c.release(id)
		return
	}
	c.launch(ctx, issue, retry.attempt)
}

func sameIssueVersion(a, b domain.Issue) bool {
	if a.ID != b.ID {
		return false
	}
	if a.UpdatedAt == nil || b.UpdatedAt == nil {
		return a.UpdatedAt == nil && b.UpdatedAt == nil
	}
	return a.UpdatedAt.Equal(*b.UpdatedAt)
}

func (c *Coordinator) stopRun(id string, reason stopReason) bool {
	c.mu.Lock()
	r, ok := c.running[id]
	if !ok || r.stopped != "" {
		c.mu.Unlock()
		return false
	}
	r.stopped = reason
	r.cancel()
	c.mu.Unlock()
	c.cancelSession(context.Background(), r.session)
	return true
}

func (c *Coordinator) cancelAll(ctx context.Context, runs []*running) {
	var wg sync.WaitGroup
	for _, r := range runs {
		wg.Add(1)
		go func(session domain.AgentSession) {
			defer wg.Done()
			c.cancelSession(ctx, session)
		}(r.session)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

func (c *Coordinator) cancelSession(parent context.Context, session domain.AgentSession) {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.agent.Cancel(ctx, session) }()
	select {
	case err := <-done:
		if err != nil {
			c.log.Warn("agent cancellation failed", "session_id", session.ID, "error", err)
		}
	case <-ctx.Done():
		c.log.Warn("agent cancellation timed out", "session_id", session.ID)
	}
}

func (c *Coordinator) shouldRun(ctx context.Context, i domain.Issue) bool {
	ok, err := c.workspaces.ShouldRun(ctx, i)
	if err != nil {
		c.log.Warn("workspace run eligibility check failed", "issue_id", i.ID, "issue_identifier", i.Identifier, "error", err)
		return false
	}
	return ok
}

func (c *Coordinator) release(id string) {
	c.mu.Lock()
	if retry, ok := c.retries[id]; ok && retry.timer != nil {
		retry.timer.Stop()
	}
	delete(c.claimed, id)
	delete(c.claimState, id)
	delete(c.retries, id)
	c.mu.Unlock()
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

func active(i domain.Issue, s config.Settings) bool {
	for _, x := range s.Tracker.ActiveStates {
		if norm(i.State) == x {
			return true
		}
	}
	return false
}
func terminal(i domain.Issue, s config.Settings) bool {
	for _, x := range s.Tracker.TerminalStates {
		if norm(i.State) == x {
			return true
		}
	}
	return false
}
func routable(i domain.Issue, s config.Settings) bool {
	if !i.Dispatchable {
		return false
	}
	have := map[string]bool{}
	for _, x := range i.Labels {
		have[norm(x)] = true
	}
	for _, x := range s.Tracker.RequiredLabels {
		if x == "" || !have[x] {
			return false
		}
	}
	return true
}
func eligible(i domain.Issue, s config.Settings) bool {
	return i.ID != "" && i.Identifier != "" && i.Title != "" && active(i, s) && !terminal(i, s) && routable(i, s)
}
func norm(v string) string { return strings.ToLower(strings.TrimSpace(v)) }
func sortIssues(v []domain.Issue) {
	sort.SliceStable(v, func(i, j int) bool {
		a, b := v[i], v[j]
		ap, bp := priority(a), priority(b)
		if ap != bp {
			return ap < bp
		}
		if a.CreatedAt == nil {
			return false
		}
		if b.CreatedAt == nil {
			return true
		}
		if !a.CreatedAt.Equal(*b.CreatedAt) {
			return a.CreatedAt.Before(*b.CreatedAt)
		}
		return a.Identifier < b.Identifier
	})
}
func priority(i domain.Issue) int {
	if i.Priority != nil && *i.Priority >= 1 && *i.Priority <= 4 {
		return *i.Priority
	}
	return 5
}
func backoff(a int, max time.Duration) time.Duration {
	d := 10 * time.Second
	for n := 1; n < a && d < max; n++ {
		d *= 2
	}
	if d > max {
		return max
	}
	return d
}
func render(s config.Settings, i domain.Issue, attempt int) (string, error) {
	return s.Render(i, attempt)
}
