// Package service manages one explicit, repository-scoped macOS Symphony
// LaunchAgent. It deliberately has no scheduler or TUI responsibilities.
package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"unicode"

	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/operator"
	"github.com/pmrrasmussen/symphony/internal/preflight"
)

// linearKeyEnvironment and githubKeyEnvironment are the plist variables this
// LaunchAgent hands *to* the daemon, which is the opposite direction from
// config.ReservedSecretEnvNames -- the names no agent child may inherit. They
// are deliberately separate constants rather than indexes into that list, since
// each is one specific variable this installer writes and neither is a policy
// over a set. The two lists must still agree: a name the plist sets that the
// reserved list does not carry would be exported into the daemon and then
// inherited by every agent child. TestReservedNamesCoverTheServiceCredentialVariables
// is what holds them together.
const (
	linearKeyEnvironment = "SYMPHONY_LINEAR_API_KEY_FILE"
	githubKeyEnvironment = "SYMPHONY_GITHUB_TOKEN_FILE"
	basePath             = "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
)

// Options controls service installation. Empty Repository and Binary select
// the current repository and ~/.local/bin/symphony respectively.
type Options struct {
	Repository      string
	Workflow        string
	Name            string
	Binary          string
	LaunchAgentsDir string
	LinearKeyFile   string
	GitHubTokenFile string
	LogLevel        string
	Runner          Runner
}

// Runner is the small macOS boundary used for plist validation and launchctl.
// It lets the filesystem and generated plist behavior be tested on any OS.
type Runner interface {
	Run(context.Context, string, ...string) ([]byte, error)
}

type systemRunner struct{}

func (systemRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// Instance is the selected repository service with no resolved secret values.
type Instance struct {
	Label     string
	PlistPath string
	Workflow  string
}

type desired struct {
	Instance
	Repository      string
	LogsRoot        string
	StatusFile      string
	WorkspaceSource string
	Stdout          string
	Stderr          string
	Content         []byte
}

// Install validates all inputs and the generated plist before it changes an
// existing LaunchAgent. It is a no-op when the managed plist is already exact.
func Install(ctx context.Context, options Options) (Instance, bool, error) {
	d, runner, err := prepare(ctx, options, true)
	if err != nil {
		return Instance{}, false, err
	}
	if err := validatePlist(ctx, runner, d.Content); err != nil {
		return Instance{}, false, err
	}
	if err := rejectConflicts(ctx, options, d, ""); err != nil {
		return Instance{}, false, err
	}
	if err := ensureDirectories(d); err != nil {
		return Instance{}, false, err
	}
	old, err := os.ReadFile(d.PlistPath)
	if err == nil && bytes.Equal(old, d.Content) {
		return d.Instance, false, nil
	}
	if err != nil && !os.IsNotExist(err) {
		return Instance{}, false, fmt.Errorf("read existing LaunchAgent: %w", err)
	}
	// Observe the registration being replaced before it is overwritten: a
	// rollback must not start a service that was not running.
	previous := prior{Content: old, Restore: true}
	if old != nil {
		previous.Loaded = serviceLoadState(ctx, runner, d.Label) == loadLoaded
	}
	if err := atomicWrite(d.PlistPath, d.Content, 0o600); err != nil {
		return Instance{}, false, fmt.Errorf("install LaunchAgent plist: %w", err)
	}
	if err := reload(ctx, runner, d, previous); err != nil {
		return Instance{}, false, err
	}
	return d.Instance, true, nil
}

// Status finds exactly one managed service for this repository.
func Status(ctx context.Context, options Options) (operator.Instance, error) {
	d, _, err := prepare(ctx, options, false)
	if err != nil {
		return operator.Instance{}, err
	}
	return findManaged(ctx, options, d)
}

// Restart kickstarts only this repository's managed instance.
func Restart(ctx context.Context, options Options) (Instance, error) {
	d, runner, err := prepare(ctx, options, false)
	if err != nil {
		return Instance{}, err
	}
	selected, err := findManaged(ctx, options, d)
	if err != nil {
		return Instance{}, err
	}
	if err := launchctl(ctx, runner, "kickstart", "-k", launchService(selected.ID)); err != nil {
		return Instance{}, fmt.Errorf("restart %s: %w", selected.ID, err)
	}
	return Instance{Label: selected.ID, PlistPath: selected.Paths.Plist, Workflow: selected.Paths.Workflow}, nil
}

// Uninstall unloads and removes only a plist created by this package.
func Uninstall(ctx context.Context, options Options) (Instance, error) {
	d, runner, err := prepare(ctx, options, false)
	if err != nil {
		return Instance{}, err
	}
	selected, err := findManaged(ctx, options, d)
	if err != nil {
		return Instance{}, err
	}
	// A valid managed plist can be installed but currently unloaded. Removing
	// that selected registration is still an idempotent uninstall operation.
	_ = launchctl(ctx, runner, "bootout", launchService(selected.ID))
	if err := os.Remove(selected.Paths.Plist); err != nil {
		return Instance{}, fmt.Errorf("remove LaunchAgent plist: %w", err)
	}
	return Instance{Label: selected.ID, PlistPath: selected.Paths.Plist, Workflow: selected.Paths.Workflow}, nil
}

func prepare(ctx context.Context, options Options, validate bool) (desired, Runner, error) {
	if runtime.GOOS != "darwin" && options.Runner == nil {
		return desired{}, nil, errors.New("symphony service is only supported on macOS")
	}
	runner := options.Runner
	if runner == nil {
		runner = systemRunner{}
	}
	if level := strings.ToLower(strings.TrimSpace(options.LogLevel)); level != "" && level != "info" && level != "debug" {
		return desired{}, nil, fmt.Errorf("invalid --log-level %q: supported values are info, debug", options.LogLevel)
	}
	repository, err := repositoryRoot(ctx, runner, options.Repository)
	if err != nil {
		return desired{}, nil, err
	}
	workflow := options.Workflow
	if workflow == "" {
		workflow = filepath.Join(repository, "WORKFLOW.md")
	}
	workflow, err = filepath.Abs(workflow)
	if err != nil {
		return desired{}, nil, fmt.Errorf("resolve workflow path: %w", err)
	}
	if info, err := os.Stat(workflow); err != nil || info.IsDir() {
		return desired{}, nil, fmt.Errorf("WORKFLOW.md is unavailable: %s", workflow)
	}
	name, owner, repo, err := instanceName(ctx, runner, repository, workflow, options.Name)
	if err != nil {
		return desired{}, nil, err
	}
	label := operator.LabelPrefix + "." + name
	home, err := os.UserHomeDir()
	if err != nil {
		return desired{}, nil, fmt.Errorf("resolve user home: %w", err)
	}
	launchDir := options.LaunchAgentsDir
	if launchDir == "" {
		launchDir = filepath.Join(home, "Library", "LaunchAgents")
	}
	launchDir, err = filepath.Abs(launchDir)
	if err != nil {
		return desired{}, nil, fmt.Errorf("resolve LaunchAgents directory: %w", err)
	}
	binary := options.Binary
	if binary == "" {
		binary = filepath.Join(home, ".local", "bin", "symphony")
	}
	binary, err = filepath.Abs(binary)
	if err != nil {
		return desired{}, nil, fmt.Errorf("resolve Symphony binary: %w", err)
	}
	if validate {
		if info, err := os.Stat(binary); err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
			return desired{}, nil, fmt.Errorf("shared Symphony binary is not executable: %s (run ./scripts/install from the Symphony repository)", binary)
		}
	}
	logsRoot := filepath.Join(repository, ".symphony", "logs")
	statusFile := filepath.Join(repository, ".symphony", "service", "status.json")
	d := desired{Instance: Instance{Label: label, PlistPath: filepath.Join(launchDir, label+".plist"), Workflow: workflow}, Repository: repository, LogsRoot: logsRoot, StatusFile: statusFile, Stdout: filepath.Join(repository, ".symphony", "service", "stdout.log"), Stderr: filepath.Join(repository, ".symphony", "service", "stderr.log")}
	if validate {
		environment, err := credentialEnvironment(workflow, owner, repo, options, home)
		if err != nil {
			return desired{}, nil, err
		}
		result := preflight.RunWithEnvironment(ctx, workflow, logsRoot, environment)
		if !result.OK() {
			return desired{}, nil, fmt.Errorf("service preflight failed: %s", failedChecks(result))
		}
		loaded, err := config.LoadWithEnvironment(workflow, logsRoot, environment)
		if err != nil {
			return desired{}, nil, fmt.Errorf("load validated service workflow: %w", err)
		}
		d.WorkspaceSource = loaded.Config.Workspace.SourceRoot
		d.Content = renderPlist(d, binary, options.LogLevel, environment)
	}
	return d, runner, nil
}

func repositoryRoot(ctx context.Context, runner Runner, requested string) (string, error) {
	if requested == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		requested = cwd
	}
	requested, err := filepath.Abs(requested)
	if err != nil {
		return "", err
	}
	out, err := runner.Run(ctx, "git", "-C", requested, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("service install requires a Git repository: %s", requested)
	}
	root := strings.TrimSpace(string(out))
	if root == "" {
		return "", fmt.Errorf("Git did not report a repository root for %s", requested)
	}
	return filepath.Clean(root), nil
}

func instanceName(ctx context.Context, runner Runner, repository, workflow, requested string) (string, string, string, error) {
	owner, repo := originOwnerRepo(ctx, runner, repository)
	if requested == "" {
		if owner != "" && repo != "" {
			requested = owner + "-" + repo
		} else {
			requested = filepath.Base(repository)
		}
	}
	name := slug(requested)
	if name == "" {
		return "", "", "", fmt.Errorf("invalid service instance name %q; use --name with letters, numbers, and hyphens", requested)
	}
	if owner == "" || repo == "" {
		if loaded, err := config.Load(workflow, filepath.Join(repository, ".symphony", "logs")); err == nil && loaded.Config.GitHub.Enabled {
			owner, repo = loaded.Config.GitHub.Owner, loaded.Config.GitHub.Repository
		}
	}
	return name, owner, repo, nil
}

func originOwnerRepo(ctx context.Context, runner Runner, repository string) (string, string) {
	out, err := runner.Run(ctx, "git", "-C", repository, "remote", "get-url", "origin")
	if err != nil {
		return "", ""
	}
	value := strings.TrimSuffix(strings.TrimSpace(string(out)), ".git")
	value = strings.TrimPrefix(value, "git@github.com:")
	value = strings.TrimPrefix(value, "https://github.com/")
	parts := strings.Split(value, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", ""
	}
	return parts[0], parts[1]
}

func slug(value string) string {
	var result strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(value)) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			result.WriteRune(r)
			lastDash = false
		} else if !lastDash {
			result.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(result.String(), "-")
}

func credentialEnvironment(workflow, owner, repo string, options Options, home string) (map[string]string, error) {
	references, err := credentialReferences(workflow)
	if err != nil {
		return nil, err
	}
	environment := map[string]string{"PATH": basePath + ":" + filepath.Join(home, ".local", "bin")}
	linearConvention := ""
	if references.linear.environment == linearKeyEnvironment {
		linearConvention = filepath.Join(home, ".config", "symphony", "linear-api-key")
	}
	linearPath, err := selectCredential("Linear API key", references.linear, options.LinearKeyFile, linearConvention, "")
	if err != nil {
		return nil, err
	}
	if references.linear.environment != "" {
		environment[references.linear.environment] = linearPath
	}
	if references.github.configured {
		fallback := ""
		if references.github.environment == githubKeyEnvironment && owner != "" && repo != "" {
			fallback = filepath.Join(home, ".config", "symphony", "github", slug(owner)+"-"+slug(repo)+".token")
		}
		githubPath, err := selectCredential("GitHub token", references.github, options.GitHubTokenFile, fallback, filepath.Join(home, ".config", "symphony", "github.token"))
		if err != nil {
			return nil, err
		}
		if references.github.environment != "" {
			environment[references.github.environment] = githubPath
		}
	}
	return environment, nil
}

type credentialReference struct {
	configured  bool
	environment string
	path        string
}

type credentials struct{ linear, github credentialReference }

// credentialReferences reads only the credential reference syntax. It never
// reads the referenced files, which keeps errors and generated content secret-free.
func credentialReferences(workflow string) (credentials, error) {
	data, err := os.ReadFile(workflow)
	if err != nil {
		return credentials{}, err
	}
	// config.Workflow keeps this parsing authoritative and lets this narrow
	// extractor reject malformed front matter before we inspect references.
	raw, _, err := workflowRaw(data)
	if err != nil {
		return credentials{}, err
	}
	tracker, _ := raw["tracker"].(map[string]any)
	provider, _ := tracker["provider"].(map[string]any)
	github, _ := raw["github"].(map[string]any)
	base := filepath.Dir(workflow)
	linear, err := credentialFromRaw(provider, "api_key_file", "api_key", base)
	if err != nil {
		return credentials{}, fmt.Errorf("Linear credential: %w", err)
	}
	gh, err := credentialFromRaw(github, "token_file", "token", base)
	if err != nil {
		return credentials{}, fmt.Errorf("GitHub credential: %w", err)
	}
	return credentials{linear: linear, github: gh}, nil
}

// workflowRaw uses config's YAML parser via a small exported helper kept in
// config, preventing a second service-specific workflow grammar.
func workflowRaw(data []byte) (map[string]any, string, error) { return config.ParseRaw(data) }

func credentialFromRaw(values map[string]any, fileKey, inlineKey, base string) (credentialReference, error) {
	if values == nil {
		return credentialReference{}, nil
	}
	if raw, ok := values[fileKey]; ok {
		value, ok := raw.(string)
		if !ok || strings.TrimSpace(value) == "" {
			return credentialReference{}, errors.New("credential file reference must be a non-empty string")
		}
		if name, ok := environmentName(value); ok {
			return credentialReference{configured: true, environment: name}, nil
		}
		if strings.HasPrefix(value, "$") {
			return credentialReference{}, fmt.Errorf("unsupported environment reference %q", value)
		}
		if !filepath.IsAbs(value) {
			value = filepath.Join(base, value)
		}
		return credentialReference{configured: true, path: filepath.Clean(value)}, nil
	}
	if _, configured := values[inlineKey]; configured {
		return credentialReference{}, errors.New("must use a credential file reference; inline credential values cannot be installed in a LaunchAgent")
	}
	return credentialReference{}, errors.New("credential file reference is required")
}

func environmentName(value string) (string, bool) {
	if !strings.HasPrefix(value, "$") {
		return "", false
	}
	name := strings.TrimPrefix(value, "$")
	if name == "" {
		return "", false
	}
	for i, r := range name {
		if !(r == '_' || unicode.IsLetter(r) || (i > 0 && unicode.IsDigit(r))) {
			return "", false
		}
	}
	return name, true
}

func selectCredential(kind string, reference credentialReference, override, convention, legacy string) (string, error) {
	if !reference.configured {
		return "", fmt.Errorf("%s file reference is required", kind)
	}
	path := strings.TrimSpace(override)
	if path == "" && reference.path != "" {
		path = reference.path
	}
	if path == "" && reference.environment != "" {
		path = strings.TrimSpace(os.Getenv(reference.environment))
	}
	if path == "" {
		path = convention
	}
	if path == "" && legacy != "" {
		if _, err := os.Stat(legacy); err == nil {
			return "", fmt.Errorf("%s needs a repository-scoped credential file; legacy shared path exists at %s, pass an explicit --github-token-file to use it", kind, legacy)
		}
	}
	if path == "" {
		return "", fmt.Errorf("%s file is required; configure the workflow reference or provide an explicit file", kind)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve %s file: %w", kind, err)
	}
	info, err := os.Stat(abs)
	if err != nil || info.IsDir() {
		return "", fmt.Errorf("%s file is unavailable: %s", kind, abs)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("%s file must not be group- or world-readable: %s", kind, abs)
	}
	if info.Size() == 0 {
		return "", fmt.Errorf("%s file is empty: %s", kind, abs)
	}
	return abs, nil
}

func failedChecks(result preflight.Result) string {
	var messages []string
	for _, check := range result.Checks {
		if check.Status == preflight.StatusFailed {
			messages = append(messages, check.Name+": "+check.Message)
		}
	}
	return strings.Join(messages, "; ")
}

func ensureDirectories(d desired) error {
	for _, dir := range []string{filepath.Join(d.Repository, ".symphony", "workspaces"), d.LogsRoot, filepath.Dir(d.StatusFile)} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create service directory %s: %w", dir, err)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("secure service directory %s: %w", dir, err)
		}
	}
	return nil
}

func renderPlist(d desired, binary, logLevel string, environment map[string]string) []byte {
	if strings.TrimSpace(logLevel) == "" {
		logLevel = "info"
	}
	args := []string{binary, "--workflow", d.Workflow, "--logs-root", d.LogsRoot, "--status-file", d.StatusFile, "--log-level", logLevel}
	var b strings.Builder
	b.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n<plist version=\"1.0\"><dict>")
	plistString(&b, "Label", d.Label)
	b.WriteString("<key>SymphonyManaged</key><true/>")
	b.WriteString("<key>ProgramArguments</key><array>")
	for _, argument := range args {
		xmlString(&b, argument)
	}
	b.WriteString("</array>")
	plistString(&b, "WorkingDirectory", d.Repository)
	b.WriteString("<key>EnvironmentVariables</key><dict>")
	keys := make([]string, 0, len(environment))
	for key := range environment {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		plistString(&b, key, environment[key])
	}
	b.WriteString("</dict>")
	plistString(&b, "StandardOutPath", d.Stdout)
	plistString(&b, "StandardErrorPath", d.Stderr)
	b.WriteString("<key>RunAtLoad</key><true/><key>KeepAlive</key><true/><key>ThrottleInterval</key><integer>10</integer></dict></plist>\n")
	return []byte(b.String())
}

func plistString(b *strings.Builder, key, value string) {
	b.WriteString("<key>")
	xmlText(b, key)
	b.WriteString("</key>")
	xmlString(b, value)
}
func xmlString(b *strings.Builder, value string) {
	b.WriteString("<string>")
	xmlText(b, value)
	b.WriteString("</string>")
}
func xmlText(b *strings.Builder, value string) {
	for _, r := range value {
		switch r {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '"':
			b.WriteString("&quot;")
		case '\'':
			b.WriteString("&apos;")
		default:
			b.WriteRune(r)
		}
	}
}

func validatePlist(ctx context.Context, runner Runner, content []byte) error {
	dir, err := os.MkdirTemp("", "symphony-plist-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "candidate.plist")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		return err
	}
	if _, err := runner.Run(ctx, "plutil", "-lint", path); err != nil {
		return fmt.Errorf("generated LaunchAgent plist is invalid: %w", err)
	}
	return nil
}

// rejectConflicts refuses any registration that would collide with another
// service. ignorePlist is the one validated pre-installer LaunchAgent an
// explicit migration is replacing; it is empty for every other operation.
func rejectConflicts(ctx context.Context, options Options, d desired, ignorePlist string) error {
	instances, err := operator.Discover(ctx, operator.Options{LaunchAgentsDir: filepath.Dir(d.PlistPath)})
	if err != nil {
		return fmt.Errorf("discover existing Symphony services: %w", err)
	}
	for _, existing := range instances {
		if ignorePlist != "" && canonical(existing.Paths.Plist) == canonical(ignorePlist) {
			continue
		}
		if existing.ID == d.Label {
			if canonical(existing.Paths.Plist) != canonical(d.PlistPath) || !existing.Managed {
				return fmt.Errorf("refusing to overwrite unmanaged LaunchAgent %s", d.PlistPath)
			}
			if canonical(existing.Paths.Workflow) != canonical(d.Workflow) {
				return fmt.Errorf("service instance identity %s is already registered for workflow %s", d.Label, existing.Paths.Workflow)
			}
			continue
		}
		for _, conflict := range []struct{ kind, left, right string }{
			{"workflow", d.Workflow, existing.Paths.Workflow},
			{"status file", d.StatusFile, existing.Paths.StatusFile},
			{"log root", d.LogsRoot, existing.Paths.LogsRoot},
		} {
			if conflict.right != "" && canonical(conflict.left) == canonical(conflict.right) {
				return fmt.Errorf("service conflict with %s: shared %s %s", existing.ID, conflict.kind, conflict.left)
			}
		}
		if existing.Config != nil && d.WorkspaceSource != "" && existing.Config.WorkspaceSource != "" && canonical(d.WorkspaceSource) == canonical(existing.Config.WorkspaceSource) {
			return fmt.Errorf("service conflict with %s: shared workspace source %s", existing.ID, d.WorkspaceSource)
		}
	}
	return nil
}

func findManaged(ctx context.Context, options Options, d desired) (operator.Instance, error) {
	instances, err := operator.Discover(ctx, operator.Options{LaunchAgentsDir: filepath.Dir(d.PlistPath)})
	if err != nil {
		return operator.Instance{}, err
	}
	var matches []operator.Instance
	for _, instance := range instances {
		if instance.ID == d.Label || canonical(instance.Paths.Workflow) == canonical(d.Workflow) {
			matches = append(matches, instance)
		}
	}
	if len(matches) == 0 {
		return operator.Instance{}, fmt.Errorf("no Symphony service is installed for %s", d.Workflow)
	}
	if len(matches) > 1 {
		return operator.Instance{}, fmt.Errorf("ambiguous Symphony services for %s; leave exactly one LaunchAgent for this repository in place, then run symphony service migrate to adopt it", d.Workflow)
	}
	instance := matches[0]
	if !instance.Managed || canonical(instance.Paths.Workflow) != canonical(d.Workflow) {
		return operator.Instance{}, fmt.Errorf("refusing to manage unrelated LaunchAgent %s; run symphony service migrate to adopt a compatible hand-authored Symphony LaunchAgent for this repository", instance.Paths.Plist)
	}
	if options.Name != "" && instance.ID != d.Label {
		return operator.Instance{}, fmt.Errorf("configured service name %s does not match %s", d.Label, instance.ID)
	}
	return instance, nil
}

// canonical resolves symlinks so path comparisons match what Git, launchd, and
// a running daemon actually use. A repository reached through a symlink must
// not read as a different repository. Paths that do not exist yet, such as a
// status file, are resolved through their nearest existing parent.
func canonical(path string) string {
	if path == "" {
		return ""
	}
	path = filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	parent := filepath.Dir(path)
	if parent == path {
		return path
	}
	return filepath.Join(canonical(parent), filepath.Base(path))
}

func atomicWrite(path string, content []byte, permission os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, ".symphony-plist-")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(permission); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(content); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempPath, path)
}

// prior describes the registration a failed update must put back. Loaded and
// Restore keep the two rollback paths from stepping on each other: a rollback
// never bootstraps a registration that was not loaded, and never bootstraps
// one whose restoration another caller owns.
type prior struct {
	Content []byte
	Loaded  bool
	Restore bool
}

func reload(ctx context.Context, runner Runner, d desired, previous prior) error {
	service := launchService(d.Label)
	// bootout is expected to fail for a first installation. Any prior plist was
	// already validated. If loading or starting the replacement fails, restore
	// that prior registration before reporting the failed update.
	_ = launchctl(ctx, runner, "bootout", service)
	if err := launchctl(ctx, runner, "bootstrap", "gui/"+fmt.Sprint(os.Getuid()), d.PlistPath); err != nil {
		return restoreAfterReloadFailure(ctx, runner, d, previous, "load", err)
	}
	if err := launchctl(ctx, runner, "kickstart", "-k", service); err != nil {
		// Do not leave a replacement registration loaded when it did not start.
		_ = launchctl(ctx, runner, "bootout", service)
		return restoreAfterReloadFailure(ctx, runner, d, previous, "start", err)
	}
	return nil
}

func restoreAfterReloadFailure(ctx context.Context, runner Runner, d desired, previous prior, operation string, cause error) error {
	if previous.Content == nil {
		if err := os.Remove(d.PlistPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("%s %s: %w (remove failed installation plist: %v)", operation, d.Label, cause, err)
		}
		return fmt.Errorf("%s %s: %w", operation, d.Label, cause)
	}
	if err := atomicWrite(d.PlistPath, previous.Content, 0o600); err != nil {
		return fmt.Errorf("%s %s: %w (restore prior plist: %v)", operation, d.Label, cause, err)
	}
	if previous.Restore && previous.Loaded {
		if err := launchctl(ctx, runner, "bootstrap", "gui/"+fmt.Sprint(os.Getuid()), d.PlistPath); err != nil {
			return fmt.Errorf("%s %s: %w (restore prior service: %v)", operation, d.Label, cause, err)
		}
	}
	return fmt.Errorf("%s %s: %w", operation, d.Label, cause)
}

func launchService(label string) string { return "gui/" + fmt.Sprint(os.Getuid()) + "/" + label }

func launchctl(ctx context.Context, runner Runner, args ...string) error {
	out, err := runner.Run(ctx, "launchctl", args...)
	if err != nil {
		return fmt.Errorf("%s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}
