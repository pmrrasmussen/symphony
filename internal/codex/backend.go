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

	"github.com/pmrrasmussen/symphony/internal/domain"
)

type Backend struct {
	mu          sync.Mutex
	sessions    map[string]*client
	secretNames []string
}

func New(secretNames ...string) *Backend {
	return &Backend{sessions: map[string]*client{}, secretNames: secretNames}
}
func (b *Backend) Start(ctx context.Context, r domain.AgentRequest) (domain.AgentSession, <-chan domain.Event, error) {
	c, err := start(ctx, r, b.secretNames)
	if err != nil {
		return domain.AgentSession{}, nil, err
	}
	if _, err = c.call(ctx, "initialize", map[string]any{"clientInfo": map[string]any{"name": "symphony-go", "version": "0.1.0"}, "capabilities": map[string]any{"experimentalApi": true}}); err != nil {
		c.kill()
		return domain.AgentSession{}, nil, err
	}
	c.notify("initialized", map[string]any{})
	res, err := c.call(ctx, "thread/start", map[string]any{"cwd": r.Workspace, "approvalPolicy": r.ApprovalPolicy, "sandbox": r.ThreadSandbox})
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
	mu                  sync.Mutex
	next                int
	pending             map[int]chan rpc
	active              chan domain.Event
	activeDone          chan struct{}
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

func start(ctx context.Context, r domain.AgentRequest, secrets []string) (*client, error) {
	command := strings.TrimSpace(r.Command)
	if command == "" {
		command = "codex app-server"
	}
	cmd := exec.CommandContext(ctx, "sh", "-lc", "exec "+command)
	cmd.Dir = r.Workspace
	cmd.Env = filteredEnv(secrets)
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
	c := &client{cmd: cmd, in: in, workspace: r.Workspace, approval: r.ApprovalPolicy, policy: r.TurnSandboxPolicy, readTimeout: r.ReadTimeout, turnTimeout: r.TurnTimeout, pending: map[int]chan rpc{}, done: make(chan struct{})}
	go c.read(out)
	go drain(stderr)
	go func() { _ = cmd.Wait(); c.finish() }()
	return c, nil
}
func filteredEnv(names []string) []string {
	blocked := map[string]bool{}
	for _, n := range names {
		blocked[n] = true
	}
	out := []string{}
	for _, v := range os.Environ() {
		k := strings.SplitN(v, "=", 2)[0]
		if !blocked[k] {
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
	turnDone := c.activeDone
	c.mu.Unlock()
	events <- domain.Event{Kind: domain.EventSessionStarted, At: time.Now(), ThreadID: thread}
	res, err := c.call(ctx, "turn/start", params)
	if err != nil {
		c.closeActive()
		return domain.AgentSession{}, nil, err
	}
	turn, ok := nestedString(res, "turn", "id")
	if !ok {
		c.closeActive()
		return domain.AgentSession{}, nil, errors.New("codex malformed turn/start response")
	}
	s := domain.AgentSession{ID: thread + "-" + turn, ThreadID: thread, TurnID: turn}
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
		_ = c.send(map[string]any{"jsonrpc": "2.0", "id": x.ID, "result": map[string]any{"success": false, "output": "unsupported tool", "contentItems": []any{}}})
		c.emit(domain.Event{Kind: domain.EventBlocked, At: time.Now(), Message: "Codex requested an unsupported client-side tool"})
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
		c.closeActive()
		return
	}
	if method == "turn/failed" || method == "turn/cancelled" {
		c.emit(domain.Event{Kind: domain.EventFailed, At: time.Now(), Message: method})
		c.closeActive()
		return
	}
	c.emit(domain.Event{Kind: domain.EventProgress, At: time.Now(), Message: method})
}
func (c *client) emit(e domain.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := c.active
	if ch != nil {
		select {
		case ch <- e:
		default:
		}
	}
}
func (c *client) closeActive() {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := c.active
	done := c.activeDone
	c.active = nil
	c.activeDone = nil
	if ch != nil {
		close(ch)
	}
	if done != nil {
		close(done)
	}
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
func drain(r io.Reader) {
	s := bufio.NewScanner(r)
	for s.Scan() { /* diagnostics intentionally not mixed into JSON-RPC */
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
