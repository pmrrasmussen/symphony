package preflight

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/pmrrasmussen/symphony/internal/config"
)

func TestRunExercisesLifecycleWithoutCreatingConfiguredState(t *testing.T) {
	dir := t.TempDir()
	workspaceRoot := filepath.Join(dir, "state", "workspaces")
	logRoot := filepath.Join(dir, "state", "logs")
	workflow := filepath.Join(dir, "WORKFLOW.md")
	writeWorkflow(t, workflow, workspaceRoot, `go version`, "")

	result := Run(context.Background(), workflow, logRoot, "")
	if !result.OK() {
		t.Fatalf("result=%+v", result)
	}
	for _, check := range []string{"workflow", "github_handoff", "tracker", "workspace_root", "workspace_source", "log_root", "status_file", "agent_command", "agent_authentication", "hooks", "scheduler_lifecycle"} {
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

	result := Run(context.Background(), workflow, filepath.Join(dir, "logs"), "")
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

// TestStatusFileCheckReportsAWarningWhenNotConfigured pins the never-silently-
// missing rule PMR-122 argues for on agent_authentication: an operator who
// never set --status-file still finds a status_file check, rather than
// having to notice its total absence, and it never fails the overall result.
func TestStatusFileCheckReportsAWarningWhenNotConfigured(t *testing.T) {
	status, message := inspectStatusFile("")
	if status != StatusWarning || !strings.Contains(message, "not configured") {
		t.Fatalf("status=%v message=%q", status, message)
	}
}

// TestStatusFileCheckAcceptsAMissingParentDirectory pins that a status
// directory Symphony has not created yet is not a failure: status.Publisher
// creates it mode 0700 on first write, so there is nothing to flag.
func TestStatusFileCheckAcceptsAMissingParentDirectory(t *testing.T) {
	dir := t.TempDir()
	status, message := inspectStatusFile(filepath.Join(dir, "runtime", "status.json"))
	if status != StatusPassed || !strings.Contains(message, "0700") {
		t.Fatalf("status=%v message=%q", status, message)
	}
}

// TestStatusFileCheckCatchesTheNonOwnerOnlyDirectoryPMR125Observed
// reproduces the PMR-125 finding directly: --dry-run must fail, not pass all
// green, when the status directory's mode would make every runtime status
// write fail identically for the rest of the process's life.
func TestStatusFileCheckCatchesTheNonOwnerOnlyDirectoryPMR125Observed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not expose Unix permission semantics")
	}
	dir := t.TempDir()
	statusDir := filepath.Join(dir, ".symphony")
	if err := os.Mkdir(statusDir, 0o755); err != nil {
		t.Fatal(err)
	}

	status, message := inspectStatusFile(filepath.Join(statusDir, "status.json"))
	if status != StatusFailed || !strings.Contains(message, "owner-only") {
		t.Fatalf("status=%v message=%q", status, message)
	}

	workflow := filepath.Join(dir, "WORKFLOW.md")
	writeWorkflow(t, workflow, filepath.Join(dir, "workspaces"), `go version`, "")
	result := Run(context.Background(), workflow, filepath.Join(dir, "logs"), filepath.Join(statusDir, "status.json"))
	if result.OK() {
		t.Fatalf("result reported ok with a non-owner-only status directory: %+v", result)
	}
}

// TestStatusFileCheckAcceptsAnExistingOwnerOnlyDirectory is the accepting
// counterpart: a correctly configured status directory must not turn the
// overall preflight result into a failure or warning.
func TestStatusFileCheckAcceptsAnExistingOwnerOnlyDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not expose Unix permission semantics")
	}
	dir := t.TempDir()
	statusDir := filepath.Join(dir, ".symphony")
	if err := os.Mkdir(statusDir, 0o700); err != nil {
		t.Fatal(err)
	}
	status, message := inspectStatusFile(filepath.Join(statusDir, "status.json"))
	if status != StatusPassed {
		t.Fatalf("status=%v message=%q", status, message)
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

	result := Run(context.Background(), workflow, filepath.Join(dir, "logs"), "")
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

// TestTheHandoffCheckReportsExactlyWhatAWorkerWillBeTold pins the github_handoff
// result to the rendered delivery guidance, by comparing the two rather than by
// restating the condition a third time. An enabled GitHub integration is not on
// its own a publishing capability -- the scoped session is prepared only on top
// of a review handoff -- so reporting one as available would have preflight
// promise what the worker is then told is unavailable.
//
// Each row states its inputs independently of its expectation, and the
// expectation is read out of config.DeliveryInstructions rather than written
// down, so a row cannot agree with itself.
func TestTheHandoffCheckReportsExactlyWhatAWorkerWillBeTold(t *testing.T) {
	t.Setenv("PMR52_PREFLIGHT_TOKEN", "github-secret")
	const githubBlock = "github: {owner: o, repository: r, token: $PMR52_PREFLIGHT_TOKEN}\n"
	for name, tc := range map[string]struct{ handoff, github string }{
		"github enabled with no handoff state": {github: githubBlock},
		"github enabled with a handoff state":  {handoff: ", handoff_state: In Review", github: githubBlock},
		"a handoff state with no github":       {handoff: ", handoff_state: In Review"},
		"neither":                              {},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			workflow := filepath.Join(dir, "WORKFLOW.md")
			content := "---\n" +
				"tracker:\n  kind: linear\n  provider: {project_slug_id: preflight, api_key: not-a-live-key" + tc.handoff + "}\n" +
				"  active_states: [Todo]\n  terminal_states: [Done]\n" +
				tc.github +
				"workspace: {root: " + filepath.Join(dir, "workspaces") + ", source_root: " + dir + "}\n" +
				"codex: {command: go}\n" +
				"---\nWork on {{.issue.identifier}}"
			if err := os.WriteFile(workflow, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			w, err := config.Load(workflow, filepath.Join(dir, "logs"))
			if err != nil {
				t.Fatal(err)
			}
			// The expectation: what a worker on this workflow is actually told.
			promised := strings.Contains(w.Config.DeliveryInstructions(w.Config.Agent.Backend), config.HostSidePublishPromiseMarker)
			var got Status
			for _, check := range result(t, workflow, dir).Checks {
				if check.Name == "github_handoff" {
					got = check.Status
				}
			}
			if passed := got == StatusPassed; passed != promised {
				t.Fatalf("preflight reports publish available=%v (%q) but the rendered guidance promises it=%v", passed, got, promised)
			}
		})
	}
}

func result(t *testing.T, workflow, dir string) Result {
	t.Helper()
	return Run(context.Background(), workflow, filepath.Join(dir, "logs"), "")
}

// TestRunWithEnvironmentResolvesTokensFromItsOwnEnvironmentOverlay pins the
// contract that makes RunWithEnvironment its own code path rather than a thin
// alias for Run: a LaunchAgent's credential references live in its own host
// environment, not the invoking terminal's, so the same workflow reads as
// unavailable without the overlay and available with it.
func TestRunWithEnvironmentResolvesTokensFromItsOwnEnvironmentOverlay(t *testing.T) {
	const varName = "PMR122_PREFLIGHT_RUNWITHENV_TOKEN"
	if _, ok := os.LookupEnv(varName); ok {
		t.Fatalf("%s must not already be set in the test process environment", varName)
	}
	dir := t.TempDir()
	workflow := filepath.Join(dir, "WORKFLOW.md")
	content := "---\n" +
		"tracker:\n  kind: linear\n  provider: {project_slug_id: preflight, api_key: not-a-live-key, handoff_state: In Review}\n" +
		"  active_states: [Todo]\n  terminal_states: [Done]\n" +
		"github: {owner: o, repository: r, token: $" + varName + "}\n" +
		"workspace: {root: " + filepath.Join(dir, "workspaces") + ", source_root: " + dir + "}\n" +
		"codex: {command: go}\n" +
		"---\nWork on {{.issue.identifier}}"
	if err := os.WriteFile(workflow, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	without := RunWithEnvironment(context.Background(), workflow, filepath.Join(dir, "logs"), "", nil)
	if got := handoffStatus(without); got != StatusWarning {
		t.Fatalf("github_handoff=%v without the token in the environment overlay, want warning", got)
	}

	withOverlay := RunWithEnvironment(context.Background(), workflow, filepath.Join(dir, "logs"), "", map[string]string{varName: "github-secret"})
	if got := handoffStatus(withOverlay); got != StatusPassed {
		t.Fatalf("github_handoff=%v with the token in the environment overlay, want passed", got)
	}
}

func handoffStatus(result Result) Status {
	for _, check := range result.Checks {
		if check.Name == "github_handoff" {
			return check.Status
		}
	}
	return ""
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
	argv, ok := authenticationArgv(config.ClaudeAgentBackend)
	if !ok {
		t.Fatal("claude backend has no authentication argv")
	}

	t.Run("authenticated session", func(t *testing.T) {
		command := writeAuthCommand(t, `printf '%s' '`+authStatusJSON("true")+`'`)
		status, err := authenticated(context.Background(), command, argv, authenticationTimeout)
		if err != nil || !status {
			t.Fatalf("status=%v err=%v", status, err)
		}
	})

	t.Run("no authenticated session", func(t *testing.T) {
		command := writeAuthCommand(t, `printf '%s' '`+authStatusJSON("false")+`'`)
		status, err := authenticated(context.Background(), command, argv, authenticationTimeout)
		if status || err == nil || !strings.Contains(err.Error(), "no authenticated session") {
			t.Fatalf("status=%v err=%v", status, err)
		}
		assertNoAccountDetails(t, err.Error())
	})

	t.Run("unreadable answer", func(t *testing.T) {
		command := writeAuthCommand(t, `printf '%s' 'logged in as `+authEmail+`'`)
		status, err := authenticated(context.Background(), command, argv, authenticationTimeout)
		if status || err == nil || !strings.Contains(err.Error(), "unreadable authentication status") {
			t.Fatalf("status=%v err=%v", status, err)
		}
		// The CLI's own output must not be quoted into the failure.
		assertNoAccountDetails(t, err.Error())
	})

	t.Run("non-zero exit", func(t *testing.T) {
		command := writeAuthCommand(t, "exit 3")
		status, err := authenticated(context.Background(), command, argv, authenticationTimeout)
		if status || err == nil || !strings.Contains(err.Error(), "could not report authentication status") {
			t.Fatalf("status=%v err=%v", status, err)
		}
	})

	t.Run("wrapper command is not probed and is not a pass", func(t *testing.T) {
		status, err := authenticated(context.Background(), "sh -c exit", argv, authenticationTimeout)
		if status || err != nil {
			t.Fatalf("status=%v err=%v", status, err)
		}
	})

	t.Run("a hanging CLI fails the check instead of hanging the dry run", func(t *testing.T) {
		// The background child is what makes this test meaningful everywhere.
		// Killing the probe on its deadline does not close the output pipes a
		// descendant still holds, so without a bounded wait the probe waits for
		// that descendant instead of for its budget. A plain "sleep 60" only
		// reproduces it where the shell forks rather than execs, which is why
		// this first failed in CI and not locally.
		command := writeAuthCommand(t, "sleep 60 &\nwait")
		start := time.Now()
		status, err := authenticated(context.Background(), command, argv, 50*time.Millisecond)
		if status || err == nil || !strings.Contains(err.Error(), "did not report authentication status") {
			t.Fatalf("status=%v err=%v", status, err)
		}
		// The budget is 50ms and the pipe wait is bounded in the same order, so
		// anything approaching a second means the bound is not being applied.
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Fatalf("probe waited for the command rather than its budget: %v", elapsed)
		}
	})

	t.Run("empty command", func(t *testing.T) {
		if _, err := authenticated(context.Background(), "   ", argv, authenticationTimeout); err == nil || !strings.Contains(err.Error(), "claude.command is empty") {
			t.Fatalf("err=%v", err)
		}
	})
}

// TestCodexHasNoAuthenticationArgvAndTheCheckSaysSo pins the Codex half of the
// PMR-122 gap directly: the backend defines no probe, so run must still emit
// an agent_authentication check -- warning, not simply absent -- naming the
// backend and saying why there is nothing to ask.
func TestCodexHasNoAuthenticationArgvAndTheCheckSaysSo(t *testing.T) {
	if _, ok := authenticationArgv(config.DefaultAgentBackend); ok {
		t.Fatal("codex unexpectedly has an authentication argv")
	}

	message := checkMessage(t, "codex", "agent_authentication")
	if !strings.Contains(message, "codex") || !strings.Contains(message, "no side-effect-free authentication probe") {
		t.Fatalf("agent_authentication reported %q for codex", message)
	}

	dir := t.TempDir()
	workflow := filepath.Join(dir, "WORKFLOW.md")
	writeWorkflow(t, workflow, filepath.Join(dir, "workspaces"), "go", "")
	found := false
	for _, check := range result(t, workflow, dir).Checks {
		if check.Name == "agent_authentication" {
			found = true
			if check.Status != StatusWarning {
				t.Fatalf("agent_authentication=%+v", check)
			}
		}
	}
	if !found {
		t.Fatalf("a codex workflow produced no agent_authentication check")
	}
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

	result := Run(context.Background(), workflow, filepath.Join(dir, "logs"), "")
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

// The handoff check names the tool the selected backend actually serves. An
// operator told "publishing is enabled" who then greps for the wrong string
// concludes the handoff never happened, so the name is part of the report.

// checkMessage runs a preflight over a minimal workflow on the given backend and
// returns one check's message. GitHub and a handoff state are both configured so
// the handoff check takes its available branch, which is the one that has to name
// the tool.
func checkMessage(t *testing.T, backend, check string) string {
	t.Helper()
	t.Setenv("PMR53_PREFLIGHT_TOKEN", "github-secret")
	dir := t.TempDir()
	workflow := filepath.Join(dir, "WORKFLOW.md")
	content := "---\n" +
		"tracker:\n  kind: linear\n  provider: {project_slug_id: preflight, api_key: not-a-live-key, handoff_state: In Review}\n" +
		"  active_states: [Todo]\n  terminal_states: [Done]\n" +
		"github: {owner: o, repository: r, token: $PMR53_PREFLIGHT_TOKEN}\n" +
		"workspace: {root: " + filepath.Join(dir, "workspaces") + ", source_root: " + dir + "}\n" +
		"agent: {backend: " + backend + "}\n" +
		backend + ": {command: go}\n" +
		"---\nWork on {{.issue.identifier}}"
	if err := os.WriteFile(workflow, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range result(t, workflow, dir).Checks {
		if c.Name == check {
			return c.Message
		}
	}
	t.Fatalf("no %s check in the result", check)
	return ""
}

func TestTheHandoffCheckNamesTheToolTheSelectedBackendServes(t *testing.T) {
	for _, tc := range []struct {
		backend, want, absent string
	}{
		{backend: "codex", want: "github_publish_pr", absent: "mcp__symphony__"},
		{backend: "claude", want: "mcp__symphony__github_publish_pr"},
	} {
		t.Run(tc.backend, func(t *testing.T) {
			message := checkMessage(t, tc.backend, "github_handoff")
			if !strings.Contains(message, tc.want) {
				t.Fatalf("github_handoff reported %q, want it to name %q", message, tc.want)
			}
			if tc.absent != "" && strings.Contains(message, tc.absent) {
				t.Fatalf("github_handoff reported %q, which names %q on a backend that does not use it", message, tc.absent)
			}
			if !strings.Contains(message, tc.backend) {
				t.Fatalf("github_handoff reported %q without naming the %s backend", message, tc.backend)
			}
		})
	}
}

// The lifecycle check runs entirely against fakes, so it must not imply it
// exercised an agent backend or the capability transport. Claiming coverage it
// does not have is worse than reporting none: it is what an operator relies on
// before a live run.
func TestTheLifecycleCheckDoesNotClaimAgentCoverage(t *testing.T) {
	message := checkMessage(t, "claude", "scheduler_lifecycle")
	if !strings.Contains(message, "fakes") {
		t.Fatalf("scheduler_lifecycle reported %q without saying it ran against fakes", message)
	}
	for _, unexercised := range []string{"agent backend", "router", "capability registry", "capability transport"} {
		if !strings.Contains(message, unexercised) {
			t.Fatalf("scheduler_lifecycle reported %q without disclaiming the %s", message, unexercised)
		}
	}
}
