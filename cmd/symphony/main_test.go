package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/domain"
	githubhost "github.com/pmrrasmussen/symphony/internal/github"
	"github.com/pmrrasmussen/symphony/internal/observability"
	"github.com/pmrrasmussen/symphony/internal/operator"
	"github.com/pmrrasmussen/symphony/internal/preflight"
	"github.com/pmrrasmussen/symphony/internal/tui"
)

// fakeAuthenticatedAgentCommand stands in for the codex binary --dry-run's
// agent_authentication probe actually invokes: a real program on PATH, like
// the "go" placeholder these fixtures used before every backend got a probe,
// no longer reports a session and now fails the dry run.
func fakeAuthenticatedAgentCommand(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "fake-codex")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunAcceptsPositionalWorkflowForDryRun(t *testing.T) {
	dir := t.TempDir()
	workflow := filepath.Join(dir, "workflow.md")
	workspaceRoot := filepath.Join(dir, "workspaces")
	logRoot := filepath.Join(dir, "logs")
	content := "---\ntracker: {kind: linear, provider: {project_slug_id: preflight, api_key: dummy}, active_states: [Todo], terminal_states: [Done]}\nworkspace: {root: " + workspaceRoot + ", source_root: " + dir + "}\ncodex: {command: " + fakeAuthenticatedAgentCommand(t, dir) + "}\n---\n{{.issue.identifier}}"
	if err := os.WriteFile(workflow, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"--dry-run", "--logs-root", logRoot, workflow}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d stderr=%s stdout=%s", code, stderr.String(), stdout.String())
	}
	var result preflight.Result
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.OK() || len(result.Checks) == 0 {
		t.Fatalf("result=%+v", result)
	}
	for _, path := range []string{workspaceRoot, logRoot} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("dry run created %s: %v", path, err)
		}
	}
}

func TestRunAcceptsStatusFileFlagInDryRun(t *testing.T) {
	dir := t.TempDir()
	workflow := filepath.Join(dir, "workflow.md")
	statusFile := filepath.Join(dir, "runtime", "status.json")
	content := "---\ntracker: {kind: linear, provider: {project_slug_id: preflight, api_key: dummy}, active_states: [Todo], terminal_states: [Done]}\nworkspace: {root: " + filepath.Join(dir, "workspaces") + ", source_root: " + dir + "}\ncodex: {command: " + fakeAuthenticatedAgentCommand(t, dir) + "}\n---\n{{.issue.identifier}}"
	if err := os.WriteFile(workflow, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--dry-run", "--status-file", statusFile, workflow}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d stderr=%s stdout=%s", code, stderr.String(), stdout.String())
	}
	if _, err := os.Stat(statusFile); !os.IsNotExist(err) {
		t.Fatalf("dry run created status file: %v", err)
	}
}

func TestRunRetainsWorkflowFlagAndRejectsAmbiguousPaths(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--workflow", "one.md", "two.md"}, &stdout, &stderr); code != 2 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
}

func TestParseLogLevelAcceptsDocumentedValuesAndRejectsOthers(t *testing.T) {
	for _, test := range []struct {
		input string
		want  slog.Level
	}{
		{"", slog.LevelInfo},
		{"info", slog.LevelInfo},
		{"INFO", slog.LevelInfo},
		{"debug", slog.LevelDebug},
		{" Debug ", slog.LevelDebug},
	} {
		got, err := parseLogLevel(test.input)
		if err != nil || got != test.want {
			t.Fatalf("parseLogLevel(%q)=%v,%v want %v,nil", test.input, got, err, test.want)
		}
	}
	if _, err := parseLogLevel("trace"); err == nil {
		t.Fatal("parseLogLevel accepted an undocumented level")
	}
}

func TestRunRejectsUndocumentedLogLevel(t *testing.T) {
	dir := t.TempDir()
	workflow := filepath.Join(dir, "workflow.md")
	content := "---\ntracker: {kind: linear, provider: {project_slug_id: preflight, api_key: dummy}, active_states: [Todo], terminal_states: [Done]}\nworkspace: {root: " + filepath.Join(dir, "workspaces") + ", source_root: " + dir + "}\ncodex: {command: go}\n---\n{{.issue.identifier}}"
	if err := os.WriteFile(workflow, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--dry-run", "--log-level", "trace", workflow}, &stdout, &stderr); code != 2 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "--log-level") {
		t.Fatalf("stderr did not explain the rejected flag: %s", stderr.String())
	}
}

func TestRunTUIIsReadOnlyAndExitsCleanly(t *testing.T) {
	var stdout, stderr bytes.Buffer
	calls := 0
	discover := tui.Discover(func(_ context.Context, _ operator.Options) ([]operator.Instance, error) {
		calls++
		return []operator.Instance{{ID: "com.pmrrasmussen.symphony.test", Liveness: operator.LivenessStopped}}, nil
	})
	if code := runTUI(nil, strings.NewReader("q\n"), &stdout, &stderr, discover); code != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if calls != 1 || !strings.Contains(stdout.String(), "Symphony operator view") {
		t.Fatalf("calls=%d stdout=%s", calls, stdout.String())
	}
	if code := runTUI([]string{"--workflow", "x"}, strings.NewReader(""), &stdout, &stderr, discover); code != 2 {
		t.Fatalf("flagged tui exit=%d", code)
	}
}

func TestRunServiceUsageDocumentsMigrate(t *testing.T) {
	for _, args := range [][]string{nil, {"bogus"}} {
		var stdout, stderr bytes.Buffer
		if code := runService(args, &stdout, &stderr); code != 2 {
			t.Fatalf("args=%v exit=%d stderr=%s", args, code, stderr.String())
		}
		if !strings.Contains(stderr.String(), "install|migrate|status|restart|uninstall") {
			t.Fatalf("args=%v usage did not document migrate: %s", args, stderr.String())
		}
	}
}

func TestLogStartupCredentialStatusReportsConfigurationWithoutSecrets(t *testing.T) {
	var output bytes.Buffer
	log := observability.New(slog.NewJSONHandler(&output, nil), nil)
	logStartupCredentialStatus(log, config.Settings{
		GitHub: config.GitHub{Enabled: true, Token: "github-secret"},
	}, true)

	var record map[string]any
	if err := json.Unmarshal(output.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	if record["msg"] != "startup credential configuration" || record["linear_credentials_configured"] != true || record["github_credentials_configured"] != true {
		t.Fatalf("record=%v", record)
	}
	if strings.Contains(output.String(), "github-secret") {
		t.Fatalf("credential appeared in log: %s", output.String())
	}
}

// TestLogStartupCredentialStatusReflectsTrackerValidateOutcome pins PMR-119: the
// Linear field must be the caller's actual tracker.Validate() result, not a
// literal true that would keep asserting success if validation ever moved,
// became non-fatal, or was bypassed.
func TestLogStartupCredentialStatusReflectsTrackerValidateOutcome(t *testing.T) {
	var output bytes.Buffer
	log := observability.New(slog.NewJSONHandler(&output, nil), nil)
	logStartupCredentialStatus(log, config.Settings{}, false)

	var record map[string]any
	if err := json.Unmarshal(output.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	if record["linear_credentials_configured"] != false {
		t.Fatalf("record=%v, want linear_credentials_configured=false", record)
	}
}

type fakeTerminalTracker struct {
	states []string
	issues []domain.Issue
	err    error
}

func (f *fakeTerminalTracker) ListTerminal(_ context.Context, states []string) ([]domain.Issue, error) {
	f.states = states
	return f.issues, f.err
}

type fakeTerminalCleaner struct {
	cleaned  []string
	outcomes map[string]domain.CleanupOutcome
	errs     map[string]error
}

func (f *fakeTerminalCleaner) Cleanup(_ context.Context, issue domain.Issue) (domain.CleanupOutcome, error) {
	f.cleaned = append(f.cleaned, issue.Identifier)
	if err := f.errs[issue.Identifier]; err != nil {
		return "", err
	}
	return f.outcomes[issue.Identifier], nil
}

// Startup cleanup runs before the scheduler and against a tracker and a
// worktree root Symphony does not control, so its whole contract is that it
// stays out of the way: it asks for the configured terminal states, it reports
// a discarded landed worktree, and neither a failing issue nor a failing query
// stops it or startup.
func TestCleanupTerminalWorkspacesVisitsEveryTerminalIssueAndSurvivesFailures(t *testing.T) {
	var output bytes.Buffer
	log := observability.New(slog.NewJSONHandler(&output, nil), &output)
	tracker := &fakeTerminalTracker{issues: []domain.Issue{
		{ID: "one", Identifier: "PMR-1"}, {ID: "two", Identifier: "PMR-2"}, {ID: "three", Identifier: "PMR-3"},
	}}
	ws := &fakeTerminalCleaner{
		outcomes: map[string]domain.CleanupOutcome{"PMR-1": domain.CleanupLanded, "PMR-3": domain.CleanupClean},
		errs:     map[string]error{"PMR-2": errors.New("worktree busy")},
	}

	cleanupTerminalWorkspaces(context.Background(), log, tracker, ws, []string{"Done", "Canceled"})

	if !reflect.DeepEqual(tracker.states, []string{"Done", "Canceled"}) {
		t.Fatalf("cleanup queried states=%v", tracker.states)
	}
	if !reflect.DeepEqual(ws.cleaned, []string{"PMR-1", "PMR-2", "PMR-3"}) {
		t.Fatalf("a failed cleanup stopped the sweep: cleaned=%v", ws.cleaned)
	}
	if !strings.Contains(output.String(), "terminal workspace cleanup removed verified landed work") ||
		!strings.Contains(output.String(), "terminal workspace cleanup failed") {
		t.Fatalf("logs did not report the landed removal and the failure: %s", output.String())
	}
	if strings.Count(output.String(), "removed verified landed work") != 1 {
		t.Fatalf("only the landed workspace may be reported as removed: %s", output.String())
	}
}

func TestCleanupTerminalWorkspacesSkipsCleanupWhenTheQueryFails(t *testing.T) {
	var output bytes.Buffer
	log := observability.New(slog.NewJSONHandler(&output, nil), &output)
	ws := &fakeTerminalCleaner{}

	cleanupTerminalWorkspaces(context.Background(), log, &fakeTerminalTracker{err: errors.New("linear unavailable")}, ws, []string{"Done"})

	if len(ws.cleaned) != 0 {
		t.Fatalf("an unreadable tracker still drove cleanup: cleaned=%v", ws.cleaned)
	}
	if !strings.Contains(output.String(), "startup terminal cleanup query failed") {
		t.Fatalf("failed query was not reported: %s", output.String())
	}
}

// TestWireGivesTheHostTheGitHubManagerItsBackendsUse asserts the process-level
// invariant the whole provider-ownership seam exists for: the manager this
// process polls and verifies landings on is the very instance bound to the
// backend that runs sessions. Two managers would each own a linked-pull-request
// table and an exactly-once completion guard, so the poll loop would walk a
// table no session writes into and a merged pull request would leave its Linear
// issue unreconciled. Every wired backend that reports a manager must report
// that same one, so a second capability-bearing backend given a manager of its
// own fails here rather than in production -- provided it exposes the manager
// it holds, which is the reason codex.Backend does. A backend reporting none is
// fine, since that is an unwired integration, but no backend reporting one at
// all is not: the invariant would then be silently unasserted.
func TestWireGivesTheHostTheGitHubManagerItsBackendsUse(t *testing.T) {
	settings := func() config.Settings { return config.Settings{} }
	backends, polled, endpoint, err := wire(settings, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = endpoint.Close(context.Background()) })
	if polled == nil {
		t.Fatal("wire returned no GitHub manager, so the host has nothing to poll or verify landings with")
	}
	if endpoint == nil {
		t.Fatal("wire returned no capability endpoint, so a capability-bearing backend would have no transport to serve one over")
	}
	// The same one manager is what run() hands the scheduler as its
	// IssueForgetter, which is the only thing that ever ends polling for a pull
	// request that is still open on an issue a human finished (PMR-112). A
	// manager that stopped satisfying that contract would leave the wiring
	// silently absent, so assert the contract here rather than at the call.
	if _, ok := any(polled).(domain.IssueForgetter); !ok {
		t.Fatal("the polled GitHub manager cannot be wired as the scheduler's issue forgetter, so terminal issues would be polled for the life of the process")
	}
	bound := 0
	for name, backend := range backends {
		holder, ok := backend.(interface {
			GitHubManager() *githubhost.Manager
		})
		if !ok {
			continue
		}
		if holder.GitHubManager() != polled {
			t.Fatalf("backend %q runs sessions against a GitHub manager the host neither polls nor verifies landings on", name)
		}
		bound++
	}
	if bound == 0 {
		t.Fatal("no wired backend reported a bound GitHub manager, so the one-manager-per-process invariant is unasserted")
	}
}
