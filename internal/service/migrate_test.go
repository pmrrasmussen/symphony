package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMigrateAdoptsExactLegacyAgentAndThenManagesTheRepository(t *testing.T) {
	dir, options, runner := serviceFixture(t)
	// This mirrors the real pre-installer agent: the bare legacy label, a
	// repository-local executable, and no --status-file argument.
	legacy, content := writeLegacyAgent(t, options.LaunchAgentsDir, legacyAgent{
		label:      "com.pmrrasmussen.symphony",
		repository: dir,
		executable: repositoryExecutable(t, dir),
		workflow:   filepath.Join(dir, "WORKFLOW.md"),
		logsRoot:   filepath.Join(dir, ".symphony", "logs"),
		keyFile:    options.LinearKeyFile,
	})
	runner.loaded = map[string]bool{"com.pmrrasmussen.symphony": true}
	migration, err := Migrate(context.Background(), options)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if !migration.Changed || migration.Legacy != "com.pmrrasmussen.symphony" || migration.Label != "com.pmrrasmussen.symphony.owner-repository" {
		t.Fatalf("migration = %#v", migration)
	}
	if runner.loaded["com.pmrrasmussen.symphony"] || !runner.loaded[migration.Label] {
		t.Fatalf("launchd state after migration = %v", runner.loaded)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("legacy plist survived migration: %v", err)
	}
	backup, err := os.ReadFile(migration.Backup)
	if err != nil || string(backup) != content {
		t.Fatalf("backup = %q err=%v", backup, err)
	}
	installed, err := os.ReadFile(migration.PlistPath)
	if err != nil || !strings.Contains(string(installed), "<key>SymphonyManaged</key><true/>") ||
		!strings.Contains(string(installed), filepath.Join(dir, ".symphony", "service", "status.json")) {
		t.Fatalf("installed plist = %s err=%v", installed, err)
	}
	entries, err := os.ReadDir(options.LaunchAgentsDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("LaunchAgents entries = %v err=%v", entries, err)
	}
	if boot, load := callIndex(runner, "launchctl bootout gui/"), callIndex(runner, "launchctl bootstrap gui/"); boot < 0 || load < 0 || boot > load {
		t.Fatalf("legacy agent was not booted out before the managed one loaded: %v", runner.calls)
	}
	// A rerun is a successful no-op, and the normal commands now select the
	// managed instance for this repository.
	repeat, err := Migrate(context.Background(), options)
	if err != nil || repeat.Changed || repeat.Label != migration.Label {
		t.Fatalf("repeat migration = %#v err=%v", repeat, err)
	}
	status, err := Status(context.Background(), options)
	if err != nil || !status.Managed || status.ID != migration.Label {
		t.Fatalf("status = %#v err=%v", status, err)
	}
	if _, err := Restart(context.Background(), options); err != nil {
		t.Fatalf("restart: %v", err)
	}
	if _, _, err := Install(context.Background(), options); err != nil {
		t.Fatalf("install after migration: %v", err)
	}
	if _, err := Uninstall(context.Background(), options); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if _, err := os.Stat(migration.PlistPath); !os.IsNotExist(err) {
		t.Fatalf("uninstall left the managed plist: %v", err)
	}
}

func TestMigrateRefusesRelatedButIncompatibleAgent(t *testing.T) {
	dir, options, _ := serviceFixture(t)
	legacy, content := writeLegacyAgent(t, options.LaunchAgentsDir, legacyAgent{
		label:      "com.pmrrasmussen.symphony.legacy",
		repository: dir,
		executable: "/bin/sh",
		workflow:   filepath.Join(dir, "elsewhere", "WORKFLOW.md"),
		logsRoot:   filepath.Join(dir, "elsewhere", "logs"),
		keyFile:    options.LinearKeyFile,
	})
	_, err := Migrate(context.Background(), options)
	if err == nil || !strings.Contains(err.Error(), "refusing to migrate LaunchAgent "+legacy) {
		t.Fatalf("err=%v", err)
	}
	for _, want := range []string{"is not a Symphony executable", "workflow", "log root"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("diagnostic %q missing from %v", want, err)
		}
	}
	if data, readErr := os.ReadFile(legacy); readErr != nil || string(data) != content {
		t.Fatalf("legacy plist changed = %q err=%v", data, readErr)
	}
	assertNoManagedPlist(t, options.LaunchAgentsDir)
}

func TestMigrateRefusesAmbiguousLegacyAgents(t *testing.T) {
	dir, options, _ := serviceFixture(t)
	executable := repositoryExecutable(t, dir)
	first, firstContent := writeLegacyAgent(t, options.LaunchAgentsDir, legacyAgent{
		label: "com.pmrrasmussen.symphony", repository: dir, executable: executable,
		workflow: filepath.Join(dir, "WORKFLOW.md"), logsRoot: filepath.Join(dir, ".symphony", "logs"), keyFile: options.LinearKeyFile,
	})
	second, secondContent := writeLegacyAgent(t, options.LaunchAgentsDir, legacyAgent{
		label: "com.pmrrasmussen.symphony.legacy", repository: dir, executable: executable,
		workflow: filepath.Join(dir, "WORKFLOW.md"), logsRoot: filepath.Join(dir, ".symphony", "logs"), keyFile: options.LinearKeyFile,
	})
	_, err := Migrate(context.Background(), options)
	if err == nil || !strings.Contains(err.Error(), "ambiguous hand-authored Symphony LaunchAgents") {
		t.Fatalf("err=%v", err)
	}
	for path, want := range map[string]string{first: firstContent, second: secondContent} {
		if data, readErr := os.ReadFile(path); readErr != nil || string(data) != want {
			t.Fatalf("%s changed = %q err=%v", path, data, readErr)
		}
	}
	assertNoManagedPlist(t, options.LaunchAgentsDir)
}

func TestMigrateLeavesUnrelatedLaunchAgentUntouched(t *testing.T) {
	_, options, _ := serviceFixture(t)
	other := t.TempDir()
	unrelated, content := writeLegacyAgent(t, options.LaunchAgentsDir, legacyAgent{
		label: "com.pmrrasmussen.symphony.other", repository: other, executable: repositoryExecutable(t, other),
		workflow: filepath.Join(other, "WORKFLOW.md"), logsRoot: filepath.Join(other, ".symphony", "logs"), keyFile: options.LinearKeyFile,
	})
	_, err := Migrate(context.Background(), options)
	if err == nil || !strings.Contains(err.Error(), "no hand-authored Symphony LaunchAgent to migrate") {
		t.Fatalf("err=%v", err)
	}
	if data, readErr := os.ReadFile(unrelated); readErr != nil || string(data) != content {
		t.Fatalf("unrelated plist changed = %q err=%v", data, readErr)
	}
	assertNoManagedPlist(t, options.LaunchAgentsDir)
}

func TestMigrateRestoresAndReloadsLegacyAgentWhenTheManagedServiceFailsToStart(t *testing.T) {
	dir, options, runner := serviceFixture(t)
	legacy, content := writeLegacyAgent(t, options.LaunchAgentsDir, legacyAgent{
		label:      "com.pmrrasmussen.symphony.legacy",
		repository: dir,
		executable: repositoryExecutable(t, dir),
		workflow:   filepath.Join(dir, "WORKFLOW.md"),
		logsRoot:   filepath.Join(dir, ".symphony", "logs"),
		keyFile:    options.LinearKeyFile,
	})
	runner.loaded = map[string]bool{"com.pmrrasmussen.symphony.legacy": true}
	runner.fail = "launchctl kickstart"
	migration, err := Migrate(context.Background(), options)
	if err == nil || migration.Changed || !strings.Contains(err.Error(), "restored the prior LaunchAgent "+legacy) {
		t.Fatalf("migration=%#v err=%v", migration, err)
	}
	if data, readErr := os.ReadFile(legacy); readErr != nil || string(data) != content {
		t.Fatalf("legacy plist not restored = %q err=%v", data, readErr)
	}
	if !runner.loaded["com.pmrrasmussen.symphony.legacy"] || runner.loaded["com.pmrrasmussen.symphony.owner-repository"] {
		t.Fatalf("prior service was not the only one left loaded: %v", runner.loaded)
	}
	assertNoManagedPlist(t, options.LaunchAgentsDir)
	if _, statErr := os.Stat(filepath.Join(dir, ".symphony", "service", filepath.Base(legacy)+backupSuffix)); statErr != nil {
		t.Fatalf("failed migration left no recoverable backup: %v", statErr)
	}
}

func TestMigrateRestoresUnloadedLegacyPlistWithoutBootstrappingIt(t *testing.T) {
	dir, options, runner := serviceFixture(t)
	legacy, content := writeLegacyAgent(t, options.LaunchAgentsDir, legacyAgent{
		label:      "com.pmrrasmussen.symphony.legacy",
		repository: dir,
		executable: repositoryExecutable(t, dir),
		workflow:   filepath.Join(dir, "WORKFLOW.md"),
		logsRoot:   filepath.Join(dir, ".symphony", "logs"),
		keyFile:    options.LinearKeyFile,
	})
	runner.fail = "launchctl bootstrap"
	_, err := Migrate(context.Background(), options)
	if err == nil || !strings.Contains(err.Error(), "restored the prior LaunchAgent "+legacy) {
		t.Fatalf("err=%v", err)
	}
	if data, readErr := os.ReadFile(legacy); readErr != nil || string(data) != content {
		t.Fatalf("legacy plist not restored = %q err=%v", data, readErr)
	}
	if len(runner.loaded) != 0 {
		t.Fatalf("a service was loaded during rollback: %v", runner.loaded)
	}
	assertNoManagedPlist(t, options.LaunchAgentsDir)
}

// A hand-authored plist that sits on disk unloaded is the common case, and
// launchctl bootout reports a failure for it. That must still migrate.
func TestMigrateAdoptsLegacyAgentThatWasNeverLoaded(t *testing.T) {
	dir, options, runner := serviceFixture(t)
	legacy, _ := writeLegacyAgent(t, options.LaunchAgentsDir, legacyAgent{
		label:      "com.pmrrasmussen.symphony.legacy",
		repository: dir,
		executable: repositoryExecutable(t, dir),
		workflow:   filepath.Join(dir, "WORKFLOW.md"),
		logsRoot:   filepath.Join(dir, ".symphony", "logs"),
		keyFile:    options.LinearKeyFile,
	})
	migration, err := Migrate(context.Background(), options)
	if err != nil || !migration.Changed {
		t.Fatalf("migration=%#v err=%v", migration, err)
	}
	if callIndex(runner, "launchctl bootout gui/") < 0 {
		t.Fatalf("migration skipped the legacy bootout: %v", runner.calls)
	}
	if _, statErr := os.Stat(legacy); !os.IsNotExist(statErr) {
		t.Fatalf("legacy plist survived migration: %v", statErr)
	}
	if !runner.loaded[migration.Label] {
		t.Fatalf("managed service was not loaded: %v", runner.loaded)
	}
}

func TestMigrateAbortsWhenTheLegacyServiceStaysLoaded(t *testing.T) {
	dir, options, runner := serviceFixture(t)
	legacy, content := writeLegacyAgent(t, options.LaunchAgentsDir, legacyAgent{
		label:      "com.pmrrasmussen.symphony.legacy",
		repository: dir,
		executable: repositoryExecutable(t, dir),
		workflow:   filepath.Join(dir, "WORKFLOW.md"),
		logsRoot:   filepath.Join(dir, ".symphony", "logs"),
		keyFile:    options.LinearKeyFile,
	})
	runner.loaded = map[string]bool{"com.pmrrasmussen.symphony.legacy": true}
	runner.fail = "launchctl bootout"
	migration, err := Migrate(context.Background(), options)
	if err == nil || migration.Changed {
		t.Fatalf("migration=%#v err=%v", migration, err)
	}
	for _, want := range []string{"com.pmrrasmussen.symphony.legacy", "still loaded after bootout", "launchctl bootout"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("diagnostic %q missing from %v", want, err)
		}
	}
	if data, readErr := os.ReadFile(legacy); readErr != nil || string(data) != content {
		t.Fatalf("legacy plist changed = %q err=%v", data, readErr)
	}
	if !runner.loaded["com.pmrrasmussen.symphony.legacy"] {
		t.Fatalf("legacy service state = %v", runner.loaded)
	}
	assertNoManagedPlist(t, options.LaunchAgentsDir)
	if index := callIndex(runner, "launchctl bootstrap"); index >= 0 {
		t.Fatalf("aborted migration still bootstrapped a service: %v", runner.calls)
	}
}

type legacyAgent struct {
	label      string
	repository string
	executable string
	workflow   string
	logsRoot   string
	keyFile    string
}

// writeLegacyAgent renders a hand-authored, unmarked LaunchAgent in the same
// shape pre-installer operators wrote by hand.
func writeLegacyAgent(t *testing.T, launchDir string, agent legacyAgent) (string, string) {
	t.Helper()
	content := "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<plist version=\"1.0\">\n<dict>\n" +
		"<key>Label</key><string>" + agent.label + "</string>\n" +
		"<key>ProgramArguments</key><array>" +
		"<string>" + agent.executable + "</string>" +
		"<string>--workflow</string><string>" + agent.workflow + "</string>" +
		"<string>--logs-root</string><string>" + agent.logsRoot + "</string>" +
		"<string>--log-level</string><string>debug</string></array>\n" +
		"<key>WorkingDirectory</key><string>" + agent.repository + "</string>\n" +
		"<key>EnvironmentVariables</key><dict><key>PATH</key><string>/usr/bin:/bin</string>" +
		"<key>SYMPHONY_LINEAR_API_KEY_FILE</key><string>" + agent.keyFile + "</string></dict>\n" +
		"<key>RunAtLoad</key><true/>\n<key>KeepAlive</key><true/>\n<key>ThrottleInterval</key><integer>30</integer>\n" +
		"<key>StandardOutPath</key><string>" + filepath.Join(agent.repository, ".symphony", "service", "stdout.log") + "</string>\n" +
		"<key>StandardErrorPath</key><string>" + filepath.Join(agent.repository, ".symphony", "service", "stderr.log") + "</string>\n" +
		"</dict>\n</plist>\n"
	path := filepath.Join(launchDir, agent.label+".plist")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, content
}

func repositoryExecutable(t *testing.T, repository string) string {
	t.Helper()
	dir := filepath.Join(repository, ".symphony", "bin")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "symphony")
	if err := os.WriteFile(path, []byte("legacy binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func assertNoManagedPlist(t *testing.T, launchDir string) {
	t.Helper()
	path := filepath.Join(launchDir, "com.pmrrasmussen.symphony.owner-repository.plist")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("managed plist was written: %v", err)
	}
}

func callIndex(runner *fakeRunner, prefix string) int {
	for i, call := range runner.calls {
		if strings.HasPrefix(call, prefix) {
			return i
		}
	}
	return -1
}
