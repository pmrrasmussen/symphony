package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/operator"
)

// fakeRunner models launchd the way the service package must cope with it.
// Load state is a set of labels that exists independently of any plist on
// disk, so a job whose plist was renamed or deleted is representable; print
// can fail without reporting absence; and an unload can be reported as still
// in progress.
type fakeRunner struct {
	root  string
	calls []string
	fail  string
	// loaded are the labels launchd currently has registered.
	loaded map[string]bool
	// printFail are labels whose print fails for a reason that is not absence,
	// so the caller learns nothing about whether they are loaded.
	printFail map[string]bool
	// unloadLag is how many further prints still report a label as loaded
	// after a successful bootout, modelling an asynchronous unload.
	unloadLag map[string]int
}

func (r *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	joined := strings.Join(args, " ")
	r.calls = append(r.calls, name+" "+joined)
	if name == "git" && joined == "-C "+r.root+" rev-parse --show-toplevel" {
		return []byte(r.root + "\n"), nil
	}
	if name == "git" && strings.Contains(joined, "remote get-url origin") {
		return []byte("git@github.com:owner/repository.git\n"), nil
	}
	if r.fail != "" && (name == "plutil" || name == "launchctl") && strings.Contains(name+" "+joined, r.fail) {
		return []byte("forced failure"), errors.New("forced failure")
	}
	if name == "plutil" {
		return nil, nil
	}
	if name == "launchctl" {
		return r.launchctl(args)
	}
	return nil, errors.New("unexpected command")
}

// launchctl reproduces the observable contract the service package relies on,
// including the messages launchd uses to report a genuinely absent service.
func (r *fakeRunner) launchctl(args []string) ([]byte, error) {
	if r.loaded == nil {
		r.loaded = map[string]bool{}
	}
	switch args[0] {
	case "print":
		label := serviceLabel(args[1])
		if r.printFail[label] {
			return []byte("Could not print domain: 5: Input/output error"), errors.New("exit status 5")
		}
		if r.loaded[label] {
			return []byte("state = running"), nil
		}
		if r.unloadLag[label] > 0 {
			r.unloadLag[label]--
			return []byte("state = exited"), nil
		}
		return []byte("Could not find service \"" + label + "\" in domain for user gui: 501"), errors.New("exit status 113")
	case "bootout":
		label := serviceLabel(args[1])
		if !r.loaded[label] {
			return []byte("Boot-out failed: 3: No such process"), errors.New("exit status 3")
		}
		delete(r.loaded, label)
		return nil, nil
	case "list":
		lines := []string{"PID\tStatus\tLabel"}
		labels := make([]string, 0, len(r.loaded))
		for label := range r.loaded {
			labels = append(labels, label)
		}
		sort.Strings(labels)
		for _, label := range labels {
			lines = append(lines, "1234\t0\t"+label)
		}
		return []byte(strings.Join(lines, "\n") + "\n"), nil
	case "bootstrap":
		r.loaded[strings.TrimSuffix(filepath.Base(args[len(args)-1]), ".plist")] = true
		return nil, nil
	}
	return nil, nil
}

// serviceLabel extracts a label from a gui/<uid>[/<label>] service target.
func serviceLabel(target string) string {
	_, label, _ := strings.Cut(strings.TrimPrefix(target, "gui/"), "/")
	return label
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

func TestInstallStartFailureRemovesNewPlist(t *testing.T) {
	_, options, runner := serviceFixture(t)
	runner.fail = "launchctl kickstart"
	_, changed, err := Install(context.Background(), options)
	if err == nil || changed || !strings.Contains(err.Error(), "start com.pmrrasmussen.symphony.owner-repository") {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	target := filepath.Join(options.LaunchAgentsDir, "com.pmrrasmussen.symphony.owner-repository.plist")
	if _, statErr := os.Stat(target); !os.IsNotExist(statErr) {
		t.Fatalf("new plist remained after failed start: %v", statErr)
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

// TestReservedNamesCoverTheServiceCredentialVariables holds two lists together
// that live in different packages for different reasons: the plist variables
// this installer hands to the daemon, and config.ReservedSecretEnvNames, the
// names no agent child may inherit. The direction is opposite but the names must
// agree -- a credential variable this installer exports into the daemon that the
// reserved list does not carry would be inherited by every agent child, whatever
// the workflow configured. Neither list can import the other's role, so this is
// what keeps "one definition" honest across the split.
func TestReservedNamesCoverTheServiceCredentialVariables(t *testing.T) {
	reserved := config.ReservedSecretEnvNames()
	for _, name := range []string{linearKeyEnvironment, githubKeyEnvironment} {
		if !slices.Contains(reserved, name) {
			t.Fatalf("the LaunchAgent sets %s but config.ReservedSecretEnvNames does not block it, so every agent child would inherit it: %v", name, reserved)
		}
	}
}
