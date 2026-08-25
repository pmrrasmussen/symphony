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
package claude

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/pmrrasmussen/symphony/internal/capability"
	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/domain"
	githubhost "github.com/pmrrasmussen/symphony/internal/github"
	"github.com/pmrrasmussen/symphony/internal/linear"
	"github.com/pmrrasmussen/symphony/internal/mcpbridge"
	"github.com/pmrrasmussen/symphony/internal/observability"
)

// maxLine bounds one stdout line. A single assistant message or tool result is
// one line and is routinely large, so an oversized line is normal traffic here
// and is skipped rather than failing the run.
const maxLine = 8 << 20

// eventBuffer leaves room for the terminal event even when a consumer stops
// reading: the coordinator returns as soon as it sees a terminal event, so a
// blocking send afterwards would leak this goroutine and orphan the child.
const eventBuffer = 64

// reservedTerminalSlots keeps space for the terminal event by dropping ordinary
// progress once the buffer is nearly full. One slot is now provably enough,
// because sink.emitTerminal admits exactly one terminal event per turn, but the
// second is kept: it costs one buffered progress event and it is what makes the
// reservation hold even if a turn ever gains a second outcome to report.
const reservedTerminalSlots = 2

// waitDelay bounds Wait's post-exit I/O wait. It is short because by the time it
// applies the process is already gone and only an escaped descendant can still
// be holding a pipe.
const waitDelay = 2 * time.Second

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

	mu       sync.Mutex
	sessions map[string]*session
}

// session is the per-run state that outlives a single turn: the assigned
// session ID, the turn counter, cumulative usage, whichever process is currently
// running, and the capability registry every turn of this run serves.
type session struct {
	id string
	// ctx is the run-lived context Start was given. Capability invocations and
	// the turn-ended finalizer run on it, never on a turn's or an HTTP request's
	// context: a killed child cancels those instantly, which is exactly the case
	// where aborting a merge already in flight would do the damage.
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
	return &Backend{settings: settings, secretNames: secretNames, sessions: map[string]*session{}}
}

// NewWithProviders binds already-built host providers, and the endpoint that
// serves them, to this backend instead of constructing them.
//
// The providers are the same instances internal/codex is given, and sharing them
// is not an optimization. One githubhost.Manager owns the linked pull request
// table its poll loop walks and the exactly-once Linear completion guard, so a
// process holding two would poll one table while sessions write into the other,
// and a merged pull request would complete its issue twice or never. settings
// must likewise be the callback both providers were built from, for the reasons
// codex.NewWithProviders states.
//
// endpoint is the loopback MCP endpoint. It is a parameter rather than something
// this backend binds for itself because it is one listener for the daemon's
// lifetime, shared by every concurrent session and separated by per-registration
// bearer tokens; a listener per backend would be a second socket nothing closes.
//
// A nil provider leaves its capabilities unbound, exactly as an unconfigured
// integration does, and a nil endpoint leaves the session with no reachable
// capability at all -- which is what New produces and what every Claude session
// still gets, because configuration refuses a Claude workflow that enables one.
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
	var secretMatcher func(string) bool
	if handoff != nil || b.github != nil {
		secretMatcher = func(candidate string) bool {
			return handoff != nil && handoff.MatchesSecret(candidate) || githubSession != nil && githubSession.MatchesSecret(candidate)
		}
	}
	id, err := newSessionID()
	if err != nil {
		return domain.AgentSession{}, nil, err
	}
	registry := capability.Build(capability.Bindings{Settings: settings, Issue: r.Issue, Handoff: handoff, GitHub: githubSession})
	s := &session{id: id, ctx: ctx, registry: registry, advertised: advertisedNames(registry), secretMatcher: secretMatcher}
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

// registration is the session's binding to the capability endpoint for one turn:
// the endpoint registration itself, plus the latch that decides which of the
// paths racing to retire it owns reporting what happened.
//
// Three paths can retire the same registration -- the turn's own shutdown, the
// next turn's launch, and a hard Cancel -- and mcpbridge.Revoke is idempotent, so
// without a latch a single expired drain would be reported once per path. The
// losing caller still blocks until the winner has finished draining and
// finalizing, because that is what makes "fully retired before the next
// registration exists" true however the race resolves.
type registration struct {
	bridge endpointRegistration
	once   sync.Once
	err    error
}

// endpointRegistration is the whole of what this backend uses one registration
// for. *mcpbridge.Registration satisfies it. Naming the surface is what makes
// the expiry paths reachable from a test: mcpbridge's drain and finalizer bounds
// are private and are only ever the production constants, so a real registration
// cannot be made to return ErrDrainExpired or ErrFinalizerExpired inside a test's
// patience, and the latch that decides which retirement path owns reporting them
// would otherwise only ever run its nil-error case.
type endpointRegistration interface {
	URL() string
	Token() string
	Revoke(ctx context.Context) error
}

var _ endpointRegistration = (*mcpbridge.Registration)(nil)

// revoke retires the registration and reports whether this caller is the one
// that performed it, and therefore owns reporting err.
func (g *registration) revoke(ctx context.Context) (bool, error) {
	owned := false
	g.once.Do(func() {
		g.err = g.bridge.Revoke(ctx)
		owned = true
	})
	return owned, g.err
}

// reportRetirement forwards an expired revocation to a turn's event stream. Both
// sentinel reasons are fixed strings that name no URL, token, argument, or
// result, so the endpoint's no-logging doctrine survives reporting them -- and
// not reporting them is worse than the risk: nothing in mcpbridge logs, so an
// invariant the code knowingly gave up on would otherwise be invisible.
func reportRetirement(events *sink, expired error) {
	if expired == nil {
		return
	}
	events.emit(domain.Event{Kind: domain.EventDiagnostic, At: time.Now(),
		Message: "claude capability endpoint revocation: " + expired.Error()})
}

// retireEndpoint revokes whatever registration the session currently holds and
// reports the outcome, which is only ever non-nil when Revoke gave up on an
// invariant it exists to hold: an invocation still in flight when the drain
// expired, or a turn-ended finalizer that had not returned. Both are
// operator-visible facts with no URL, token, argument, or result in them.
//
// The finalizer runs on the run-lived session context rather than on a caller's,
// exactly as the Codex transport's does: a Cancel context is bounded at seconds
// and a turn context is already cancelled. That is the better of the available
// contexts, not a live one -- on a hard cancel the coordinator has usually
// already cancelled the run context too, so the deferred transition runs on a
// dead context on this backend exactly as it does on Codex. Fixing that means
// giving a session a context that outlives its run, which is a change to both
// backends and is tracked separately.
//
// only, when non-nil, retires that registration and nothing else. The turn's own
// shutdown passes its own registration because by then the session may already
// hold the next turn's, and retiring that one would revoke a live turn's
// authority mid-run.
func (s *session) retireEndpoint(only *registration) error {
	s.mu.Lock()
	target := s.endpoint
	if only != nil {
		target = only
	}
	s.mu.Unlock()
	if target == nil {
		return nil
	}
	owned, err := target.revoke(s.ctx)
	// The session's slot is cleared only now, after the revocation has finished.
	// Clearing it first would open the window this ordering exists to close: a
	// concurrent launch would find the slot empty, conclude there was nothing to
	// retire, and start the next turn's child while this registration was still
	// draining. Because the slot stays occupied, that launch finds this
	// registration instead and blocks on the same latch until it is fully
	// retired. The compare is what keeps a slow loser from clearing the slot the
	// next turn has since claimed.
	s.mu.Lock()
	if s.endpoint == target {
		s.endpoint = nil
	}
	s.mu.Unlock()
	if !owned {
		// Another path performed the revocation and has already reported this
		// outcome. Reporting it again would double-count one expiry.
		return nil
	}
	return err
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
//     waiting or resolved is what ends the logical run -- and it has to be able
//     to do that from the moment the child can call a tool.
//   - The previous turn's registration is retired before this turn's is minted.
//     "Revoked at turn end" is not "revoked before the next turn": after
//     emitting its terminal event, turn N's goroutine is still waiting on
//     stderr, on Wait, and on the process-group kill, while the coordinator has
//     already called Continue. Registering first would leave two live
//     registrations for one session, so an escaped descendant of turn N could
//     call a capability concurrently with turn N+1 -- against the same provider
//     sessions, whose idempotency latches are all that stand between that and a
//     second landing attempt presenting itself as a first.
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
	t, err := spawn(ctx, r, contract, environment, events, endpoint)
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

// bindEndpoint registers this turn against the capability endpoint and returns
// what the child needs to reach it.
//
// A registration is created whenever this backend has an endpoint at all, not
// only when something is advertised, because the registration is also what runs
// the registry's turn-ended finalizer -- the Codex transport calls that on every
// turn end unconditionally, and an unadvertised capability may still hold state
// that has to be settled. What advertisement decides is only whether the child
// is told the endpoint exists: with nothing advertised the returned
// capabilityEndpoint is nil, so no --mcp-config is rendered and no token reaches
// the child, and the registration is unreachable by construction.
func (b *Backend) bindEndpoint(s *session, events *sink) (*capabilityEndpoint, *registration, error) {
	if b.endpoint == nil {
		return nil, nil, nil
	}
	bridge, err := b.endpoint.Register(s.ctx, s.registry, events.emit)
	if err != nil {
		return nil, nil, fmt.Errorf("register claude capability endpoint: %w", err)
	}
	held := &registration{bridge: bridge}
	if len(s.advertised) == 0 {
		return nil, held, nil
	}
	return &capabilityEndpoint{url: bridge.URL(), token: bridge.Token(), names: s.advertised}, held, nil
}

// discardEndpoint retires a registration for a turn that never started. There is
// no stream to report an expiry on and nothing was ever in flight to drain, so
// the outcome is deliberately dropped rather than reported: the caller is
// already returning the launch failure that explains it.
func (s *session) discardEndpoint(held *registration) {
	if held == nil {
		return
	}
	_, _ = held.revoke(s.ctx)
}

// turn is one child process.
type turn struct {
	cmd    *exec.Cmd
	stdout io.ReadCloser
	stderr io.ReadCloser
	exited chan struct{}

	timeout time.Duration

	// killOnce bounds the process-group signal to one delivery per turn. The
	// group kill deliberately bypasses Go's post-Wait guard, so repeating it
	// after the child is reaped could signal an unrelated group once the pid is
	// recycled.
	killOnce sync.Once
	// closeIO closes the parent's ends of the pipes exactly once.
	closeIO sync.Once

	// sink is the only route from any goroutine to this turn's event channel. It
	// belongs to the turn rather than to stream's locals because the read loop is
	// not the only thing that can have something to report about a turn, and it is
	// built before the turn so the capability endpoint can be given it before the
	// child that reaches that endpoint exists.
	sink *sink

	// contract is what this turn was launched under, carried so verifyInit checks
	// the echo against the argument vector that produced it. See launchContract.
	contract launchContract
	// registration is this turn's capability-endpoint authority, retired when the
	// turn ends however it ends. It is nil for a turn with no endpoint.
	registration *registration
	// endpointURL and endpointToken are kept only so child output can be
	// scrubbed of them. See withoutEndpoint.
	endpointURL   string
	endpointToken string

	mu     sync.Mutex
	killed bool
}

// spawn starts the CLI with the prompt on stdin and a scrubbed environment.
func spawn(ctx context.Context, r domain.AgentRequest, contract launchContract, environment []string, events *sink, endpoint *capabilityEndpoint) (*turn, error) {
	command := strings.TrimSpace(r.Command)
	if command == "" {
		command = "claude"
	}
	// The command is a configured program name, and the arguments are built
	// here, so they are passed directly rather than through a shell: the
	// settings payload is JSON and must not be word-split or expanded.
	fields := strings.Fields(command)
	cmd := exec.CommandContext(ctx, fields[0], append(fields[1:], contract.args...)...)
	cmd.Dir = r.Workspace
	cmd.Env = environment
	// A process group is what makes cancellation reach the CLI's own children:
	// a killed turn can leave background shell commands behind otherwise.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// Go closes a command's parent-side pipes in Start and Wait, so a failure
	// between creating one and reaching Start would leak the descriptors -- and
	// this only happens under descriptor exhaustion, where a leak compounds.
	var opened []io.Closer
	closeOpened := func() {
		for _, pipe := range opened {
			_ = pipe.Close()
		}
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	opened = append(opened, stdout)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		closeOpened()
		return nil, err
	}
	opened = append(opened, stderr)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		closeOpened()
		return nil, err
	}
	opened = append(opened, stdin)
	t := &turn{
		cmd: cmd, stdout: stdout, stderr: stderr, exited: make(chan struct{}), timeout: r.TurnTimeout,
		sink: events, contract: contract,
	}
	if endpoint != nil {
		t.endpointURL, t.endpointToken = endpoint.url, endpoint.token
	}
	cmd.Cancel = func() error { t.kill(); return nil }
	// WaitDelay bounds how long Wait blocks on I/O after the process itself is
	// gone, so a descendant still holding an inherited pipe cannot keep Wait
	// from returning.
	cmd.WaitDelay = waitDelay
	if err := cmd.Start(); err != nil {
		closeOpened()
		return nil, err
	}
	// The prompt goes on stdin, never in the arguments: several launch flags are
	// variadic and would swallow a trailing positional prompt. Closing stdin is
	// required either way -- the CLI waits on it before proceeding.
	go func() {
		_, _ = io.WriteString(stdin, r.Prompt)
		_ = stdin.Close()
	}()
	return t, nil
}

func (t *turn) killProcessGroup() error {
	if t.cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-t.cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

// kill terminates the turn. Killing the process group is not sufficient on its
// own: a descendant that leaves the group -- setsid, nohup, any double fork --
// keeps the inherited stdout write end open, so the reader would never see EOF
// and the turn would hang with no terminal event and no closed channel. Closing
// the parent's ends of the pipes is what actually ends the read.
func (t *turn) kill() {
	t.mu.Lock()
	t.killed = true
	t.mu.Unlock()
	t.killOnce.Do(func() { _ = t.killProcessGroup() })
	t.closePipes()
}

func (t *turn) closePipes() {
	t.closeIO.Do(func() {
		_ = t.stdout.Close()
		_ = t.stderr.Close()
	})
}

// withoutEndpoint removes this turn's endpoint address and bearer token from
// child output before any of it becomes an event.
//
// observability.Text redacts credential-shaped text -- a Bearer header, a
// token= parameter -- and a loopback URL is neither, so a CLI that prints its
// MCP configuration or an MCP connect error to stderr would otherwise put the
// endpoint address in a diagnostic and a log. The address is not a credential
// on its own, since the token is what authorizes anything, but "the endpoint
// URL and token appear in no log line or event" is the property this endpoint
// was built to hold and it costs nothing to keep.
func (t *turn) withoutEndpoint(text string) string {
	if text == "" {
		return ""
	}
	for _, secret := range []string{t.endpointToken, t.endpointURL} {
		if secret != "" {
			text = strings.ReplaceAll(text, secret, "[redacted]")
		}
	}
	return text
}

func (t *turn) cancelled() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.killed
}

// stream reads the turn's stdout, normalizes it, and ends the turn's event
// stream. At most one terminal event reaches a consumer, however many goroutines
// had something to report -- the sink's latch, not this loop, is what says so.
func (t *turn) stream(s *session, r domain.AgentRequest, turnNumber int) {
	// The channel is closed by the sink, from inside the sink's own mutex, so
	// there is no ordering here to get wrong and no window in which an emit can
	// find the channel closed. Closing it here instead would reintroduce one.
	defer t.sink.close()
	defer close(t.exited)
	// Retiring this turn's endpoint registration is deferred after both of those
	// so it runs before them. Before the stream closes, because the revocation
	// drains an invocation that may still be running and the terminal event that
	// invocation produces has to reach this stream. Before t.exited closes,
	// because that is what a hard Cancel waits on, so a Cancel that returns has
	// either retired this registration itself or waited for this to.
	//
	// It retires only this turn's own registration: by now the session may
	// already hold the next turn's, and revoking that one would strip a live
	// turn of its authority mid-run.
	defer func() { reportRetirement(t.sink, s.retireEndpoint(t.registration)) }()
	// Once this turn is over it must stop being the session's live process, or a
	// later cancellation would signal a process group whose pid has been reaped
	// and possibly recycled.
	defer func() {
		s.mu.Lock()
		if s.running == t {
			s.running = nil
		}
		s.mu.Unlock()
	}()

	emit := t.sink.emit
	pending := map[string]pendingCall{}

	// The turn budget is enforced here rather than by the context, so the
	// timeout is reported as a normalized failure instead of an opaque kill.
	var timer *time.Timer
	timedOut := make(chan struct{})
	if t.timeout > 0 {
		timer = time.AfterFunc(t.timeout, func() {
			close(timedOut)
			// kill closes the pipes as well, which is what unblocks the read
			// loop below and lets this turn report its own timeout.
			t.kill()
		})
		defer timer.Stop()
	}

	stderr := &boundedTail{}
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		stderr.readFrom(t.stderr)
	}()

	lines := newLineReader(t.stdout)
	var initVerified bool
	var readErr error
	for {
		line, skipped, err := lines.next()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				readErr = err
			}
			break
		}
		if skipped {
			// An over-long line is discarded, but reading continues: stopping
			// would block the child on a full pipe and hang the turn.
			continue
		}
		envelope, ok := decode(line)
		if !ok {
			// Undecodable output is skipped too. It is the child's output, and
			// one bad line must not end a run that is otherwise progressing.
			continue
		}
		switch envelope.Type {
		case "system":
			switch envelope.Subtype {
			case "init":
				var event initEvent
				_ = json.Unmarshal(line, &event)
				if refusal := verifyInit(event, r.Workspace, t.contract); refusal != "" {
					// The policy did not apply. Fail closed rather than run a
					// turn under an unknown boundary.
					// Two refused init lines can arrive in a single read, and
					// killing the child does not discard what is already
					// buffered, so the latch is what keeps this to one failure.
					t.sink.emitTerminal(domain.Event{Kind: domain.EventFailed, At: time.Now(), Message: refusal})
					t.kill()
					continue
				}
				initVerified = true
				emit(domain.Event{
					Kind: domain.EventSessionStarted, At: time.Now(),
					SessionID: s.id, ThreadID: s.id, TurnID: strconv.Itoa(turnNumber),
					PID: t.cmd.Process.Pid,
				})
			case "permission_denied":
				var event permissionDeniedEvent
				_ = json.Unmarshal(line, &event)
				emit(domain.Event{
					Kind: domain.EventItem, At: time.Now(),
					ItemID:   observability.Text(event.ToolUseID),
					ItemType: itemType(event.ToolName),
					ToolName: observability.Text(event.ToolName),
					Outcome:  domain.ItemDeclined,
				})
			}
		case "assistant":
			var message assistantMessage
			_ = json.Unmarshal(line, &message)
			for _, content := range message.Message.Content {
				if content.Type != "tool_use" || content.ID == "" {
					continue
				}
				pending[content.ID] = pendingCall{tool: content.Name, started: time.Now()}
				emit(domain.Event{
					Kind: domain.EventItem, At: time.Now(),
					ItemID:   observability.Text(content.ID),
					ItemType: itemType(content.Name),
					ToolName: observability.Text(content.Name),
					Outcome:  domain.ItemStarted,
				})
			}
		case "user":
			var message userMessage
			_ = json.Unmarshal(line, &message)
			for _, content := range message.Message.Content {
				if content.Type != "tool_result" || content.ToolUseID == "" {
					continue
				}
				call, known := pending[content.ToolUseID]
				delete(pending, content.ToolUseID)
				outcome := domain.ItemCompleted
				if content.IsError {
					outcome = domain.ItemFailed
				}
				event := domain.Event{
					Kind: domain.EventItem, At: time.Now(),
					ItemID:   observability.Text(content.ToolUseID),
					ItemType: itemType(call.tool),
					ToolName: observability.Text(call.tool),
					Outcome:  outcome,
				}
				if known {
					event.DurationMs = time.Since(call.started).Milliseconds()
				}
				emit(event)
			}
		case "rate_limit_event":
			var event rateLimitEvent
			_ = json.Unmarshal(line, &event)
			// This is reported as a diagnostic rather than EventRateLimit on
			// purpose. The scheduler normalizes rate-limit payloads through a
			// fixed numeric allowlist (limit, remaining, used, reset_seconds,
			// window_seconds); the CLI's actionable fields are strings under
			// different names, so an EventRateLimit here would be silently
			// discarded and never reach a log.
			emit(domain.Event{Kind: domain.EventDiagnostic, At: time.Now(),
				Message: "claude reported a rate limit: " + observability.Text(firstNonEmpty(event.RateLimitInfo.Status, "unspecified")) +
					" (" + observability.Text(firstNonEmpty(event.RateLimitInfo.RateLimitType, "unspecified")) + ")"})
		case "result":
			if t.sink.settled() {
				// Something already ended this turn -- a refused init, or a
				// terminal event raised off this loop. Reporting the result too
				// would emit a second terminal event and misreport the reason.
				// This is only a shortcut; emitTerminal below is what enforces it.
				continue
			}
			var event resultEvent
			_ = json.Unmarshal(line, &event)
			// The CLI reports usage per turn while the scheduler keeps a
			// component-wise maximum across a run, so the running total is
			// accumulated here.
			s.mu.Lock()
			s.usage = add(s.usage, event.Usage.totals())
			total := s.usage
			s.mu.Unlock()
			if total != (domain.Usage{}) {
				emit(domain.Event{Kind: domain.EventUsage, At: time.Now(), Usage: total})
			}
			for _, denial := range event.PermissionDenials {
				emit(domain.Event{Kind: domain.EventDiagnostic, At: time.Now(),
					Message: "claude denied a tool call: " + observability.Text(denial.ToolName)})
			}
			if event.IsError {
				// is_error is the authoritative failure signal: an
				// authentication failure arrives with subtype "success".
				t.sink.emitTerminal(domain.Event{Kind: domain.EventFailed, At: time.Now(),
					Message: "claude turn failed: " + observability.Text(firstNonEmpty(event.TerminalReason, event.APIErrorStatus, event.StopReason, "unspecified"))})
				continue
			}
			if !initVerified {
				// A turn that never announced its policy is not a turn whose
				// boundary is known.
				t.sink.emitTerminal(domain.Event{Kind: domain.EventFailed, At: time.Now(), Message: "claude session refused: no init event was reported"})
				continue
			}
			t.sink.emitTerminal(domain.Event{Kind: domain.EventCompleted, At: time.Now()})
		}
	}
	<-stderrDone
	waitErr := t.cmd.Wait()
	// Kill the group again: the leader can exit while descendants still hold
	// inherited pipes.
	_ = t.killProcessGroup()

	// The loop ended without a terminal event, so this is the last chance to
	// report why -- unless this turn's outcome was already reported elsewhere.
	if t.sink.settled() {
		return
	}
	select {
	case <-timedOut:
		t.sink.emitTerminal(domain.Event{Kind: domain.EventFailed, At: time.Now(), Message: "claude turn timeout"})
		return
	default:
	}
	if t.cancelled() {
		t.sink.emitTerminal(domain.Event{Kind: domain.EventFailed, At: time.Now(), Message: "claude turn cancelled"})
		return
	}
	if tail := t.withoutEndpoint(stderr.text()); tail != "" {
		emit(domain.Event{Kind: domain.EventDiagnostic, At: time.Now(), Message: tail})
	}
	switch {
	case readErr != nil:
		t.sink.emitTerminal(domain.Event{Kind: domain.EventFailed, At: time.Now(), Message: "claude stdout read failed"})
	case waitErr != nil:
		t.sink.emitTerminal(domain.Event{Kind: domain.EventFailed, At: time.Now(), Message: "claude exited without completing the turn: " + exitText(waitErr)})
	default:
		t.sink.emitTerminal(domain.Event{Kind: domain.EventFailed, At: time.Now(), Message: "claude exited without reporting a result"})
	}
}

type pendingCall struct {
	tool    string
	started time.Time
}

func add(a, b domain.Usage) domain.Usage {
	return domain.Usage{
		InputTokens:  a.InputTokens + b.InputTokens,
		OutputTokens: a.OutputTokens + b.OutputTokens,
		TotalTokens:  a.TotalTokens + b.TotalTokens,
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// exitText reports an exit status without the child's own output.
func exitText(err error) string {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return "exit status " + strconv.Itoa(exit.ExitCode())
	}
	return "process error"
}

// sink owns a turn's event channel outright: every send, the terminal latch, and
// the close itself happen under one mutex. Nothing about a turn's event stream is
// left to an invariant about which goroutine does what.
//
// That the mutex is the guard, and not the select/default idiom the sends use, is
// the whole point: default covers a full channel, not a closed one, and a send on
// a closed channel panics unrecoverably and process-wide -- one late event would
// kill every parallel session, not just its own turn. Because the close happens
// here too, there is no state in which the channel is closed and the sink is not.
//
// No send ever blocks. A consumer stops reading as soon as it sees a terminal
// event, so ordinary progress is dropped once the buffer is nearly full and the
// terminal event keeps the reserved room.
type sink struct {
	mu       sync.Mutex
	events   chan domain.Event
	closed   bool
	terminal bool
}

// emit reports progress. A terminal event handed to it still goes through the
// latch, so there is no path to an unclaimed outcome.
func (s *sink) emit(event domain.Event) {
	if terminal(event.Kind) {
		s.emitTerminal(event)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || len(s.events) >= cap(s.events)-reservedTerminalSlots {
		return
	}
	s.sendLocked(event)
}

// emitTerminal reports the turn's one outcome and reports whether this caller is
// the one that settled it. Claiming and sending are a single operation on
// purpose: a claim that could not deliver its event would leave the turn settled
// with nothing to show for it, and the consumer would then see the stream close
// with no outcome and report "closed before completion" instead of the real
// reason. A second outcome is no better -- Coordinator.consume returns on the
// first, so the run would be recorded as finished under that reason while the
// child kept burning tokens until a cancellation arrived.
func (s *sink) emitTerminal(event domain.Event) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.terminal {
		return false
	}
	s.terminal = true
	s.sendLocked(event)
	return true
}

// settled reports whether the turn's outcome is already spoken for. It is only
// ever a shortcut -- emitTerminal is what enforces the latch -- so a caller may
// act on it, but must not treat a false as a reservation.
func (s *sink) settled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.terminal
}

// close ends the stream. It closes the channel under the mutex, so an emit is
// either already done or sees closed; neither can be mid-send.
func (s *sink) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	close(s.events)
}

func (s *sink) sendLocked(event domain.Event) {
	select {
	case s.events <- event:
	default:
	}
}

func terminal(kind domain.EventKind) bool {
	switch kind {
	case domain.EventCompleted, domain.EventFailed, domain.EventBlocked,
		domain.EventLandingWaiting, domain.EventLandingResolved:
		return true
	}
	return false
}

// boundedTail keeps only the last bounded, redacted slice of stderr, so a noisy
// child cannot flood a log and no unbounded child output is retained.
type boundedTail struct {
	mu   sync.Mutex
	tail []byte
}

func (b *boundedTail) readFrom(r io.Reader) {
	buf := make([]byte, 4<<10)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			b.mu.Lock()
			b.tail = append(b.tail, buf[:n]...)
			if len(b.tail) > observability.MaxDiagnosticBytes {
				b.tail = b.tail[len(b.tail)-observability.MaxDiagnosticBytes:]
			}
			b.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

func (b *boundedTail) text() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.tail) == 0 {
		return ""
	}
	return observability.Text(string(b.tail))
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
