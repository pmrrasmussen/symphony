package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/pmrrasmussen/symphony/internal/preflight"
)

func TestRunAcceptsPositionalWorkflowForDryRun(t *testing.T) {
	dir := t.TempDir()
	workflow := filepath.Join(dir, "workflow.md")
	workspaceRoot := filepath.Join(dir, "workspaces")
	logRoot := filepath.Join(dir, "logs")
	content := "---\ntracker: {kind: linear, provider: {project_slug: preflight, api_key: dummy}, active_states: [Todo], terminal_states: [Done]}\nworkspace: {root: " + workspaceRoot + ", source_root: " + dir + "}\ncodex: {command: go}\n---\n{{.issue.identifier}}"
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

func TestRunRetainsWorkflowFlagAndRejectsAmbiguousPaths(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--workflow", "one.md", "two.md"}, &stdout, &stderr); code != 2 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
}
