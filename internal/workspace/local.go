// Package workspace implements local, bounded workspace lifecycle operations.
package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sync"

	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/domain"
	"github.com/pmrrasmussen/symphony/internal/observability"
)

var unsafe = regexp.MustCompile(`[^A-Za-z0-9._-]`)

type Local struct {
	settings func() config.Settings
	// landing is the optional, host-owned merge verification terminal cleanup
	// consults before it may discard a clean worktree's local commits. It stays
	// nil until an integration is wired, and a nil verifier keeps the original
	// fail-closed refusal.
	landing domain.LandingVerifier
	// log routes the PMR-65 integrity alert and hook diagnostics through the
	// shared redaction boundary instead of os.Stderr. It stays nil until
	// SetLogger is called or logger is first used, at which point logger falls
	// back to the process default, so the zero value and existing tests keep
	// working.
	log *slog.Logger
	// fetchMu serializes every base-ref fetch addWorktree issues. It is the
	// same invariant internal/github's Manager.fetchMu protects: refs/remotes/
	// origin/<base> and packed-refs live in the shared Git common directory,
	// not in any one workspace, so concurrent workspace creation racing that
	// fetch is racing the same repository-wide ref, not independent state
	// (PMR-162).
	fetchMu sync.Mutex
}

// logger returns the operator log sink, defaulting to the process-wide
// handler for a Local built without SetLogger (including the zero value).
func (l *Local) logger() *slog.Logger {
	if l.log != nil {
		return l.log
	}
	return observability.Logger(nil)
}

func New(settings func() config.Settings) *Local {
	return &Local{settings: settings, log: observability.Logger(nil)}
}

// SetLandingVerifier installs the host-side merge verification terminal
// cleanup uses to tell a published, merged commit apart from unpublished local
// work. It must only ever be given a Symphony-owned, read-only verifier; it is
// never reachable from an agent session.
func (l *Local) SetLandingVerifier(v domain.LandingVerifier) { l.landing = v }

// SetLogger routes the PMR-65 integrity alert and hook failure diagnostics at
// the operator log handler instead of the process default, the same way
// linear.Tracker and github.Manager are wired, so those records land in
// symphony.jsonl instead of launchd's stderr file.
func (l *Local) SetLogger(logger *slog.Logger) {
	if logger != nil {
		l.log = observability.Logger(logger)
	}
}
func Key(identifier string) string {
	clean := unsafe.ReplaceAllString(identifier, "_")
	if clean == identifier && clean != "" && clean != "." && clean != ".." && clean != stateDirectory {
		return clean
	}
	if clean == "" || clean == "." || clean == ".." {
		clean = "issue"
	}
	h := sha256.Sum256([]byte(identifier))
	return fmt.Sprintf("%s--%x", clean, h[:8])
}

func (l *Local) Prepare(ctx context.Context, issue domain.Issue) (domain.Workspace, error) {
	if err := ctx.Err(); err != nil {
		return domain.Workspace{}, err
	}
	root, err := l.ensureWorkspaceRoot()
	if err != nil {
		return domain.Workspace{}, err
	}
	key := Key(issue.Identifier)
	path, err := workspacePath(root, key)
	if err != nil {
		return domain.Workspace{}, err
	}
	state, found, err := l.loadState(issue)
	if err != nil {
		return domain.Workspace{}, err
	}
	if found {
		if err := validateStateOwner(state, issue); err != nil {
			return domain.Workspace{}, err
		}
		if state.Preparation != "" && state.Preparation != preparationReady {
			if err := l.recoverPreparation(ctx, issue, path, state); err != nil {
				return domain.Workspace{}, err
			}
			state, found = workspaceState{}, false
		}
	}
	info, err := os.Stat(path)
	created := os.IsNotExist(err)
	if err == nil && !info.IsDir() {
		return domain.Workspace{}, fmt.Errorf("workspace exists but is not a directory: %s", path)
	}
	if err != nil && !os.IsNotExist(err) {
		return domain.Workspace{}, err
	}
	if !found && !created {
		return domain.Workspace{}, fmt.Errorf("workspace state is missing for existing workspace %q; manual recovery is required", path)
	}
	settings := l.settings()
	if created {
		if settings.Workspace.SourceRoot != "" {
			identity, err := sourceIdentity(ctx, settings, settings.Workspace.SourceRoot)
			if err != nil {
				return domain.Workspace{}, err
			}
			state = workspaceState{Schema: workspaceStateSchema, IssueID: issue.ID, Identifier: issue.Identifier, Preparation: preparationCreating, SourceRoot: identity.sourceRoot, GitCommonDir: identity.commonDir, GitCommonDevice: identity.commonDevice, GitCommonInode: identity.commonInode}
			if err := l.writeState(issue, state); err != nil {
				return domain.Workspace{}, err
			}
			if err := l.addWorktree(ctx, settings, identity.sourceRoot, path, settings.GitHub.BaseBranch); err != nil {
				return domain.Workspace{}, err
			}
			worktreeDir, err := worktreeIdentity(ctx, settings, path, identity.commonDir)
			if err != nil {
				return domain.Workspace{}, err
			}
			state.GitWorktreeDir = worktreeDir
			state.BaseCommit, err = gitHead(ctx, settings, path)
			if err != nil {
				return domain.Workspace{}, err
			}
		} else {
			state = workspaceState{Schema: workspaceStateSchema, IssueID: issue.ID, Identifier: issue.Identifier, Preparation: preparationCreating}
			if err := l.writeState(issue, state); err != nil {
				return domain.Workspace{}, err
			}
			if err := os.MkdirAll(path, 0o755); err != nil {
				return domain.Workspace{}, err
			}
		}
		state.Preparation = preparationHookPending
		if err := l.writeState(issue, state); err != nil {
			return domain.Workspace{}, err
		}
	}
	// Re-check after creating the directory or invoking Git. A trusted local
	// process can still race this check with a rename or symlink replacement;
	// see docs/architecture.md for that unavoidable filesystem limitation.
	if path, err = workspacePath(root, key); err != nil {
		return domain.Workspace{}, err
	}
	ws := domain.Workspace{Path: path, Key: key, CreatedNow: created}
	if created && settings.Hooks.AfterCreate != "" {
		if err := l.hook(ctx, ws, issue, "after_create", settings.Hooks.AfterCreate); err != nil {
			recoveryErr := l.recoverPreparation(ctx, issue, path, state)
			if recoveryErr != nil {
				return domain.Workspace{}, fmt.Errorf("%w; partial workspace recovery also failed: %v; retry cleanup or preserve the workspace outside the managed root for manual recovery", err, recoveryErr)
			}
			return domain.Workspace{}, err
		}
	}
	if created {
		if state.SourceRoot == "" {
			git, gitErr := isGitWorkspace(path)
			if gitErr != nil {
				return domain.Workspace{}, gitErr
			}
			if git {
				recoveryErr := l.recoverPreparation(ctx, issue, path, state)
				if recoveryErr != nil {
					return domain.Workspace{}, fmt.Errorf("after_create created an unclassifiable plain Git repository; recovery failed: %v; manual recovery is required", recoveryErr)
				}
				return domain.Workspace{}, errors.New("after_create created a Git repository without a source-worktree identity; the partial workspace was removed and must be configured with workspace.source_root")
			}
		}
		state.Preparation = preparationReady
		if err := l.writeState(issue, state); err != nil {
			return domain.Workspace{}, err
		}
	}
	if err := l.ensureState(ctx, ws, issue); err != nil {
		return domain.Workspace{}, err
	}
	if err := l.setGitMetadataRoot(ctx, &ws, issue); err != nil {
		return domain.Workspace{}, err
	}
	return ws, nil
}

// setGitMetadataRoot grants the agent only the Git boundary a detached-HEAD
// commit in a linked worktree needs: the source repository's shared object
// store and this worktree's own per-worktree metadata directory. Git stores a
// linked worktree's HEAD, index, and reflog under the per-worktree directory and
// its objects in the shared store, so those two roots are sufficient to add and
// commit. It deliberately excludes the rest of the source common directory --
// refs/heads (including the primary branch), the primary working tree's index,
// packed-refs, and other worktrees' directories -- so a misbehaving agent cannot
// write the source repository's branches or primary index (PMR-65). It also
// records an integrity baseline so AfterRun can detect any drift that slips past
// this narrowed grant. The baseline covers refs/heads only, not the primary
// index: the index is outside these two granted roots already, and unlike a
// branch head it has no ancestry to check, so a legitimate concurrent `git add`
// or `git pull` in the operator's own checkout cannot be told apart from a
// write worth alerting on -- it would only add back the false-positive class
// this baseline exists to avoid (PMR-145).
func (l *Local) setGitMetadataRoot(ctx context.Context, ws *domain.Workspace, issue domain.Issue) error {
	state, found, err := l.loadState(issue)
	if err != nil {
		return err
	}
	if !found || state.SourceRoot == "" {
		return nil
	}
	if err := validateStateOwner(state, issue); err != nil {
		return err
	}
	if err := validateWorktreeIdentity(ws.Path, state); err != nil {
		return fmt.Errorf("validate Git workspace for local commits: %w", err)
	}
	settings := l.settings()
	available, err := gitRepositoryAvailable(ctx, settings, state)
	if err != nil {
		return fmt.Errorf("validate Git workspace repository: %w", err)
	}
	if !available {
		return errors.New("recorded Git workspace repository is unavailable; cannot grant local commit access")
	}
	objects, err := canonicalExistingDirectory(filepath.Join(state.GitCommonDir, "objects"))
	if err != nil {
		return fmt.Errorf("resolve Git object store for local commits: %w", err)
	}
	ws.GitMetadataRoots = []string{objects, filepath.Clean(state.GitWorktreeDir)}
	// The source root travels with the workspace so the post-run check reads the
	// repository this baseline was taken against, rather than re-reading a state
	// record a terminal run's own cleanup has already removed by then (PMR-161).
	ws.SourceRoot = state.SourceRoot
	// The integrity baseline is a best-effort backstop: an unexpected failure to
	// fingerprint the source must not fail an otherwise valid workspace. An empty
	// baseline simply skips the post-run assertion.
	if snapshot, err := captureSourceIntegrity(ctx, settings, state.SourceRoot); err == nil {
		if encoded, err := json.Marshal(snapshot); err == nil {
			ws.GitIntegrityBaseline = string(encoded)
		}
	}
	return nil
}
func (l *Local) BeforeRun(ctx context.Context, ws domain.Workspace, issue domain.Issue) error {
	return l.hook(ctx, ws, issue, "before_run", l.settings().Hooks.BeforeRun)
}

// AfterRun runs the after_run hook and then the source-integrity check, and
// returns only the latter's verdict (see domain.WorkspaceExecutor). The hook
// runs first and its failure is logged rather than returned: it is
// repository-owned automation that may legitimately fail, while the integrity
// verdict is the enforced remainder of the write boundary and fails the run.
func (l *Local) AfterRun(ctx context.Context, ws domain.Workspace, issue domain.Issue) error {
	if err := l.hook(ctx, ws, issue, "after_run", l.settings().Hooks.AfterRun); err != nil {
		l.logger().Warn("workspace hook failed", "hook", "after_run", "issue_id", issue.ID, "issue_identifier", issue.Identifier, "error", err)
	}
	return l.assertSourceIntegrity(ctx, ws, issue)
}

// Cleanup discards the workspace Symphony owns for issue, or reports why it
// refused to. It is idempotent in both directions: a workspace that is already
// gone is removed work, not a failure, whichever step discovers that.
func (l *Local) Cleanup(ctx context.Context, issue domain.Issue) (domain.CleanupOutcome, error) {
	path, err := l.workspacePath(issue)
	if err != nil {
		return domain.CleanupClean, err
	}
	outcome, err := l.removeWorkspace(ctx, issue, path)
	if err != nil && workspaceAbsent(path) {
		// A removal that raced another removal of the same worktree fails on
		// whichever step it happened to reach -- "fatal: this operation must be
		// run in a work tree" from the change inspection, "fatal: '...' is not a
		// working tree" from the removal itself -- and both were reported as
		// operator-actionable cleanup failures for a workspace that was, by then,
		// exactly as gone as this call was asking it to be (PMR-160). An absent
		// path is the only thing forgiven here, so every refusal in
		// docs/dogfooding.md section 7 still fails loudly: each of those is a
		// decision to keep a workspace that is still there.
		err = nil
	}
	if err != nil {
		return outcome, err
	}
	// Do not discard the completion record until the workspace was removed.
	return outcome, l.removeState(issue)
}

// workspaceAbsent reports whether nothing remains at path. It uses Lstat rather
// than Stat so a dangling symlink counts as present -- something is still there
// to look at, and Cleanup's own ownership checks refuse a symlinked workspace
// rather than following it.
func workspaceAbsent(path string) bool {
	_, err := os.Lstat(path)
	return os.IsNotExist(err)
}

// removeWorkspace is Cleanup's decision and removal body, without the state
// record that outlives it. It reports the removal outcome and, on failure, the
// reason -- which Cleanup reads together with the path itself.
func (l *Local) removeWorkspace(ctx context.Context, issue domain.Issue, path string) (domain.CleanupOutcome, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return domain.CleanupClean, nil
	} else if err != nil {
		return domain.CleanupClean, err
	}
	state, found, err := l.loadState(issue)
	if err != nil {
		return domain.CleanupClean, err
	}
	if !found {
		return domain.CleanupClean, errors.New("refusing to remove workspace without durable ownership state; preserve it outside the managed root for manual recovery")
	}
	if err := validateStateOwner(state, issue); err != nil {
		return domain.CleanupClean, err
	}
	settings := l.settings()
	ws := domain.Workspace{Path: path, Key: Key(issue.Identifier)}
	if err := l.hook(ctx, ws, issue, "before_remove", settings.Hooks.BeforeRemove); err != nil {
		l.logger().Warn("workspace hook failed", "hook", "before_remove", "issue_id", issue.ID, "issue_identifier", issue.Identifier, "error", err)
	}
	git, err := isGitWorkspace(path)
	if err != nil {
		return domain.CleanupClean, err
	}
	outcome := domain.CleanupClean
	if git {
		if state.BaseCommit == "" {
			return outcome, errors.New("refusing to remove Git workspace without a recorded base commit")
		}
		if state.SourceRoot != "" {
			if err := validateWorktreeIdentity(path, state); err != nil {
				return outcome, err
			}
			available, availableErr := gitRepositoryAvailable(ctx, settings, state)
			if availableErr != nil {
				return outcome, availableErr
			}
			if available {
				unchanged := ensureGitWorkspaceUnchanged(ctx, settings, path, state.BaseCommit)
				landed, err := l.permitCommittedRemoval(ctx, issue, unchanged)
				if err != nil {
					return outcome, err
				}
				if landed {
					outcome = domain.CleanupLanded
				}
				if err := removeRecordedWorktree(ctx, settings, state, path, false); err != nil {
					return outcome, err
				}
				if err := pruneRecordedWorktrees(ctx, settings, state); err != nil {
					return outcome, err
				}
			} else {
				return outcome, errors.New("recorded source and Git common directory are unavailable; refusing to remove a worktree whose local changes cannot be verified; preserve it outside the managed root for manual recovery")
			}
		} else {
			return outcome, errors.New("refusing to remove legacy Git workspace without recorded source-worktree identity; preserve it outside the managed root for manual recovery")
		}
	} else {
		if state.SourceRoot != "" || state.GitCommonDir != "" || state.GitWorktreeDir != "" || state.BaseCommit != "" {
			return outcome, errors.New("recorded Git workspace no longer has its worktree identity; refusing cleanup because local changes cannot be verified; manual recovery is required")
		}
		if err := os.RemoveAll(path); err != nil {
			return outcome, err
		}
	}
	return outcome, nil
}

// permitCommittedRemoval decides whether a worktree safety refusal may be
// overridden. Only one refusal ever can be: an otherwise clean, owned worktree
// whose HEAD is a local commit past the recorded base commit, and only when the
// configured verifier confirms that exact commit as the merged pull request
// head for this issue. It reports whether such a verified landing was the
// reason removal proceeds. Every other refusal, a missing verifier, an
// unverified commit, and any verification error stay fail-closed, so
// uncommitted or untracked changes and unpublished commits are still preserved.
func (l *Local) permitCommittedRemoval(ctx context.Context, issue domain.Issue, refusal error) (bool, error) {
	if refusal == nil {
		return false, nil
	}
	var ahead committedAheadError
	if !errors.As(refusal, &ahead) || l.landing == nil {
		return false, refusal
	}
	landed, err := l.landing.VerifyLanded(ctx, issue, ahead.head)
	if err != nil {
		// The refusal text is preserved so an unverifiable commit is reported and
		// classified exactly like an unpublished one; the verifier logs why.
		return false, fmt.Errorf("%w; merged landing could not be verified", refusal)
	}
	if !landed {
		return false, refusal
	}
	return true, nil
}
