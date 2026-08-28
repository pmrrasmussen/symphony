package coordinator

import (
	"context"
	"strings"
	"time"

	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/domain"
	"github.com/pmrrasmussen/symphony/internal/observability"
)

type running struct {
	issue domain.Issue
	// backend names the agent runtime that started this run. Continuation and
	// cancellation are routed by session, but the scheduler's own per-backend
	// policy lookups (currently the stall budget) resolve current settings under
	// this name, so a reload cannot start applying another backend's policy to a
	// run already in flight.
	backend   string
	session   domain.AgentSession
	last      time.Time
	cancel    context.CancelFunc
	workspace domain.Workspace
	stopped   stopReason
	rateLimit map[string]int64
	run       domain.Run
	// outstanding is the last app-server tool/item that started without a
	// matching completion. It is the actionable answer to "what is Codex
	// waiting on" for heartbeat and stall records.
	outstanding *outstandingOp
	// landingResolved records that this run's landing capability reported a
	// terminal outcome (the pull request is merged and the issue reconciled to
	// its terminal state). The run then ends without another turn even if the
	// tracker refresh has not yet observed the transition (PMR-78).
	landingResolved bool
	// lastGeneric* coalesce repeated generic progress notifications (protocol
	// methods Symphony does not otherwise classify) so an idle-looking but
	// chatty protocol stream cannot flood the log.
	lastGenericKind    domain.EventKind
	lastGenericMessage string
	genericRepeat      int
}

// logIssueRefreshFailure warns on a tracker refresh failure, unless the ctx
// used for that refresh has already ended. A cancelled ctx there is routine
// -- the run or retry timer it belonged to raced the in-flight request to
// completion -- and not a Linear problem worth an operator's attention.
func (c *Coordinator) logIssueRefreshFailure(ctx context.Context, msg string, args ...any) {
	if ctx.Err() != nil {
		c.log.Debug(msg, args...)
		return
	}
	c.log.Warn(msg, args...)
}

func (c *Coordinator) reconcile(ctx context.Context) error {
	type runRef struct {
		r     *running
		issue domain.Issue
	}
	c.mu.Lock()
	runs := make([]runRef, 0, len(c.states))
	for _, st := range c.states {
		if st.run != nil {
			runs = append(runs, runRef{r: st.run, issue: st.run.issue})
		}
	}
	c.mu.Unlock()
	if len(runs) == 0 {
		return nil
	}
	ids := make([]string, len(runs))
	for i, run := range runs {
		ids[i] = run.issue.ID
	}
	issues, err := c.tracker.GetIssues(ctx, ids)
	if err != nil {
		c.logIssueRefreshFailure(ctx, "running issue refresh failed", "error", err)
		return err
	}
	byID := map[string]domain.Issue{}
	for _, i := range issues {
		byID[i.ID] = i
	}
	s := c.settings()
	now := c.clock.Now()
	for _, run := range runs {
		r := run.r
		fresh, found := byID[run.issue.ID]
		reason := stopReason("")
		if !found || !eligible(fresh, s) {
			reason = stopIneligible
			if found && issueTerminal(fresh, s) {
				reason = stopTerminal
			}
		}
		c.mu.Lock()
		last := r.last
		c.mu.Unlock()
		launch, known := s.AgentLaunchFor(r.backend)
		if !known {
			// A run whose backend this configuration cannot describe has no
			// policy to apply. Say so: silently treating the zero budget as
			// "no stall timeout" would leave the run unsupervised.
			c.log.Warn("agent backend policy unavailable", "issue_identifier", r.issue.Identifier, "agent_backend", r.backend)
		}
		if reason == "" && known && launch.StallTimeout > 0 && now.Sub(last) > launch.StallTimeout {
			reason = stopStalled
		}
		if reason == "" {
			// Reconciliation is also the authoritative snapshot refresh. Keep
			// state accounting in step with tracker transitions while the run
			// remains eligible, so later admissions see the current state.
			c.refreshRunIssue(r, fresh)
			c.logHeartbeat(r, now, last)
			continue
		}
		if !c.stopRun(run.issue.ID, reason) {
			continue
		}
		// A run stopped because its issue left the active set for the review
		// handoff state is Symphony's own handoff; remember it so a later
		// external revert of that handoff is attributable at poll time.
		c.noteHandoffObservation(fresh, s, now)
		attrs := []any{"issue_id", run.issue.ID, "issue_identifier", run.issue.Identifier, "session_id", r.session.ID, "reason", reason}
		if reason == stopStalled {
			attrs = append(attrs, c.outstandingAttrs(r, now, last)...)
		}
		if reason == stopTerminal {
			c.cleanupWorkspace(ctx, fresh)
		}
		c.log.Info("agent reconciled", attrs...)
	}
	return nil
}

// launch reserves capacity before starting asynchronous preparation. This
// closes the gap where several goroutines could otherwise all observe room
// before any of them had inserted a backend session into running.
//
// The reservation ownership rule (PMR-121): the goroutine below owns exactly
// the reservation reserveLocked minted for it, identified by `reservation`,
// and every release names that generation. So each exit releases the slot the
// moment this launch stops needing it -- which is what keeps a failure or
// landing-wait redispatch from idling a worker slot behind its timer -- while
// the deferred backstop covers the exits that do not, and neither can free a
// slot a later dispatch of the same issue has already taken. Exactly one
// release per reservation: the first call that matches wins and the rest are
// no-ops.
func (c *Coordinator) launch(parent context.Context, i domain.Issue, attempt int) bool {
	s := c.settings()
	c.mu.Lock()
	if c.stopping {
		c.mu.Unlock()
		c.release(i.ID)
		return false
	}
	reservation := c.reserveLocked(i, s)
	if reservation == 0 {
		c.mu.Unlock()
		return false
	}
	c.wg.Add(1)
	c.mu.Unlock()
	go func() {
		defer c.wg.Done()
		defer c.unreserve(i.ID, reservation)
		ctx, cancel := context.WithCancel(parent)
		defer cancel()
		c.log.Debug("workspace preparation started", "issue_id", i.ID, "issue_identifier", i.Identifier, "attempt", attempt)
		ws, err := c.workspaces.Prepare(ctx, i)
		if err != nil {
			c.unreserve(i.ID, reservation)
			c.finishFailure(parent, i, attempt, "workspace_prepare", err)
			return
		}
		c.log.Debug("workspace prepared", "issue_id", i.ID, "issue_identifier", i.Identifier, "attempt", attempt, "created", ws.CreatedNow)
		if err = c.workspaces.BeforeRun(ctx, ws, i); err != nil {
			c.workspaces.AfterRun(context.Background(), ws, i)
			c.unreserve(i.ID, reservation)
			c.finishFailure(parent, i, attempt, "before_run", err)
			return
		}
		s := c.settings()
		if state := c.transitionToStarted(ctx, i, s); state != "" {
			i.State = state
		}
		// The launch is resolved before the prompt, not after, because the
		// prompt has to be rendered for the backend that will actually run it:
		// how a bounded capability is named is a property of the transport. One
		// resolution feeds both, so the guidance and the session cannot describe
		// different backends.
		launch := s.AgentLaunch()
		prompt, deliveryBytes, err := render(s, i, attempt, launch.Backend)
		if err != nil {
			c.workspaces.AfterRun(context.Background(), ws, i)
			c.unreserve(i.ID, reservation)
			c.finishFailure(parent, i, attempt, "prompt_render", err)
			return
		}
		// Byte lengths only, never the prompt itself: this is what answers
		// whether trimming WORKFLOW.md's prompt body is worth anything.
		c.log.Debug("prompt rendered", "issue_id", i.ID, "issue_identifier", i.Identifier, "attempt", attempt, "prompt_bytes", len(prompt), "delivery_instruction_bytes", deliveryBytes)
		c.log.Debug("agent launch requested", "issue_id", i.ID, "issue_identifier", i.Identifier, "attempt", attempt, "agent_backend", launch.Backend)
		session, events, err := c.agent.Start(ctx, domain.AgentRequest{Issue: i, Backend: launch.Backend, Model: launch.Model, Workspace: ws.Path, GitMetadataRoots: ws.GitMetadataRoots, Prompt: prompt, Command: launch.Command, ApprovalPolicy: launch.ApprovalPolicy, ThreadSandbox: launch.ThreadSandbox, TurnSandboxPolicy: launch.TurnSandboxPolicy, TurnTimeout: launch.TurnTimeout, ReadTimeout: launch.ReadTimeout, StartTimeout: launch.StartTimeout})
		if err != nil {
			c.workspaces.AfterRun(context.Background(), ws, i)
			c.unreserve(i.ID, reservation)
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
			issue: i, backend: runBackend(session, launch), session: session, last: now, cancel: cancel, workspace: ws,
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
		c.ensureStateLocked(i.ID).run = r
		c.mu.Unlock()
		ended, _, consumeErr := c.runTurns(ctx, r, events, s)
		c.mu.Lock()
		if st := c.stateLocked(i.ID); st != nil && st.run == r {
			st.run = nil
			c.pruneLocked(i.ID)
		}
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
			// A run that ended because its issue reached the review handoff
			// state is Symphony's own handoff; remember it so an external
			// revert of that handoff is attributable at the next poll.
			c.mu.Lock()
			final := r.issue
			c.mu.Unlock()
			c.noteHandoffObservation(final, s, c.clock.Now())
			c.release(i.ID)
			return
		}
		if stopped != "" || ctx.Err() != nil {
			c.log.Info("agent run cancelled", "issue_id", i.ID, "issue_identifier", i.Identifier, "session_id", session.ID, "reason", cancellationReason(stopped, ctx))
			if stopped == stopStalled {
				c.unreserve(i.ID, reservation)
				c.finishFailure(parent, i, attempt, "stalled", context.DeadlineExceeded)
				return
			}
			c.release(i.ID)
			return
		}
		c.unreserve(i.ID, reservation)
		if wait, ok := landingWait(consumeErr); ok {
			c.finishLandingWait(parent, i, attempt, wait.reason)
			return
		}
		c.finishFailure(parent, i, attempt, agentFailureReason(consumeErr), consumeErr)
	}()
	return true
}

// transitionToStarted deterministically moves a freshly dispatched issue from
// its configured unstarted active state into the started active state (the
// canonical lifecycle's Todo -> In Progress) using the host tracker
// credential, so the board reflects in-progress work without relying on the
// agent to self-transition. It is:
//   - idempotent: an issue whose current state has no configured start edge, or
//     that is already in the target state, is left untouched. The adapter also
//     re-reads and no-ops if the live state already matches, so a run
//     re-dispatched after a restart or turn-limit exhaustion (already In
//     Progress) is never re-transitioned;
//   - fail-safe: a failed transition is logged and never blocks the run or
//     causes a double dispatch — the session starts regardless, and the poll
//     loop retries or reconciles the tracker state on a later tick.
func (c *Coordinator) transitionToStarted(ctx context.Context, i domain.Issue, s config.Settings) string {
	target, ok := s.Tracker.HostTransitions.Start[config.Norm(i.State)]
	if !ok || strings.TrimSpace(target) == "" {
		return ""
	}
	if strings.EqualFold(strings.TrimSpace(i.State), strings.TrimSpace(target)) {
		return target
	}
	if err := c.tracker.Transition(ctx, i, target); err != nil {
		c.log.Warn("dispatch start transition failed", "operation", observability.OperationStartTransition, "issue_id", i.ID, "issue_identifier", i.Identifier, "from_state", config.Norm(i.State), "to_state", config.Norm(target), "error", err)
		return ""
	}
	c.log.Info("issue moved to started state", "operation", observability.OperationStartTransition, "issue_id", i.ID, "issue_identifier", i.Identifier, "from_state", config.Norm(i.State), "to_state", config.Norm(target))
	return target
}

func (c *Coordinator) refreshRunIssue(r *running, fresh domain.Issue) {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.stateLocked(fresh.ID)
	if st == nil || st.run != r || r.stopped != "" {
		return
	}
	r.issue = fresh
	// One record, so the claim's state and the state its reservation is
	// counted under move together by construction; setClaimStateLocked carries
	// the per-state tally with them.
	c.setClaimStateLocked(st, config.Norm(fresh.State))
}

// render returns the prompt sent to the agent along with the byte length of
// the delivery-instruction suffix, so a caller can report the rendered size
// without ever logging the prompt itself.
func render(s config.Settings, i domain.Issue, attempt int, backend string) (string, int, error) {
	prompt, err := s.Render(i, attempt)
	if err != nil {
		return "", 0, err
	}
	instructions := s.DeliveryInstructions(backend)
	return prompt + "\n\n" + instructions, len(instructions), nil
}
