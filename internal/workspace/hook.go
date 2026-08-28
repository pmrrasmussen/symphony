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
// host credential reaches it (PMR-113). Trusting the script is not the same as
// trusting what it reaches: cmd.Dir is the agent's own worktree, so `make
// setup`, `./scripts/...`, or `npm run ...` runs code the agent wrote and
// committed, outside the agent sandbox. before_run and after_run bracket every
// turn on that worktree, which puts this on the ordinary lifecycle rather than
// behind operator error.
//
// A hook has no session, so it passes no capability.SecretMatcher and gets
// filters 1 through 3, which are derived from settings alone and cover every
// credential a loaded workflow resolves; filter 4 is what any caller outside a
// session forgoes. See config.ReservedSecretEnvNames for all four.
//
// The two SYMPHONY_ISSUE_* names are blocked on the way in and appended after
// filtering, so this function is their only source: Go's exec keeps the last
// value for a duplicated name, and an inherited variable cannot pre-empt them.
//
// `sh -c`, not `sh -lc`: a login shell sources the operator's profile, which is
// a second uncontrolled input to a process running in an agent-writable
// directory and could re-export a variable the filter just removed. It is also
// the form preflight validates the script with (`sh -n -c`), so what is checked
// is what runs. A hook resolves commands from the daemon's own PATH -- under a
// LaunchAgent, the one internal/service writes into the plist -- rather than
// from a profile.
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
