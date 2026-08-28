package workspace

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/pmrrasmussen/symphony/internal/domain"
	"github.com/pmrrasmussen/symphony/internal/hostenv"
	"github.com/pmrrasmussen/symphony/internal/observability"
)

const maxHookOutput = 16 << 10

// The only two variables a hook is given. They are named here because hook
// both blocks them and sets them, and the two must not drift apart.
const (
	issueIDEnvName         = "SYMPHONY_ISSUE_ID"
	issueIdentifierEnvName = "SYMPHONY_ISSUE_IDENTIFIER"
)

// hook runs one WORKFLOW.md hook script. The script is repository-owned,
// versioned policy and is trusted as such, but a hook is a child Symphony
// spawns like any other, so its environment goes through hostenv.Filter and no
// host credential reaches it (PMR-113). A hook has no session, so it passes no
// capability.SecretMatcher and gets filters 1 through 3.
//
// The two SYMPHONY_ISSUE_* names are blocked on the way in and appended after
// filtering, so this function is their only source: Go's exec keeps the last
// value for a duplicated name, and an inherited variable cannot pre-empt them.
//
// `sh -c`, not `sh -lc`: a login shell sources the operator's profile, which is
// a second uncontrolled input to a process running in an agent-writable
// directory, and it is not the form preflight validates the script with
// (`sh -n -c`).
//
// docs/architecture.md's "The host credential filter" section states why
// trusting the script is not the same as trusting what it reaches, and what
// filter 4 costs a caller outside a session.
func (l *Local) hook(ctx context.Context, ws domain.Workspace, issue domain.Issue, name, script string) error {
	if script == "" {
		return nil
	}
	path, err := l.managedWorkspacePath(ws.Path)
	if err != nil {
		return err
	}
	settings := l.settings()
	timeout := settings.Hooks.Timeout
	if timeout <= 0 {
		timeout = time.Minute
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "sh", "-c", script)
	cmd.Dir = path
	cmd.Env = append(hostenv.Filter(os.Environ(), []string{issueIDEnvName, issueIdentifierEnvName}, settings, nil),
		issueIDEnvName+"="+issue.ID, issueIdentifierEnvName+"="+issue.Identifier)
	var stdout, stderr boundedBuffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	if err != nil {
		// Bounded and masked here, at the source, so every consumer of this error
		// -- the coordinator's BeforeRun failure path and the after_run/
		// before_remove records this package logs directly -- sees the same
		// redacted diagnostics rather than depending on the caller to apply
		// observability.Text itself.
		diagnostics := observability.Text(strings.TrimSpace(strings.Join([]string{stdout.String(), stderr.String()}, "\n")))
		return fmt.Errorf("%s hook failed: %w: %s", name, err, diagnostics)
	}
	return nil
}

type boundedBuffer struct {
	b []byte
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	remaining := maxHookOutput - len(b.b)
	if remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		b.b = append(b.b, p...)
	}
	return n, nil
}

func (b *boundedBuffer) String() string {
	value := strings.TrimSpace(string(b.b))
	if len(b.b) == maxHookOutput {
		value += "...[truncated]"
	}
	return value
}
