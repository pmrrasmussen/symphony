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

type retryKind string

const (
	retryAgent        retryKind = "agent"
	retryCompletion   retryKind = "completion_marker"
	continuationDelay           = time.Second
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
	log        *observability.Logger

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
	last      time.Time
	cancel    context.CancelFunc
	workspace domain.Workspace
	stopped   stopReason
	rateLimit map[string]int64
	run       domain.Run
}

func New(t domain.Tracker, a domain.AgentBackend, w domain.WorkspaceExecutor, settings func() config.Settings, logger *slog.Logger) *Coordinator {
	if logger == nil {
		logger = slog.Default()
	}
	return &Coordinator{
		tracker: t, agent: a, workspaces: w, settings: settings,
		timer: realTimer{}, clock: realClock{}, log: observability.FromSlog(logger),
		running: map[string]*running{}, claimed: map[string]bool{},
		claimState: map[string]string{}, retries: map[string]retryState{},
	}
}

// Snapshot is a read-only, intentionally reduced view of coordinator state.
// It excludes issue bodies, prompts, workspace paths, raw events, and tracker
// identifiers that may be provider-specific.
type Snapshot struct {
	Claimed  int
	Running  []RunningSnapshot
	Retrying []RetrySnapshot
}

type RunningSnapshot struct {
	IssueIdentifier string
	SessionID       string
	ThreadID        string
	TurnID          string
	Attempt         int
	TurnCount       int
	StartedAt       time.Time
	LastEventAt     time.Time
	Usage           domain.Usage
	RateLimit       map[string]int64
}

type RetrySnapshot struct {
	IssueIdentifier string
	Attempt         int
	Kind            string
	Reason          string
	Due             time.Time
}

// Snapshot copies the coordinator's public operational metadata while holding
// its mutex, so callers cannot observe or mutate its live scheduling maps.
func (c *Coordinator) Snapshot() Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	snapshot := Snapshot{Claimed: len(c.claimed), Running: make([]RunningSnapshot, 0, len(c.running)), Retrying: make([]RetrySnapshot, 0, len(c.retries))}
	for _, run := range c.running {
		snapshot.Running = append(snapshot.Running, RunningSnapshot{IssueIdentifier: run.issue.Identifier, SessionID: run.session.ID, ThreadID: run.session.ThreadID, TurnID: run.session.TurnID, Attempt: run.run.Attempt, TurnCount: run.run.TurnCount, StartedAt: run.run.StartedAt, LastEventAt: run.last, Usage: run.run.Usage, RateLimit: copyRateLimit(run.rateLimit)})
	}
	for _, retry := range c.retries {
		snapshot.Retrying = append(snapshot.Retrying, RetrySnapshot{IssueIdentifier: retry.issue.Identifier, Attempt: retry.attempt, Kind: string(retry.kind), Reason: retry.reason, Due: retry.due})
	}
	sort.Slice(snapshot.Running, func(i, j int) bool { return snapshot.Running[i].IssueIdentifier < snapshot.Running[j].IssueIdentifier })
	sort.Slice(snapshot.Retrying, func(i, j int) bool {
		return snapshot.Retrying[i].IssueIdentifier < snapshot.Retrying[j].IssueIdentifier
	})
	return snapshot
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

func (c *Coordinator) tick(ctx context.Context) error {
	if ctx.Err() != nil || c.isStopping() {
		return ctx.Err()
	}
	if err := c.reconcile(ctx); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	s := c.settings()
	issues, err := c.tracker.ListCandidates(ctx, s.Tracker.ActiveStates)
	if err != nil {
		c.log.Error("candidate poll failed", "error", err)
		return err
	}
	sortIssues(issues)
	for _, i := range issues {
		if ctx.Err() != nil || c.isStopping() {
			return ctx.Err()
		}
		if !eligible(i, s) || !c.shouldRun(ctx, i) || !c.claim(i, s) {
			continue
		}
		c.launch(ctx, i, 0)
	}
	return nil
}

func (c *Coordinator) reconcile(ctx context.Context) error {
	c.mu.Lock()
	runs := make([]*running, 0, len(c.running))
	for _, r := range c.running {
		runs = append(runs, r)
	}
	c.mu.Unlock()
	if len(runs) == 0 {
		return nil
	}
	ids := make([]string, len(runs))
	for i, r := range runs {
		ids[i] = r.issue.ID
	}
	issues, err := c.tracker.GetIssues(ctx, ids)
	if err != nil {
		c.log.Warn("running issue refresh failed", "error", err)
		return err
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
	return nil
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
		r := &running{
			issue: i, session: session, last: now, cancel: cancel, workspace: ws,
			run: domain.Run{IssueID: i.ID, IssueIdentifier: i.Identifier, WorkspacePath: ws.Path, SessionID: session.ID, Attempt: attempt, TurnCount: 1, StartedAt: now},
		}
		c.mu.Lock()
		if c.stopping {
			c.mu.Unlock()
			c.cancelSession(context.Background(), session)
			c.workspaces.AfterRun(context.Background(), ws, i)
			c.release(i.ID)
			return
		}
		c.running[i.ID] = r
		c.mu.Unlock()
		ended, shouldMark, current, consumeErr := c.runTurns(ctx, r, events, s)
		c.mu.Lock()
		delete(c.running, i.ID)
		stopped := r.stopped
		completionAllowed := ended && !c.stopping && stopped == "" && parent.Err() == nil
		c.mu.Unlock()
		if ended && !completionAllowed {
			ended = false
			if consumeErr == nil {
				consumeErr = context.Canceled
			}
		}
		c.finishRun(r, ended, stopped, ctx, consumeErr)
		if ended {
			c.cancelSession(context.Background(), r.session)
		} else if consumeErr != nil && stopped == "" && ctx.Err() == nil {
			c.cancelSession(context.Background(), r.session)
		}
		c.workspaces.AfterRun(context.Background(), ws, i)
		if ended {
			if shouldMark {
				c.recordCompletion(parent, r, ws, current, attempt, s)
			} else {
				c.release(i.ID)
			}
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

func (c *Coordinator) runTurns(ctx context.Context, r *running, events <-chan domain.Event, settings config.Settings) (ended, shouldMark bool, current domain.Issue, err error) {
	current = r.issue
	for {
		completed, err := c.consume(ctx, r, events)
		if !completed {
			return false, false, current, err
		}
		fresh, err := c.tracker.GetIssues(ctx, []string{current.ID})
		if err != nil {
			return false, false, current, fmt.Errorf("refresh issue after turn: %w", err)
		}
		if len(fresh) != 1 || fresh[0].ID != current.ID {
			return true, false, current, nil
		}
		current = fresh[0]
		c.mu.Lock()
		turnCount := r.run.TurnCount
		c.mu.Unlock()
		if !eligible(current, settings) {
			if terminal(current, settings) {
				if err := c.workspaces.Cleanup(ctx, current); err != nil {
					c.log.Warn("terminal workspace cleanup failed", "issue_id", current.ID, "issue_identifier", current.Identifier, "error", err)
				}
			}
			return true, false, current, nil
		}
		if turnCount >= settings.Agent.MaxTurns {
			return true, true, current, nil
		}
		if err := c.waitForContinuation(ctx); err != nil {
			return false, false, current, err
		}
		guidance := continuationGuidance(turnCount+1, settings.Agent.MaxTurns)
		events, err = c.agent.Continue(ctx, r.session, guidance)
		if err != nil {
			return false, false, current, fmt.Errorf("continue agent session: %w", err)
		}
		c.mu.Lock()
		r.run.TurnCount++
		c.mu.Unlock()
	}
}

func (c *Coordinator) waitForContinuation(ctx context.Context) error {
	fired := make(chan struct{})
	timer := c.timer.AfterFunc(continuationDelay, func() { close(fired) })
	select {
	case <-ctx.Done():
		timer.Stop()
		return ctx.Err()
	case <-fired:
		return nil
	}
}

func continuationGuidance(turn, maxTurns int) string {
	// The upstream workflow schema configures the turn bound but deliberately
	// has no continuation-prompt field. Generate its prescribed guidance here
	// so continuation turns do not resend the repository task template.
	return fmt.Sprintf(`Continuation guidance:

- The previous Codex turn completed normally, but the tracker work item is still in an active state.
- This is continuation turn #%d of %d for the current agent run.
- Resume from the current workspace and workpad state instead of restarting from scratch.
- The original task instructions and prior turn context are already present in this thread, so do not restate them before acting.
- Focus on the remaining ticket work and do not end the turn while the issue stays active unless you are truly blocked.`, turn, maxTurns)
}

func (c *Coordinator) finishRun(r *running, completed bool, stopped stopReason, ctx context.Context, err error) {
	c.mu.Lock()
	switch {
	case stopped == stopStalled:
		r.run.Status = domain.RunStalled
	case stopped != "" || ctx.Err() != nil:
		r.run.Status = domain.RunCanceled
	case completed:
		r.run.Status = domain.RunSucceeded
	case err != nil && strings.Contains(err.Error(), "agent blocked"):
		r.run.Status = domain.RunBlocked
	case err != nil && strings.Contains(strings.ToLower(err.Error()), "timeout"):
		r.run.Status = domain.RunTimedOut
	default:
		r.run.Status = domain.RunFailed
	}
	if err != nil {
		r.run.Error = err.Error()
	}
	run := r.run
	c.mu.Unlock()
	c.log.Info("agent logical run finished", "issue_id", run.IssueID, "issue_identifier", run.IssueIdentifier, "session_id", run.SessionID, "status", string(run.Status), "attempt", run.Attempt, "turn_count", run.TurnCount)
}

// recordCompletion performs one final tracker read after an event stream says
// the turn completed. Reconciliation can have taken a snapshot just before the
// worker removes itself from running; without this read, that interleaving can
// write a stale completion marker for a terminal or rerouted issue.
func (c *Coordinator) recordCompletion(parent context.Context, r *running, ws domain.Workspace, issue domain.Issue, attempt int, settings config.Settings) {
	if !c.completionAllowed(parent, r) {
		c.log.Info("agent run cancelled", "issue_id", issue.ID, "issue_identifier", issue.Identifier, "session_id", r.session.ID, "reason", cancellationReason(r.stopped, parent))
		c.release(issue.ID)
		return
	}
	fresh, err := c.tracker.GetIssues(parent, []string{issue.ID})
	if err != nil {
		if parent.Err() != nil {
			c.release(issue.ID)
			return
		}
		c.log.Warn("completion issue refresh failed", "issue_id", issue.ID, "issue_identifier", issue.Identifier, "session_id", r.session.ID, "error", err)
		c.scheduleRetry(parent, issue, ws, attempt+1, retryCompletion, "completion_reconcile", backoff(attempt+1, settings.Agent.MaxRetryBackoff))
		return
	}
	if len(fresh) != 1 || fresh[0].ID != issue.ID {
		c.log.Warn("completion issue refresh returned no matching issue", "issue_id", issue.ID, "issue_identifier", issue.Identifier, "session_id", r.session.ID)
		c.release(issue.ID)
		return
	}
	current := fresh[0]
	if !eligible(current, settings) {
		if terminal(current, settings) {
			if err := c.workspaces.Cleanup(parent, current); err != nil {
				c.log.Warn("terminal workspace cleanup failed", "issue_id", current.ID, "issue_identifier", current.Identifier, "error", err)
			}
		}
		c.release(issue.ID)
		return
	}
	if !sameIssueVersion(issue, current) {
		c.log.Info("completion marker skipped for updated issue", "issue_id", issue.ID, "issue_identifier", issue.Identifier, "session_id", r.session.ID)
		c.release(issue.ID)
		return
	}
	if !c.completionAllowed(parent, r) {
		c.log.Info("agent run cancelled", "issue_id", issue.ID, "issue_identifier", issue.Identifier, "session_id", r.session.ID, "reason", cancellationReason(r.stopped, parent))
		c.release(issue.ID)
		return
	}
	if err := c.workspaces.MarkCompleted(parent, ws, current); err != nil {
		c.log.Error("record completed work failed", "issue_id", issue.ID, "issue_identifier", issue.Identifier, "session_id", r.session.ID, "error", err)
		c.scheduleRetry(parent, issue, ws, attempt+1, retryCompletion, "completion_marker", backoff(attempt+1, settings.Agent.MaxRetryBackoff))
		return
	}
	c.release(issue.ID)
}

func (c *Coordinator) completionAllowed(parent context.Context, r *running) bool {
	if parent.Err() != nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.stopping && r.stopped == ""
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
			c.logEvent(r, e)
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
				c.logTerminalSummary(r)
				return true, nil
			}
		}
	}
}

type retryDelayError interface {
	RetryDelay() time.Duration
}

func pollDelay(interval time.Duration, err error) time.Duration {
	delay := interval
	var retry retryDelayError
	if errors.As(err, &retry) && retry.RetryDelay() > delay {
		delay = retry.RetryDelay()
	}
	return delay
}

func (c *Coordinator) logEvent(r *running, event domain.Event) {
	sessionID := event.SessionID
	if sessionID == "" {
		sessionID = r.session.ID
	}
	attrs := []any{"issue_id", r.issue.ID, "issue_identifier", r.issue.Identifier, "session_id", sessionID, "event", event.Kind}
	switch event.Kind {
	case domain.EventSessionStarted:
		if event.ThreadID != "" {
			attrs = append(attrs, "thread_id", event.ThreadID)
		}
		if event.TurnID != "" {
			attrs = append(attrs, "turn_id", event.TurnID)
		}
		if event.PID > 0 {
			attrs = append(attrs, "pid", event.PID)
		}
		c.log.Info("agent session started", attrs...)
	case domain.EventUsage:
		usage := c.updateUsage(r, event.Usage)
		attrs = append(attrs, "input_tokens", usage.InputTokens, "output_tokens", usage.OutputTokens, "total_tokens", usage.TotalTokens)
		c.log.Info("agent usage", attrs...)
	case domain.EventRateLimit:
		rateLimit := normalizedRateLimit(event.RateLimit)
		c.mu.Lock()
		r.rateLimit = copyRateLimit(rateLimit)
		c.mu.Unlock()
		attrs = append(attrs, "rate_limit", rateLimit)
		c.log.Info("agent rate limit", attrs...)
	case domain.EventDiagnostic:
		attrs = append(attrs, "stderr", observability.Text(event.Message))
		c.log.Warn("agent stderr", attrs...)
	default:
		// Messages can contain model output, tool arguments, prompt excerpts, or
		// provider data. The event type is sufficient for ordinary operation.
		c.log.Info("agent event", attrs...)
	}
}

func (c *Coordinator) updateUsage(r *running, update domain.Usage) domain.Usage {
	update = normalizedUsage(update)
	c.mu.Lock()
	// App-server usage notifications are cumulative. Taking the component-wise
	// maximum makes repeated notifications idempotent and avoids double-counting.
	r.run.Usage.InputTokens = max(r.run.Usage.InputTokens, update.InputTokens)
	r.run.Usage.OutputTokens = max(r.run.Usage.OutputTokens, update.OutputTokens)
	r.run.Usage.TotalTokens = max(r.run.Usage.TotalTokens, update.TotalTokens)
	if r.run.Usage.TotalTokens < r.run.Usage.InputTokens+r.run.Usage.OutputTokens {
		r.run.Usage.TotalTokens = r.run.Usage.InputTokens + r.run.Usage.OutputTokens
	}
	usage := r.run.Usage
	c.mu.Unlock()
	return usage
}

func (c *Coordinator) logTerminalSummary(r *running) {
	c.mu.Lock()
	usage := r.run.Usage
	rateLimit := copyRateLimit(r.rateLimit)
	started := r.run.StartedAt
	attempt := r.run.Attempt
	turnCount := r.run.TurnCount
	c.mu.Unlock()
	attrs := []any{"issue_id", r.issue.ID, "issue_identifier", r.issue.Identifier, "session_id", r.session.ID, "attempt", attempt, "turn_count", turnCount, "runtime_ms", c.clock.Now().Sub(started).Milliseconds(), "input_tokens", usage.InputTokens, "output_tokens", usage.OutputTokens, "total_tokens", usage.TotalTokens}
	if len(rateLimit) > 0 {
		attrs = append(attrs, "rate_limit", rateLimit)
	}
	c.log.Info("agent turn completed", attrs...)
}

func copyRateLimit(value map[string]int64) map[string]int64 {
	if len(value) == 0 {
		return nil
	}
	copy := make(map[string]int64, len(value))
	for key, item := range value {
		copy[key] = item
	}
	return copy
}

func normalizedUsage(usage domain.Usage) domain.Usage {
	if usage.InputTokens < 0 {
		usage.InputTokens = 0
	}
	if usage.OutputTokens < 0 {
		usage.OutputTokens = 0
	}
	if usage.TotalTokens < 0 {
		usage.TotalTokens = 0
	}
	if usage.TotalTokens == 0 {
		usage.TotalTokens = usage.InputTokens + usage.OutputTokens
	}
	return usage
}

// normalizedRateLimit selects the small numeric subset useful to an operator.
// In particular it never serializes the raw app-server payload, which may
// include account, model, or provider-specific data.
func normalizedRateLimit(raw map[string]any) map[string]int64 {
	result := map[string]int64{}
	var visit func(map[string]any)
	visit = func(values map[string]any) {
		for key, value := range values {
			switch nested := value.(type) {
			case map[string]any:
				visit(nested)
			case float64:
				if nested < 0 {
					continue
				}
				switch strings.ToLower(strings.ReplaceAll(key, "_", "")) {
				case "limit", "remaining", "used", "resetseconds", "windowseconds":
					result[strings.ToLower(key)] = int64(nested)
				}
			case int:
				if nested >= 0 {
					switch strings.ToLower(strings.ReplaceAll(key, "_", "")) {
					case "limit", "remaining", "used", "resetseconds", "windowseconds":
						result[strings.ToLower(key)] = int64(nested)
					}
				}
			}
		}
	}
	visit(raw)
	return result
}

func (c *Coordinator) finishFailure(ctx context.Context, i domain.Issue, attempt int, reason string, err error) {
	if ctx.Err() != nil {
		c.log.Info("agent run cancelled", "issue_id", i.ID, "issue_identifier", i.Identifier, "reason", cancellationReason("", ctx))
		c.release(i.ID)
		return
	}
	c.scheduleRetry(ctx, i, domain.Workspace{}, attempt+1, retryAgent, reason, backoff(attempt+1, c.settings().Agent.MaxRetryBackoff))
	attrs := []any{"issue_id", i.ID, "issue_identifier", i.Identifier, "reason", reason, "attempt", attempt + 1}
	if err != nil && reason != "prompt_render" && reason != "agent_event" {
		attrs = append(attrs, "error", err)
	}
	c.log.Warn("agent run retry scheduled", attrs...)
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
		if norm(i.State) == norm(x) {
			return true
		}
	}
	return false
}
func terminal(i domain.Issue, s config.Settings) bool {
	for _, x := range s.Tracker.TerminalStates {
		if norm(i.State) == norm(x) {
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
		if a.CreatedAt != nil && b.CreatedAt != nil && !a.CreatedAt.Equal(*b.CreatedAt) {
			return a.CreatedAt.Before(*b.CreatedAt)
		}
		if (a.CreatedAt == nil) != (b.CreatedAt == nil) {
			return a.CreatedAt != nil
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
