package capability

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pmrrasmussen/symphony/internal/domain"
)

// The dispatch tests stand in for both transports at once. That is the point of
// the sequence living here: what a call does, in what order, and which calls are
// reported as item records are no longer answerable per transport, so they are
// asserted once against the algorithm both of them run.

// stubCapability is a bound capability whose reporting, argument validation, and
// invocation the test supplies.
type stubCapability struct {
	name      string
	lifecycle bool
	prepare   func(json.RawMessage) (Invocation, *Failure)
}

func (c stubCapability) Definition() Definition {
	return Definition{Name: c.name, Description: c.name, InputSchema: map[string]any{"type": "object"}}
}
func (c stubCapability) Lifecycle() bool { return c.lifecycle }
func (c stubCapability) Prepare(arguments json.RawMessage) (Invocation, *Failure) {
	return c.prepare(arguments)
}

// runs is a capability that accepts any arguments and invokes invoke.
func runs(name string, lifecycle bool, invoke Invocation) stubCapability {
	return stubCapability{name: name, lifecycle: lifecycle, prepare: func(json.RawMessage) (Invocation, *Failure) {
		return invoke, nil
	}}
}

func succeeds(payload any) Invocation {
	return func(context.Context) (Result, *Failure) { return Result{Success: true, Payload: payload}, nil }
}

type stubResolver map[string]Capability

func (r stubResolver) Lookup(name string) (Capability, bool) {
	bound, ok := r[name]
	return bound, ok
}

// recorder is a transport that records everything Dispatch hands it, in order.
// The order is as much of the contract as the content: a terminal event emitted
// before the response, or a released slot before either, is a behaviour change
// this package cannot see any other way.
type recorder struct {
	steps    []string
	events   []domain.Event
	outcomes []Outcome
}

func (r *recorder) transport() Transport {
	return Transport{
		CallID:  "call-1",
		Respond: func(outcome Outcome) { r.steps, r.outcomes = append(r.steps, "respond"), append(r.outcomes, outcome) },
		Emit: func(event domain.Event) {
			step := "emit:" + string(event.Kind)
			if event.Kind == domain.EventItem {
				step = "item:" + event.Outcome
			}
			r.steps, r.events = append(r.steps, step), append(r.events, event)
		},
	}
}

// gated is the transport a gated endpoint supplies: an advertisement gate and an
// invocation slot, both recorded.
func (r *recorder) gated(allow bool) Transport {
	transport := r.transport()
	transport.Allow = func(string) bool {
		r.steps = append(r.steps, "allow")
		return allow
	}
	transport.Enter = func() (func(), *Failure) {
		r.steps = append(r.steps, "enter")
		return func() { r.steps = append(r.steps, "leave") }, nil
	}
	return transport
}

func (r *recorder) items() []domain.Event {
	var items []domain.Event
	for _, event := range r.events {
		if event.Kind == domain.EventItem {
			items = append(items, event)
		}
	}
	return items
}

func (r *recorder) refusal(t *testing.T) string {
	t.Helper()
	if len(r.outcomes) != 1 {
		t.Fatalf("dispatch responded %d times, want exactly one response", len(r.outcomes))
	}
	if r.outcomes[0].Refusal == nil {
		t.Fatalf("dispatch responded with a result, want a refusal: %+v", r.outcomes[0])
	}
	return r.outcomes[0].Refusal.Message
}

// TestALifecycleCapabilityIsReportedAsADynamicToolCallPair is the behaviour the
// MCP transport was missing entirely before this dispatch existed: the same
// capability call was visible with a duration under Codex and invisible under
// Claude. Both transports now emit these records from here, so the record shape
// is asserted once.
func TestALifecycleCapabilityIsReportedAsADynamicToolCallPair(t *testing.T) {
	var records recorder
	Dispatch(context.Background(), stubResolver{NameGitHubLandPR: runs(NameGitHubLandPR, true, func(context.Context) (Result, *Failure) {
		time.Sleep(5 * time.Millisecond)
		return Result{Success: true, Payload: map[string]any{"status": "merged"}}, nil
	})}, records.transport(), NameGitHubLandPR, json.RawMessage(`{}`))

	items := records.items()
	if len(items) != 2 {
		t.Fatalf("emitted %d item records, want a started/completed pair: %+v", len(items), items)
	}
	for _, item := range items {
		if item.ItemType != "dynamicToolCall" || item.ToolName != NameGitHubLandPR || item.ItemID != "call-1" {
			t.Fatalf("item record = %+v, want a dynamicToolCall named %s under the transport's call ID", item, NameGitHubLandPR)
		}
	}
	if items[0].Outcome != domain.ItemStarted || items[0].DurationMs != 0 {
		t.Fatalf("started record = %+v", items[0])
	}
	if items[1].Outcome != domain.ItemCompleted || items[1].DurationMs < 1 {
		t.Fatalf("completed record = %+v, want a measured duration", items[1])
	}
	// The response follows both records, so a consumer never sees a result for a
	// call it has not been told finished.
	if got := strings.Join(records.steps, ","); got != "item:started,item:completed,respond" {
		t.Fatalf("dispatch order = %s", got)
	}
}

// TestACapabilityThatOptsOutOfLifecycleReportingEmitsNoItemRecords is
// create_followup_issue's guarantee, now that one boolean decides it for both
// transports rather than one transport's code path.
func TestACapabilityThatOptsOutOfLifecycleReportingEmitsNoItemRecords(t *testing.T) {
	var records recorder
	Dispatch(context.Background(), stubResolver{NameCreateFollowupIssue: runs(NameCreateFollowupIssue, false, succeeds("created"))},
		records.transport(), NameCreateFollowupIssue, json.RawMessage(`{}`))
	if items := records.items(); len(items) != 0 {
		t.Fatalf("an unreported capability emitted %+v", items)
	}
	if len(records.outcomes) != 1 || records.outcomes[0].Refusal != nil || !records.outcomes[0].Success {
		t.Fatalf("responded %+v", records.outcomes)
	}
	if string(records.outcomes[0].Payload) != `"created"` {
		t.Fatalf("payload = %s", records.outcomes[0].Payload)
	}
}

// TestNothingDecodedFromTheWireReachesAnItemRecord pins the rule both transports
// used to hold on their own: the requested name selects a capability and is then
// discarded, so a hostile name cannot be echoed into an event.
func TestNothingDecodedFromTheWireReachesAnItemRecord(t *testing.T) {
	var records recorder
	Dispatch(context.Background(), stubResolver{"GitHub-Land-PR\n$(whoami)": runs(NameGitHubLandPR, true, succeeds("done"))},
		records.transport(), "GitHub-Land-PR\n$(whoami)", json.RawMessage(`{}`))
	for _, item := range records.items() {
		if item.ToolName != NameGitHubLandPR {
			t.Fatalf("item record carried the requested name: %+v", item)
		}
	}
}

func TestAnUnknownNameIsRefusedWithoutAnyRecord(t *testing.T) {
	var records recorder
	Dispatch(context.Background(), stubResolver{}, records.transport(), "shell", json.RawMessage(`{}`))
	if message := records.refusal(t); message != "Unsupported client-side tool." {
		t.Fatalf("refusal = %q", message)
	}
	if len(records.events) != 0 {
		t.Fatalf("an unknown name emitted %+v", records.events)
	}
}

// TestTheAdvertisementGateRefusesBeforeAnyCapabilityWork covers the gate only
// internal/mcpbridge supplies: it is consulted on the resolved capability, and a
// refusal is indistinguishable from an unknown name, so an unadvertised
// capability cannot be probed for.
func TestTheAdvertisementGateRefusesBeforeAnyCapabilityWork(t *testing.T) {
	var records recorder
	prepared := false
	bound := stubCapability{name: NameGitHubLandPR, lifecycle: true, prepare: func(json.RawMessage) (Invocation, *Failure) {
		prepared = true
		return succeeds("landed"), nil
	}}
	Dispatch(context.Background(), stubResolver{NameGitHubLandPR: bound}, records.gated(false), NameGitHubLandPR, json.RawMessage(`{}`))
	if prepared {
		t.Fatal("an unadvertised capability was prepared: the gate must refuse before any capability work")
	}
	if message := records.refusal(t); message != "Unsupported client-side tool." {
		t.Fatalf("refusal = %q, want it indistinguishable from an unknown name", message)
	}
	if got := strings.Join(records.steps, ","); got != "allow,respond" {
		t.Fatalf("dispatch order = %s, want the gate consulted before anything else", got)
	}
}

// TestTheInvocationSlotIsClaimedAfterArgumentsValidateAndReleasedLast is the
// ordering internal/mcpbridge's drain depends on. The slot must not be claimed by
// a call that never runs, and it must still be held while the result and the
// terminal event are delivered: a Revoke waiting on the drain wakes when it is
// released and drops anything emitted after that.
func TestTheInvocationSlotIsClaimedAfterArgumentsValidateAndReleasedLast(t *testing.T) {
	var records recorder
	lands := runs(NameGitHubLandPR, true, func(context.Context) (Result, *Failure) {
		return Result{Success: true, Payload: "waiting", Terminal: domain.EventLandingWaiting, Reason: "required checks are pending"}, nil
	})
	Dispatch(context.Background(), stubResolver{NameGitHubLandPR: lands}, records.gated(true), NameGitHubLandPR, json.RawMessage(`{}`))
	want := "allow,enter,item:started,item:completed,respond,emit:landing_waiting,leave"
	if got := strings.Join(records.steps, ","); got != want {
		t.Fatalf("dispatch order = %s, want %s", got, want)
	}
	terminal := records.events[len(records.events)-1]
	if terminal.Kind != domain.EventLandingWaiting || terminal.Message != "required checks are pending" {
		t.Fatalf("terminal event = %+v", terminal)
	}

	var refused recorder
	rejects := stubCapability{name: NameGitHubLandPR, lifecycle: true, prepare: func(json.RawMessage) (Invocation, *Failure) {
		return nil, &Failure{Message: "Unsupported client-side tool.", Outcome: domain.ItemFailed}
	}}
	Dispatch(context.Background(), stubResolver{NameGitHubLandPR: rejects}, refused.gated(true), NameGitHubLandPR, json.RawMessage(`{"unexpected":1}`))
	if got := strings.Join(refused.steps, ","); got != "allow,respond" {
		t.Fatalf("a rejected argument list produced %s, want no slot claimed and no reported call", got)
	}
}

// TestAClosedInvocationSlotRefusesWithoutReportingACall covers the other gate
// outcome: the call never ran, so it is not reported as one, and the transport's
// own refusal text reaches the model unchanged.
func TestAClosedInvocationSlotRefusesWithoutReportingACall(t *testing.T) {
	var records recorder
	transport := records.transport()
	invoked := false
	transport.Enter = func() (func(), *Failure) {
		return nil, &Failure{Message: "Another Symphony tool call is already running."}
	}
	Dispatch(context.Background(), stubResolver{NameGitHubLandPR: runs(NameGitHubLandPR, true, func(context.Context) (Result, *Failure) {
		invoked = true
		return Result{}, nil
	})}, transport, NameGitHubLandPR, json.RawMessage(`{}`))
	if invoked {
		t.Fatal("a refused slot still ran the invocation")
	}
	if message := records.refusal(t); message != "Another Symphony tool call is already running." {
		t.Fatalf("refusal = %q", message)
	}
	if len(records.events) != 0 {
		t.Fatalf("a refused slot emitted %+v", records.events)
	}
}

// TestAFailedInvocationIsFinishedWithTheProvidersOwnOutcome keeps the item
// outcome vocabulary the provider chose: a landing that is already resolved
// declines, everything else fails.
func TestAFailedInvocationIsFinishedWithTheProvidersOwnOutcome(t *testing.T) {
	var records recorder
	Dispatch(context.Background(), stubResolver{NameGitHubLandPR: runs(NameGitHubLandPR, true, func(context.Context) (Result, *Failure) {
		return Result{}, &Failure{Message: "GitHub landing already completed for this run.", Outcome: domain.ItemDeclined}
	})}, records.transport(), NameGitHubLandPR, json.RawMessage(`{}`))
	items := records.items()
	if len(items) != 2 || items[1].Outcome != domain.ItemDeclined {
		t.Fatalf("item records = %+v, want the call finished as declined", items)
	}
	if message := records.refusal(t); message != "GitHub landing already completed for this run." {
		t.Fatalf("refusal = %q", message)
	}
}

// TestAnUnencodableResultIsRefusedAndReportedAsFailed keeps a started call from
// being left outstanding forever when the payload cannot be marshalled: the
// coordinator tracks a started record as this run's outstanding operation until
// its finish arrives. The refusal is deliberately not the unknown-capability
// one -- the invocation already ran, so telling the model the tool does not
// exist would invite another route to work that has already happened -- and the
// terminal outcome is still emitted, so a landing that merged still ends the run
// (PMR-186).
func TestAnUnencodableResultIsRefusedAndReportedAsFailed(t *testing.T) {
	var records recorder
	Dispatch(context.Background(), stubResolver{NameGitHubLandPR: runs(NameGitHubLandPR, true, func(context.Context) (Result, *Failure) {
		return Result{Success: true, Payload: map[string]any{"unencodable": make(chan int)},
			Terminal: domain.EventLandingResolved, Reason: "merged"}, nil
	})}, records.transport(), NameGitHubLandPR, json.RawMessage(`{}`))
	items := records.items()
	if len(items) != 2 || items[1].Outcome != domain.ItemFailed {
		t.Fatalf("item records = %+v, want the call finished as failed", items)
	}
	if message := records.refusal(t); message == "Unsupported client-side tool." || !strings.Contains(message, "already happened") {
		t.Fatalf("refusal = %q, want one that does not deny a tool the model just ran", message)
	}
	want := "item:started,item:failed,respond,emit:landing_resolved"
	if got := strings.Join(records.steps, ","); got != want {
		t.Fatalf("dispatch order = %s, want %s: an unencodable payload must not swallow the terminal outcome", got, want)
	}
}

// TestAbsentNullAndEmptyArgumentsAllReachAZeroArgumentCapability pins the
// normalization both transports now share. Both wire protocols declare the
// argument object optional, and the registry's zero-argument decoder accepts
// only an empty object, so an omitted or null field would refuse
// github_pr_context, github_land_pr, and refresh_base_ref for good. It lived in
// internal/mcpbridge alone and the Codex adapter had already drifted (PMR-186),
// which is the kind of divergence this dispatch exists to make impossible.
//
// The decoder under test is decodeNoInput itself, not a stand-in, because what
// is being pinned is the pair: what the dispatch normalizes and what the
// registry accepts have to agree.
func TestAbsentNullAndEmptyArgumentsAllReachAZeroArgumentCapability(t *testing.T) {
	zeroArgument := stubCapability{name: NameGitHubPRContext, lifecycle: true,
		prepare: func(arguments json.RawMessage) (Invocation, *Failure) {
			if failure := decodeNoInput(arguments); failure != nil {
				return nil, failure
			}
			return succeeds("context"), nil
		}}
	resolver := stubResolver{NameGitHubPRContext: zeroArgument}

	for _, arguments := range []json.RawMessage{nil, json.RawMessage(""), json.RawMessage("null"), json.RawMessage("{}")} {
		var records recorder
		Dispatch(context.Background(), resolver, records.transport(), NameGitHubPRContext, arguments)
		if len(records.outcomes) != 1 {
			t.Fatalf("arguments %q produced %d responses", arguments, len(records.outcomes))
		}
		if outcome := records.outcomes[0]; outcome.Refusal != nil || !outcome.Success || string(outcome.Payload) != `"context"` {
			t.Fatalf("arguments %q reached the capability as %+v, want the invocation to have run", arguments, outcome)
		}
	}

	// The mapping is narrow: content of any kind is still the capability's to
	// reject, so a zero-argument capability keeps refusing a field.
	var records recorder
	Dispatch(context.Background(), resolver, records.transport(), NameGitHubPRContext, json.RawMessage(`{"number":7}`))
	if message := records.refusal(t); message != "Unsupported client-side tool." {
		t.Fatalf("refusal = %q, want an argument list still refused by the capability", message)
	}
}

// TestTheInvocationRunsOnTheContextItWasDispatchedWith keeps the context choice
// with the transport: internal/mcpbridge deliberately runs an invocation on the
// session context rather than the killed child's request context.
func TestTheInvocationRunsOnTheContextItWasDispatchedWith(t *testing.T) {
	type key struct{}
	ctx := context.WithValue(context.Background(), key{}, "session")
	var seen any
	var canceled error
	stopped, cancel := context.WithCancel(ctx)
	cancel()
	var records recorder
	Dispatch(stopped, stubResolver{NameGitHubPRContext: runs(NameGitHubPRContext, true, func(ctx context.Context) (Result, *Failure) {
		seen, canceled = ctx.Value(key{}), ctx.Err()
		return Result{Success: true, Payload: "read"}, nil
	})}, records.transport(), NameGitHubPRContext, json.RawMessage(`{}`))
	if seen != "session" || !errors.Is(canceled, context.Canceled) {
		t.Fatalf("invocation saw value %v and error %v, want the dispatched context verbatim", seen, canceled)
	}
}
