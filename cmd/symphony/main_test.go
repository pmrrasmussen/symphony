package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pmrrasmussen/symphony/internal/config"
	githubhost "github.com/pmrrasmussen/symphony/internal/github"
	"github.com/pmrrasmussen/symphony/internal/observability"
	"github.com/pmrrasmussen/symphony/internal/operator"
	"github.com/pmrrasmussen/symphony/internal/preflight"
	"github.com/pmrrasmussen/symphony/internal/tui"
)

func TestRunAcceptsPositionalWorkflowForDryRun(t *testing.T) {
	dir := t.TempDir()
	workflow := filepath.Join(dir, "workflow.md")
	workspaceRoot := filepath.Join(dir, "workspaces")
	logRoot := filepath.Join(dir, "logs")
	content := "---\ntracker: {kind: linear, provider: {project_slug_id: preflight, api_key: dummy}, active_states: [Todo], terminal_states: [Done]}\nworkspace: {root: " + workspaceRoot + ", source_root: " + dir + "}\ncodex: {command: go}\n---\n{{.issue.identifier}}"
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
	content := "---\ntracker: {kind: linear, provider: {project_slug_id: preflight, api_key: dummy}, active_states: [Todo], terminal_states: [Done]}\nworkspace: {root: " + filepath.Join(dir, "workspaces") + ", source_root: " + dir + "}\ncodex: {command: go}\n---\n{{.issue.identifier}}"
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
	})

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
