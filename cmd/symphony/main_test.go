package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pmrrasmussen/symphony/internal/preflight"
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
