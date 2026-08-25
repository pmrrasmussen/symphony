package preflight

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunExercisesLifecycleWithoutCreatingConfiguredState(t *testing.T) {
	dir := t.TempDir()
	workspaceRoot := filepath.Join(dir, "state", "workspaces")
	logRoot := filepath.Join(dir, "state", "logs")
	workflow := filepath.Join(dir, "WORKFLOW.md")
	writeWorkflow(t, workflow, workspaceRoot, `go version`, "")

	result := Run(context.Background(), workflow, logRoot)
	if !result.OK() {
		t.Fatalf("result=%+v", result)
	}
	for _, check := range []string{"workflow", "github_handoff", "tracker", "workspace_root", "workspace_source", "log_root", "agent_command", "hooks", "scheduler_lifecycle"} {
		if !hasCheck(result, check) {
			t.Fatalf("missing %s check: %+v", check, result)
		}
	}
	for _, path := range []string{workspaceRoot, logRoot} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("preflight created %s: %v", path, err)
		}
	}
}

func TestRunReportsIndependentBoundaryFailures(t *testing.T) {
	dir := t.TempDir()
	workflow := filepath.Join(dir, "WORKFLOW.md")
	writeWorkflow(t, workflow, filepath.Join(dir, "workspaces"), `symphony-command-that-does-not-exist`, "if then")

	result := Run(context.Background(), workflow, filepath.Join(dir, "logs"))
	if result.OK() || result.Status != StatusFailed {
		t.Fatalf("result=%+v", result)
	}
	for _, check := range []string{"agent_command", "hooks"} {
		found := false
		for _, item := range result.Checks {
			if item.Name == check && item.Status == StatusFailed {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing failed %s check: %+v", check, result)
		}
	}
}

func TestRunReportsSafeLegacyProjectMigrationWarning(t *testing.T) {
	dir := t.TempDir()
	workflow := filepath.Join(dir, "WORKFLOW.md")
	content := "---\n" +
		"tracker:\n  kind: linear\n  provider: {project_slug: private-project, api_key: not-a-live-key}\n  active_states: [Todo]\n  terminal_states: [Done]\n" +
		"workspace: {root: " + filepath.Join(dir, "workspaces") + ", source_root: " + dir + "}\n" +
		"codex: {command: go}\n" +
		"---\nWork on {{.issue.identifier}}"
	if err := os.WriteFile(workflow, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	result := Run(context.Background(), workflow, filepath.Join(dir, "logs"))
	found := false
	for _, check := range result.Checks {
		if check.Name == "workflow_migration" && check.Status == StatusWarning {
			found = true
			if strings.Contains(check.Message, "private-project") {
				t.Fatalf("migration warning exposed project value: %+v", check)
			}
		}
	}
	if !found {
		t.Fatalf("missing migration warning: %+v", result)
	}
}

func writeWorkflow(t *testing.T, path, workspaceRoot, command, beforeRun string) {
	t.Helper()
	content := "---\n" +
		"tracker:\n  kind: linear\n  provider: {project_slug_id: preflight, api_key: not-a-live-key}\n  active_states: [Todo]\n  terminal_states: [Done]\n" +
		"workspace: {root: " + workspaceRoot + ", source_root: " + filepath.Dir(path) + "}\n" +
		"hooks:\n  before_run: \"" + beforeRun + "\"\n" +
		"codex:\n  command: \"" + command + "\"\n" +
		"---\nWork on {{.issue.identifier}}"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func hasCheck(result Result, name string) bool {
	for _, check := range result.Checks {
		if check.Name == name && check.Status != StatusFailed && strings.TrimSpace(check.Message) != "" {
			return true
		}
	}
	return false
}
