package workspace

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/domain"
	"github.com/pmrrasmussen/symphony/internal/observability"
)

func TestAfterCreateDiagnosticsAreBounded(t *testing.T) {
	root := filepath.Join(t.TempDir(), "workspaces")
	script := `(yes stdout | head -c 40000); (yes stderr | head -c 40000 >&2); exit 1`
	s := config.Settings{Workspace: config.Workspace{Root: root}, Hooks: config.Hooks{AfterCreate: script}}
	l := New(func() config.Settings { return s })
	issue := domain.Issue{ID: "issue-17", Identifier: "PMR-17"}
	_, err := l.Prepare(context.Background(), issue)
	if err == nil {
		t.Fatal("Prepare unexpectedly succeeded")
	}
	if len(err.Error()) > 2*maxHookOutput+1024 {
		t.Fatalf("hook error is not bounded: %d bytes", len(err.Error()))
	}
	if !strings.Contains(err.Error(), "[truncated]") {
		t.Fatalf("hook error lacks truncation marker: %v", err)
	}
}

// TestNoHostCredentialReachesAHook is the hook's share of the guarantee
// config.ReservedSecretEnvNames documents, and the counterpart of
// TestNoHostCredentialReachesTheChildEnvironment in internal/codex and
// TestHostSecretsNeverReachTheChild in internal/claude. A hook is the third
// child Symphony spawns, and until PMR-113 it was the one that ran with the
// daemon's environment whole -- in the agent's own worktree, where it can invoke
// anything the agent committed.
//
// It runs a real after_create hook through Prepare rather than calling hook
// directly, because what is proven here is that the launcher reaches the filter
// at all; what the filter then removes is proven once, over hostenv.Filter.
//
// The reserved names are written out rather than read from
// config.ReservedSecretEnvNames, deliberately: a test that iterates the list
// asserts nothing about its contents, and dropping an entry would leave it
// green.
func TestNoHostCredentialReachesAHook(t *testing.T) {
	// Filter 1: the reserved names, whatever the workflow configures. Each value
	// is unique and is matched by no other filter, so only the name can remove
	// it.
	reserved := map[string]string{
		"LINEAR_API_KEY":               "reserved-linear-key-value",
		"SYMPHONY_LINEAR_API_KEY_FILE": "/private/reserved-linear-key-path",
		"GITHUB_TOKEN":                 "reserved-forge-token-value",
		"SYMPHONY_GITHUB_TOKEN":        "reserved-symphony-forge-token-value",
		"SYMPHONY_GITHUB_TOKEN_FILE":   "/private/reserved-forge-token-path",
	}
	for name, value := range reserved {
		t.Setenv(name, value)
	}
	// Filter 2: configured names, including the credential *file path* form no
	// value filter can see. Filter 3: a configured credential inside the value
	// of a variable no list mentions. Filter 4 has no case here: a hook has no
	// session to build a matcher from, which is the documented shape of this
	// caller rather than a gap in the test.
	t.Setenv("PMR113_CONFIGURED_NAME", "configured-name-value")
	t.Setenv("PMR113_PADDED_NAME", "padded-name-value")
	t.Setenv("PMR113_CONFIGURED_FILE", "/private/configured-key-path")
	t.Setenv("PMR113_INHERITED_CONFIGURED", "Bearer configured-secret-value")
	t.Setenv("PMR113_KEPT", "ordinary-value")
	// Inherited variables under the two names Symphony sets itself: the hook
	// must be told about this run's issue, never about whatever the host
	// exported under the same name.
	t.Setenv(issueIDEnvName, "inherited-issue-id")
	t.Setenv(issueIdentifierEnvName, "inherited-identifier")

	root := filepath.Join(t.TempDir(), "workspaces")
	environment := filepath.Join(t.TempDir(), "environment")
	s := config.Settings{
		Workspace: config.Workspace{Root: root},
		Hooks:     config.Hooks{AfterCreate: "env > " + environment},
		// The padded and blank names are hostenv.Filter's, and are here because
		// this test runs the real hook: a Settings that never went through
		// config.Load can carry either, and the launcher must reach the filter
		// that handles them.
		HostSecretEnvNames: []string{"PMR113_CONFIGURED_NAME", "  PMR113_PADDED_NAME  ", "   ", "PMR113_CONFIGURED_FILE"},
		HostSecretValues:   []string{"configured-secret-value"},
	}
	l := New(func() config.Settings { return s })
	issue := domain.Issue{ID: "issue-113", Identifier: "PMR-113"}
	if _, err := l.Prepare(context.Background(), issue); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(environment)
	if err != nil {
		t.Fatal(err)
	}
	child := string(data)
	names := hookEnvironmentNames(child)
	for name, value := range reserved {
		if slices.Contains(names, name) {
			t.Fatalf("hook environment retained reserved variable %s", name)
		}
		if strings.Contains(child, value) {
			t.Fatalf("hook environment retained the value of reserved variable %s", name)
		}
	}
	for _, leaked := range []string{"configured-name-value", "padded-name-value",
		"/private/configured-key-path", "configured-secret-value",
		"inherited-issue-id", "inherited-identifier"} {
		if strings.Contains(child, leaked) {
			t.Fatalf("hook environment retained %q", leaked)
		}
	}
	if !strings.Contains(child, "ordinary-value") {
		t.Fatal("the host credential filter removed unrelated variables")
	}
	// The hook still gets what it is for. Without this the filter could pass by
	// handing every hook an empty environment.
	for _, want := range []string{issueIDEnvName + "=" + issue.ID, issueIdentifierEnvName + "=" + issue.Identifier} {
		if !slices.Contains(strings.Split(child, "\n"), want) {
			t.Fatalf("hook environment lacks %q", want)
		}
	}
}

// hookEnvironmentNames reads the variable names out of `env` output, so an
// absence assertion on one name cannot be satisfied by another name that merely
// ends with it -- GITHUB_TOKEN inside SYMPHONY_GITHUB_TOKEN, for example.
//
// Only a line whose prefix is shaped like a variable name counts, because a
// developer or CI machine can hold a multi-line value whose continuation lines
// would otherwise read as names and produce a confusing failure.
func hookEnvironmentNames(child string) []string {
	var names []string
	for _, line := range strings.Split(child, "\n") {
		if name, _, found := strings.Cut(line, "="); found && environmentName(name) {
			names = append(names, name)
		}
	}
	return names
}

func environmentName(candidate string) bool {
	if candidate == "" {
		return false
	}
	for i, r := range candidate {
		digit := r >= '0' && r <= '9'
		if !(r == '_' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || digit && i > 0) {
			return false
		}
	}
	return true
}

// TestHookDiagnosticsAreBoundedAndMaskedOnBothPaths proves observability.Text
// is applied once, at the source in hook, so a token-shaped credential in hook
// output is masked and the diagnostics are bounded to
// observability.MaxDiagnosticBytes on both the returned-error path
// (BeforeRun, which the coordinator logs) and the logged path (AfterRun,
// which this package logs directly).
func TestHookDiagnosticsAreBoundedAndMaskedOnBothPaths(t *testing.T) {
	root := filepath.Join(t.TempDir(), "workspaces")
	const secret = "token=super-secret-hook-value"
	script := fmt.Sprintf(`echo %s; (yes filler | head -c 4000); exit 1`, secret)
	s := config.Settings{Workspace: config.Workspace{Root: root}, Hooks: config.Hooks{BeforeRun: script, AfterRun: script}}
	l := New(func() config.Settings { return s })
	var logs bytes.Buffer
	l.SetLogger(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	issue := domain.Issue{ID: "issue-114", Identifier: "PMR-114"}
	ws, err := l.Prepare(context.Background(), issue)
	if err != nil {
		t.Fatal(err)
	}

	// The returned-error path: what the coordinator's BeforeRun failure carries.
	beforeRunErr := l.BeforeRun(context.Background(), ws, issue)
	if beforeRunErr == nil {
		t.Fatal("BeforeRun unexpectedly succeeded")
	}
	if strings.Contains(beforeRunErr.Error(), secret) {
		t.Fatalf("hook secret leaked through the returned error: %v", beforeRunErr)
	}
	if !strings.Contains(beforeRunErr.Error(), "[REDACTED]") {
		t.Fatalf("returned hook error was not masked: %v", beforeRunErr)
	}
	if got := len(beforeRunErr.Error()); got > observability.MaxDiagnosticBytes+256 {
		t.Fatalf("returned hook error is not bounded: %d bytes: %v", got, beforeRunErr)
	}

	// The logged path: AfterRun's hook failure never returns to a caller, it is
	// logged directly.
	l.AfterRun(context.Background(), ws, issue)
	if strings.Contains(logs.String(), secret) {
		t.Fatalf("hook secret leaked into the log: %s", logs.String())
	}
	if !strings.Contains(logs.String(), "[REDACTED]") {
		t.Fatalf("logged hook diagnostics were not masked: %s", logs.String())
	}
}
