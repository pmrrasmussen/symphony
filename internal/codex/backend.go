// Package codex adapts the Codex app-server JSON-RPC stdio protocol.
package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/domain"
	githubhost "github.com/pmrrasmussen/symphony/internal/github"
	"github.com/pmrrasmussen/symphony/internal/linear"
	"github.com/pmrrasmussen/symphony/internal/observability"
)

type Backend struct {
	mu          sync.Mutex
	sessions    map[string]*client
	secretNames []string
	handoff     *linear.Handoff
	github      *githubhost.Manager
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

// NewWithIntegrations enables the fixed-scope Linear and optional GitHub
// capabilities and returns the GitHub manager so the host can poll linked PRs.
func NewWithIntegrations(settings func() config.Settings, logger *slog.Logger, secretNames ...string) (*Backend, *githubhost.Manager) {
	b := NewWithLinearHandoff(settings, secretNames...)
	manager := githubhost.New(settings, logger)
	b.github = manager
	return b, manager
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
	var githubSession *githubhost.Session
	if b.github != nil {
		githubSession = b.github.Prepare(r.Issue, r.Workspace, handoff)
	}
	if handoff != nil || b.github != nil {
		secretMatcher = func(candidate string) bool {
			return handoff != nil && handoff.MatchesSecret(candidate) || githubSession != nil && githubSession.MatchesSecret(candidate)
		}
	}
	c, err := start(ctx, r, b.secretNames, secretMatcher, handoff, githubSession)
	if err != nil {
		return domain.AgentSession{}, nil, err
	}
	if _, err = c.call(ctx, "initialize", map[string]any{"clientInfo": map[string]any{"name": "symphony-go", "version": "0.1.0"}, "capabilities": map[string]any{"experimentalApi": true}}); err != nil {
		c.kill()
		return domain.AgentSession{}, nil, err
	}
	if err := c.notify("initialized", map[string]any{}); err != nil {
		c.kill()
		return domain.AgentSession{}, nil, err
	}
	threadParams := map[string]any{"cwd": r.Workspace, "approvalPolicy": r.ApprovalPolicy, "sandbox": r.ThreadSandbox}
	tools := []map[string]any{}
	if handoff != nil {
		tools = append(tools, linearGraphQLToolDefinition())
	}
	if githubSession != nil {
		tools = append(tools, githubToolDefinition())
	}
	if len(tools) > 0 {
		threadParams["dynamicTools"] = tools
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
	go func() {
		<-c.exited
		b.mu.Lock()
		if b.sessions[s.ID] == c {
			delete(b.sessions, s.ID)
		}
		b.mu.Unlock()
	}()
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
func (b *Backend) Cancel(ctx context.Context, s domain.AgentSession) error {
	b.mu.Lock()
	c := b.sessions[s.ID]
	delete(b.sessions, s.ID)
	b.mu.Unlock()
	if c == nil {
		return nil
	}
	c.kill()
	select {
	case <-c.exited:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
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
	github              *githubhost.Session
	mu                  sync.Mutex
	writeMu             sync.Mutex
	next                int
	pending             map[int]chan callResult
	active              chan domain.Event
	activeDone          chan struct{}
	diagnostics         []domain.Event
	activeReady         bool
	pendingEvents       []domain.Event
	pendingTerminal     *domain.Event
	done                chan struct{}
	exited              chan struct{}
	finishOnce          sync.Once
	killOnce            sync.Once
}

type callResult struct {
	rpc rpc
	err error
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

func start(ctx context.Context, r domain.AgentRequest, secrets []string, secretMatcher func(string) bool, handoff *linear.HandoffSession, githubSession *githubhost.Session) (*client, error) {
	command := strings.TrimSpace(r.Command)
	if command == "" {
		command = "codex app-server"
	}
	cmd := exec.CommandContext(ctx, "sh", "-lc", "exec "+command)
	cmd.Dir = r.Workspace
	cmd.Env = filteredEnv(secrets, secretMatcher)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, outWriter, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	stderr, stderrWriter, err := os.Pipe()
	if err != nil {
		_ = out.Close()
		_ = outWriter.Close()
		return nil, err
	}
	cmd.Stdout = outWriter
	cmd.Stderr = stderrWriter
	c := &client{cmd: cmd, in: in, workspace: r.Workspace, approval: r.ApprovalPolicy, policy: r.TurnSandboxPolicy, readTimeout: r.ReadTimeout, turnTimeout: r.TurnTimeout, ctx: ctx, handoff: handoff, github: githubSession, pending: map[int]chan callResult{}, done: make(chan struct{}), exited: make(chan struct{})}
	cmd.Cancel = func() error { return c.killProcessGroup() }
	if err := cmd.Start(); err != nil {
		_ = out.Close()
		_ = outWriter.Close()
		_ = stderr.Close()
		_ = stderrWriter.Close()
		return nil, err
	}
	_ = outWriter.Close()
	_ = stderrWriter.Close()
	stdoutDone := make(chan error, 1)
	stderrDone := make(chan error, 1)
	go func() {
		defer out.Close()
		err := c.read(out)
		stdoutDone <- err
		if err != nil {
			c.abort(err)
		}
	}()
	go func() {
		defer stderr.Close()
		stderrDone <- drain(stderr, c.diagnostic)
	}()
	go func() {
		waitErr := cmd.Wait()
		// The leader can exit while descendants still hold inherited pipes.
		// Terminating the group makes both reader completions deterministic.
		_ = c.killProcessGroup()
		stdoutErr := <-stdoutDone
		stderrErr := <-stderrDone
		switch {
		case stdoutErr != nil:
			c.finish(stdoutErr)
		case stderrErr != nil:
			c.finish(fmt.Errorf("codex stderr read failed: %w", stderrErr))
		case waitErr != nil:
			c.finish(fmt.Errorf("codex process exited: %w", waitErr))
		default:
			c.finish(errors.New("codex process exited"))
		}
		close(c.exited)
	}()
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
	ch := make(chan callResult, 1)
	c.pending[id] = ch
	c.mu.Unlock()
	if err := c.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		c.removePending(id)
		err = fmt.Errorf("codex %s write failed: %w", method, err)
		c.abort(err)
		return nil, err
	}
	var result callResult
	select {
	case result = <-ch:
	case <-ctx.Done():
		c.removePending(id)
		err := fmt.Errorf("codex %s timed out: %w", method, ctx.Err())
		c.abort(err)
		return nil, err
	case <-c.done:
		c.removePending(id)
		// Process finalization waits for the stdout reader, so a response that
		// raced with shutdown has already been dispatched. Prefer that response
		// over the generic exit error.
		select {
		case result = <-ch:
		default:
			return nil, errors.New("codex process exited")
		}
	}
	if result.err != nil {
		return nil, result.err
	}
	x := result.rpc
	if x.Error != nil {
		return nil, fmt.Errorf("codex %s: %s", method, x.Error.Message)
	}
	var out map[string]any
	if err := json.Unmarshal(x.Result, &out); err != nil {
		err = fmt.Errorf("codex malformed %s response: %w", method, err)
		c.abort(err)
		return nil, err
	}
	return out, nil
}
func (c *client) notify(method string, params any) error {
	if err := c.send(map[string]any{"jsonrpc": "2.0", "method": method, "params": params}); err != nil {
		err = fmt.Errorf("codex %s write failed: %w", method, err)
		c.abort(err)
		return err
	}
	return nil
}
func (c *client) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
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
	c.activate(events, s, pid)
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
func (c *client) activate(events chan domain.Event, s domain.AgentSession, pid int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.activeReady = true
	pending := c.pendingEvents
	c.pendingEvents = nil
	pendingTerminal := c.pendingTerminal
	c.pendingTerminal = nil
	events <- domain.Event{Kind: domain.EventSessionStarted, At: time.Now(), SessionID: s.ID, ThreadID: s.ThreadID, TurnID: s.TurnID, PID: pid}
	for _, event := range pending {
		if len(events) < cap(events)-1 {
			events <- event
		}
	}
	if pendingTerminal != nil {
		events <- *pendingTerminal
		c.detachActiveLocked()
	}
}
func (c *client) read(r io.Reader) error {
	scan := bufio.NewScanner(r)
	buf := make([]byte, 0, 64<<10)
	scan.Buffer(buf, 1<<20)
	for scan.Scan() {
		var x rpc
		if err := json.Unmarshal(scan.Bytes(), &x); err != nil {
			return fmt.Errorf("codex malformed app-server message: %w", err)
		}
		// A method always identifies a server request/notification. Responses
		// have no method, so a server request may safely reuse a pending ID.
		if x.Method == "" && x.ID != nil {
			id, ok := asID(x.ID)
			if ok {
				c.mu.Lock()
				ch := c.pending[id]
				if ch != nil {
					delete(c.pending, id)
				}
				c.mu.Unlock()
				if ch != nil {
					if x.Result == nil && x.Error == nil {
						err := errors.New("codex malformed response: missing result and error")
						ch <- callResult{err: err}
						return err
					} else {
						ch <- callResult{rpc: x}
					}
					continue
				}
			}
			return errors.New("codex malformed response: unknown or invalid id")
		}
		c.handle(x)
	}
	if err := scan.Err(); err != nil {
		return fmt.Errorf("codex stdout scanner failed: %w", err)
	}
	return nil
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

func githubToolDefinition() map[string]any {
	return map[string]any{
		"type": "function", "name": "github_publish_pr",
		"description": "Publish the current committed clean worktree to its fixed issue branch, create or reuse its pull request, and hand the active Linear issue to review.",
		"inputSchema": map[string]any{"type": "object", "additionalProperties": false},
	}
}

func (c *client) handleToolCall(x rpc) {
	var request struct {
		Tool      string          `json:"tool"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(x.Params, &request); err != nil {
		c.unsupportedTool(x.ID)
		return
	}
	if request.Tool == "github_publish_pr" && c.github != nil {
		var args map[string]json.RawMessage
		if json.Unmarshal(request.Arguments, &args) != nil || len(args) != 0 {
			c.unsupportedTool(x.ID)
			return
		}
		result, err := c.github.Publish(c.ctx)
		if err != nil {
			if c.sendServerResponse(x.ID, map[string]any{"success": false, "contentItems": []any{map[string]any{"type": "inputText", "text": "GitHub pull request publication was rejected."}}}) {
				c.emit(domain.Event{Kind: domain.EventBlocked, At: time.Now(), Message: "Codex GitHub publication request was rejected"})
			}
			return
		}
		content, _ := json.Marshal(map[string]any{"branch": result.Branch, "pull_request": result.URL, "number": result.Number})
		c.sendServerResponse(x.ID, map[string]any{"success": true, "contentItems": []any{map[string]any{"type": "inputText", "text": string(content)}}})
		return
	}
	if request.Tool != "linear_graphql" || c.handoff == nil {
		c.unsupportedTool(x.ID)
		return
	}
	result, err := c.handoff.Call(c.ctx, request.Arguments)
	if err != nil {
		// Do not return provider errors, issue data, or any credential-derived
		// value to the child. The generic response is enough for the model to
		// choose another path, while the normalized event informs the scheduler.
		if !c.sendServerResponse(x.ID, map[string]any{"success": false, "contentItems": []any{map[string]any{"type": "inputText", "text": "Linear handoff request was rejected."}}}) {
			return
		}
		c.emit(domain.Event{Kind: domain.EventBlocked, At: time.Now(), Message: "Codex Linear handoff request was rejected"})
		return
	}
	text, err := json.Marshal(result.Data)
	if err != nil {
		c.unsupportedTool(x.ID)
		return
	}
	c.sendServerResponse(x.ID, map[string]any{"success": result.Success, "contentItems": []any{map[string]any{"type": "inputText", "text": string(text)}}})
}

func (c *client) unsupportedTool(id any) {
	if !c.sendServerResponse(id, map[string]any{"success": false, "contentItems": []any{map[string]any{"type": "inputText", "text": "Unsupported client-side tool."}}}) {
		return
	}
	c.emit(domain.Event{Kind: domain.EventBlocked, At: time.Now(), Message: "Codex requested an unsupported client-side tool"})
}
func (c *client) sendServerResponse(id, result any) bool {
	if err := c.send(map[string]any{"jsonrpc": "2.0", "id": id, "result": result}); err != nil {
		c.abort(fmt.Errorf("codex server response write failed: %w", err))
		return false
	}
	return true
}
func (c *client) emit(e domain.Event) {
	c.mu.Lock()
	ch := c.active
	if ch != nil {
		if !c.activeReady {
			if terminal(e.Kind) && c.pendingTerminal == nil {
				copy := e
				c.pendingTerminal = &copy
			} else if c.pendingTerminal == nil && len(c.pendingEvents) < cap(ch)-2 {
				c.pendingEvents = append(c.pendingEvents, e)
			}
			c.mu.Unlock()
			return
		}
		if terminal(e.Kind) {
			// Non-terminal sends reserve one slot, so terminal delivery cannot
			// block even when the consumer falls behind.
			ch <- e
			c.detachActiveLocked()
			c.mu.Unlock()
			return
		}
		if len(ch) < cap(ch)-1 {
			ch <- e
		}
	} else if e.Kind == domain.EventDiagnostic && len(c.diagnostics) < 16 {
		c.diagnostics = append(c.diagnostics, e)
	}
	c.mu.Unlock()
}
func (c *client) closeActive() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.detachActiveLocked()
}
func (c *client) detachActiveLocked() {
	ch := c.active
	done := c.activeDone
	c.active = nil
	c.activeDone = nil
	c.activeReady = false
	c.pendingEvents = nil
	c.pendingTerminal = nil
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
func (c *client) finish(err error) {
	c.finishOnce.Do(func() {
		if err == nil {
			err = errors.New("codex process exited")
		}
		c.failPending(err)
		c.emit(domain.Event{Kind: domain.EventFailed, At: time.Now(), Message: observability.Text(err.Error())})
		_ = c.in.Close()
		close(c.done)
	})
}
func (c *client) abort(err error) {
	c.kill()
	c.finish(err)
}
func (c *client) kill() {
	c.killOnce.Do(func() {
		_ = c.in.Close()
		_ = c.killProcessGroup()
	})
}
func (c *client) killProcessGroup() error {
	if c.cmd == nil || c.cmd.Process == nil {
		return nil
	}
	err := syscall.Kill(-c.cmd.Process.Pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}
func (c *client) removePending(id int) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}
func (c *client) failPending(err error) {
	c.mu.Lock()
	pending := c.pending
	c.pending = map[int]chan callResult{}
	c.mu.Unlock()
	for _, ch := range pending {
		ch <- callResult{err: err}
	}
}
func (c *client) diagnostic(message string) {
	c.emit(domain.Event{Kind: domain.EventDiagnostic, At: time.Now(), Message: observability.Text(message)})
}

func drain(r io.Reader, report func(string)) error {
	reader := bufio.NewReaderSize(r, observability.MaxDiagnosticBytes)
	line := make([]byte, 0, observability.MaxDiagnosticBytes)
	oversized := false
	for {
		part, err := reader.ReadSlice('\n')
		if !oversized {
			remaining := observability.MaxDiagnosticBytes - len(line)
			if len(part) <= remaining {
				line = append(line, part...)
			} else {
				oversized = true
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			oversized = true
			continue
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		if oversized {
			report("stderr diagnostic exceeded limit")
		} else if message := strings.TrimSuffix(strings.TrimSuffix(string(line), "\n"), "\r"); message != "" {
			report(message)
		}
		line = line[:0]
		oversized = false
		if errors.Is(err, io.EOF) {
			return nil
		}
	}
}
func terminal(kind domain.EventKind) bool {
	return kind == domain.EventBlocked || kind == domain.EventCompleted || kind == domain.EventFailed
}
func nestedString(m map[string]any, a, b string) (string, bool) {
	x, ok := m[a].(map[string]any)
	if !ok {
		return "", false
	}
	v, ok := x[b].(string)
	return v, ok
}
func asID(v any) (int, bool) {
	x, ok := v.(float64)
	if !ok || x < 0 || x != math.Trunc(x) || x > float64(^uint(0)>>1) {
		return 0, false
	}
	return int(x), true
}
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
