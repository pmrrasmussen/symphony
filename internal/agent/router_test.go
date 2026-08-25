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
	if _, _, err := router.Start(context.Background(), domain.AgentRequest{Backend: "claude"}); err == nil {
		t.Fatal("an unavailable requested backend must fail the launch")
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
	// The probe must carry the backend the binding would have been keyed under,
	// or it cannot collide with a wrongly created binding and the assertion is
	// vacuous.
	probe := domain.AgentSession{ID: "codex-session", Backend: "codex"}
	if _, err := router.Continue(context.Background(), probe, "go"); err == nil {
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
}

// TestStartHonorsTheRequestedBackendOverTheCurrentSelection is the fix for the
// two-reads race: the scheduler resolves the command, sandbox, and timeouts for
// one backend, so the router must run that backend rather than resolving the
// selection a second time against a configuration that may have reloaded.
func TestStartHonorsTheRequestedBackendOverTheCurrentSelection(t *testing.T) {
	codex, other := &fakeBackend{name: "codex"}, &fakeBackend{name: "other"}
	router := NewRouter(func() config.Settings { return settingsFor("other") },
		map[string]domain.AgentBackend{"codex": codex, "other": other})
	session, _, err := router.Start(context.Background(), domain.AgentRequest{Backend: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if starts, _, _ := codex.observed(); starts != 1 {
		t.Fatal("the requested backend did not start the session")
	}
	if starts, _, _ := other.observed(); starts != 0 {
		t.Fatal("the configured backend overrode the resolved request")
	}
	// The session is stamped, so no later lookup has to re-derive the runtime.
	if session.Backend != "codex" {
		t.Fatalf("session.Backend=%q, want codex", session.Backend)
	}
	if _, err := router.Continue(context.Background(), session, "turn 2"); err != nil {
		t.Fatal(err)
	}
	if _, continues, _ := codex.observed(); len(continues) != 1 {
		t.Fatal("the stamped session did not resume on its own backend")
	}
}

// TestSessionsAreBoundPerBackendNotByIDAlone matters because session IDs are
// minted by the backends: two runtimes may hand back the same one, and one must
// not be able to steal or release the other's binding.
func TestSessionsAreBoundPerBackendNotByIDAlone(t *testing.T) {
	// Both backends mint the same session ID.
	first, second := &fakeBackend{name: "same"}, &fakeBackend{name: "same"}
	router := NewRouter(func() config.Settings { return config.Settings{} },
		map[string]domain.AgentBackend{"first": first, "second": second})

	firstSession, _, err := router.Start(context.Background(), domain.AgentRequest{Backend: "first"})
	if err != nil {
		t.Fatal(err)
	}
	secondSession, _, err := router.Start(context.Background(), domain.AgentRequest{Backend: "second"})
	if err != nil {
		t.Fatal(err)
	}
	if firstSession.ID != secondSession.ID {
		t.Fatalf("fixture no longer exercises the collision: %q vs %q", firstSession.ID, secondSession.ID)
	}

	if err := router.Cancel(context.Background(), firstSession); err != nil {
		t.Fatal(err)
	}
	if _, _, cancels := first.observed(); len(cancels) != 1 {
		t.Fatal("cancelling the first session did not reach its own backend")
	}
	if _, _, cancels := second.observed(); len(cancels) != 0 {
		t.Fatal("cancelling one session cancelled another backend's identically named session")
	}
	// The other binding survived, so its run can still be continued.
	if _, err := router.Continue(context.Background(), secondSession, "still alive"); err != nil {
		t.Fatalf("cancelling one backend's session released another's binding: %v", err)
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

// TestValidateRefusesARegistryMissingASelectableBackend closes the gap between
// the set of names configuration accepts and the set the process can actually
// run. Without this, a valid configuration passes validation and preflight and
// fails only at the first dispatch.
func TestValidateRefusesARegistryMissingASelectableBackend(t *testing.T) {
	full := map[string]domain.AgentBackend{}
	for _, name := range config.AgentBackends() {
		full[name] = &fakeBackend{name: name}
	}
	if err := Validate(full); err != nil {
		t.Fatalf("a complete registry was refused: %v", err)
	}
	if len(full) < 2 {
		t.Fatal("expected more than one selectable backend")
	}
	for _, name := range config.AgentBackends() {
		partial := map[string]domain.AgentBackend{}
		for other, backend := range full {
			if other != name {
				partial[other] = backend
			}
		}
		err := Validate(partial)
		if err == nil {
			t.Fatalf("a registry missing %q was accepted", name)
		}
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("error does not name the missing backend %q: %v", name, err)
		}
	}
	if err := Validate(nil); err == nil {
		t.Fatal("an empty registry was accepted")
	}
}
