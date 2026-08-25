package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/domain"
)

// fakeBackend records what the router asked of it and hands back a session whose
// ID is unique per backend, so a misrouted continuation is visible.
type fakeBackend struct {
	name string

	mu        sync.Mutex
	starts    int
	continues []string
	cancels   []string
	startErr  error
}

func (b *fakeBackend) Start(_ context.Context, _ domain.AgentRequest) (domain.AgentSession, <-chan domain.Event, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.startErr != nil {
		return domain.AgentSession{}, nil, b.startErr
	}
	b.starts++
	events := make(chan domain.Event)
	close(events)
	return domain.AgentSession{ID: b.name + "-session", ThreadID: b.name + "-thread", TurnID: "turn-1"}, events, nil
}

func (b *fakeBackend) Continue(_ context.Context, s domain.AgentSession, _ string) (<-chan domain.Event, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.continues = append(b.continues, s.ID)
	events := make(chan domain.Event)
	close(events)
	return events, nil
}

func (b *fakeBackend) Cancel(_ context.Context, s domain.AgentSession) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cancels = append(b.cancels, s.ID)
	return nil
}

func (b *fakeBackend) observed() (int, []string, []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.starts, append([]string(nil), b.continues...), append([]string(nil), b.cancels...)
}

func settingsFor(backend string) config.Settings {
	s := config.Settings{}
	s.Agent.Backend = backend
	return s
}

func TestStartRoutesToTheConfiguredBackend(t *testing.T) {
	codex, other := &fakeBackend{name: "codex"}, &fakeBackend{name: "other"}
	router := NewRouter(func() config.Settings { return settingsFor("other") },
		map[string]domain.AgentBackend{"codex": codex, "other": other})
	if _, _, err := router.Start(context.Background(), domain.AgentRequest{}); err != nil {
		t.Fatal(err)
	}
	if starts, _, _ := codex.observed(); starts != 0 {
		t.Fatalf("codex started %d sessions despite another backend being selected", starts)
	}
	if starts, _, _ := other.observed(); starts != 1 {
		t.Fatalf("selected backend started %d sessions, want 1", starts)
	}
}

// TestAnUnsetBackendStartsOnCodex is the compatibility case: a workflow written
// before agent.backend existed must behave exactly as it did.
func TestAnUnsetBackendStartsOnCodex(t *testing.T) {
	codex := &fakeBackend{name: "codex"}
	router := NewRouter(func() config.Settings { return config.Settings{} },
		map[string]domain.AgentBackend{"codex": codex})
	if _, _, err := router.Start(context.Background(), domain.AgentRequest{}); err != nil {
		t.Fatal(err)
	}
	if starts, _, _ := codex.observed(); starts != 1 {
		t.Fatalf("codex started %d sessions for an unset backend, want 1", starts)
	}
}

func TestAnUnavailableBackendFailsTheLaunchWithoutFallingBack(t *testing.T) {
	codex := &fakeBackend{name: "codex"}
	router := NewRouter(func() config.Settings { return settingsFor("claude") },
		map[string]domain.AgentBackend{"codex": codex})
	_, _, err := router.Start(context.Background(), domain.AgentRequest{})
	if err == nil {
		t.Fatal("an unavailable backend must fail the launch")
	}
	if !strings.Contains(err.Error(), `"claude"`) || !strings.Contains(err.Error(), "codex") {
		t.Fatalf("error does not name the selection and what is available: %v", err)
	}
	if starts, _, _ := codex.observed(); starts != 0 {
		t.Fatal("an unavailable selection silently fell back to another backend")
	}
}

func TestAFailedStartBindsNothing(t *testing.T) {
	failing := &fakeBackend{name: "codex", startErr: errors.New("spawn failed")}
	router := NewRouter(func() config.Settings { return settingsFor("codex") },
		map[string]domain.AgentBackend{"codex": failing})
	if _, _, err := router.Start(context.Background(), domain.AgentRequest{}); err == nil {
		t.Fatal("expected the start error to surface")
	}
	if _, err := router.Continue(context.Background(), domain.AgentSession{ID: "codex-session"}, "go"); err == nil {
		t.Fatal("a session that never started must not be continuable")
	}
}

// TestReloadDoesNotMoveAnInFlightSessionToAnotherBackend is the pinning
// guarantee: a configuration change between the start and the continuation of a
// run must not hand that run to a backend which has never seen it.
func TestReloadDoesNotMoveAnInFlightSessionToAnotherBackend(t *testing.T) {
	codex, other := &fakeBackend{name: "codex"}, &fakeBackend{name: "other"}
	var calls int
	// The first settings read selects codex; every later read selects another
	// backend, exactly as a mid-run reload would.
	router := NewRouter(func() config.Settings {
		calls++
		if calls == 1 {
			return settingsFor("codex")
		}
		return settingsFor("other")
	}, map[string]domain.AgentBackend{"codex": codex, "other": other})

	session, _, err := router.Start(context.Background(), domain.AgentRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := router.Continue(context.Background(), session, "turn 2"); err != nil {
		t.Fatal(err)
	}
	if _, err := router.Continue(context.Background(), session, "turn 3"); err != nil {
		t.Fatal(err)
	}
	if err := router.Cancel(context.Background(), session); err != nil {
		t.Fatal(err)
	}

	starts, continues, cancels := codex.observed()
	if starts != 1 || len(continues) != 2 || len(cancels) != 1 {
		t.Fatalf("originating backend saw starts=%d continues=%v cancels=%v", starts, continues, cancels)
	}
	for _, id := range append(continues, cancels...) {
		if id != session.ID {
			t.Fatalf("originating backend saw session %q, want %q", id, session.ID)
		}
	}
	if starts, continues, cancels := other.observed(); starts != 0 || len(continues) != 0 || len(cancels) != 0 {
		t.Fatalf("newly configured backend touched an in-flight session: starts=%d continues=%v cancels=%v", starts, continues, cancels)
	}
	// The reported backend for the run stays the one that started it, so the
	// scheduler resolves its policy under the right runtime.
	if got := router.Backend(session); got != "" && got != "other" {
		t.Fatalf("Backend() = %q for a released session", got)
	}
}

func TestBackendReportsTheRuntimeThatOwnsASession(t *testing.T) {
	codex, other := &fakeBackend{name: "codex"}, &fakeBackend{name: "other"}
	selection := "codex"
	router := NewRouter(func() config.Settings { return settingsFor(selection) },
		map[string]domain.AgentBackend{"codex": codex, "other": other})
	session, _, err := router.Start(context.Background(), domain.AgentRequest{})
	if err != nil {
		t.Fatal(err)
	}
	selection = "other"
	if got := router.Backend(session); got != "codex" {
		t.Fatalf("Backend() = %q after the selection changed, want the runtime that started the run", got)
	}
	// An unknown session falls back to the current selection rather than "".
	if got := router.Backend(domain.AgentSession{ID: "unknown"}); got != "other" {
		t.Fatalf("Backend() = %q for an unknown session, want the current selection", got)
	}
}

func TestCancellingAnUnknownSessionIsANoOp(t *testing.T) {
	codex := &fakeBackend{name: "codex"}
	router := NewRouter(func() config.Settings { return settingsFor("codex") },
		map[string]domain.AgentBackend{"codex": codex})
	if err := router.Cancel(context.Background(), domain.AgentSession{ID: "never-started"}); err != nil {
		t.Fatalf("cancelling an unknown session returned %v", err)
	}
	if _, _, cancels := codex.observed(); len(cancels) != 0 {
		t.Fatalf("cancelling an unknown session reached a backend: %v", cancels)
	}
}

// TestCancelReleasesTheBinding keeps the router from growing without bound over
// a long-running process.
func TestCancelReleasesTheBinding(t *testing.T) {
	codex := &fakeBackend{name: "codex"}
	router := NewRouter(func() config.Settings { return settingsFor("codex") },
		map[string]domain.AgentBackend{"codex": codex})
	session, _, err := router.Start(context.Background(), domain.AgentRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if err := router.Cancel(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	router.mu.Lock()
	remaining := len(router.bound)
	router.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("%d session bindings retained after cancellation", remaining)
	}
	if _, err := router.Continue(context.Background(), session, "after cancel"); err == nil {
		t.Fatal("a cancelled session must not be continuable")
	}
}
