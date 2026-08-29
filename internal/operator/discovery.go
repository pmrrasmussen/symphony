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
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"howett.net/plist"

	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/preflight"
	"github.com/pmrrasmussen/symphony/internal/status"
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
	ClaudeCommand        string         `json:"claude_command,omitempty"`
	ClaudeModel          string         `json:"claude_model,omitempty"`
	TurnTimeout          time.Duration  `json:"turn_timeout"`
	ReadTimeout          time.Duration  `json:"read_timeout,omitempty"`
	StartTimeout         time.Duration  `json:"start_timeout,omitempty"`
	StallTimeout         time.Duration  `json:"stall_timeout"`
	GitHubOwner          string         `json:"github_owner,omitempty"`
	GitHubRepository     string         `json:"github_repository,omitempty"`
	GitHubBaseBranch     string         `json:"github_base_branch,omitempty"`
	GitHubMergeMethod    string         `json:"github_merge_method,omitempty"`
	GitHubRequiredChecks []string       `json:"github_required_checks,omitempty"`
	Credentials          Credentials    `json:"credentials"`
}

// Snapshot is the runtime status snapshot as published by status.Publisher,
// plus UpdatedAt, the one field discovery genuinely adds for itself: the
// freshness timestamp used by finalizeLiveness and by readers of a snapshot
// written before status.Snapshot carried GeneratedAt.
type Snapshot struct {
	status.Snapshot
	UpdatedAt time.Time `json:"-"`
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
	// Preflight reuses each instance's validation result across sweeps. A nil
	// cache validates on every sweep, which is what a one-shot caller wants; a
	// repeating caller passes one so a sweep that changed nothing spawns no
	// agent CLI.
	Preflight *PreflightCache
	// RefreshPreflight validates again even when the cache holds a current
	// result. It is the operator's explicit refresh: logging an agent CLI in
	// changes neither the plist nor the workflow, so nothing else would clear a
	// stale authentication finding.
	RefreshPreflight bool
}

// PreflightCache holds one preflight result per LaunchAgent, keyed by the
// modification time and size of the two files that result is derived from: the
// plist, which supplies the paths and environment, and the workflow it names.
// The preflight is the one part of discovery that is not a read of local state
// -- it execs the configured agent CLI to ask whether it holds a stored login,
// which on macOS can block for seconds behind a keychain prompt -- so a
// five-second refresh must not repeat it. A zero value is ready to use, and a
// nil *PreflightCache is a cache that never hits.
//
// See docs/macos-services.md's "Status, logs, and the TUI" for what the timed
// pass and the explicit refresh each cost an operator.
type PreflightCache struct {
	mu      sync.Mutex
	entries map[string]preflightEntry
}

type preflightEntry struct {
	stamp  preflightStamp
	result preflight.Result
}

type preflightStamp struct {
	plist    fileStamp
	workflow fileStamp
}

// fileStamp is comparable, so two stamps are equal only when both files are in
// the state they were validated in. A file that cannot be stat'ed stamps as
// absent rather than as an error, which keeps a missing workflow cacheable.
type fileStamp struct {
	present bool
	modTime time.Time
	size    int64
}

func stampFile(path string) fileStamp {
	info, err := os.Stat(path)
	if err != nil {
		return fileStamp{}
	}
	return fileStamp{present: true, modTime: info.ModTime(), size: info.Size()}
}

// result returns the cached validation for these paths, or runs and stores one.
// run is deliberately called outside the lock: an overlapping sweep would
// otherwise wait out a probe that is already blocked on a keychain prompt,
// which is the hang this cache exists to keep off the refresh path. The cost of
// that choice is at most one duplicated probe.
func (c *PreflightCache) result(paths Paths, refresh bool, run func() preflight.Result) preflight.Result {
	if c == nil {
		return run()
	}
	stamp := preflightStamp{plist: stampFile(paths.Plist), workflow: stampFile(paths.Workflow)}
	if !refresh {
		c.mu.Lock()
		entry, ok := c.entries[paths.Plist]
		c.mu.Unlock()
		if ok && entry.stamp == stamp {
			return entry.result
		}
	}
	result := run()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[string]preflightEntry)
	}
	c.entries[paths.Plist] = preflightEntry{stamp: stamp, result: result}
	return result
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
	values, err := parsePlist(data)
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
	secretValues := inspectWorkflow(ctx, &instance, stringMap(values["EnvironmentVariables"]), options)
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

func inspectWorkflow(ctx context.Context, instance *Instance, environment map[string]string, options Options) []string {
	workflow, err := config.LoadWithEnvironment(instance.Paths.Workflow, instance.Paths.LogsRoot, environment)
	if err != nil {
		add(instance, "workflow_invalid", SeverityError, err.Error())
		return nil
	}
	settings := workflow.Config
	launch := settings.AgentLaunch()
	instance.Config = &EffectiveConfig{
		TrackerKind: settings.Tracker.Kind, ProjectSelector: providerString(settings.Tracker.Provider, "project_slug_id"),
		ActiveStates: append([]string(nil), settings.Tracker.ActiveStates...), HandoffState: settings.Tracker.HandoffState,
		MergeState: settings.GitHub.MergeState, TerminalStates: append([]string(nil), settings.Tracker.TerminalStates...),
		PollInterval: settings.Polling.Interval, MaxConcurrentAgents: settings.Agent.MaxConcurrent,
		MaxConcurrentByState: copyLimits(settings.Agent.ByState), MaxTurns: settings.Agent.MaxTurns,
		WorkspaceRoot: settings.Workspace.Root, WorkspaceSource: settings.Workspace.SourceRoot,
		AgentBackend: launch.Backend, TurnTimeout: launch.TurnTimeout, StallTimeout: launch.StallTimeout,
		GitHubOwner: settings.GitHub.Owner, GitHubRepository: settings.GitHub.Repository, GitHubBaseBranch: settings.GitHub.BaseBranch,
		GitHubMergeMethod: settings.GitHub.MergeMethod, GitHubRequiredChecks: append([]string(nil), settings.GitHub.RequiredChecks...),
		Credentials: credentialPresence(workflow.Raw, instance.Paths.Workflow, environment),
	}
	switch launch.Backend {
	case config.ClaudeAgentBackend:
		instance.Config.ClaudeCommand = launch.Command
		instance.Config.ClaudeModel = launch.Model
	default:
		instance.Config.CodexCommand = launch.Command
		instance.Config.CodexApprovalPolicy = launch.ApprovalPolicy
		instance.Config.CodexThreadSandbox = launch.ThreadSandbox
		instance.Config.ReadTimeout = launch.ReadTimeout
		instance.Config.StartTimeout = launch.StartTimeout
	}
	// The caller's context, not a background one: a sweep the operator has
	// already walked away from must be able to take its probes down with it.
	validation := options.Preflight.result(instance.Paths, options.RefreshPreflight, func() preflight.Result {
		return preflight.RunWithEnvironment(ctx, instance.Paths.Workflow, instance.Paths.LogsRoot, instance.Paths.StatusFile, environment)
	})
	for _, check := range validation.Checks {
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

// readSnapshot decodes directly into status.Snapshot, so a field the writer
// adds is visible to every operator client without a second, mirrored
// declaration here. json.Unmarshal already tolerates an older or newer
// schema version -- a missing field simply decodes to its zero value and an
// added one is ignored -- so no SchemaVersion check gates the decode.
//
// UpdatedAt falls back through the legacy field names a snapshot written
// before GeneratedAt existed may still use, so an old snapshot still yields a
// usable freshness reading rather than failing to parse.
func readSnapshot(path string) (*Snapshot, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var raw struct {
		status.Snapshot
		UpdatedAt      string `json:"updated_at"`
		UpdatedAtCamel string `json:"updatedAt"`
		Timestamp      string `json:"timestamp"`
		UpdatedAtMS    int64  `json:"updated_at_ms"`
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
	return &Snapshot{Snapshot: raw.Snapshot, UpdatedAt: updated}, nil
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

// parsePlist decodes a property list's bytes with howett.net/plist, the same
// decoder on every platform and for both the XML and binary formats launchd
// uses. It never rewrites the LaunchAgent on disk.
func parsePlist(data []byte) (map[string]any, error) {
	var values map[string]any
	if _, err := plist.Unmarshal(data, &values); err != nil {
		return nil, fmt.Errorf("parse plist: %w", err)
	}
	if values == nil {
		return nil, errors.New("plist root is not a dict")
	}
	return values, nil
}
