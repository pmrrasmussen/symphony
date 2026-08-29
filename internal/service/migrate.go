package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/pmrrasmussen/symphony/internal/operator"
)

// backupSuffix marks the repository-scoped copy of a replaced LaunchAgent. It
// deliberately does not end in .plist and is kept outside ~/Library/LaunchAgents
// so launchd can never load the replaced registration again by accident.
const backupSuffix = ".pre-migration.backup"

// unloadAttempts and unloadDelay bound the verification that a legacy service
// really stopped, tolerating launchd reporting an unload as in progress.
const (
	unloadAttempts = 3
	unloadDelay    = 250 * time.Millisecond
)

// launchd exit statuses this package must tell apart. 113 is print's "could
// not find service" and 3 is bootout's ESRCH; both are positive statements
// that launchd has no such job. Every other status means the observation
// failed and says nothing about whether a service is running.
const (
	statusNoSuchService = 113
	statusNoSuchProcess = 3
)

// Migration reports one explicit migration outcome. Legacy, LegacyPlist, and
// Backup are empty when the repository was already managed and nothing needed
// adopting.
type Migration struct {
	Instance
	Legacy      string
	LegacyPlist string
	Backup      string
	Changed     bool
}

// Migrate adopts exactly one compatible, hand-authored Symphony LaunchAgent
// for this repository into the managed convention. Running this command is the
// required operator intent: nothing else in the service surface ever replaces
// an unmanaged LaunchAgent. The candidate must match this repository, its
// workflow, a Symphony executable, the Symphony label convention, and the
// managed runtime paths. Unrelated agents are never inspected further and an
// ambiguous set is refused.
//
// Every check runs before anything is written or unloaded: no other Symphony
// job may be registered for this repository, and the legacy service must be
// positively observed as gone. Once mutation starts, a failure puts the
// replaced LaunchAgent back byte for byte with its original mode, and
// re-bootstraps it only if it was loaded to begin with.
func Migrate(ctx context.Context, options Options) (Migration, error) {
	d, runner, err := prepare(ctx, options, true)
	if err != nil {
		return Migration{}, err
	}
	if err := validatePlist(ctx, runner, d.Content); err != nil {
		return Migration{}, err
	}
	launchDir := filepath.Dir(d.PlistPath)
	instances, err := operator.Discover(ctx, operator.Options{LaunchAgentsDir: launchDir})
	if err != nil {
		return Migration{}, fmt.Errorf("discover existing Symphony services: %w", err)
	}
	candidate, err := selectLegacy(instances, d)
	if err != nil {
		return Migration{}, err
	}
	if candidate == nil {
		// A rerun after a completed migration must stay a successful no-op,
		// and migrate must never become an implicit first-time installer.
		managed, err := findManaged(ctx, options, d, false)
		if err != nil {
			return Migration{}, fmt.Errorf("no hand-authored Symphony LaunchAgent to migrate for %s: %w", d.Repository, err)
		}
		return Migration{Instance: Instance{Label: managed.ID, PlistPath: managed.Paths.Plist, Workflow: managed.Paths.Workflow}}, nil
	}
	if err := rejectConflicts(ctx, options, d, candidate.Paths.Plist); err != nil {
		return Migration{}, err
	}
	legacy, err := readLegacy(*candidate)
	if err != nil {
		return Migration{}, err
	}
	if err := assertNoRivalSchedulers(ctx, runner, launchDir, allowedLabels(instances, d, legacy.label)); err != nil {
		return Migration{}, err
	}
	legacy.loaded, err = unloadLegacy(ctx, runner, legacy.label, legacy.plist)
	if err != nil {
		return Migration{}, err
	}
	// Only now, with every guard passed and the legacy scheduler proven
	// stopped, does anything on disk change.
	if err := ensureDirectories(d); err != nil {
		return Migration{}, err
	}
	legacy.backup = filepath.Join(d.Repository, ".symphony", "service", filepath.Base(legacy.plist)+backupSuffix)
	if err := atomicWrite(legacy.backup, legacy.content, 0o600); err != nil {
		return Migration{}, fmt.Errorf("back up legacy LaunchAgent: %w", err)
	}
	old, err := os.ReadFile(d.PlistPath)
	if err != nil && !os.IsNotExist(err) {
		return Migration{}, fmt.Errorf("read existing LaunchAgent: %w", err)
	}
	separate := canonical(legacy.plist) != canonical(d.PlistPath)
	if separate {
		if err := os.Remove(legacy.plist); err != nil {
			return Migration{}, legacy.restore(ctx, runner, fmt.Errorf("remove legacy LaunchAgent plist: %w", err))
		}
	}
	if err := atomicWrite(d.PlistPath, d.Content, 0o600); err != nil {
		return Migration{}, legacy.restore(ctx, runner, fmt.Errorf("install LaunchAgent plist: %w", err))
	}
	// reload restores the managed path's own prior registration only when that
	// is a different service; when the prior file at that path is this legacy
	// agent, legacy.restore owns putting it back, so exactly one rollback
	// bootstraps any given registration.
	previous := prior{Content: old, Restore: separate}
	if separate && old != nil {
		previous.Loaded = serviceLoadState(ctx, runner, d.Label) == loadLoaded
	}
	if err := reload(ctx, runner, d, previous); err != nil {
		return Migration{}, legacy.restore(ctx, runner, err)
	}
	return Migration{Instance: d.Instance, Legacy: legacy.label, LegacyPlist: legacy.plist, Backup: legacy.backup, Changed: true}, nil
}

// legacyState is everything needed to put a replaced LaunchAgent back exactly
// as it was found.
type legacyState struct {
	label   string
	plist   string
	content []byte
	mode    os.FileMode
	loaded  bool
	backup  string
}

func readLegacy(instance operator.Instance) (legacyState, error) {
	content, err := os.ReadFile(instance.Paths.Plist)
	if err != nil {
		return legacyState{}, fmt.Errorf("read legacy LaunchAgent: %w", err)
	}
	// Restoring a 0644 plist as 0600 would not be the same registration, so
	// the original mode is carried through rollback.
	mode := os.FileMode(0o600)
	if info, err := os.Stat(instance.Paths.Plist); err == nil {
		mode = info.Mode().Perm()
	}
	return legacyState{label: instance.ID, plist: instance.Paths.Plist, content: content, mode: mode}, nil
}

// restore puts the replaced registration back and re-bootstraps it only if it
// was loaded before the migration, so a failed migration never starts a
// service the operator had deliberately stopped.
func (l legacyState) restore(ctx context.Context, runner Runner, cause error) error {
	if err := atomicWrite(l.plist, l.content, l.mode); err != nil {
		return fmt.Errorf("migrate %s: %w (restoring %s failed: %v; its contents remain at %s)", l.label, cause, l.plist, err, l.backup)
	}
	if l.loaded {
		if err := launchctl(ctx, runner, "bootstrap", userDomain(), l.plist); err != nil {
			return fmt.Errorf("migrate %s: %w (restored the prior LaunchAgent %s but could not load it: %v; a copy remains at %s)", l.label, cause, l.plist, err, l.backup)
		}
	}
	return fmt.Errorf("migrate %s: %w (restored the prior LaunchAgent %s; a copy remains at %s)", l.label, cause, l.plist, l.backup)
}

// selectLegacy returns the single migratable LaunchAgent for this repository,
// nil when there is none, or an actionable error. Agents that share no
// repository path with the candidate installation are left entirely alone.
func selectLegacy(instances []operator.Instance, d desired) (*operator.Instance, error) {
	var related []operator.Instance
	for _, instance := range instances {
		if !instance.Managed && sharesRepository(instance, d) {
			related = append(related, instance)
		}
	}
	if len(related) == 0 {
		return nil, nil
	}
	if len(related) > 1 {
		paths := make([]string, 0, len(related))
		for _, instance := range related {
			paths = append(paths, instance.Paths.Plist)
		}
		sort.Strings(paths)
		return nil, fmt.Errorf("ambiguous hand-authored Symphony LaunchAgents for %s: %s; leave exactly one in place before migrating", d.Repository, strings.Join(paths, ", "))
	}
	candidate := related[0]
	if reasons := incompatibleLegacy(candidate, d); len(reasons) > 0 {
		return nil, fmt.Errorf("refusing to migrate LaunchAgent %s: %s", candidate.Paths.Plist, strings.Join(reasons, "; "))
	}
	return &candidate, nil
}

// sharesRepository is the narrow relation that makes an unmanaged LaunchAgent
// this repository's business: it would otherwise run a second scheduler over
// the same workflow, logs, or status file.
func sharesRepository(instance operator.Instance, d desired) bool {
	for _, pair := range [][2]string{
		{instance.Paths.WorkingDirectory, d.Repository},
		{instance.Paths.Workflow, d.Workflow},
		{instance.Paths.LogsRoot, d.LogsRoot},
		{instance.Paths.StatusFile, d.StatusFile},
	} {
		if pair[0] != "" && canonical(pair[0]) == canonical(pair[1]) {
			return true
		}
	}
	return false
}

// incompatibleLegacy lists every reason a related unmanaged LaunchAgent is not
// an exact pre-installer service for this repository. A non-empty result is
// always a refusal: a partial match may belong to something else entirely.
func incompatibleLegacy(instance operator.Instance, d desired) []string {
	var reasons []string
	filename := strings.TrimSuffix(filepath.Base(instance.Paths.Plist), ".plist")
	if instance.ID != filename {
		reasons = append(reasons, fmt.Sprintf("Label %q does not match plist filename %q", instance.ID, filename))
	}
	if instance.ID != operator.LabelPrefix && !strings.HasPrefix(instance.ID, operator.LabelPrefix+".") {
		reasons = append(reasons, fmt.Sprintf("Label %q is not a Symphony service label", instance.ID))
	}
	for _, finding := range instance.Findings {
		if finding.Severity == operator.SeverityError && strings.HasPrefix(finding.Code, "plist_") {
			reasons = append(reasons, finding.Message)
		}
	}
	if instance.Paths.WorkingDirectory == "" || canonical(instance.Paths.WorkingDirectory) != canonical(d.Repository) {
		reasons = append(reasons, fmt.Sprintf("WorkingDirectory %q is not the repository %s", instance.Paths.WorkingDirectory, d.Repository))
	}
	if canonical(instance.Paths.Workflow) != canonical(d.Workflow) {
		reasons = append(reasons, fmt.Sprintf("workflow %q is not %s", instance.Paths.Workflow, d.Workflow))
	}
	reasons = append(reasons, legacyExecutableReasons(instance.Paths.Executable)...)
	if canonical(instance.Paths.LogsRoot) != canonical(d.LogsRoot) {
		reasons = append(reasons, fmt.Sprintf("log root %q is not %s", instance.Paths.LogsRoot, d.LogsRoot))
	}
	if instance.Paths.StatusFile != "" && canonical(instance.Paths.StatusFile) != canonical(d.StatusFile) {
		reasons = append(reasons, fmt.Sprintf("status file %q is not %s", instance.Paths.StatusFile, d.StatusFile))
	}
	for _, path := range []string{instance.Paths.StandardOut, instance.Paths.StandardError} {
		if !withinRepository(path, d.Repository) {
			reasons = append(reasons, fmt.Sprintf("service log %q is outside the repository %s", path, d.Repository))
		}
	}
	return reasons
}

// legacyExecutableReasons accepts any Symphony executable, including an older
// repository-local one. The managed plist always replaces it with the shared
// binary, so only the identity of the program being replaced is checked here.
func legacyExecutableReasons(executable string) []string {
	if executable == "" {
		return []string{"LaunchAgent has no Program or ProgramArguments"}
	}
	if filepath.Base(executable) != "symphony" {
		return []string{fmt.Sprintf("executable %q is not a Symphony executable", executable)}
	}
	info, err := os.Stat(executable)
	if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
		return []string{fmt.Sprintf("executable %q is not an executable file", executable)}
	}
	return nil
}

func withinRepository(path, repository string) bool {
	if path == "" {
		return true
	}
	relative, err := filepath.Rel(canonical(repository), canonical(path))
	if err != nil {
		return false
	}
	return relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

// allowedLabels are the Symphony jobs that may legitimately be registered
// while this repository is migrated: the managed target, the agent being
// replaced, and the services of other repositories found on disk.
func allowedLabels(instances []operator.Instance, d desired, legacy string) map[string]bool {
	allowed := map[string]bool{d.Label: true, legacy: true}
	for _, instance := range instances {
		if instance.ID != "" && !sharesRepository(instance, d) {
			allowed[instance.ID] = true
		}
	}
	return allowed
}

// assertNoRivalSchedulers refuses to migrate while an unaccounted-for Symphony
// job is registered. No per-label check can see a job whose plist was renamed
// or deleted, yet such a job keeps scheduling this repository's workflow, so
// the loaded set itself is enumerated before anything is replaced.
func assertNoRivalSchedulers(ctx context.Context, runner Runner, launchDir string, allowed map[string]bool) error {
	labels, err := loadedSymphonyServices(ctx, runner)
	if err != nil {
		return err
	}
	var rivals []string
	for _, label := range labels {
		if !allowed[label] {
			rivals = append(rivals, label)
		}
	}
	if len(rivals) == 0 {
		return nil
	}
	sort.Strings(rivals)
	return fmt.Errorf("refusing to migrate while other Symphony services are loaded: %s; no LaunchAgent in %s accounts for them, so they may still be scheduling this repository. Unload each with launchctl bootout \"gui/%d/<label>\" (or restore its LaunchAgent so it can be identified), then rerun symphony service migrate", strings.Join(rivals, ", "), launchDir, os.Getuid())
}

// loadedSymphonyServices enumerates registered jobs under the Symphony label
// prefix. Failing to enumerate is itself a refusal: an unverified launchd is
// not evidence that nothing is running.
func loadedSymphonyServices(ctx context.Context, runner Runner) ([]string, error) {
	out, err := runner.Run(ctx, "launchctl", "list")
	if err != nil {
		return nil, fmt.Errorf("cannot enumerate loaded Symphony services: %w: %s", err, strings.TrimSpace(string(out)))
	}
	var labels []string
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		label := fields[len(fields)-1]
		if label == "Label" {
			continue
		}
		if label == operator.LabelPrefix || strings.HasPrefix(label, operator.LabelPrefix+".") {
			labels = append(labels, label)
		}
	}
	return labels, nil
}

type loadState int

const (
	// loadUnknown means the observation failed. It must never be read as
	// absence: print and bootout target the same service string, so a wrong
	// label makes them fail together.
	loadUnknown loadState = iota
	loadNotFound
	loadLoaded
)

// serviceLoadState is the same read-only launchd observation discovery makes,
// routed through the injected Runner so migration stays testable.
func serviceLoadState(ctx context.Context, runner Runner, label string) loadState {
	out, err := runner.Run(ctx, "launchctl", "print", launchService(label))
	if err == nil {
		return loadLoaded
	}
	if launchdAbsent(out, err, statusNoSuchService) {
		return loadNotFound
	}
	return loadUnknown
}

// launchdAbsent reports whether a failed launchctl call positively stated that
// no such job exists, by exit status or by launchd's own message.
func launchdAbsent(out []byte, err error, status int) bool {
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == status {
		return true
	}
	text := strings.ToLower(string(out))
	return strings.Contains(text, "could not find service") || strings.Contains(text, "no such process")
}

// unloadLegacy stops a pre-installer service and proves it is gone. Absence
// must be positively observed: launchd stating that it has no such job is the
// only benign negative. A bootout that fails for any other reason, or an
// observation that simply does not answer, aborts the migration before any
// destructive step rather than being assumed harmless. It reports whether the
// service was loaded, which decides whether a rollback may start it again.
func unloadLegacy(ctx context.Context, runner Runner, label, plist string) (bool, error) {
	before := serviceLoadState(ctx, runner, label)
	out, bootout := runner.Run(ctx, "launchctl", "bootout", launchService(label))
	// A successful bootout is itself proof the service had been loaded.
	wasLoaded := before == loadLoaded || bootout == nil
	if bootout != nil && launchdAbsent(out, bootout, statusNoSuchProcess) {
		return wasLoaded, nil
	}
	for attempt := 1; ; attempt++ {
		switch serviceLoadState(ctx, runner, label) {
		case loadNotFound:
			return wasLoaded, nil
		case loadUnknown:
			return wasLoaded, fmt.Errorf("cannot verify that legacy service %s is unloaded, so it may still be scheduling this repository; its LaunchAgent is unchanged at %s. Check it with launchctl print %q and unload it manually before rerunning symphony service migrate", label, plist, launchService(label))
		}
		if attempt == unloadAttempts {
			break
		}
		// launchd can report an unload as still in progress.
		select {
		case <-ctx.Done():
			return wasLoaded, fmt.Errorf("migration canceled while unloading legacy service %s: %w; a bootout was already issued, so it may now be stopped, and its LaunchAgent is unchanged at %s", label, ctx.Err(), plist)
		case <-time.After(unloadDelay):
		}
	}
	reason := "launchd still reports it"
	if bootout != nil {
		reason = strings.TrimSpace(string(out)) + ": " + bootout.Error()
	}
	return wasLoaded, fmt.Errorf("legacy service %s is still loaded after bootout (%s); its LaunchAgent and process were left in place. Unload it manually with launchctl bootout %q, then rerun symphony service migrate", label, reason, launchService(label))
}
