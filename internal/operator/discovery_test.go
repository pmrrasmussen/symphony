package operator

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type fakeInspector map[string]LaunchdStatus

func (f fakeInspector) Launchd(_ context.Context, label string) LaunchdStatus { return f[label] }

func TestDiscoverMultipleIndependentInstancesAndStaleSnapshot(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	first := fixtureWorkflow(t, dir, "first")
	second := fixtureWorkflow(t, dir, "second")
	firstStatus := filepath.Join(dir, "first-status.json")
	secondStatus := filepath.Join(dir, "second-status.json")
	write(t, firstStatus, `{"state":"running","updated_at":"2026-08-25T11:59:30Z"}`)
	write(t, secondStatus, `{"state":"running","updated_at":"2026-08-25T11:50:00Z"}`)
	writePlist(t, dir, labelPrefix+".zeta", first, filepath.Join(dir, "logs-first"), firstStatus)
	writePlist(t, dir, labelPrefix, second, filepath.Join(dir, "logs-second"), secondStatus)

	instances, err := Discover(context.Background(), Options{
		LaunchAgentsDir: dir, Now: func() time.Time { return now },
		Inspector: fakeInspector{labelPrefix: {Loaded: true, PID: 12, Process: true}, labelPrefix + ".zeta": {Loaded: true, PID: 13, Process: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := []string{instances[0].ID, instances[1].ID}, []string{labelPrefix, labelPrefix + ".zeta"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ordered IDs = %v, want %v", got, want)
	}
	if instances[0].Liveness != LivenessStale || instances[1].Liveness != LivenessRunning {
		t.Fatalf("livenesses = %s, %s", instances[0].Liveness, instances[1].Liveness)
	}
	if instances[1].Config == nil || instances[1].Config.WorkspaceSource == "" || instances[1].Config.ProjectSelector != "project-first" {
		t.Fatalf("missing normalized effective configuration: %#v", instances[1].Config)
	}
	if hasCode(instances[0], "duplicate_workflow") || hasCode(instances[1], "duplicate_workflow") {
		t.Fatal("independent instances were incorrectly marked duplicate")
	}
}

func TestParseXMLPlistFallback(t *testing.T) {
	values, err := parseXMLPlist([]byte("<?xml version=\"1.0\"?><plist version=\"1.0\"><dict><key>Label</key><string>com.pmrrasmussen.symphony.test</string><key>ProgramArguments</key><array><string>/bin/sh</string><string>--workflow</string></array></dict></plist>"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := values["Label"], "com.pmrrasmussen.symphony.test"; got != want {
		t.Fatalf("Label = %#v, want %#v", got, want)
	}
	if got, want := stringSlice(values["ProgramArguments"]), []string{"/bin/sh", "--workflow"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("arguments = %v, want %v", got, want)
	}
}

func TestDiscoverKeepsMalformedAndMissingWorkflowCandidates(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, labelPrefix+".broken.plist"), "not a plist")
	missing := filepath.Join(dir, "missing.md")
	writePlist(t, dir, labelPrefix+".missing", missing, filepath.Join(dir, "logs"), filepath.Join(dir, "missing-status.json"))

	instances, err := Discover(context.Background(), Options{LaunchAgentsDir: dir, Inspector: fakeInspector{}})
	if err != nil {
		t.Fatal(err)
	}
	if len(instances) != 2 || instances[0].Liveness != LivenessInvalid || instances[1].Liveness != LivenessInvalid {
		t.Fatalf("instances = %#v", instances)
	}
	if !hasCode(instances[0], "plist_invalid") || !hasCode(instances[1], "workflow_invalid") {
		t.Fatalf("findings = %#v %#v", instances[0].Findings, instances[1].Findings)
	}
}

func TestDiscoverReportsDuplicateUnsafePathsAndMissingStatus(t *testing.T) {
	dir := t.TempDir()
	workflow := fixtureWorkflow(t, dir, "shared")
	status := filepath.Join(dir, "status.json")
	writePlist(t, dir, labelPrefix+".one", workflow, filepath.Join(dir, "logs"), status)
	writePlist(t, dir, labelPrefix+".two", workflow, filepath.Join(dir, "logs"), status)

	instances, err := Discover(context.Background(), Options{LaunchAgentsDir: dir, Inspector: fakeInspector{}})
	if err != nil {
		t.Fatal(err)
	}
	for _, instance := range instances {
		for _, code := range []string{"duplicate_workflow", "duplicate_status_file", "duplicate_log_root", "duplicate_workspace_root", "duplicate_workspace_source", "status_unavailable"} {
			if !hasCode(instance, code) {
				t.Fatalf("%s missing %s: %#v", instance.ID, code, instance.Findings)
			}
		}
	}
}

func TestSnapshotCannotMakeStoppedLaunchAgentRunning(t *testing.T) {
	dir := t.TempDir()
	workflow := fixtureWorkflow(t, dir, "stopped")
	status := filepath.Join(dir, "status.json")
	write(t, status, `{"state":"running","updated_at":"2026-08-25T11:59:30Z"}`)
	writePlist(t, dir, labelPrefix+".stopped", workflow, filepath.Join(dir, "logs"), status)

	instances, err := Discover(context.Background(), Options{LaunchAgentsDir: dir, Now: func() time.Time { return time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC) }, Inspector: fakeInspector{labelPrefix + ".stopped": {Loaded: true}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := instances[0].Liveness; got != LivenessStopped {
		t.Fatalf("liveness = %s, want stopped", got)
	}
}

func TestCredentialMetadataNeverExposesSecretValues(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PMR74_LINEAR", "linear-secret-value")
	t.Setenv("PMR74_GITHUB", "github-secret-value")
	workflow := filepath.Join(dir, "WORKFLOW.md")
	write(t, workflow, "---\ntracker: {kind: linear, provider: {project_slug_id: safe, api_key: $PMR74_LINEAR}, active_states: [Todo], terminal_states: [Done]}\nworkspace: {root: work, source_root: .}\ncodex: {command: go}\ngithub: {owner: owner, repository: repo, token: $PMR74_GITHUB}\n---\nprompt")
	logs := filepath.Join(dir, "logs")
	if err := os.Mkdir(logs, 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(logs, "symphony.jsonl"), `{"time":"2026-08-25T12:00:00Z","level":"INFO","msg":"linear-secret-value github-secret-value"}`+"\n")
	writePlist(t, dir, labelPrefix+".secret", workflow, logs, "")
	instances, err := Discover(context.Background(), Options{LaunchAgentsDir: dir, Inspector: fakeInspector{}})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(instances[0])
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, secret := range []string{"linear-secret-value", "github-secret-value"} {
		if strings.Contains(text, secret) {
			t.Fatalf("operator model exposed %q: %s", secret, text)
		}
	}
	if got := instances[0].Config.Credentials.Tracker.EnvironmentNames; !reflect.DeepEqual(got, []string{"PMR74_LINEAR"}) {
		t.Fatalf("tracker credential references = %v", got)
	}
}

func TestDiscoverUsesLaunchAgentCredentialFileReferenceWithoutLeakingIt(t *testing.T) {
	dir := t.TempDir()
	secretFile := filepath.Join(dir, "daemon-key")
	write(t, secretFile, "daemon-secret-value")
	workflow := filepath.Join(dir, "WORKFLOW.md")
	write(t, workflow, "---\ntracker: {kind: linear, provider: {project_slug_id: service, api_key_file: $PMR74_SERVICE_FILE}, active_states: [Todo], terminal_states: [Done]}\nworkspace: {root: work, source_root: .}\ncodex: {command: go}\n---\nprompt")
	writePlistWithEnvironment(t, dir, labelPrefix+".service", workflow, filepath.Join(dir, "logs"), "", map[string]string{"PMR74_SERVICE_FILE": secretFile})
	instances, err := Discover(context.Background(), Options{LaunchAgentsDir: dir, Inspector: fakeInspector{}})
	if err != nil {
		t.Fatal(err)
	}
	if instances[0].Config == nil {
		t.Fatalf("service workflow was not loaded: %#v", instances[0].Findings)
	}
	credentials := instances[0].Config.Credentials.Tracker
	if !reflect.DeepEqual(credentials.EnvironmentNames, []string{"PMR74_SERVICE_FILE"}) || !reflect.DeepEqual(credentials.FileReferences, []string{secretFile}) {
		t.Fatalf("credential metadata = %#v", credentials)
	}
	encoded, _ := json.Marshal(instances[0])
	if strings.Contains(string(encoded), "daemon-secret-value") {
		t.Fatalf("operator model exposed credential value: %s", encoded)
	}
}

func fixtureWorkflow(t *testing.T, dir, name string) string {
	t.Helper()
	workflow := filepath.Join(dir, name+".md")
	source := filepath.Join(dir, name+"-source")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, workflow, "---\ntracker: {kind: linear, provider: {project_slug_id: project-"+name+", api_key: dummy}, active_states: [Todo], terminal_states: [Done]}\nworkspace: {root: "+name+"-work, source_root: "+source+"}\ncodex: {command: go}\n---\nprompt")
	return workflow
}

func writePlist(t *testing.T, dir, label, workflow, logs, status string) {
	writePlistWithEnvironment(t, dir, label, workflow, logs, status, nil)
}

func writePlistWithEnvironment(t *testing.T, dir, label, workflow, logs, status string, environment map[string]string) {
	t.Helper()
	args := "<string>/bin/sh</string><string>--workflow</string><string>" + workflow + "</string><string>--logs-root=" + logs + "</string>"
	if status != "" {
		args += "<string>--status-file</string><string>" + status + "</string>"
	}
	env := ""
	if len(environment) > 0 {
		env = "<key>EnvironmentVariables</key><dict>"
		for name, value := range environment {
			env += "<key>" + name + "</key><string>" + value + "</string>"
		}
		env += "</dict>"
	}
	content := "<?xml version=\"1.0\" encoding=\"UTF-8\"?><!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\"><plist version=\"1.0\"><dict><key>Label</key><string>" + label + "</string><key>ProgramArguments</key><array>" + args + "</array><key>WorkingDirectory</key><string>" + dir + "</string>" + env + "</dict></plist>"
	write(t, filepath.Join(dir, label+".plist"), content)
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func hasCode(instance Instance, code string) bool {
	for _, finding := range instance.Findings {
		if finding.Code == code {
			return true
		}
	}
	return false
}
