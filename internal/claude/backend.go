// Package claude runs turns on the Claude Code CLI as a Symphony agent backend.
//
// The shape differs from the Codex app-server in one way that drives this whole
// package: `claude --print` runs a single turn and exits. There is no long-lived
// process to send a second turn to, so a continuation spawns a new process and
// resumes the session by ID. Symphony assigns that ID up front rather than
// reading it back, which removes a start-time race and makes the session
// identity known before the child exists.
//
// The launch policy is fixed by launch.go, not configurable, and the only
// confirmation that it applied is the CLI's own init event -- see verifyInit.
package claude

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/domain"
	"github.com/pmrrasmussen/symphony/internal/observability"
)

// maxLine bounds one stdout line. A single assistant message or tool result is
// one line and is routinely large, so an oversized line is normal traffic here
// and is skipped rather than failing the run.
const maxLine = 8 << 20

// eventBuffer leaves room for the terminal event even when a consumer stops
// reading: the coordinator returns as soon as it sees a terminal event, so a
// blocking send afterwards would leak this goroutine and orphan the child.
const eventBuffer = 64

// reservedTerminalSlots keeps space for the terminal event by dropping ordinary
// progress once the buffer is nearly full.
const reservedTerminalSlots = 2

// waitDelay bounds Wait's post-exit I/O wait. It is short because by the time it
// applies the process is already gone and only an escaped descendant can still
// be holding a pipe.
const waitDelay = 2 * time.Second

// Backend implements domain.AgentBackend on the Claude Code CLI.
type Backend struct {
	settings    func() config.Settings
	secretNames []string

	mu       sync.Mutex
	sessions map[string]*session
}

// session is the per-run state that outlives a single turn: the assigned
// session ID, the turn counter, cumulative usage, and whichever process is
// currently running.
type session struct {
	id string

	mu   sync.Mutex
	turn int
	// request is the first turn's launch request. A resume must re-apply the
	// entire contract -- the CLI restores none of --settings, --tools, or the
	// permission mode -- and the workspace and Git metadata roots are not
	// derivable from a session ID.
	request domain.AgentRequest
	usage   domain.Usage
	running *turn
}

// New builds a Claude backend. secretNames are environment variable names whose
// values must never reach the child.
func New(settings func() config.Settings, secretNames ...string) *Backend {
	return &Backend{settings: settings, secretNames: secretNames, sessions: map[string]*session{}}
}

// Start assigns a session ID and runs the first turn.
func (b *Backend) Start(ctx context.Context, r domain.AgentRequest) (domain.AgentSession, <-chan domain.Event, error) {
	id, err := newSessionID()
	if err != nil {
		return domain.AgentSession{}, nil, err
	}
	s := &session{id: id}
	b.mu.Lock()
	b.sessions[id] = s
	b.mu.Unlock()
	events, err := b.run(ctx, s, r, false)
	if err != nil {
		b.forget(id)
		return domain.AgentSession{}, nil, err
	}
	return domain.AgentSession{ID: id, ThreadID: id, TurnID: "1"}, events, nil
}

// Continue resumes the session in a new process. The launch policy is rebuilt
// from the request the caller supplies, because none of it survives a resume:
// the CLI does not restore --settings, --mcp-config, --tools, or the permission
// mode, so every turn must re-apply the whole contract or the boundary silently
// disappears after the first turn.
func (b *Backend) Continue(ctx context.Context, agentSession domain.AgentSession, prompt string) (<-chan domain.Event, error) {
	b.mu.Lock()
	s := b.sessions[agentSession.ID]
	b.mu.Unlock()
	if s == nil {
		return nil, errors.New("unknown claude session")
	}
	s.mu.Lock()
	r := s.request
	s.mu.Unlock()
	r.Prompt = prompt
	return b.run(ctx, s, r, true)
}

// Cancel terminates whichever turn is running and forgets the session. Between
// turns there is no process at all, so an idle session cancels without error --
// unlike a long-lived app-server, where cancellation always has something to
// signal.
func (b *Backend) Cancel(ctx context.Context, agentSession domain.AgentSession) error {
	b.mu.Lock()
	s := b.sessions[agentSession.ID]
	delete(b.sessions, agentSession.ID)
	b.mu.Unlock()
	if s == nil {
		return nil
	}
	s.mu.Lock()
	active := s.running
	s.mu.Unlock()
	if active == nil {
		return nil
	}
	active.kill()
	select {
	case <-active.exited:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *Backend) forget(id string) {
	b.mu.Lock()
	delete(b.sessions, id)
	b.mu.Unlock()
}

// run spawns one turn and returns its event stream.
func (b *Backend) run(ctx context.Context, s *session, r domain.AgentRequest, resume bool) (<-chan domain.Event, error) {
	args, err := launchArgs(r, s.id, resume)
	if err != nil {
		return nil, err
	}
	// The spawn happens under the session lock so a cancellation cannot arrive
	// between the child starting and the session recording it -- that window
	// would kill nothing and leave the process orphaned.
	s.mu.Lock()
	s.turn++
	turnNumber := s.turn
	if !resume {
		s.request = r
	}
	t, err := spawn(ctx, r, args, b.secretNames, b.settings)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	s.running = t
	s.mu.Unlock()

	events := make(chan domain.Event, eventBuffer)
	go t.stream(s, r, turnNumber, events)
	return events, nil
}

// turn is one child process.
type turn struct {
	cmd    *exec.Cmd
	stdout io.ReadCloser
	stderr io.ReadCloser
	exited chan struct{}

	timeout time.Duration

	// killOnce bounds the process-group signal to one delivery per turn. The
	// group kill deliberately bypasses Go's post-Wait guard, so repeating it
	// after the child is reaped could signal an unrelated group once the pid is
	// recycled.
	killOnce sync.Once
	// closeIO closes the parent's ends of the pipes exactly once.
	closeIO sync.Once

	mu     sync.Mutex
	killed bool
}

// spawn starts the CLI with the prompt on stdin and a scrubbed environment.
func spawn(ctx context.Context, r domain.AgentRequest, args []string, secretNames []string, settings func() config.Settings) (*turn, error) {
	command := strings.TrimSpace(r.Command)
	if command == "" {
		command = "claude"
	}
	// The command is a configured program name, and the arguments are built
	// here, so they are passed directly rather than through a shell: the
	// settings payload is JSON and must not be word-split or expanded.
	fields := strings.Fields(command)
	cmd := exec.CommandContext(ctx, fields[0], append(fields[1:], args...)...)
	cmd.Dir = r.Workspace
	cmd.Env = filteredEnv(secretNames, settings)
	// A process group is what makes cancellation reach the CLI's own children:
	// a killed turn can leave background shell commands behind otherwise.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// Go closes a command's parent-side pipes in Start and Wait, so a failure
	// between creating one and reaching Start would leak the descriptors -- and
	// this only happens under descriptor exhaustion, where a leak compounds.
	var opened []io.Closer
	closeOpened := func() {
		for _, pipe := range opened {
			_ = pipe.Close()
		}
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	opened = append(opened, stdout)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		closeOpened()
		return nil, err
	}
	opened = append(opened, stderr)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		closeOpened()
		return nil, err
	}
	opened = append(opened, stdin)
	t := &turn{cmd: cmd, stdout: stdout, stderr: stderr, exited: make(chan struct{}), timeout: r.TurnTimeout}
	cmd.Cancel = func() error { t.kill(); return nil }
	// WaitDelay bounds how long Wait blocks on I/O after the process itself is
	// gone, so a descendant still holding an inherited pipe cannot keep Wait
	// from returning.
	cmd.WaitDelay = waitDelay
	if err := cmd.Start(); err != nil {
		closeOpened()
		return nil, err
	}
	// The prompt goes on stdin, never in the arguments: several launch flags are
	// variadic and would swallow a trailing positional prompt. Closing stdin is
	// required either way -- the CLI waits on it before proceeding.
	go func() {
		_, _ = io.WriteString(stdin, r.Prompt)
		_ = stdin.Close()
	}()
	return t, nil
}

func (t *turn) killProcessGroup() error {
	if t.cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-t.cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

// kill terminates the turn. Killing the process group is not sufficient on its
// own: a descendant that leaves the group -- setsid, nohup, any double fork --
// keeps the inherited stdout write end open, so the reader would never see EOF
// and the turn would hang with no terminal event and no closed channel. Closing
// the parent's ends of the pipes is what actually ends the read.
func (t *turn) kill() {
	t.mu.Lock()
	t.killed = true
	t.mu.Unlock()
	t.killOnce.Do(func() { _ = t.killProcessGroup() })
	t.closePipes()
}

func (t *turn) closePipes() {
	t.closeIO.Do(func() {
		_ = t.stdout.Close()
		_ = t.stderr.Close()
	})
}

func (t *turn) cancelled() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.killed
}

// stream reads the turn's stdout, normalizes it, and closes the channel after
// exactly one terminal event.
func (t *turn) stream(s *session, r domain.AgentRequest, turnNumber int, events chan<- domain.Event) {
	defer close(events)
	defer close(t.exited)
	// Once this turn is over it must stop being the session's live process, or a
	// later cancellation would signal a process group whose pid has been reaped
	// and possibly recycled.
	defer func() {
		s.mu.Lock()
		if s.running == t {
			s.running = nil
		}
		s.mu.Unlock()
	}()

	emitter := &emitter{events: events}
	pending := map[string]pendingCall{}

	// The turn budget is enforced here rather than by the context, so the
	// timeout is reported as a normalized failure instead of an opaque kill.
	var timer *time.Timer
	timedOut := make(chan struct{})
	if t.timeout > 0 {
		timer = time.AfterFunc(t.timeout, func() {
			close(timedOut)
			// kill closes the pipes as well, which is what unblocks the read
			// loop below and lets this turn report its own timeout.
			t.kill()
		})
		defer timer.Stop()
	}

	stderr := &boundedTail{}
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		stderr.readFrom(t.stderr)
	}()

	lines := newLineReader(t.stdout)
	var sawTerminal bool
	var initVerified bool
	var readErr error
	for {
		line, skipped, err := lines.next()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				readErr = err
			}
			break
		}
		if skipped {
			// An over-long line is discarded, but reading continues: stopping
			// would block the child on a full pipe and hang the turn.
			continue
		}
		envelope, ok := decode(line)
		if !ok {
			// Undecodable output is skipped too. It is the child's output, and
			// one bad line must not end a run that is otherwise progressing.
			continue
		}
		switch envelope.Type {
		case "system":
			switch envelope.Subtype {
			case "init":
				var event initEvent
				_ = json.Unmarshal(line, &event)
				if refusal := verifyInit(event, r.Workspace); refusal != "" {
					// The policy did not apply. Fail closed rather than run a
					// turn under an unknown boundary.
					emitter.emit(domain.Event{Kind: domain.EventFailed, At: time.Now(), Message: refusal})
					sawTerminal = true
					t.kill()
					continue
				}
				initVerified = true
				emitter.emit(domain.Event{
					Kind: domain.EventSessionStarted, At: time.Now(),
					SessionID: s.id, ThreadID: s.id, TurnID: strconv.Itoa(turnNumber),
					PID: t.cmd.Process.Pid,
				})
			case "permission_denied":
				var event permissionDeniedEvent
				_ = json.Unmarshal(line, &event)
				emitter.emit(domain.Event{
					Kind: domain.EventItem, At: time.Now(),
					ItemID:   observability.Text(event.ToolUseID),
					ItemType: itemType(event.ToolName),
					ToolName: observability.Text(event.ToolName),
					Outcome:  domain.ItemDeclined,
				})
			}
		case "assistant":
			var message assistantMessage
			_ = json.Unmarshal(line, &message)
			for _, content := range message.Message.Content {
				if content.Type != "tool_use" || content.ID == "" {
					continue
				}
				pending[content.ID] = pendingCall{tool: content.Name, started: time.Now()}
				emitter.emit(domain.Event{
					Kind: domain.EventItem, At: time.Now(),
					ItemID:   observability.Text(content.ID),
					ItemType: itemType(content.Name),
					ToolName: observability.Text(content.Name),
					Outcome:  domain.ItemStarted,
				})
			}
		case "user":
			var message userMessage
			_ = json.Unmarshal(line, &message)
			for _, content := range message.Message.Content {
				if content.Type != "tool_result" || content.ToolUseID == "" {
					continue
				}
				call, known := pending[content.ToolUseID]
				delete(pending, content.ToolUseID)
				outcome := domain.ItemCompleted
				if content.IsError {
					outcome = domain.ItemFailed
				}
				event := domain.Event{
					Kind: domain.EventItem, At: time.Now(),
					ItemID:   observability.Text(content.ToolUseID),
					ItemType: itemType(call.tool),
					ToolName: observability.Text(call.tool),
					Outcome:  outcome,
				}
				if known {
					event.DurationMs = time.Since(call.started).Milliseconds()
				}
				emitter.emit(event)
			}
		case "rate_limit_event":
			var event rateLimitEvent
			_ = json.Unmarshal(line, &event)
			// This is reported as a diagnostic rather than EventRateLimit on
			// purpose. The scheduler normalizes rate-limit payloads through a
			// fixed numeric allowlist (limit, remaining, used, reset_seconds,
			// window_seconds); the CLI's actionable fields are strings under
			// different names, so an EventRateLimit here would be silently
			// discarded and never reach a log.
			emitter.emit(domain.Event{Kind: domain.EventDiagnostic, At: time.Now(),
				Message: "claude reported a rate limit: " + observability.Text(firstNonEmpty(event.RateLimitInfo.Status, "unspecified")) +
					" (" + observability.Text(firstNonEmpty(event.RateLimitInfo.RateLimitType, "unspecified")) + ")"})
		case "result":
			if sawTerminal {
				// A refused init already ended this turn. Reading a later result
				// would emit a second terminal event and misreport the reason.
				continue
			}
			var event resultEvent
			_ = json.Unmarshal(line, &event)
			// The CLI reports usage per turn while the scheduler keeps a
			// component-wise maximum across a run, so the running total is
			// accumulated here.
			s.mu.Lock()
			s.usage = add(s.usage, event.Usage.totals())
			total := s.usage
			s.mu.Unlock()
			if total != (domain.Usage{}) {
				emitter.emit(domain.Event{Kind: domain.EventUsage, At: time.Now(), Usage: total})
			}
			for _, denial := range event.PermissionDenials {
				emitter.emit(domain.Event{Kind: domain.EventDiagnostic, At: time.Now(),
					Message: "claude denied a tool call: " + observability.Text(denial.ToolName)})
			}
			sawTerminal = true
			if event.IsError {
				// is_error is the authoritative failure signal: an
				// authentication failure arrives with subtype "success".
				emitter.emit(domain.Event{Kind: domain.EventFailed, At: time.Now(),
					Message: "claude turn failed: " + observability.Text(firstNonEmpty(event.TerminalReason, event.APIErrorStatus, event.StopReason, "unspecified"))})
				continue
			}
			if !initVerified {
				// A turn that never announced its policy is not a turn whose
				// boundary is known.
				emitter.emit(domain.Event{Kind: domain.EventFailed, At: time.Now(), Message: "claude session refused: no init event was reported"})
				continue
			}
			emitter.emit(domain.Event{Kind: domain.EventCompleted, At: time.Now()})
		}
	}
	<-stderrDone
	waitErr := t.cmd.Wait()
	// Kill the group again: the leader can exit while descendants still hold
	// inherited pipes.
	_ = t.killProcessGroup()

	if sawTerminal {
		return
	}
	select {
	case <-timedOut:
		emitter.emit(domain.Event{Kind: domain.EventFailed, At: time.Now(), Message: "claude turn timeout"})
		return
	default:
	}
	if t.cancelled() {
		emitter.emit(domain.Event{Kind: domain.EventFailed, At: time.Now(), Message: "claude turn cancelled"})
		return
	}
	if tail := stderr.text(); tail != "" {
		emitter.emit(domain.Event{Kind: domain.EventDiagnostic, At: time.Now(), Message: tail})
	}
	switch {
	case readErr != nil:
		emitter.emit(domain.Event{Kind: domain.EventFailed, At: time.Now(), Message: "claude stdout read failed"})
	case waitErr != nil:
		emitter.emit(domain.Event{Kind: domain.EventFailed, At: time.Now(), Message: "claude exited without completing the turn: " + exitText(waitErr)})
	default:
		emitter.emit(domain.Event{Kind: domain.EventFailed, At: time.Now(), Message: "claude exited without reporting a result"})
	}
}

type pendingCall struct {
	tool    string
	started time.Time
}

func add(a, b domain.Usage) domain.Usage {
	return domain.Usage{
		InputTokens:  a.InputTokens + b.InputTokens,
		OutputTokens: a.OutputTokens + b.OutputTokens,
		TotalTokens:  a.TotalTokens + b.TotalTokens,
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// exitText reports an exit status without the child's own output.
func exitText(err error) string {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return "exit status " + strconv.Itoa(exit.ExitCode())
	}
	return "process error"
}

// emitter never blocks. A consumer stops reading as soon as it sees a terminal
// event, so ordinary progress is dropped once the buffer is nearly full and the
// terminal event keeps the reserved room.
type emitter struct {
	events chan<- domain.Event
}

func (e *emitter) emit(event domain.Event) {
	if terminal(event.Kind) {
		select {
		case e.events <- event:
		default:
		}
		return
	}
	if len(e.events) >= cap(e.events)-reservedTerminalSlots {
		return
	}
	select {
	case e.events <- event:
	default:
	}
}

func terminal(kind domain.EventKind) bool {
	switch kind {
	case domain.EventCompleted, domain.EventFailed, domain.EventBlocked,
		domain.EventLandingWaiting, domain.EventLandingResolved:
		return true
	}
	return false
}

// boundedTail keeps only the last bounded, redacted slice of stderr, so a noisy
// child cannot flood a log and no unbounded child output is retained.
type boundedTail struct {
	mu   sync.Mutex
	tail []byte
}

func (b *boundedTail) readFrom(r io.Reader) {
	buf := make([]byte, 4<<10)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			b.mu.Lock()
			b.tail = append(b.tail, buf[:n]...)
			if len(b.tail) > observability.MaxDiagnosticBytes {
				b.tail = b.tail[len(b.tail)-observability.MaxDiagnosticBytes:]
			}
			b.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

func (b *boundedTail) text() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.tail) == 0 {
		return ""
	}
	return observability.Text(string(b.tail))
}

// newSessionID mints the UUID the CLI is told to use. Assigning it means the
// session identity exists before the child does, so a child that dies before
// announcing itself is still addressable and resumable.
func newSessionID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate claude session id: %w", err)
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16]), nil
}
