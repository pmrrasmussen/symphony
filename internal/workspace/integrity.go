package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/domain"
	"github.com/pmrrasmussen/symphony/internal/observability"
)

// assertSourceIntegrity is the PMR-65 backstop, and since PMR-161 the enforced
// half of the write boundary rather than defense in depth: the Claude CLI
// widens its own git-metadata grant to the whole source .git directory, so a
// Bash command there can still move refs/heads/* and no launch setting narrows
// it back. It re-checks that the run left the source repository's branches
// (other than the symphony/* publish branches Symphony itself creates) exactly
// as they were when the workspace was prepared, alerts if not, and returns that
// alert so the caller fails the run. It still never rewrites the operator's
// refs: it cannot distinguish an agent breach from a concurrent operator change
// well enough to repair one, only well enough to refuse to call the run good.
//
// A moved ref is not automatically an alert: a fast-forward to a commit
// reachable from that branch's remote-tracking ref is logged at Debug, fails
// nothing, and returns nil (PMR-145). Any other change to refs/heads/* still
// alerts at Error, and diffSourceRefs fails closed on a ref it could not
// classify rather than dropping it (PMR-147).
//
// Both halves of what it compares -- the baseline and the source root it was
// taken against -- travel on the workspace value, not in the state record on
// disk. A run whose issue reached a terminal state removes that record from
// inside the run, before AfterRun is reached, so reading the root from disk
// skipped this check entirely on the runs that ended cleanly (PMR-161).
//
// The check itself failing -- an unreadable baseline, a source repository that
// cannot be fingerprinted -- stays a Warn and fails nothing: that is evidence
// about the check, not about the boundary. A ref it did read and could not
// classify is the opposite case and does fail the run, because there the
// evidence is about a ref that genuinely moved (see diffSourceRefs).
//
// docs/architecture.md's "Workspace isolation and the sandbox boundary" section
// states why classification is the expensive half and what its failure modes
// are.
func (l *Local) assertSourceIntegrity(ctx context.Context, ws domain.Workspace, issue domain.Issue) error {
	if ws.GitIntegrityBaseline == "" {
		return nil
	}
	var baseline sourceIntegritySnapshot
	if err := json.Unmarshal([]byte(ws.GitIntegrityBaseline), &baseline); err != nil {
		l.logger().Warn("workspace source integrity check failed", "issue_id", issue.ID, "issue_identifier", issue.Identifier, "error", err)
		return nil
	}
	if ws.SourceRoot == "" {
		return nil
	}
	settings := l.settings()
	current, err := captureSourceIntegrity(ctx, settings, ws.SourceRoot)
	if err != nil {
		l.logger().Warn("workspace source integrity check failed", "issue_id", issue.ID, "issue_identifier", issue.Identifier, "error", err)
		return nil
	}
	alerts, explained := diffSourceRefs(ctx, settings, ws.SourceRoot, baseline.Refs, current.Refs)
	if len(explained) > 0 {
		l.logger().Debug("workspace source integrity change explained by a fast-forward reachable from a remote-tracking ref",
			"issue_id", issue.ID, "issue_identifier", issue.Identifier, "changed_refs", formatRefChanges(explained))
	}
	if len(alerts) == 0 {
		return nil
	}
	attributeRefChanges(ctx, settings, ws.SourceRoot, alerts)
	changes := formatRefChanges(alerts)
	l.logger().Error("workspace source integrity alert", "operation", observability.OperationSourceIntegrityAlert,
		"issue_id", issue.ID, "issue_identifier", issue.Identifier, "source_root", ws.SourceRoot, "changed_refs", changes)
	return domain.SourceIntegrityError{SourceRoot: ws.SourceRoot, Changes: changes}
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
// it names the git subprocess failure responsible. Attribution names the
// workspace that wrote the commit, when attributeRefChanges could identify one.
type refChange struct{ Name, Before, After, Reason, Attribution string }

// attributeRefChanges names, for each alerting change, the workspace whose own
// HEAD carries the commit the ref moved to -- the session that wrote it, rather
// than whichever session happened to finish first and run the check. PMR-156
// observed the difference directly: an alert filed under one issue reported
// refs/heads/main moving to another issue's commit, and named only the reporter.
//
// Two shapes of evidence count and nothing else does. A workspace HEAD equal to
// the new value is unambiguous. Otherwise the commit must be reachable from that
// HEAD *and* from no remote-tracking ref: every workspace starts detached at the
// freshly fetched base commit, so a commit it contains that no remote has is one
// written inside it. Dropping that second condition would attribute a ref moved
// backwards onto an old commit to every live workspace at once, since they all
// contain it. Ambiguity is reported rather than resolved -- several matches are
// all named -- and a git call that fails leaves the change unattributed, because
// attribution is a diagnostic and the alert stands without it.
func attributeRefChanges(ctx context.Context, settings config.Settings, sourceRoot string, alerts []refChange) {
	worktrees, err := linkedWorktrees(ctx, settings, sourceRoot)
	if err != nil || len(worktrees) == 0 {
		return
	}
	for i := range alerts {
		commit := alerts[i].After
		if commit == "" {
			continue // A deleted ref names no commit to attribute.
		}
		reachable, err := reachableFromRemote(ctx, settings, sourceRoot, commit)
		local := err == nil && !reachable
		var exact, contains []string
		for _, wt := range worktrees {
			if wt.head == commit {
				exact = append(exact, wt.key)
				continue
			}
			if !local {
				continue
			}
			if ancestor, err := isAncestor(ctx, settings, sourceRoot, commit, wt.head); err == nil && ancestor {
				contains = append(contains, wt.key)
			}
		}
		matched := exact
		if len(matched) == 0 {
			matched = contains
		}
		if len(matched) > 0 {
			sort.Strings(matched)
			alerts[i].Attribution = strings.Join(matched, ",")
		}
	}
}

// linkedWorktree is one of Symphony's own workspaces as the source repository
// knows it: the workspace key its directory is named for, and the commit its
// HEAD is at.
type linkedWorktree struct{ key, head string }

// linkedWorktrees lists the source repository's linked worktrees, excluding its
// own main working tree -- a ref that moved to the commit the operator's own
// checkout is sitting on says nothing about which session wrote it.
func linkedWorktrees(ctx context.Context, settings config.Settings, sourceRoot string) ([]linkedWorktree, error) {
	out, err := gitMetadataAllowEmpty(ctx, settings, sourceRoot, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	main := resolvedPath(sourceRoot)
	var list []linkedWorktree
	var path, head string
	flush := func() {
		if path != "" && path != main && head != "" {
			list = append(list, linkedWorktree{key: filepath.Base(path), head: head})
		}
		path, head = "", ""
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "worktree "):
			flush()
			path = resolvedPath(strings.TrimPrefix(line, "worktree "))
		case strings.HasPrefix(line, "HEAD "):
			head = strings.TrimSpace(strings.TrimPrefix(line, "HEAD "))
		}
	}
	flush()
	return list, nil
}

// resolvedPath canonicalizes a path for comparison, falling back to a lexical
// clean for one that no longer exists: git's own listing and Symphony's
// recorded source root can name the same directory through different symlinks
// (/var and /private/var on darwin), and a failed comparison would leave the
// main working tree in the candidate set.
func resolvedPath(path string) string {
	if resolved, err := canonicalExistingDirectory(path); err == nil {
		return resolved
	}
	return filepath.Clean(strings.TrimSpace(path))
}

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
// as a diagnostic gap rather than mistake it for a plain ref move. An
// attributed change appends " attributed_to=<workspace keys>", which is the
// session an operator should look at rather than the one this record is filed
// under; its absence means no live workspace carries that commit.
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
		if c.Attribution != "" {
			entry += " attributed_to=" + c.Attribution
		}
		if c.Reason != "" {
			entry += " classification_failed=" + c.Reason
		}
		parts = append(parts, entry)
	}
	return strings.Join(parts, "; ")
}
