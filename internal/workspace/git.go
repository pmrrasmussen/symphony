package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/hostenv"
)

// gitEnvironment is the environment every git subprocess this package spawns
// runs with. Git is a child Symphony starts like any other, so it gets the same
// filter rather than the daemon's environment whole (PMR-175): most of these
// runners are `git -C <agent worktree>` over state the agent wrote, and git
// reads repository configuration the agent can reach, so a host credential
// inherited here is one an agent-influenced subprocess can be made to carry.
// Git needs none: the one authenticated path Symphony has is
// internal/github's push, which injects its own credential explicitly.
//
// These runners have no session, so they pass no capability.SecretMatcher and
// get filters 1 through 3. settings is threaded to every one of them rather
// than read here, so the filter is applied at the exec site and a runner cannot
// acquire an unfiltered environment by being called from somewhere new.
//
// docs/architecture.md's "The host credential filter" section states why a
// child that only reads local repository state is still a child.
func gitEnvironment(settings config.Settings) []string {
	return hostenv.Filter(os.Environ(), nil, settings, nil)
}

func isGitWorkspace(path string) (bool, error) {
	_, err := os.Lstat(filepath.Join(path, ".git"))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect Git workspace: %w", err)
	}
	return true, nil
}

func gitHead(ctx context.Context, settings config.Settings, path string) (string, error) {
	head, err := gitMetadata(ctx, settings, path, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return "", fmt.Errorf("read Git workspace HEAD: %w", err)
	}
	if head == "" {
		return "", errors.New("read Git workspace HEAD: empty commit")
	}
	return head, nil
}

func ensureGitWorkspaceUnchanged(ctx context.Context, settings config.Settings, path, baseCommit string) error {
	status, err := gitMetadataAllowEmpty(ctx, settings, path, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return fmt.Errorf("inspect Git workspace changes: %w", err)
	}
	if strings.TrimSpace(status) != "" {
		return errors.New("refusing to remove Git workspace with uncommitted or untracked changes")
	}
	head, err := gitHead(ctx, settings, path)
	if err != nil {
		return err
	}
	if head != baseCommit {
		return committedAheadError{head: head, baseCommit: baseCommit}
	}
	return nil
}

// committedAheadError is the single cleanup refusal a verified host landing may
// override: an otherwise clean, owned worktree whose HEAD is a local commit
// past the recorded base commit. Its message is unchanged from the untyped
// error it replaces so the coordinator's fixed status vocabulary and the
// operator documentation keep describing the same refusal.
type committedAheadError struct{ head, baseCommit string }

func (e committedAheadError) Error() string {
	return fmt.Sprintf("refusing to remove Git workspace whose HEAD %s differs from recorded base commit %s", e.head, e.baseCommit)
}

type gitSourceIdentity struct {
	sourceRoot   string
	commonDir    string
	commonDevice uint64
	commonInode  uint64
}

func sourceIdentity(ctx context.Context, settings config.Settings, sourceRoot string) (gitSourceIdentity, error) {
	top, err := gitMetadata(ctx, settings, sourceRoot, "rev-parse", "--path-format=absolute", "--show-toplevel")
	if err != nil {
		return gitSourceIdentity{}, fmt.Errorf("classify workspace source repository: %w", err)
	}
	common, err := gitMetadata(ctx, settings, sourceRoot, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return gitSourceIdentity{}, fmt.Errorf("classify workspace source Git directory: %w", err)
	}
	top, err = canonicalExistingDirectory(top)
	if err != nil {
		return gitSourceIdentity{}, fmt.Errorf("resolve workspace source repository: %w", err)
	}
	common, err = canonicalExistingDirectory(common)
	if err != nil {
		return gitSourceIdentity{}, fmt.Errorf("resolve workspace source Git directory: %w", err)
	}
	device, inode, err := directoryIdentity(common)
	if err != nil {
		return gitSourceIdentity{}, fmt.Errorf("identify workspace source Git directory: %w", err)
	}
	return gitSourceIdentity{sourceRoot: top, commonDir: common, commonDevice: device, commonInode: inode}, nil
}

func directoryIdentity(path string) (uint64, uint64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, 0, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, errors.New("filesystem does not expose stable directory identity")
	}
	return uint64(stat.Dev), uint64(stat.Ino), nil
}

func worktreeIdentity(ctx context.Context, settings config.Settings, path, expectedCommon string) (string, error) {
	common, err := gitMetadata(ctx, settings, path, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", fmt.Errorf("classify created worktree common directory: %w", err)
	}
	common, err = canonicalExistingDirectory(common)
	if err != nil {
		return "", fmt.Errorf("resolve created worktree common directory: %w", err)
	}
	if common != expectedCommon {
		return "", fmt.Errorf("created worktree belongs to Git directory %q, expected %q; manual recovery is required", common, expectedCommon)
	}
	gitDir, err := gitMetadata(ctx, settings, path, "rev-parse", "--path-format=absolute", "--absolute-git-dir")
	if err != nil {
		return "", fmt.Errorf("classify created worktree Git directory: %w", err)
	}
	gitDir, err = canonicalExistingDirectory(gitDir)
	if err != nil {
		return "", fmt.Errorf("resolve created worktree Git directory: %w", err)
	}
	return gitDir, nil
}

func validateWorktreeIdentity(path string, state workspaceState) error {
	if state.GitWorktreeDir == "" || state.GitCommonDir == "" || state.GitCommonDevice == 0 || state.GitCommonInode == 0 {
		return errors.New("refusing to remove Git workspace with incomplete source-worktree identity; preserve it outside the managed root for manual recovery")
	}
	b, err := os.ReadFile(filepath.Join(path, ".git"))
	if err != nil {
		return fmt.Errorf("read owned worktree identity: %w", err)
	}
	line := strings.TrimSpace(string(b))
	if !strings.HasPrefix(line, "gitdir:") {
		return errors.New("owned worktree has an unrecognized .git identity; manual recovery is required")
	}
	gitDir := strings.TrimSpace(strings.TrimPrefix(line, "gitdir:"))
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(path, gitDir)
	}
	abs, err := filepath.Abs(filepath.Clean(gitDir))
	if err != nil {
		return fmt.Errorf("resolve owned worktree identity: %w", err)
	}
	if abs != filepath.Clean(state.GitWorktreeDir) {
		return fmt.Errorf("refusing to remove worktree whose Git directory is %q, not recorded identity %q; manual recovery is required", abs, state.GitWorktreeDir)
	}
	if !below(filepath.Clean(state.GitCommonDir), abs) {
		return errors.New("recorded worktree Git directory is outside its recorded Git common directory; manual recovery is required")
	}
	return nil
}

func gitMetadata(ctx context.Context, settings config.Settings, dir string, args ...string) (string, error) {
	value, err := gitMetadataAllowEmpty(ctx, settings, dir, args...)
	if err != nil {
		return "", err
	}
	if value == "" {
		return "", fmt.Errorf("git %s returned empty metadata", strings.Join(args, " "))
	}
	return value, nil
}

func gitMetadataAllowEmpty(ctx context.Context, settings config.Settings, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = gitEnvironment(settings)
	var stdout, stderr boundedBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, stderr.String())
	}
	value := stdout.String()
	return value, nil
}

// addWorktree fetches baseBranch, defaulting to "main" when the github
// integration is not configured (config.decodeGitHub only applies that same
// default while the integration remains enabled), so a worktree is created
// from the branch github.base_branch actually names -- the same ref
// Session.Publish's descendant check reads -- rather than a literal that
// silently diverges from it (PMR-135).
func (l *Local) addWorktree(ctx context.Context, settings config.Settings, sourceRoot, path, baseBranch string) error {
	if baseBranch == "" {
		baseBranch = "main"
	}
	baseCommit, err := l.fetchBaseCommit(ctx, settings, sourceRoot, baseBranch)
	if err != nil {
		return err
	}
	if err := gitMutation(ctx, settings, sourceRoot, "worktree", "add", "--detach", path, baseCommit); err != nil {
		return fmt.Errorf("create workspace worktree: %w", err)
	}
	return nil
}

// fetchBaseCommit fetches baseBranch into refs/remotes/origin/<baseBranch> in
// sourceRoot's shared Git common directory and resolves its commit, holding
// fetchMu across both calls. Every concurrently-created workspace shares that
// one common directory, so without the lock two workspaces fetching the same
// ref at once can lose a lock race on refs/remotes/origin/<baseBranch>, the
// same hazard fetchMu's doc comment on the Local struct describes (PMR-162).
func (l *Local) fetchBaseCommit(ctx context.Context, settings config.Settings, sourceRoot, baseBranch string) (string, error) {
	l.fetchMu.Lock()
	defer l.fetchMu.Unlock()
	refspec := "+refs/heads/" + baseBranch + ":refs/remotes/origin/" + baseBranch
	if err := gitMutation(ctx, settings, sourceRoot, "fetch", "--no-tags", "origin", refspec); err != nil {
		return "", fmt.Errorf("refresh origin/%s before creating workspace: %w", baseBranch, err)
	}
	baseCommit, err := gitMetadata(ctx, settings, sourceRoot, "rev-parse", "--verify", "refs/remotes/origin/"+baseBranch+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("resolve refreshed origin/%s commit: %w", baseBranch, err)
	}
	return baseCommit, nil
}

func gitRepositoryAvailable(ctx context.Context, settings config.Settings, state workspaceState) (bool, error) {
	if _, err := os.Stat(state.SourceRoot); err == nil {
		identity, identityErr := sourceIdentity(ctx, settings, state.SourceRoot)
		if identityErr != nil {
			return false, fmt.Errorf("validate recorded source repository: %w; manual recovery is required", identityErr)
		}
		if identity.sourceRoot != filepath.Clean(state.SourceRoot) || identity.commonDir != filepath.Clean(state.GitCommonDir) || identity.commonDevice != state.GitCommonDevice || identity.commonInode != state.GitCommonInode {
			return false, errors.New("recorded source path now identifies a different Git repository; manual recovery is required")
		}
		return true, nil
	} else if !os.IsNotExist(err) {
		return false, fmt.Errorf("inspect recorded source repository path: %w", err)
	}
	if common, err := canonicalExistingDirectory(state.GitCommonDir); err == nil {
		device, inode, identityErr := directoryIdentity(common)
		if identityErr != nil {
			return false, identityErr
		}
		if device != state.GitCommonDevice || inode != state.GitCommonInode {
			return false, errors.New("recorded Git common directory now identifies a different repository; manual recovery is required")
		}
		return true, nil
	} else if !os.IsNotExist(err) {
		return false, fmt.Errorf("inspect recorded Git common directory: %w", err)
	}
	return false, nil
}

func removeRecordedWorktree(ctx context.Context, settings config.Settings, state workspaceState, path string, force bool) error {
	args := []string{"worktree", "remove"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, path)
	if err := gitRecordedMutation(ctx, settings, state, args...); err != nil {
		return fmt.Errorf("remove workspace worktree: %w", err)
	}
	return nil
}

func pruneRecordedWorktrees(ctx context.Context, settings config.Settings, state workspaceState) error {
	if err := gitRecordedMutation(ctx, settings, state, "worktree", "prune"); err != nil {
		return fmt.Errorf("prune workspace worktrees: %w", err)
	}
	return nil
}

func gitRecordedMutation(ctx context.Context, settings config.Settings, state workspaceState, args ...string) error {
	if _, err := os.Stat(state.SourceRoot); err == nil {
		identity, identityErr := sourceIdentity(ctx, settings, state.SourceRoot)
		if identityErr != nil || identity.sourceRoot != filepath.Clean(state.SourceRoot) || identity.commonDir != filepath.Clean(state.GitCommonDir) || identity.commonDevice != state.GitCommonDevice || identity.commonInode != state.GitCommonInode {
			return errors.New("recorded source path no longer identifies the expected Git repository; manual recovery is required")
		}
		return gitMutation(ctx, settings, state.SourceRoot, args...)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect recorded source repository: %w", err)
	}
	if _, err := os.Stat(state.GitCommonDir); err != nil {
		return fmt.Errorf("inspect recorded Git common directory: %w", err)
	}
	device, inode, err := directoryIdentity(state.GitCommonDir)
	if err != nil || device != state.GitCommonDevice || inode != state.GitCommonInode {
		return errors.New("recorded Git common directory no longer identifies the expected repository; manual recovery is required")
	}
	cmd := exec.CommandContext(ctx, "git", append([]string{"--git-dir=" + state.GitCommonDir}, args...)...)
	cmd.Env = gitEnvironment(settings)
	var stdout, stderr boundedBuffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, stderr.String())
	}
	return nil
}

func gitMutation(ctx context.Context, settings config.Settings, sourceRoot string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", sourceRoot}, args...)...)
	cmd.Env = gitEnvironment(settings)
	var stdout, stderr boundedBuffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, stderr.String())
	}
	return nil
}
