// Package preflight validates a Symphony workflow without using live side effects.
package preflight

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

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
func Run(ctx context.Context, workflowPath, logRoot string) Result {
	result := Result{Status: StatusPassed}
	store, err := config.NewStore(workflowPath, logRoot)
	if err != nil {
		result.add("workflow", StatusFailed, err.Error())
		return result
	}
	settings := store.Current().Config
	result.add("workflow", StatusPassed, "workflow parsed and normalized")
	for _, warning := range settings.Warnings {
		result.add("workflow_migration", StatusWarning, warning)
	}
	if settings.GitHub.Enabled && settings.Tracker.HandoffState != "" {
		result.add("github_handoff", StatusPassed, "host-side pull request publishing is enabled for the configured handoff state")
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

	if err := executable(settings.Codex.Command); err != nil {
		result.add("codex_command", StatusFailed, err.Error())
	} else {
		result.add("codex_command", StatusPassed, "command syntax and executable availability are valid; Codex was not started")
	}

	if err := hooks(settings.Hooks); err != nil {
		result.add("hooks", StatusFailed, err.Error())
	} else {
		result.add("hooks", StatusPassed, "configured hooks have valid shell syntax; hooks were not executed")
	}

	if err := lifecycle(ctx, settings); err != nil {
		result.add("scheduler_lifecycle", StatusFailed, err.Error())
	} else {
		result.add("scheduler_lifecycle", StatusPassed, "synthetic active issue exercised tracker, workspace, agent, and exhaustion-retry boundaries")
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

func executable(command string) error {
	if _, err := exec.LookPath("sh"); err != nil {
		return errorsf("shell executable is unavailable: %v", err)
	}
	if err := exec.Command("sh", "-n", "-c", "exec "+command).Run(); err != nil {
		return errorsf("codex.command has invalid shell syntax: %v", err)
	}
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return errorsf("codex.command is empty")
	}
	program := strings.Trim(fields[0], "'\"")
	if program == "" || strings.ContainsAny(program, "$`\\|&;()<>") {
		return errorsf("codex.command executable must be a literal program name")
	}
	if _, err := exec.LookPath(program); err != nil {
		return errorsf("codex.command executable %q is unavailable", program)
	}
	return nil
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
func (*fakeBoundaries) Cleanup(context.Context, domain.Issue) error {
	return errorsf("unexpected cleanup")
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
