package coordinator

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pmrrasmussen/symphony/internal/domain"
	"github.com/pmrrasmussen/symphony/internal/observability"
)

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

// outstandingOp is the coordinator's view of a started-but-not-yet-completed
// app-server item or dynamic tool call. It never stores anything derived from
// tool arguments, command bodies, or outputs.
type outstandingOp struct {
	ItemID, ItemType, ToolName string
	Since                      time.Time
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
		usage := c.updateUsage(r, event.Usage, event.UsageAuthoritative)
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
			attrs = append(attrs, "operation", observability.OperationRateLimit, "status", observability.Text(event.RateLimitStatus))
			c.log.Warn("agent rate limit", attrs...)
			return
		}
		attrs = append(attrs, "stderr", observability.Text(event.Message))
		c.log.Warn("agent stderr", attrs...)
	case domain.EventRateLimited:
		attrs = append(attrs, "operation", observability.OperationRateLimit, "status", observability.Text(event.RateLimitStatus), "retry_after_ms", event.RetryAfter.Milliseconds())
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

// updateUsage folds a new usage figure into the run's recorded total.
// authoritative distinguishes two contracts this shared code has to serve for
// different backends (PMR-153):
//
//   - Cumulative, monotonically increasing sources (authoritative == false;
//     Codex's thread/tokenUsage/updated notifications, and Claude's mid-turn
//     provisional estimate) are merged with a component-wise maximum, which
//     makes repeated or reordered notifications idempotent without ever
//     reporting a figure lower than one already seen.
//   - Authoritative sources (authoritative == true; today, only Claude's
//     end-of-turn result) replace the recorded total outright. A max() here
//     would be wrong: the CLI's own turn total is the settled truth for that
//     turn even when it is lower than this host's own mid-turn estimate, and
//     merging the two would let an inflated provisional figure latch
//     permanently instead of being corrected.
func (c *Coordinator) updateUsage(r *running, update domain.Usage, authoritative bool) domain.Usage {
	update = normalizedUsage(update)
	c.mu.Lock()
	if authoritative {
		r.run.Usage = update
	} else {
		r.run.Usage.InputTokens = max(r.run.Usage.InputTokens, update.InputTokens)
		r.run.Usage.OutputTokens = max(r.run.Usage.OutputTokens, update.OutputTokens)
		r.run.Usage.TotalTokens = max(r.run.Usage.TotalTokens, update.TotalTokens)
	}
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
