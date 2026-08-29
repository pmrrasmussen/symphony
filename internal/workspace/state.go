package workspace

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pmrrasmussen/symphony/internal/domain"
)

const stateDirectory = ".symphony-state"

const (
	legacyWorkspaceStateSchema = "symphony.workspace-state/v1"
	workspaceStateSchema       = "symphony.workspace-state/v2"
)

const (
	preparationCreating    = "creating"
	preparationHookPending = "hook_pending"
	preparationReady       = "ready"
)

// workspaceState is deliberately stored below workspace.root rather than in a
// process-local directory. It is the durable handoff record used after a
// service restart and includes the initial detached-worktree commit so cleanup
// can refuse to delete work that was changed or committed locally.
type workspaceState struct {
	Schema          string `json:"schema,omitempty"`
	IssueID         string `json:"issue_id"`
	Identifier      string `json:"identifier"`
	BaseCommit      string `json:"base_commit,omitempty"`
	Preparation     string `json:"preparation,omitempty"`
	SourceRoot      string `json:"source_root,omitempty"`
	GitCommonDir    string `json:"git_common_dir,omitempty"`
	GitWorktreeDir  string `json:"git_worktree_dir,omitempty"`
	GitCommonDevice uint64 `json:"git_common_device,omitempty"`
	GitCommonInode  uint64 `json:"git_common_inode,omitempty"`
	// CompletedUpdatedAt is decoded only for compatibility with state written by
	// older releases. Completion timestamps no longer affect dispatch and are
	// removed whenever Symphony next writes this state.
	CompletedUpdatedAt *time.Time `json:"completed_updated_at,omitempty"`
}

func (l *Local) workspacePath(issue domain.Issue) (string, error) {
	root, err := l.workspaceRoot()
	if err != nil {
		return "", err
	}
	return workspacePath(root, Key(issue.Identifier))
}

func (l *Local) statePath(issue domain.Issue) (string, error) {
	root, err := l.workspaceRoot()
	if err != nil {
		return "", err
	}
	dir, err := statePath(root)
	if err != nil {
		return "", err
	}
	keySum := sha256.Sum256([]byte(Key(issue.Identifier)))
	path := filepath.Join(dir, fmt.Sprintf("%x.json", keySum[:]))
	if err := regularManagedPath(root, path, "workspace state marker"); err != nil {
		return "", err
	}
	return path, nil
}

func (l *Local) loadState(issue domain.Issue) (workspaceState, bool, error) {
	path, err := l.statePath(issue)
	if err != nil {
		return workspaceState{}, false, err
	}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return workspaceState{}, false, nil
	}
	if err != nil {
		return workspaceState{}, false, fmt.Errorf("read workspace state %q: %w", path, err)
	}
	var state workspaceState
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil {
		return workspaceState{}, false, fmt.Errorf("decode workspace state %q: %w", path, err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return workspaceState{}, false, fmt.Errorf("decode workspace state %q: %w", path, err)
	}
	if state.Schema != "" && state.Schema != legacyWorkspaceStateSchema && state.Schema != workspaceStateSchema {
		return workspaceState{}, false, fmt.Errorf("workspace state %q uses unsupported schema %q; manual recovery is required", path, state.Schema)
	}
	if strings.TrimSpace(state.IssueID) == "" || strings.TrimSpace(state.Identifier) == "" {
		return workspaceState{}, false, fmt.Errorf("workspace state %q is missing required ownership fields; manual recovery is required", path)
	}
	if state.Preparation != "" && state.Preparation != preparationCreating && state.Preparation != preparationHookPending && state.Preparation != preparationReady {
		return workspaceState{}, false, fmt.Errorf("workspace state %q has invalid preparation phase %q; manual recovery is required", path, state.Preparation)
	}
	if state.Schema == legacyWorkspaceStateSchema && (state.Preparation != "" || state.SourceRoot != "" || state.GitCommonDir != "" || state.GitWorktreeDir != "" || state.GitCommonDevice != 0 || state.GitCommonInode != 0) {
		return workspaceState{}, false, fmt.Errorf("workspace state %q mixes v2 recovery fields into the v1 schema; manual recovery is required", path)
	}
	return state, true, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err == io.EOF {
		return nil
	} else if err != nil {
		return err
	}
	return errors.New("workspace state contains multiple JSON values")
}

func validateStateOwner(state workspaceState, issue domain.Issue) error {
	if state.IssueID != issue.ID {
		return fmt.Errorf("workspace state belongs to issue %q, not %q; manual recovery is required", state.IssueID, issue.ID)
	}
	if state.Identifier != issue.Identifier {
		return fmt.Errorf("workspace state identifier is %q, not %q; manual recovery is required", state.Identifier, issue.Identifier)
	}
	return nil
}

func (l *Local) writeState(issue domain.Issue, state workspaceState) error {
	if _, err := l.ensureWorkspaceRoot(); err != nil {
		return err
	}
	path, err := l.statePath(issue)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create workspace state directory: %w", err)
	}
	state.Schema = workspaceStateSchema
	state.CompletedUpdatedAt = nil
	b, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode workspace state: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".state-*")
	if err != nil {
		return fmt.Errorf("create workspace state: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("set workspace state permissions: %w", err)
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("write workspace state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close workspace state: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace workspace state: %w", err)
	}
	return nil
}

func (l *Local) removeState(issue domain.Issue) error {
	path, err := l.statePath(issue)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove workspace state: %w", err)
	}
	return nil
}

func (l *Local) ensureState(ctx context.Context, ws domain.Workspace, issue domain.Issue) error {
	state, found, err := l.loadState(issue)
	if err != nil {
		return err
	}
	if found {
		if err := validateStateOwner(state, issue); err != nil {
			return err
		}
	}
	if !found {
		if !ws.CreatedNow {
			return errors.New("workspace state is missing for an existing workspace; manual recovery is required")
		}
		state = workspaceState{Schema: workspaceStateSchema, IssueID: issue.ID, Identifier: issue.Identifier}
	}
	git, err := isGitWorkspace(ws.Path)
	if err != nil {
		return err
	}
	if git && state.BaseCommit == "" {
		if state.SourceRoot == "" {
			return errors.New("workspace became a Git repository without recorded source-worktree identity; manual recovery is required")
		}
		state.BaseCommit, err = gitHead(ctx, l.settings(), ws.Path)
		if err != nil {
			return err
		}
	}
	state.IssueID = issue.ID
	state.Identifier = issue.Identifier
	if state.Preparation == "" {
		state.Preparation = preparationReady
	}
	return l.writeState(issue, state)
}
