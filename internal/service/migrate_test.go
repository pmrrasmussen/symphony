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
	assertNoBackup(t, dir, legacy)
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

// An operator who renames a plist and its Label without reloading leaves a
// registered job that no per-label check can see: the plist on disk is a
// perfectly compatible candidate while the old label keeps scheduling the same
// workflow. Enumerating the loaded set is the only way to catch it.
func TestMigrateRefusesWhenARenamedLabelIsStillLoaded(t *testing.T) {
	dir, options, runner := serviceFixture(t)
	legacy, content := writeLegacyAgent(t, options.LaunchAgentsDir, legacyAgent{
		label:      "com.pmrrasmussen.symphony.renamed",
		repository: dir,
		executable: repositoryExecutable(t, dir),
		workflow:   filepath.Join(dir, "WORKFLOW.md"),
		logsRoot:   filepath.Join(dir, ".symphony", "logs"),
		keyFile:    options.LinearKeyFile,
	})
	// The job still registered under the pre-rename label has no plist at all.
	runner.loaded = map[string]bool{"com.pmrrasmussen.symphony": true}
	migration, err := Migrate(context.Background(), options)
	if err == nil || migration.Changed {
		t.Fatalf("migration=%#v err=%v", migration, err)
	}
	for _, want := range []string{"other Symphony services are loaded", "com.pmrrasmussen.symphony", "launchctl bootout"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("diagnostic %q missing from %v", want, err)
		}
	}
	if data, readErr := os.ReadFile(legacy); readErr != nil || string(data) != content {
		t.Fatalf("legacy plist changed = %q err=%v", data, readErr)
	}
	assertNoManagedPlist(t, options.LaunchAgentsDir)
	assertNoBackup(t, dir, legacy)
	if index := callIndex(runner, "launchctl bootout"); index >= 0 {
		t.Fatalf("a refused migration still touched launchd: %v", runner.calls)
	}
}

// A loaded service that belongs to another repository is accounted for and
// must not block this repository's migration.
func TestMigrateIgnoresLoadedServicesOfOtherRepositories(t *testing.T) {
	dir, options, runner := serviceFixture(t)
	other := t.TempDir()
	writeLegacyAgent(t, options.LaunchAgentsDir, legacyAgent{
		label: "com.pmrrasmussen.symphony.other", repository: other, executable: repositoryExecutable(t, other),
		workflow: filepath.Join(other, "WORKFLOW.md"), logsRoot: filepath.Join(other, ".symphony", "logs"), keyFile: options.LinearKeyFile,
	})
	writeLegacyAgent(t, options.LaunchAgentsDir, legacyAgent{
		label: "com.pmrrasmussen.symphony.legacy", repository: dir, executable: repositoryExecutable(t, dir),
		workflow: filepath.Join(dir, "WORKFLOW.md"), logsRoot: filepath.Join(dir, ".symphony", "logs"), keyFile: options.LinearKeyFile,
	})
	runner.loaded = map[string]bool{
		"com.pmrrasmussen.symphony.other":  true,
		"com.pmrrasmussen.symphony.legacy": true,
	}
	migration, err := Migrate(context.Background(), options)
	if err != nil || !migration.Changed {
		t.Fatalf("migration=%#v err=%v", migration, err)
	}
	if !runner.loaded["com.pmrrasmussen.symphony.other"] {
		t.Fatalf("another repository's service was unloaded: %v", runner.loaded)
	}
}

// A legacy service that is running while launchd cannot be observed must never
// be treated as absent, because print and bootout target the same service
// string and fail together when the label is what is wrong.
func TestMigrateRefusesWhenTheLegacyServiceCannotBeObserved(t *testing.T) {
	dir, options, runner := serviceFixture(t)
	legacy, content := writeLegacyAgent(t, options.LaunchAgentsDir, legacyAgent{
		label: "com.pmrrasmussen.symphony.legacy", repository: dir, executable: repositoryExecutable(t, dir),
		workflow: filepath.Join(dir, "WORKFLOW.md"), logsRoot: filepath.Join(dir, ".symphony", "logs"), keyFile: options.LinearKeyFile,
	})
	runner.loaded = map[string]bool{"com.pmrrasmussen.symphony.legacy": true}
	runner.printFail = map[string]bool{"com.pmrrasmussen.symphony.legacy": true}
	migration, err := Migrate(context.Background(), options)
	if err == nil || migration.Changed {
		t.Fatalf("migration=%#v err=%v", migration, err)
	}
	if !strings.Contains(err.Error(), "cannot verify that legacy service com.pmrrasmussen.symphony.legacy is unloaded") {
		t.Fatalf("err=%v", err)
	}
	if data, readErr := os.ReadFile(legacy); readErr != nil || string(data) != content {
		t.Fatalf("legacy plist changed = %q err=%v", data, readErr)
	}
	assertNoManagedPlist(t, options.LaunchAgentsDir)
	assertNoBackup(t, dir, legacy)
	if index := callIndex(runner, "launchctl bootstrap"); index >= 0 {
		t.Fatalf("an unverifiable unload still bootstrapped a service: %v", runner.calls)
	}
}

// An unobservable print with no other evidence of absence is not proof that
// nothing is running, even when the service really is not loaded.
func TestMigrateRefusesWhenNeitherObservationProvesAbsence(t *testing.T) {
	dir, options, runner := serviceFixture(t)
	legacy, _ := writeLegacyAgent(t, options.LaunchAgentsDir, legacyAgent{
		label: "com.pmrrasmussen.symphony.legacy", repository: dir, executable: repositoryExecutable(t, dir),
		workflow: filepath.Join(dir, "WORKFLOW.md"), logsRoot: filepath.Join(dir, ".symphony", "logs"), keyFile: options.LinearKeyFile,
	})
	runner.printFail = map[string]bool{"com.pmrrasmussen.symphony.legacy": true}
	runner.fail = "launchctl bootout"
	_, err := Migrate(context.Background(), options)
	if err == nil || !strings.Contains(err.Error(), "cannot verify that legacy service") {
		t.Fatalf("err=%v", err)
	}
	if _, statErr := os.Stat(legacy); statErr != nil {
		t.Fatalf("legacy plist removed on an unverifiable unload: %v", statErr)
	}
	assertNoManagedPlist(t, options.LaunchAgentsDir)
}

// launchd may report an unload as still in progress. bootout answering
// authoritatively is trusted, but a bootout that only succeeds is verified,
// which is what the bounded retry exists for.
func TestMigrateWaitsForAnAsynchronousUnload(t *testing.T) {
	dir, options, runner := serviceFixture(t)
	writeLegacyAgent(t, options.LaunchAgentsDir, legacyAgent{
		label: "com.pmrrasmussen.symphony.legacy", repository: dir, executable: repositoryExecutable(t, dir),
		workflow: filepath.Join(dir, "WORKFLOW.md"), logsRoot: filepath.Join(dir, ".symphony", "logs"), keyFile: options.LinearKeyFile,
	})
	runner.loaded = map[string]bool{"com.pmrrasmussen.symphony.legacy": true}
	runner.unloadLag = map[string]int{"com.pmrrasmussen.symphony.legacy": 1}
	migration, err := Migrate(context.Background(), options)
	if err != nil || !migration.Changed {
		t.Fatalf("migration=%#v err=%v", migration, err)
	}
	prints := 0
	for _, call := range runner.calls {
		if strings.HasPrefix(call, "launchctl print gui/") && strings.HasSuffix(call, "com.pmrrasmussen.symphony.legacy") {
			prints++
		}
	}
	if prints < 3 {
		t.Fatalf("the unload retry was not exercised, prints=%d calls=%v", prints, runner.calls)
	}
}

// bootout stating that launchd has no such job is authoritative, so a plist
// that was never loaded migrates even when print cannot answer.
func TestMigrateTrustsBootoutProofOfAbsence(t *testing.T) {
	dir, options, runner := serviceFixture(t)
	writeLegacyAgent(t, options.LaunchAgentsDir, legacyAgent{
		label: "com.pmrrasmussen.symphony.legacy", repository: dir, executable: repositoryExecutable(t, dir),
		workflow: filepath.Join(dir, "WORKFLOW.md"), logsRoot: filepath.Join(dir, ".symphony", "logs"), keyFile: options.LinearKeyFile,
	})
	runner.printFail = map[string]bool{"com.pmrrasmussen.symphony.legacy": true}
	migration, err := Migrate(context.Background(), options)
	if err != nil || !migration.Changed {
		t.Fatalf("migration=%#v err=%v", migration, err)
	}
}

// A hand-authored plist can already occupy the managed path and label. That
// branch must adopt it in place with the same guarantees as a separate one.
func TestMigrateAdoptsLegacyAgentAtTheManagedPath(t *testing.T) {
	dir, options, runner := serviceFixture(t)
	legacy, _ := writeLegacyAgent(t, options.LaunchAgentsDir, legacyAgent{
		label: "com.pmrrasmussen.symphony.owner-repository", repository: dir, executable: repositoryExecutable(t, dir),
		workflow: filepath.Join(dir, "WORKFLOW.md"), logsRoot: filepath.Join(dir, ".symphony", "logs"), keyFile: options.LinearKeyFile,
	})
	runner.loaded = map[string]bool{"com.pmrrasmussen.symphony.owner-repository": true}
	migration, err := Migrate(context.Background(), options)
	if err != nil || !migration.Changed || migration.PlistPath != legacy {
		t.Fatalf("migration=%#v err=%v", migration, err)
	}
	data, readErr := os.ReadFile(legacy)
	if readErr != nil || !strings.Contains(string(data), "<key>SymphonyManaged</key><true/>") {
		t.Fatalf("plist at the managed path was not replaced = %s err=%v", data, readErr)
	}
	if _, statErr := os.Stat(migration.Backup); statErr != nil {
		t.Fatalf("no backup of the replaced plist: %v", statErr)
	}
	entries, readDirErr := os.ReadDir(options.LaunchAgentsDir)
	if readDirErr != nil || len(entries) != 1 {
		t.Fatalf("LaunchAgents entries=%v err=%v", entries, readDirErr)
	}
}

// The in-place branch must not start a service that was deliberately stopped,
// and must report the restoration the documentation promises.
func TestMigrateRollsBackInPlaceAdoptionWithoutStartingIt(t *testing.T) {
	dir, options, runner := serviceFixture(t)
	legacy, content := writeLegacyAgent(t, options.LaunchAgentsDir, legacyAgent{
		label: "com.pmrrasmussen.symphony.owner-repository", repository: dir, executable: repositoryExecutable(t, dir),
		workflow: filepath.Join(dir, "WORKFLOW.md"), logsRoot: filepath.Join(dir, ".symphony", "logs"), keyFile: options.LinearKeyFile,
	})
	runner.fail = "launchctl kickstart"
	_, err := Migrate(context.Background(), options)
	if err == nil || !strings.Contains(err.Error(), "restored the prior LaunchAgent "+legacy) {
		t.Fatalf("err=%v", err)
	}
	if data, readErr := os.ReadFile(legacy); readErr != nil || string(data) != content {
		t.Fatalf("legacy plist not restored = %q err=%v", data, readErr)
	}
	if len(runner.loaded) != 0 {
		t.Fatalf("a rollback started a service that was not running: %v", runner.loaded)
	}
}

// A repository can already have a managed plist on disk while a legacy agent
// is the one actually running. A failed migration must restore both files but
// bootstrap only the service that was loaded.
func TestMigrateRollbackRestoresOnlyTheServiceThatWasLoaded(t *testing.T) {
	dir, options, runner := serviceFixture(t)
	managed, _, err := Install(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	installed, err := os.ReadFile(managed.PlistPath)
	if err != nil {
		t.Fatal(err)
	}
	// The managed plist stays on disk but is no longer loaded, which is the
	// state the documented manual undo leaves behind.
	delete(runner.loaded, managed.Label)
	legacy, legacyContent := writeLegacyAgent(t, options.LaunchAgentsDir, legacyAgent{
		label: "com.pmrrasmussen.symphony.legacy", repository: dir, executable: repositoryExecutable(t, dir),
		workflow: filepath.Join(dir, "WORKFLOW.md"), logsRoot: filepath.Join(dir, ".symphony", "logs"), keyFile: options.LinearKeyFile,
	})
	runner.loaded = map[string]bool{"com.pmrrasmussen.symphony.legacy": true}
	runner.fail = "launchctl kickstart"
	if _, err := Migrate(context.Background(), options); err == nil {
		t.Fatal("failed migration reported success")
	}
	if data, readErr := os.ReadFile(legacy); readErr != nil || string(data) != legacyContent {
		t.Fatalf("legacy plist not restored = %q err=%v", data, readErr)
	}
	if data, readErr := os.ReadFile(managed.PlistPath); readErr != nil || string(data) != string(installed) {
		t.Fatalf("managed plist not restored = %q err=%v", data, readErr)
	}
	if !runner.loaded["com.pmrrasmussen.symphony.legacy"] || runner.loaded[managed.Label] || len(runner.loaded) != 1 {
		t.Fatalf("rollback loaded services = %v, want only the legacy one", runner.loaded)
	}
}

// A repository reached through a symlink is the same repository. Detection
// must not depend on which spelling of the path each side recorded.
func TestMigrateDetectsLegacyAgentThroughASymlinkedRepository(t *testing.T) {
	dir, options, runner := serviceFixture(t)
	link := filepath.Join(t.TempDir(), "linked-repository")
	if err := os.Symlink(dir, link); err != nil {
		t.Skip(err)
	}
	// The legacy plist records the real paths; the command is run through the
	// symlink, and the fake git echoes that spelling back verbatim.
	writeLegacyAgent(t, options.LaunchAgentsDir, legacyAgent{
		label: "com.pmrrasmussen.symphony.legacy", repository: dir, executable: repositoryExecutable(t, dir),
		workflow: filepath.Join(dir, "WORKFLOW.md"), logsRoot: filepath.Join(dir, ".symphony", "logs"), keyFile: options.LinearKeyFile,
	})
	runner.root, options.Repository = link, link
	runner.loaded = map[string]bool{"com.pmrrasmussen.symphony.legacy": true}
	migration, err := Migrate(context.Background(), options)
	if err != nil || !migration.Changed || migration.Legacy != "com.pmrrasmussen.symphony.legacy" {
		t.Fatalf("migration=%#v err=%v", migration, err)
	}
	status, err := Status(context.Background(), options)
	if err != nil || status.ID != migration.Label {
		t.Fatalf("status=%#v err=%v", status, err)
	}
}

// A replaced plist must come back with the mode it had, not the mode a managed
// plist would get.
func TestMigrateRollbackPreservesLegacyPlistMode(t *testing.T) {
	dir, options, runner := serviceFixture(t)
	legacy, _ := writeLegacyAgent(t, options.LaunchAgentsDir, legacyAgent{
		label: "com.pmrrasmussen.symphony.legacy", repository: dir, executable: repositoryExecutable(t, dir),
		workflow: filepath.Join(dir, "WORKFLOW.md"), logsRoot: filepath.Join(dir, ".symphony", "logs"), keyFile: options.LinearKeyFile,
	})
	if err := os.Chmod(legacy, 0o644); err != nil {
		t.Fatal(err)
	}
	runner.fail = "launchctl bootstrap"
	if _, err := Migrate(context.Background(), options); err == nil {
		t.Fatal("failed migration reported success")
	}
	info, err := os.Stat(legacy)
	if err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("restored mode = %v err=%v, want 0644", info.Mode().Perm(), err)
	}
}

// An unverified launchd is not evidence that nothing is running.
func TestMigrateRefusesWhenLaunchdCannotBeEnumerated(t *testing.T) {
	dir, options, runner := serviceFixture(t)
	legacy, content := writeLegacyAgent(t, options.LaunchAgentsDir, legacyAgent{
		label: "com.pmrrasmussen.symphony.legacy", repository: dir, executable: repositoryExecutable(t, dir),
		workflow: filepath.Join(dir, "WORKFLOW.md"), logsRoot: filepath.Join(dir, ".symphony", "logs"), keyFile: options.LinearKeyFile,
	})
	runner.fail = "launchctl list"
	_, err := Migrate(context.Background(), options)
	if err == nil || !strings.Contains(err.Error(), "cannot enumerate loaded Symphony services") {
		t.Fatalf("err=%v", err)
	}
	if data, readErr := os.ReadFile(legacy); readErr != nil || string(data) != content {
		t.Fatalf("legacy plist changed = %q err=%v", data, readErr)
	}
	assertNoManagedPlist(t, options.LaunchAgentsDir)
	assertNoBackup(t, dir, legacy)
}

func assertNoBackup(t *testing.T, repository, legacyPlist string) {
	t.Helper()
	path := filepath.Join(repository, ".symphony", "service", filepath.Base(legacyPlist)+backupSuffix)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("a migration that changed nothing left %s: %v", path, err)
	}
}
