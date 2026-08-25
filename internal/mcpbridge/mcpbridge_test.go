package mcpbridge

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pmrrasmussen/symphony/internal/capability"
	"github.com/pmrrasmussen/symphony/internal/domain"
)

// stubCapability is a bound capability whose argument validation and invocation
// are supplied by the test. A real capability reaches a provider session, so the
// endpoint's own behaviour -- serialization, drain ordering, refusal shapes, the
// context an invocation runs on -- is only observable through a capability the
// test controls.
type stubCapability struct {
	definition capability.Definition
	prepare    func(json.RawMessage) (capability.Invocation, *capability.Failure)
}

func (c stubCapability) Definition() capability.Definition { return c.definition }
func (c stubCapability) Lifecycle() bool                   { return true }
func (c stubCapability) Prepare(arguments json.RawMessage) (capability.Invocation, *capability.Failure) {
	return c.prepare(arguments)
}

// stubRegistry stands in for *capability.Registry, which cannot be assembled
// around a test capability from outside its own package. Definitions and
// bindings are fixed before a test starts serving, so only the turn-ended
// record needs guarding.
type stubRegistry struct {
	definitions []capability.Definition
	bound       map[string]capability.Capability

	mu          sync.Mutex
	turnEnded   int
	onTurnEnded func()
}

func newRegistry() *stubRegistry {
	return &stubRegistry{bound: map[string]capability.Capability{}}
}

func (s *stubRegistry) Definitions() []capability.Definition { return s.definitions }

func (s *stubRegistry) Lookup(name string) (capability.Capability, bool) {
	bound, ok := s.bound[name]
	return bound, ok
}

func (s *stubRegistry) TurnEnded(context.Context) {
	s.mu.Lock()
	s.turnEnded++
	hook := s.onTurnEnded
	s.mu.Unlock()
	if hook != nil {
		hook()
	}
}

func (s *stubRegistry) turnEndedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.turnEnded
}

// with advertises a capability that accepts any arguments and runs invoke.
func (s *stubRegistry) with(name string, invoke capability.Invocation) *stubRegistry {
	return s.withPrepare(name, func(json.RawMessage) (capability.Invocation, *capability.Failure) {
		return invoke, nil
	})
}

func (s *stubRegistry) withPrepare(name string, prepare func(json.RawMessage) (capability.Invocation, *capability.Failure)) *stubRegistry {
	definition := capability.Definition{
		Name:        name,
		Description: name + " description",
		InputSchema: map[string]any{"type": "object", "additionalProperties": false},
	}
	s.definitions = append(s.definitions, definition)
	s.bound[name] = stubCapability{definition: definition, prepare: prepare}
	return s
}

func succeeds(payload any) capability.Invocation {
	return func(context.Context) (capability.Result, *capability.Failure) {
		return capability.Result{Success: true, Payload: payload}, nil
	}
}

// recorder collects everything a registration emits so a test can assert both
// what was emitted and, more importantly, what was not.
type recorder struct {
	mu     sync.Mutex
	events []domain.Event
}

func (r *recorder) sink(event domain.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

func (r *recorder) all() []domain.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]domain.Event(nil), r.events...)
}

func endpoint(t *testing.T) *Server {
	t.Helper()
	server, err := Listen()
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = server.Close(context.Background()) })
	return server
}

func register(t *testing.T, server *Server, registry Capabilities) (*Registration, *recorder) {
	t.Helper()
	events := &recorder{}
	registration, err := server.Register(context.Background(), registry, events.sink)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	return registration, events
}

// send issues one request with the exact headers claude 2.1.245 was observed to
// send, so the handshake under test is the one the real client performs.
func send(t *testing.T, method, url, token, body string) (*http.Response, []byte) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatalf("build %s request: %v", method, err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer func() { _ = response.Body.Close() }()
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	return response, payload
}

func rpc(t *testing.T, g *Registration, token, method, params string) map[string]any {
	t.Helper()
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":7,"method":%q`, method)
	if params != "" {
		body += ",\"params\":" + params
	}
	body += "}"
	response, payload := send(t, http.MethodPost, g.URL(), token, body)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("%s returned status %d, want 200 (%s)", method, response.StatusCode, payload)
	}
	var envelope map[string]any
	if err := json.Unmarshal(payload, &envelope); err != nil {
		t.Fatalf("decode %s response %s: %v", method, payload, err)
	}
	return envelope
}

func result(t *testing.T, envelope map[string]any) map[string]any {
	t.Helper()
	if _, present := envelope["error"]; present {
		t.Fatalf("response carried a JSON-RPC error: %v", envelope)
	}
	value, ok := envelope["result"].(map[string]any)
	if !ok {
		t.Fatalf("response carried no result object: %v", envelope)
	}
	return value
}

// toolOutcome extracts the single text block and the error flag from a
// tools/call result.
func toolOutcome(t *testing.T, envelope map[string]any) (string, bool) {
	t.Helper()
	value := result(t, envelope)
	isError, ok := value["isError"].(bool)
	if !ok {
		t.Fatalf("tool result carried no isError flag: %v", value)
	}
	content, ok := value["content"].([]any)
	if !ok || len(content) != 1 {
		t.Fatalf("tool result carried %v, want exactly one content block", value["content"])
	}
	block, ok := content[0].(map[string]any)
	if !ok || block["type"] != "text" {
		t.Fatalf("tool result content block is %v, want a text block", content[0])
	}
	text, ok := block["text"].(string)
	if !ok {
		t.Fatalf("tool result text is %v, want a string", block["text"])
	}
	return text, isError
}

func callTool(t *testing.T, g *Registration, token, name string) map[string]any {
	t.Helper()
	return rpc(t, g, token, "tools/call", fmt.Sprintf(`{"name":%q,"arguments":{}}`, name))
}

// TestHandshakeListingAndInvocation drives the whole session the real client
// performs, in order, with its own headers.
func TestHandshakeListingAndInvocation(t *testing.T) {
	registry := newRegistry().
		with("first_tool", succeeds(map[string]any{"ok": true})).
		with("second_tool", succeeds("plain"))
	server := endpoint(t)
	registration, events := register(t, server, registry)

	initialized := result(t, rpc(t, registration, registration.Token(), "initialize",
		`{"protocolVersion":"2025-06-18","clientInfo":{"name":"claude","version":"2.1.245"}}`))
	if initialized["protocolVersion"] != "2025-06-18" {
		t.Fatalf("negotiated %v, want the requested 2025-06-18", initialized["protocolVersion"])
	}
	capabilities, ok := initialized["capabilities"].(map[string]any)
	if !ok || capabilities["tools"] == nil {
		t.Fatalf("initialize declared %v, want a tools capability", initialized["capabilities"])
	}
	info, ok := initialized["serverInfo"].(map[string]any)
	if !ok || info["name"] != serverName {
		t.Fatalf("initialize reported serverInfo %v", initialized["serverInfo"])
	}

	response, payload := send(t, http.MethodPost, registration.URL(), registration.Token(),
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if response.StatusCode != http.StatusAccepted || len(payload) != 0 {
		t.Fatalf("notifications/initialized returned %d %q, want 202 with no body", response.StatusCode, payload)
	}
	if got := response.Header.Get("Mcp-Session-Id"); got != "" {
		t.Fatalf("endpoint issued an Mcp-Session-Id of %q", got)
	}

	if pinged := result(t, rpc(t, registration, registration.Token(), "ping", "")); len(pinged) != 0 {
		t.Fatalf("ping returned %v, want an empty result", pinged)
	}

	listed := result(t, rpc(t, registration, registration.Token(), "tools/list", "{}"))
	tools, ok := listed["tools"].([]any)
	if !ok || len(tools) != len(registry.Definitions()) {
		t.Fatalf("tools/list returned %v, want %d tools", listed["tools"], len(registry.Definitions()))
	}
	for i, definition := range registry.Definitions() {
		advertised, ok := tools[i].(map[string]any)
		if !ok {
			t.Fatalf("tool %d is %v", i, tools[i])
		}
		if advertised["name"] != definition.Name || advertised["description"] != definition.Description {
			t.Fatalf("tool %d advertised %v, want %s in registry order", i, advertised, definition.Name)
		}
		if advertised["inputSchema"] == nil {
			t.Fatalf("tool %d advertised no input schema", i)
		}
	}

	text, isError := toolOutcome(t, callTool(t, registration, registration.Token(), "first_tool"))
	if isError || text != `{"ok":true}` {
		t.Fatalf("tools/call returned %q (isError %v), want the marshaled payload", text, isError)
	}
	if emitted := events.all(); len(emitted) != 0 {
		t.Fatalf("a successful call emitted %v; the CLI's own stream already reports it", emitted)
	}
}

// TestProtocolVersionNegotiation pins every version the real client was seen to
// offer, and the fallback for one this endpoint does not implement.
func TestProtocolVersionNegotiation(t *testing.T) {
	registration, _ := register(t, endpoint(t), newRegistry())
	for _, tc := range []struct{ requested, want string }{
		{"2024-11-05", "2024-11-05"},
		{"2025-03-26", "2025-03-26"},
		{"2025-06-18", "2025-06-18"},
		{"1999-01-01", "2025-06-18"},
		{"", "2025-06-18"},
	} {
		params := fmt.Sprintf(`{"protocolVersion":%q}`, tc.requested)
		negotiated := result(t, rpc(t, registration, registration.Token(), "initialize", params))
		if negotiated["protocolVersion"] != tc.want {
			t.Fatalf("requested %q, negotiated %v, want %q", tc.requested, negotiated["protocolVersion"], tc.want)
		}
	}
}

// TestServerDiscoverIsAMethodNotFoundError covers the undocumented probe the
// client issues before initialize. The real client proceeds past the protocol's
// own unknown-method error.
func TestServerDiscoverIsAMethodNotFoundError(t *testing.T) {
	registration, _ := register(t, endpoint(t), newRegistry())
	envelope := rpc(t, registration, registration.Token(), "server/discover", "{}")
	failure, ok := envelope["error"].(map[string]any)
	if !ok {
		t.Fatalf("server/discover returned %v, want a JSON-RPC error", envelope)
	}
	if code, _ := failure["code"].(float64); int(code) != codeMethodNotFound {
		t.Fatalf("server/discover returned code %v, want %d", failure["code"], codeMethodNotFound)
	}
	if _, present := envelope["result"]; present {
		t.Fatalf("server/discover returned both a result and an error: %v", envelope)
	}
}

// TestRefusalsAreToolErrorsNotTransportErrors pins the boundary between the two
// kinds of failure: anything the model can recover from arrives as a tool result
// with isError set, never as a JSON-RPC error or an HTTP status.
func TestRefusalsAreToolErrorsNotTransportErrors(t *testing.T) {
	refuse := func(json.RawMessage) (capability.Invocation, *capability.Failure) {
		return nil, &capability.Failure{Message: "arguments were rejected.", Outcome: domain.ItemFailed}
	}
	registry := newRegistry().
		withPrepare("refuses_arguments", refuse).
		with("fails", func(context.Context) (capability.Result, *capability.Failure) {
			return capability.Result{}, &capability.Failure{Message: "provider refused.", Outcome: domain.ItemFailed}
		}).
		with("returns_unsuccessful", func(context.Context) (capability.Result, *capability.Failure) {
			return capability.Result{Success: false, Payload: map[string]any{"why": "gate"}}, nil
		})
	registration, events := register(t, endpoint(t), registry)

	for _, tc := range []struct{ name, want string }{
		{"refuses_arguments", "arguments were rejected."},
		{"fails", "provider refused."},
		{"returns_unsuccessful", `{"why":"gate"}`},
		{"never_registered", "Unsupported client-side tool."},
	} {
		text, isError := toolOutcome(t, callTool(t, registration, registration.Token(), tc.name))
		if !isError || text != tc.want {
			t.Fatalf("%s returned %q (isError %v), want %q with isError true", tc.name, text, isError, tc.want)
		}
	}
	if emitted := events.all(); len(emitted) != 0 {
		t.Fatalf("refusals emitted %v", emitted)
	}
	// A refusal that precedes the call must not have claimed the invocation
	// slot, or one bad argument list would close the session's tools.
	if _, isError := toolOutcome(t, callTool(t, registration, registration.Token(), "returns_unsuccessful")); !isError {
		t.Fatal("a later call did not run after earlier refusals")
	}
}

// TestNonPostMethodsAreRefused covers the client's SSE stream probe and session
// teardown, neither of which this endpoint offers.
func TestNonPostMethodsAreRefused(t *testing.T) {
	registration, _ := register(t, endpoint(t), newRegistry())
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		response, _ := send(t, method, registration.URL(), registration.Token(), "")
		if response.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("%s returned %d, want 405", method, response.StatusCode)
		}
		if allow := response.Header.Get("Allow"); allow != http.MethodPost {
			t.Fatalf("%s returned Allow %q, want POST", method, allow)
		}
	}
}

// TestUnauthorizedTokensAreRefused covers every way a caller can fail to prove
// it owns a registration.
func TestUnauthorizedTokensAreRefused(t *testing.T) {
	server := endpoint(t)
	registration, _ := register(t, server, newRegistry().with("tool", succeeds("ok")))
	revoked, _ := register(t, server, newRegistry().with("tool", succeeds("ok")))
	revoked.Revoke(context.Background())

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`
	for _, tc := range []struct{ name, header string }{
		{"missing", ""},
		{"unknown", "Bearer " + strings.Repeat("a", len(registration.Token()))},
		{"truncated", "Bearer " + registration.Token()[:len(registration.Token())-1]},
		{"revoked", "Bearer " + revoked.Token()},
	} {
		request, err := http.NewRequest(http.MethodPost, registration.URL(), strings.NewReader(body))
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		request.Header.Set("Content-Type", "application/json")
		if tc.header != "" {
			request.Header.Set("Authorization", tc.header)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatalf("%s token: %v", tc.name, err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s token returned %d, want 401", tc.name, response.StatusCode)
		}
		// An OAuth challenge would send the client into a discovery flow this
		// endpoint must never take part in.
		if challenge := response.Header.Get("Www-Authenticate"); challenge != "" {
			t.Fatalf("401 carried an authentication challenge %q", challenge)
		}
	}
	// A wrong scheme is not a credential either.
	response, _ := send(t, http.MethodPost, registration.URL(), "", body)
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("schemeless request returned %d, want 401", response.StatusCode)
	}
}

// TestOneSessionsTokenCannotReachAnotherSessionsRegistry is the isolation that
// makes a single shared listener safe: a token resolves to exactly one registry,
// so a session can neither list nor invoke another session's capabilities.
func TestOneSessionsTokenCannotReachAnotherSessionsRegistry(t *testing.T) {
	server := endpoint(t)
	first, _ := register(t, server, newRegistry().with("first_only", succeeds("first")))
	second, _ := register(t, server, newRegistry().with("second_only", succeeds("second")))

	listed := result(t, rpc(t, first, first.Token(), "tools/list", "{}"))
	tools, _ := listed["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("the first session was told about %v", listed["tools"])
	}
	if advertised, _ := tools[0].(map[string]any); advertised["name"] != "first_only" {
		t.Fatalf("the first session was told about %v", tools[0])
	}

	// The second session's own tool, invoked with the first session's token.
	text, isError := toolOutcome(t, callTool(t, first, first.Token(), "second_only"))
	if !isError || text != "Unsupported client-side tool." {
		t.Fatalf("cross-session call returned %q (isError %v), want an unsupported-tool refusal", text, isError)
	}
	if text, isError := toolOutcome(t, callTool(t, second, second.Token(), "second_only")); isError || text != `"second"` {
		t.Fatalf("the owning session's call returned %q (isError %v)", text, isError)
	}
}

// TestMalformedAndOversizeBodiesAreRefused covers the two bodies that never
// parse, so there is no JSON-RPC id to answer and no call to refuse.
func TestMalformedAndOversizeBodiesAreRefused(t *testing.T) {
	registration, _ := register(t, endpoint(t), newRegistry().with("tool", succeeds("ok")))

	response, payload := send(t, http.MethodPost, registration.URL(), registration.Token(), "{not json")
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed body returned %d, want 400", response.StatusCode)
	}
	var envelope map[string]any
	if err := json.Unmarshal(payload, &envelope); err != nil {
		t.Fatalf("malformed body returned %q, want a JSON-RPC error envelope", payload)
	}
	failure, ok := envelope["error"].(map[string]any)
	if !ok {
		t.Fatalf("malformed body returned %v", envelope)
	}
	if code, _ := failure["code"].(float64); int(code) != codeParseError {
		t.Fatalf("malformed body returned code %v, want %d", failure["code"], codeParseError)
	}
	if envelope["id"] != nil {
		t.Fatalf("malformed body was answered with id %v, want null", envelope["id"])
	}

	oversize := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"tool","arguments":{"pad":%q}}}`,
		strings.Repeat("p", maxBodyBytes))
	response, _ = send(t, http.MethodPost, registration.URL(), registration.Token(), oversize)
	if response.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize body returned %d, want 413", response.StatusCode)
	}
	// The endpoint still serves the session afterwards.
	if _, isError := toolOutcome(t, callTool(t, registration, registration.Token(), "tool")); isError {
		t.Fatal("a call after an oversize body was refused")
	}
}

// TestRequestCarryingOriginIsRefused guards against DNS rebinding: the real
// client sends no Origin, so a request that carries one was mediated by a
// browser and has no legitimate caller to break.
func TestRequestCarryingOriginIsRefused(t *testing.T) {
	registration, _ := register(t, endpoint(t), newRegistry().with("tool", succeeds("ok")))
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"tool","arguments":{}}}`
	request, err := http.NewRequest(http.MethodPost, registration.URL(), strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+registration.Token())
	request.Header.Set("Origin", "https://attacker.example")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("origin request: %v", err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("a request carrying Origin returned %d, want 403", response.StatusCode)
	}
}

func TestUnknownPathIsNotFound(t *testing.T) {
	registration, _ := register(t, endpoint(t), newRegistry())
	response, _ := send(t, http.MethodPost, registration.URL()+"/extra", registration.Token(),
		`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown path returned %d, want 404", response.StatusCode)
	}
}

// TestConcurrentCallsSerializePerRegistration is the invariant every provider
// idempotency latch behind the registry depends on: one invocation at a time,
// and a parallel second call refused rather than queued.
func TestConcurrentCallsSerializePerRegistration(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var mu sync.Mutex
	var running, peak int
	slow := func(context.Context) (capability.Result, *capability.Failure) {
		mu.Lock()
		running++
		if running > peak {
			peak = running
		}
		mu.Unlock()
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		mu.Lock()
		running--
		mu.Unlock()
		return capability.Result{Success: true, Payload: "done"}, nil
	}
	registration, _ := register(t, endpoint(t), newRegistry().with("slow", slow))

	first := asyncCall(t, registration, "slow")
	awaitSignal(t, entered, "the first invocation to start")

	var wg sync.WaitGroup
	refusals := make(chan string, 3)
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			text, isError := toolOutcome(t, callTool(t, registration, registration.Token(), "slow"))
			if !isError {
				refusals <- "a parallel call ran instead of being refused"
				return
			}
			if !strings.Contains(text, "already running") {
				refusals <- "a parallel call was refused with " + text
			}
		}()
	}
	wg.Wait()
	close(refusals)
	for problem := range refusals {
		t.Fatal(problem)
	}

	close(release)
	if text, isError := toolOutcome(t, await(t, first, "the in-flight call")); isError || text != `"done"` {
		t.Fatalf("the in-flight call returned %q (isError %v)", text, isError)
	}
	mu.Lock()
	observed := peak
	mu.Unlock()
	if observed != 1 {
		t.Fatalf("%d invocations ran at once, want 1", observed)
	}
	// The slot is released, so the session keeps working.
	if _, isError := toolOutcome(t, callTool(t, registration, registration.Token(), "slow")); isError {
		t.Fatal("the invocation slot was not released")
	}
}

// TestRevokeDrainsInFlightWorkBeforeTurnEnded pins the ordering the deferred
// Merging -> In Review transition depends on: the finalizer must not run while a
// landing call is still in flight.
func TestRevokeDrainsInFlightWorkBeforeTurnEnded(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var mu sync.Mutex
	returned := false
	finalizedAfterReturn := false

	registry := newRegistry().with("slow", func(context.Context) (capability.Result, *capability.Failure) {
		close(entered)
		<-release
		mu.Lock()
		returned = true
		mu.Unlock()
		return capability.Result{Success: true, Payload: "done"}, nil
	})
	registry.onTurnEnded = func() {
		mu.Lock()
		finalizedAfterReturn = returned
		mu.Unlock()
	}
	registration, _ := register(t, endpoint(t), registry)

	call := asyncCall(t, registration, "slow")
	awaitSignal(t, entered, "the invocation to start")

	revoked := make(chan struct{})
	go func() {
		registration.Revoke(context.Background())
		close(revoked)
	}()

	// Revoke marks the registration inactive before it drains, so the token
	// stops authenticating immediately even though the call is still running.
	waitFor(t, func() bool {
		response, _ := send(t, http.MethodPost, registration.URL(), registration.Token(),
			`{"jsonrpc":"2.0","id":1,"method":"ping"}`)
		return response.StatusCode == http.StatusUnauthorized
	}, "the revoked token to stop authenticating")
	if count := registry.turnEndedCount(); count != 0 {
		t.Fatalf("the turn-ended finalizer ran %d times while a call was in flight", count)
	}

	close(release)
	awaitSignal(t, revoked, "Revoke to return")
	await(t, call, "the drained call")
	mu.Lock()
	ordered := finalizedAfterReturn
	mu.Unlock()
	if !ordered {
		t.Fatal("the turn-ended finalizer ran before the in-flight invocation returned")
	}
	if count := registry.turnEndedCount(); count != 1 {
		t.Fatalf("the turn-ended finalizer ran %d times, want 1", count)
	}
	// Revoke is idempotent.
	registration.Revoke(context.Background())
	if count := registry.turnEndedCount(); count != 1 {
		t.Fatalf("a second Revoke ran the finalizer again (%d times)", count)
	}
}

// TestTerminalResultReachesTheOwningSink covers the only event this endpoint
// emits.
func TestTerminalResultReachesTheOwningSink(t *testing.T) {
	registry := newRegistry().with("lands", func(context.Context) (capability.Result, *capability.Failure) {
		return capability.Result{Success: true, Payload: map[string]any{"status": "waiting"},
			Terminal: domain.EventLandingWaiting, Reason: "required checks are pending"}, nil
	})
	registration, events := register(t, endpoint(t), registry)
	if _, isError := toolOutcome(t, callTool(t, registration, registration.Token(), "lands")); isError {
		t.Fatal("a terminal result was reported as an error")
	}
	waitFor(t, func() bool { return len(events.all()) == 1 }, "the terminal event")
	emitted := events.all()[0]
	if emitted.Kind != domain.EventLandingWaiting || emitted.Message != "required checks are pending" {
		t.Fatalf("emitted %+v", emitted)
	}
	if emitted.Kind == domain.EventItem {
		t.Fatal("the endpoint emitted an item event")
	}
}

// TestRevokedRegistrationCannotEmit covers the late terminal outcome: the turn
// is over, the consumer has already seen its terminal event, and a second one
// must not reach it.
func TestRevokedRegistrationCannotEmit(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	registry := newRegistry().with("lands", func(context.Context) (capability.Result, *capability.Failure) {
		close(entered)
		<-release
		return capability.Result{Success: true, Payload: "merged", Terminal: domain.EventLandingResolved}, nil
	})
	registration, events := register(t, endpoint(t), registry)

	call := asyncCall(t, registration, "lands")
	awaitSignal(t, entered, "the invocation to start")

	revoked := make(chan struct{})
	go func() {
		registration.Revoke(context.Background())
		close(revoked)
	}()
	waitFor(t, func() bool {
		response, _ := send(t, http.MethodPost, registration.URL(), registration.Token(),
			`{"jsonrpc":"2.0","id":1,"method":"ping"}`)
		return response.StatusCode == http.StatusUnauthorized
	}, "the revoked token to stop authenticating")

	close(release)
	awaitSignal(t, revoked, "Revoke to return")
	await(t, call, "the drained call")
	if emitted := events.all(); len(emitted) != 0 {
		t.Fatalf("a revoked registration emitted %v", emitted)
	}
	registration.emit(domain.Event{Kind: domain.EventCompleted})
	if emitted := events.all(); len(emitted) != 0 {
		t.Fatalf("a revoked registration emitted %v", emitted)
	}
}

// TestIndependentRegistrationsServeConcurrently exercises four sessions on the
// one listener at once, which is the configured capacity. Run under -race it
// covers the registration table, the per-registration gate, and revocation.
func TestIndependentRegistrationsServeConcurrently(t *testing.T) {
	server := endpoint(t)
	const sessions = 4
	registrations := make([]*Registration, sessions)
	registries := make([]*stubRegistry, sessions)
	for i := range registrations {
		name := fmt.Sprintf("tool_%d", i)
		registries[i] = newRegistry().with(name, succeeds(name))
		registrations[i], _ = register(t, server, registries[i])
	}

	var wg sync.WaitGroup
	problems := make(chan string, sessions*8)
	for i, registration := range registrations {
		for worker := 0; worker < 2; worker++ {
			wg.Add(1)
			go func(i int, registration *Registration) {
				defer wg.Done()
				name := fmt.Sprintf("tool_%d", i)
				for round := 0; round < 8; round++ {
					listed := result(t, rpc(t, registration, registration.Token(), "tools/list", "{}"))
					tools, _ := listed["tools"].([]any)
					if len(tools) != 1 {
						problems <- fmt.Sprintf("session %d was told about %v", i, listed["tools"])
						return
					}
					if advertised, _ := tools[0].(map[string]any); advertised["name"] != name {
						problems <- fmt.Sprintf("session %d was told about %v", i, tools[0])
						return
					}
					// A parallel worker on the same registration may hold the
					// single invocation slot, which is a refusal, not a fault.
					if text, isError := toolOutcome(t, callTool(t, registration, registration.Token(), name)); !isError && text != fmt.Sprintf("%q", name) {
						problems <- fmt.Sprintf("session %d call returned %q", i, text)
						return
					}
				}
			}(i, registration)
		}
	}
	wg.Wait()
	close(problems)
	for problem := range problems {
		t.Fatal(problem)
	}

	var revoking sync.WaitGroup
	for _, registration := range registrations {
		revoking.Add(1)
		go func(registration *Registration) {
			defer revoking.Done()
			registration.Revoke(context.Background())
		}(registration)
	}
	revoking.Wait()
	for i, registry := range registries {
		if count := registry.turnEndedCount(); count != 1 {
			t.Fatalf("session %d ran its finalizer %d times, want 1", i, count)
		}
	}
}

// TestInvocationNeverRunsOnTheRequestContext is the guard against the failure it
// prevents: a killed child closes its connection, net/http cancels the request
// context instantly, and an invocation running on it would abort a merge already
// in flight. The request handled here carries an already-cancelled context.
func TestInvocationNeverRunsOnTheRequestContext(t *testing.T) {
	sessionCtx, endSession := context.WithCancel(context.Background())
	defer endSession()

	observed := make(chan context.Context, 1)
	registry := newRegistry().with("tool", func(ctx context.Context) (capability.Result, *capability.Failure) {
		observed <- ctx
		return capability.Result{Success: true, Payload: "ok"}, nil
	})
	server := endpoint(t)
	registration, err := server.Register(sessionCtx, registry, func(domain.Event) {})
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"tool","arguments":{}}}`
	request := httptest.NewRequest(http.MethodPost, endpointPath, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+registration.Token())
	dead, cancel := context.WithCancel(context.Background())
	cancel()
	recorded := httptest.NewRecorder()
	server.serve(recorded, request.WithContext(dead))

	if recorded.Code != http.StatusOK {
		t.Fatalf("a request on a cancelled context returned %d, want 200", recorded.Code)
	}
	invocationCtx := <-observed
	select {
	case <-invocationCtx.Done():
		t.Fatal("the invocation ran on a context cancelled with the request")
	default:
	}
	// It is the session's context: ending the session ends the invocation's.
	endSession()
	select {
	case <-invocationCtx.Done():
	default:
		t.Fatal("the invocation context is not derived from the session context")
	}
}

// TestAbsentArgumentsBecomeAnEmptyObject covers the one normalization this
// transport performs. MCP declares arguments optional, and the registry's
// zero-argument decoder accepts only an empty object, so an omitted field would
// otherwise refuse every zero-argument capability for good.
func TestAbsentArgumentsBecomeAnEmptyObject(t *testing.T) {
	seen := make(chan string, 1)
	registry := newRegistry().withPrepare("tool", func(arguments json.RawMessage) (capability.Invocation, *capability.Failure) {
		seen <- string(arguments)
		return succeeds("ok"), nil
	})
	registration, _ := register(t, endpoint(t), registry)

	for _, tc := range []struct{ params, want string }{
		{`{"name":"tool"}`, "{}"},
		{`{"name":"tool","arguments":null}`, "{}"},
		{`{"name":"tool","arguments":{}}`, "{}"},
		{`{"name":"tool","arguments":{"why":"x"}}`, `{"why":"x"}`},
	} {
		if _, isError := toolOutcome(t, rpc(t, registration, registration.Token(), "tools/call", tc.params)); isError {
			t.Fatalf("%s was refused", tc.params)
		}
		if got := <-seen; got != tc.want {
			t.Fatalf("%s reached the capability as %q, want %q", tc.params, got, tc.want)
		}
	}
}

// TestEndpointAdvertisesTheLiteralLoopbackAddress pins the address the client is
// given. "localhost" would be resolved in verbatim DNS order by Node >= 17 and
// can yield ::1 first, which never reaches this IPv4 listener and surfaces as a
// 30-second connect timeout.
func TestEndpointAdvertisesTheLiteralLoopbackAddress(t *testing.T) {
	server := endpoint(t)
	first, _ := register(t, server, newRegistry())
	second, _ := register(t, server, newRegistry())

	if !strings.HasPrefix(first.URL(), "http://127.0.0.1:") || !strings.HasSuffix(first.URL(), endpointPath) {
		t.Fatalf("advertised %q, want a literal loopback address and the fixed path", first.URL())
	}
	if strings.Contains(first.URL(), "localhost") {
		t.Fatalf("advertised %q by name", first.URL())
	}
	// The token is the credential, not the path: sessions share one URL, and no
	// secret is in it.
	if first.URL() != second.URL() {
		t.Fatalf("sessions were advertised different URLs: %q and %q", first.URL(), second.URL())
	}
	if first.Token() == "" || first.Token() == second.Token() {
		t.Fatalf("tokens %q and %q are empty or shared", first.Token(), second.Token())
	}
	if strings.Contains(first.URL(), first.Token()) {
		t.Fatalf("the token is embedded in the advertised URL %q", first.URL())
	}
}

func TestEmptyRegistryAdvertisesAnEmptyToolArray(t *testing.T) {
	registration, _ := register(t, endpoint(t), newRegistry())
	listed := result(t, rpc(t, registration, registration.Token(), "tools/list", "{}"))
	tools, ok := listed["tools"].([]any)
	if !ok || len(tools) != 0 {
		t.Fatalf("an empty registry advertised %v, want an empty array", listed["tools"])
	}
}

// TestRegisterRequiresCompleteWiring fails closed on the three inputs whose
// absence would silently disable part of the contract.
func TestRegisterRequiresCompleteWiring(t *testing.T) {
	server := endpoint(t)
	registry := newRegistry()
	// A nil context is exactly what this guard refuses.
	if _, err := server.Register(nil, registry, func(domain.Event) {}); err == nil {
		t.Fatal("a registration without a session context was accepted")
	}
	if _, err := server.Register(context.Background(), nil, func(domain.Event) {}); err == nil {
		t.Fatal("a registration without a registry was accepted")
	}
	if _, err := server.Register(context.Background(), registry, nil); err == nil {
		t.Fatal("a registration without an event sink was accepted")
	}
	if err := server.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := server.Register(context.Background(), registry, func(domain.Event) {}); err == nil {
		t.Fatal("a registration on a closed endpoint was accepted")
	}
}

// asyncCall runs one tools/call on another goroutine. The channel is closed
// however the call ends, so a helper that fails inside the goroutine reports a
// failure instead of deadlocking whoever is waiting for the result.
func asyncCall(t *testing.T, g *Registration, name string) <-chan map[string]any {
	envelopes := make(chan map[string]any, 1)
	go func() {
		defer close(envelopes)
		envelopes <- callTool(t, g, g.Token(), name)
	}()
	return envelopes
}

func await(t *testing.T, envelopes <-chan map[string]any, what string) map[string]any {
	t.Helper()
	select {
	case envelope, ok := <-envelopes:
		if !ok {
			t.Fatalf("%s failed", what)
		}
		return envelope
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		return nil
	}
}

func awaitSignal(t *testing.T, signal <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// waitFor polls a condition that is settled by another goroutine. Every caller
// waits on a state change that has already been triggered, so the bound is a
// failure report, not a timing assumption.
func waitFor(t *testing.T, settled func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !settled() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}
