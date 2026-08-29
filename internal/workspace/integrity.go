package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"

	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/domain"
	"github.com/pmrrasmussen/symphony/internal/observability"
)

// assertSourceIntegrity is the PMR-65 defense-in-depth backstop. It re-checks
// that the run left the source repository's branches (other than the symphony/*
// publish branches Symphony itself creates) exactly as they were when the
// workspace was prepared, and alerts if not. It deliberately only detects and
// alerts; it never rewrites the operator's refs, because it cannot distinguish
// an agent breach from a legitimate concurrent operator change.
//
// A moved ref is not automatically an alert: a fast-forward to a commit
// reachable from that branch's remote-tracking ref is logged at Debug instead
// (PMR-145). Any other change to refs/heads/* still alerts at Error, and
// diffSourceRefs fails closed on a ref it could not classify rather than
// dropping it (PMR-147).
//
// docs/architecture.md's "Workspace isolation and the sandbox boundary" section
// states why classification is the expensive half and what its failure modes
// are.
func (l *Local) assertSourceIntegrity(ctx context.Context, ws domain.Workspace, issue domain.Issue) {
	if ws.GitIntegrityBaseline == "" {
		return
	}
	var baseline sourceIntegritySnapshot
	if err := json.Unmarshal([]byte(ws.GitIntegrityBaseline), &baseline); err != nil {
		l.logger().Warn("workspace source integrity check failed", "issue_id", issue.ID, "issue_identifier", issue.Identifier, "error", err)
		return
	}
	state, found, err := l.loadState(issue)
	if err != nil || !found || state.SourceRoot == "" {
		return
	}
	settings := l.settings()
	current, err := captureSourceIntegrity(ctx, settings, state.SourceRoot)
	if err != nil {
		l.logger().Warn("workspace source integrity check failed", "issue_id", issue.ID, "issue_identifier", issue.Identifier, "error", err)
		return
	}
	alerts, explained := diffSourceRefs(ctx, settings, state.SourceRoot, baseline.Refs, current.Refs)
	if len(explained) > 0 {
		l.logger().Debug("workspace source integrity change explained by a fast-forward reachable from a remote-tracking ref",
			"issue_id", issue.ID, "issue_identifier", issue.Identifier, "changed_refs", formatRefChanges(explained))
	}
	if len(alerts) == 0 {
		return
	}
	l.logger().Error("workspace source integrity alert", "operation", observability.OperationSourceIntegrityAlert,
		"issue_id", issue.ID, "issue_identifier", issue.Identifier, "source_root", state.SourceRoot, "changed_refs", formatRefChanges(alerts))
}

// sourceIntegritySnapshot is the JSON-encoded shape of GitIntegrityBaseline: the
// source repository's branch heads an isolated detached worktree must never
// modify, keyed by full ref name and excluding the symphony/* publish branches
// Symphony itself creates.
type sourceIntegritySnapshot struct {
	Refs map[string]string `json:"refs"`
}

// captureSourceIntegrity reads the current refs/heads state to compare against
// or record as a sourceIntegritySnapshot.
func captureSourceIntegrity(ctx context.Context, settings config.Settings, sourceRoot string) (sourceIntegritySnapshot, error) {
	refs, err := gitMetadataAllowEmpty(ctx, settings, sourceRoot, "for-each-ref", "--format=%(refname) %(objectname)", "refs/heads/")
	if err != nil {
		return sourceIntegritySnapshot{}, fmt.Errorf("read source branch heads: %w", err)
	}
	kept := make(map[string]string)
	for _, line := range strings.Split(refs, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		name, oid, ok := strings.Cut(line, " ")
		if !ok || strings.HasPrefix(name, "refs/heads/symphony/") {
			continue
		}
		kept[name] = oid
	}
	return sourceIntegritySnapshot{Refs: kept}, nil
}

// refChange describes a single refs/heads/* entry whose value differs between
// the prepare-time baseline and the post-run snapshot. Before or After is
// empty when the ref did not exist on that side. Reason is set only when this
// change is an alert because it could not be classified (see diffSourceRefs);
// it names the git subprocess failure responsible.
type refChange struct{ Name, Before, After, Reason string }

// diffSourceRefs classifies every ref that moved between baseline and current
// into alerts (report at Error) and explained (a fast-forward reachable from a
// remote-tracking ref -- ordinary operator pull activity, report at Debug). A
// ref that is new, deleted, or moved by anything other than a fast-forward
// landing a commit some remote already has is always an alert: only the one
// documented, narrow shape of concurrent operator activity is explained.
//
// Classification itself can fail -- isAncestor or reachableFromRemote can
// return an error distinct from a negative answer, e.g. a pruned object, a
// concurrent `git gc`, or lock contention. That is not evidence the change was
// benign, so it is not silently dropped: it is reported as an alert with
// Reason set to the classification failure (PMR-147). isAncestor's exit-code-1
// "not an ancestor" result is a legitimate negative answer, not a failure, and
// is handled by isAncestor itself.
func diffSourceRefs(ctx context.Context, settings config.Settings, sourceRoot string, baseline, current map[string]string) (alerts, explained []refChange) {
	names := make(map[string]struct{}, len(baseline)+len(current))
	for name := range baseline {
		names[name] = struct{}{}
	}
	for name := range current {
		names[name] = struct{}{}
	}
	for name := range names {
		before, hadBefore := baseline[name]
		after, hasAfter := current[name]
		if hadBefore && hasAfter && before == after {
			continue
		}
		change := refChange{Name: name, Before: before, After: after}
		if hadBefore && hasAfter {
			ff, ffErr := isAncestor(ctx, settings, sourceRoot, before, after)
			if ffErr != nil {
				change.Reason = ffErr.Error()
				alerts = append(alerts, change)
				continue
			}
			if ff {
				reachable, reachErr := reachableFromRemote(ctx, settings, sourceRoot, after)
				if reachErr != nil {
					change.Reason = reachErr.Error()
					alerts = append(alerts, change)
					continue
				}
				if reachable {
					explained = append(explained, change)
					continue
				}
			}
		}
		alerts = append(alerts, change)
	}
	sort.Slice(alerts, func(i, j int) bool { return alerts[i].Name < alerts[j].Name })
	sort.Slice(explained, func(i, j int) bool { return explained[i].Name < explained[j].Name })
	return alerts, explained
}

// isAncestor reports whether ancestor is an ancestor of (or equal to)
// descendant, i.e. whether descendant is a fast-forward from ancestor.
func isAncestor(ctx context.Context, settings config.Settings, dir, ancestor, descendant string) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "merge-base", "--is-ancestor", ancestor, descendant)
	cmd.Env = gitEnvironment(settings)
	var stderr boundedBuffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("git merge-base --is-ancestor: %w: %s", err, stderr.String())
}

// reachableFromRemote reports whether commit is reachable from any
// remote-tracking ref, the mark of a commit the operator's own `git pull` (or
// equivalent fetch) could plausibly have introduced.
func reachableFromRemote(ctx context.Context, settings config.Settings, dir, commit string) (bool, error) {
	out, err := gitMetadataAllowEmpty(ctx, settings, dir, "for-each-ref", "--contains="+commit, "refs/remotes/")
	if err != nil {
		return false, fmt.Errorf("check remote-tracking reachability: %w", err)
	}
	return strings.TrimSpace(out) != "", nil
}

// formatRefChanges renders ref changes for a log attribute: one
// "name before->after" entry per change, "(none)" standing in for a ref that
// did not exist on that side. A change whose classification failed (Reason
// set) appends " classification_failed=<error>" so an operator can triage it
// as a diagnostic gap rather than mistake it for a plain ref move.
func formatRefChanges(changes []refChange) string {
	parts := make([]string, 0, len(changes))
	for _, c := range changes {
		before, after := c.Before, c.After
		if before == "" {
			before = "(none)"
		}
		if after == "" {
			after = "(none)"
		}
		entry := fmt.Sprintf("%s %s->%s", c.Name, before, after)
		if c.Reason != "" {
			entry += " classification_failed=" + c.Reason
		}
		parts = append(parts, entry)
	}
	return strings.Join(parts, "; ")
}
