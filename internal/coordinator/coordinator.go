// Package coordinator owns all mutable scheduling state.
package coordinator

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/domain"
)

type Timer interface{ AfterFunc(time.Duration, func()) }
type realTimer struct{}

func (realTimer) AfterFunc(d time.Duration, f func()) { time.AfterFunc(d, f) }

type Coordinator struct {
	tracker    domain.Tracker
	agent      domain.AgentBackend
	workspaces domain.WorkspaceExecutor
	settings   func() config.Settings
	timer      Timer
	log        *slog.Logger
	mu         sync.Mutex
	running    map[string]*running
	claimed    map[string]bool
	claimState map[string]string
	retries    map[string]int
	ctx        context.Context
	cancel     context.CancelFunc
	wg         sync.WaitGroup
}
type running struct {
	issue         domain.Issue
	session       domain.AgentSession
	started, last time.Time
	cancel        context.CancelFunc
	attempt       int
	workspace     domain.Workspace
}

func New(t domain.Tracker, a domain.AgentBackend, w domain.WorkspaceExecutor, settings func() config.Settings, logger *slog.Logger) *Coordinator {
	if logger == nil {
		logger = slog.Default()
	}
	return &Coordinator{tracker: t, agent: a, workspaces: w, settings: settings, timer: realTimer{}, log: logger, running: map[string]*running{}, claimed: map[string]bool{}, claimState: map[string]string{}, retries: map[string]int{}}
}
func (c *Coordinator) Start(parent context.Context) {
	c.mu.Lock()
	if c.ctx != nil {
		c.mu.Unlock()
		return
	}
	c.ctx, c.cancel = context.WithCancel(parent)
	c.mu.Unlock()
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		for {
			c.Tick(c.ctx)
			d := c.settings().Polling.Interval
			timer := time.NewTimer(d)
			select {
			case <-c.ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
}
func (c *Coordinator) Shutdown(ctx context.Context) error {
	c.mu.Lock()
	if c.cancel != nil {
		c.cancel()
	}
	runs := make([]*running, 0, len(c.running))
	for _, r := range c.running {
		runs = append(runs, r)
		r.cancel()
	}
	c.mu.Unlock()
	for _, r := range runs {
		_ = c.agent.Cancel(context.Background(), r.session)
	}
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
	c.reconcile(ctx)
	s := c.settings()
	issues, err := c.tracker.ListCandidates(ctx, s.Tracker.ActiveStates)
	if err != nil {
		c.log.Error("candidate poll failed", "error", err)
		return
	}
	sortIssues(issues)
	for _, i := range issues {
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
	now := time.Now()
	for _, r := range runs {
		fresh, found := byID[r.issue.ID]
		stop := !found || terminal(fresh, s) || !active(fresh, s) || !routable(fresh, s)
		stall := s.Codex.StallTimeout > 0 && now.Sub(r.last) > s.Codex.StallTimeout
		if stop || stall {
			r.cancel()
			_ = c.agent.Cancel(context.Background(), r.session)
			if found && terminal(fresh, s) {
				_ = c.workspaces.Cleanup(ctx, fresh)
			}
			c.log.Info("agent reconciled", "issue", r.issue.Identifier, "reason", map[bool]string{true: "ineligible", false: "stalled"}[stop])
		}
	}
}
func (c *Coordinator) claim(i domain.Issue, s config.Settings) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.claimed[i.ID] || len(c.claimed) >= s.Agent.MaxConcurrent {
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
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		ctx, cancel := context.WithCancel(parent)
		if !c.shouldRun(ctx, i) {
			cancel()
			c.release(i.ID)
			return
		}
		ws, err := c.workspaces.Prepare(ctx, i)
		if err != nil {
			cancel()
			c.done(i, attempt, err)
			return
		}
		if err = c.workspaces.BeforeRun(ctx, ws, i); err != nil {
			c.workspaces.AfterRun(context.Background(), ws, i)
			cancel()
			c.done(i, attempt, err)
			return
		}
		s := c.settings()
		prompt, err := render(s, i, attempt)
		completed := false
		var completionErr error
		if err == nil {
			session, events, startErr := c.agent.Start(ctx, domain.AgentRequest{Issue: i, Workspace: ws.Path, Prompt: prompt, Command: s.Codex.Command, ApprovalPolicy: s.Codex.ApprovalPolicy, ThreadSandbox: s.Codex.ThreadSandbox, TurnSandboxPolicy: s.Codex.TurnSandboxPolicy, TurnTimeout: s.Codex.TurnTimeout, ReadTimeout: s.Codex.ReadTimeout})
			if startErr != nil {
				err = startErr
			} else {
				r := &running{issue: i, session: session, started: time.Now(), last: time.Now(), cancel: cancel, attempt: attempt, workspace: ws}
				c.mu.Lock()
				c.running[i.ID] = r
				c.mu.Unlock()
				completed, err = c.consume(ctx, r, events)
				c.mu.Lock()
				delete(c.running, i.ID)
				c.mu.Unlock()
				if completed {
					r.cancel()
					_ = c.agent.Cancel(context.Background(), r.session)
					completionErr = c.workspaces.MarkCompleted(parent, ws, i)
				}
			}
		}
		c.workspaces.AfterRun(context.Background(), ws, i)
		cancel()
		if completed {
			if completionErr != nil {
				c.log.Error("record completed work failed", "issue", i.Identifier, "error", completionErr)
				c.scheduleCompletionRecord(i, ws, attempt+1)
				return
			}
			c.release(i.ID)
			return
		}
		c.done(i, attempt, err)
	}()
}
func (c *Coordinator) consume(ctx context.Context, r *running, events <-chan domain.Event) (bool, error) {
	for {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case e, ok := <-events:
			if !ok {
				return false, fmt.Errorf("agent event stream closed before completion")
			}
			c.mu.Lock()
			r.last = e.At
			c.mu.Unlock()
			c.log.Info("agent event", "issue", r.issue.Identifier, "session", e.SessionID, "event", e.Kind)
			switch e.Kind {
			case domain.EventBlocked:
				return false, fmt.Errorf("agent blocked: %s", e.Message)
			case domain.EventFailed:
				return false, fmt.Errorf("agent failed: %s", e.Message)
			case domain.EventCompleted:
				return true, nil
			}
		}
	}
}
func (c *Coordinator) done(i domain.Issue, attempt int, err error) {
	c.schedule(i, attempt+1, backoff(attempt+1, c.settings().Agent.MaxRetryBackoff))
}
func (c *Coordinator) schedule(i domain.Issue, attempt int, d time.Duration) {
	c.mu.Lock()
	c.retries[i.ID] = attempt
	c.mu.Unlock()
	c.timer.AfterFunc(d, func() {
		ctx := c.context()
		if ctx == nil || ctx.Err() != nil {
			return
		}
		fresh, err := c.tracker.GetIssues(ctx, []string{i.ID})
		if err != nil || len(fresh) != 1 {
			c.release(i.ID)
			return
		}
		s := c.settings()
		if terminal(fresh[0], s) {
			_ = c.workspaces.Cleanup(ctx, fresh[0])
			c.release(i.ID)
			return
		}
		if !active(fresh[0], s) || !routable(fresh[0], s) {
			c.release(i.ID)
			return
		}
		if !c.shouldRun(ctx, fresh[0]) {
			c.release(i.ID)
			return
		}
		c.mu.Lock()
		delete(c.retries, i.ID)
		c.mu.Unlock()
		c.launch(ctx, fresh[0], attempt)
	})
}

// scheduleCompletionRecord retries only durable completion recording. It keeps
// the issue claimed so a transient state-filesystem failure cannot cause Codex
// to rerun an already completed turn.
func (c *Coordinator) scheduleCompletionRecord(i domain.Issue, ws domain.Workspace, attempt int) {
	delay := backoff(attempt, c.settings().Agent.MaxRetryBackoff)
	c.mu.Lock()
	c.retries[i.ID] = attempt
	c.mu.Unlock()
	c.timer.AfterFunc(delay, func() {
		ctx := c.context()
		if ctx == nil || ctx.Err() != nil {
			return
		}
		fresh, err := c.tracker.GetIssues(ctx, []string{i.ID})
		if err != nil || len(fresh) != 1 {
			c.release(i.ID)
			return
		}
		s := c.settings()
		if terminal(fresh[0], s) {
			_ = c.workspaces.Cleanup(ctx, fresh[0])
			c.release(i.ID)
			return
		}
		if !active(fresh[0], s) || !routable(fresh[0], s) {
			c.release(i.ID)
			return
		}
		if err := c.workspaces.MarkCompleted(ctx, ws, i); err != nil {
			c.log.Error("record completed work retry failed", "issue", i.Identifier, "error", err)
			c.scheduleCompletionRecord(i, ws, attempt+1)
			return
		}
		c.release(i.ID)
	})
}
func (c *Coordinator) shouldRun(ctx context.Context, i domain.Issue) bool {
	ok, err := c.workspaces.ShouldRun(ctx, i)
	if err != nil {
		c.log.Warn("workspace run eligibility check failed", "issue", i.Identifier, "error", err)
		return false
	}
	return ok
}
func (c *Coordinator) release(id string) {
	c.mu.Lock()
	delete(c.claimed, id)
	delete(c.claimState, id)
	delete(c.retries, id)
	c.mu.Unlock()
}
func (c *Coordinator) context() context.Context { c.mu.Lock(); defer c.mu.Unlock(); return c.ctx }
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
