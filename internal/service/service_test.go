package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pmrrasmussen/symphony/internal/operator"
)

type fakeRunner struct {
	root  string
	calls []string
	fail  string
}

func (r *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, name+" "+strings.Join(args, " "))
	if name == "git" && strings.Join(args, " ") == "-C "+r.root+" rev-parse --show-toplevel" {
		return []byte(r.root + "\n"), nil
	}
	if name == "git" && strings.Contains(strings.Join(args, " "), "remote get-url origin") {
		return []byte("git@github.com:owner/repository.git\n"), nil
	}
	if name == "plutil" || name == "launchctl" {
		if r.fail != "" && strings.Contains(name+" "+strings.Join(args, " "), r.fail) {
			return []byte("forced failure"), errors.New("forced failure")
		}
		return nil, nil
	}
	return nil, errors.New("unexpected command")
}

func TestInstallIsIdempotentAndUsesRepositoryScopedPaths(t *testing.T) {
	dir, options, runner := serviceFixture(t)
	instance, changed, err := Install(context.Background(), options)
	if err != nil || !changed {
		t.Fatalf("first install = %#v, %v, %v", instance, changed, err)
	}
	data, err := os.ReadFile(instance.PlistPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{
		"<key>SymphonyManaged</key><true/>",
		filepath.Join(dir, ".symphony", "logs"),
		filepath.Join(dir, ".symphony", "service", "status.json"),
		filepath.Join(dir, ".symphony", "service", "stdout.log"),
		"SYMPHONY_LINEAR_API_KEY_FILE",
		basePath,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("plist missing %q: %s", want, text)
		}
	}
	if strings.Contains(text, "linear-secret") {
		t.Fatalf("plist contains credential value: %s", text)
	}
	instances, discoverErr := operator.Discover(context.Background(), operator.Options{LaunchAgentsDir: options.LaunchAgentsDir})
	if discoverErr != nil || len(instances) != 1 || !instances[0].Managed {
		t.Fatalf("discovery before repeat: %#v, %v", instances, discoverErr)
	}
	_, changed, err = Install(context.Background(), options)
	if err != nil || changed {
		t.Fatalf("second install changed=%v err=%v", changed, err)
	}
	bootstrap := 0
	for _, call := range runner.calls {
		if strings.Contains(call, "launchctl bootstrap") {
			bootstrap++
		}
	}
	if bootstrap != 1 {
		t.Fatalf("bootstrap calls=%d, calls=%v", bootstrap, runner.calls)
	}
}

func TestInstallRejectsDuplicateWorkflowBeforeReplacingExistingService(t *testing.T) {
	dir, options, _ := serviceFixture(t)
	other := filepath.Join(options.LaunchAgentsDir, "com.pmrrasmussen.symphony.other.plist")
	content := "<?xml version=\"1.0\"?><plist version=\"1.0\"><dict><key>Label</key><string>com.pmrrasmussen.symphony.other</string><key>SymphonyManaged</key><true/><key>ProgramArguments</key><array><string>/bin/sh</string><string>--workflow</string><string>" + filepath.Join(dir, "WORKFLOW.md") + "</string></array><key>WorkingDirectory</key><string>" + dir + "</string></dict></plist>"
	if err := os.WriteFile(other, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := Install(context.Background(), options)
	if err == nil || !strings.Contains(err.Error(), "shared workflow") {
		t.Fatalf("err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(options.LaunchAgentsDir, "com.pmrrasmussen.symphony.owner-repository.plist")); !os.IsNotExist(err) {
		t.Fatalf("target plist was written despite conflict: %v", err)
	}
}

func TestInstallRejectsUnmanagedTargetAndDoesNotExposeSecret(t *testing.T) {
	_, options, _ := serviceFixture(t)
	target := filepath.Join(options.LaunchAgentsDir, "com.pmrrasmussen.symphony.owner-repository.plist")
	if err := os.WriteFile(target, []byte("<?xml version=\"1.0\"?><plist version=\"1.0\"><dict><key>Label</key><string>com.pmrrasmussen.symphony.owner-repository</string><key>Program</key><string>/bin/sh</string></dict></plist>"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := Install(context.Background(), options)
	if err == nil || !strings.Contains(err.Error(), "unmanaged") {
		t.Fatalf("err=%v", err)
	}
	if strings.Contains(fmtError(err), "linear-secret") {
		t.Fatalf("error leaked secret: %v", err)
	}
}

func TestInstallAllowsIndependentRepositoriesWithOneSharedBinary(t *testing.T) {
	first, options, runner := serviceFixture(t)
	if _, _, err := Install(context.Background(), options); err != nil {
		t.Fatal(err)
	}
	second := t.TempDir()
	if err := os.WriteFile(filepath.Join(second, "WORKFLOW.md"), []byte("---\ntracker: {kind: linear, provider: {project_slug_id: second, api_key_file: $SYMPHONY_LINEAR_API_KEY_FILE}, active_states: [Todo], terminal_states: [Done]}\nworkspace: {root: .symphony/workspaces, source_root: .}\ncodex: {command: go}\n---\nprompt"), 0o600); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(second, "linear-key")
	if err := os.WriteFile(key, []byte("another-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner.root = second
	options.Repository, options.Name, options.LinearKeyFile = second, "second", key
	secondInstance, _, err := Install(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if first == second || secondInstance.PlistPath == "" {
		t.Fatal("fixture did not create distinct repositories")
	}
	entries, err := os.ReadDir(options.LaunchAgentsDir)
	if err != nil || len(entries) != 2 {
		t.Fatalf("LaunchAgent files=%v err=%v", entries, err)
	}
	data, err := os.ReadFile(secondInstance.PlistPath)
	if err != nil || !strings.Contains(string(data), filepath.Join(second, ".symphony", "service", "status.json")) {
		t.Fatalf("second plist=%s err=%v", data, err)
	}
}

func TestInstallValidationFailureLeavesExistingPlistUntouched(t *testing.T) {
	_, options, runner := serviceFixture(t)
	target := filepath.Join(options.LaunchAgentsDir, "com.pmrrasmussen.symphony.owner-repository.plist")
	if err := os.WriteFile(target, []byte("previous valid service"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner.fail = "plutil"
	_, _, err := Install(context.Background(), options)
	if err == nil || !strings.Contains(err.Error(), "generated LaunchAgent plist is invalid") {
		t.Fatalf("err=%v", err)
	}
	data, readErr := os.ReadFile(target)
	if readErr != nil || string(data) != "previous valid service" {
		t.Fatalf("target changed=%q err=%v", data, readErr)
	}
}

func TestInstallStartFailureRestoresExistingService(t *testing.T) {
	_, options, runner := serviceFixture(t)
	instance, _, err := Install(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(instance.PlistPath)
	if err != nil {
		t.Fatal(err)
	}
	runner.fail = "launchctl kickstart"
	options.LogLevel = "debug"
	_, _, err = Install(context.Background(), options)
	if err == nil || !strings.Contains(err.Error(), "start "+instance.Label) {
		t.Fatalf("err=%v", err)
	}
	after, err := os.ReadFile(instance.PlistPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("replacement plist was not restored\nbefore=%s\nafter=%s", before, after)
	}
}

func TestInstallDoesNotChangeExistingLaunchAgentsPermissions(t *testing.T) {
	_, options, _ := serviceFixture(t)
	if err := os.Chmod(options.LaunchAgentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Install(context.Background(), options); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(options.LaunchAgentsDir)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o755 {
		t.Fatalf("LaunchAgents permissions = %#o, want 0755", got)
	}
}

func TestRenderPlistEscapesSpecialPathCharacters(t *testing.T) {
	d := desired{Instance: Instance{Label: "com.pmrrasmussen.symphony.example", Workflow: "/tmp/a & b/WORKFLOW.md"}, Repository: "/tmp/a & b", LogsRoot: "/tmp/a & b/.symphony/logs", StatusFile: "/tmp/a & b/.symphony/service/status.json", Stdout: "/tmp/a & b/.symphony/service/stdout.log", Stderr: "/tmp/a & b/.symphony/service/stderr.log"}
	content := string(renderPlist(d, "/tmp/a & b/symphony", "info", map[string]string{"PATH": "/bin"}))
	if strings.Contains(content, "/tmp/a & b") || !strings.Contains(content, "/tmp/a &amp; b") {
		t.Fatalf("plist did not escape paths: %s", content)
	}
}

func serviceFixture(t *testing.T) (string, Options, *fakeRunner) {
	t.Helper()
	dir := t.TempDir()
	launch := filepath.Join(dir, "LaunchAgents")
	if err := os.MkdirAll(filepath.Join(dir, "source"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(launch, 0o700); err != nil {
		t.Fatal(err)
	}
	workflow := "---\ntracker: {kind: linear, provider: {project_slug_id: service, api_key_file: $SYMPHONY_LINEAR_API_KEY_FILE}, active_states: [Todo], terminal_states: [Done]}\nworkspace: {root: .symphony/workspaces, source_root: .}\ncodex: {command: go}\n---\nprompt"
	if err := os.WriteFile(filepath.Join(dir, "WORKFLOW.md"), []byte(workflow), 0o600); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(dir, "linear-key")
	if err := os.WriteFile(key, []byte("linear-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, "symphony")
	if err := os.WriteFile(binary, []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{root: dir}
	return dir, Options{Repository: dir, Binary: binary, LaunchAgentsDir: launch, LinearKeyFile: key, Runner: runner}, runner
}

func fmtError(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
