// Package preflight validates a Symphony workflow without using live side effects.
package preflight

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/pmrrasmussen/symphony/internal/capability"
	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/coordinator"
	"github.com/pmrrasmussen/symphony/internal/domain"
	"github.com/pmrrasmussen/symphony/internal/linear"
)

type Status string

const (
	StatusPassed  Status = "passed"
	StatusWarning Status = "warning"
	StatusFailed  Status = "failed"
)

// Check is one side-effect boundary inspected by the preflight.
type Check struct {
	Name    string `json:"name"`
	Status  Status `json:"status"`
	Message string `json:"message"`
}

// Result is the machine-readable aggregate emitted by --dry-run.
type Result struct {
	Status Status  `json:"status"`
	Checks []Check `json:"checks"`
}

func (r Result) OK() bool { return r.Status != StatusFailed }

// Run validates static boundaries and then exercises the real coordinator with
// in-memory implementations. It never calls Linear, starts Codex, executes a
// hook, creates a log, or prepares a real workspace.
//
// statusFile is the operator's --status-file value, or empty if runtime status
// snapshots are disabled; either way it always produces a status_file check,
// so the check is never silently missing.
func Run(ctx context.Context, workflowPath, logRoot, statusFile string) Result {
	return run(ctx, workflowPath, logRoot, statusFile, nil)
}

// RunWithEnvironment is the read-only variant used to inspect a service whose
// credential references are supplied by its own host environment, rather than
// the invoking terminal. It has the same no-live-side-effect contract as Run.
func RunWithEnvironment(ctx context.Context, workflowPath, logRoot, statusFile string, environment map[string]string) Result {
	return run(ctx, workflowPath, logRoot, statusFile, environment)
}

// handoffToolName is how a worker on the selected backend actually names the
// scoped publish capability. It matters in a preflight message because the two
// transports do not name it the same way: a Codex worker calls a dynamic tool by
// its bare registry name, and a Claude worker calls it through the private
// loopback MCP endpoint, where the CLI prefixes every tool with its server name.
// An operator reading "publishing is enabled" and then grepping their logs for
// the wrong string concludes the handoff never happened.
//
// This reports what the transport names it, not whether this run advertises it.
// Availability is the bound issue's business -- github_land_pr also depends on
// its state -- and no side-effect-free check can know that.
func handoffToolName(settings config.Settings) string {
	if settings.AgentLaunch().Backend == config.ClaudeAgentBackend {
		return config.MCPToolPrefix + capability.NameGitHubPublishPR
	}
	return capability.NameGitHubPublishPR
}

func run(ctx context.Context, workflowPath, logRoot, statusFile string, environment map[string]string) Result {
	result := Result{Status: StatusPassed}
	workflow, err := config.LoadWithEnvironment(workflowPath, logRoot, environment)
	if err != nil {
		result.add("workflow", StatusFailed, err.Error())
		return result
	}
	settings := workflow.Config
	result.add("workflow", StatusPassed, "workflow parsed and normalized")
	for _, warning := range settings.Warnings {
		result.add("workflow_migration", StatusWarning, warning)
	}
	// The same predicate the rendered guidance branches on and the Claude backend
	// cross-checks its registry against, rather than a third copy of it: what
	// preflight reports available is then what a worker is told is available.
	if settings.HostSidePublishPromised() {
		result.add("github_handoff", StatusPassed, "host-side pull request publishing is enabled for the configured handoff state; a "+settings.AgentLaunch().Backend+" worker reaches it as "+handoffToolName(settings))
	} else if settings.GitHub.Enabled {
		result.add("github_handoff", StatusWarning, "host-side pull request publishing is unavailable: configure tracker.provider.handoff_state")
	} else if settings.Tracker.HandoffState != "" {
		result.add("github_handoff", StatusWarning, "host-side pull request publishing is unavailable: configure the fixed github integration")
	} else {
		result.add("github_handoff", StatusWarning, "host-side pull request publishing is unavailable; workers will use manual delivery mode")
	}

	tracker := linear.New(func() config.Settings { return settings })
	if err := tracker.Validate(); err != nil {
		result.add("tracker", StatusFailed, err.Error())
	} else {
		result.add("tracker", StatusPassed, "linear selection and provider configuration are valid; no request was sent")
	}

	result.addPath("workspace_root", settings.Workspace.Root)
	if settings.Workspace.SourceRoot == "" {
		result.add("workspace_source", StatusWarning, "workspace.source_root is not configured")
	} else {
		result.addPath("workspace_source", settings.Workspace.SourceRoot)
	}
	result.addPath("log_root", settings.LogRoot)
	result.addStatusFile(statusFile)

	// The check is named for the role, not the runtime, so selecting another
	// backend does not rename a machine-readable result. The message and every
	// failure name the backend's own command field, which is where an operator
	// has to go to fix it.
	launch := settings.AgentLaunch()
	if err := executable(launch.Command, launch.Backend+".command"); err != nil {
		result.add("agent_command", StatusFailed, err.Error())
	} else {
		result.add("agent_command", StatusPassed, fmt.Sprintf("command syntax and executable availability are valid; the %s agent was not started", launch.Backend))
	}

	// An unauthenticated agent CLI otherwise surfaces only at dispatch, where it
	// looks like a finished turn rather than a setup problem: the Claude CLI
	// reports an auth failure as a result with is_error set.
	if launch.Backend == config.ClaudeAgentBackend {
		switch status, err := authenticated(ctx, launch.Command, authenticationTimeout); {
		case err != nil:
			result.add("agent_authentication", StatusFailed, err.Error())
		case !status:
			// The command is a wrapper, so there was nothing to ask. Say that
			// rather than report an authenticated session that was never checked.
			result.add("agent_authentication", StatusWarning, "authentication was not verified: the configured command is not a bare program name")
		default:
			result.add("agent_authentication", StatusPassed, "the agent CLI reports an authenticated session")
		}
	}

	if err := hooks(settings.Hooks); err != nil {
		result.add("hooks", StatusFailed, err.Error())
	} else {
		result.add("hooks", StatusPassed, "configured hooks have valid shell syntax; hooks were not executed")
	}

	if err := lifecycle(ctx, settings); err != nil {
		result.add("scheduler_lifecycle", StatusFailed, err.Error())
	} else {
		result.add("scheduler_lifecycle", StatusPassed, "synthetic active issue exercised the tracker, workspace, and exhaustion-retry boundaries against fakes; no agent backend, router, capability registry, or capability transport was started")
	}
	return result
}

func (r *Result) add(name string, status Status, message string) {
	r.Checks = append(r.Checks, Check{Name: name, Status: status, Message: message})
	if status == StatusFailed {
		r.Status = StatusFailed
	} else if status == StatusWarning && r.Status == StatusPassed {
		r.Status = StatusWarning
	}
}

func (r *Result) addPath(name, path string) {
	status, message := inspectPath(path)
	r.add(name, status, message)
}

func (r *Result) addStatusFile(path string) {
	status, message := inspectStatusFile(path)
	r.add("status_file", status, message)
}

// inspectStatusFile validates the operator-supplied --status-file contract
// that status.Publisher enforces: the file's parent directory, if it already
// exists, must be owner-only. Unlike workspace_root and log_root, a missing
// parent is not a warning: status.Publisher creates it mode 0700 on first
// write (status.secureDirectory), so there is nothing for an operator to fix.
// A statusFile of "" reports a warning rather than being left out of the
// result, so an operator scanning checks always finds one for this flag.
func inspectStatusFile(statusFile string) (Status, string) {
	if statusFile == "" {
		return StatusWarning, "--status-file is not configured; runtime status snapshots are disabled"
	}
	path, err := filepath.Abs(statusFile)
	if err != nil {
		return StatusFailed, fmt.Sprintf("resolve --status-file %s: %v", statusFile, err)
	}
	dir := filepath.Dir(path)
	info, err := os.Stat(dir)
	if os.IsNotExist(err) {
		return StatusPassed, fmt.Sprintf("%s does not exist; Symphony creates it mode 0700 on first status write", dir)
	}
	if err != nil {
		return StatusFailed, fmt.Sprintf("inspect status directory %s: %v", dir, err)
	}
	if !info.IsDir() {
		return StatusFailed, fmt.Sprintf("status directory %s is not a directory", dir)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return StatusFailed, fmt.Sprintf("status directory %s must be owner-only (mode %#o); see docs/runtime-status.md", dir, info.Mode().Perm())
	}
	return StatusPassed, fmt.Sprintf("%s is an existing owner-only directory", dir)
}

func inspectPath(path string) (Status, string) {
	path = filepath.Clean(path)
	info, err := os.Stat(path)
	if err == nil {
		if !info.IsDir() {
			return StatusFailed, fmt.Sprintf("%s is not a directory", path)
		}
		return StatusPassed, fmt.Sprintf("%s is an existing directory", path)
	}
	if !os.IsNotExist(err) {
		return StatusFailed, fmt.Sprintf("inspect %s: %v", path, err)
	}
	ancestor := path
	for {
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return StatusFailed, fmt.Sprintf("no existing directory ancestor for %s", path)
		}
		ancestor = parent
		info, err = os.Stat(ancestor)
		if err == nil {
			if !info.IsDir() {
				return StatusFailed, fmt.Sprintf("ancestor %s is not a directory", ancestor)
			}
			return StatusWarning, fmt.Sprintf("%s does not exist; existing ancestor %s was validated without creating it", path, ancestor)
		}
		if !os.IsNotExist(err) {
			return StatusFailed, fmt.Sprintf("inspect ancestor %s: %v", ancestor, err)
		}
	}
}

func executable(command, field string) error {
	if _, err := exec.LookPath("sh"); err != nil {
		return errorsf("shell executable is unavailable: %v", err)
	}
	if err := exec.Command("sh", "-n", "-c", "exec "+command).Run(); err != nil {
		return errorsf("%s has invalid shell syntax: %v", field, err)
	}
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return errorsf("%s is empty", field)
	}
	program := strings.Trim(fields[0], "'\"")
	if program == "" || strings.ContainsAny(program, "$`\\|&;()<>") {
		return errorsf("%s executable must be a literal program name", field)
	}
	if _, err := exec.LookPath(program); err != nil {
		return errorsf("%s executable %q is unavailable", field, program)
	}
	return nil
}

// authenticationTimeout bounds the authentication probe. Reading a stored
// login is local and immediate, so five seconds is far more than the command
// needs; the budget exists because this is the only external call in a dry run
// that is not a parse-only check, and a CLI blocked on a keychain prompt or a
// hung token refresh would otherwise leave --dry-run waiting forever.
const authenticationTimeout = 5 * time.Second

// authenticated asks the agent CLI whether it holds a session. It is read-only,
// which is what makes it safe in a dry run.
//
// Only the boolean is read. The command also reports the operator's email,
// organization, and subscription, none of which may reach a check message.
// The boolean reports whether authentication was actually established. A
// wrapper command cannot be asked, and reporting that as success would claim a
// check that never ran.
//
// The budget is a parameter so the bound itself is exercised by a test without
// one waiting out the production value.
// probeWaitDelay bounds how long the probe waits for its output pipes after the
// command itself is gone.
const probeWaitDelay = 500 * time.Millisecond

func authenticated(ctx context.Context, command string, timeout time.Duration) (bool, error) {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return false, errorsf("claude.command is empty")
	}
	if len(fields) > 1 {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	probe := exec.CommandContext(ctx, fields[0], "auth", "status")
	// Killing the probe on the deadline is not enough to return on it. The
	// deadline kills the command itself, but Output waits for the output pipes
	// to close, and any grandchild the command left behind still holds them --
	// so without a WaitDelay the probe waits for that descendant instead of for
	// its own budget, which is the hang this timeout exists to prevent.
	probe.WaitDelay = probeWaitDelay
	out, err := probe.Output()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		// A probe that had to be killed is a failed check, not a pass: nothing
		// was learned about the session.
		return false, errorsf("claude.command did not report authentication status before its %s timeout expired", timeout)
	}
	if err != nil {
		return false, errorsf("claude.command could not report authentication status")
	}
	var status struct {
		LoggedIn bool `json:"loggedIn"`
	}
	if err := json.Unmarshal(out, &status); err != nil {
		return false, errorsf("claude.command returned an unreadable authentication status")
	}
	if !status.LoggedIn {
		return false, errorsf("claude.command reports no authenticated session; run its login command as the service user")
	}
	return true, nil
}

func hooks(h config.Hooks) error {
	for _, hook := range []struct{ name, script string }{
		{"after_create", h.AfterCreate},
		{"before_run", h.BeforeRun},
		{"after_run", h.AfterRun},
		{"before_remove", h.BeforeRemove},
	} {
		if hook.script == "" {
			continue
		}
		if err := exec.Command("sh", "-n", "-c", hook.script).Run(); err != nil {
			return errorsf("%s hook has invalid shell syntax: %v", hook.name, err)
		}
	}
	return nil
}

func errorsf(format string, args ...any) error { return fmt.Errorf(format, args...) }

func lifecycle(ctx context.Context, settings config.Settings) error {
	// One turn is sufficient to exercise every dry-run boundary without making
	// preflight wait through the configured continuation lifecycle.
	settings.Agent.MaxTurns = 1
	issue := domain.Issue{ID: "preflight", Identifier: "PREFLIGHT-1", Title: "Synthetic preflight", State: settings.Tracker.ActiveStates[0], Labels: append([]string(nil), settings.Tracker.RequiredLabels...), Dispatchable: true}
	if _, err := settings.Render(issue, 0); err != nil {
		return err
	}
	boundaries := &fakeBoundaries{issue: issue, afterRun: make(chan struct{})}
	c := coordinator.New(boundaries, boundaries, boundaries, func() config.Settings { return settings }, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.Tick(ctx)
	select {
	case <-boundaries.afterRun:
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(time.Second):
		return errorsf("synthetic scheduler lifecycle timed out")
	}
	if err := c.Shutdown(ctx); err != nil {
		return fmt.Errorf("stop synthetic scheduler lifecycle: %w", err)
	}
	if err := boundaries.verify(); err != nil {
		return err
	}
	return nil
}

type fakeBoundaries struct {
	mu          sync.Mutex
	issue       domain.Issue
	afterRun    chan struct{}
	lists       int
	gets        int
	prepares    int
	before      int
	starts      int
	after       int
	cancels     int
	transitions int
}

func (f *fakeBoundaries) ListCandidates(context.Context, []string) ([]domain.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lists++
	return []domain.Issue{f.issue}, nil
}
func (f *fakeBoundaries) GetIssues(context.Context, []string) ([]domain.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets++
	return []domain.Issue{f.issue}, nil
}
func (*fakeBoundaries) ListTerminal(context.Context, []string) ([]domain.Issue, error) {
	return nil, nil
}
func (f *fakeBoundaries) Transition(context.Context, domain.Issue, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.transitions++
	return nil
}
func (f *fakeBoundaries) Prepare(context.Context, domain.Issue) (domain.Workspace, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prepares++
	return domain.Workspace{Path: "preflight://workspace"}, nil
}
func (f *fakeBoundaries) BeforeRun(context.Context, domain.Workspace, domain.Issue) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.before++
	return nil
}
func (f *fakeBoundaries) AfterRun(context.Context, domain.Workspace, domain.Issue) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.after++
	if f.after == 1 {
		close(f.afterRun)
	}
}
func (*fakeBoundaries) Cleanup(context.Context, domain.Issue) (domain.CleanupOutcome, error) {
	return domain.CleanupClean, errorsf("unexpected cleanup")
}
func (*fakeBoundaries) Execute(context.Context, domain.Workspace, string, []string) ([]byte, error) {
	return nil, errorsf("unexpected command execution")
}
func (f *fakeBoundaries) Start(context.Context, domain.AgentRequest) (domain.AgentSession, <-chan domain.Event, error) {
	f.mu.Lock()
	f.starts++
	f.mu.Unlock()
	events := make(chan domain.Event, 1)
	events <- domain.Event{Kind: domain.EventCompleted}
	close(events)
	return domain.AgentSession{ID: "preflight", ThreadID: "preflight", TurnID: "preflight"}, events, nil
}
func (*fakeBoundaries) Continue(context.Context, domain.AgentSession, string) (<-chan domain.Event, error) {
	return nil, errorsf("unexpected continuation")
}
func (f *fakeBoundaries) Cancel(context.Context, domain.AgentSession) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancels++
	return nil
}
func (f *fakeBoundaries) verify() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lists != 1 || f.gets != 1 || f.prepares != 1 || f.before != 1 || f.starts != 1 || f.after != 1 || f.cancels != 1 {
		return errorsf("unexpected synthetic boundary counts: list=%d get=%d prepare=%d before=%d start=%d after=%d cancel=%d", f.lists, f.gets, f.prepares, f.before, f.starts, f.after, f.cancels)
	}
	return nil
}
