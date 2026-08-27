// Package status publishes a small, safe runtime snapshot for local operator
// clients. It is deliberately observational: callers can report write errors
// but scheduling never waits for or depends on a snapshot being written.
package status

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/pmrrasmussen/symphony/internal/coordinator"
)

const (
	SchemaVersion   = 1
	DefaultInterval = time.Second
)

// ProcessState lets a reader distinguish a normally running process from its
// final graceful-shutdown record. It is not proof of liveness: readers must
// compare GeneratedAt with their freshness policy and the host process state.
type ProcessState string

const (
	Running  ProcessState = "running"
	Stopping ProcessState = "stopping"
	Stopped  ProcessState = "stopped"
)

// Metadata is fixed at process startup. Only explicit operational paths are
// represented; arbitrary environment values and configuration contents are
// intentionally absent.
type Metadata struct {
	PID          int
	StartedAt    time.Time
	WorkflowPath string
	LogRoot      string
}

// Snapshot is the versioned on-disk contract for local operator clients.
// Coordinator state is already reduced by coordinator.Snapshot and contains
// no issue content, prompt, workspace, protocol, or credential fields.
type Snapshot struct {
	SchemaVersion int                  `json:"schema_version"`
	PID           int                  `json:"pid"`
	StartedAt     time.Time            `json:"process_started_at"`
	GeneratedAt   time.Time            `json:"generated_at"`
	State         ProcessState         `json:"state"`
	WorkflowPath  string               `json:"workflow_path"`
	LogRoot       string               `json:"log_root"`
	Coordinator   coordinator.Snapshot `json:"coordinator"`
}

// Publisher owns a single independently configured status path. Its mutex
// serializes concurrent periodic and shutdown writes while readers observe
// each complete replacement through rename(2).
type Publisher struct {
	path     string
	metadata Metadata
	now      func() time.Time
	mu       sync.Mutex
}

func New(path string, metadata Metadata) (*Publisher, error) {
	if path == "" {
		return nil, errors.New("status snapshot path is empty")
	}
	if metadata.PID <= 0 {
		metadata.PID = os.Getpid()
	}
	if metadata.StartedAt.IsZero() {
		metadata.StartedAt = time.Now()
	}
	return &Publisher{path: filepath.Clean(path), metadata: metadata, now: time.Now}, nil
}

func (p *Publisher) Snapshot(state ProcessState, coordinatorSnapshot coordinator.Snapshot) Snapshot {
	return Snapshot{
		SchemaVersion: SchemaVersion,
		PID:           p.metadata.PID,
		StartedAt:     p.metadata.StartedAt,
		GeneratedAt:   p.now(),
		State:         state,
		WorkflowPath:  p.metadata.WorkflowPath,
		LogRoot:       p.metadata.LogRoot,
		Coordinator:   coordinatorSnapshot,
	}
}

// Write atomically replaces the status file. The containing runtime directory
// and every replacement file are owner-only. If any step fails, a prior
// snapshot remains in place (or no snapshot exists), and the caller can carry
// on without treating status as a scheduler dependency.
func (p *Publisher) Write(state ProcessState, coordinatorSnapshot coordinator.Snapshot) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	dir := filepath.Dir(p.path)
	if err := secureDirectory(dir); err != nil {
		return err
	}

	temporary, err := os.CreateTemp(dir, ".symphony-status-")
	if err != nil {
		return fmt.Errorf("create temporary status snapshot: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("secure temporary status snapshot: %w", err)
	}
	encoder := json.NewEncoder(temporary)
	if err := encoder.Encode(p.Snapshot(state, coordinatorSnapshot)); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("encode status snapshot: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync status snapshot: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close status snapshot: %w", err)
	}
	if err := os.Rename(temporaryPath, p.path); err != nil {
		return fmt.Errorf("replace status snapshot: %w", err)
	}
	return nil
}

// secureDirectory creates a dedicated runtime directory with owner-only
// permissions. An existing directory must already be owner-only: silently
// chmod'ing an arbitrary parent supplied in --status-file could alter a shared
// directory such as a repository root or /tmp.
func secureDirectory(path string) error {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return fmt.Errorf("create status directory: %w", err)
		}
		if err := os.Chmod(path, 0o700); err != nil {
			return fmt.Errorf("secure status directory: %w", err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect status directory: %w", err)
	}
	if !info.IsDir() {
		return errors.New("status directory is not a directory")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return errors.New("status directory must be owner-only")
	}
	return nil
}

// Run publishes immediately and then at a fixed cadence until ctx is done.
// Errors are reported through report and never returned to, or block, the
// coordinator's scheduling path.
//
// Each publish derives its ProcessState from the coordinator snapshot's own
// Stopping flag rather than hardcoding Running, so the periodic publisher
// keeps ticking, and reporting freshly, straight through a graceful shutdown:
// once the coordinator starts draining, the very next tick reports Stopping
// without any coordination from the caller beyond leaving Run running.
//
// A write failure is usually structural -- an owner-only directory
// requirement violated by the operator, say -- so it recurs unchanged on
// every subsequent tick. report is therefore called once per distinct error,
// not once per tick: a fixed condition would otherwise repeat identically for
// the rest of the process's life, at DefaultInterval, forever. Recovery, or a
// change in the failure itself, is reported again.
func (p *Publisher) Run(ctx context.Context, interval time.Duration, source func() coordinator.Snapshot, report func(error)) {
	if interval <= 0 {
		interval = DefaultInterval
	}
	var lastReported string
	publish := func() {
		snapshot := source()
		state := Running
		if snapshot.Stopping {
			state = Stopping
		}
		err := p.Write(state, snapshot)
		if err == nil {
			lastReported = ""
			return
		}
		if report == nil {
			return
		}
		if message := err.Error(); message != lastReported {
			lastReported = message
			report(err)
		}
	}
	publish()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			publish()
		}
	}
}
