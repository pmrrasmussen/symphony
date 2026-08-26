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
	retryAgent retryKind = "agent"
	// retryLanding is a coordinator-owned landing redispatch after the
	// host-side landing capability reported a non-terminal wait. It is
	// deliberately distinct from retryAgent: it is not an agent failure, it does
	// not escalate the attempt, and its delay follows the GitHub poll interval
	// instead of the failure backoff (PMR-78).
	retryLanding      retryKind = "landing"
	continuationDelay           = time.Second
	// defaultPollRetryDelay is the fixed-cadence redispatch floor -- for a
	// contended landing slot or a contended orchestrator slot alike -- when no
	// poll interval is configured to derive one from.
	defaultPollRetryDelay = 30 * time.Second
)

// handoffObservationFloor is the lower bound for how long the coordinator
// remembers that it drove an issue into the review handoff state. The effective
// retention is max(this, 2*poll interval) so a poll always runs while the
// memory is live and an external automation that reverts the handoff (the
// PMR-63 In Review -> In Progress flap) is observed and logged rather than
// silently re-dispatched with no trace. Healthy handoffs that stay in review
// are swept out after this window, so the map never grows unbounded.
const handoffObservationFloor = 2 * time.Minute

// turnLimitError means that the agent used its bounded session without
// reaching a verified handoff or a terminal tracker state. It is deliberately
// retriable: the issue remains active and no terminal tracker transition occurs
// for work that may still be incomplete.
type turnLimitError struct{ limit int }

func (e turnLimitError) Error() string {
	return fmt.Sprintf("agent turn limit exhausted after %d turns while issue remains active", e.limit)
}

// landingWaitError means the host-side landing capability returned a
// non-terminal waiting result: required checks or GitHub's own mergeability
// computation have not settled, so no further model turn can advance the issue.
// It is deliberately NOT an agent failure — the run ends, the worker slot is
// released, and the coordinator redispatches the same attempt after a bounded
// delay, so a wait consumes neither agent.max_turns nor the failure backoff
// escalation (PMR-78). The reason is the github package's own fixed, bounded,
// secret-free waiting string.
type landingWaitError struct{ reason string }

func (e landingWaitError) Error() string {
	if e.reason == "" {
		return "landing waiting"
	}
	return "landing waiting: " + e.reason
}

// landingWait reports whether an agent run ended on a landing wait.
func landingWait(err error) (landingWaitError, bool) {
	var wait landingWaitError
	if errors.As(err, &wait) {
		return wait, true
	}
	return landingWaitError{}, false
}

// blockedError carries only a normalized blocker category. Agent event text
// can contain model or provider data and must not be copied into scheduler
// logs or retry state.
type blockedError struct{ category string }

func (e blockedError) Error() string { return "agent blocked: " + e.category }

// rateLimitedError reports a definitive Claude quota rejection (PMR-131). It
// carries the backend's own reset time, when one was reported, so
// finishFailure can schedule the next attempt from it instead of the
// ordinary attempt-keyed backoff ladder -- a rejection says nothing about
// this issue's work, and the account-wide window it names routinely
// outlasts agent.max_retry_backoff_ms by hours.
type rateLimitedError struct{ retryAfter time.Duration }

func (e rateLimitedError) Error() string { return "agent rate limited" }

// errStreamClosed means consume's event channel closed without ever
// delivering a terminal event. Every backend emits EventFailed or
// EventCompleted before it closes its channel (see claude/backend.go and
// codex/backend.go), so this is not a model- or provider-reported failure at
// all -- it is the host's own event plumbing failing to deliver a verdict,
// and it is a sentinel rather than a wrapped cause because there is no
// further error upstream to name.
var errStreamClosed = errors.New("agent event stream closed before completion")

// issueRefreshError wraps the tracker error from runTurns' post-turn
// GetIssues refresh, so agentFailureReason can name it distinctly from a
// stream failure or a continuation failure -- it is the case PMR-115
// confirmed live: a Linear timeout on the refresh that follows a turn the
// agent completed successfully.
type issueRefreshError struct{ err error }

func (e issueRefreshError) Error() string { return "refresh issue after turn: " + e.err.Error() }
func (e issueRefreshError) Unwrap() error { return e.err }

// sessionContinueError wraps the backend error from runTurns' agent.Continue
// call. It is Symphony's own backend adapter refusing or failing to resume a
// session, never model text, so it carries the same distinct treatment as
// issueRefreshError.
type sessionContinueError struct{ err error }

func (e sessionContinueError) Error() string { return "continue agent session: " + e.err.Error() }
func (e sessionContinueError) Unwrap() error { return e.err }

func blockerCategory(message string) string {
	switch {
	case strings.Contains(message, "interactive approval or input"):
		return "interactive_input"
	case strings.Contains(message, "GitHub publication"):
		return "github_publication"
	case strings.Contains(message, "Linear handoff"):
		return "linear_handoff"
	case strings.Contains(message, "unsupported client-side tool"):
		return "unsupported_tool"
	default:
		return "agent_reported"
	}
}

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
	// waiting records an eligible issue that could not reserve an orchestrator
	// slot this poll and was never claimed, so no retry timer exists to
	// remember it (PMR-139): a candidate rejected only for capacity is
	// re-evaluated fresh from ListCandidates on the next poll, with nothing else
	// tracking it in the interim. It is keyed by issue ID and cleared the moment
	// the issue is no longer seen at capacity -- admitted, turned ineligible, or
	// dropped from the tracker's candidate list.
	waiting map[string]waitingState
	// waitingEscalated marks, per waiting issue, that the "still waiting for
	// capacity" Warn has already fired once, mirroring landingEscalated: a
	// queue that stays contended keeps logging Info every poll transition, not
	// a Warn every poll.
	waitingEscalated map[string]bool
	nextRetry        uint64
	stopping         bool
	ctx              context.Context
	cancel           context.CancelFunc
	wg               sync.WaitGroup
}

// handoffObservation is the coordinator's memory of one host-driven transition
// into the review handoff state. It stores only the normalized state name and
// the time of the observation — never issue content — so the poll loop can
// attribute a later active-state re-appearance to an external actor.
type handoffObservation struct {
	state string
	at    time.Time
}

// waitingState is the coordinator's memory of one eligible issue that has not
// yet reserved an orchestrator slot. It stores the issue itself, exactly as
// retryState does, only so Snapshot can report its current identifier and
// state -- since is the timestamp of the first poll it was seen unadmitted.
type waitingState struct {
	issue domain.Issue
	since time.Time
}

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

// outstandingOp is the coordinator's view of a started-but-not-yet-completed
// app-server item or dynamic tool call. It never stores anything derived from
// tool arguments, command bodies, or outputs.
type outstandingOp struct {
	ItemID, ItemType, ToolName string
	Since                      time.Time
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

// Snapshot is a read-only, intentionally reduced view of coordinator state.
// It excludes issue bodies, prompts, workspace paths, raw events, and tracker
// identifiers that may be provider-specific.
type Snapshot struct {
	Claimed  int               `json:"claimed"`
	Running  []RunningSnapshot `json:"running"`
	Retrying []RetrySnapshot   `json:"retrying"`
	// Waiting lists an eligible issue that has reserved neither an
	// orchestrator slot nor (unlike Retrying) a retry timer: a candidate the
	// poll rejected only for capacity, re-checked fresh on every poll (PMR-139).
	// It never overlaps Running or Retrying -- a claimed issue is removed here
	// the moment it is claimed.
	Waiting  []WaitingSnapshot `json:"waiting"`
	Stopping bool              `json:"stopping"`
}

type RunningSnapshot struct {
	IssueIdentifier      string                        `json:"issue_identifier"`
	IssueState           string                        `json:"issue_state"`
	SessionID            string                        `json:"session_id"`
	ThreadID             string                        `json:"thread_id"`
	TurnID               string                        `json:"turn_id"`
	Attempt              int                           `json:"attempt"`
	TurnCount            int                           `json:"turn_count"`
	StartedAt            time.Time                     `json:"started_at"`
	LastEventAt          time.Time                     `json:"last_activity_at"`
	Usage                domain.Usage                  `json:"usage"`
	RateLimit            map[string]int64              `json:"rate_limit,omitempty"`
	OutstandingOperation *OutstandingOperationSnapshot `json:"outstanding_operation,omitempty"`
}

type RetrySnapshot struct {
	IssueIdentifier string `json:"issue_identifier"`
	Attempt         int    `json:"attempt"`
	Kind            string `json:"kind"`
	Reason          string `json:"reason"`
	// WaitAttempt is the number of consecutive landing waits behind a
	// "landing" retry. It is the operator's "this landing is stuck" signal:
	// the agent attempt deliberately stays put for a non-failure, so a climbing
	// wait count (and the growing delay it drives) is what distinguishes a slow
	// check run from a gate that will never settle (PMR-78).
	WaitAttempt int       `json:"wait_attempt,omitempty"`
	Due         time.Time `json:"due_at"`
}

// WaitingSnapshot is one issue eligible for dispatch but not yet admitted.
// WaitingMS is how long it has gone unadmitted, in milliseconds, computed at
// snapshot time rather than stored as a duration so JSON does not have to
// carry Go's duration encoding.
type WaitingSnapshot struct {
	IssueIdentifier string    `json:"issue_identifier"`
	IssueState      string    `json:"issue_state"`
	Since           time.Time `json:"since"`
	WaitingMS       int64     `json:"waiting_ms"`
}

// OutstandingOperationSnapshot identifies the one safe app-server operation
// that has started but not finished. It intentionally excludes arguments,
// command bodies, outputs, and the protocol item's opaque identifier.
type OutstandingOperationSnapshot struct {
	Type      string    `json:"type"`
	Name      string    `json:"name,omitempty"`
	StartedAt time.Time `json:"started_at"`
	AgeMS     int64     `json:"age_ms"`
}

// Snapshot copies the coordinator's public operational metadata while holding
// its mutex, so callers cannot observe or mutate its live scheduling maps.
func (c *Coordinator) Snapshot() Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.clock.Now()
	snapshot := Snapshot{Claimed: len(c.claimed), Running: make([]RunningSnapshot, 0, len(c.running)), Retrying: make([]RetrySnapshot, 0, len(c.retries)), Waiting: make([]WaitingSnapshot, 0, len(c.waiting)), Stopping: c.stopping}
	for _, run := range c.running {
		item := snapshotOutstanding(run.outstanding, now)
		snapshot.Running = append(snapshot.Running, RunningSnapshot{IssueIdentifier: run.issue.Identifier, IssueState: run.issue.State, SessionID: run.session.ID, ThreadID: run.session.ThreadID, TurnID: run.session.TurnID, Attempt: run.run.Attempt, TurnCount: run.run.TurnCount, StartedAt: run.run.StartedAt, LastEventAt: run.last, Usage: run.run.Usage, RateLimit: copyRateLimit(run.rateLimit), OutstandingOperation: item})
	}
	for _, retry := range c.retries {
		snapshot.Retrying = append(snapshot.Retrying, RetrySnapshot{IssueIdentifier: retry.issue.Identifier, Attempt: retry.attempt, Kind: string(retry.kind), Reason: retry.reason, WaitAttempt: c.landingWaits[retry.issue.ID], Due: retry.due})
	}
	for _, wait := range c.waiting {
		age := now.Sub(wait.since).Milliseconds()
		if age < 0 {
			age = 0
		}
		snapshot.Waiting = append(snapshot.Waiting, WaitingSnapshot{IssueIdentifier: wait.issue.Identifier, IssueState: wait.issue.State, Since: wait.since, WaitingMS: age})
	}
	sort.Slice(snapshot.Running, func(i, j int) bool { return snapshot.Running[i].IssueIdentifier < snapshot.Running[j].IssueIdentifier })
	sort.Slice(snapshot.Retrying, func(i, j int) bool {
		return snapshot.Retrying[i].IssueIdentifier < snapshot.Retrying[j].IssueIdentifier
	})
	sort.Slice(snapshot.Waiting, func(i, j int) bool {
		return snapshot.Waiting[i].IssueIdentifier < snapshot.Waiting[j].IssueIdentifier
	})
	return snapshot
}

func snapshotOutstanding(operation *outstandingOp, now time.Time) *OutstandingOperationSnapshot {
	if operation == nil {
		return nil
	}
	age := now.Sub(operation.Since).Milliseconds()
	if age < 0 {
		age = 0
	}
	return &OutstandingOperationSnapshot{Type: operation.ItemType, Name: operation.ToolName, StartedAt: operation.Since, AgeMS: age}
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
	now := c.clock.Now()
	c.sweepHandoffObservations(now, s)
	issues, err := c.tracker.ListCandidates(ctx, s.Tracker.ActiveStates)
	if err != nil {
		c.log.Error("candidate poll failed", "error", err)
		return err
	}
	sortIssues(issues)
	summary := pollSummary{candidates: len(issues), rejected: map[string]int64{}}
	waiting := map[string]domain.Issue{}
	for _, i := range issues {
		if ctx.Err() != nil || c.isStopping() {
			c.logPollSummary(summary)
			return ctx.Err()
		}
		// A candidate the tracker returns in an active state that Symphony itself
		// just handed off to the review state was moved by someone else: a human
		// review decision (approve for landing, or send back for rework), or an
		// external reversion of the handoff (see PMR-63: Linear's native GitHub
		// PR automation). Log the external delta, classified, so the edge is
		// visible in the JSONL instead of only in Linear's history. Symphony does
		// not itself re-assert the handoff here.
		c.notePostHandoffStateChange(i, s, now)
		if reason := ineligibleReason(i, s); reason != "" {
			summary.rejected[reason]++
			attrs := []any{"issue_identifier", i.Identifier, "reason", reason}
			if reason == "blocked_by_relation" {
				attrs = append(attrs, "blocked_by", blockerIdentifiers(openBlockers(i)))
			}
			c.log.Debug("poll candidate rejected", attrs...)
			continue
		}
		summary.eligible++
		if reason := c.admissionRejectReason(i, s); reason != "" {
			summary.rejected[reason]++
			c.log.Debug("poll candidate rejected", "issue_identifier", i.Identifier, "reason", reason)
			if reason == "at_capacity" {
				waiting[i.ID] = i
			}
			continue
		}
		if !c.claim(i, s) {
			// A concurrent reconciliation or retry changed capacity between the
			// check above and this claim; still a rejection, just a narrower one.
			summary.rejected["claim_raced"]++
			continue
		}
		summary.admitted++
		if !c.launch(ctx, i, 0) {
			c.release(i.ID)
			summary.admitted--
			summary.rejected["launch_reservation_lost"]++
		}
	}
	c.updateWaiting(waiting, now, s)
	c.logPollSummary(summary)
	return nil
}

// pollSummary is the opt-in debug accounting of one poll pass: how many
// candidates the tracker returned, how many were eligible, how many were
// admitted (reserved an orchestrator slot), and a categorized count of every
// rejection. It never carries issue identifiers or content, only counts.
type pollSummary struct {
	candidates, eligible, admitted int
	rejected                       map[string]int64
}

func (c *Coordinator) logPollSummary(summary pollSummary) {
	attrs := []any{"candidates", summary.candidates, "eligible", summary.eligible, "admitted", summary.admitted}
	if len(summary.rejected) > 0 {
		attrs = append(attrs, "rejected", summary.rejected)
	}
	c.log.Debug("poll summary", attrs...)
}

// waitingEscalationFloor is the lower bound on how long an eligible issue can
// sit unadmitted for capacity before the wait is escalated to Warn. The
// effective threshold is max(this, waitingEscalationMultiplier*poll interval),
// mirroring handoffObservationFloor, so a fast-polling instance does not warn
// after a couple of missed cycles and a slow-polling one is not held to an
// unreasonably short deadline.
const waitingEscalationFloor = 5 * time.Minute

// waitingEscalationMultiplier is how many poll intervals an issue may go
// unadmitted before waitingEscalationFloor's alternative kicks in. It is
// deliberately generous: losing one or two admission races to fresher
// candidates (PMR-129) is the queue working as designed, not a stuck issue.
const waitingEscalationMultiplier = 10

// updateWaiting reconciles the coordinator's memory of eligible issues
// rejected only for capacity ("at_capacity") this poll against the previous
// poll's memory, and reports every genuinely new entry once by logging its
// identifier and state -- the one thing pollSummary is not allowed to carry.
// An issue absent from seen this poll is no longer waiting for whatever
// reason (admitted, turned ineligible, or dropped from the tracker's
// candidate list) and its memory is dropped here, so the waiting set can only
// ever describe issues this exact poll actually re-observed at capacity.
func (c *Coordinator) updateWaiting(seen map[string]domain.Issue, now time.Time, s config.Settings) {
	c.mu.Lock()
	for id := range c.waiting {
		if _, ok := seen[id]; !ok {
			delete(c.waiting, id)
			delete(c.waitingEscalated, id)
		}
	}
	var newlyWaiting []domain.Issue
	for id, issue := range seen {
		entry, already := c.waiting[id]
		if !already {
			entry = waitingState{issue: issue, since: now}
			newlyWaiting = append(newlyWaiting, issue)
		} else {
			entry.issue = issue
		}
		c.waiting[id] = entry
	}
	c.mu.Unlock()
	for _, issue := range newlyWaiting {
		c.log.Info("issue eligible but waiting for capacity", "issue_id", issue.ID, "issue_identifier", issue.Identifier, "issue_state", config.Norm(issue.State))
	}
	c.escalateStuckWaits(now, s)
}

// escalateStuckWaits raises a one-time Warn for any issue that has sat in the
// waiting set past waitingEscalationFloor's effective threshold, exactly the
// way finishLandingWait escalates a landing wait past
// landingWaitEscalated. Below the threshold the Info logged by updateWaiting
// on entry is enough; at and above it a wait that is still recurring is no
// longer distinguishable, on the timeline alone, from a queue that will never
// clear.
func (c *Coordinator) escalateStuckWaits(now time.Time, s config.Settings) {
	threshold := waitingEscalationFloor
	if window := waitingEscalationMultiplier * s.Polling.Interval; window > threshold {
		threshold = window
	}
	c.mu.Lock()
	var escalated []domain.Issue
	for id, entry := range c.waiting {
		if c.waitingEscalated[id] || now.Sub(entry.since) < threshold {
			continue
		}
		c.waitingEscalated[id] = true
		escalated = append(escalated, entry.issue)
	}
	c.mu.Unlock()
	for _, issue := range escalated {
		c.log.Warn("issue still waiting for capacity", "issue_id", issue.ID, "issue_identifier", issue.Identifier, "issue_state", config.Norm(issue.State))
	}
}

// ineligibleReason mirrors eligible's own checks so a rejected candidate's
// debug record explains exactly which one failed. blocked_by_relation is
// split out from the generic not_routable so a Todo issue held by an open
// blocker (PMR-146) is distinguishable, at the poll log, from one rejected
// for an assignee mismatch or a missing required label.
//
// The assignee check is ordered ahead of the blocker check because
// dispatchable() in internal/linear/tracker.go decides Dispatchable in that
// same order: an assignee-policy mismatch fails an issue regardless of its
// blockers, so an issue carrying both must not be misreported as
// blocked_by_relation, which would name a resolvable blocker as the cause of
// something an operator resolving it would never fix.
func ineligibleReason(i domain.Issue, s config.Settings) string {
	switch {
	case i.ID == "" || i.Identifier == "" || i.Title == "":
		return "missing_identity"
	case !active(i, s):
		return "not_active"
	case issueTerminal(i, s):
		return "terminal"
	case !i.Dispatchable && assigneeMismatch(i, s):
		return "not_routable"
	case !i.Dispatchable && len(openBlockers(i)) > 0:
		return "blocked_by_relation"
	case !routable(i, s):
		return "not_routable"
	default:
		return ""
	}
}

// assigneeMismatch reports whether the issue's own assignee is the reason
// dispatchable() (internal/linear/tracker.go) refused it, using the same
// tracker.provider.assignee policy value dispatchable() itself reads. An
// unconfigured policy never mismatches, matching dispatchable()'s own
// unconditional pass in that case.
func assigneeMismatch(i domain.Issue, s config.Settings) bool {
	configured, _ := s.Tracker.Provider["assignee"].(string)
	configured = strings.TrimSpace(configured)
	return configured != "" && i.AssigneeID != configured
}

// openBlockers is the subset of the issue's blockers that are not yet
// resolved -- the ones actually responsible for a Dispatchable=false Todo
// issue -- so a poll rejection can name the blocker instead of only refusing
// the candidate.
func openBlockers(i domain.Issue) []domain.Blocker {
	var open []domain.Blocker
	for _, b := range i.BlockedBy {
		if !b.Dispatchable {
			open = append(open, b)
		}
	}
	return open
}

// blockerIdentifiers renders a safe, content-free log value from a blocker
// list: tracker issue identifiers only, never titles or descriptions, joined
// into a plain string so the observability logger's attribute allowlist (which
// omits unrecognized non-scalar kinds) does not drop it.
func blockerIdentifiers(blockers []domain.Blocker) string {
	identifiers := make([]string, 0, len(blockers))
	for _, b := range blockers {
		if b.Identifier != "" {
			identifiers = append(identifiers, b.Identifier)
		}
	}
	return strings.Join(identifiers, ",")
}

// admissionRejectReason peeks at claim's own admission checks purely to
// categorize a poll rejection; it does not itself claim or mutate state.
func (c *Coordinator) admissionRejectReason(i domain.Issue, s config.Settings) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case c.stopping:
		return "stopping"
	case c.claimed[i.ID]:
		return "already_claimed"
	case !c.capacityAvailableLocked(config.Norm(i.State), s):
		return "at_capacity"
	default:
		return ""
	}
}

// noteHandoffObservation records that Symphony itself drove issue into the
// configured review handoff state, so a later external revert of that handoff
// is attributable at poll time. It is a no-op when no handoff state is
// configured or the issue is not in it.
func (c *Coordinator) noteHandoffObservation(issue domain.Issue, s config.Settings, now time.Time) {
	handoff := config.Norm(s.Tracker.HandoffState)
	if handoff == "" || issue.ID == "" || config.Norm(issue.State) != handoff {
		return
	}
	c.mu.Lock()
	c.handoffs[issue.ID] = handoffObservation{state: handoff, at: now}
	c.mu.Unlock()
}

// notePostHandoffStateChange logs, exactly once, when an active candidate is an
// issue Symphony recently handed off to the review state. Every such change is
// external — the review state is human-controlled and Symphony has no
// In Review -> active writer — but not every one is a fault: the human approval
// and rework decisions are the lifecycle working as designed and are logged as
// expected changes, while an unexpected reactivation stays a warning. It
// consumes the observation so a single change is reported once, and never
// mutates the tracker — re-asserting a reverted handoff is a documented
// follow-up.
func (c *Coordinator) notePostHandoffStateChange(i domain.Issue, s config.Settings, now time.Time) {
	if config.Norm(s.Tracker.HandoffState) == "" || i.ID == "" {
		return
	}
	c.mu.Lock()
	observation, ok := c.handoffs[i.ID]
	if ok {
		delete(c.handoffs, i.ID)
	}
	c.mu.Unlock()
	if !ok || config.Norm(i.State) == observation.state {
		return
	}
	operation := postHandoffOperation(config.Norm(i.State), s)
	attrs := []any{
		"operation", operation,
		"issue_id", i.ID,
		"issue_identifier", i.Identifier,
		"from_state", observation.state,
		"to_state", config.Norm(i.State),
		"since_handoff_ms", now.Sub(observation.at).Milliseconds(),
	}
	if operation == observability.OperationExternalReversion {
		c.log.Warn("external tracker state change observed", attrs...)
		return
	}
	c.log.Info("human review state change observed", attrs...)
}

// postHandoffOperation classifies one state change out of the review handoff
// state that Symphony did not perform. Moving the issue into the configured
// github.merge_state is the documented human approval to land, and moving it
// into the lifecycle's rework state is the documented human request for
// changes; both are expected. Everything else — including any destination
// Symphony cannot name from the configured lifecycle — contradicts the handoff
// by reactivating handed-off work as though implementation had not happened
// (the PMR-63 flap of the tracker's native PR-to-status automation) and stays
// an actionable warning. The warning is the default on purpose: a silent
// expected-change record for a state Symphony merely failed to recognize would
// hide exactly the fault this record exists to surface.
func postHandoffOperation(to string, s config.Settings) observability.Operation {
	switch {
	case to != "" && to == config.Norm(s.GitHub.MergeState):
		return observability.OperationReviewApproved
	case reworkDecision(to, s):
		return observability.OperationReworkRequested
	default:
		return observability.OperationExternalReversion
	}
}

// reworkDecision reports whether state is the lifecycle's human rework state.
// Symphony names that state by elimination against the configured lifecycle:
// tracker.provider.transitions.start enumerates the pre-review implementation
// states (the canonical Todo -> In Progress edge) and github.merge_state is the
// landing authorization, so removing both from active_states leaves the states
// only a human review decision moves an issue into.
//
// That naming is trusted only when exactly one state remains, which is the
// canonical lifecycle's Rework. With no start policy configured, or with two or
// more remaining candidates (an extra parked state such as Blocked, a Backlog
// in active_states, or a dispatch entry state that no start edge names),
// Symphony cannot tell the rework state from a state an external writer parked
// handed-off work in, so nothing qualifies and every such change keeps its
// warning. The merge state is excluded here too, so this predicate is correct
// on its own rather than relying on postHandoffOperation's case order.
func reworkDecision(state string, s config.Settings) bool {
	if state == "" || state == config.Norm(s.GitHub.MergeState) {
		return false
	}
	candidates := reworkCandidates(s)
	return len(candidates) == 1 && candidates[0] == state
}

// reworkCandidates returns the normalized active states that neither the host
// start policy nor the merge state accounts for. An empty start policy yields
// no candidates: without it Symphony cannot identify the pre-review
// implementation states, so it can name nothing by elimination.
func reworkCandidates(s config.Settings) []string {
	if len(s.Tracker.HostTransitions.Start) == 0 {
		return nil
	}
	accounted := map[string]bool{config.Norm(s.GitHub.MergeState): true}
	for source, target := range s.Tracker.HostTransitions.Start {
		accounted[config.Norm(source)] = true
		accounted[config.Norm(target)] = true
	}
	candidates := make([]string, 0, len(s.Tracker.ActiveStates))
	for _, state := range s.Tracker.ActiveStates {
		name := config.Norm(state)
		if name == "" || accounted[name] {
			continue
		}
		accounted[name] = true // Also de-duplicates a repeated active state.
		candidates = append(candidates, name)
	}
	return candidates
}

// sweepHandoffObservations discards handoff memories older than the retention
// window (max of the floor and two poll intervals, so a poll always runs while
// a memory is live). A healthy handoff that stays in review is never reverted
// and so is only ever cleared here, keeping the map bounded.
func (c *Coordinator) sweepHandoffObservations(now time.Time, s config.Settings) {
	ttl := handoffObservationFloor
	if window := 2 * s.Polling.Interval; window > ttl {
		ttl = window
	}
	c.mu.Lock()
	for id, observation := range c.handoffs {
		if now.Sub(observation.at) > ttl {
			delete(c.handoffs, id)
		}
	}
	c.mu.Unlock()
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
	runs := make([]runRef, 0, len(c.running))
	for _, r := range c.running {
		runs = append(runs, runRef{r: r, issue: r.issue})
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

// logHeartbeat is opt-in debug detail: the last-activity age and any
// outstanding tool/item make an apparently idle-but-active run actionable
// without reading the raw Codex rollout.
func (c *Coordinator) logHeartbeat(r *running, now, last time.Time) {
	c.mu.Lock()
	issue, session := r.issue, r.session
	c.mu.Unlock()
	attrs := append([]any{"issue_id", issue.ID, "issue_identifier", issue.Identifier, "session_id", session.ID}, c.outstandingAttrs(r, now, last)...)
	c.log.Debug("agent heartbeat", attrs...)
}

// outstandingAttrs reports how long since the run last produced any event and
// identifies the single started-but-not-completed operation, if any, without
// ever including tool arguments or output.
func (c *Coordinator) outstandingAttrs(r *running, now, last time.Time) []any {
	c.mu.Lock()
	outstanding := r.outstanding
	c.mu.Unlock()
	attrs := []any{"last_activity_age_ms", now.Sub(last).Milliseconds()}
	if outstanding == nil {
		return attrs
	}
	attrs = append(attrs, "outstanding_item_type", outstanding.ItemType, "outstanding_item_id", outstanding.ItemID, "outstanding_age_ms", now.Sub(outstanding.Since).Milliseconds())
	if outstanding.ToolName != "" {
		attrs = append(attrs, "outstanding_item_name", outstanding.ToolName)
	}
	return attrs
}

func (c *Coordinator) claim(i domain.Issue, s config.Settings) bool {
	c.mu.Lock()
	if c.stopping || c.claimed[i.ID] || !c.capacityAvailableLocked(config.Norm(i.State), s) {
		c.mu.Unlock()
		return false
	}
	c.claimed[i.ID] = true
	c.claimState[i.ID] = config.Norm(i.State)
	// A claimed issue is being actively worked; any prior handoff memory is
	// stale (the poll loop already reported an external revert before this).
	delete(c.handoffs, i.ID)
	c.mu.Unlock()
	c.log.Debug("issue claimed", "issue_id", i.ID, "issue_identifier", i.Identifier, "state", config.Norm(i.State))
	return true
}

// launch reserves capacity before starting asynchronous preparation. This
// closes the gap where several goroutines could otherwise all observe room
// before any of them had inserted a backend session into running.
func (c *Coordinator) launch(parent context.Context, i domain.Issue, attempt int) bool {
	s := c.settings()
	c.mu.Lock()
	if c.stopping {
		c.mu.Unlock()
		c.release(i.ID)
		return false
	}
	if !c.reserveLocked(i, s) {
		c.mu.Unlock()
		return false
	}
	c.wg.Add(1)
	c.mu.Unlock()
	go func() {
		defer c.wg.Done()
		defer c.unreserve(i.ID)
		ctx, cancel := context.WithCancel(parent)
		defer cancel()
		c.log.Debug("workspace preparation started", "issue_id", i.ID, "issue_identifier", i.Identifier, "attempt", attempt)
		ws, err := c.workspaces.Prepare(ctx, i)
		if err != nil {
			c.unreserve(i.ID)
			c.finishFailure(parent, i, attempt, "workspace_prepare", err)
			return
		}
		c.log.Debug("workspace prepared", "issue_id", i.ID, "issue_identifier", i.Identifier, "attempt", attempt, "created", ws.CreatedNow)
		if err = c.workspaces.BeforeRun(ctx, ws, i); err != nil {
			c.workspaces.AfterRun(context.Background(), ws, i)
			c.unreserve(i.ID)
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
		prompt, err := render(s, i, attempt, launch.Backend)
		if err != nil {
			c.workspaces.AfterRun(context.Background(), ws, i)
			c.unreserve(i.ID)
			c.finishFailure(parent, i, attempt, "prompt_render", err)
			return
		}
		c.log.Debug("agent launch requested", "issue_id", i.ID, "issue_identifier", i.Identifier, "attempt", attempt, "agent_backend", launch.Backend)
		session, events, err := c.agent.Start(ctx, domain.AgentRequest{Issue: i, Backend: launch.Backend, Model: launch.Model, Workspace: ws.Path, GitMetadataRoots: ws.GitMetadataRoots, Prompt: prompt, Command: launch.Command, ApprovalPolicy: launch.ApprovalPolicy, ThreadSandbox: launch.ThreadSandbox, TurnSandboxPolicy: launch.TurnSandboxPolicy, TurnTimeout: launch.TurnTimeout, ReadTimeout: launch.ReadTimeout, StartTimeout: launch.StartTimeout})
		if err != nil {
			c.workspaces.AfterRun(context.Background(), ws, i)
			c.unreserve(i.ID)
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
		c.running[i.ID] = r
		c.mu.Unlock()
		ended, _, consumeErr := c.runTurns(ctx, r, events, s)
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
				c.unreserve(i.ID)
				c.finishFailure(parent, i, attempt, "stalled", context.DeadlineExceeded)
				return
			}
			c.release(i.ID)
			return
		}
		c.unreserve(i.ID)
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

func (c *Coordinator) runTurns(ctx context.Context, r *running, events <-chan domain.Event, settings config.Settings) (ended bool, current domain.Issue, err error) {
	c.mu.Lock()
	current = r.issue
	c.mu.Unlock()
	for {
		completed, err := c.consume(ctx, r, events)
		if !completed {
			return false, current, err
		}
		fresh, err := c.tracker.GetIssues(ctx, []string{current.ID})
		if err != nil {
			return false, current, issueRefreshError{err: err}
		}
		if len(fresh) != 1 || fresh[0].ID != current.ID {
			return true, current, nil
		}
		current = fresh[0]
		c.refreshRunIssue(r, current)
		c.mu.Lock()
		turnCount := r.run.TurnCount
		c.mu.Unlock()
		if !eligible(current, settings) {
			if issueTerminal(current, settings) {
				c.cleanupWorkspaceAtRunEnd(ctx, r, current)
			}
			return true, current, nil
		}
		c.mu.Lock()
		landingResolved := r.landingResolved
		c.mu.Unlock()
		if landingResolved {
			// Landing already merged the pull request and reconciled the issue.
			// End the run here even when this refresh still reports the pre-merge
			// state, so no later turn or landing tool call is possible (PMR-78).
			// This issue will never be polled or retried again, so the workspace
			// must be released here exactly as on the terminal-state path above.
			c.cleanupWorkspaceAtRunEnd(ctx, r, current)
			return true, current, nil
		}
		if turnCount >= settings.Agent.MaxTurns {
			return false, current, turnLimitError{limit: settings.Agent.MaxTurns}
		}
		if err := c.waitForContinuation(ctx); err != nil {
			return false, current, err
		}
		guidance := continuationGuidance(turnCount+1, settings.Agent.MaxTurns)
		events, err = c.agent.Continue(ctx, r.session, guidance)
		if err != nil {
			return false, current, sessionContinueError{err: err}
		}
		c.mu.Lock()
		r.run.TurnCount++
		c.mu.Unlock()
	}
}

func isLandingWait(err error) bool {
	_, ok := landingWait(err)
	return ok
}

func agentFailureReason(err error) string {
	var blocked blockedError
	if errors.As(err, &blocked) {
		return "agent_blocked"
	}
	var limited rateLimitedError
	if errors.As(err, &limited) {
		return "agent_rate_limited"
	}
	var exhausted turnLimitError
	if errors.As(err, &exhausted) {
		return "turn_limit_exhausted"
	}
	if errors.Is(err, errStreamClosed) {
		return "stream_closed"
	}
	var refresh issueRefreshError
	if errors.As(err, &refresh) {
		return "issue_refresh"
	}
	var cont sessionContinueError
	if errors.As(err, &cont) {
		return "session_continue"
	}
	// Anything else reaching here is domain.EventFailed carrying e.Message
	// verbatim (consume, "agent failed: %s") -- genuine model or provider
	// text, the one case this reason is now reserved for.
	return "agent_event"
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

// runBackend records the runtime that actually created the session. The router
// stamps it, so a reload between resolving the launch and starting the session
// cannot leave the run tagged with a backend it never ran on. The resolved
// launch is only a fallback, for a backend that does not stamp sessions.
func runBackend(session domain.AgentSession, launch config.AgentLaunch) string {
	if backend := strings.TrimSpace(session.Backend); backend != "" {
		return backend
	}
	return launch.Backend
}

func continuationGuidance(turn, maxTurns int) string {
	// The upstream workflow schema configures the turn bound but deliberately
	// has no continuation-prompt field. Generate its prescribed guidance here
	// so continuation turns do not resend the repository task template.
	//
	// This text is backend-neutral on purpose, and neutral in both directions.
	// "workpad" and "thread" were Codex vocabulary and a Claude turn has neither;
	// a note about tool-name prefixes is Claude vocabulary and a Codex turn has no
	// prefix, so it does not belong here either. One shared prompt cannot carry
	// either transport's wording, and the rule that decides what may live here is
	// what rules it out: anything that varies from turn to turn belongs in this
	// function, and everything else belongs in the initial prompt.
	//
	// The tool naming is emphatically the second kind. config.DeliveryInstructions
	// renders it, every fresh dispatch renders that, and a resume replays it --
	// and it is safe there because the advertised set is frozen when the session's
	// registry is built (capability.landAdvertised reads Issue.State once, at
	// Build time) and no later turn can change it. A landing_waiting redispatch is
	// not a continuation: it goes through scheduleRetry/retryLanding to a fresh
	// Start and therefore a fresh render. So there is nothing for a continuation
	// turn to correct, and adding a note anyway only leaked one backend's
	// vocabulary into the other's prompt.
	return fmt.Sprintf(`Continuation guidance:

- The previous agent turn completed normally, but the tracker work item is still in an active state.
- This is continuation turn #%d of %d for the current agent run.
- Resume from the current workspace and session state instead of restarting from scratch.
- The original task instructions and prior turn context are already present in this session, so do not restate them before acting.
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
	case isLandingWait(err):
		r.run.Status = domain.RunWaiting
	case agentFailureReason(err) == "agent_blocked", agentFailureReason(err) == "turn_limit_exhausted":
		r.run.Status = domain.RunBlocked
	case err != nil && strings.Contains(strings.ToLower(err.Error()), "timeout"):
		r.run.Status = domain.RunTimedOut
	default:
		r.run.Status = domain.RunFailed
	}
	run := r.run
	c.mu.Unlock()
	c.log.Info("agent logical run finished", "issue_id", run.IssueID, "issue_identifier", run.IssueIdentifier, "session_id", run.SessionID, "status", string(run.Status), "attempt", run.Attempt, "turn_count", run.TurnCount)
}

func (c *Coordinator) consume(ctx context.Context, r *running, events <-chan domain.Event) (bool, error) {
	for {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case e, ok := <-events:
			if !ok {
				return false, errStreamClosed
			}
			at := e.At
			if at.IsZero() {
				at = c.clock.Now()
			}
			e.At = at
			c.mu.Lock()
			r.last = at
			c.mu.Unlock()
			c.logEvent(r, e)
			switch e.Kind {
			case domain.EventBlocked:
				return false, blockedError{category: blockerCategory(e.Message)}
			case domain.EventRateLimited:
				return false, rateLimitedError{retryAfter: e.RetryAfter}
			case domain.EventFailed:
				return false, fmt.Errorf("agent failed: %s", e.Message)
			case domain.EventLandingWaiting:
				// A landing wait ends the run without another turn; the
				// coordinator owns the delayed landing retry from here (PMR-78).
				return false, landingWaitError{reason: observability.Text(e.Message)}
			case domain.EventLandingResolved, domain.EventCompleted:
				if e.Kind == domain.EventLandingResolved {
					c.mu.Lock()
					r.landingResolved = true
					c.mu.Unlock()
				}
				// Reconciliation and event delivery can race. An event that
				// arrives after reconciliation has canceled this run must never
				// turn into a successful terminal outcome.
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
	c.mu.Lock()
	issue := r.issue
	c.mu.Unlock()
	sessionID := event.SessionID
	if sessionID == "" {
		sessionID = r.session.ID
	}
	attrs := []any{"issue_id", issue.ID, "issue_identifier", issue.Identifier, "session_id", sessionID, "event", event.Kind}
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
		if len(rateLimit) == 0 {
			// An empty snapshot carries no operator-actionable information; the
			// app-server sends these often enough that logging each one would
			// only add noise to the default log.
			return
		}
		attrs = append(attrs, "rate_limit", rateLimit)
		c.log.Info("agent rate limit", attrs...)
	case domain.EventDiagnostic:
		if event.RateLimitStatus != "" {
			// A non-allowed, non-terminal rate-limit status (today, Claude's
			// "allowed_warning") is a decoded protocol event, not undecodable
			// child output, so it gets its own name and status attribute
			// rather than "agent stderr" (PMR-126).
			attrs = append(attrs, "status", observability.Text(event.RateLimitStatus))
			c.log.Warn("agent rate limit", attrs...)
			return
		}
		attrs = append(attrs, "stderr", observability.Text(event.Message))
		c.log.Warn("agent stderr", attrs...)
	case domain.EventRateLimited:
		attrs = append(attrs, "status", observability.Text(event.RateLimitStatus), "retry_after_ms", event.RetryAfter.Milliseconds())
		c.log.Warn("agent rate limit rejected", attrs...)
	case domain.EventLandingWaiting:
		// The reason is the github package's own fixed waiting string, so it is
		// safe to log; it is still redacted defensively like every other text.
		attrs = append(attrs, "operation", "landing_waiting", "reason", observability.Text(event.Message))
		c.log.Info("agent landing waiting", attrs...)
	case domain.EventLandingResolved:
		attrs = append(attrs, "operation", "landing_resolved")
		c.log.Info("agent landing resolved", attrs...)
	case domain.EventItem:
		c.logItemEvent(r, event, attrs)
	default:
		// Messages can contain model output, tool arguments, prompt excerpts, or
		// provider data. The event type is sufficient for ordinary operation.
		// This is opt-in debug detail: coalesce identical repeats so a chatty
		// protocol stream cannot flood even the debug log, and keep it off the
		// default info level entirely.
		c.mu.Lock()
		repeat := event.Kind == r.lastGenericKind && event.Message == r.lastGenericMessage
		if repeat {
			r.genericRepeat++
		} else {
			r.lastGenericKind = event.Kind
			r.lastGenericMessage = event.Message
			r.genericRepeat = 0
		}
		count := r.genericRepeat
		c.mu.Unlock()
		if repeat {
			if count%20 != 0 {
				return
			}
			attrs = append(attrs, "repeated", count+1)
		}
		c.log.Debug("agent event", attrs...)
	}
}

// logItemEvent classifies a safe tool/item lifecycle transition and tracks
// the run's single outstanding operation so heartbeat and stall records can
// report what Codex is waiting on. It is opt-in debug detail: turn-level
// start/completion already appears at the default level.
func (c *Coordinator) logItemEvent(r *running, event domain.Event, attrs []any) {
	c.mu.Lock()
	if event.Outcome == domain.ItemStarted {
		r.outstanding = &outstandingOp{ItemID: event.ItemID, ItemType: event.ItemType, ToolName: event.ToolName, Since: event.At}
	} else if r.outstanding != nil && r.outstanding.ItemID == event.ItemID {
		r.outstanding = nil
	}
	c.mu.Unlock()
	attrs = append(attrs, "item_type", observability.Text(event.ItemType), "item_id", observability.Text(event.ItemID), "outcome", observability.Text(event.Outcome))
	if event.ToolName != "" {
		attrs = append(attrs, "item_name", observability.Text(event.ToolName))
	}
	if event.DurationMs > 0 {
		attrs = append(attrs, "duration_ms", event.DurationMs)
	}
	c.log.Debug("agent item event", attrs...)
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
	issue := r.issue
	usage := r.run.Usage
	rateLimit := copyRateLimit(r.rateLimit)
	started := r.run.StartedAt
	attempt := r.run.Attempt
	turnCount := r.run.TurnCount
	c.mu.Unlock()
	attrs := []any{"issue_id", issue.ID, "issue_identifier", issue.Identifier, "session_id", r.session.ID, "attempt", attempt, "turn_count", turnCount, "runtime_ms", c.clock.Now().Sub(started).Milliseconds(), "input_tokens", usage.InputTokens, "output_tokens", usage.OutputTokens, "total_tokens", usage.TotalTokens}
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

// workspaceCleanupTimeout bounds cleanupWorkspaceAtRunEnd's detached context
// (PMR-130), so a wedged git invocation cannot hold its goroutine forever once
// it no longer inherits the run's own cancellation.
const workspaceCleanupTimeout = 15 * time.Second

// cleanupWorkspace releases what Symphony still holds for an issue it has just
// decided is finished. It is the single place that decision is made, so it is
// also where the host's IssueForgetter is told (PMR-112) -- an issue that will
// never be dispatched again needs no linked pull request polled on its behalf.
//
// It then removes the issue's workspace and reports the lifecycle outcome an
// operator needs to know without reading the workspace package's own error
// text: a clean removal, a removal that discarded local commits Symphony
// verified as merged, or why the workspace was kept (uncommitted/untracked
// changes, or local commits ahead of the recorded base revision that a human
// should review before it is discarded). Call sites that run on a context
// reconciliation already holds live -- the poll loop's own stopTerminal branch
// and a redispatch retry's refresh -- are authoritative: their failure is
// always reported at WARN.
func (c *Coordinator) cleanupWorkspace(ctx context.Context, issue domain.Issue) {
	c.finalizeWorkspace(ctx, issue, nil)
}

// cleanupWorkspaceAtRunEnd releases a workspace from inside runTurns, at the
// moment a run decides its own issue is done (landing resolved, or the issue
// went terminal between turns). That decision races the poll loop's own
// reconcile pass, which can concurrently reach the same conclusion about the
// same issue and call stopRun -- cancelling the very context runTurns was
// about to clean up on and turning a healthy landing into a killed git
// subprocess (PMR-130). So this attempt runs on a context detached from the
// run's own cancellation (bounded by workspaceCleanupTimeout instead), and if
// r.stopped is stopTerminal once it finishes, reconcile's own stopTerminal
// branch holds -- or is about to hold -- an authoritative attempt on its own
// live context right after stopRun returns; this attempt's failure is then a
// duplicate, not a call to action, and is reported below WARN. Any other stop
// reason (ineligible, stalled) does not carry that guarantee -- reconcile
// only re-cleans up on stopTerminal -- so a failure raced by one of those must
// still reach WARN, or a genuine leak is swallowed as a duplicate that never
// actually gets retried.
func (c *Coordinator) cleanupWorkspaceAtRunEnd(ctx context.Context, r *running, issue domain.Issue) {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), workspaceCleanupTimeout)
	defer cancel()
	c.finalizeWorkspace(cctx, issue, r)
}

// finalizeWorkspace is cleanupWorkspace's and cleanupWorkspaceAtRunEnd's
// shared implementation. r is nil for an authoritative caller and set for the
// run-end caller whose failure reporting depends on whether reconciliation
// raced it.
func (c *Coordinator) finalizeWorkspace(ctx context.Context, issue domain.Issue, r *running) {
	if c.forget != nil {
		c.forget.Forget(issue.ID)
	}
	outcome, err := c.workspaces.Cleanup(ctx, issue)
	status := cleanupStatus(outcome, err)
	attrs := []any{"issue_id", issue.ID, "issue_identifier", issue.Identifier, "status", status}
	if err == nil {
		c.log.Info("workspace cleanup", attrs...)
		return
	}
	attrs = append(attrs, "error", err)
	if r != nil {
		c.mu.Lock()
		superseded := r.stopped == stopTerminal
		c.mu.Unlock()
		if superseded {
			c.log.Info("workspace cleanup", attrs...)
			return
		}
	}
	c.log.Warn("workspace cleanup", attrs...)
}

// cleanupStatus classifies a Cleanup result into the fixed
// clean/landed/dirty/committed/blocked/failed vocabulary the workspace
// package's own outcome and refusal messages already describe. It only ever
// reports a workspace-owned outcome constant or matches fixed, secret-free
// substrings the workspace package controls, never issue or workspace
// content.
//
// blocked is reserved for a verified refusal: Cleanup inspected the workspace
// and is declining to discard it, and every such refusal names itself with
// "refusing" (dirty and committed are just its two classified cases). Any
// other failure -- Cleanup could not even reach a refusal, for example a
// killed subprocess or an unreadable path -- is failed instead, so an
// operator can tell "your work is safe but unmerged" from "we could not run
// git" and only walk to a terminal for the former.
func cleanupStatus(outcome domain.CleanupOutcome, err error) string {
	if err == nil {
		if outcome == domain.CleanupLanded {
			return string(domain.CleanupLanded)
		}
		return string(domain.CleanupClean)
	}
	switch msg := err.Error(); {
	case strings.Contains(msg, "uncommitted or untracked changes"):
		return "dirty"
	case strings.Contains(msg, "differs from recorded base commit"):
		return "committed"
	case strings.Contains(msg, "refusing"):
		return "blocked"
	default:
		return "failed"
	}
}

func (c *Coordinator) finishFailure(ctx context.Context, i domain.Issue, attempt int, reason string, err error) {
	if ctx.Err() != nil {
		c.log.Info("agent run cancelled", "issue_id", i.ID, "issue_identifier", i.Identifier, "reason", cancellationReason("", ctx))
		c.release(i.ID)
		return
	}
	s := c.settings()
	next := attempt + 1
	attrs := []any{"issue_id", i.ID, "issue_identifier", i.Identifier, "reason", reason, "attempt", next}
	var blocked blockedError
	if errors.As(err, &blocked) {
		attrs = append(attrs, "blocker", blocked.category)
	}
	// The error is attached for every reason, including prompt_render (a Go
	// template error over repository-owned WORKFLOW.md content) and
	// agent_event (which, after PMR-115, is reserved for domain.EventFailed's
	// verbatim model/provider text). observability.safeAttr already routes an
	// "error"-valued attribute through observability.Text for every log call,
	// masking credential-shaped text and truncating it, so excluding a reason
	// here bought no additional safety -- it only discarded the three
	// host-generated diagnostics (stream_closed, issue_refresh,
	// session_continue) this issue exists to stop discarding.
	if err != nil {
		attrs = append(attrs, "error", err)
	}
	if c.attemptsExhausted(i, retryAgent, reason, next, s, attrs) {
		return
	}
	c.log.Warn("agent run retry scheduled", attrs...)
	delay := backoff(next, s.Agent.MaxRetryBackoff)
	var limited rateLimitedError
	if errors.As(err, &limited) {
		// A backend-reported reset time always wins over the ordinary
		// attempt-keyed ladder: retrying against a limit already known to be
		// closed is wasted regardless of how many attempts have accumulated,
		// and the ladder's own cap (max_retry_backoff_ms) is far too short
		// for a multi-hour quota window (PMR-131).
		delay = rateLimitRetryDelay(limited.retryAfter, s.Agent.MaxRetryBackoff)
	}
	c.scheduleRetry(ctx, i, domain.Workspace{}, next, retryAgent, reason, delay)
}

// rateLimitRetryDelay bounds the wait before a retryAgent episode ended by a
// Claude quota rejection is retried. It defers entirely to the backend's own
// reset time when one was reported: a five-hour window's remainder is
// routinely far longer than agent.max_retry_backoff_ms, and honoring the
// ordinary ladder instead is the bug this exists to fix (PMR-131: 203
// launches rejected across six issues at a five-minute cadence, against a
// limit with hours left to run). When Claude reports no reset at all, the
// floor is ten times the ordinary ceiling -- still bounded, but far enough
// above max_retry_backoff_ms that this path can never collapse back onto the
// ladder it exists to replace.
func rateLimitRetryDelay(retryAfter, maxRetryBackoff time.Duration) time.Duration {
	if retryAfter > 0 {
		return retryAfter
	}
	return 10 * maxRetryBackoff
}

// systemicFailureReasons are the reasons -- from agentFailureReason or raised
// directly at a call site -- that name a boundary the coordinator's own
// host, or a shared backend, crosses -- never evidence that this issue's
// work is unworkable -- so none of them arms attemptsExhausted's ceiling:
//
//   - "agent_event": the ceiling's original exemption (PMR-111). It is
//     agentFailureReason's fallback for domain.EventFailed carrying model
//     or provider text the coordinator cannot itself name.
//   - "agent_rate_limited": a Claude quota rejection (PMR-131), named by its
//     own reason rather than falling through to "agent_event" as it did
//     before this reason existed. Observed live: 203 such rejections across
//     six healthy issues in one 2.5-hour window, an account-wide condition
//     that would have abandoned every one of them under the ceiling -- and
//     finishFailure schedules its retry from the backend's own reset time
//     rather than the ordinary ladder (see rateLimitRetryDelay), so the
//     exemption here is what stops the ceiling from cutting that wait short
//     with an abandonment instead.
//   - "issue_refresh": a tracker error from runTurns' post-turn GetIssues
//     refresh (PMR-115, confirmed live: a 30s Linear client timeout
//     following a turn the agent completed successfully). That is Linear
//     infrastructure, not this issue: with this codebase's default
//     max_attempts=5 and its escalating backoff, a two-and-a-half-minute
//     Linear outage would otherwise abandon every issue currently running,
//     since they all fail the same way at the same time.
//   - "retry_refresh": the same tracker.GetIssues failure, observed instead
//     at runRetry's pre-dispatch refresh (PMR-142). It is the same Linear
//     infrastructure as "issue_refresh", just crossed at a different
//     moment -- the moment an issue is waiting to redispatch rather than
//     the moment one just finished a turn -- and a sustained outage drives
//     every retrying issue through exactly this site, raising `attempt`
//     each time. Governing it from the same map as "issue_refresh" is
//     deliberate: the two must not be able to drift into opposite verdicts
//     on identical evidence again.
//   - "session_continue": Symphony's own backend adapter (agent.Continue)
//     failing to resume a session. A broken `claude` binary or lapsed
//     backend auth fails every running issue's next turn the same way, at
//     the same time -- the same account-wide shape as the quota rejection
//     above, just raised by Symphony's own code instead of the model's.
//   - "stream_closed": the host's own event plumbing failing to deliver a
//     verdict (see errStreamClosed). By construction every backend emits
//     a terminal event before its channel closes, so this can never be a
//     repository- or issue-specific outcome -- only ever a host bug, and a
//     host bug affects whichever issues happen to be running when it
//     fires, not the one that happened to surface it first.
//
// Once PMR-128 gives tracker errors a real Retryable signal, "issue_refresh"
// and "retry_refresh" can narrow together from a blanket exemption to "arm
// the ceiling only when the wrapped error is not Retryable" -- but that is
// future work, not a precondition for this one.
//
// Of agentFailureReason's outputs, only "turn_limit_exhausted" and
// "agent_blocked" are left to arm the ceiling -- each is evidence about
// *this* issue's run (it exhausted its turns; its own agent reported a
// blocker), the same way "prompt_render" is evidence about this issue's own
// WORKFLOW.md template. None of the six reasons above says anything about
// the issue at all; each says something about the shared environment
// dispatching it. This map is not exhaustive for finishFailure's other
// direct reasons ("workspace_prepare", "before_run", "prompt_render",
// "session_start", "stalled") -- those are deliberately absent because they
// are issue-attributable, not systemic.
var systemicFailureReasons = map[string]bool{
	"agent_event":        true,
	"agent_rate_limited": true,
	"issue_refresh":      true,
	"retry_refresh":      true,
	"session_continue":   true,
	"stream_closed":      true,
}

// attemptsExhausted reports whether next has reached agent.max_attempts for a
// retryAgent episode, abandoning the dispatch (and releasing its claim) if so.
// Only retryAgent consumes the ceiling, and only on a genuine, classified
// dispatch failure that is not systemic (see systemicFailureReasons): a
// retryLanding redispatch — whether from finishLandingWait or either
// escalation in runRetry — never raises its attempt counter, and neither does
// a retryAgent episode that merely lost an orchestrator slot race (see
// agentSlotRetryDelay). config rejects a non-positive max_attempts, so the
// MaxAttempts <= 0 case only covers a hand-built Settings, which keeps the
// pre-PMR-111 unbounded ladder rather than having a zero ceiling abandon
// every first failure.
func (c *Coordinator) attemptsExhausted(i domain.Issue, kind retryKind, reason string, next int, s config.Settings, attrs []any) bool {
	if kind != retryAgent || systemicFailureReasons[reason] || s.Agent.MaxAttempts <= 0 || next < s.Agent.MaxAttempts {
		return false
	}
	c.abandonDispatch(i, s.Agent.MaxAttempts, attrs)
	return true
}

// abandonDispatch ends one dispatch episode that reached agent.max_attempts
// (PMR-111). The escalated attempt counter is the ceiling's unit: a failure at
// attempt N ends the (N+1)th launch of the episode, so a boundary that fails
// every time dispatches exactly max_attempts times and arms no further retry.
// Of runRetry's two host-side escalations for a retryAgent episode, only the
// failed retry refresh raises that counter and routes it through
// attemptsExhausted at all -- but "retry_refresh" is itself one of
// systemicFailureReasons' exemptions (PMR-142), so that check never actually
// abandons the episode; the counter keeps climbing the ordinary backoff
// ladder instead, the same way a classified "issue_refresh" failure does.
// The other escalation, a contended orchestrator slot, never reaches here at
// all: it is capacity contention rather than a failure, so it keeps the
// attempt fixed (see agentSlotRetryDelay), the same way a retryLanding
// episode's own escalations do (see runRetry), since the attempt feeds the
// rendered prompt and neither a wait nor contention should inflate it.
//
// What it does NOT do is as deliberate as what it does:
//
//   - It does not touch the tracker. The issue stays where a human left it and
//     stays re-poll-able, so a later poll starts a fresh, equally bounded
//     episode rather than an in-process loop nothing can kill. That makes this
//     record load-bearing: it is the only trace of the give-up, and it is at
//     error level because abandonment bounds one episode rather than
//     quarantining the issue — with nobody acting on it, new episodes keep
//     starting at the poll interval.
//   - It does not comment either, and that is the same decision rather than an
//     omission. The coordinator holds only domain.Tracker (candidates, refresh,
//     and the one start edge), and an abandoned dispatch has often failed before
//     any session existed (workspace_prepare, before_run, prompt_render), so
//     there is no HandoffSession whose LandComment shape could be reused —
//     only a new host tracker-write path, for a record that would repeat on
//     every episode.
//   - It does not apply to a landing wait. finishLandingWait keeps its own
//     unbounded redispatch on purpose (see landingRetryDelay): a wait is not an
//     agent failure, does not escalate the attempt, and never reaches here.
func (c *Coordinator) abandonDispatch(i domain.Issue, maxAttempts int, attrs []any) {
	attrs = append(attrs, "operation", observability.OperationDispatchAbandoned, "max_attempts", maxAttempts)
	c.log.Error("dispatch abandoned after max attempts", attrs...)
	c.release(i.ID)
}

// finishLandingWait ends a run whose landing capability reported a
// non-terminal wait. A wait is not an agent failure: the same attempt is
// redispatched after landingRetryDelay, so it consumes neither Codex turns nor
// the failure backoff escalation, and while the timer waits it holds only the
// duplicate-prevention claim — never an orchestrator slot (PMR-78).
func (c *Coordinator) finishLandingWait(ctx context.Context, i domain.Issue, attempt int, reason string) {
	if ctx.Err() != nil {
		c.log.Info("agent run cancelled", "issue_id", i.ID, "issue_identifier", i.Identifier, "reason", cancellationReason("", ctx))
		c.release(i.ID)
		return
	}
	s := c.settings()
	c.mu.Lock()
	c.landingWaits[i.ID]++
	waits := c.landingWaits[i.ID]
	escalate := landingWaitEscalated(s, waits) && !c.landingEscalated[i.ID]
	if escalate {
		c.landingEscalated[i.ID] = true
	}
	c.mu.Unlock()
	delay := landingRetryDelay(s, waits)
	if !c.scheduleRetry(ctx, i, domain.Workspace{}, attempt, retryLanding, "landing_waiting", delay) {
		return
	}
	attrs := []any{"operation", "landing_waiting", "issue_id", i.ID, "issue_identifier", i.Identifier, "reason", reason, "attempt", attempt, "wait_attempt", waits, "delay_ms", delay.Milliseconds()}
	if escalate {
		c.log.Warn("landing wait retry scheduled", attrs...)
		return
	}
	c.log.Info("landing wait retry scheduled", attrs...)
}

// landingRetryDelay bounds the wait before a landing is redispatched. Its floor
// is the configured GitHub poll interval -- the cadence at which the GitHub
// state being waited on can change -- and it escalates with the number of
// consecutive waits toward agent.max_retry_backoff_ms, so a gate that never
// settles cannot respawn a session every poll interval forever. The coordinator
// deliberately does not itself give up on a stuck landing: returning the issue
// to review is the landing capability's own bounded, commented authority
// (github_land_pr's merge_state -> In Review fallback), and a Merging issue that
// stops making progress stays visible on the board with a climbing wait_attempt.
// The floor is never undercut, so a small backoff ceiling cannot turn landing
// into a tighter GitHub poll than the configured interval.
func landingRetryDelay(s config.Settings, waits int) time.Duration {
	floor := s.GitHub.PollInterval
	if floor <= 0 {
		floor = s.Polling.Interval
	}
	if floor <= 0 {
		// The same fallback the GitHub linked-PR poll loop uses when no interval
		// is configured (see internal/github.Manager.Run).
		floor = defaultPollRetryDelay
	}
	// backoff already caps its escalation at the ceiling, and the poll floor
	// always wins: a small max_retry_backoff_ms must never make landing poll
	// GitHub faster than its configured interval.
	delay := floor
	if escalated := backoff(waits, s.Agent.MaxRetryBackoff); escalated > delay {
		delay = escalated
	}
	return delay
}

// landingWaitEscalated reports whether waits has reached the point where
// backoff's own escalation has saturated at agent.max_retry_backoff_ms: the
// ceiling landingRetryDelay's ladder climbs toward and then holds at. Below
// that point a wait's Info-level log is enough -- the redispatch delay is
// still climbing on its own. At and above it a wait that is still recurring
// is no longer distinguishable, on the timeline alone, from a landing that
// will never settle (PMR-116), so finishLandingWait raises the log level to
// Warn the first time this turns true for the issue and leaves it there.
func landingWaitEscalated(s config.Settings, waits int) bool {
	max := s.Agent.MaxRetryBackoff
	return backoff(waits, max) >= max
}

// agentSlotRetryDelay bounds the wait before a retryAgent episode that lost
// the race for an orchestrator slot is retried. Unlike landingRetryDelay it
// does not escalate: a contended slot is no more an agent failure than a
// contended landing slot (see the runRetry branch that calls this), so there
// is no failure ladder to climb and no ceiling for it to threaten -- just the
// poll interval, the cadence at which a fresh admission could actually free
// one up.
func agentSlotRetryDelay(s config.Settings) time.Duration {
	if s.Polling.Interval > 0 {
		return s.Polling.Interval
	}
	return defaultPollRetryDelay
}

// scheduleRetry arms one delayed redispatch and reports whether it did: a
// shutdown or a released claim declines it, so a caller that logs the retry
// separately can stay truthful.
func (c *Coordinator) scheduleRetry(ctx context.Context, i domain.Issue, ws domain.Workspace, attempt int, kind retryKind, reason string, delay time.Duration) bool {
	c.mu.Lock()
	if c.stopping || !c.claimed[i.ID] {
		c.mu.Unlock()
		return false
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
	return true
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
	s := c.settings()
	fresh, err := c.tracker.GetIssues(ctx, []string{id})
	if err != nil {
		c.logIssueRefreshFailure(ctx, "retry issue refresh failed", "issue_id", id, "reason", retry.reason, "error", err)
		if retry.kind == retryLanding {
			// A stale tracker read is no more an agent failure than the wait
			// itself: keep the attempt (it feeds the rendered prompt) and the
			// landing cadence, exactly like the slot-contention branch below.
			c.mu.Lock()
			waits := c.landingWaits[id]
			c.mu.Unlock()
			c.scheduleRetry(ctx, retry.issue, retry.workspace, retry.attempt, retryLanding, "retry_refresh", landingRetryDelay(s, waits))
			return
		}
		attempt := retry.attempt + 1
		attrs := []any{"issue_id", retry.issue.ID, "issue_identifier", retry.issue.Identifier, "reason", "retry_refresh", "attempt", attempt}
		if c.attemptsExhausted(retry.issue, retry.kind, "retry_refresh", attempt, s, attrs) {
			return
		}
		c.scheduleRetry(ctx, retry.issue, retry.workspace, attempt, retry.kind, "retry_refresh", backoff(attempt, s.Agent.MaxRetryBackoff))
		return
	}
	if len(fresh) != 1 || fresh[0].ID != id {
		c.release(id)
		return
	}
	issue := fresh[0]
	if !eligible(issue, s) {
		if issueTerminal(issue, s) {
			c.cleanupWorkspace(ctx, issue)
		}
		c.release(id)
		return
	}
	if c.launch(ctx, issue, retry.attempt) {
		return
	}
	// The retry still owns its claim, but another admitted run used the slot
	// after we refreshed it. Keep that duplicate-prevention claim and retry on
	// a fixed cadence instead of dropping the work: losing a slot race is
	// capacity contention, not a failure, for either retry kind.
	if retry.kind == retryLanding {
		// A contended landing slot is no more an agent failure than the wait
		// itself: keep the attempt (it feeds the rendered prompt) and the
		// landing cadence, so a queued landing never polls GitHub faster than
		// the configured interval (PMR-78).
		c.mu.Lock()
		waits := c.landingWaits[id]
		c.mu.Unlock()
		c.scheduleRetry(ctx, issue, retry.workspace, retry.attempt, retryLanding, "landing_slot_unavailable", landingRetryDelay(s, waits))
		return
	}
	// A contended orchestrator slot is no more an agent failure than a
	// contended landing slot is (see the retryLanding branch above): keep the
	// attempt fixed and retry on agentSlotRetryDelay's fixed cadence instead
	// of attempt+1 and the escalating failure backoff, and do not consume
	// attemptsExhausted's ceiling. A healthy issue that merely loses slot
	// races against fresh poll candidates must keep waiting for capacity
	// indefinitely rather than being abandoned for a failure it never had.
	c.scheduleRetry(ctx, issue, retry.workspace, retry.attempt, retryAgent, "agent_slot_unavailable", agentSlotRetryDelay(s))
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

func (c *Coordinator) release(id string) {
	c.mu.Lock()
	if retry, ok := c.retries[id]; ok && retry.timer != nil {
		retry.timer.Stop()
	}
	delete(c.claimed, id)
	delete(c.claimState, id)
	delete(c.admitted, id)
	delete(c.retries, id)
	delete(c.landingWaits, id)
	delete(c.landingEscalated, id)
	c.mu.Unlock()
}

func (c *Coordinator) unreserve(id string) {
	c.mu.Lock()
	delete(c.admitted, id)
	c.mu.Unlock()
}

func (c *Coordinator) reserveLocked(i domain.Issue, s config.Settings) bool {
	if _, admitted := c.admitted[i.ID]; !c.claimed[i.ID] || admitted || !c.capacityAvailableLocked(config.Norm(i.State), s) {
		return false
	}
	state := config.Norm(i.State)
	c.admitted[i.ID] = state
	c.claimState[i.ID] = state
	return true
}

func (c *Coordinator) capacityAvailableLocked(state string, s config.Settings) bool {
	if len(c.admitted) >= s.Agent.MaxConcurrent {
		return false
	}
	limit, ok := s.Agent.ByState[state]
	if !ok {
		return true
	}
	count := 0
	for _, admittedState := range c.admitted {
		if admittedState == state {
			count++
		}
	}
	return count < limit
}

func (c *Coordinator) refreshRunIssue(r *running, fresh domain.Issue) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.running[fresh.ID] != r || r.stopped != "" {
		return
	}
	r.issue = fresh
	state := config.Norm(fresh.State)
	c.claimState[fresh.ID] = state
	if _, admitted := c.admitted[fresh.ID]; admitted {
		c.admitted[fresh.ID] = state
	}
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
		if config.Norm(i.State) == config.Norm(x) {
			return true
		}
	}
	return false
}

// issueTerminal reports whether the issue's tracker state is one of the
// configured terminal states -- unrelated to domain.EventKind.Terminal, which
// answers the same question for an agent session's event stream.
func issueTerminal(i domain.Issue, s config.Settings) bool {
	for _, x := range s.Tracker.TerminalStates {
		if config.Norm(i.State) == config.Norm(x) {
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
		have[config.Norm(x)] = true
	}
	for _, x := range s.Tracker.RequiredLabels {
		if x == "" || !have[x] {
			return false
		}
	}
	return true
}
func eligible(i domain.Issue, s config.Settings) bool {
	return i.ID != "" && i.Identifier != "" && i.Title != "" && active(i, s) && !issueTerminal(i, s) && routable(i, s)
}
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
func render(s config.Settings, i domain.Issue, attempt int, backend string) (string, error) {
	prompt, err := s.Render(i, attempt)
	if err != nil {
		return "", err
	}
	return prompt + "\n\n" + s.DeliveryInstructions(backend), nil
}
