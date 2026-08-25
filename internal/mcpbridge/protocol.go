package mcpbridge

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/pmrrasmussen/symphony/internal/domain"
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

// callTool runs one tools/call against this registration's registry, writing the
// MCP result through respond and emitting any terminal outcome the capability
// produced.
//
// Both happen before the deferred endCall releases the invocation slot, and that
// ordering is load-bearing. A Revoke waiting in drain() wakes the moment the
// slot is released and retires the registration; anything emitted after that is
// dropped. A turn cancelled while a landing is in flight is exactly the case
// this transport is built for -- the child is killed, the landing completes,
// reports waiting or merged, and that event is what schedules the delayed retry
// or ends the run. Releasing the slot first would destroy it, and the provider's
// own finalizer would then also do nothing, because it sees the waiting outcome
// the lost event was reporting.
//
// respond runs first so the model still receives the result of the call that
// ended its turn, as it does on the Codex transport.
//
// Every decision about what a capability is, whether it accepts these
// arguments, and what it refuses belongs to the registry. This function owns
// only the transport, the single-invocation gate, and the context the invocation
// runs under.
//
// It emits no item events, and that is a deliberate trade with a known cost. A
// call the agent CLI makes is already reported in the CLI's own stream as a
// tool_use/tool_result pair named mcp__symphony__<tool>, which the backend pairs
// and classifies, so emitting here would double-count it. A call the child makes
// by other means -- its shell holds the endpoint token, and loopback is inside
// its sandbox -- appears in no stream and therefore in no item record at all.
// The advertisement gate below is what bounds that: such a call can only reach a
// capability the model was already permitted to call, so it grants no authority,
// but it does go unrecorded. That is also why the registry's Lifecycle flag is
// not consulted: this transport has no lifecycle record to suppress.
//
// Nothing decoded from the wire is echoed anywhere -- no log line, no event -- so
// the registry-owned Definition().Name that internal/codex carries forward has
// no consumer here. Any log or event ever added to this function must take the
// name from the resolved capability's own definition, never from the decoded
// request.
func (g *Registration) callTool(params json.RawMessage, respond func(map[string]any)) {
	var call struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &call); err != nil {
		respond(unsupportedTool())
		return
	}
	bound, ok := g.capabilities.Lookup(call.Name)
	if !ok || !g.advertises(bound.Definition().Name) {
		// Refused as a tool result, not a JSON-RPC error: an unknown name is
		// something the model chose and can correct, and the refusal reveals
		// nothing about what is configured -- an unadvertised capability is
		// indistinguishable from one that does not exist.
		respond(unsupportedTool())
		return
	}
	invoke, failure := bound.Prepare(arguments(call.Arguments))
	if failure != nil {
		// A rejected argument list precedes the call, so it is not reported as
		// one, and it does not claim the invocation slot.
		respond(toolFailure(failure.Message))
		return
	}
	if err := g.beginCall(); err != nil {
		respond(toolFailure(gateRefusal(err)))
		return
	}
	defer g.endCall()
	// The session context, never the request's: see Register.
	result, failure := invoke(g.sessionCtx)
	if failure != nil {
		respond(toolFailure(failure.Message))
		return
	}
	payload, err := json.Marshal(result.Payload)
	if err != nil {
		respond(unsupportedTool())
		return
	}
	respond(map[string]any{"content": text(string(payload)), "isError": !result.Success})
	if result.Terminal != "" {
		// A capability may settle the whole run. Reason is a fixed, bounded
		// string owned by the provider.
		g.emit(domain.Event{Kind: result.Terminal, At: time.Now(), Message: result.Reason})
	}
}

// advertises reports whether a resolved capability is one this registration
// advertises, which over this transport is the same question as whether the
// agent is allowed to call it.
//
// The registry's own Lookup deliberately ignores advertisement, because on the
// Codex transport advertisement is only a filter over what the model is told
// about: the model can call nothing the app-server did not advertise, so
// dispatch could stay open and let each provider re-validate its own
// preconditions. That reasoning does not survive this transport. The child's
// shell holds the endpoint token and loopback is reachable from inside its
// sandbox, so the child can address this endpoint directly and name a
// capability that never appeared in --tools, in --allowedTools, or in
// tools/list. Provider re-validation still holds -- a landing re-checks the
// tracker state, a follow-up re-checks that creation is enabled -- so what the
// gate closes is not an authority hole but the gap between what the launch
// contract pins as reachable and what actually is. With it, the only
// capabilities reachable by any means are the ones the model was already
// permitted to call, which is what makes the set-equality the launch contract
// checks a statement about reachability rather than only about advertisement.
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

// arguments normalizes an absent argument object. MCP declares "arguments"
// optional, so a zero-argument tool call may legitimately arrive without it,
// while the registry's canonical spelling of "no arguments" is the empty JSON
// object and its decoder refuses absent bytes -- an omitted field would turn
// every zero-argument capability into a permanent refusal. The mapping is a
// transport normalization only: any argument content at all is passed through
// untouched for the capability to validate.
func arguments(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return json.RawMessage("{}")
	}
	return raw
}

// gateRefusal turns a closed invocation slot into text the model can act on.
// Neither message names a capability or a session.
func gateRefusal(err error) string {
	if errors.Is(err, errBusy) {
		return "Another Symphony tool call is already running. Wait for its result before calling another one."
	}
	return "Symphony tools are no longer available for this session."
}

// unsupportedTool is the refusal for an unknown capability and for a call this
// endpoint cannot even shape a result for. It matches the registry's own
// unsupported-capability wording, so an agent cannot distinguish an unknown name
// from a capability that refused to decode its arguments.
func unsupportedTool() map[string]any {
	return toolFailure("Unsupported client-side tool.")
}

// toolFailure is a normal, successful MCP response carrying an error result: the
// model can read the refusal and keep working in the same turn. A JSON-RPC error
// would instead be a client-level transport failure.
func toolFailure(message string) map[string]any {
	return map[string]any{"content": text(message), "isError": true}
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
