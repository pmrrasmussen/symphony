package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/pmrrasmussen/symphony/internal/domain"
)

func (l *Local) recoverPreparation(ctx context.Context, issue domain.Issue, path string, state workspaceState) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if state.Preparation != preparationCreating && state.Preparation != preparationHookPending {
		return fmt.Errorf("workspace preparation phase %q cannot be recovered automatically; manual recovery is required", state.Preparation)
	}
	managedPath, err := l.workspacePath(issue)
	if err != nil {
		return err
	}
	if filepath.Clean(path) != managedPath {
		return errors.New("partial workspace is outside its owned managed path")
	}
	_, statErr := os.Stat(path)
	if statErr == nil {
		git, err := isGitWorkspace(path)
		if err != nil {
			return err
		}
		if git && state.SourceRoot != "" {
			if state.GitWorktreeDir == "" {
				if _, sourceErr := os.Stat(state.SourceRoot); sourceErr != nil {
					return errors.New("partial worktree identity is incomplete and its source repository is unavailable; manual recovery is required")
				}
				state.GitWorktreeDir, err = worktreeIdentity(ctx, path, state.GitCommonDir)
				if err != nil {
					return err
				}
				if err := l.writeState(issue, state); err != nil {
					return err
				}
			}
			if err := validateWorktreeIdentity(path, state); err != nil {
				return err
			}
		}
		if state.SourceRoot != "" {
			available, availableErr := gitRepositoryAvailable(ctx, state)
			if availableErr != nil {
				return availableErr
			}
			if available {
				// Failure is recoverable below: removing the owned path followed by
				// prune reconciles a stale registration left by Git.
				_ = removeRecordedWorktree(ctx, state, path, true)
			}
		}
		if _, err := os.Stat(path); err == nil {
			if err := os.RemoveAll(path); err != nil {
				return fmt.Errorf("remove partial owned workspace: %w", err)
			}
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("inspect partial owned workspace: %w", err)
		}
	} else if !os.IsNotExist(statErr) {
		return fmt.Errorf("inspect partial owned workspace: %w", statErr)
	}
	if state.SourceRoot != "" {
		available, availableErr := gitRepositoryAvailable(ctx, state)
		if availableErr != nil {
			return availableErr
		}
		if available {
			if err := pruneRecordedWorktrees(ctx, state); err != nil {
				return err
			}
		}
	}
	return l.removeState(issue)
}
