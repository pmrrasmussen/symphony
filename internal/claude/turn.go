package claude

import (
	"context"
	"io"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/pmrrasmussen/symphony/internal/domain"
	"github.com/pmrrasmussen/symphony/internal/procgroup"
)

// waitDelay bounds Wait's post-exit I/O wait. It is short because by the time it
// applies the process is already gone and only an escaped descendant can still
// be holding a pipe.
const waitDelay = 2 * time.Second

// turn is one child process.
type turn struct {
	cmd    *exec.Cmd
	stdout io.ReadCloser
	stderr io.ReadCloser
	exited chan struct{}

	timeout time.Duration
	// timer is the seam the turn's budget is scheduled on, carried from the
	// backend so a test can decide when that budget elapses. See Timer.
	timer Timer

	// killOnce bounds the process-group signal to one delivery per turn. The
	// group kill deliberately bypasses Go's post-Wait guard, so repeating it
	// after the child is reaped could signal an unrelated group once the pid is
	// recycled.
	killOnce sync.Once
	// closeIO closes the parent's ends of the pipes exactly once.
	closeIO sync.Once

	// sink is the only route from any goroutine to this turn's event channel. It
	// belongs to the turn rather than to stream's locals because the read loop is
	// not the only thing that can have something to report about a turn, and it is
	// built before the turn so the capability endpoint can be given it before the
	// child that reaches that endpoint exists.
	sink *sink

	// contract is what this turn was launched under, carried so verifyInit checks
	// the echo against the argument vector that produced it. See launchContract.
	contract launchContract
	// registration is this turn's capability-endpoint authority, retired when the
	// turn ends however it ends. It is nil for a turn with no endpoint.
	registration *registration
	// endpointURL and endpointToken are kept only so child output can be
	// scrubbed of them. See withoutEndpoint.
	endpointURL   string
	endpointToken string

	mu     sync.Mutex
	killed bool
}

// spawn starts the CLI with the prompt on stdin and a scrubbed environment.
func spawn(ctx context.Context, r domain.AgentRequest, contract launchContract, environment []string, events *sink, endpoint *capabilityEndpoint, timer Timer) (*turn, error) {
	command := strings.TrimSpace(r.Command)
	if command == "" {
		command = "claude"
	}
	// The command is a configured program name, and the arguments are built
	// here, so they are passed directly rather than through a shell: the
	// settings payload is JSON and must not be word-split or expanded.
	fields := strings.Fields(command)
	cmd := exec.CommandContext(ctx, fields[0], append(fields[1:], contract.args...)...)
	cmd.Dir = r.Workspace
	cmd.Env = environment
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
	t := &turn{
		cmd: cmd, stdout: stdout, stderr: stderr, exited: make(chan struct{}), timeout: r.TurnTimeout,
		timer: timer, sink: events, contract: contract,
	}
	if endpoint != nil {
		t.endpointURL, t.endpointToken = endpoint.url, endpoint.token
	}
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

// kill terminates the turn. Killing the process group is not sufficient on its
// own: a descendant that leaves the group -- setsid, nohup, any double fork --
// keeps the inherited stdout write end open, so the reader would never see EOF
// and the turn would hang with no terminal event and no closed channel. Closing
// the parent's ends of the pipes is what actually ends the read.
func (t *turn) kill() {
	t.mu.Lock()
	t.killed = true
	t.mu.Unlock()
	t.killOnce.Do(func() { _ = procgroup.Kill(t.cmd) })
	t.closePipes()
}

func (t *turn) closePipes() {
	t.closeIO.Do(func() {
		_ = t.stdout.Close()
		_ = t.stderr.Close()
	})
}

// withoutEndpoint removes this turn's endpoint address and bearer token from
// child output before any of it becomes an event.
//
// observability.Text redacts credential-shaped text -- a Bearer header, a
// token= parameter -- and a loopback URL is neither, so a CLI that prints its
// MCP configuration or an MCP connect error to stderr would otherwise put the
// endpoint address in a diagnostic and a log. The address is not a credential
// on its own, since the token is what authorizes anything, but "the endpoint
// URL and token appear in no log line or event" is the property this endpoint
// was built to hold and it costs nothing to keep.
func (t *turn) withoutEndpoint(text string) string {
	if text == "" {
		return ""
	}
	for _, secret := range []string{t.endpointToken, t.endpointURL} {
		if secret != "" {
			text = strings.ReplaceAll(text, secret, "[redacted]")
		}
	}
	return text
}

func (t *turn) cancelled() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.killed
}
