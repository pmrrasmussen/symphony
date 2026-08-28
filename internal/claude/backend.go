// Package claude runs turns on the Claude Code CLI as a Symphony agent backend.
//
// The shape differs from the Codex app-server in one way that drives this whole
// package: `claude --print` runs a single turn and exits. There is no long-lived
// process to send a second turn to, so a continuation spawns a new process and
// resumes the session by ID. Symphony assigns that ID up front rather than
// reading it back, which removes a start-time race and makes the session
// identity known before the child exists.
//
// The launch policy is fixed by launch.go, not configurable, and the only
// confirmation that it applied is the CLI's own init event -- see verifyInit.
//
// One ordering problem is not solved here: the prompt that promises which
// bounded tools exist is rendered by the coordinator before Start is called,
// from a different settings snapshot than the registry that grants them is built
// from. verifyPromises detects rather than removes the problem; the shape that
// removes it -- hoisting capability.Build into the host and passing the
// *Registry on domain.AgentRequest -- touches internal/domain, both backends,
// and the router. docs/architecture.md's opening section states why detecting it
// is not optional.
package claude

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/pmrrasmussen/symphony/internal/capability"
	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/domain"
	githubhost "github.com/pmrrasmussen/symphony/internal/github"
	"github.com/pmrrasmussen/symphony/internal/linear"
	"github.com/pmrrasmussen/symphony/internal/mcpbridge"
)

// maxLine bounds one stdout line. A single assistant message or tool result is
// one line and is routinely large, so an oversized line is normal traffic here
// and is skipped rather than failing the run.
const maxLine = 8 << 20

// eventBuffer leaves room for the terminal event even when a consumer stops
// reading: the coordinator returns as soon as it sees a terminal event, so a
// blocking send afterwards would leak this goroutine and orphan the child.
const eventBuffer = 64

// Backend implements domain.AgentBackend on the Claude Code CLI.
type Backend struct {
	settings    func() config.Settings
	secretNames []string
	// handoff, github, and endpoint are the host-owned providers and the
	// transport that reaches them. All three are process-wide and none of them
	// belongs to this backend: see NewWithProviders.
	handoff  *linear.Handoff
	github   *githubhost.Manager
	endpoint *mcpbridge.Server
	// timer schedules every turn's budget. It defaults to the real one and is
	// replaced only by a test. See Timer.
	timer Timer

	mu       sync.Mutex
	sessions map[string]*session
}

// session is the per-run state that outlives a single turn: the assigned
// session ID, the turn counter, cumulative usage, whichever process is currently
// running, and the capability registry every turn of this run serves.
type session struct {
	id string
	// ctx is the run-lived context Start was given. Capability invocations run on
	// it, never on a turn's or an HTTP request's context: a killed child cancels
	// those instantly, which is exactly the case where aborting a merge already
	// in flight would do the damage.
	//
	// The turn-ended finalizer is the one thing that does not run on it. The
	// coordinator stops a run by cancelling this very context and only then
	// cancelling the session, so it is routinely dead exactly when the deferred
	// Merging -> In Review transition is owed (PMR-95). The endpoint derives that
	// finalizer's context itself, dropping this one's cancellation and keeping its
	// values -- see mcpbridge.finalizerBudget.
	ctx context.Context

	// registry is built once per run and holds the provider session pointers the
	// launcher prepared, because every per-run idempotency latch -- landing
	// attempts, the resolved-landing latch, a stale-base update -- lives in
	// those pointers. It cannot live on a turn: claude --print runs one turn and
	// exits, so a per-turn registry would reset that state on every
	// continuation and a second landing attempt would look like a first.
	// Its type is the endpoint's own narrow view of a registry rather than
	// *capability.Registry, because that is the whole of what this backend ever
	// needs from one: what to advertise, how to resolve a name, and the
	// turn-ended finalizer to run when a turn is over.
	registry mcpbridge.Capabilities
	// advertised is the capability names registry advertised when it was built,
	// frozen here for the same reason the registry is: it decides --tools, while
	// the registry itself answers tools/list, and the two must not be able to
	// disagree. See capabilityEndpoint.
	advertised []string
	// secretMatcher recognizes a provider-resolved credential by value, so it
	// can be removed from the child environment even though it has no configured
	// name and no configured value. See filteredEnv.
	secretMatcher func(string) bool

	mu   sync.Mutex
	turn int
	// request is the first turn's launch request. A resume must re-apply the
	// entire contract -- the CLI restores none of --settings, --mcp-config,
	// --tools, or the permission mode -- and the workspace and Git metadata
	// roots are not derivable from a session ID.
	request domain.AgentRequest
	usage   domain.Usage
	running *turn
	// endpoint is the live capability-endpoint registration, which is per turn:
	// its bearer token authorizes exactly one turn, and it must be retired
	// before the next one is minted. It is held here rather than only on the
	// turn so a hard Cancel and the next turn's launch can both reach it.
	endpoint *registration
}

// New builds a Claude backend with no Symphony capabilities at all. secretNames
// are environment variable names whose values must never reach the child.
func New(settings func() config.Settings, secretNames ...string) *Backend {
	return &Backend{settings: settings, secretNames: secretNames, sessions: map[string]*session{}, timer: realTimer{}}
}

// NewWithProviders binds already-built host providers, and the endpoint that
// serves them, to this backend instead of constructing them.
//
// The providers are the same instances internal/codex is given, and settings
// must be the callback both were built from. Sharing them is not an
// optimization: see docs/architecture.md's "One GitHub manager per process" for
// what a second manager, or a second settings callback, would break.
//
// endpoint is likewise one loopback listener for the daemon's lifetime, shared
// by every concurrent session and separated by per-registration bearer tokens.
//
// A nil provider leaves its capabilities unbound, exactly as an unconfigured
// integration does, and a nil endpoint leaves the session with no reachable
// capability at all -- which is what New produces.
func NewWithProviders(settings func() config.Settings, handoff *linear.Handoff, github *githubhost.Manager, endpoint *mcpbridge.Server, secretNames ...string) *Backend {
	b := New(settings, secretNames...)
	b.handoff = handoff
	b.github = github
	b.endpoint = endpoint
	return b
}

// GitHubManager reports the manager this backend was given, so the host can read
// its poll loop and landing-verifier target back out of a backend rather than
// keeping a local of its own. See codex.Backend.GitHubManager: the one-manager
// invariant is asserted over every wired backend that answers this, so a backend
// that held a manager without exposing it would silently escape the assertion.
func (b *Backend) GitHubManager() *githubhost.Manager { return b.github }

// Start prepares this run's provider sessions and capability registry, assigns a
// session ID, and runs the first turn.
//
// The preparation mirrors codex.Backend.Start deliberately: the same providers,
// the same settings snapshot, the same secret matcher, and the same
// capability.Build call, so there is no second implementation of a capability
// for the two transports to drift apart on. What differs is only how a call is
// framed on the wire, which is the whole point of internal/capability being
// transport-neutral.
//
// One settings snapshot is frozen here for the run's lifetime -- which
// capabilities exist, and the config.GitHub the session is bound to -- because a
// reload mid-run that changed either would leave the registry and the launch
// contract describing different sessions.
func (b *Backend) Start(ctx context.Context, r domain.AgentRequest) (domain.AgentSession, <-chan domain.Event, error) {
	settings := config.Settings{}
	if b.settings != nil {
		settings = b.settings()
	}
	var handoff *linear.HandoffSession
	var err error
	if b.handoff != nil && settings.LinearSessionCapabilityEnabled() {
		handoff, err = b.handoff.PrepareWithSettings(ctx, settings, r.Issue)
		if err != nil {
			return domain.AgentSession{}, nil, fmt.Errorf("prepare Linear handoff: %w", err)
		}
	}
	var githubSession *githubhost.Session
	if b.github != nil {
		githubSession = b.github.PrepareWithSettings(settings.GitHub, r.Issue, r.Workspace, handoff)
	}
	id, err := newSessionID()
	if err != nil {
		return domain.AgentSession{}, nil, err
	}
	// One set of bindings drives both the registry and the secret matcher, so
	// the providers this session can reach and the credentials it strips cannot
	// disagree -- and neither can this backend and internal/codex, which derives
	// both from the same call.
	bindings := capability.Bindings{Settings: settings, Issue: r.Issue, Handoff: handoff, GitHub: githubSession}
	registry := capability.Build(bindings)
	advertised := advertisedNames(registry)
	if err := verifyPromises(settings, r.Prompt, advertised); err != nil {
		return domain.AgentSession{}, nil, err
	}
	s := &session{id: id, ctx: ctx, registry: registry, advertised: advertised,
		secretMatcher: capability.SecretMatcher(bindings, b.github)}
	b.mu.Lock()
	b.sessions[id] = s
	b.mu.Unlock()
	events, err := b.run(ctx, s, r, false)
	if err != nil {
		b.forget(id)
		return domain.AgentSession{}, nil, err
	}
	return domain.AgentSession{ID: id, ThreadID: id, TurnID: "1"}, events, nil
}

// advertisedNames snapshots the registry's advertised capability names. The
// registry is the single source: the names are read out of it rather than
// recomputed from settings, so --tools and the tools/list this same registry
// serves cannot describe different sets.
func advertisedNames(registry mcpbridge.Capabilities) []string {
	definitions := registry.Definitions()
	if len(definitions) == 0 {
		return nil
	}
	names := make([]string, 0, len(definitions))
	for _, definition := range definitions {
		names = append(names, definition.Name)
	}
	return names
}

// verifyPromises is the launch-time consistency guard: it refuses a turn whose
// rendered prompt and whose reachable capability set do not describe the same
// session. It reads r.Prompt and not only settings, and that is the point: the
// prompt was rendered by the coordinator from its own settings snapshot, this
// function runs against a snapshot the backend read later, and no comparison
// made purely within either snapshot sees a reload between the two.
//
// Three refusals, and they are three different failures: a promise with nothing
// to serve it; publish advertised with no handoff state to publish into, which
// is the more damaging direction because LinkAndHandoff mutates GitHub before it
// fails; and a prompt that names an advertised capability without the rule that
// maps it to the name this transport serves it under.
//
// That third check is not "no bare name appears": a bare name is legitimate and
// routine, and refusing every one would refuse every real dispatch. The
// invariant is the conditional one -- a prompt that names an advertised
// capability must also carry the rule that maps it, because the rule and the
// prefixes are emitted together or not at all.
//
// See the package comment, and docs/architecture.md's opening section, for why a
// cross-check is what stands in for a fix here.
func verifyPromises(settings config.Settings, prompt string, advertised []string) error {
	servesPublish := slices.Contains(advertised, capability.NameGitHubPublishPR)
	promisesPublish := settings.HostSidePublishPromised() || strings.Contains(prompt, config.HostSidePublishPromiseMarker)
	if promisesPublish && !servesPublish {
		return fmt.Errorf("claude launch refused: host-side publish is promised for this run but this session advertises no %s capability", capability.NameGitHubPublishPR)
	}
	if servesPublish && strings.TrimSpace(settings.Tracker.HandoffState) == "" {
		return fmt.Errorf("claude launch refused: this session advertises %s with no tracker.provider.handoff_state, so a publish would leave the pull request created and the issue untransitioned", capability.NameGitHubPublishPR)
	}
	if !strings.Contains(prompt, config.MCPNamingRuleMarker) {
		for _, name := range advertised {
			if strings.Contains(prompt, name) {
				return fmt.Errorf("claude launch refused: the rendered prompt names the %s capability with no %s naming rule to map it, so it names a tool this session does not serve", name, config.MCPToolPrefix)
			}
		}
	}
	return nil
}

// Continue resumes the session in a new process. The launch policy is rebuilt
// from the request the caller supplies, because none of it survives a resume:
// the CLI does not restore --settings, --mcp-config, --tools, or the permission
// mode, so every turn must re-apply the whole contract or the boundary silently
// disappears after the first turn.
func (b *Backend) Continue(ctx context.Context, agentSession domain.AgentSession, prompt string) (<-chan domain.Event, error) {
	b.mu.Lock()
	s := b.sessions[agentSession.ID]
	b.mu.Unlock()
	if s == nil {
		return nil, errors.New("unknown claude session")
	}
	s.mu.Lock()
	r := s.request
	s.mu.Unlock()
	r.Prompt = prompt
	return b.run(ctx, s, r, true)
}

// Cancel terminates whichever turn is running and forgets the session. Between
// turns there is no process at all, so an idle session cancels without error --
// unlike a long-lived app-server, where cancellation always has something to
// signal.
func (b *Backend) Cancel(ctx context.Context, agentSession domain.AgentSession) error {
	b.mu.Lock()
	s := b.sessions[agentSession.ID]
	delete(b.sessions, agentSession.ID)
	b.mu.Unlock()
	if s == nil {
		return nil
	}
	s.mu.Lock()
	active := s.running
	s.mu.Unlock()
	// The kill comes first because it is immediate and is what a caller of Cancel
	// needs promptly, while retiring the endpoint can wait on a capability call
	// that is still running. Ordering them the other way would leave a live child
	// for as long as that drain lasts.
	if active != nil {
		active.kill()
	}
	// A hard cancel retires the endpoint here rather than leaving it to the
	// turn's own shutdown, and the reason is narrower than it looks. The turn's
	// shutdown does normally get there: killing the turn closes the parent's
	// pipe ends, so the read loop ends even when a descendant escaped the
	// process group, and in practice its retirement runs microseconds later.
	// What this call adds is that it is ordered before the wait below, so the
	// guarantee holds on the path where that wait does not complete -- ctx is
	// bounded at five seconds by the coordinator, and on that branch Cancel
	// returns having waited for nothing. It also covers the window stream's own
	// LIFO defers open: s.running is cleared before the retirement defer runs,
	// so a cancel arriving in between finds no live turn and would otherwise
	// return with the registration still live.
	//
	// Revoke is idempotent and drains before finalizing, so this races the
	// turn's shutdown safely, only one of the two reports the outcome, and the
	// loser blocks until the winner is done.
	problems := []error{s.retireEndpoint(nil)}
	if active == nil {
		return errors.Join(problems...)
	}
	select {
	case <-active.exited:
	case <-ctx.Done():
		problems = append(problems, ctx.Err())
	}
	return errors.Join(problems...)
}

func (b *Backend) forget(id string) {
	b.mu.Lock()
	delete(b.sessions, id)
	b.mu.Unlock()
}

// run spawns one turn and returns its event stream.
//
// The order here is the part of this wiring that is easiest to get wrong and
// hardest to notice. Every step before spawn has to happen before it:
//
//   - The event channel exists first, because the endpoint registration must be
//     able to deliver a capability's terminal outcome -- a landing that reports
//     waiting or resolved is what ends the logical run -- from the moment the
//     child can call a tool.
//   - The previous turn's registration is retired before this turn's is minted.
//     "Revoked at turn end" is not "revoked before the next turn"; see
//     docs/architecture.md on why two live registrations for one session is the
//     hazard, not a leaked struct.
//   - The registration is minted before spawn, because the CLI connects to its
//     MCP servers before it emits system/init. A token minted afterwards would
//     race the handshake, and losing that race is not an error the child
//     reports: it is a server stuck at "pending", which verifyInit then refuses.
func (b *Backend) run(ctx context.Context, s *session, r domain.AgentRequest, resume bool) (<-chan domain.Event, error) {
	events := &sink{events: make(chan domain.Event, eventBuffer)}
	retired := s.retireEndpoint(nil)
	endpoint, held, err := b.bindEndpoint(s, events)
	if err != nil {
		return nil, errors.Join(err, retired)
	}
	contract, err := launchArgs(r, s.id, resume, endpoint)
	if err != nil {
		s.discardEndpoint(held)
		// retired is joined in rather than dropped: no turn will exist to carry
		// it as a diagnostic, and an expired drain or finalizer has no other
		// route to an operator at all.
		return nil, errors.Join(err, retired)
	}
	environment := filteredEnv(b.secretNames, b.settings, s.secretMatcher, endpoint)
	// The spawn happens under the session lock so a cancellation cannot arrive
	// between the child starting and the session recording it -- that window
	// would kill nothing and leave the process orphaned. The registration is
	// recorded under the same lock, so a Cancel sees both or neither.
	s.mu.Lock()
	s.turn++
	turnNumber := s.turn
	if !resume {
		s.request = r
	}
	t, err := spawn(ctx, r, contract, environment, events, endpoint, b.timer)
	if err != nil {
		s.mu.Unlock()
		// Nothing will ever read this registration's stream, and no turn will
		// end to retire it. Leaving it would be a credential lifetime leak, not
		// a leaked struct: the registration holds the GitHub session, so a
		// loopback-reachable, token-bearing capability set would stay live for
		// the daemon's lifetime.
		s.discardEndpoint(held)
		return nil, errors.Join(err, retired)
	}
	t.registration = held
	s.running = t
	s.endpoint = held
	s.mu.Unlock()

	// The previous turn's revocation may have given up on an ordering invariant.
	// Its own stream is closed by now, so this turn's is where an operator can
	// still see it.
	reportRetirement(events, retired)
	go t.stream(s, r, turnNumber)
	return t.sink.events, nil
}

// newSessionID mints the UUID the CLI is told to use. Assigning it means the
// session identity exists before the child does, so a child that dies before
// announcing itself is still addressable and resumable.
func newSessionID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate claude session id: %w", err)
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16]), nil
}
