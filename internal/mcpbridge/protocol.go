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
		result, terminal := registration.callTool(call.Params)
		writeResult(w, call.ID, result)
		if terminal != nil {
			// Emitted only after the response is written, mirroring
			// internal/codex: a terminal event ends the logical run, and the
			// model should still receive the result of the call that ended it.
			registration.emit(*terminal)
		}
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

// callTool runs one tools/call against this registration's registry and returns
// the MCP result, plus the terminal event to emit once the response is written.
//
// Every decision about what a capability is, whether it accepts these
// arguments, and what it refuses belongs to the registry. This function owns
// only the transport, the single-invocation gate, and the context the invocation
// runs under.
//
// It emits no item events. The CLI's own stream already reports every MCP call
// as a tool_use/tool_result pair named mcp__symphony__<tool>, which the backend
// already pairs and classifies, so emitting here would double-count the same
// call. That is also why the registry's Lifecycle flag is not consulted: this
// transport has no lifecycle record to suppress.
//
// Nothing decoded from the wire is echoed anywhere -- no log line, no event -- so
// the registry-owned Definition().Name that internal/codex carries forward has
// no consumer here. Any log or event ever added to this function must take the
// name from the resolved capability's own definition, never from the decoded
// request.
func (g *Registration) callTool(params json.RawMessage) (map[string]any, *domain.Event) {
	var call struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &call); err != nil {
		return unsupportedTool(), nil
	}
	bound, ok := g.capabilities.Lookup(call.Name)
	if !ok {
		// Refused as a tool result, not a JSON-RPC error: an unknown name is
		// something the model chose and can correct, and the refusal reveals
		// nothing about what is configured.
		return unsupportedTool(), nil
	}
	invoke, failure := bound.Prepare(arguments(call.Arguments))
	if failure != nil {
		// A rejected argument list precedes the call, so it is not reported as
		// one, and it does not claim the invocation slot.
		return toolFailure(failure.Message), nil
	}
	if err := g.beginCall(); err != nil {
		return toolFailure(gateRefusal(err)), nil
	}
	defer g.endCall()
	// The session context, never the request's: see Register.
	result, failure := invoke(g.sessionCtx)
	if failure != nil {
		return toolFailure(failure.Message), nil
	}
	payload, err := json.Marshal(result.Payload)
	if err != nil {
		return unsupportedTool(), nil
	}
	var terminal *domain.Event
	if result.Terminal != "" {
		// A capability may settle the whole run. Reason is a fixed, bounded
		// string owned by the provider.
		terminal = &domain.Event{Kind: result.Terminal, At: time.Now(), Message: result.Reason}
	}
	return map[string]any{"content": text(string(payload)), "isError": !result.Success}, terminal
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
