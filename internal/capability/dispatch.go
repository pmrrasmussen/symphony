package capability

import (
	"context"
	"encoding/json"
	"time"

	"github.com/pmrrasmussen/symphony/internal/domain"
)

// dynamicToolCallItem is the item type every bounded capability call is reported
// under, whichever transport carried it. It is the Codex app-server's own
// vocabulary for a client-side tool call, and the MCP endpoint reports its calls
// under the same name on purpose: one log query then answers "what was this
// session's capability call doing" for either backend.
const dynamicToolCallItem = "dynamicToolCall"

// Resolver is the single registry operation a dispatch needs. *Registry
// satisfies it, and so does a transport's own narrower seam over one, which is
// why Dispatch is a function over this interface rather than a method on
// Registry: internal/mcpbridge serves the registry through its own Capabilities
// interface, and a method would force every substitute for that seam to carry a
// second copy of the algorithm below.
type Resolver interface {
	Lookup(name string) (Capability, bool)
}

var _ Resolver = (*Registry)(nil)

// Outcome is one dispatched call's result in the form a transport still has to
// frame: a marshalled payload plus the success flag, or a refusal instead of
// both. Nothing here is a wire envelope -- how a payload or a refusal message is
// wrapped is the only thing the two transports still decide for themselves.
//
// A terminal outcome is deliberately not part of this. Ending the whole run is
// not something a transport frames, it is an event that must be emitted after
// the call has been answered and, on a gated transport, before the gate is
// released -- an ordering that only holds if Dispatch owns it. See Transport.Emit.
type Outcome struct {
	Payload []byte
	Success bool
	// Refusal is set instead of the two fields above. Message is what the model
	// is told; it never carries a provider error or any credential-derived value.
	Refusal *Failure
}

// Transport is everything about running one capability call that belongs to the
// agent transport carrying it rather than to the registry. Respond and Emit are
// required; Allow and Enter are the two gates only one transport has.
type Transport struct {
	// CallID identifies this call in the item records Dispatch emits for a
	// capability whose Lifecycle reports it as observable. The transport chooses
	// it because only the transport has one: the Codex adapter uses the
	// protocol-assigned JSON-RPC request ID, and the MCP endpoint mints its own
	// rather than echo an ID an untrusted child chose.
	CallID string
	// Respond frames one result in this transport's own envelope and writes it.
	// Dispatch calls it exactly once per call, and always before any terminal
	// event, so a capability that settles the run still answers the model that
	// called it.
	Respond func(Outcome)
	// Emit delivers this call's item records and any terminal outcome to the
	// session's event stream.
	Emit func(domain.Event)
	// Allow, when set, reports whether this transport permits a call to the
	// named capability at all. A refusal is answered exactly as an unknown name
	// is, so a capability this transport withholds stays indistinguishable from
	// one that does not exist. The name passed is the resolved capability's own,
	// never the value decoded from the wire.
	//
	// It exists for internal/mcpbridge, whose advertisement gate is a real
	// transport-level authority question: the child's shell can address that
	// endpoint directly and name a capability that was never advertised. The
	// Codex adapter sets nothing here, because there the model can only call what
	// the app-server was told about.
	Allow func(name string) bool
	// Enter, when set, claims this transport's invocation slot and returns the
	// release to run once the call has been answered and its terminal event
	// emitted. A refusal here precedes the invocation, so the call is never
	// reported as started.
	//
	// It is consulted after arguments validate, not before: a rejected argument
	// list must not claim a slot that a concurrent call could have used. It
	// exists for internal/mcpbridge, which serves concurrent HTTP requests per
	// session; the Codex adapter dispatches inline from its read loop and has
	// nothing to gate.
	Enter func() (leave func(), refusal *Failure)
}

// Unsupported is the refusal a transport answers a call it could not decode at
// all with -- an envelope that is not shaped like a call names no capability, so
// it is refused exactly as an unknown name is. It is the registry's own
// unknown-capability refusal, so the two cannot drift apart.
func Unsupported() *Failure { return unsupported() }

// Dispatch runs one bounded capability call: resolve the name, apply the
// transport's own gates, validate the arguments, invoke, marshal, answer, and
// emit whatever the outcome owes the session's event stream. It is the only
// implementation of that sequence -- both the Codex app-server adapter and the
// private MCP endpoint call it -- so the two transports cannot drift on ordering,
// on what a refusal looks like, or on which calls are reported as items. They
// did drift before this existed: only one of them honoured Lifecycle, and a
// Claude session's landing round trip was therefore absent from the log where
// the identical Codex call was visible with a duration.
//
// Every decision about what a capability is, whether it accepts these arguments,
// and what it refuses belongs to the registry and to the provider behind it.
// Nothing decoded from the wire is echoed into a log or an event: name is used
// only to select a capability, and every record below carries the resolved
// capability's own registry-owned Definition().Name instead.
func Dispatch(ctx context.Context, resolver Resolver, transport Transport, name string, arguments json.RawMessage) {
	bound, ok := resolver.Lookup(name)
	if !ok {
		// No transport binds anything beyond this registry, so an unresolved name
		// is a capability that does not exist for this session -- the agent has no
		// Linear state-transition capability, whatever it asks for.
		transport.Respond(Outcome{Refusal: unsupported()})
		return
	}
	// The capability's own name is used from here on, never the decoded wire
	// value, so nothing an agent chooses can reach a log or an event.
	resolved := bound.Definition().Name
	if transport.Allow != nil && !transport.Allow(resolved) {
		transport.Respond(Outcome{Refusal: unsupported()})
		return
	}
	invoke, failure := bound.Prepare(arguments)
	if failure != nil {
		// A rejected argument list precedes the call, so it is not reported as one.
		transport.Respond(Outcome{Refusal: failure})
		return
	}
	if transport.Enter != nil {
		leave, refusal := transport.Enter()
		if refusal != nil {
			transport.Respond(Outcome{Refusal: refusal})
			return
		}
		defer leave()
	}
	call := lifecycle{transport: transport, name: resolved, observed: bound.Lifecycle()}
	call.started()
	result, failure := invoke(ctx)
	if failure != nil {
		call.finished(failure.Outcome)
		transport.Respond(Outcome{Refusal: failure})
		return
	}
	payload, err := json.Marshal(result.Payload)
	if err != nil {
		call.finished(domain.ItemFailed)
		transport.Respond(Outcome{Refusal: unsupported()})
		return
	}
	call.finished(domain.ItemCompleted)
	transport.Respond(Outcome{Payload: payload, Success: result.Success})
	if result.Terminal != "" {
		// A capability may settle the whole run. Reason is a fixed, bounded string
		// owned by the provider.
		transport.Emit(domain.Event{Kind: result.Terminal, At: time.Now(), Message: result.Reason})
	}
}

// lifecycle reports one call's start and finish as dynamicToolCall item records,
// so a slow provider round trip is visible the same way a native app-server item
// is. Whether a call is reported at all is the capability's own answer
// (Capability.Lifecycle) and is read here only -- a single bounded tracker round
// trip has no outstanding operation worth surfacing, and reporting one would also
// report provider-side argument validation as a started call.
//
// The record carries the registry-owned capability name, the transport's call ID,
// an outcome, and a Symphony-measured duration. Arguments, results, and provider
// errors have no field to reach.
type lifecycle struct {
	transport Transport
	name      string
	observed  bool
	at        time.Time
}

func (l *lifecycle) started() {
	if !l.observed {
		return
	}
	l.at = time.Now()
	l.transport.Emit(domain.Event{Kind: domain.EventItem, At: l.at, ItemID: l.transport.CallID,
		ItemType: dynamicToolCallItem, ToolName: l.name, Outcome: domain.ItemStarted})
}

func (l *lifecycle) finished(outcome string) {
	if !l.observed {
		return
	}
	l.transport.Emit(domain.Event{Kind: domain.EventItem, At: time.Now(), ItemID: l.transport.CallID,
		ItemType: dynamicToolCallItem, ToolName: l.name, Outcome: outcome,
		DurationMs: time.Since(l.at).Milliseconds()})
}
