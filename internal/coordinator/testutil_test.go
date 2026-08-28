package coordinator

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/domain"
)

// syncBuffer is a concurrency-safe io.Writer/String() pair used to poll a log
// sink that a background coordinator goroutine is still writing to.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}
func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func waitForSubstring(t *testing.T, buf *syncBuffer, substr string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		output := buf.String()
		if strings.Contains(output, substr) {
			return output
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for log containing %q; got: %s", substr, output)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// findLine returns the single log line within output containing marker, so a
// test can pin an assertion to one record (for example the terminal summary)
// rather than the whole buffered log, where an earlier, legitimately
// different figure would otherwise make a Contains check pass or fail for the
// wrong reason.
func findLine(t *testing.T, output, marker string) string {
	t.Helper()
	for _, line := range strings.Split(strings.TrimRight(output, "\n"), "\n") {
		if strings.Contains(line, marker) {
			return line
		}
	}
	t.Fatalf("no log line contains %q: %s", marker, output)
	return ""
}

// waitForRunning blocks until identifier appears in c's running snapshot.
// agent.started only confirms Start was called; the launch goroutine still
// needs to record the session on the issue's state record afterward, and a test that
// advances the clock and re-ticks before that happens will see reconcile
// find no running sessions at all, so a stall or eligibility change it
// expects to observe is silently missed and any following blocking receive
// (e.g. <-ws.after) hangs until the test binary's own timeout.
func waitForRunning(t *testing.T, c *Coordinator, identifier string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		for _, r := range c.Snapshot().Running {
			if r.IssueIdentifier == identifier {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("issue %s never appeared in the running snapshot", identifier)
		}
		time.Sleep(time.Millisecond)
	}
}

// cleanupLogLines returns every "workspace cleanup" record in a JSONL log, in
// order, so a test can assert on each attempt's level and status without a
// substring match spuriously matching across lines.
func cleanupLogLines(records string) []string {
	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(records), "\n") {
		if strings.Contains(line, `"msg":"workspace cleanup"`) {
			lines = append(lines, line)
		}
	}
	return lines
}

// waitForRelease waits until a finished run has released its claim, which the
// launch goroutine does after the workspace after_run hook.
func waitForRelease(t *testing.T, c *Coordinator, id string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		claimed, running := c.claimHeld(id), c.runningCount()
		if !claimed && running == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("run for %s never released its claim (claimed=%v running=%d)", id, claimed, running)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func landingWaitingEvents() <-chan domain.Event {
	ch := make(chan domain.Event, 2)
	ch <- domain.Event{Kind: domain.EventItem, At: time.Now(), ItemID: "1", ItemType: "dynamicToolCall", ToolName: "github_land_pr", Outcome: domain.ItemCompleted}
	ch <- domain.Event{Kind: domain.EventLandingWaiting, At: time.Now(), SessionID: "t-u", Message: "required checks are pending"}
	close(ch)
	return ch
}

func landingResolvedEvents() <-chan domain.Event {
	ch := make(chan domain.Event, 2)
	ch <- domain.Event{Kind: domain.EventItem, At: time.Now(), ItemID: "1", ItemType: "dynamicToolCall", ToolName: "github_land_pr", Outcome: domain.ItemCompleted}
	ch <- domain.Event{Kind: domain.EventLandingResolved, At: time.Now(), SessionID: "t-u"}
	close(ch)
	return ch
}

// startTransitionSettings adds a Todo -> In Progress dispatch-time start
// transition to the base test workflow so the coordinator's host-side move is
// exercised. In Progress is added as an active state so the moved issue stays
// eligible for reconciliation exactly as production configuration requires.
func startTransitionSettings(t *testing.T) config.Settings {
	t.Helper()
	w := testSettings(t)
	s := w.Config
	s.Tracker.ActiveStates = []string{"Todo", "In Progress"}
	s.Tracker.HostTransitions = config.HostTransitions{Start: map[string]string{"todo": "In Progress"}}
	return s
}

func testCoordinator(settings config.Settings, tracker domain.Tracker, agent domain.AgentBackend, ws domain.WorkspaceExecutor) *Coordinator {
	c := New(tracker, agent, ws, func() config.Settings { return settings }, nil)
	c.clock = fakeClock{now: time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)}
	return c
}

func testSettings(t *testing.T) config.Workflow {
	t.Helper()
	d := t.TempDir()
	workflow := filepath.Join(d, "WORKFLOW.md")
	if err := os.WriteFile(workflow, []byte("---\ntracker: {kind: linear, active_states: [Todo], terminal_states: [Done]}\nagent: {max_concurrent_agents: 1, max_turns: 1}\nworkspace: {root: /tmp/work}\n---\nWork on {{.issue.identifier}}"), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := config.Load(workflow, "")
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func testIssue() domain.Issue {
	return domain.Issue{ID: "id", Identifier: "ENG-1", Title: "Work", State: "Todo", Dispatchable: true}
}

func completedEvents() <-chan domain.Event {
	ch := make(chan domain.Event, 1)
	ch <- domain.Event{Kind: domain.EventCompleted, At: time.Now(), SessionID: "t-u"}
	close(ch)
	return ch
}

func closedEvents() <-chan domain.Event {
	ch := make(chan domain.Event)
	close(ch)
	return ch
}

// failedEvents returns a domain.EventFailed carrying message, the shape a
// real backend uses for model/provider-reported text (see
// claude/backend.go's emitTerminal calls) -- unlike closedEvents, which is
// the host's own event plumbing giving up with no verdict at all.
func failedEvents(message string) func() <-chan domain.Event {
	return func() <-chan domain.Event {
		ch := make(chan domain.Event, 1)
		ch <- domain.Event{Kind: domain.EventFailed, At: time.Now(), Message: message}
		close(ch)
		return ch
	}
}

// rateLimitedEvents returns a domain.EventRateLimited carrying retryAfter,
// the shape the Claude backend reports for a quota rejection (PMR-131).
func rateLimitedEvents(retryAfter time.Duration) func() <-chan domain.Event {
	return func() <-chan domain.Event {
		ch := make(chan domain.Event, 1)
		ch <- domain.Event{Kind: domain.EventRateLimited, At: time.Now(), Message: "claude reported a rate limit: rejected (five_hour)", RateLimitStatus: "rejected", RetryAfter: retryAfter}
		close(ch)
		return ch
	}
}
