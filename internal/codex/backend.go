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
	settings    func() config.Settings
	handoff     *linear.Handoff
	github      *githubhost.Manager
}

var reservedSecretEnvNames = []string{
	"LINEAR_API_KEY",
	"SYMPHONY_LINEAR_API_KEY_FILE",
	"GITHUB_TOKEN",
	"SYMPHONY_GITHUB_TOKEN",
	"SYMPHONY_GITHUB_TOKEN_FILE",
}

func New(secretNames ...string) *Backend {
	names := append(append([]string(nil), reservedSecretEnvNames...), secretNames...)
	return &Backend{sessions: map[string]*client{}, secretNames: uniquePaths(names)}
}

// NewWithLinearHandoff enables the sole supported client-side tool. The
// Linear adapter owns its configuration and HTTP transport; Codex sees only a
// session-bound capability once the policy has been validated.
func NewWithLinearHandoff(settings func() config.Settings, secretNames ...string) *Backend {
	b := New(secretNames...)
	b.settings = settings
	b.handoff = linear.NewHandoff(settings)
	return b
}

// NewWithIntegrations enables the fixed-scope Linear and optional GitHub
// capabilities and returns the GitHub manager so the host can poll linked PRs.
func NewWithIntegrations(settings func() config.Settings, logger *slog.Logger, secretNames ...string) (*Backend, *githubhost.Manager) {
	b := NewWithLinearHandoff(settings, secretNames...)
	if b.handoff != nil {
		b.handoff.SetLogger(logger)
	}
	manager := githubhost.New(settings, logger)
	b.github = manager
	return b, manager
}
func (b *Backend) Start(ctx context.Context, r domain.AgentRequest) (domain.AgentSession, <-chan domain.Event, error) {
	settings := config.Settings{}
	if b.settings != nil {
		settings = b.settings()
	}
	var handoff *linear.HandoffSession
	var err error
	if b.handoff != nil && settings.LinearSessionCapabilityEnabled() {
		handoff, err = b.handoff.PrepareWithSettings(ctx, settings, r.Issue)
		if err != nil {
			return domain.AgentSession{}, nil, fmt.Errorf("prepare Linear handoff: %w", err)
		}
	}
	var secretMatcher func(string) bool
	var githubSession *githubhost.Session
	if b.github != nil {
		githubSession = b.github.PrepareWithSettings(settings.GitHub, r.Issue, r.Workspace, handoff)
	}
	if handoff != nil || b.github != nil {
		secretMatcher = func(candidate string) bool {
			return handoff != nil && handoff.MatchesSecret(candidate) || githubSession != nil && githubSession.MatchesSecret(candidate)
		}
	}
	r.TurnSandboxPolicy = localCommitSandbox(r)
	secretNames := append(append([]string(nil), b.secretNames...), settings.HostSecretEnvNames...)
	secretMatcher = withSecretValues(secretMatcher, settings.HostSecretValues)
	c, err := start(ctx, r, secretNames, secretMatcher, handoff, githubSession)
	if err != nil {
		return domain.AgentSession{}, nil, err
	}
	if _, err = c.callWithTimeout(ctx, "initialize", map[string]any{"clientInfo": map[string]any{"name": "symphony-go", "version": "0.1.0"}, "capabilities": map[string]any{"experimentalApi": true}}, c.startTimeout); err != nil {
		c.kill()
		return domain.AgentSession{}, nil, err
	}
	if err := c.notify("initialized", map[string]any{}); err != nil {
		c.kill()
		return domain.AgentSession{}, nil, err
	}
	threadParams := map[string]any{"cwd": r.Workspace, "approvalPolicy": r.ApprovalPolicy, "sandbox": r.ThreadSandbox}
	tools := []map[string]any{}
	if handoff != nil && settings.Tracker.FollowupIssueCreation {
		tools = append(tools, createFollowupIssueToolDefinition())
	}
	if githubSession != nil {
		tools = append(tools, githubToolDefinition(), githubContextToolDefinition())
		// github_land_pr is advertised only for a session bound to an issue
		// currently in the exact configured Merging state; Land itself
		// re-validates that Linear state immediately before any mutation, so
		// this is a coarse dispatch-time filter, not the authority.
		if mergeState := strings.TrimSpace(settings.GitHub.MergeState); mergeState != "" && strings.EqualFold(strings.TrimSpace(r.Issue.State), mergeState) {
			tools = append(tools, githubLandToolDefinition())
		}
	}
	if len(tools) > 0 {
		threadParams["dynamicTools"] = tools
	}
	// The cold-start handshake and thread/start use the generous start timeout:
	// a cold app-server's first model load routinely exceeds the small
	// steady-state read timeout. Every subsequent RPC keeps the read timeout.
	res, err := c.callWithTimeout(ctx, "thread/start", threadParams, c.startTimeout)
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
	// A hard cancel may pre-empt turn/completed, so finalize the deferred
	// landing refusal here too. It is idempotent and a no-op when unused.
	c.finalizeLanding()
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
	startTimeout        time.Duration
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
	cmd := exec.CommandContext(ctx, "bash", "-lc", command)
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
	c := &client{cmd: cmd, in: in, workspace: r.Workspace, approval: r.ApprovalPolicy, policy: r.TurnSandboxPolicy, readTimeout: r.ReadTimeout, startTimeout: r.StartTimeout, turnTimeout: r.TurnTimeout, ctx: ctx, handoff: handoff, github: githubSession, pending: map[int]chan callResult{}, done: make(chan struct{}), exited: make(chan struct{})}
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

// localCommitSandbox extends only workspace-write turns with the narrow Git
// roots Symphony validated for this linked worktree: the source repository's
// shared object store and this worktree's own per-worktree metadata directory.
// The worker still has no GitHub credential -- host-owned Linear and GitHub
// secrets never reach the child environment, whatever network the configured
// policy allows -- while a detached-HEAD git commit can write its objects,
// HEAD, index, and reflog. The rest of the source common directory (branch
// refs, the primary index, other worktrees) stays outside the grant, so an
// agent cannot mutate source branches (PMR-65).
func localCommitSandbox(r domain.AgentRequest) any {
	grants := uniquePaths(r.GitMetadataRoots)
	if len(grants) == 0 || r.ThreadSandbox != "workspace-write" {
		return r.TurnSandboxPolicy
	}
	if r.TurnSandboxPolicy == nil {
		return map[string]any{"type": "workspaceWrite", "writableRoots": grants}
	}
	policy, ok := r.TurnSandboxPolicy.(map[string]any)
	if !ok || policy["type"] != "workspaceWrite" {
		return r.TurnSandboxPolicy
	}
	copy := make(map[string]any, len(policy)+1)
	for key, value := range policy {
		copy[key] = value
	}
	var roots []string
	if configured, ok := copy["writableRoots"].([]string); ok {
		roots = append(roots, configured...)
	} else if configured, ok := copy["writableRoots"].([]any); ok {
		for _, value := range configured {
			if path, ok := value.(string); ok && strings.TrimSpace(path) != "" {
				roots = append(roots, path)
			}
		}
	}
	roots = append(roots, grants...)
	copy["writableRoots"] = uniquePaths(roots)
	return copy
}

func uniquePaths(paths []string) []string {
	seen := make(map[string]bool, len(paths))
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path != "" && !seen[path] {
			seen[path] = true
			out = append(out, path)
		}
	}
	return out
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

func withSecretValues(matcher func(string) bool, values []string) func(string) bool {
	if len(values) == 0 {
		return matcher
	}
	return func(candidate string) bool {
		if matcher != nil && matcher(candidate) {
			return true
		}
		for _, value := range values {
			if value != "" && strings.Contains(candidate, value) {
				return true
			}
		}
		return false
	}
}

// call applies the steady-state read timeout to a single JSON-RPC round trip.
// The cold-start handshake and thread/start use callWithTimeout with the
// separate, more generous start timeout so a first model load does not trip
// mid-turn hang detection.
func (c *client) call(ctx context.Context, method string, params any) (map[string]any, error) {
	return c.callWithTimeout(ctx, method, params, c.readTimeout)
}

func (c *client) callWithTimeout(ctx context.Context, method string, params any, timeout time.Duration) (map[string]any, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
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
	if method == "item/started" || method == "item/completed" {
		c.emitItemEvent(method, x.Params)
		return
	}
	if method == "turn/completed" {
		c.finalizeLanding()
		if usage := usageFrom(x.Params); usage != (domain.Usage{}) {
			c.emit(domain.Event{Kind: domain.EventUsage, At: time.Now(), Usage: usage})
		}
		c.emit(domain.Event{Kind: domain.EventCompleted, At: time.Now()})
		return
	}
	if method == "turn/failed" || method == "turn/cancelled" {
		c.finalizeLanding()
		c.emit(domain.Event{Kind: domain.EventFailed, At: time.Now(), Message: method})
		return
	}
	c.emit(domain.Event{Kind: domain.EventProgress, At: time.Now(), Message: method})
}

// itemLifecycle is deliberately narrow: it decodes only the protocol-defined
// item identity, type, an already-fixed tool name (never a value parsed out
// of tool arguments), status, and a protocol-computed duration. Any other
// field on the item payload -- command bodies, tool arguments, tool output,
// diffs, search queries, model reasoning -- has no matching struct field and
// is silently discarded by json.Unmarshal, so it can never reach a log.
type itemLifecycle struct {
	Item struct {
		ID         string `json:"id"`
		Type       string `json:"type"`
		Tool       string `json:"tool"`
		Status     string `json:"status"`
		DurationMs int64  `json:"durationMs"`
	} `json:"item"`
}

// emitItemEvent classifies an "item/started" or "item/completed" app-server
// notification into a safe EventItem record: the outstanding operation's
// protocol-defined type and ID, its fixed tool name when the protocol
// supplies one directly (MCP and dynamic tool calls), and its started or
// completed outcome. See itemLifecycle for the parsing boundary.
func (c *client) emitItemEvent(method string, raw json.RawMessage) {
	var notification itemLifecycle
	if err := json.Unmarshal(raw, &notification); err != nil || notification.Item.ID == "" {
		return
	}
	outcome := domain.ItemStarted
	if method == "item/completed" {
		outcome = notification.Item.Status
		if outcome == "" {
			outcome = domain.ItemCompleted
		}
	}
	c.emit(domain.Event{
		Kind: domain.EventItem, At: time.Now(),
		ItemID: observability.Text(notification.Item.ID), ItemType: observability.Text(notification.Item.Type),
		ToolName: observability.Text(notification.Item.Tool), Outcome: observability.Text(outcome),
		DurationMs: notification.Item.DurationMs,
	})
}

func createFollowupIssueToolDefinition() map[string]any {
	return map[string]any{
		"type": "function", "name": "create_followup_issue",
		"description": "Capture meaningful out-of-scope work as a new Backlog Linear issue in the active issue's configured project and team, then continue the current issue. The follow-up is not a child and is not dispatchable until a human promotes it. relationship may only relate it to the current issue or mark it blocked by the current issue.",
		"inputSchema": map[string]any{
			"type": "object", "additionalProperties": false,
			"properties": map[string]any{
				"title":               map[string]any{"type": "string", "minLength": 1, "maxLength": 255},
				"description":         map[string]any{"type": "string", "minLength": 1, "maxLength": 16000},
				"acceptance_criteria": map[string]any{"type": "string", "minLength": 1, "maxLength": 4000},
				"relationship":        map[string]any{"type": "string", "enum": []string{"related", "blocked_by_current"}},
			},
			"required": []string{"title", "description", "acceptance_criteria"},
		},
	}
}

func githubToolDefinition() map[string]any {
	return map[string]any{
		"type": "function", "name": "github_publish_pr",
		"description": "Publish the current committed clean worktree to its fixed issue branch, create or reuse its pull request with a structured Why/What changed/On Call body, and hand the active Linear issue to review.",
		"inputSchema": map[string]any{
			"type": "object", "additionalProperties": false,
			"properties": map[string]any{
				// Bounds must match internal/github.maxPublishWhyBytes and friends.
				"why":          map[string]any{"type": "string", "minLength": 1, "maxLength": 4096},
				"what_changed": map[string]any{"type": "string", "minLength": 1, "maxLength": 8192},
				"on_call":      map[string]any{"type": "string", "maxLength": 2048},
			},
			"required": []string{"why", "what_changed", "on_call"},
		},
	}
}

func githubContextToolDefinition() map[string]any {
	return map[string]any{
		"type": "function", "name": "github_pr_context",
		"description": "Read bounded check status, effective review state, comment/review excerpts, and unresolved review-thread counts for the pull request already bound to this issue, repository, and branch. Read-only; it cannot select another repository, issue, branch, or pull request.",
		"inputSchema": map[string]any{"type": "object", "additionalProperties": false},
	}
}

func githubLandToolDefinition() map[string]any {
	return map[string]any{
		"type": "function", "name": "github_land_pr",
		"description": "Merge the pull request already bound to this issue, repository, base, and branch using the configured merge method, once required checks pass, reviews have no effective changes-requested state, and no review thread is unresolved. Returns a non-terminal waiting result while required checks or GitHub's mergeability computation are pending; with github.update_stale_branch enabled, one clean stale-base update also waits for checks on its new head. Other hard gates (failing checks, requested changes, unresolved threads, a stale base, conflicts, or a closed/mismatched PR) refuse landing. No repository, issue, branch, PR, method, state, or credential input.",
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
		input, err := githubhost.ParsePublishInput(request.Arguments)
		if err != nil {
			c.toolFailure(x.ID, "GitHub pull request publication arguments were rejected.")
			return
		}
		callID := callIDText(x.ID)
		started := c.emitCallStarted(callID, "github_publish_pr")
		result, err := c.github.Publish(c.ctx, input)
		if err != nil {
			c.emitCallFinished(callID, "github_publish_pr", domain.ItemFailed, started)
			c.toolFailure(x.ID, "GitHub pull request publication was rejected.")
			return
		}
		c.emitCallFinished(callID, "github_publish_pr", domain.ItemCompleted, started)
		content, _ := json.Marshal(map[string]any{"branch": result.Branch, "pull_request": result.URL, "number": result.Number, "body_updated": result.BodyUpdated})
		c.sendServerResponse(x.ID, map[string]any{"success": true, "contentItems": []any{map[string]any{"type": "inputText", "text": string(content)}}})
		return
	}
	if request.Tool == "github_pr_context" && c.github != nil {
		var args map[string]json.RawMessage
		if json.Unmarshal(request.Arguments, &args) != nil || len(args) != 0 {
			c.unsupportedTool(x.ID)
			return
		}
		callID := callIDText(x.ID)
		started := c.emitCallStarted(callID, "github_pr_context")
		result, err := c.github.Context(c.ctx)
		if err != nil {
			c.emitCallFinished(callID, "github_pr_context", domain.ItemFailed, started)
			c.toolFailure(x.ID, "GitHub pull request context request was rejected.")
			return
		}
		content, err := json.Marshal(result)
		if err != nil {
			c.emitCallFinished(callID, "github_pr_context", domain.ItemFailed, started)
			c.unsupportedTool(x.ID)
			return
		}
		c.emitCallFinished(callID, "github_pr_context", domain.ItemCompleted, started)
		c.sendServerResponse(x.ID, map[string]any{"success": true, "contentItems": []any{map[string]any{"type": "inputText", "text": string(content)}}})
		return
	}
	if request.Tool == "github_land_pr" && c.github != nil {
		var args map[string]json.RawMessage
		if json.Unmarshal(request.Arguments, &args) != nil || len(args) != 0 {
			c.unsupportedTool(x.ID)
			return
		}
		callID := callIDText(x.ID)
		started := c.emitCallStarted(callID, "github_land_pr")
		result, err := c.github.Land(c.ctx)
		if err != nil {
			c.emitCallFinished(callID, "github_land_pr", domain.ItemFailed, started)
			// A retryable landing gate is non-terminal: name the exact gate so
			// Codex can fix it, push, and call github_land_pr again in this turn.
			// Every reason is a fixed/config-derived, bounded, secret-free string
			// defined in the github package. Any other error keeps the generic
			// refusal message.
			var gate *githubhost.LandGateError
			if errors.As(err, &gate) && gate.Retryable {
				c.toolFailure(x.ID, "GitHub landing needs a fix: "+gate.Reason+".")
				return
			}
			c.toolFailure(x.ID, "GitHub pull request landing was rejected.")
			return
		}
		content, err := json.Marshal(result)
		if err != nil {
			c.emitCallFinished(callID, "github_land_pr", domain.ItemFailed, started)
			c.unsupportedTool(x.ID)
			return
		}
		c.emitCallFinished(callID, "github_land_pr", domain.ItemCompleted, started)
		c.sendServerResponse(x.ID, map[string]any{"success": true, "contentItems": []any{map[string]any{"type": "inputText", "text": string(content)}}})
		return
	}
	if request.Tool == "create_followup_issue" && c.handoff != nil {
		result, err := c.handoff.CreateFollowupIssue(c.ctx, request.Arguments)
		if err != nil {
			// Do not return provider errors, issue data, or any credential-derived
			// value to the child. The generic response is enough for the model to
			// choose another path, while the normalized event informs the scheduler.
			c.toolFailure(x.ID, "Linear follow-up issue creation was rejected.")
			return
		}
		text, err := json.Marshal(result.Data)
		if err != nil {
			c.unsupportedTool(x.ID)
			return
		}
		c.sendServerResponse(x.ID, map[string]any{"success": result.Success, "contentItems": []any{map[string]any{"type": "inputText", "text": string(text)}}})
		return
	}
	// No other client-side tool is bound: the agent has no Linear state-transition tool.
	c.unsupportedTool(x.ID)
}

// emitCallStarted and emitCallFinished report the lifecycle of Symphony's own
// bound dynamic tools (the fixed "github_publish_pr"/"github_land_pr" capability
// names, never a value read from the model's call arguments) so a slow GitHub
// round trip is visible the same way an app-server item is. The call ID is the
// protocol-assigned JSON-RPC request ID.
func (c *client) emitCallStarted(callID, tool string) time.Time {
	started := time.Now()
	c.emit(domain.Event{Kind: domain.EventItem, At: started, ItemID: callID, ItemType: "dynamicToolCall", ToolName: tool, Outcome: domain.ItemStarted})
	return started
}
func (c *client) emitCallFinished(callID, tool, outcome string, started time.Time) {
	c.emit(domain.Event{Kind: domain.EventItem, At: time.Now(), ItemID: callID, ItemType: "dynamicToolCall", ToolName: tool, Outcome: outcome, DurationMs: time.Since(started).Milliseconds()})
}
func callIDText(id any) string {
	return observability.Text(fmt.Sprint(id))
}

func (c *client) unsupportedTool(id any) {
	c.toolFailure(id, "Unsupported client-side tool.")
}

// finalizeLanding fires the deferred Merging -> In Review transition once when
// a Codex turn ends after a retryable github_land_pr gate but without a
// successful landing. It is a safe no-op when there is no bound GitHub session,
// when the bounded-fix feature is off, or when landing already resolved.
func (c *client) finalizeLanding() {
	if c.github != nil {
		c.github.FinalizeLanding(c.ctx)
	}
}

// Tool failures are normal app-server responses: the model can inspect the
// structured rejection and keep working in the same turn. EventBlocked is
// reserved for interactive approval or user-input requests.
func (c *client) toolFailure(id any, message string) {
	c.sendServerResponse(id, map[string]any{"success": false, "contentItems": []any{map[string]any{"type": "inputText", "text": message}}})
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
