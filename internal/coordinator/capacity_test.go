package coordinator

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/domain"
)

// TestWaitingIssueAppearsInSnapshotUntilAdmitted guards the PMR-139 operator
// visibility requirement: an eligible issue rejected only for capacity earns
// no claim and no retry timer, so the poll's own waiting memory is the only
// place it can be observed. It must appear there with its identifier and
// state while capacity stays full, and disappear the moment it is admitted.
func TestWaitingIssueAppearsInSnapshotUntilAdmitted(t *testing.T) {
	w := testSettings(t)
	w.Config.Agent.MaxConcurrent = 1
	occupying := testIssue()
	occupying.ID, occupying.Identifier = "occupying", "ENG-OCCUPY"
	waitingIssue := testIssue()
	waitingIssue.ID, waitingIssue.Identifier = "queued", "ENG-QUEUED"
	tracker := &issueMapTracker{candidates: []domain.Issue{waitingIssue}, issues: map[string]domain.Issue{occupying.ID: occupying, waitingIssue.ID: waitingIssue}}
	block := make(chan domain.Event)
	agent := &fakeAgent{events: func() <-chan domain.Event { return block }, started: make(chan struct{}, 1)}
	ws := &fakeWorkspace{after: make(chan struct{}, 1)}
	c := testCoordinator(w.Config, tracker, agent, ws)
	defer assertInvariants(t, c)

	// Occupy the only slot directly, exactly as other coordinator tests seed
	// running/claimed/retrying state, so the poll observes real contention
	// without needing a second live run.
	c.occupySlot(occupying)

	c.Tick(context.Background())
	snapshot := c.Snapshot()
	if len(snapshot.Waiting) != 1 {
		t.Fatalf("waiting=%+v, want exactly the queued issue", snapshot.Waiting)
	}
	if got := snapshot.Waiting[0]; got.IssueIdentifier != "ENG-QUEUED" || got.IssueState != "Todo" || got.WaitingMS < 0 {
		t.Fatalf("waiting entry=%+v", got)
	}

	// Free the slot and re-poll: the queued issue must be admitted and drop out
	// of Waiting, exactly as an issue newly claimed always has.
	c.releaseSlot(occupying.ID)
	c.Tick(context.Background())
	<-agent.started
	waitForRunning(t, c, "ENG-QUEUED")

	snapshot = c.Snapshot()
	if len(snapshot.Waiting) != 0 {
		t.Fatalf("waiting=%+v, want empty once admitted", snapshot.Waiting)
	}
	if len(snapshot.Running) != 1 || snapshot.Running[0].IssueIdentifier != "ENG-QUEUED" {
		t.Fatalf("running=%+v, want the formerly queued issue", snapshot.Running)
	}
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-ws.after
}

// TestWaitingListNeverDuplicatesARunningOrRetryingIssue guards the PMR-139
// acceptance criterion that the waiting list never grows a second entry for
// an issue already visible in Running or Retrying: admissionRejectReason
// reports already_claimed for a claimed issue before it ever reaches the
// at_capacity check that populates Waiting.
func TestWaitingListNeverDuplicatesARunningOrRetryingIssue(t *testing.T) {
	w := testSettings(t)
	w.Config.Agent.MaxConcurrent = 1
	runningIssue := testIssue()
	runningIssue.ID, runningIssue.Identifier = "running", "ENG-RUNNING"
	retrying := testIssue()
	retrying.ID, retrying.Identifier = "retrying", "ENG-RETRYING"
	queued := testIssue()
	queued.ID, queued.Identifier = "queued", "ENG-QUEUED"
	tracker := &issueMapTracker{candidates: []domain.Issue{runningIssue, retrying, queued}, issues: map[string]domain.Issue{runningIssue.ID: runningIssue, retrying.ID: retrying, queued.ID: queued}}
	c := testCoordinator(w.Config, tracker, &fakeAgent{}, &fakeWorkspace{})
	defer assertInvariants(t, c)
	timer := &fakeTimer{}
	c.timer = timer

	c.occupySlot(runningIssue)
	c.mu.Lock()
	c.stateLocked(runningIssue.ID).run = &running{issue: runningIssue, session: domain.AgentSession{ID: "s"}, last: time.Now(), cancel: func() {}}
	// A retry timer holds only the duplicate-prevention claim, never an
	// orchestrator slot (PMR-78/PMR-129), so it is seeded directly here rather
	// than through claim(), which would itself require the capacity this test
	// deliberately keeps fully occupied by runningIssue.
	seeded := c.ensureStateLocked(retrying.ID)
	seeded.claimed = true
	seeded.state = config.Norm(retrying.State)
	c.mu.Unlock()
	c.scheduleRetry(context.Background(), retrying, domain.Workspace{}, 1, retryAgent, "test", time.Minute)

	c.Tick(context.Background())
	snapshot := c.Snapshot()
	if len(snapshot.Running) != 1 || snapshot.Running[0].IssueIdentifier != "ENG-RUNNING" {
		t.Fatalf("running=%+v, want the seeded running issue", snapshot.Running)
	}
	if len(snapshot.Retrying) != 1 || snapshot.Retrying[0].IssueIdentifier != "ENG-RETRYING" {
		t.Fatalf("retrying=%+v", snapshot.Retrying)
	}
	if len(snapshot.Waiting) != 1 || snapshot.Waiting[0].IssueIdentifier != "ENG-QUEUED" {
		t.Fatalf("waiting=%+v, want only the queued issue -- running and retrying must never duplicate into it", snapshot.Waiting)
	}
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestBlockedIssueAppearsInSnapshotWithBlockerIdentifiersUntilResolved guards
// the PMR-152 operator-visibility requirement: a Todo issue held ineligible
// by an open blocker relation earns no claim and no retry timer, exactly like
// a capacity-only rejection (PMR-139), so it must appear in Waiting with its
// blocker identifiers and disappear the moment the blocker is resolved.
func TestBlockedIssueAppearsInSnapshotWithBlockerIdentifiersUntilResolved(t *testing.T) {
	w := testSettings(t)
	blocked := testIssue()
	blocked.Dispatchable = false
	blocked.BlockedBy = []domain.Blocker{{ID: "blocker-id", Identifier: "ENG-0", State: "In Progress", Dispatchable: false}}
	tracker := &issueMapTracker{candidates: []domain.Issue{blocked}, issues: map[string]domain.Issue{blocked.ID: blocked}}
	block := make(chan domain.Event)
	agent := &fakeAgent{events: func() <-chan domain.Event { return block }, started: make(chan struct{}, 1)}
	ws := &fakeWorkspace{after: make(chan struct{}, 1)}
	c := testCoordinator(w.Config, tracker, agent, ws)
	defer assertInvariants(t, c)

	c.Tick(context.Background())
	snapshot := c.Snapshot()
	if len(snapshot.Waiting) != 1 {
		t.Fatalf("waiting=%+v, want exactly the blocked issue", snapshot.Waiting)
	}
	if got := snapshot.Waiting[0]; got.IssueIdentifier != blocked.Identifier || got.IssueState != "Todo" || got.Reason != "blocked_by_relation" || len(got.BlockedBy) != 1 || got.BlockedBy[0] != "ENG-0" || got.WaitingMS < 0 {
		t.Fatalf("blocked waiting entry=%+v", got)
	}

	// Resolve the blocker and re-poll: the issue must become dispatchable,
	// leave Waiting, and (capacity being free) get admitted, exactly as a
	// capacity-only wait clears once a slot opens (PMR-139).
	resolved := blocked
	resolved.Dispatchable = true
	resolved.BlockedBy = []domain.Blocker{{ID: "blocker-id", Identifier: "ENG-0", State: "Done", Dispatchable: true}}
	tracker.setIssue(resolved)
	c.Tick(context.Background())
	<-agent.started
	waitForRunning(t, c, blocked.Identifier)

	snapshot = c.Snapshot()
	if len(snapshot.Waiting) != 0 {
		t.Fatalf("waiting=%+v, want empty once the blocker resolves", snapshot.Waiting)
	}
	if len(snapshot.Running) != 1 || snapshot.Running[0].IssueIdentifier != blocked.Identifier {
		t.Fatalf("running=%+v, want the formerly blocked issue", snapshot.Running)
	}
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-ws.after
}

// TestBlockedWaitReasonChangesToCapacityWithoutStayingBlocked guards two
// PMR-152 acceptance criteria at once: an issue is never reported under both
// waiting reasons at once, and a reason change (here, a resolved blocker
// immediately followed by full capacity) still counts as leaving the
// blocked-by-relation state and re-entering as a fresh, separately dated
// wait rather than a stale one that silently changed meaning underneath its
// own timestamp.
func TestBlockedWaitReasonChangesToCapacityWithoutStayingBlocked(t *testing.T) {
	w := testSettings(t)
	w.Config.Agent.MaxConcurrent = 1
	occupying := testIssue()
	occupying.ID, occupying.Identifier = "occupying", "ENG-OCCUPY"
	blocked := testIssue()
	blocked.ID, blocked.Identifier = "blocked", "ENG-BLOCKED"
	blocked.Dispatchable = false
	blocked.BlockedBy = []domain.Blocker{{ID: "blocker-id", Identifier: "ENG-0", State: "In Progress", Dispatchable: false}}
	tracker := &issueMapTracker{candidates: []domain.Issue{blocked}, issues: map[string]domain.Issue{occupying.ID: occupying, blocked.ID: blocked}}
	c := testCoordinator(w.Config, tracker, &fakeAgent{}, &fakeWorkspace{})
	defer assertInvariants(t, c)
	clock := &mutableClock{now: time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)}
	c.clock = clock

	c.occupySlot(occupying)

	c.Tick(context.Background())
	snapshot := c.Snapshot()
	if len(snapshot.Waiting) != 1 || snapshot.Waiting[0].Reason != "blocked_by_relation" {
		t.Fatalf("waiting=%+v, want exactly one blocked_by_relation entry", snapshot.Waiting)
	}

	clock.set(clock.now.Add(time.Minute))
	resolved := blocked
	resolved.Dispatchable = true
	resolved.BlockedBy = []domain.Blocker{{ID: "blocker-id", Identifier: "ENG-0", State: "Done", Dispatchable: true}}
	tracker.setIssue(resolved)
	c.Tick(context.Background())

	snapshot = c.Snapshot()
	if len(snapshot.Waiting) != 1 {
		t.Fatalf("waiting=%+v, want the issue still waiting, now for capacity", snapshot.Waiting)
	}
	got := snapshot.Waiting[0]
	if got.Reason != "at_capacity" || len(got.BlockedBy) != 0 {
		t.Fatalf("waiting entry after blocker resolved=%+v, want at_capacity with no blockers", got)
	}
	if got.WaitingMS != 0 {
		t.Fatalf("waiting entry since=%v, want reset to the poll that observed the reason change, not the original blocked_by_relation entry", got.WaitingMS)
	}
}

// TestBlockedWaitLoggedOnceNotPerPoll guards the PMR-152 acceptance criterion
// that a blocker hold is edge-triggered, mirroring PMR-139's own waiting-for-
// capacity record: an issue that stays blocked across several polls must log
// "issue blocked by an open dependency" exactly once, on the poll it entered
// the state, not on every subsequent poll that merely reobserves it.
func TestBlockedWaitLoggedOnceNotPerPoll(t *testing.T) {
	w := testSettings(t)
	blocked := testIssue()
	blocked.Dispatchable = false
	blocked.BlockedBy = []domain.Blocker{{ID: "blocker-id", Identifier: "ENG-0", State: "In Progress", Dispatchable: false}}
	tracker := &issueMapTracker{candidates: []domain.Issue{blocked}, issues: map[string]domain.Issue{blocked.ID: blocked}}
	var logs bytes.Buffer
	c := New(tracker, &fakeAgent{}, &fakeWorkspace{}, func() config.Settings { return w.Config }, slog.New(slog.NewJSONHandler(&logs, nil)))
	defer assertInvariants(t, c)

	c.Tick(context.Background())
	c.Tick(context.Background())
	c.Tick(context.Background())

	output := logs.String()
	if got := strings.Count(output, `"msg":"issue blocked by an open dependency"`); got != 1 {
		t.Fatalf("blocked-entry log fired %d times across 3 polls, want exactly 1: %s", got, output)
	}
}

// TestBlockedWaitEscalatesToWarnAfterThreshold guards the proposal's
// escalation for a blocker hold that outlasts the poll cadence by a wide
// margin -- the signal that the blocker is not actually scheduled -- mirroring
// PMR-139's own capacity-wait escalation.
func TestBlockedWaitEscalatesToWarnAfterThreshold(t *testing.T) {
	w := testSettings(t)
	w.Config.Polling.Interval = time.Second
	blocked := testIssue()
	blocked.Dispatchable = false
	blocked.BlockedBy = []domain.Blocker{{ID: "blocker-id", Identifier: "ENG-0", State: "In Progress", Dispatchable: false}}
	tracker := &issueMapTracker{candidates: []domain.Issue{blocked}, issues: map[string]domain.Issue{blocked.ID: blocked}}
	var logs bytes.Buffer
	c := New(tracker, &fakeAgent{}, &fakeWorkspace{}, func() config.Settings { return w.Config }, slog.New(slog.NewJSONHandler(&logs, nil)))
	defer assertInvariants(t, c)
	clock := &mutableClock{now: time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)}
	c.clock = clock

	c.Tick(context.Background())
	if strings.Contains(logs.String(), `"msg":"issue still blocked by an open dependency"`) {
		t.Fatalf("escalated before the threshold: %s", logs.String())
	}

	clock.set(clock.now.Add(waitingEscalationFloor))
	c.Tick(context.Background())
	output := logs.String()
	if got := strings.Count(output, `"msg":"issue still blocked by an open dependency"`); got != 1 {
		t.Fatalf("blocked-entry escalation fired %d times, want exactly 1: %s", got, output)
	}
	if !strings.Contains(output, `"blocked_by":"ENG-0"`) {
		t.Fatalf("escalation missing the blocker identifier: %s", output)
	}

	// A further poll past the threshold must not escalate a second time.
	clock.set(clock.now.Add(time.Minute))
	c.Tick(context.Background())
	if got := strings.Count(logs.String(), `"msg":"issue still blocked by an open dependency"`); got != 1 {
		t.Fatalf("blocked-entry escalation fired again: %s", logs.String())
	}
}

func TestFourImplementationAndReworkIssuesRunConcurrently(t *testing.T) {
	w := testSettings(t)
	w.Config.Agent.MaxConcurrent = 4
	w.Config.Tracker.ActiveStates = []string{"Todo", "In Progress", "Rework"}

	issues := make([]domain.Issue, 5)
	states := []string{"Todo", "In Progress", "Rework", "Rework", "In Progress"}
	issueMap := make(map[string]domain.Issue, len(issues))
	for index := range issues {
		issues[index] = testIssue()
		issues[index].ID = fmt.Sprintf("issue-%d", index+1)
		issues[index].Identifier = fmt.Sprintf("ENG-%d", index+1)
		issues[index].State = states[index]
		issueMap[issues[index].ID] = issues[index]
	}

	tracker := &issueMapTracker{candidates: issues, issues: issueMap}
	block := make(chan domain.Event)
	agent := &fakeAgent{events: func() <-chan domain.Event { return block }, started: make(chan struct{}, 4)}
	ws := &fakeWorkspace{after: make(chan struct{}, 4)}
	c := testCoordinator(w.Config, tracker, agent, ws)
	defer assertInvariants(t, c)

	c.Tick(context.Background())
	for range 4 {
		<-agent.started
	}
	starts, _, _ := agent.counts()
	if starts != 4 {
		t.Fatalf("starts=%d, want four concurrent implementation/rework agents", starts)
	}
	admitted := c.admittedTotal()
	if admitted != 4 {
		t.Fatalf("admitted=%d, want the global four-agent capacity fully occupied", admitted)
	}
	if c.claim(issues[4], w.Config) {
		t.Fatal("a fifth implementation issue exceeded the global four-agent capacity")
	}

	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	for range 4 {
		<-ws.after
	}
}

// TestMergingAndUnrelatedImplementationRunConcurrentlyUnderByStateCapacity
// exercises the active four-agent policy end to end at the coordinator level:
// one Merging landing agent and unrelated implementation agents admit and run
// at the same time, a queued retry timer never occupies a concurrency slot
// while it waits, and max_concurrent_agents_by_state still refuses a second
// concurrent Merging issue even though overall capacity has spare room.
func TestMergingAndUnrelatedImplementationRunConcurrentlyUnderByStateCapacity(t *testing.T) {
	w := testSettings(t)
	w.Config.Agent.MaxConcurrent = 4
	w.Config.Tracker.ActiveStates = []string{"In Progress", "Merging"}
	w.Config.Agent.ByState = map[string]int{"merging": 1}

	implementation := testIssue()
	implementation.ID, implementation.Identifier, implementation.State = "impl", "ENG-1", "In Progress"
	landing := testIssue()
	landing.ID, landing.Identifier, landing.State = "landing", "ENG-2", "Merging"
	secondLanding := testIssue()
	secondLanding.ID, secondLanding.Identifier, secondLanding.State = "landing-2", "ENG-3", "Merging"
	retryable := testIssue()
	retryable.ID, retryable.Identifier, retryable.State = "retryable", "ENG-4", "In Progress"
	extra := testIssue()
	extra.ID, extra.Identifier, extra.State = "extra", "ENG-5", "In Progress"
	fourth := testIssue()
	fourth.ID, fourth.Identifier, fourth.State = "fourth", "ENG-6", "In Progress"

	tracker := &issueMapTracker{
		candidates: []domain.Issue{implementation, landing},
		issues: map[string]domain.Issue{
			implementation.ID: implementation, landing.ID: landing, secondLanding.ID: secondLanding,
			retryable.ID: retryable, extra.ID: extra, fourth.ID: fourth,
		},
	}
	block := make(chan domain.Event)
	agent := &fakeAgent{events: func() <-chan domain.Event { return block }, started: make(chan struct{}, 4)}
	ws := &fakeWorkspace{after: make(chan struct{}, 4)}
	c := testCoordinator(w.Config, tracker, agent, ws)
	defer assertInvariants(t, c)
	timer := &fakeTimer{}
	c.timer = timer

	// One unrelated implementation issue and one Merging landing issue admit
	// and launch together in the same poll.
	c.Tick(context.Background())
	<-agent.started
	<-agent.started
	if starts, _, _ := agent.counts(); starts != 2 {
		t.Fatalf("starts=%d, want the unrelated implementation and Merging issues both admitted", starts)
	}

	// A queued retry for a third, unrelated issue must not occupy a
	// concurrency slot while it waits.
	if !c.claim(retryable, w.Config) {
		t.Fatal("retryable issue was not claimed")
	}
	c.scheduleRetry(context.Background(), retryable, domain.Workspace{}, 1, retryAgent, "test", time.Minute)
	admitted := c.admittedTotal()
	if admitted != 2 {
		t.Fatalf("admitted=%d, want a queued retry timer to consume no concurrency slot", admitted)
	}

	// A second concurrent Merging issue is refused by the per-state cap even
	// though overall capacity (2 of 4) still has room.
	if c.claim(secondLanding, w.Config) {
		t.Fatal("a second concurrent Merging issue must be refused by max_concurrent_agents_by_state")
	}

	// The retry's reserved claim must not itself block a genuinely free
	// general-capacity slot from admitting a new, unrelated candidate.
	tracker.mu.Lock()
	tracker.candidates = append(tracker.candidates, extra, fourth)
	tracker.mu.Unlock()
	c.Tick(context.Background())
	<-agent.started
	<-agent.started
	if starts, _, _ := agent.counts(); starts != 4 {
		t.Fatalf("starts=%d, want both free general-capacity slots admitted despite the queued retry", starts)
	}

	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-ws.after
	<-ws.after
	<-ws.after
	<-ws.after
}
