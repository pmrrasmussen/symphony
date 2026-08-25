package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pmrrasmussen/symphony/internal/operator"
)

// backupSuffix marks the repository-scoped copy of a replaced LaunchAgent. It
// deliberately does not end in .plist and is kept outside ~/Library/LaunchAgents
// so launchd can never load the replaced registration again by accident.
const backupSuffix = ".pre-migration.backup"

// Migration reports one explicit migration outcome. Legacy and Backup are
// empty when the repository was already managed and nothing needed adopting.
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
// managed runtime paths. Unrelated agents are never inspected further, an
// ambiguous set is refused, and the prior registration is restored when
// validation, installation, or bootstrap fails.
func Migrate(ctx context.Context, options Options) (Migration, error) {
	d, runner, err := prepare(ctx, options, true)
	if err != nil {
		return Migration{}, err
	}
	if err := validatePlist(ctx, runner, d.Content); err != nil {
		return Migration{}, err
	}
	instances, err := operator.Discover(ctx, operator.Options{LaunchAgentsDir: filepath.Dir(d.PlistPath)})
	if err != nil {
		return Migration{}, fmt.Errorf("discover existing Symphony services: %w", err)
	}
	legacy, err := selectLegacy(instances, d)
	if err != nil {
		return Migration{}, err
	}
	if legacy == nil {
		// A rerun after a completed migration must stay a successful no-op,
		// and migrate must never become an implicit first-time installer.
		managed, err := findManaged(ctx, options, d)
		if err != nil {
			return Migration{}, fmt.Errorf("no hand-authored Symphony LaunchAgent to migrate for %s: %w", d.Repository, err)
		}
		return Migration{Instance: Instance{Label: managed.ID, PlistPath: managed.Paths.Plist, Workflow: managed.Paths.Workflow}}, nil
	}
	if err := rejectConflicts(ctx, options, d, legacy.Paths.Plist); err != nil {
		return Migration{}, err
	}
	if err := ensureDirectories(d); err != nil {
		return Migration{}, err
	}
	legacyContent, err := os.ReadFile(legacy.Paths.Plist)
	if err != nil {
		return Migration{}, fmt.Errorf("read legacy LaunchAgent: %w", err)
	}
	backup := filepath.Join(d.Repository, ".symphony", "service", filepath.Base(legacy.Paths.Plist)+backupSuffix)
	if err := atomicWrite(backup, legacyContent, 0o600); err != nil {
		return Migration{}, fmt.Errorf("back up legacy LaunchAgent: %w", err)
	}
	old, err := os.ReadFile(d.PlistPath)
	if err != nil && !os.IsNotExist(err) {
		return Migration{}, fmt.Errorf("read existing LaunchAgent: %w", err)
	}
	separate := filepath.Clean(legacy.Paths.Plist) != filepath.Clean(d.PlistPath)
	// Stop the legacy scheduler and drop its registration before the managed
	// one loads, so the repository never has two schedulers loaded at once.
	_ = launchctl(ctx, runner, "bootout", launchService(legacy.ID))
	if separate {
		if err := os.Remove(legacy.Paths.Plist); err != nil {
			return Migration{}, restoreLegacy(ctx, runner, *legacy, legacyContent, fmt.Errorf("remove legacy LaunchAgent plist: %w", err))
		}
	}
	if err := atomicWrite(d.PlistPath, d.Content, 0o600); err != nil {
		return Migration{}, restoreLegacy(ctx, runner, *legacy, legacyContent, fmt.Errorf("install LaunchAgent plist: %w", err))
	}
	if err := reload(ctx, runner, d, old); err != nil {
		// reload already restored or removed the managed plist; only a
		// separate legacy registration still needs restoring.
		if separate {
			return Migration{}, restoreLegacy(ctx, runner, *legacy, legacyContent, err)
		}
		return Migration{}, err
	}
	return Migration{Instance: d.Instance, Legacy: legacy.ID, LegacyPlist: legacy.Paths.Plist, Backup: backup, Changed: true}, nil
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
		if pair[0] != "" && filepath.Clean(pair[0]) == filepath.Clean(pair[1]) {
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
	if instance.Paths.WorkingDirectory == "" || filepath.Clean(instance.Paths.WorkingDirectory) != d.Repository {
		reasons = append(reasons, fmt.Sprintf("WorkingDirectory %q is not the repository %s", instance.Paths.WorkingDirectory, d.Repository))
	}
	if filepath.Clean(instance.Paths.Workflow) != filepath.Clean(d.Workflow) {
		reasons = append(reasons, fmt.Sprintf("workflow %q is not %s", instance.Paths.Workflow, d.Workflow))
	}
	reasons = append(reasons, legacyExecutableReasons(instance.Paths.Executable)...)
	if filepath.Clean(instance.Paths.LogsRoot) != filepath.Clean(d.LogsRoot) {
		reasons = append(reasons, fmt.Sprintf("log root %q is not %s", instance.Paths.LogsRoot, d.LogsRoot))
	}
	if instance.Paths.StatusFile != "" && filepath.Clean(instance.Paths.StatusFile) != filepath.Clean(d.StatusFile) {
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
	relative, err := filepath.Rel(repository, filepath.Clean(path))
	if err != nil {
		return false
	}
	return relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

// restoreLegacy puts a validated pre-installer registration back exactly as it
// was found, so a failed migration still leaves a recoverable service.
func restoreLegacy(ctx context.Context, runner Runner, legacy operator.Instance, content []byte, cause error) error {
	if err := atomicWrite(legacy.Paths.Plist, content, 0o600); err != nil {
		return fmt.Errorf("migrate %s: %w (restore legacy plist %s: %v)", legacy.ID, cause, legacy.Paths.Plist, err)
	}
	if legacy.Launchd.Loaded {
		if err := launchctl(ctx, runner, "bootstrap", "gui/"+fmt.Sprint(os.Getuid()), legacy.Paths.Plist); err != nil {
			return fmt.Errorf("migrate %s: %w (restore legacy service: %v)", legacy.ID, cause, err)
		}
	}
	return fmt.Errorf("migrate %s: %w (restored the prior LaunchAgent %s)", legacy.ID, cause, legacy.Paths.Plist)
}
