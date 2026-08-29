package coordinator

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/domain"
)

// TestHandoffRunRecordsObservationForRevertDetection proves a completed run
// that ends because its issue reached the review handoff state is remembered,
// so a later external revert of that handoff can be attributed at poll time.
func TestHandoffRunRecordsObservationForRevertDetection(t *testing.T) {
	w := testSettings(t)
	w.Config.Tracker.HandoffState = "In Review"
	w.Config.Tracker.ActiveStates = []string{"Todo", "In Progress"}
	issue := testIssue()
	issue.State = "In Progress"
	handoff := issue
	handoff.State = "In Review"
	handoff.Dispatchable = false
	tracker := &fakeTracker{issue: issue}
	tracker.setFresh(handoff)
	agent := &fakeAgent{events: completedEvents}
	ws := &fakeWorkspace{shouldRun: true, after: make(chan struct{}, 1)}
	c := testCoordinator(w.Config, tracker, agent, ws)
	defer assertInvariants(t, c)

	c.Tick(context.Background())
	<-ws.after

	deadline := time.Now().Add(time.Second)
	for {
		observation, ok := c.handoffMemory(issue.ID)
		if ok && observation.state == "in review" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("completed handoff run did not record a review-state observation")
		}
		time.Sleep(time.Millisecond)
	}
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestExternalHandoffRevertIsObservedAtPoll proves the PMR-63 flap is now
// visible in the log: an active candidate that Symphony itself just handed off
// to the review state was reverted by an external actor, so the poll loop logs
// the external delta exactly once and never re-logs or mutates the tracker.
func TestExternalHandoffRevertIsObservedAtPoll(t *testing.T) {
	w := testSettings(t)
	w.Config.Tracker.HandoffState = "In Review"
	w.Config.Tracker.ActiveStates = []string{"Todo", "In Progress"}
	// In Progress is a start-policy endpoint, so the revert warns because it
	// reactivated a pre-review implementation state — not merely because the
	// destination was unclassifiable.
	w.Config.Tracker.HostTransitions.Start = map[string]string{"todo": "In Progress"}
	reverted := testIssue()
	reverted.State = "In Progress"
	tracker := &fakeTracker{issue: reverted}
	var logs bytes.Buffer
	c := New(tracker, &fakeAgent{}, &fakeWorkspace{}, func() config.Settings { return w.Config }, slog.New(slog.NewJSONHandler(&logs, nil)))
	defer assertInvariants(t, c)
	c.clock = fakeClock{now: time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)}

	// Pre-claim the reverted issue so the poll does not also launch a run, then
	// record Symphony's own prior handoff into the review state (claiming clears
	// any prior memory, so the observation must be set afterward).
	if !claims(c, reverted, w.Config) {
		t.Fatal("pre-claim failed")
	}
	c.noteHandoffObservation(domain.Issue{ID: reverted.ID, Identifier: reverted.Identifier, State: "In Review"}, w.Config, c.clock.Now())

	c.Tick(context.Background())

	output := logs.String()
	if !strings.Contains(output, `"msg":"external tracker state change observed"`) ||
		!strings.Contains(output, `"operation":"external_reversion"`) ||
		!strings.Contains(output, `"from_state":"in review"`) ||
		!strings.Contains(output, `"to_state":"in progress"`) ||
		!strings.Contains(output, `"issue_identifier":"ENG-1"`) {
		t.Fatalf("external handoff revert was not logged from the poll loop: %s", output)
	}
	_, still := c.handoffMemory(reverted.ID)
	if still {
		t.Fatal("handoff observation was not consumed after the revert was logged")
	}

	logs.Reset()
	c.Tick(context.Background())
	if strings.Contains(logs.String(), "external_reversion") {
		t.Fatalf("a single external revert was re-logged on a later poll: %s", logs.String())
	}
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestHealthyHandoffIsSweptWithoutExternalRevertLog proves the common case — an
// issue that stays in review and is never reverted — neither logs a spurious
// external delta nor leaks its handoff memory: the retention window bounds the
// map even when the issue never reappears as a candidate.
func TestHealthyHandoffIsSweptWithoutExternalRevertLog(t *testing.T) {
	w := testSettings(t)
	w.Config.Tracker.HandoffState = "In Review"
	w.Config.Polling.Interval = 30 * time.Second
	tracker := &issueMapTracker{issues: map[string]domain.Issue{}}
	var logs bytes.Buffer
	clock := &mutableClock{now: time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)}
	c := New(tracker, &fakeAgent{}, &fakeWorkspace{}, func() config.Settings { return w.Config }, slog.New(slog.NewJSONHandler(&logs, nil)))
	defer assertInvariants(t, c)
	c.clock = clock
	c.noteHandoffObservation(domain.Issue{ID: "id", Identifier: "ENG-1", State: "In Review"}, w.Config, clock.Now())

	c.Tick(context.Background())
	_, present := c.handoffMemory("id")
	if !present {
		t.Fatal("handoff memory was dropped inside the retention window")
	}

	clock.set(clock.now.Add(3 * time.Minute))
	c.Tick(context.Background())
	_, stillThere := c.handoffMemory("id")
	if stillThere {
		t.Fatal("stale handoff memory was not swept after the retention window")
	}
	if strings.Contains(logs.String(), "external_reversion") {
		t.Fatalf("a healthy handoff wrongly logged an external revert: %s", logs.String())
	}
}

// TestPostHandoffStateChangeIsClassified proves the human-controlled review
// state's outbound edges are told apart in the log: moving the issue to the
// merge state is the human approval that authorizes landing and moving it to
// the rework state is a human review decision — both expected, both info —
// while anything Symphony cannot name from the configured lifecycle, including
// a reactivation into a pre-review implementation state, stays an actionable
// warning.
func TestPostHandoffStateChangeIsClassified(t *testing.T) {
	canonical := []string{"Todo", "In Progress", "Rework", "Merging"}
	fromTodo := map[string]string{"todo": "In Progress"}
	const (
		expectedMessage = "human review state change observed"
		warnedMessage   = "external tracker state change observed"
	)
	for _, tc := range []struct {
		name                             string
		activeStates                     []string
		start                            map[string]string
		mergeState                       string
		state, operation, message, level string
	}{
		{
			name: "approval into the merge state", activeStates: canonical, start: fromTodo, mergeState: "Merging",
			state: "Merging", operation: "review_approved", message: expectedMessage, level: "INFO",
		},
		{
			name: "changes requested into the single remaining state", activeStates: canonical, start: fromTodo, mergeState: "Merging",
			state: "Rework", operation: "rework_requested", message: expectedMessage, level: "INFO",
		},
		{
			name: "reactivation into the start policy's target", activeStates: canonical, start: fromTodo, mergeState: "Merging",
			state: "In Progress", operation: "external_reversion", message: warnedMessage, level: "WARN",
		},
		{
			name: "reactivation into the start policy's source", activeStates: canonical, start: fromTodo, mergeState: "Merging",
			state: "Todo", operation: "external_reversion", message: warnedMessage, level: "WARN",
		},
		{
			// A second unaccounted-for active state makes the rework state
			// unnameable, so work parked in it is never passed off as expected.
			name:         "parked in an extra active state",
			activeStates: []string{"Todo", "In Progress", "Rework", "Merging", "Blocked"},
			start:        fromTodo, mergeState: "Merging",
			state: "Blocked", operation: "external_reversion", message: warnedMessage, level: "WARN",
		},
		{
			// The same ambiguity suppresses the rework naming itself: Symphony
			// warns rather than guessing which of the two states is Rework.
			name:         "ambiguous candidates suppress the rework naming",
			activeStates: []string{"Todo", "In Progress", "Rework", "Merging", "Blocked"},
			start:        fromTodo, mergeState: "Merging",
			state: "Rework", operation: "external_reversion", message: warnedMessage, level: "WARN",
		},
		{
			name:         "thrown back to a dispatchable backlog",
			activeStates: []string{"Backlog", "Todo", "In Progress", "Rework", "Merging"},
			start:        fromTodo, mergeState: "Merging",
			state: "Backlog", operation: "external_reversion", message: warnedMessage, level: "WARN",
		},
		{
			// A dispatch entry state the start policy does not name is still a
			// pre-review implementation state; reactivating into it must warn.
			name:         "reactivation into an unnamed entry state",
			activeStates: []string{"Todo", "Ready", "In Progress", "Rework", "Merging"},
			start:        map[string]string{"ready": "In Progress"}, mergeState: "Merging",
			state: "Todo", operation: "external_reversion", message: warnedMessage, level: "WARN",
		},
		{
			name: "no start policy leaves rework unnameable", activeStates: canonical, mergeState: "Merging",
			state: "Rework", operation: "external_reversion", message: warnedMessage, level: "WARN",
		},
		{
			// The approval edge is identified by github.merge_state alone, so it
			// survives a missing start policy.
			name: "approval without a start policy", activeStates: canonical, mergeState: "Merging",
			state: "Merging", operation: "review_approved", message: expectedMessage, level: "INFO",
		},
		{
			// With landing unconfigured there is no merge state to recognize, so
			// the same edge is unnameable and warns instead.
			name: "approval with landing unconfigured", activeStates: canonical, start: fromTodo,
			state: "Merging", operation: "external_reversion", message: warnedMessage, level: "WARN",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := testSettings(t)
			w.Config.Tracker.HandoffState = "In Review"
			w.Config.Tracker.ActiveStates = tc.activeStates
			w.Config.Tracker.HostTransitions.Start = tc.start
			w.Config.GitHub.MergeState = tc.mergeState
			moved := testIssue()
			moved.State = tc.state
			tracker := &fakeTracker{issue: moved}
			var logs bytes.Buffer
			c := New(tracker, &fakeAgent{}, &fakeWorkspace{}, func() config.Settings { return w.Config }, slog.New(slog.NewJSONHandler(&logs, nil)))
			defer assertInvariants(t, c)
			c.clock = fakeClock{now: time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)}

			// Pre-claim so the poll only classifies the change instead of also
			// launching a run, then record Symphony's own prior handoff (claiming
			// clears any prior memory, so the observation must be set afterward).
			if !claims(c, moved, w.Config) {
				t.Fatal("pre-claim failed")
			}
			c.noteHandoffObservation(domain.Issue{ID: moved.ID, Identifier: moved.Identifier, State: "In Review"}, w.Config, c.clock.Now())

			c.Tick(context.Background())

			output := logs.String()
			if !strings.Contains(output, `"msg":"`+tc.message+`"`) ||
				!strings.Contains(output, `"operation":"`+tc.operation+`"`) ||
				!strings.Contains(output, `"level":"`+tc.level+`"`) ||
				!strings.Contains(output, `"from_state":"in review"`) ||
				!strings.Contains(output, `"to_state":"`+config.Norm(tc.state)+`"`) ||
				!strings.Contains(output, `"issue_identifier":"ENG-1"`) {
				t.Fatalf("post-handoff change to %s was not classified as %s: %s", tc.state, tc.operation, output)
			}
			if tc.operation != "external_reversion" && strings.Contains(output, "external_reversion") {
				t.Fatalf("an expected human review decision was also logged as a reversion: %s", output)
			}
			if err := c.Shutdown(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}
