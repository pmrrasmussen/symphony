// Package codex adapts the Codex app-server JSON-RPC stdio protocol.
package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/domain"
	"github.com/pmrrasmussen/symphony/internal/linear"
	"github.com/pmrrasmussen/symphony/internal/observability"
)

type Backend struct {
	mu          sync.Mutex
	sessions    map[string]*client
	secretNames []string
	handoff     *linear.Handoff
}

func New(secretNames ...string) *Backend {
	return &Backend{sessions: map[string]*client{}, secretNames: secretNames}
}

// NewWithLinearHandoff enables the sole supported client-side tool. The
// Linear adapter owns its configuration and HTTP transport; Codex sees only a
// session-bound capability once the policy has been validated.
func NewWithLinearHandoff(settings func() config.Settings, secretNames ...string) *Backend {
	b := New(secretNames...)
	b.handoff = linear.NewHandoff(settings)
	return b
}
func (b *Backend) Start(ctx context.Context, r domain.AgentRequest) (domain.AgentSession, <-chan domain.Event, error) {
	var handoff *linear.HandoffSession
	var err error
	if b.handoff != nil && b.handoff.Enabled() {
		handoff, err = b.handoff.Prepare(ctx, r.Issue)
		if err != nil {
			return domain.AgentSession{}, nil, fmt.Errorf("prepare Linear handoff: %w", err)
		}
	}
	var secretMatcher func(string) bool
	if handoff != nil {
		secretMatcher = handoff.MatchesSecret
	}
	c, err := start(ctx, r, b.secretNames, secretMatcher, handoff)
	if err != nil {
		return domain.AgentSession{}, nil, err
	}
	if _, err = c.call(ctx, "initialize", map[string]any{"clientInfo": map[string]any{"name": "symphony-go", "version": "0.1.0"}, "capabilities": map[string]any{"experimentalApi": true}}); err != nil {
		c.kill()
		return domain.AgentSession{}, nil, err
	}
	c.notify("initialized", map[string]any{})
	threadParams := map[string]any{"cwd": r.Workspace, "approvalPolicy": r.ApprovalPolicy, "sandbox": r.ThreadSandbox}
	if handoff != nil {
		threadParams["dynamicTools"] = []map[string]any{linearGraphQLToolDefinition()}
	}
	res, err := c.call(ctx, "thread/start", threadParams)
	if err != nil {
		c.kill()
		return domain.AgentSession{}, nil, err
	}
	thread, ok := nestedString(res, "thread", "id")
	if !ok {
		c.kill()
		return domain.AgentSession{}, nil, errors.New("codex malformed thread/start response")
	}
	s, events, err := c.turn(ctx, thread, r.Prompt, r)
	if err != nil {
		c.kill()
		return domain.AgentSession{}, nil, err
	}
	b.mu.Lock()
	b.sessions[s.ID] = c
	b.mu.Unlock()
	return s, events, nil
}
func (b *Backend) Continue(ctx context.Context, s domain.AgentSession, prompt string) (<-chan domain.Event, error) {
	b.mu.Lock()
	c := b.sessions[s.ID]
	b.mu.Unlock()
	if c == nil {
		return nil, errors.New("unknown codex session")
	}
	_, events, err := c.turn(ctx, s.ThreadID, prompt, domain.AgentRequest{Workspace: c.workspace, ApprovalPolicy: c.approval, TurnSandboxPolicy: c.policy, TurnTimeout: c.turnTimeout, ReadTimeout: c.readTimeout})
	return events, err
}
func (b *Backend) Cancel(_ context.Context, s domain.AgentSession) error {
	b.mu.Lock()
	c := b.sessions[s.ID]
	delete(b.sessions, s.ID)
	b.mu.Unlock()
	if c == nil {
		return nil
	}
	c.kill()
	return nil
}

type client struct {
	cmd                 *exec.Cmd
	in                  io.WriteCloser
	workspace, approval string
	policy              any
	readTimeout         time.Duration
	turnTimeout         time.Duration
	ctx                 context.Context
	handoff             *linear.HandoffSession
	mu                  sync.Mutex
	next                int
	pending             map[int]chan rpc
	active              chan domain.Event
	activeDone          chan struct{}
	diagnostics         []domain.Event
	activeReady         bool
	pendingEvents       []domain.Event
	pendingClose        bool
	done                chan struct{}
}
type rpc struct {
	ID     any             `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func start(ctx context.Context, r domain.AgentRequest, secrets []string, secretMatcher func(string) bool, handoff *linear.HandoffSession) (*client, error) {
	command := strings.TrimSpace(r.Command)
	if command == "" {
		command = "codex app-server"
	}
	cmd := exec.CommandContext(ctx, "sh", "-lc", "exec "+command)
	cmd.Dir = r.Workspace
	cmd.Env = filteredEnv(secrets, secretMatcher)
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	c := &client{cmd: cmd, in: in, workspace: r.Workspace, approval: r.ApprovalPolicy, policy: r.TurnSandboxPolicy, readTimeout: r.ReadTimeout, turnTimeout: r.TurnTimeout, ctx: ctx, handoff: handoff, pending: map[int]chan rpc{}, done: make(chan struct{})}
	go c.read(out)
	stderrDone := make(chan struct{})
	go func() { drain(stderr, c.diagnostic); close(stderrDone) }()
	go func() { _ = cmd.Wait(); <-stderrDone; c.finish() }()
	return c, nil
}
func filteredEnv(names []string, secretMatcher func(string) bool) []string {
	blocked := map[string]bool{}
	for _, n := range names {
		blocked[n] = true
	}
	out := []string{}
	for _, v := range os.Environ() {
		k := strings.SplitN(v, "=", 2)[0]
		value := strings.TrimPrefix(v, k+"=")
		if !blocked[k] && (secretMatcher == nil || !secretMatcher(value)) {
			out = append(out, v)
		}
	}
	return out
}
func (c *client) call(ctx context.Context, method string, params any) (map[string]any, error) {
	if c.readTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.readTimeout)
		defer cancel()
	}
	c.mu.Lock()
	c.next++
	id := c.next
	ch := make(chan rpc, 1)
	c.pending[id] = ch
	c.mu.Unlock()
	if err := c.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	select {
	case x := <-ch:
		if x.Error != nil {
			return nil, fmt.Errorf("codex %s: %s", method, x.Error.Message)
		}
		var out map[string]any
		if err := json.Unmarshal(x.Result, &out); err != nil {
			return nil, fmt.Errorf("codex malformed %s response: %w", method, err)
		}
		return out, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.done:
		return nil, errors.New("codex process exited")
	}
}
func (c *client) notify(method string, params any) {
	_ = c.send(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}
func (c *client) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err = c.in.Write(append(b, '\n'))
	return err
}
func (c *client) turn(ctx context.Context, thread, prompt string, r domain.AgentRequest) (domain.AgentSession, <-chan domain.Event, error) {
	params := map[string]any{"threadId": thread, "input": []map[string]string{{"type": "text", "text": prompt}}, "cwd": r.Workspace, "approvalPolicy": r.ApprovalPolicy}
	if r.TurnSandboxPolicy != nil {
		params["sandboxPolicy"] = r.TurnSandboxPolicy
	}
	events := make(chan domain.Event, 32)
	c.mu.Lock()
	c.active = events
	c.activeDone = make(chan struct{})
	c.activeReady = false
	c.pendingEvents = append(c.pendingEvents, c.diagnostics...)
	c.diagnostics = nil
	turnDone := c.activeDone
	c.mu.Unlock()
	res, err := c.call(ctx, "turn/start", params)
	if err != nil {
		c.forceCloseActive()
		return domain.AgentSession{}, nil, err
	}
	turn, ok := nestedString(res, "turn", "id")
	if !ok {
		c.forceCloseActive()
		return domain.AgentSession{}, nil, errors.New("codex malformed turn/start response")
	}
	s := domain.AgentSession{ID: thread + "-" + turn, ThreadID: thread, TurnID: turn}
	pid := 0
	if c.cmd.Process != nil {
		pid = c.cmd.Process.Pid
	}
	c.mu.Lock()
	c.activeReady = true
	pending := c.pendingEvents
	c.pendingEvents = nil
	pendingClose := c.pendingClose
	c.pendingClose = false
	c.mu.Unlock()
	c.emit(domain.Event{Kind: domain.EventSessionStarted, At: time.Now(), SessionID: s.ID, ThreadID: thread, TurnID: turn, PID: pid})
	for _, event := range pending {
		c.emit(event)
	}
	if pendingClose {
		c.closeActive()
	}
	if r.TurnTimeout > 0 {
		go func() {
			timer := time.NewTimer(r.TurnTimeout)
			defer timer.Stop()
			select {
			case <-turnDone:
			case <-timer.C:
				c.emit(domain.Event{Kind: domain.EventFailed, At: time.Now(), Message: "Codex turn timeout"})
				c.kill()
			}
		}()
	}
	return s, events, nil
}
func (c *client) read(r io.Reader) {
	scan := bufio.NewScanner(r)
	buf := make([]byte, 0, 64<<10)
	scan.Buffer(buf, 1<<20)
	for scan.Scan() {
		var x rpc
		if err := json.Unmarshal(scan.Bytes(), &x); err != nil {
			c.emit(domain.Event{Kind: domain.EventProgress, At: time.Now(), Message: "malformed Codex app-server message"})
			continue
		}
		if x.ID != nil {
			id, ok := asID(x.ID)
			if ok {
				c.mu.Lock()
				ch := c.pending[id]
				delete(c.pending, id)
				c.mu.Unlock()
				if ch != nil {
					ch <- x
					continue
				}
			}
		}
		c.handle(x)
	}
	c.finish()
}
func (c *client) handle(x rpc) {
	method := x.Method
	if method == "" {
		return
	}
	if method == "item/tool/call" && x.ID != nil {
		c.handleToolCall(x)
		return
	}
	if strings.Contains(method, "requestApproval") || method == "item/tool/requestUserInput" {
		c.emit(domain.Event{Kind: domain.EventBlocked, At: time.Now(), Message: "Codex requested interactive approval or input"})
		c.kill()
		return
	}
	if method == "account/rateLimits/updated" {
		var p map[string]any
		_ = json.Unmarshal(x.Params, &p)
		c.emit(domain.Event{Kind: domain.EventRateLimit, At: time.Now(), RateLimit: p})
		return
	}
	if method == "turn/completed" {
		if usage := usageFrom(x.Params); usage != (domain.Usage{}) {
			c.emit(domain.Event{Kind: domain.EventUsage, At: time.Now(), Usage: usage})
		}
		c.emit(domain.Event{Kind: domain.EventCompleted, At: time.Now()})
		return
	}
	if method == "turn/failed" || method == "turn/cancelled" {
		c.emit(domain.Event{Kind: domain.EventFailed, At: time.Now(), Message: method})
		c.closeActive()
		return
	}
	c.emit(domain.Event{Kind: domain.EventProgress, At: time.Now(), Message: method})
}

func linearGraphQLToolDefinition() map[string]any {
	return map[string]any{
		"type": "function", "name": "linear_graphql",
		"description": "Read the active Linear issue, move only it to the workflow-configured handoff state, or add a bounded comment only to it.",
		"inputSchema": map[string]any{
			"type": "object", "additionalProperties": false,
			"properties": map[string]any{
				"operation": map[string]any{"type": "string", "enum": []string{"read", "handoff", "comment"}},
				"body":      map[string]any{"type": "string", "maxLength": 8192},
			},
			"required": []string{"operation"},
		},
	}
}

func (c *client) handleToolCall(x rpc) {
	var request struct {
		Tool      string          `json:"tool"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(x.Params, &request); err != nil || request.Tool != "linear_graphql" || c.handoff == nil {
		c.unsupportedTool(x.ID)
		return
	}
	result, err := c.handoff.Call(c.ctx, request.Arguments)
	if err != nil {
		// Do not return provider errors, issue data, or any credential-derived
		// value to the child. The generic response is enough for the model to
		// choose another path, while the normalized event informs the scheduler.
		_ = c.send(map[string]any{"jsonrpc": "2.0", "id": x.ID, "result": map[string]any{"success": false, "contentItems": []any{map[string]any{"type": "inputText", "text": "Linear handoff request was rejected."}}}})
		c.emit(domain.Event{Kind: domain.EventBlocked, At: time.Now(), Message: "Codex Linear handoff request was rejected"})
		return
	}
	text, err := json.Marshal(result.Data)
	if err != nil {
		c.unsupportedTool(x.ID)
		return
	}
	_ = c.send(map[string]any{"jsonrpc": "2.0", "id": x.ID, "result": map[string]any{"success": result.Success, "contentItems": []any{map[string]any{"type": "inputText", "text": string(text)}}}})
}

func (c *client) unsupportedTool(id any) {
	_ = c.send(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"success": false, "contentItems": []any{map[string]any{"type": "inputText", "text": "Unsupported client-side tool."}}}})
	c.emit(domain.Event{Kind: domain.EventBlocked, At: time.Now(), Message: "Codex requested an unsupported client-side tool"})
}
func (c *client) emit(e domain.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := c.active
	if ch != nil {
		if !c.activeReady {
			if len(c.pendingEvents) < 32 {
				c.pendingEvents = append(c.pendingEvents, e)
			}
			return
		}
		select {
		case ch <- e:
		default:
		}
	} else if e.Kind == domain.EventDiagnostic && len(c.diagnostics) < 16 {
		c.diagnostics = append(c.diagnostics, e)
	}
}
func (c *client) closeActive() {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := c.active
	if ch != nil && !c.activeReady {
		c.pendingClose = true
		return
	}
	done := c.activeDone
	c.active = nil
	c.activeDone = nil
	c.activeReady = false
	c.pendingEvents = nil
	c.pendingClose = false
	if ch != nil {
		close(ch)
	}
	if done != nil {
		close(done)
	}
}
func (c *client) forceCloseActive() {
	c.mu.Lock()
	c.activeReady = true
	c.mu.Unlock()
	c.closeActive()
}
func (c *client) finish() {
	select {
	case <-c.done:
	default:
		close(c.done)
	}
	c.closeActive()
}
func (c *client) kill() {
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
}
func (c *client) diagnostic(message string) {
	c.emit(domain.Event{Kind: domain.EventDiagnostic, At: time.Now(), Message: observability.Text(message)})
}

func drain(r io.Reader, report func(string)) {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 0, observability.MaxDiagnosticBytes), observability.MaxDiagnosticBytes)
	for s.Scan() { /* diagnostics intentionally not mixed into JSON-RPC */
		report(s.Text())
	}
	if err := s.Err(); err != nil {
		report("stderr diagnostic exceeded limit")
	}
}
func nestedString(m map[string]any, a, b string) (string, bool) {
	x, ok := m[a].(map[string]any)
	if !ok {
		return "", false
	}
	v, ok := x[b].(string)
	return v, ok
}
func asID(v any) (int, bool) { x, ok := v.(float64); return int(x), ok }
func usageFrom(raw json.RawMessage) domain.Usage {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return domain.Usage{}
	}
	var find func(any) domain.Usage
	find = func(v any) domain.Usage {
		m, ok := v.(map[string]any)
		if !ok {
			return domain.Usage{}
		}
		get := func(k string) int64 { n, _ := m[k].(float64); return int64(n) }
		u := domain.Usage{InputTokens: get("inputTokens"), OutputTokens: get("outputTokens"), TotalTokens: get("totalTokens")}
		if u != (domain.Usage{}) {
			return u
		}
		for _, child := range m {
			if found := find(child); found != (domain.Usage{}) {
				return found
			}
		}
		return domain.Usage{}
	}
	return find(value)
}
