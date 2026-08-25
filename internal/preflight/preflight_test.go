package preflight

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// TestAgentCommandCheckNamesTheSelectedBackendsField keeps the operator-visible
// text pointing at the field an operator must actually edit. The check is named
// for the role so selecting another backend does not rename a machine-readable
// result, but every message names that backend's own command field.
func TestAgentCommandCheckNamesTheSelectedBackendsField(t *testing.T) {
	if err := executable("this-program-does-not-exist-symphony-test", "codex.command"); err == nil {
		t.Fatal("expected an unavailable executable to fail")
	} else if !strings.Contains(err.Error(), "codex.command executable") {
		t.Fatalf("failure does not name the backend's command field: %v", err)
	}
	if err := executable("", "codex.command"); err == nil || !strings.Contains(err.Error(), "codex.command is empty") {
		t.Fatalf("empty command failure=%v", err)
	}
	if err := executable("codex app-server '", "codex.command"); err == nil || !strings.Contains(err.Error(), "codex.command has invalid shell syntax") {
		t.Fatalf("invalid syntax failure=%v", err)
	}
	if err := executable("$(echo codex)", "codex.command"); err == nil || !strings.Contains(err.Error(), "codex.command executable must be a literal program name") {
		t.Fatalf("non-literal program failure=%v", err)
	}
}

// The account details the CLI reports alongside the boolean. They are declared
// once here so every authentication test can assert that none of them reaches
// an operator-visible string.
const (
	authEmail        = "operator@example.com"
	authOrganization = "example-org-9f3a"
	authSubscription = "plan-7c21"
)

func authStatusJSON(loggedIn string) string {
	return `{"loggedIn":` + loggedIn + `,"email":"` + authEmail + `","organization":"` + authOrganization + `","subscription":"` + authSubscription + `"}`
}

// writeAuthCommand writes a scripted stand-in for the agent CLI. A real
// `claude` is never invoked from a test: the probe is about how preflight reads
// a response, not about this machine's login.
func writeAuthCommand(t *testing.T, body string) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), "fake-agent-cli.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
	return script
}

// TestAuthenticationProbeReportsEachOutcomeWithoutClaimingAnUncheckedPass
// covers every branch of the probe. The pass is the dangerous one: a wrapper
// command, an unreadable answer, a non-zero exit, and a probe that had to be
// killed must all be distinguishable from a session that was actually verified.
func TestAuthenticationProbeReportsEachOutcomeWithoutClaimingAnUncheckedPass(t *testing.T) {
	t.Run("authenticated session", func(t *testing.T) {
		command := writeAuthCommand(t, `printf '%s' '`+authStatusJSON("true")+`'`)
		status, err := authenticated(context.Background(), command, authenticationTimeout)
		if err != nil || !status {
			t.Fatalf("status=%v err=%v", status, err)
		}
	})

	t.Run("no authenticated session", func(t *testing.T) {
		command := writeAuthCommand(t, `printf '%s' '`+authStatusJSON("false")+`'`)
		status, err := authenticated(context.Background(), command, authenticationTimeout)
		if status || err == nil || !strings.Contains(err.Error(), "no authenticated session") {
			t.Fatalf("status=%v err=%v", status, err)
		}
		assertNoAccountDetails(t, err.Error())
	})

	t.Run("unreadable answer", func(t *testing.T) {
		command := writeAuthCommand(t, `printf '%s' 'logged in as `+authEmail+`'`)
		status, err := authenticated(context.Background(), command, authenticationTimeout)
		if status || err == nil || !strings.Contains(err.Error(), "unreadable authentication status") {
			t.Fatalf("status=%v err=%v", status, err)
		}
		// The CLI's own output must not be quoted into the failure.
		assertNoAccountDetails(t, err.Error())
	})

	t.Run("non-zero exit", func(t *testing.T) {
		command := writeAuthCommand(t, "exit 3")
		status, err := authenticated(context.Background(), command, authenticationTimeout)
		if status || err == nil || !strings.Contains(err.Error(), "could not report authentication status") {
			t.Fatalf("status=%v err=%v", status, err)
		}
	})

	t.Run("wrapper command is not probed and is not a pass", func(t *testing.T) {
		status, err := authenticated(context.Background(), "sh -c exit", authenticationTimeout)
		if status || err != nil {
			t.Fatalf("status=%v err=%v", status, err)
		}
	})

	t.Run("a hanging CLI fails the check instead of hanging the dry run", func(t *testing.T) {
		command := writeAuthCommand(t, "sleep 60")
		start := time.Now()
		status, err := authenticated(context.Background(), command, 50*time.Millisecond)
		if status || err == nil || !strings.Contains(err.Error(), "did not report authentication status") {
			t.Fatalf("status=%v err=%v", status, err)
		}
		if elapsed := time.Since(start); elapsed > 30*time.Second {
			t.Fatalf("probe waited for the command rather than its budget: %v", elapsed)
		}
	})

	t.Run("empty command", func(t *testing.T) {
		if _, err := authenticated(context.Background(), "   ", authenticationTimeout); err == nil || !strings.Contains(err.Error(), "claude.command is empty") {
			t.Fatalf("err=%v", err)
		}
	})
}

// TestAuthenticationCheckKeepsAccountDetailsOutOfEveryResult is the whole-result
// version of the same promise: `claude auth status` reports the operator's
// email, organization, and subscription, and only the boolean may be read.
func TestAuthenticationCheckKeepsAccountDetailsOutOfEveryResult(t *testing.T) {
	command := writeAuthCommand(t, `printf '%s' '`+authStatusJSON("true")+`'`)
	dir := t.TempDir()
	workflow := filepath.Join(dir, "WORKFLOW.md")
	content := "---\n" +
		"tracker:\n  kind: linear\n  provider: {project_slug_id: preflight, api_key: not-a-live-key}\n  active_states: [Todo]\n  terminal_states: [Done]\n" +
		"workspace: {root: " + filepath.Join(dir, "workspaces") + ", source_root: " + dir + "}\n" +
		"agent: {backend: claude}\n" +
		"claude: {command: \"" + command + "\"}\n" +
		"---\nWork on {{.issue.identifier}}"
	if err := os.WriteFile(workflow, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	result := Run(context.Background(), workflow, filepath.Join(dir, "logs"))
	found := false
	for _, check := range result.Checks {
		assertNoAccountDetails(t, check.Message)
		if check.Name == "agent_authentication" {
			found = true
			if check.Status != StatusPassed {
				t.Fatalf("agent_authentication=%+v", check)
			}
		}
	}
	if !found {
		t.Fatalf("a claude workflow produced no agent_authentication check: %+v", result)
	}
}

func assertNoAccountDetails(t *testing.T, message string) {
	t.Helper()
	for _, detail := range []string{authEmail, authOrganization, authSubscription} {
		if strings.Contains(message, detail) {
			t.Fatalf("check text exposed an account detail %q: %s", detail, message)
		}
	}
}
