// Package codex adapts the Codex app-server JSON-RPC stdio protocol.
package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/pmrrasmussen/symphony/internal/capability"
	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/domain"
	githubhost "github.com/pmrrasmussen/symphony/internal/github"
	"github.com/pmrrasmussen/symphony/internal/hostenv"
	"github.com/pmrrasmussen/symphony/internal/linear"
	"github.com/pmrrasmussen/symphony/internal/observability"
	"github.com/pmrrasmussen/symphony/internal/procgroup"
)

type Backend struct {
	mu          sync.Mutex
	sessions    map[string]*client
	secretNames []string
	settings    func() config.Settings
	handoff     *linear.Handoff
	github      *githubhost.Manager
	// timer schedules every bound a session enforces itself. It defaults to the
	// real one and is replaced only by a test. See Timer.
	timer Timer
}

// finalizeBudget bounds the turn-ended finalizer's own work, which runs detached
// from the run's cancellation (see finalizeLanding).
//
// The work is a handful of sequential Linear GraphQL round trips: the deferred
// transition re-reads the issue, resolves the team's states, transitions,
// re-reads to confirm, and comments. Seconds is the right order of magnitude,
// and the ceiling that actually binds is the coordinator's: it waits exactly five
// seconds for agent.Cancel to return, and a hard Cancel runs this finalizer
// before returning. A longer budget does not lose the transition -- the
// cancellation goroutine outlives the coordinator's wait -- but it does make the
// coordinator log "agent cancellation timed out" for a session that is shutting
// down exactly as designed, so five seconds is what keeps a correct cancel quiet.
// The Claude transport gives the same finalizer the same budget, derived at its
// own transport's only correct point: see mcpbridge.finalizerBudget.
//
// Two ceilings that look like they bind do not, and are recorded so they are not
// mistaken for constraints later. The daemon's twenty-second graceful shutdown
// deadline waits on the run goroutine, which is blocked on the child's exit
// rather than on this call. And the endpoint's thirty-second finalizer bound is
// on the other transport and starts after its drain, so it shares no clock with
// this.
//
// The price of expiry is not "the transition is retried later". It is that the
// transition is lost: the GitHub session latches its deferred-fired flag before
// attempting anything, so an expired finalizer leaves the issue in Merging with
// no later turn end that will attempt it again. Latching on success instead is a
// change to that provider's idempotency contract and is deliberately not made
// here.
const finalizeBudget = 5 * time.Second

// New builds a Codex backend. secretNames are extra environment variable names
// this child may not inherit, on top of everything hostenv.Filter blocks for
// every child Symphony spawns.
func New(secretNames ...string) *Backend {
	return &Backend{sessions: map[string]*client{}, secretNames: uniquePaths(secretNames), timer: realTimer{}}
}

// NewWithProviders binds already-built host providers to this backend instead
// of constructing them. Both are process-wide and neither belongs to Codex, but
// the GitHub manager is why this seam has to exist: one manager owns the linked
// pull request table its poll loop walks and the exactly-once Linear completion
// guard, so a process holding two of them would poll one table while sessions
// write into the other, and a merged pull request would complete its issue
// twice or never. While the manager could only be obtained by constructing a
// Codex backend, no other backend could be given the one the host polls.
// A nil provider leaves its capabilities unbound, exactly as an unconfigured
// integration does.
//
// settings must be the same callback the two providers were built from. It is a
// separate parameter only because neither provider exposes the closure it
// captured, and Go cannot compare closures, so this cannot be enforced here:
// Start freezes one settings snapshot for the session (which capabilities exist,
// and the config.GitHub the session is bound to) while the manager independently
// reads its own callback for Enabled, MatchesSecret, and the read-only
// VerifyLanded. Feeding them different callbacks makes those disagree -- a
// session that froze GitHub as disabled beside a landing verifier that sees it
// enabled would let terminal cleanup discard local commits for an issue no
// session ever published.
func NewWithProviders(settings func() config.Settings, handoff *linear.Handoff, github *githubhost.Manager, secretNames ...string) *Backend {
	b := New(secretNames...)
	b.settings = settings
	b.handoff = handoff
	b.github = github
	return b
}

// GitHubManager reports the manager this backend was given. The host reads its
// poll loop and landing-verifier target back out of the backend rather than
// keeping a local of its own, so the instance it polls cannot drift from the
// instance sessions write into: there is deliberately no constructor that mints
// a manager for a single backend, because every such call would produce a
// second linked-pull-request table no poll loop walks. It is nil when GitHub is
// unwired, which leaves cleanup verification and polling as strict as they were
// before the integration existed.
func (b *Backend) GitHubManager() *githubhost.Manager { return b.github }
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
	var githubSession *githubhost.Session
	if b.github != nil {
		githubSession = b.github.PrepareWithSettings(settings.GitHub, r.Issue, r.Workspace, handoff)
	}
	r.TurnSandboxPolicy = localCommitSandbox(r)
	// The registry is per session and holds these same provider session
	// pointers, because every per-run idempotency latch lives in them. The
	// secret matcher is derived from the same bindings, so the providers this
	// session can reach and the credentials it strips cannot disagree.
	bindings := capability.Bindings{Settings: settings, Issue: r.Issue, Handoff: handoff, GitHub: githubSession}
	capabilities := capability.Build(bindings)
	// The child environment is filtered by hostenv, which every child Symphony
	// spawns shares: this backend adds no name of its own beyond the ones its
	// constructor was given, and nothing at all after filtering.
	environment := hostenv.Filter(os.Environ(), b.secretNames, settings, capability.SecretMatcher(bindings, b.github))
	c, err := start(ctx, r, environment, capabilities, b.timer)
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
	if tools := dynamicTools(c.capabilities); len(tools) > 0 {
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
	// The kill comes first because it is immediate and is what a caller of Cancel
	// needs promptly, while the finalizer below can spend its whole budget on an
	// unresponsive tracker. Ordering them the other way left a child the
	// coordinator had already decided to stop running for that entire budget --
	// which cost nothing while the finalizer ran on a dead context and did no
	// work, and costs seconds now that it does (PMR-95).
	c.kill()
	// A hard cancel may pre-empt turn/completed, so finalize the deferred
	// landing refusal here too. It is idempotent and a no-op when unused.
	c.finalizeLanding()
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
	timer               Timer
	ctx                 context.Context
	capabilities        *capability.Registry
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
	usageMissOnce       sync.Once
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

// start spawns the app-server with an already-filtered environment. It takes
// the entries rather than the inputs to hostenv.Filter because the filter's
// inputs are the session's -- the settings snapshot Start froze and the matcher
// built from its bindings -- and because a nil slice here would hand the child
// the daemon's complete environment, which is exactly what the filter exists to
// prevent.
func start(ctx context.Context, r domain.AgentRequest, environment []string, capabilities *capability.Registry, timer Timer) (*client, error) {
	command := strings.TrimSpace(r.Command)
	if command == "" {
		command = "codex app-server"
	}
	cmd := exec.CommandContext(ctx, "bash", "-lc", command)
	cmd.Dir = r.Workspace
	cmd.Env = environment
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
	c := &client{cmd: cmd, in: in, workspace: r.Workspace, approval: r.ApprovalPolicy, policy: r.TurnSandboxPolicy, readTimeout: r.ReadTimeout, startTimeout: r.StartTimeout, turnTimeout: r.TurnTimeout, timer: timer, ctx: ctx, capabilities: capabilities, pending: map[int]chan callResult{}, done: make(chan struct{}), exited: make(chan struct{})}
	cmd.Cancel = func() error { return procgroup.Kill(c.cmd) }
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
		_ = procgroup.Kill(c.cmd)
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

// call applies the steady-state read timeout to a single JSON-RPC round trip.
// The cold-start handshake and thread/start use callWithTimeout with the
// separate, more generous start timeout so a first model load does not trip
// mid-turn hang detection.
func (c *client) call(ctx context.Context, method string, params any) (map[string]any, error) {
	return c.callWithTimeout(ctx, method, params, c.readTimeout)
}

// callWithTimeout bounds one round trip with the budget it is given.
//
// The budget is scheduled on the timer seam rather than folded into ctx, which
// keeps two bounds that expire for different reasons distinguishable: this
// call's own timeout, and the caller's cancellation. It is also what makes the
// bound testable without waiting it out -- and, in a test, which budget is live
// while a call is outstanding is the whole of "thread/start is governed by the
// start timeout, not the read timeout" (see Timer).
func (c *client) callWithTimeout(ctx context.Context, method string, params any, timeout time.Duration) (map[string]any, error) {
	expired := make(chan struct{})
	if timeout > 0 {
		stop := c.timer.AfterFunc(timeout, func() { close(expired) })
		defer stop()
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
	case <-expired:
		c.removePending(id)
		err := fmt.Errorf("codex %s timed out: %w", method, context.DeadlineExceeded)
		c.abort(err)
		return nil, err
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
		stop := c.timer.AfterFunc(r.TurnTimeout, func() {
			c.emit(domain.Event{Kind: domain.EventFailed, At: time.Now(), Message: "Codex turn timeout"})
			c.kill()
		})
		// The budget is cancelled when the turn ends, so a turn that finished
		// well inside it neither kills a process group the next turn is using nor
		// leaves a timer alive for the rest of the run.
		go func() {
			<-turnDone
			stop()
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
	if method == "thread/tokenUsage/updated" {
		c.emitTokenUsage(x.Params)
		return
	}
	if method == "turn/completed" {
		c.finalizeLanding()
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

// dynamicTools wraps agent-neutral capability definitions in the app-server's
// own dynamic-tool envelope. Order follows the registry, which is stable.
func dynamicTools(registry *capability.Registry) []map[string]any {
	definitions := registry.Definitions()
	tools := make([]map[string]any, 0, len(definitions))
	for _, definition := range definitions {
		tools = append(tools, map[string]any{
			"type": "function", "name": definition.Name,
			"description": definition.Description, "inputSchema": definition.InputSchema,
		})
	}
	return tools
}

// handleToolCall decodes one app-server tool call and hands it to the shared
// capability dispatch, which owns the whole lookup-to-terminal-event sequence
// for both of Symphony's agent transports. What is left here is what is
// genuinely this protocol's: the request envelope, the JSON-RPC ID that both
// answers the call and identifies it in item records, and the response envelope.
func (c *client) handleToolCall(x rpc) {
	var request struct {
		Tool      string          `json:"tool"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(x.Params, &request); err != nil {
		// An envelope this adapter cannot read names no capability, so it is
		// answered exactly as an unknown one is.
		c.respondToCall(x.ID, capability.Outcome{Refusal: capability.Unsupported()})
		return
	}
	capability.Dispatch(c.ctx, c.capabilities, capability.Transport{
		// The protocol-assigned request ID: it is the app-server's own, not a
		// value the model chose, and it is the identity the response is keyed to.
		CallID:  callIDText(x.ID),
		Respond: func(outcome capability.Outcome) { c.respondToCall(x.ID, outcome) },
		Emit:    c.emit,
		// Neither gate applies here. Advertisement is this transport's only route
		// to a call, so dispatch stays open and each provider re-validates its own
		// preconditions; and a call runs inline on the read loop, so there is no
		// concurrent second invocation for a slot to refuse.
	}, request.Tool, request.Arguments)
}

// respondToCall frames one dispatched outcome in the app-server's tool response
// envelope. A refusal is a normal app-server response carrying success:false --
// the model can inspect the structured rejection and keep working in the same
// turn -- so EventBlocked stays reserved for interactive approval or user-input
// requests.
func (c *client) respondToCall(id any, outcome capability.Outcome) {
	text, success := string(outcome.Payload), outcome.Success
	if outcome.Refusal != nil {
		text, success = outcome.Refusal.Message, false
	}
	c.sendServerResponse(id, map[string]any{"success": success, "contentItems": []any{map[string]any{"type": "inputText", "text": text}}})
}

func callIDText(id any) string {
	return observability.Text(fmt.Sprint(id))
}

// finalizeLanding settles capability state that outlives a single call once this
// turn ends, however it ended. It is idempotent, so the three paths that can end
// a turn -- turn/completed, turn/failed or turn/cancelled, and a hard Cancel --
// all call it.
//
// It deliberately does not run on the run-lived context Start was given, because
// by the time this matters that context is already dead. The coordinator stops a
// run by cancelling that very context and only then cancelling the session
// (Coordinator.stopRun, and Shutdown), so a finalizer inheriting it could not
// issue the deferred Merging -> In Review transition at all (PMR-95).
//
// What that costs is not a stalled issue: the configured Merging state is a
// workflow active state, so the coordinator redispatches the issue. It is that
// the redispatch is the same landing attempt against the same failed gate, on an
// issue no human has been told to look at, holding the one state-aware Merging
// slot while it loops. The transition is what breaks that loop.
//
// Dropping the run's cancellation is therefore the point, and finalizeBudget is
// what keeps that from being unbounded.
func (c *client) finalizeLanding() {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.ctx), finalizeBudget)
	defer cancel()
	c.capabilities.TurnEnded(ctx)
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
			if e.Kind.Terminal() && c.pendingTerminal == nil {
				copy := e
				c.pendingTerminal = &copy
			} else if c.pendingTerminal == nil && len(c.pendingEvents) < cap(ch)-2 {
				c.pendingEvents = append(c.pendingEvents, e)
			}
			c.mu.Unlock()
			return
		}
		if e.Kind.Terminal() {
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
		_ = procgroup.Kill(c.cmd)
	})
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
		message := strings.TrimSuffix(strings.TrimSuffix(string(line), "\n"), "\r")
		if oversized {
			report(message + "…[truncated]")
		} else if message != "" {
			report(message)
		}
		line = line[:0]
		oversized = false
		if errors.Is(err, io.EOF) {
			return nil
		}
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
func asID(v any) (int, bool) {
	x, ok := v.(float64)
	if !ok || x < 0 || x != math.Trunc(x) || x > float64(^uint(0)>>1) {
		return 0, false
	}
	return int(x), true
}

// tokenUsageBreakdown is the app-server's own TokenUsageBreakdown, confirmed
// against codex-cli 0.149.1's generated protocol schema (`codex app-server
// generate-json-schema`): cachedInputTokens, cacheWriteInputTokens,
// inputTokens, outputTokens, reasoningOutputTokens, and totalTokens are its
// complete, required field set. Folding the cache and reasoning components
// into InputTokens/OutputTokens mirrors how the Claude backend folds its own
// cache tokens into input (claude/events.go's usage.totals): both are tokens
// the model actually processed, just billed under a component domain.Usage
// has no field of its own for.
type tokenUsageBreakdown struct {
	CachedInputTokens     int64 `json:"cachedInputTokens"`
	CacheWriteInputTokens int64 `json:"cacheWriteInputTokens"`
	InputTokens           int64 `json:"inputTokens"`
	OutputTokens          int64 `json:"outputTokens"`
	ReasoningOutputTokens int64 `json:"reasoningOutputTokens"`
	TotalTokens           int64 `json:"totalTokens"`
}

func (b tokenUsageBreakdown) totals() domain.Usage {
	return domain.Usage{
		InputTokens:  b.InputTokens + b.CachedInputTokens + b.CacheWriteInputTokens,
		OutputTokens: b.OutputTokens + b.ReasoningOutputTokens,
		TotalTokens:  b.TotalTokens,
	}
}

// emitTokenUsage decodes one thread/tokenUsage/updated notification -- the
// app-server's *only* notification that carries usage. Neither turn/completed
// nor any item notification has a usage field: Turn, the payload
// turn/completed itself carries, has none at all, confirmed against the same
// generated schema that gives tokenUsageBreakdown its field names above. The
// notification's total is the thread's own running sum across every turn so
// far -- monotonically increasing by construction -- which is why it is
// reported non-authoritative: the coordinator folds repeated or reordered
// figures with a component-wise maximum (PMR-153), and that merge is exactly
// what a monotonic running total wants.
//
// Reporting from this notification rather than only at turn/completed is
// also what makes cancellation harmless: Symphony's own publish flow ends a
// successful run by cancelling the session the moment the landing capability
// resolves, which kills the app-server before it ever reaches turn/completed
// (PMR-155). This notification arrives during the turn, so whatever usage the
// CLI already reported survives a cancellation that pre-empts the turn's own
// end.
func (c *client) emitTokenUsage(raw json.RawMessage) {
	var notification struct {
		TokenUsage struct {
			Total tokenUsageBreakdown `json:"total"`
		} `json:"tokenUsage"`
	}
	if err := json.Unmarshal(raw, &notification); err != nil {
		return
	}
	usage := notification.TokenUsage.Total.totals()
	if usage == (domain.Usage{}) {
		// A silent zero here is indistinguishable from a run that genuinely
		// spent nothing -- which is how a broken extraction went unnoticed for
		// the whole life of the Codex backend (PMR-155). One diagnostic per
		// session is enough to make a schema change or an empty payload
		// visible without flooding a log a chatty notification stream already
		// fills.
		c.usageMissOnce.Do(func() {
			c.diagnostic("codex thread/tokenUsage/updated reported no usage")
		})
		return
	}
	c.emit(domain.Event{Kind: domain.EventUsage, At: time.Now(), Usage: usage})
}
