package claude

import (
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/pmrrasmussen/symphony/internal/domain"
	"github.com/pmrrasmussen/symphony/internal/observability"
	"github.com/pmrrasmussen/symphony/internal/procgroup"
)

// stream reads the turn's stdout, normalizes it, and ends the turn's event
// stream. At most one terminal event reaches a consumer, however many goroutines
// had something to report -- the sink's latch, not this loop, is what says so.
func (t *turn) stream(s *session, r domain.AgentRequest, turnNumber int) {
	// The channel is closed by the sink, from inside the sink's own mutex, so
	// there is no ordering here to get wrong and no window in which an emit can
	// find the channel closed. Closing it here instead would reintroduce one.
	defer t.sink.close()
	defer close(t.exited)
	// Retiring this turn's endpoint registration is deferred after both of those
	// so it runs before them. Before the stream closes, because the revocation
	// drains an invocation that may still be running and the terminal event that
	// invocation produces has to reach this stream. Before t.exited closes,
	// because that is what a hard Cancel waits on, so a Cancel that returns has
	// either retired this registration itself or waited for this to.
	//
	// The post-loop code below normally retires it first, because the turn's own
	// fallback outcome may not be chosen until the drain has finished, so this is
	// the backstop for the paths that do not reach that call: a panic, and any
	// early return a future edit adds. Retirement is idempotent and reports its
	// outcome once, so running it twice is a no-op rather than a second
	// diagnostic.
	defer func() { t.retire(s) }()
	// Once this turn is over it must stop being the session's live process, or a
	// later cancellation would signal a process group whose pid has been reaped
	// and possibly recycled.
	defer func() {
		s.mu.Lock()
		if s.running == t {
			s.running = nil
		}
		s.mu.Unlock()
	}()

	emit := t.sink.emit
	pending := map[string]pendingCall{}

	// The turn budget is enforced here rather than by the context, so the
	// timeout is reported as a normalized failure instead of an opaque kill. It
	// is scheduled on the timer seam so a test can elapse it rather than wait it
	// out (see Timer).
	timedOut := make(chan struct{})
	if t.timeout > 0 {
		stop := t.timer.AfterFunc(t.timeout, func() {
			close(timedOut)
			// kill closes the pipes as well, which is what unblocks the read
			// loop below and lets this turn report its own timeout.
			t.kill()
		})
		defer stop()
	}

	stderr := &boundedTail{}
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		stderr.readFrom(t.stderr)
	}()

	lines := newLineReader(t.stdout)
	var initVerified bool
	var readErr error
	// turnUsage sums the Anthropic API usage of every assistant message seen so
	// far in this turn, so a live total can be emitted before the turn's
	// closing result line arrives -- or survive a turn that never gets one,
	// because the timeout above killed it first.
	var turnUsage domain.Usage
	for {
		line, skipped, err := lines.next()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				readErr = err
			}
			break
		}
		if skipped {
			// An over-long line is discarded, but reading continues: stopping
			// would block the child on a full pipe and hang the turn.
			continue
		}
		envelope, ok := decode(line)
		if !ok {
			// Undecodable output is skipped too. It is the child's output, and
			// one bad line must not end a run that is otherwise progressing.
			continue
		}
		switch envelope.Type {
		case "system":
			switch envelope.Subtype {
			case "init":
				var event initEvent
				_ = json.Unmarshal(line, &event)
				if refusal := verifyInit(event, r.Workspace, t.contract); refusal != "" {
					// The policy did not apply. Fail closed rather than run a
					// turn under an unknown boundary.
					// Two refused init lines can arrive in a single read, and
					// killing the child does not discard what is already
					// buffered, so the latch is what keeps this to one failure.
					t.sink.emitTerminal(domain.Event{Kind: domain.EventFailed, At: time.Now(), Message: refusal})
					t.kill()
					continue
				}
				initVerified = true
				emit(domain.Event{
					Kind: domain.EventSessionStarted, At: time.Now(),
					SessionID: s.id, ThreadID: s.id, TurnID: strconv.Itoa(turnNumber),
					PID: t.cmd.Process.Pid,
				})
			case "permission_denied":
				var event permissionDeniedEvent
				_ = json.Unmarshal(line, &event)
				emit(domain.Event{
					Kind: domain.EventItem, At: time.Now(),
					ItemID:   observability.Text(event.ToolUseID),
					ItemType: itemType(event.ToolName),
					ToolName: observability.Text(event.ToolName),
					Outcome:  domain.ItemDeclined,
				})
			}
		case "assistant":
			var message assistantMessage
			_ = json.Unmarshal(line, &message)
			for _, content := range message.Message.Content {
				if content.Type != "tool_use" || content.ID == "" {
					continue
				}
				pending[content.ID] = pendingCall{tool: content.Name, started: time.Now()}
				emit(domain.Event{
					Kind: domain.EventItem, At: time.Now(),
					ItemID:   observability.Text(content.ID),
					ItemType: itemType(content.Name),
					ToolName: observability.Text(content.Name),
					Outcome:  domain.ItemStarted,
				})
			}
			if call := message.Message.Usage.totals(); call != (domain.Usage{}) {
				turnUsage = add(turnUsage, call)
				s.mu.Lock()
				live := add(s.usage, turnUsage)
				s.mu.Unlock()
				// Provisional (UsageAuthoritative left false): this is this
				// host's own running sum of per-API-call deltas, not the CLI's
				// turn total, and it can overshoot the "result" line's figure
				// for the same turn (PMR-153).
				emit(domain.Event{Kind: domain.EventUsage, At: time.Now(), Usage: live})
			}
		case "user":
			var message userMessage
			_ = json.Unmarshal(line, &message)
			for _, content := range message.Message.Content {
				if content.Type != "tool_result" || content.ToolUseID == "" {
					continue
				}
				call, known := pending[content.ToolUseID]
				delete(pending, content.ToolUseID)
				outcome := domain.ItemCompleted
				if content.IsError {
					outcome = domain.ItemFailed
					if toolResultDeniedLoopbackBind(content.Content) {
						// A bind failure and a real test regression otherwise look
						// identical to an operator reading only the item outcome
						// below: both are just a failed Bash call. This is the one
						// case worth a diagnostic of its own.
						emit(domain.Event{Kind: domain.EventDiagnostic, At: time.Now(),
							Message: "sandbox denied a loopback bind; the invoked command could not validate itself in this session"})
					}
				}
				event := domain.Event{
					Kind: domain.EventItem, At: time.Now(),
					ItemID:   observability.Text(content.ToolUseID),
					ItemType: itemType(call.tool),
					ToolName: observability.Text(call.tool),
					Outcome:  outcome,
				}
				if known {
					event.DurationMs = time.Since(call.started).Milliseconds()
				}
				emit(event)
			}
		case "rate_limit_event":
			var event rateLimitEvent
			_ = json.Unmarshal(line, &event)
			// This is reported through RateLimitStatus rather than
			// EventRateLimit on purpose. The scheduler normalizes
			// EventRateLimit payloads through a fixed numeric allowlist
			// (limit, remaining, used, reset_seconds, window_seconds); the
			// CLI's actionable status is a string under a different name, so
			// an EventRateLimit here would be silently discarded and never
			// reach a log.
			// Keep the wire value local: it determines the backend's control
			// flow below, but the scheduler and its logs receive only this fixed
			// host-side vocabulary. A future CLI diagnostic must not widen the
			// log contract by appearing in RateLimitStatus (PMR-150).
			status := firstNonEmpty(event.RateLimitInfo.Status, "unspecified")
			statusCategory := rateLimitStatusCategory(status)
			rateLimitType := firstNonEmpty(event.RateLimitInfo.RateLimitType, "unspecified")
			if status == "rejected" {
				// A rejection is definitive: the account's quota for this
				// window is closed, which says nothing about this issue's
				// work and nothing a retried turn can change before the
				// window reopens. Ending the run here, rather than waiting
				// for the result event the CLI still sends a moment later,
				// stops it from spending any more of a launch already denied
				// (PMR-131).
				t.sink.emitTerminal(domain.Event{
					Kind: domain.EventRateLimited, At: time.Now(),
					Message:         "claude reported a rate limit: " + statusCategory + " (" + observability.Text(rateLimitType) + ")",
					RateLimitStatus: statusCategory,
					RetryAfter:      rateLimitRetryAfter(event.RateLimitInfo.ResetsAt, time.Now()),
				})
				t.kill()
				continue
			}
			if status != "allowed" {
				// "allowed" is the default, healthy state and arrives on
				// effectively every turn; only a non-allowed, non-terminal
				// status (today, "allowed_warning") is worth an operator's
				// attention (PMR-126).
				emit(domain.Event{Kind: domain.EventDiagnostic, At: time.Now(),
					Message:         "claude reported a rate limit: " + statusCategory + " (" + observability.Text(rateLimitType) + ")",
					RateLimitStatus: statusCategory,
				})
			}
		case "result":
			if t.sink.settled() {
				// Something already ended this turn -- a refused init, or a
				// terminal event raised off this loop. Reporting the result too
				// would emit a second terminal event and misreport the reason.
				// This is only a shortcut; emitTerminal below is what enforces it.
				continue
			}
			var event resultEvent
			_ = json.Unmarshal(line, &event)
			// The CLI reports usage per turn while the scheduler keeps a
			// component-wise maximum across a run, so the running total is
			// accumulated here.
			s.mu.Lock()
			s.usage = add(s.usage, event.Usage.totals())
			total := s.usage
			s.mu.Unlock()
			if total != (domain.Usage{}) {
				// Authoritative: this is the CLI's own turn total, added onto the
				// prior settled figure, not this host's mid-turn estimate -- it
				// must replace rather than merge with whatever the running total
				// reported while the turn was still in flight (PMR-153).
				emit(domain.Event{Kind: domain.EventUsage, At: time.Now(), Usage: total, UsageAuthoritative: true})
			}
			for _, denial := range event.PermissionDenials {
				emit(domain.Event{Kind: domain.EventDiagnostic, At: time.Now(),
					Message: "claude denied a tool call: " + observability.Text(denial.ToolName)})
			}
			if event.IsError {
				// is_error is the authoritative failure signal: an
				// authentication failure arrives with subtype "success".
				t.sink.emitTerminal(domain.Event{Kind: domain.EventFailed, At: time.Now(),
					Message: "claude turn failed: " + observability.Text(firstNonEmpty(event.TerminalReason, event.APIErrorStatus, event.StopReason, "unspecified"))})
				continue
			}
			if !initVerified {
				// A turn that never announced its policy is not a turn whose
				// boundary is known.
				t.sink.emitTerminal(domain.Event{Kind: domain.EventFailed, At: time.Now(), Message: "claude session refused: no init event was reported"})
				continue
			}
			t.sink.emitTerminal(domain.Event{Kind: domain.EventCompleted, At: time.Now()})
		}
	}
	<-stderrDone
	waitErr := t.cmd.Wait()
	// Kill the group again: the leader can exit while descendants still hold
	// inherited pipes.
	_ = procgroup.Kill(t.cmd)

	// The endpoint is retired here, before this turn picks a reason of its own,
	// and not left to the defer above. The revocation drains a capability
	// invocation that is still running, and a landing's outcome is the whole
	// turn's outcome: draining afterwards would leave the fallback below --
	// "claude turn timeout" for a budget that expired while a landing was
	// mid-merge -- holding the sink's single-terminal latch, and the merge that
	// then succeeded would be reported as a failed run (PMR-177).
	//
	// A drained invocation therefore claims the latch first when it has an
	// outcome, and everything from here on is what to say when it did not.
	t.retire(s)

	// The loop ended without a terminal event, so this is the last chance to
	// report why -- unless this turn's outcome was already reported elsewhere.
	if t.sink.settled() {
		return
	}
	select {
	case <-timedOut:
		t.sink.emitTerminal(domain.Event{Kind: domain.EventFailed, At: time.Now(), Message: "claude turn timeout"})
		return
	default:
	}
	if t.cancelled() {
		t.sink.emitTerminal(domain.Event{Kind: domain.EventFailed, At: time.Now(), Message: "claude turn cancelled"})
		return
	}
	if tail := t.withoutEndpoint(stderr.text()); tail != "" {
		emit(domain.Event{Kind: domain.EventDiagnostic, At: time.Now(), Message: tail})
	}
	switch {
	case readErr != nil:
		t.sink.emitTerminal(domain.Event{Kind: domain.EventFailed, At: time.Now(), Message: "claude stdout read failed"})
	case waitErr != nil:
		t.sink.emitTerminal(domain.Event{Kind: domain.EventFailed, At: time.Now(), Message: "claude exited without completing the turn: " + exitText(waitErr)})
	default:
		t.sink.emitTerminal(domain.Event{Kind: domain.EventFailed, At: time.Now(), Message: "claude exited without reporting a result"})
	}
}

type pendingCall struct {
	tool    string
	started time.Time
}

func add(a, b domain.Usage) domain.Usage {
	return domain.Usage{
		InputTokens:  a.InputTokens + b.InputTokens,
		OutputTokens: a.OutputTokens + b.OutputTokens,
		TotalTokens:  a.TotalTokens + b.TotalTokens,
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
