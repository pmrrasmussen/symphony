// Package agent routes agent sessions to the configured backend runtime.
//
// The router is itself a domain.AgentBackend, so coordination keeps depending on
// that one interface and learns nothing about which runtimes exist. Selection
// happens once, when a session starts: the router records which backend created
// the session and sends every later continuation and cancellation to that same
// backend, so changing agent.backend in WORKFLOW.md affects future sessions only
// and an in-flight run is never handed to a runtime that has never seen it.
package agent

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/domain"
)

// Router selects a backend for new sessions and pins existing ones.
type Router struct {
	settings func() config.Settings
	backends map[string]domain.AgentBackend

	mu    sync.Mutex
	bound map[string]domain.AgentBackend
}

// NewRouter builds a router over the named backends. The map is not copied
// defensively because it is assembled once at startup and never mutated.
func NewRouter(settings func() config.Settings, backends map[string]domain.AgentBackend) *Router {
	return &Router{settings: settings, backends: backends, bound: map[string]domain.AgentBackend{}}
}

// Start runs the request on the backend it names, falling back to the configured
// selection only when the caller did not resolve one. Honoring the request is
// what keeps the launch parameters and the runtime consistent: the scheduler
// resolves the command, sandbox, and timeouts for a specific backend, so
// resolving the selection again here could pair one backend's parameters with
// another's runtime if a reload landed in between.
//
// An unavailable selection fails the launch rather than falling back to another
// runtime: a silent fallback would run the issue under a backend the operator
// did not choose.
func (r *Router) Start(ctx context.Context, request domain.AgentRequest) (domain.AgentSession, <-chan domain.Event, error) {
	name := strings.TrimSpace(request.Backend)
	if name == "" {
		name = r.selected()
	}
	backend := r.backends[name]
	if backend == nil {
		return domain.AgentSession{}, nil, fmt.Errorf("agent backend %q is not available (have %s)", name, strings.Join(r.available(), ", "))
	}
	session, events, err := backend.Start(ctx, request)
	if err != nil {
		return session, events, err
	}
	// Stamp the runtime that created the session so every later lookup about
	// this run reads the answer instead of re-deriving it.
	session.Backend = name
	r.mu.Lock()
	r.bound[binding(session)] = backend
	r.mu.Unlock()
	return session, events, nil
}

// binding keys a session by its runtime as well as its ID: session IDs are
// minted by the backends, so two runtimes could mint the same one.
func binding(session domain.AgentSession) string {
	return session.Backend + "\x00" + session.ID
}

// Continue resumes a session on the backend that created it.
func (r *Router) Continue(ctx context.Context, session domain.AgentSession, prompt string) (<-chan domain.Event, error) {
	backend, err := r.pinned(session)
	if err != nil {
		return nil, err
	}
	return backend.Continue(ctx, session, prompt)
}

// Cancel cancels a session on the backend that created it and releases the
// binding. An unknown session is not an error: cancellation is called from
// several paths that may race a completed run, and the backends themselves
// already treat cancelling an unknown session as a no-op.
func (r *Router) Cancel(ctx context.Context, session domain.AgentSession) error {
	r.mu.Lock()
	backend := r.bound[binding(session)]
	delete(r.bound, binding(session))
	r.mu.Unlock()
	if backend == nil {
		return nil
	}
	return backend.Cancel(ctx, session)
}

func (r *Router) pinned(session domain.AgentSession) (domain.AgentBackend, error) {
	r.mu.Lock()
	backend := r.bound[binding(session)]
	r.mu.Unlock()
	if backend == nil {
		return nil, fmt.Errorf("no agent backend holds session %q", session.ID)
	}
	return backend, nil
}

func (r *Router) selected() string {
	if r.settings == nil {
		return ""
	}
	return r.settings().AgentLaunch().Backend
}

func (r *Router) available() []string {
	names := make([]string, 0, len(r.backends))
	for name := range r.backends {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
