package mcpbridge

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/pmrrasmussen/symphony/internal/capability"
)

// The JSON-RPC error codes this endpoint can produce. They are the protocol's
// own, and they are used only for transport-level faults: anything the model
// should read and recover from is a tool result with isError set instead, so a
// refusal reaches the model as text it can act on rather than as a client-level
// MCP failure.
const (
	codeParseError     = -32700
	codeMethodNotFound = -32601
)

// supportedProtocolVersions are the MCP revisions the real client was observed
// to offer. The negotiated version is echoed back when it is one of these, and
// the newest supported one otherwise, which is what the protocol asks a server
// to do for a version it does not implement.
var supportedProtocolVersions = []string{"2024-11-05", "2025-03-26", "2025-06-18"}

// request is the decoded JSON-RPC envelope. ID stays raw and is echoed back
// verbatim: the protocol allows a string or a number, and re-encoding a decoded
// number can change its spelling. A body this decodes from is already bounded,
// so a raw ID cannot be large.
type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// serve handles every request to the endpoint. The refusals come first and in
// this order because each is cheaper and more fundamental than the next: a
// browser-mediated request is rejected before a path is matched, a path before a
// method, and a method before any credential is examined or any body is read.
func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Origin") != "" {
		// The real client sends no Origin, so any request that carries one was
		// mediated by a browser -- a DNS-rebinding attempt against a loopback
		// server by a page the user merely visited. There is no legitimate
		// caller to break by refusing all of them.
		w.WriteHeader(http.StatusForbidden)
		return
	}
	if r.URL.Path != endpointPath {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if r.Method != http.MethodPost {
		// The client probes for a server-to-client SSE stream with GET, and
		// closes a session with DELETE. This endpoint streams nothing and issues
		// no Mcp-Session-Id, so it has neither to offer; the real client accepts
		// the 405 and proceeds with plain POST responses.
		w.Header().Set("Allow", http.MethodPost)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	registration := s.authenticate(bearer(r.Header.Get("Authorization")))
	if registration == nil {
		// Deliberately no WWW-Authenticate challenge: it would invite the
		// client's OAuth discovery flow, which is exactly the machinery a
		// private loopback endpoint must never enter.
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		// An oversized or truncated body never parsed, so there is no JSON-RPC
		// ID to answer and no tool call to refuse with isError. A transport
		// status is the only refusal available.
		var oversize *http.MaxBytesError
		if errors.As(err, &oversize) {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	var call request
	if err := json.Unmarshal(body, &call); err != nil || call.Method == "" {
		writeError(w, http.StatusBadRequest, nil, codeParseError, "malformed request")
		return
	}
	if notification(call.ID) {
		// A notification has no response by definition. notifications/initialized
		// is the only one the client sends, and answering any other with an
		// envelope it has no pending ID for would be a protocol error.
		w.WriteHeader(http.StatusAccepted)
		return
	}
	switch call.Method {
	case "initialize":
		writeResult(w, call.ID, initializeResult(call.Params))
	case "ping":
		writeResult(w, call.ID, map[string]any{})
	case "tools/list":
		writeResult(w, call.ID, map[string]any{"tools": toolList(registration)})
	case "tools/call":
		// The response is written from inside callTool, while the invocation
		// slot is still held: see callTool for why the write and the terminal
		// event cannot happen after the slot is released.
		registration.callTool(call.Params, func(result map[string]any) {
			writeResult(w, call.ID, result)
		})
	default:
		// The client calls an undocumented server/discover before initialize.
		// The protocol's own unknown-method error is the correct answer and the
		// real client proceeds straight to initialize after it.
		writeError(w, http.StatusOK, call.ID, codeMethodNotFound, "method not found")
	}
}

// initializeResult answers the handshake. Only tools are declared: this endpoint
// exposes no resources, prompts, or logging, and offering a capability it does
// not implement would invite calls it can only refuse.
func initializeResult(params json.RawMessage) map[string]any {
	var requested struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	// A missing or unreadable params object is not a handshake failure: the
	// version simply falls back to the newest supported one.
	_ = json.Unmarshal(params, &requested)
	return map[string]any{
		"protocolVersion": negotiate(requested.ProtocolVersion),
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]any{"name": serverName, "version": serverVersion},
	}
}

func negotiate(requested string) string {
	for _, version := range supportedProtocolVersions {
		if version == requested {
			return version
		}
	}
	return supportedProtocolVersions[len(supportedProtocolVersions)-1]
}

// toolList wraps the registry's advertised definitions in the MCP tool envelope.
// Order follows the registry, which is stable and part of the advertised
// contract. The slice is always allocated, so an empty registry advertises an
// empty array rather than a null the client would have to interpret.
func toolList(g *Registration) []map[string]any {
	definitions := g.capabilities.Definitions()
	tools := make([]map[string]any, 0, len(definitions))
	for _, definition := range definitions {
		tools = append(tools, map[string]any{
			"name": definition.Name, "description": definition.Description,
			"inputSchema": definition.InputSchema,
		})
	}
	return tools
}

// callTool runs one tools/call against this registration's registry through the
// shared capability dispatch, which owns the whole lookup-to-terminal-event
// sequence for both of Symphony's agent transports. What this endpoint keeps is
// what is genuinely its own: the MCP request and result envelopes, the
// advertisement gate, the single-invocation gate, and the context an invocation
// runs under.
//
// Both the result and any terminal event the capability produced are delivered
// before the deferred endCall releases the invocation slot, and that ordering is
// load-bearing: a Revoke waiting in drain() wakes the moment the slot is
// released, and anything emitted after that is dropped. Dispatch responds before
// it emits and runs the release last, which is why the gate is handed to it as
// Enter rather than taken around it.
//
// Nothing decoded from the wire is echoed into the item records this produces.
// The name is used only to select a capability, and the call ID is minted here
// rather than taken from the request's JSON-RPC ID, which the untrusted child
// chose. Any log or event ever added to this file must take the same care. See
// docs/architecture.md's "The loopback MCP endpoint" section for what those item
// records duplicate and what they are the only record of.
func (g *Registration) callTool(params json.RawMessage, respond func(map[string]any)) {
	var call struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &call); err != nil {
		respond(toolResult(capability.Outcome{Refusal: capability.Unsupported()}))
		return
	}
	// The session context, never the request's: see Register. The argument object
	// is handed over exactly as it arrived, absent field and all: MCP declares it
	// optional, but whether an absent one is refused is refusal semantics, so the
	// mapping onto the registry's empty object belongs to the shared dispatch,
	// where both transports get it (PMR-186).
	capability.Dispatch(g.sessionCtx, g.capabilities, capability.Transport{
		CallID:  nextCallID(),
		Respond: func(outcome capability.Outcome) { respond(toolResult(outcome)) },
		Emit:    g.emit,
		Allow:   g.advertises,
		Enter:   g.enterCall,
	}, call.Name, call.Arguments)
}

// enterCall claims the single invocation slot for one dispatched call and
// returns the release that runs once that call has been answered and its
// terminal event emitted.
func (g *Registration) enterCall() (func(), *capability.Failure) {
	if err := g.beginCall(); err != nil {
		return nil, &capability.Failure{Message: gateRefusal(err)}
	}
	return g.endCall, nil
}

// nextCallID mints the identity one dispatched call is reported under. It is
// host-side and process-wide: an ID taken from the request's JSON-RPC envelope
// would be a value the child chose reaching an event, and a per-registration
// counter would repeat itself every turn, so a stale outstanding operation could
// be cleared by the next turn's first call.
func nextCallID() string {
	return "mcp-call-" + strconv.FormatUint(callSequence.Add(1), 10)
}

var callSequence atomic.Uint64

// advertises reports whether a resolved capability is one this registration
// advertises, which over this transport is the same question as whether the
// agent is allowed to call it. It is the Allow gate the shared dispatch consults
// after a name resolves and before any provider work, and a name it refuses is
// answered exactly as an unknown one is.
//
// The registry's own Lookup deliberately ignores advertisement, because on the
// Codex transport the model can call nothing the app-server did not advertise.
// That reasoning does not survive this transport: the child can address this
// endpoint directly and name a capability that appeared in no flag and in no
// tools/list. See docs/architecture.md's "The advertisement gate".
//
// The membership test walks Definitions() rather than caching it, because a
// registry's advertised set is fixed when it is built and the set is at most a
// handful of entries.
func (g *Registration) advertises(name string) bool {
	for _, definition := range g.capabilities.Definitions() {
		if definition.Name == name {
			return true
		}
	}
	return false
}

// gateRefusal turns a closed invocation slot into text the model can act on.
// Neither message names a capability or a session.
func gateRefusal(err error) string {
	if errors.Is(err, errBusy) {
		return "Another Symphony tool call is already running. Wait for its result before calling another one."
	}
	return "Symphony tools are no longer available for this session."
}

// toolResult frames one dispatched outcome in the MCP tool-result envelope.
//
// A refusal is a normal, successful MCP response carrying an error result: the
// model can read it and keep working in the same turn, where a JSON-RPC error
// would instead be a client-level transport failure. The refusal text is the
// registry's own, so an unknown name, an unadvertised capability, and a call
// this endpoint could not shape a result for stay indistinguishable.
func toolResult(outcome capability.Outcome) map[string]any {
	if outcome.Refusal != nil {
		return map[string]any{"content": text(outcome.Refusal.Message), "isError": true}
	}
	return map[string]any{"content": text(string(outcome.Payload)), "isError": !outcome.Success}
}

func text(value string) []any {
	return []any{map[string]any{"type": "text", "text": value}}
}

// notification reports whether a decoded envelope is a notification rather than
// a request. An absent ID is the protocol's own marker; an explicit null is
// treated the same way, because there is no response an ID of null could be
// matched to either.
func notification(id json.RawMessage) bool {
	return len(id) == 0 || string(id) == "null"
}

// bearer extracts the credential from an Authorization header. The scheme is
// compared case-insensitively, as the HTTP grammar requires.
func bearer(header string) string {
	const scheme = "Bearer "
	if len(header) <= len(scheme) || !strings.EqualFold(header[:len(scheme)], scheme) {
		return ""
	}
	return header[len(scheme):]
}

func writeResult(w http.ResponseWriter, id json.RawMessage, result any) {
	writeEnvelope(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func writeError(w http.ResponseWriter, status int, id json.RawMessage, code int, message string) {
	if id == nil {
		id = json.RawMessage("null")
	}
	writeEnvelope(w, status, map[string]any{"jsonrpc": "2.0", "id": id,
		"error": map[string]any{"code": code, "message": message}})
}

// writeEnvelope marshals before writing any header, so an encoding failure
// becomes a clean 500 instead of a truncated 200 body that the client would
// have to interpret as a malformed response.
//
// The response is plain application/json. The real client accepts it and
// requires no SSE stream, and no Mcp-Session-Id is issued: this endpoint keeps
// no protocol-level session state, so a session header would only commit it to
// honouring session semantics it does not implement.
func writeEnvelope(w http.ResponseWriter, status int, envelope map[string]any) {
	body, err := json.Marshal(envelope)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
