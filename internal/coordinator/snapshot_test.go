package coordinator

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/domain"
)

// TestRunningSnapshotUsageIsLiveAndSurvivesFailure pins the running-entry
// usage field to reporting real figures while a session is still actively
// spending tokens, not only after it produces a result: a cost field that
// always reads zero mid-run is worse than absent, because it looks
// authoritative. The run here ends in EventFailed rather than EventCompleted
// -- the shape of a turn killed by turn_timeout_ms, which never gets a result
// event -- so the usage already recorded before that failure has to be the
// evidence that survives, not a per-run total computed only on success.
func TestRunningSnapshotUsageIsLiveAndSurvivesFailure(t *testing.T) {
	w := testSettings(t)
	issue := testIssue()
	tracker := &fakeTracker{issue: issue}
	ch := make(chan domain.Event)
	agent := &fakeAgent{events: func() <-chan domain.Event { return ch }, started: make(chan struct{}, 1)}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{}, 1)}
	var logs bytes.Buffer
	c := New(tracker, agent, ws, func() config.Settings { return w.Config }, slog.New(slog.NewJSONHandler(&logs, nil)))
	c.clock = fakeClock{now: time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)}

	c.Tick(context.Background())
	<-agent.started
	waitForRunning(t, c, issue.Identifier)

	ch <- domain.Event{Kind: domain.EventUsage, At: time.Now(), Usage: domain.Usage{InputTokens: 7000, OutputTokens: 300, TotalTokens: 7300}}

	deadline := time.Now().Add(2 * time.Second)
	for {
		var live domain.Usage
		for _, r := range c.Snapshot().Running {
			if r.IssueIdentifier == issue.Identifier {
				live = r.Usage
			}
		}
		if live.TotalTokens != 0 {
			if live.InputTokens != 7000 || live.OutputTokens != 300 || live.TotalTokens != 7300 {
				t.Fatalf("in-flight usage=%+v", live)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("running entry never reported non-zero in-flight usage")
		}
		time.Sleep(time.Millisecond)
	}

	// The turn ends without a result -- exactly what a turn_timeout_ms kill
	// produces -- so the only trace of what it spent is the usage already
	// logged above, not a completion-time summary.
	ch <- domain.Event{Kind: domain.EventFailed, At: time.Now(), Message: "claude turn timeout"}
	<-ws.after
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}

	output := logs.String()
	for _, field := range []string{`"input_tokens":7000`, `"output_tokens":300`, `"total_tokens":7300`} {
		if !strings.Contains(output, field) {
			t.Fatalf("usage did not survive the timeout failure in the log: missing %s: %s", field, output)
		}
	}
}

func TestSnapshotCopiesOnlySafeOperationalMetadata(t *testing.T) {
	c := New(&fakeTracker{}, &fakeAgent{}, &fakeWorkspace{}, func() config.Settings { return config.Settings{} }, nil)
	now := time.Now()
	c.claimed["provider-id"] = true
	c.stopping = true
	c.running["provider-id"] = &running{issue: domain.Issue{ID: "provider-id", Identifier: "PMR-6", State: "In Progress", Description: "must-not-appear"}, session: domain.AgentSession{ID: "session", ThreadID: "thread", TurnID: "turn"}, last: now, run: domain.Run{Attempt: 2, TurnCount: 1, StartedAt: now, Usage: domain.Usage{InputTokens: 1}}, rateLimit: map[string]int64{"remaining": 2}, outstanding: &outstandingOp{ItemID: "must-not-appear", ItemType: "dynamicToolCall", ToolName: "github_publish_pr", Since: now.Add(-time.Second)}}
	c.retries["retry-id"] = retryState{issue: domain.Issue{ID: "retry-id", Identifier: "PMR-9", Description: "must-not-appear"}, attempt: 3, kind: retryAgent, reason: "agent_event", due: now}
	snapshot := c.Snapshot()
	if snapshot.Claimed != 1 || !snapshot.Stopping || len(snapshot.Running) != 1 || len(snapshot.Retrying) != 1 {
		t.Fatalf("snapshot=%+v", snapshot)
	}
	if snapshot.Running[0].IssueIdentifier != "PMR-6" || snapshot.Running[0].IssueState != "In Progress" || snapshot.Running[0].RateLimit["remaining"] != 2 || snapshot.Running[0].OutstandingOperation == nil || snapshot.Running[0].OutstandingOperation.Type != "dynamicToolCall" || snapshot.Running[0].OutstandingOperation.Name != "github_publish_pr" || snapshot.Retrying[0].IssueIdentifier != "PMR-9" {
		t.Fatalf("snapshot=%+v", snapshot)
	}
	snapshot.Running[0].RateLimit["remaining"] = 99
	if c.running["provider-id"].rateLimit["remaining"] != 2 {
		t.Fatal("snapshot mutated live coordinator state")
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	for _, prohibited := range []string{"must-not-appear", "item_id"} {
		if strings.Contains(string(encoded), prohibited) {
			t.Fatalf("snapshot leaked %q: %s", prohibited, encoded)
		}
	}
}
