package operator

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"howett.net/plist"
)

type fakeInspector map[string]LaunchdStatus

func (f fakeInspector) Launchd(_ context.Context, label string) LaunchdStatus { return f[label] }

// fakeAuthenticatedAgentCommand stands in for the codex binary the discovery
// fixtures need since every backend, not only Claude, now gets a real
// agent_authentication probe: a real program on PATH, like the "go"
// placeholder these fixtures used before that probe existed, no longer
// reports a session, and Discover turns that failure into a SeverityError
// finding that finalizeLiveness treats as LivenessInvalid.
func fakeAuthenticatedAgentCommand(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "fake-codex")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDiscoverMultipleIndependentInstancesAndStaleSnapshot(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	first := fixtureWorkflow(t, dir, "first")
	second := fixtureWorkflow(t, dir, "second")
	firstStatus := filepath.Join(ownerOnlyDir(t, dir, "first-status"), "status.json")
	secondStatus := filepath.Join(ownerOnlyDir(t, dir, "second-status"), "status.json")
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

func TestParsePlistDecodesXML(t *testing.T) {
	values, err := parsePlist([]byte("<?xml version=\"1.0\"?><plist version=\"1.0\"><dict><key>Label</key><string>com.pmrrasmussen.symphony.test</string><key>ProgramArguments</key><array><string>/bin/sh</string><string>--workflow</string></array><key>ThrottleInterval</key><integer>10</integer></dict></plist>"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := values["Label"], "com.pmrrasmussen.symphony.test"; got != want {
		t.Fatalf("Label = %#v, want %#v", got, want)
	}
	if got, want := stringSlice(values["ProgramArguments"]), []string{"/bin/sh", "--workflow"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("arguments = %v, want %v", got, want)
	}
	if got, want := values["ThrottleInterval"], uint64(10); got != want {
		t.Fatalf("ThrottleInterval = %#v, want %#v", got, want)
	}
}

// TestParsePlistDecodesBinary is new coverage neither the old plutil branch
// nor the old XML fallback exercised in the same test run: a binary plist,
// the format launchd itself writes and rewrites, decoded by the same call
// path as the fixtures above.
func TestParsePlistDecodesBinary(t *testing.T) {
	encoded, err := plist.Marshal(map[string]any{
		"Label":            "com.pmrrasmussen.symphony.test",
		"ProgramArguments": []string{"/bin/sh", "--workflow"},
		"ThrottleInterval": int64(10),
	}, plist.BinaryFormat)
	if err != nil {
		t.Fatal(err)
	}
	values, err := parsePlist(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := values["Label"], "com.pmrrasmussen.symphony.test"; got != want {
		t.Fatalf("Label = %#v, want %#v", got, want)
	}
	if got, want := stringSlice(values["ProgramArguments"]), []string{"/bin/sh", "--workflow"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("arguments = %v, want %v", got, want)
	}
	if got, want := values["ThrottleInterval"], uint64(10); got != want {
		t.Fatalf("ThrottleInterval = %#v, want %#v", got, want)
	}
}

func TestParsePlistRejectsMalformedInput(t *testing.T) {
	if _, err := parsePlist([]byte("not a plist")); err == nil {
		t.Fatal("expected an error for malformed plist input")
	}
}

func TestParsePlistRejectsNonDictRoot(t *testing.T) {
	if _, err := parsePlist([]byte("<?xml version=\"1.0\"?><plist version=\"1.0\"><array><string>a</string></array></plist>")); err == nil {
		t.Fatal("expected an error for a non-dict plist root")
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
	status := filepath.Join(ownerOnlyDir(t, dir, "status"), "status.json")
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
	status := filepath.Join(ownerOnlyDir(t, dir, "status"), "status.json")
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

func TestReadSnapshotProjectsSafeRuntimeFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status.json")
	write(t, path, `{
"schema_version":1,"pid":73,"process_started_at":"2026-08-25T10:00:00Z","generated_at":"2026-08-25T12:00:00Z","state":"running",
"coordinator":{"claimed":2,"running":[{"issue_identifier":"PMR-75","issue_state":"In Progress","attempt":1,"turn_count":3,"started_at":"2026-08-25T11:00:00Z","last_activity_at":"2026-08-25T11:59:00Z","usage":{"input_tokens":8,"output_tokens":5,"total_tokens":13},"issue_usage":{"input_tokens":600,"output_tokens":150,"total_tokens":750},"rate_limit":{"remaining":4},"outstanding_operation":{"type":"mcpToolCall","name":"github_pr_context","started_at":"2026-08-25T11:59:30Z","age_ms":30000}}],"retrying":[{"issue_identifier":"PMR-76","attempt":2,"kind":"retry","reason":"timeout","due_at":"2026-08-25T12:01:00Z","issue_usage":{"total_tokens":4200}}],"waiting":[{"issue_identifier":"PMR-77","issue_state":"Merging","reason":"at_capacity","since":"2026-08-25T11:50:00Z","waiting_ms":600000},{"issue_identifier":"PMR-90","issue_state":"Todo","reason":"blocked_by_relation","blocked_by":["PMR-80"],"since":"2026-08-25T11:55:00Z","waiting_ms":300000}]},
"untrusted_prompt":"must not be projected"}`)

	snapshot, err := readSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.PID != 73 || snapshot.UpdatedAt.Format(time.RFC3339) != "2026-08-25T12:00:00Z" || len(snapshot.Coordinator.Running) != 1 || snapshot.Coordinator.Running[0].Usage.TotalTokens != 13 || len(snapshot.Coordinator.Retrying) != 1 || len(snapshot.Coordinator.Waiting) != 2 || snapshot.Coordinator.Waiting[0].IssueIdentifier != "PMR-77" || snapshot.Coordinator.Waiting[0].Reason != "at_capacity" {
		t.Fatalf("snapshot=%#v", snapshot)
	}
	if blocked := snapshot.Coordinator.Waiting[1]; blocked.IssueIdentifier != "PMR-90" || blocked.Reason != "blocked_by_relation" || len(blocked.BlockedBy) != 1 || blocked.BlockedBy[0] != "PMR-80" {
		t.Fatalf("blocked waiting entry=%#v", blocked)
	}
	// The per-issue total is the one field on these entries that is not about
	// the attempt in front of you (PMR-151). It decodes on both, and this is
	// where its wire name is pinned: internal/operator no longer re-declares
	// the snapshot, so this JSON is the whole contract between the coordinator
	// that writes it and the dashboard that reads it.
	if got := snapshot.Coordinator.Running[0].IssueUsage; got.InputTokens != 600 || got.OutputTokens != 150 || got.TotalTokens != 750 {
		t.Fatalf("running issue usage=%#v", got)
	}
	if got := snapshot.Coordinator.Retrying[0].IssueUsage; got.TotalTokens != 4200 {
		t.Fatalf("retrying issue usage=%#v", got)
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "untrusted_prompt") {
		t.Fatalf("snapshot projected arbitrary field: %s", encoded)
	}
}

func TestEffectiveConfigReportsResolvedAgentBackendAlongsideCodexKeys(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	workflow := fixtureWorkflow(t, dir, "backend")
	status := filepath.Join(ownerOnlyDir(t, dir, "backend-status"), "status.json")
	write(t, status, `{"state":"running","updated_at":"2026-08-25T11:59:30Z"}`)
	writePlist(t, dir, labelPrefix, workflow, filepath.Join(dir, "logs"), status)

	instances, err := Discover(context.Background(), Options{
		LaunchAgentsDir: dir, Now: func() time.Time { return now },
		Inspector: fakeInspector{labelPrefix: {Loaded: true, PID: 21, Process: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if instances[0].Config == nil {
		t.Fatalf("missing effective configuration: %#v", instances[0])
	}
	// The fixture omits agent.backend, so the projection must report the
	// resolved default rather than an empty selection.
	if got, want := instances[0].Config.AgentBackend, "codex"; got != want {
		t.Fatalf("agent backend = %q, want %q", got, want)
	}
	encoded, err := json.Marshal(instances[0].Config)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"agent_backend":"codex"`, fmt.Sprintf(`"codex_command":%q`, filepath.Join(dir, "fake-codex"))} {
		if !strings.Contains(string(encoded), want) {
			t.Fatalf("effective configuration JSON missing %s:\n%s", want, encoded)
		}
	}
}

func TestEffectiveConfigReportsOnlyTheSelectedBackendSettings(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 8, 26, 14, 55, 0, 0, time.UTC)
	status := filepath.Join(ownerOnlyDir(t, dir, "backend-status"), "status.json")
	write(t, status, `{"state":"running","updated_at":"2026-08-26T14:54:30Z"}`)

	for _, test := range []struct {
		backend string
		block   string
		turn    time.Duration
		stall   time.Duration
		present []string
		absent  []string
	}{
		{
			backend: "codex",
			block:   "codex: {command: codex-run, approval_policy: never, thread_sandbox: workspace-write, turn_timeout_ms: 1001, read_timeout_ms: 1002, start_timeout_ms: 1003, stall_timeout_ms: 1004}\nclaude: {command: claude-run, model: opus, turn_timeout_ms: 2001, stall_timeout_ms: 2004}",
			turn:    1001 * time.Millisecond,
			stall:   1004 * time.Millisecond,
			present: []string{`"codex_command":"codex-run"`, `"read_timeout":1002000000`, `"start_timeout":1003000000`},
			absent:  []string{"claude_command", "claude_model"},
		},
		{
			backend: "claude",
			// Give Codex deliberately different values so this test cannot pass
			// by projecting a shared default or coincidentally equal timeout.
			block:   "codex: {command: codex-run, approval_policy: never, thread_sandbox: workspace-write, turn_timeout_ms: 1001, read_timeout_ms: 1002, start_timeout_ms: 1003, stall_timeout_ms: 1004}\nclaude: {command: claude-run, model: opus, turn_timeout_ms: 2001, stall_timeout_ms: 2004}",
			turn:    2001 * time.Millisecond,
			stall:   2004 * time.Millisecond,
			present: []string{`"claude_command":"claude-run"`, `"claude_model":"opus"`},
			absent:  []string{"codex_command", "codex_approval_policy", "codex_thread_sandbox", "read_timeout", "start_timeout"},
		},
	} {
		t.Run(test.backend, func(t *testing.T) {
			workflow := filepath.Join(dir, test.backend+".md")
			source := filepath.Join(dir, test.backend+"-source")
			if err := os.Mkdir(source, 0o700); err != nil {
				t.Fatal(err)
			}
			write(t, workflow, "---\ntracker: {kind: linear, provider: {project_slug_id: project, api_key: dummy}, active_states: [Todo], terminal_states: [Done]}\nworkspace: {root: work, source_root: "+source+"}\nagent: {backend: "+test.backend+"}\n"+test.block+"\n---\nprompt")
			label := labelPrefix + "." + test.backend
			writePlist(t, dir, label, workflow, filepath.Join(dir, "logs"), status)

			instances, err := Discover(context.Background(), Options{LaunchAgentsDir: dir, Now: func() time.Time { return now }, Inspector: fakeInspector{label: {Loaded: true, PID: 21, Process: true}}})
			if err != nil {
				t.Fatal(err)
			}
			var instance *Instance
			for i := range instances {
				if instances[i].ID == label {
					instance = &instances[i]
					break
				}
			}
			if instance == nil || instance.Config == nil {
				t.Fatalf("missing effective configuration: %#v", instances)
			}
			if got := instance.Config.TurnTimeout; got != test.turn {
				t.Fatalf("turn timeout = %s, want %s", got, test.turn)
			}
			if got := instance.Config.StallTimeout; got != test.stall {
				t.Fatalf("stall timeout = %s, want %s", got, test.stall)
			}
			encoded, err := json.Marshal(instance.Config)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range test.present {
				if !strings.Contains(string(encoded), want) {
					t.Fatalf("effective configuration missing %s:\n%s", want, encoded)
				}
			}
			for _, forbidden := range test.absent {
				if strings.Contains(string(encoded), forbidden) {
					t.Fatalf("effective configuration exposed %s for %s:\n%s", forbidden, test.backend, encoded)
				}
			}
		})
	}
}

func TestRecentLogReadsBoundedTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "symphony.jsonl")
	write(t, path, strings.Repeat("x", maxRecentLogBytes+1)+"\n"+`{"time":"2026-08-25T12:00:00Z","level":"INFO","msg":"workspace prepared"}`+"\n")

	events, err := recentLog(path, 20, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Message != "workspace prepared" {
		t.Fatalf("events=%#v", events)
	}
}

func TestCredentialMetadataNeverExposesSecretValues(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PMR74_LINEAR", "linear-secret-value")
	t.Setenv("PMR74_GITHUB", "github-secret-value")
	workflow := filepath.Join(dir, "WORKFLOW.md")
	write(t, workflow, "---\ntracker: {kind: linear, provider: {project_slug_id: safe, api_key: $PMR74_LINEAR}, active_states: [Todo], terminal_states: [Done]}\nworkspace: {root: work, source_root: .}\ncodex: {command: "+fakeAuthenticatedAgentCommand(t, dir)+"}\ngithub: {owner: owner, repository: repo, token: $PMR74_GITHUB}\n---\nprompt")
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
	write(t, workflow, "---\ntracker: {kind: linear, provider: {project_slug_id: service, api_key_file: $PMR74_SERVICE_FILE}, active_states: [Todo], terminal_states: [Done]}\nworkspace: {root: work, source_root: .}\ncodex: {command: "+fakeAuthenticatedAgentCommand(t, dir)+"}\n---\nprompt")
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

// countingAgentCommand is an agent CLI that records every invocation, so a test
// can count the subprocesses a sweep spawns rather than time them. It is spelled
// with the trailing "app-server" a real codex.command carries, because the
// authentication probe deliberately refuses to re-invoke anything else.
func countingAgentCommand(t *testing.T, dir, counter string) string {
	t.Helper()
	path := filepath.Join(dir, "counting-codex")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf x >> "+counter+"\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path + " app-server"
}

func probeCount(t *testing.T, counter string) int {
	t.Helper()
	data, err := os.ReadFile(counter)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return len(data)
}

// probeFixture writes a plist and workflow whose agent command counts its own
// invocations, and returns the workflow path and that counter's path.
func probeFixture(t *testing.T, dir, label string) (workflow, counter string) {
	t.Helper()
	counter = filepath.Join(dir, "probes")
	workflow = filepath.Join(dir, "WORKFLOW.md")
	source := filepath.Join(dir, "source")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, workflow, "---\ntracker: {kind: linear, provider: {project_slug_id: probe, api_key: dummy}, active_states: [Todo], terminal_states: [Done]}\nworkspace: {root: work, source_root: "+source+"}\ncodex: {command: "+countingAgentCommand(t, dir, counter)+"}\n---\nprompt")
	writePlist(t, dir, label, workflow, filepath.Join(dir, "logs"), "")
	return workflow, counter
}

// TestPreflightCacheKeepsAgentProbesOffRepeatedSweeps pins the whole reason the
// cache exists: a caller that sweeps on a timer must not exec the agent CLI on
// every pass, and must still notice the two files a result is derived from.
func TestPreflightCacheKeepsAgentProbesOffRepeatedSweeps(t *testing.T) {
	dir := t.TempDir()
	workflow, counter := probeFixture(t, dir, labelPrefix+".cached")
	options := Options{LaunchAgentsDir: dir, Inspector: fakeInspector{}, Preflight: &PreflightCache{}}
	sweep := func(options Options) {
		t.Helper()
		instances, err := Discover(context.Background(), options)
		if err != nil {
			t.Fatal(err)
		}
		if len(instances) != 1 || hasCode(instances[0], "preflight_agent_authentication") {
			t.Fatalf("sweep did not report an authenticated agent: %#v", instances)
		}
	}
	for range 3 {
		sweep(options)
	}
	if got := probeCount(t, counter); got != 1 {
		t.Fatalf("three unchanged sweeps spawned %d agent CLI probes, want 1", got)
	}

	// An explicit refresh probes again: logging the CLI in changes neither file.
	refresh := options
	refresh.RefreshPreflight = true
	sweep(refresh)
	if got := probeCount(t, counter); got != 2 {
		t.Fatalf("explicit refresh left the probe count at %d, want 2", got)
	}

	// A rewritten workflow is a different result, and so is a rewritten plist.
	write(t, workflow, readFile(t, workflow)+"\nmore prompt")
	sweep(options)
	writePlist(t, dir, labelPrefix+".cached", workflow, filepath.Join(dir, "logs-moved"), "")
	sweep(options)
	if got := probeCount(t, counter); got != 4 {
		t.Fatalf("probe count = %d after a changed workflow and plist, want 4", got)
	}
}

func TestDiscoverWithoutAPreflightCacheProbesEverySweep(t *testing.T) {
	dir := t.TempDir()
	_, counter := probeFixture(t, dir, labelPrefix+".uncached")
	options := Options{LaunchAgentsDir: dir, Inspector: fakeInspector{}}
	for range 2 {
		if _, err := Discover(context.Background(), options); err != nil {
			t.Fatal(err)
		}
	}
	if got := probeCount(t, counter); got != 2 {
		t.Fatalf("uncached sweeps spawned %d probes, want one each", got)
	}
}

// TestCancelledSweepSpawnsNoAgentProbe covers the context discovery threads into
// preflight: a view that has been closed cancels its sweep, and the probe that
// sweep would otherwise have blocked on is never started.
func TestCancelledSweepSpawnsNoAgentProbe(t *testing.T) {
	dir := t.TempDir()
	_, counter := probeFixture(t, dir, labelPrefix+".cancelled")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	instances, err := Discover(ctx, Options{LaunchAgentsDir: dir, Inspector: fakeInspector{}})
	if err != nil {
		t.Fatal(err)
	}
	if got := probeCount(t, counter); got != 0 {
		t.Fatalf("cancelled sweep spawned %d agent CLI probes", got)
	}
	if !hasCode(instances[0], "preflight_agent_authentication") {
		t.Fatalf("cancelled sweep reported an authentication result anyway: %#v", instances[0].Findings)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// ownerOnlyDir creates and returns a subdirectory of dir with mode 0700.
// t.TempDir() itself is not owner-only: its numbered leaf is created with
// os.Mkdir(dir, 0o777), which a typical umask reduces to 0755, not the
// owner-only mode status.Publisher requires of a status file's parent
// directory (see preflight's status_file check).
func ownerOnlyDir(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func fixtureWorkflow(t *testing.T, dir, name string) string {
	t.Helper()
	workflow := filepath.Join(dir, name+".md")
	source := filepath.Join(dir, name+"-source")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, workflow, "---\ntracker: {kind: linear, provider: {project_slug_id: project-"+name+", api_key: dummy}, active_states: [Todo], terminal_states: [Done]}\nworkspace: {root: "+name+"-work, source_root: "+source+"}\ncodex: {command: "+fakeAuthenticatedAgentCommand(t, dir)+"}\n---\nprompt")
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
