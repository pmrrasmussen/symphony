// Package operator discovers locally configured Symphony daemons without
// changing their launchd, repository, or runtime state.
package operator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/preflight"
)

const (
	// LabelPrefix is the stable LaunchAgent namespace shared by discovery and
	// service installation. A label under this prefix alone does not make a
	// plist managed; installers also require the explicit managed marker.
	LabelPrefix       = "com.pmrrasmussen.symphony"
	defaultStatusAge  = 2 * time.Minute
	defaultLogEntries = 20
	maxRecentLogBytes = 64 * 1024
)

const labelPrefix = LabelPrefix

// Liveness is a conservative, read-only assessment. A snapshot alone never
// makes an instance running: launchd must report a live process as well.
type Liveness string

const (
	LivenessRunning Liveness = "running"
	LivenessStopped Liveness = "stopped"
	LivenessStale   Liveness = "stale_snapshot"
	LivenessInvalid Liveness = "invalid"
)

type FindingSeverity string

const (
	SeverityWarning FindingSeverity = "warning"
	SeverityError   FindingSeverity = "error"
)

// Finding is structured operator data; callers do not need to parse console
// output from the config loader or preflight command.
type Finding struct {
	Code     string          `json:"code"`
	Severity FindingSeverity `json:"severity"`
	Message  string          `json:"message"`
}

// Paths is the subset of a LaunchAgent's paths useful to an operator.
type Paths struct {
	Plist            string `json:"plist"`
	Executable       string `json:"executable,omitempty"`
	WorkingDirectory string `json:"working_directory,omitempty"`
	Workflow         string `json:"workflow,omitempty"`
	LogsRoot         string `json:"logs_root,omitempty"`
	StatusFile       string `json:"status_file,omitempty"`
	StandardOut      string `json:"standard_out,omitempty"`
	StandardError    string `json:"standard_error,omitempty"`
}

// CredentialPresence contains only references, never a resolved credential or
// literal secret. File paths may be literal paths from a WORKFLOW.md; an
// environment-backed file is represented by its environment variable name.
type CredentialPresence struct {
	Configured       bool     `json:"configured"`
	EnvironmentNames []string `json:"environment_names,omitempty"`
	FileReferences   []string `json:"file_references,omitempty"`
}

type Credentials struct {
	Tracker CredentialPresence `json:"tracker"`
	GitHub  CredentialPresence `json:"github"`
}

// EffectiveConfig is a display-safe projection of config.Settings. AgentBackend
// reports the resolved agent runtime selection, so a workflow that omits
// agent.backend still projects the default rather than an empty value.
type EffectiveConfig struct {
	TrackerKind          string         `json:"tracker_kind,omitempty"`
	ProjectSelector      string         `json:"project_selector,omitempty"`
	ActiveStates         []string       `json:"active_states,omitempty"`
	HandoffState         string         `json:"handoff_state,omitempty"`
	MergeState           string         `json:"merge_state,omitempty"`
	TerminalStates       []string       `json:"terminal_states,omitempty"`
	PollInterval         time.Duration  `json:"poll_interval"`
	MaxConcurrentAgents  int            `json:"max_concurrent_agents"`
	MaxConcurrentByState map[string]int `json:"max_concurrent_agents_by_state,omitempty"`
	MaxTurns             int            `json:"max_turns"`
	WorkspaceRoot        string         `json:"workspace_root,omitempty"`
	WorkspaceSource      string         `json:"workspace_source,omitempty"`
	AgentBackend         string         `json:"agent_backend,omitempty"`
	CodexCommand         string         `json:"codex_command,omitempty"`
	CodexApprovalPolicy  string         `json:"codex_approval_policy,omitempty"`
	CodexThreadSandbox   string         `json:"codex_thread_sandbox,omitempty"`
	TurnTimeout          time.Duration  `json:"turn_timeout"`
	ReadTimeout          time.Duration  `json:"read_timeout"`
	StartTimeout         time.Duration  `json:"start_timeout"`
	StallTimeout         time.Duration  `json:"stall_timeout"`
	GitHubOwner          string         `json:"github_owner,omitempty"`
	GitHubRepository     string         `json:"github_repository,omitempty"`
	GitHubBaseBranch     string         `json:"github_base_branch,omitempty"`
	GitHubMergeMethod    string         `json:"github_merge_method,omitempty"`
	GitHubRequiredChecks []string       `json:"github_required_checks,omitempty"`
	Credentials          Credentials    `json:"credentials"`
}

// Snapshot is the display-safe subset of status.Snapshot. It deliberately
// mirrors only the status-file contract, rather than exposing coordinator
// internals or arbitrary status-file fields to operator clients.
type Snapshot struct {
	SchemaVersion int             `json:"schema_version,omitempty"`
	PID           int             `json:"pid,omitempty"`
	StartedAt     time.Time       `json:"process_started_at,omitempty"`
	GeneratedAt   time.Time       `json:"generated_at,omitempty"`
	State         string          `json:"state,omitempty"`
	Coordinator   RuntimeSnapshot `json:"coordinator"`

	// UpdatedAt is retained for discovery's freshness calculation and older
	// status snapshots. It is not a separate field in the current contract.
	UpdatedAt time.Time `json:"-"`
}

// RuntimeSnapshot contains only fixed operational metadata made public by
// the runtime status contract.
type RuntimeSnapshot struct {
	Claimed  int               `json:"claimed"`
	Running  []RunningSnapshot `json:"running"`
	Retrying []RetrySnapshot   `json:"retrying"`
	Waiting  []WaitingSnapshot `json:"waiting"`
	Stopping bool              `json:"stopping"`
}

type RunningSnapshot struct {
	IssueIdentifier      string                `json:"issue_identifier"`
	IssueState           string                `json:"issue_state"`
	Attempt              int                   `json:"attempt"`
	TurnCount            int                   `json:"turn_count"`
	StartedAt            time.Time             `json:"started_at"`
	LastActivityAt       time.Time             `json:"last_activity_at"`
	Usage                Usage                 `json:"usage"`
	RateLimit            map[string]int64      `json:"rate_limit,omitempty"`
	OutstandingOperation *OutstandingOperation `json:"outstanding_operation,omitempty"`
}

type Usage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	TotalTokens  int64 `json:"total_tokens"`
}

type OutstandingOperation struct {
	Type      string    `json:"type"`
	Name      string    `json:"name,omitempty"`
	StartedAt time.Time `json:"started_at"`
	AgeMS     int64     `json:"age_ms"`
}

type RetrySnapshot struct {
	IssueIdentifier string    `json:"issue_identifier"`
	Attempt         int       `json:"attempt"`
	Kind            string    `json:"kind"`
	Reason          string    `json:"reason"`
	Due             time.Time `json:"due_at"`
}

// WaitingSnapshot is an eligible issue that has reserved neither a slot nor a
// retry timer, mirroring coordinator.WaitingSnapshot.
type WaitingSnapshot struct {
	IssueIdentifier string    `json:"issue_identifier"`
	IssueState      string    `json:"issue_state"`
	Since           time.Time `json:"since"`
	WaitingMS       int64     `json:"waiting_ms"`
}

// LogEvent exposes the fixed structured-log envelope, not arbitrary log
// attributes which could include sensitive user-provided text.
type LogEvent struct {
	Time    time.Time `json:"time"`
	Level   string    `json:"level,omitempty"`
	Message string    `json:"message,omitempty"`
}

// LaunchdStatus is the read-only launchd/process observation.
type LaunchdStatus struct {
	Loaded  bool `json:"loaded"`
	PID     int  `json:"pid,omitempty"`
	Process bool `json:"process"`
}

// Instance is the normalized model consumed by an operator UI.
type Instance struct {
	ID        string           `json:"id"`
	Managed   bool             `json:"managed"`
	Paths     Paths            `json:"paths"`
	Config    *EffectiveConfig `json:"config,omitempty"`
	Launchd   LaunchdStatus    `json:"launchd"`
	Snapshot  *Snapshot        `json:"snapshot,omitempty"`
	RecentLog []LogEvent       `json:"recent_log,omitempty"`
	Liveness  Liveness         `json:"liveness"`
	Findings  []Finding        `json:"findings,omitempty"`
}

// Inspector isolates the only platform-specific observations. Its production
// implementation invokes read-only launchctl print and signal(0).
type Inspector interface {
	Launchd(context.Context, string) LaunchdStatus
}

type systemInspector struct{}

func (systemInspector) Launchd(ctx context.Context, label string) LaunchdStatus {
	uid := strconv.Itoa(os.Getuid())
	out, err := exec.CommandContext(ctx, "launchctl", "print", "gui/"+uid+"/"+label).Output()
	if err != nil {
		return LaunchdStatus{}
	}
	status := LaunchdStatus{Loaded: true}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "pid =") {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "pid =")))
		if err != nil || pid <= 0 {
			continue
		}
		status.PID = pid
		process, err := os.FindProcess(pid)
		if err == nil && process.Signal(syscall.Signal(0)) == nil {
			status.Process = true
		}
		break
	}
	return status
}

// Options makes discovery deterministic and testable. Empty fields use the
// current user's LaunchAgents directory and the system inspector.
type Options struct {
	LaunchAgentsDir string
	Now             func() time.Time
	Inspector       Inspector
	SnapshotMaxAge  time.Duration
	RecentLogLimit  int
}

// Discover inspects only convention-matching LaunchAgent files. It neither
// contacts tracker/GitHub services nor mutates launchd, the workflow, or disk.
func Discover(ctx context.Context, options Options) ([]Instance, error) {
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Inspector == nil {
		options.Inspector = systemInspector{}
	}
	if options.SnapshotMaxAge <= 0 {
		options.SnapshotMaxAge = defaultStatusAge
	}
	if options.RecentLogLimit <= 0 {
		options.RecentLogLimit = defaultLogEntries
	}
	dir := options.LaunchAgentsDir
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("resolve home directory: %w", err)
		}
		dir = filepath.Join(home, "Library", "LaunchAgents")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read LaunchAgents directory: %w", err)
	}
	instances := make([]Instance, 0)
	for _, entry := range entries {
		if entry.IsDir() || !matchesName(entry.Name()) {
			continue
		}
		instance := inspectCandidate(ctx, filepath.Join(dir, entry.Name()), options)
		instances = append(instances, instance)
	}
	sort.Slice(instances, func(i, j int) bool { return instances[i].ID < instances[j].ID })
	applyConflicts(instances)
	for i := range instances {
		finalizeLiveness(&instances[i], options.Now(), options.SnapshotMaxAge)
	}
	return instances, nil
}

func matchesName(name string) bool {
	if !strings.HasSuffix(name, ".plist") {
		return false
	}
	base := strings.TrimSuffix(name, ".plist")
	return base == labelPrefix || strings.HasPrefix(base, labelPrefix+".")
}

func inspectCandidate(ctx context.Context, plistPath string, options Options) Instance {
	instance := Instance{Paths: Paths{Plist: plistPath}, Liveness: LivenessInvalid}
	data, err := os.ReadFile(plistPath)
	if err != nil {
		add(&instance, "plist_unreadable", SeverityError, err.Error())
		instance.ID = strings.TrimSuffix(filepath.Base(plistPath), ".plist")
		return instance
	}
	values, err := parsePlist(ctx, plistPath, data)
	if err != nil {
		add(&instance, "plist_invalid", SeverityError, err.Error())
		instance.ID = strings.TrimSuffix(filepath.Base(plistPath), ".plist")
		return instance
	}
	label, _ := values["Label"].(string)
	instance.ID = strings.TrimSpace(label)
	instance.Managed, _ = values["SymphonyManaged"].(bool)
	if instance.ID == "" {
		instance.ID = strings.TrimSuffix(filepath.Base(plistPath), ".plist")
		add(&instance, "plist_missing_label", SeverityError, "LaunchAgent Label is required")
	} else if instance.ID != strings.TrimSuffix(filepath.Base(plistPath), ".plist") || !matchesLabel(instance.ID) {
		add(&instance, "plist_label_mismatch", SeverityError, "LaunchAgent Label must match the Symphony plist convention")
	}
	populatePaths(&instance, values, filepath.Dir(plistPath))
	inspectServicePaths(&instance)
	if instance.ID != "" {
		instance.Launchd = options.Inspector.Launchd(ctx, instance.ID)
	}
	secretValues := inspectWorkflow(&instance, stringMap(values["EnvironmentVariables"]))
	inspectRuntimeFiles(&instance, options.RecentLogLimit, secretValues)
	return instance
}

func inspectServicePaths(instance *Instance) {
	if instance.Paths.Executable == "" {
		add(instance, "plist_missing_program", SeverityError, "LaunchAgent Program or ProgramArguments is required")
	} else if info, err := os.Stat(instance.Paths.Executable); err != nil {
		add(instance, "executable_unavailable", SeverityError, err.Error())
	} else if info.IsDir() || info.Mode()&0o111 == 0 {
		add(instance, "executable_invalid", SeverityError, "LaunchAgent executable is not executable")
	}
	if instance.Paths.WorkingDirectory != "" {
		if info, err := os.Stat(instance.Paths.WorkingDirectory); err != nil {
			add(instance, "working_directory_unavailable", SeverityError, err.Error())
		} else if !info.IsDir() {
			add(instance, "working_directory_invalid", SeverityError, "WorkingDirectory is not a directory")
		}
	}
}

func matchesLabel(label string) bool {
	return label == labelPrefix || strings.HasPrefix(label, labelPrefix+".")
}

func populatePaths(instance *Instance, values map[string]any, base string) {
	resolve := func(value string) string {
		if value == "" || filepath.IsAbs(value) {
			return value
		}
		if instance.Paths.WorkingDirectory != "" {
			return filepath.Clean(filepath.Join(instance.Paths.WorkingDirectory, value))
		}
		return filepath.Clean(filepath.Join(base, value))
	}
	if working, ok := values["WorkingDirectory"].(string); ok {
		instance.Paths.WorkingDirectory = resolve(working)
	}
	if program, ok := values["Program"].(string); ok {
		instance.Paths.Executable = resolve(program)
	}
	arguments := stringSlice(values["ProgramArguments"])
	if instance.Paths.Executable == "" && len(arguments) > 0 {
		instance.Paths.Executable = resolve(arguments[0])
	}
	for i := 0; i < len(arguments); i++ {
		name, value, assigned := splitFlag(arguments[i])
		isKnownFlag := name == "--workflow" || name == "--logs-root" || name == "--status-file"
		consumeNext := isKnownFlag && !assigned && i+1 < len(arguments)
		if consumeNext {
			value, assigned = arguments[i+1], true
		}
		if !assigned {
			continue
		}
		switch name {
		case "--workflow":
			instance.Paths.Workflow = resolve(value)
		case "--logs-root":
			instance.Paths.LogsRoot = resolve(value)
		case "--status-file":
			instance.Paths.StatusFile = resolve(value)
		}
		if consumeNext {
			i++
		}
	}
	for key, destination := range map[string]*string{
		"StandardOutPath":   &instance.Paths.StandardOut,
		"StandardErrorPath": &instance.Paths.StandardError,
	} {
		if value, ok := values[key].(string); ok {
			*destination = resolve(value)
		}
	}
	// These match the CLI defaults and let operators inspect older, compact
	// LaunchAgents which rely on them instead of spelling the flags out.
	defaultBase := instance.Paths.WorkingDirectory
	if defaultBase == "" {
		defaultBase = base
	}
	if instance.Paths.Workflow == "" {
		instance.Paths.Workflow = filepath.Join(defaultBase, "WORKFLOW.md")
	}
	if instance.Paths.LogsRoot == "" {
		instance.Paths.LogsRoot = filepath.Join(defaultBase, ".symphony", "logs")
	}
}

func stringSlice(value any) []string {
	items, ok := value.([]any)
	if !ok {
		if strings, ok := value.([]string); ok {
			return strings
		}
		return nil
	}
	result := make([]string, 0, len(items))
	for _, item := range items {
		text, ok := item.(string)
		if !ok {
			return nil
		}
		result = append(result, text)
	}
	return result
}

func splitFlag(flag string) (name, value string, found bool) {
	return strings.Cut(flag, "=")
}

func inspectWorkflow(instance *Instance, environment map[string]string) []string {
	workflow, err := config.LoadWithEnvironment(instance.Paths.Workflow, instance.Paths.LogsRoot, environment)
	if err != nil {
		add(instance, "workflow_invalid", SeverityError, err.Error())
		return nil
	}
	settings := workflow.Config
	instance.Config = &EffectiveConfig{
		TrackerKind: settings.Tracker.Kind, ProjectSelector: providerString(settings.Tracker.Provider, "project_slug_id"),
		ActiveStates: append([]string(nil), settings.Tracker.ActiveStates...), HandoffState: settings.Tracker.HandoffState,
		MergeState: settings.GitHub.MergeState, TerminalStates: append([]string(nil), settings.Tracker.TerminalStates...),
		PollInterval: settings.Polling.Interval, MaxConcurrentAgents: settings.Agent.MaxConcurrent,
		MaxConcurrentByState: copyLimits(settings.Agent.ByState), MaxTurns: settings.Agent.MaxTurns,
		WorkspaceRoot: settings.Workspace.Root, WorkspaceSource: settings.Workspace.SourceRoot,
		AgentBackend: settings.AgentLaunch().Backend,
		CodexCommand: settings.Codex.Command, TurnTimeout: settings.Codex.TurnTimeout, ReadTimeout: settings.Codex.ReadTimeout,
		CodexApprovalPolicy: settings.Codex.ApprovalPolicy, CodexThreadSandbox: settings.Codex.ThreadSandbox,
		StartTimeout: settings.Codex.StartTimeout, StallTimeout: settings.Codex.StallTimeout,
		GitHubOwner: settings.GitHub.Owner, GitHubRepository: settings.GitHub.Repository, GitHubBaseBranch: settings.GitHub.BaseBranch,
		GitHubMergeMethod: settings.GitHub.MergeMethod, GitHubRequiredChecks: append([]string(nil), settings.GitHub.RequiredChecks...),
		Credentials: credentialPresence(workflow.Raw, instance.Paths.Workflow, environment),
	}
	for _, check := range preflight.RunWithEnvironment(context.Background(), instance.Paths.Workflow, instance.Paths.LogsRoot, instance.Paths.StatusFile, environment).Checks {
		if check.Status == preflight.StatusPassed {
			continue
		}
		severity := SeverityWarning
		if check.Status == preflight.StatusFailed {
			severity = SeverityError
		}
		add(instance, "preflight_"+check.Name, severity, check.Message)
	}
	return secretValues(settings)
}

func secretValues(settings config.Settings) []string {
	values := append([]string(nil), settings.HostSecretValues...)
	if value := providerString(settings.Tracker.Provider, "api_key"); value != "" {
		values = append(values, value)
	}
	if settings.GitHub.Token != "" {
		values = append(values, settings.GitHub.Token)
	}
	return values
}

func providerString(provider map[string]any, key string) string {
	value, _ := provider[key].(string)
	return value
}

func copyLimits(value map[string]int) map[string]int {
	if len(value) == 0 {
		return nil
	}
	copy := make(map[string]int, len(value))
	for key, limit := range value {
		copy[key] = limit
	}
	return copy
}

func stringMap(value any) map[string]string {
	items, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	result := make(map[string]string, len(items))
	for name, item := range items {
		if text, ok := item.(string); ok {
			result[name] = text
		}
	}
	return result
}

func credentialPresence(raw map[string]any, workflowPath string, environment map[string]string) Credentials {
	tracker, _ := raw["tracker"].(map[string]any)
	provider, _ := tracker["provider"].(map[string]any)
	github, _ := raw["github"].(map[string]any)
	return Credentials{Tracker: credentialFrom(provider, "api_key", "api_key_file", filepath.Dir(workflowPath), environment), GitHub: credentialFrom(github, "token", "token_file", filepath.Dir(workflowPath), environment)}
}

func credentialFrom(values map[string]any, valueKey, fileKey, base string, environment map[string]string) CredentialPresence {
	result := CredentialPresence{}
	for _, key := range []string{valueKey, fileKey} {
		value, ok := values[key].(string)
		if !ok || strings.TrimSpace(value) == "" {
			continue
		}
		result.Configured = true
		if strings.HasPrefix(value, "$") {
			name := strings.TrimPrefix(value, "$")
			if config.ValidEnvironmentName(name) {
				result.EnvironmentNames = append(result.EnvironmentNames, name)
				if key == fileKey && strings.TrimSpace(environment[name]) != "" {
					result.FileReferences = append(result.FileReferences, filepath.Clean(environment[name]))
				}
			}
		} else if key == fileKey {
			if !filepath.IsAbs(value) {
				value = filepath.Join(base, value)
			}
			result.FileReferences = append(result.FileReferences, filepath.Clean(value))
		}
	}
	sort.Strings(result.EnvironmentNames)
	sort.Strings(result.FileReferences)
	return result
}

func inspectRuntimeFiles(instance *Instance, logLimit int, secretValues []string) {
	if instance.Paths.StatusFile != "" {
		snapshot, err := readSnapshot(instance.Paths.StatusFile)
		if err != nil {
			severity := SeverityWarning
			code := "status_unavailable"
			if !os.IsNotExist(err) {
				severity, code = SeverityError, "status_invalid"
			}
			add(instance, code, severity, err.Error())
		} else {
			instance.Snapshot = snapshot
		}
	}
	if instance.Paths.LogsRoot != "" {
		events, err := recentLog(filepath.Join(instance.Paths.LogsRoot, "symphony.jsonl"), logLimit, secretValues)
		if err != nil {
			if os.IsNotExist(err) {
				add(instance, "log_unavailable", SeverityWarning, err.Error())
			} else {
				add(instance, "log_unreadable", SeverityError, err.Error())
			}
		} else {
			instance.RecentLog = events
		}
	}
}

func readSnapshot(path string) (*Snapshot, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var raw struct {
		SchemaVersion  int             `json:"schema_version"`
		PID            int             `json:"pid"`
		StartedAt      time.Time       `json:"process_started_at"`
		GeneratedAt    time.Time       `json:"generated_at"`
		State          string          `json:"state"`
		Coordinator    RuntimeSnapshot `json:"coordinator"`
		UpdatedAt      string          `json:"updated_at"`
		UpdatedAtCamel string          `json:"updatedAt"`
		Timestamp      string          `json:"timestamp"`
		UpdatedAtMS    int64           `json:"updated_at_ms"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse status snapshot: %w", err)
	}
	updated := raw.GeneratedAt
	text := raw.UpdatedAt
	if text == "" {
		text = raw.UpdatedAtCamel
	}
	if text == "" {
		text = raw.Timestamp
	}
	if updated.IsZero() && text != "" {
		var err error
		updated, err = time.Parse(time.RFC3339, text)
		if err != nil {
			return nil, fmt.Errorf("parse snapshot timestamp: %w", err)
		}
	} else if updated.IsZero() && raw.UpdatedAtMS > 0 {
		updated = time.UnixMilli(raw.UpdatedAtMS)
	} else if updated.IsZero() {
		return nil, errors.New("status snapshot has no timestamp")
	}
	return &Snapshot{SchemaVersion: raw.SchemaVersion, PID: raw.PID, StartedAt: raw.StartedAt, GeneratedAt: raw.GeneratedAt, State: raw.State, Coordinator: raw.Coordinator, UpdatedAt: updated}, nil
}

func recentLog(path string, limit int, secretValues []string) ([]LogEvent, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	start := info.Size() - maxRecentLogBytes
	if start > 0 {
		if _, err := file.Seek(start, io.SeekStart); err != nil {
			return nil, err
		}
	}
	data, err := io.ReadAll(io.LimitReader(file, maxRecentLogBytes))
	if err != nil {
		return nil, err
	}
	lines := bytes.Split(data, []byte{'\n'})
	events := make([]LogEvent, 0, limit)
	for i := len(lines) - 1; i >= 0 && len(events) < limit; i-- {
		var row struct {
			Time    time.Time `json:"time"`
			Level   string    `json:"level"`
			Message string    `json:"msg"`
		}
		if json.Unmarshal(lines[i], &row) == nil && !row.Time.IsZero() {
			events = append(events, LogEvent{Time: row.Time, Level: row.Level, Message: redact(row.Message, secretValues)})
		}
	}
	for left, right := 0, len(events)-1; left < right; left, right = left+1, right-1 {
		events[left], events[right] = events[right], events[left]
	}
	return events, nil
}

func redact(value string, secretValues []string) string {
	for _, secret := range secretValues {
		secret = strings.TrimSpace(secret)
		if secret != "" {
			value = strings.ReplaceAll(value, secret, "[REDACTED]")
		}
	}
	return value
}

func applyConflicts(instances []Instance) {
	for i := range instances {
		for j := i + 1; j < len(instances); j++ {
			left, right := &instances[i], &instances[j]
			conflict(left, right, "workflow", left.Paths.Workflow, right.Paths.Workflow)
			conflict(left, right, "status_file", left.Paths.StatusFile, right.Paths.StatusFile)
			conflict(left, right, "log_root", left.Paths.LogsRoot, right.Paths.LogsRoot)
			if left.Config != nil && right.Config != nil {
				conflict(left, right, "workspace_root", left.Config.WorkspaceRoot, right.Config.WorkspaceRoot)
				conflict(left, right, "workspace_source", left.Config.WorkspaceSource, right.Config.WorkspaceSource)
			}
		}
	}
}

func conflict(left, right *Instance, kind, first, second string) {
	if first == "" || second == "" || filepath.Clean(first) != filepath.Clean(second) {
		return
	}
	message := fmt.Sprintf("conflicts with %s: shared %s %s", right.ID, strings.ReplaceAll(kind, "_", " "), first)
	add(left, "duplicate_"+kind, SeverityError, message)
	add(right, "duplicate_"+kind, SeverityError, fmt.Sprintf("conflicts with %s: shared %s %s", left.ID, strings.ReplaceAll(kind, "_", " "), second))
}

func finalizeLiveness(instance *Instance, now time.Time, maxAge time.Duration) {
	if hasError(instance.Findings) {
		instance.Liveness = LivenessInvalid
		return
	}
	if !instance.Launchd.Loaded || !instance.Launchd.Process {
		instance.Liveness = LivenessStopped
		return
	}
	if instance.Snapshot != nil && now.Sub(instance.Snapshot.UpdatedAt) > maxAge {
		instance.Liveness = LivenessStale
		return
	}
	instance.Liveness = LivenessRunning
}

func hasError(findings []Finding) bool {
	for _, finding := range findings {
		if finding.Severity == SeverityError {
			return true
		}
	}
	return false
}
func add(instance *Instance, code string, severity FindingSeverity, message string) {
	instance.Findings = append(instance.Findings, Finding{Code: code, Severity: severity, Message: message})
}

// parsePlist relies on macOS's plist decoder. It writes converted JSON only to
// stdout, so discovery never rewrites a LaunchAgent and can inspect XML and
// binary plists alike.
func parsePlist(ctx context.Context, path string, fallback []byte) (map[string]any, error) {
	if runtime.GOOS == "darwin" {
		output, err := exec.CommandContext(ctx, "plutil", "-convert", "json", "-o", "-", path).CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("parse plist: %w: %s", err, strings.TrimSpace(string(output)))
		}
		var values map[string]any
		if err := json.Unmarshal(output, &values); err != nil {
			return nil, fmt.Errorf("parse plist JSON: %w", err)
		}
		if values == nil {
			return nil, errors.New("plist root is not a dict")
		}
		return values, nil
	}
	return parseXMLPlist(fallback)
}

type xmlPlistNode struct {
	name     string
	text     strings.Builder
	children []*xmlPlistNode
}

// parseXMLPlist is intentionally a small fallback for XML property lists when
// discovery is tested or used off macOS. On macOS plutil above handles the
// complete XML and binary plist formats used by launchd.
func parseXMLPlist(data []byte) (map[string]any, error) {
	root := &xmlPlistNode{}
	stack := []*xmlPlistNode{root}
	for cursor := 0; cursor < len(data); {
		open := bytes.IndexByte(data[cursor:], '<')
		if open < 0 {
			stack[len(stack)-1].text.WriteString(xmlUnescape(string(data[cursor:])))
			break
		}
		open += cursor
		if open > cursor {
			stack[len(stack)-1].text.WriteString(xmlUnescape(string(data[cursor:open])))
		}
		close := bytes.IndexByte(data[open:], '>')
		if close < 0 {
			return nil, errors.New("parse plist XML: unterminated tag")
		}
		close += open
		tag := strings.TrimSpace(string(data[open+1 : close]))
		cursor = close + 1
		if strings.HasPrefix(tag, "?") || strings.HasPrefix(tag, "!") {
			continue
		}
		if strings.HasPrefix(tag, "/") {
			name := strings.TrimSpace(strings.TrimPrefix(tag, "/"))
			if len(stack) == 1 || stack[len(stack)-1].name != name {
				return nil, errors.New("parse plist XML: mismatched closing tag")
			}
			stack = stack[:len(stack)-1]
			continue
		}
		selfClosing := strings.HasSuffix(tag, "/")
		name := strings.Fields(strings.TrimSuffix(tag, "/"))
		if len(name) == 0 {
			return nil, errors.New("parse plist XML: empty tag")
		}
		node := &xmlPlistNode{name: name[0]}
		parent := stack[len(stack)-1]
		parent.children = append(parent.children, node)
		if !selfClosing {
			stack = append(stack, node)
		}
	}
	if len(stack) != 1 || len(root.children) != 1 || root.children[0].name != "plist" {
		return nil, errors.New("plist root must contain one plist element")
	}
	plist := root.children[0]
	if len(plist.children) != 1 || plist.children[0].name != "dict" {
		return nil, errors.New("plist root is not a dict")
	}
	value, err := xmlPlistValue(plist.children[0])
	if err != nil {
		return nil, err
	}
	values, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("plist root is not a dict")
	}
	return values, nil
}

func xmlPlistValue(node *xmlPlistNode) (any, error) {
	switch node.name {
	case "string":
		return strings.TrimSpace(node.text.String()), nil
	case "integer":
		value, err := strconv.ParseInt(strings.TrimSpace(node.text.String()), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parse plist integer: %w", err)
		}
		return value, nil
	case "true":
		return true, nil
	case "false":
		return false, nil
	case "array":
		values := make([]string, 0, len(node.children))
		for _, child := range node.children {
			value, err := xmlPlistValue(child)
			if err != nil {
				return nil, err
			}
			text, ok := value.(string)
			if !ok {
				return nil, errors.New("plist array must contain strings")
			}
			values = append(values, text)
		}
		return values, nil
	case "dict":
		if len(node.children)%2 != 0 {
			return nil, errors.New("plist dict has an unpaired key")
		}
		values := make(map[string]any, len(node.children)/2)
		for i := 0; i < len(node.children); i += 2 {
			if node.children[i].name != "key" {
				return nil, errors.New("plist dict key is invalid")
			}
			value, err := xmlPlistValue(node.children[i+1])
			if err != nil {
				return nil, err
			}
			values[strings.TrimSpace(node.children[i].text.String())] = value
		}
		return values, nil
	default:
		return nil, fmt.Errorf("unsupported plist value %q", node.name)
	}
}

func xmlUnescape(value string) string {
	replacer := strings.NewReplacer("&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", "\"", "&apos;", "'")
	return replacer.Replace(value)
}
