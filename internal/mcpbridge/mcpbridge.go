// Package mcpbridge serves Symphony's agent-neutral capability registry to an
// agent process over MCP, in-process, on one loopback HTTP listener.
//
// Why in-process HTTP. The alternative was a `symphony <helper>` stdio MCP
// server that the agent CLI spawns, bridging back to this process over a unix
// domain socket. Both variants of that -- a private wire format between helper
// and daemon, or MCP itself with the helper as a pure byte pump -- rest on two
// undocumented CLI internals: that the CLI's own sandbox permits connect() on a
// socket path outside its write allowlist, and that MCP stdio servers are
// spawned inside that sandbox at all. The launch contract's doctrine
// (internal/claude/launch.go) is that policy must never depend on something the
// CLI can silently ignore, and a wrong guess here does not surface as an error:
// it surfaces as a 30-second MCP connect timeout. The CLI unquestionably makes
// outbound HTTP from its own process, which needs nothing undocumented to hold.
// The transport is kept deliberately narrow -- one listener, one fixed path, one
// JSON-RPC envelope, no streaming -- so replacing it with a socket later is a
// change confined to this package.
//
// Trust boundary. The agent process is untrusted. It may call any advertised
// tool with any arguments, in parallel, at any time, and it may be killed
// mid-call. Nothing it sends is used for anything except selecting a capability
// by name and handing that capability its own arguments to validate. No provider
// credential crosses the boundary: the only secret the child holds is a
// per-registration bearer token that authorizes it to reach exactly one
// session's registry, and it travels in the child's environment rather than in
// argv, which is world-readable on Linux. All authority stays on this side --
// the registry decides what exists, and each provider re-validates its own
// preconditions inside the invocation.
//
// Nothing in this package logs. Its entire state -- endpoint URL, bearer token,
// tool arguments, tool results -- is exactly what must never reach an operator
// log or a domain.Event, and the CLI's own stream already reports every MCP call
// it makes, so a log record here would add nothing but a disclosure risk.
package mcpbridge

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/pmrrasmussen/symphony/internal/capability"
	"github.com/pmrrasmussen/symphony/internal/domain"
)

const (
	// endpointPath is fixed and carries no secret. An unguessable path would be
	// a credential embedded in a URL, and the URL reaches the child through a
	// command-line argument; the bearer token goes in the child's environment
	// instead, which is owner-only.
	endpointPath = "/mcp"

	// maxBodyBytes bounds one JSON-RPC request. Every request this endpoint
	// serves is a handshake, a listing, or a tool call whose arguments are
	// bounded by the advertised schemas, so a megabyte is already far beyond
	// any legitimate body.
	maxBodyBytes = 1 << 20

	// readHeaderTimeout and maxHeaderBytes are both unset in net/http by
	// default. Without them a client that opens a connection and never finishes
	// a request line holds a server goroutine for the daemon's whole lifetime,
	// and an unbounded header stream is a memory cost the child controls.
	readHeaderTimeout = 10 * time.Second
	maxHeaderBytes    = 64 << 10

	// drainTimeout bounds how long Revoke waits for an in-flight invocation. It
	// is generous on purpose: the longest capability (a landing) makes several
	// provider requests each bounded at 30 seconds plus a bounded Git push, and
	// cutting the wait short is what the drain exists to prevent. It is still a
	// bound, because a wedged provider call must not hold a finished turn open
	// forever.
	drainTimeout = 2 * time.Minute

	// finalizeTimeout bounds the turn-ended finalizer, which needs a bound of
	// its own because it can block before it looks at any context: the GitHub
	// session's finalizer takes that session's mutex as its first statement, and
	// a landing holds the same mutex across Git children that are bounded only
	// by the session context. Without this, Revoke inherits the lifetime of a
	// network-hung git process no matter what the drain does.
	finalizeTimeout = 30 * time.Second

	serverName    = "symphony"
	serverVersion = "0.1.0"
)

// Capabilities is the narrow view of a session's capability registry that this
// endpoint serves: what to advertise, how to resolve a name, and the turn-ended
// finalizer Revoke must run. *capability.Registry satisfies it. Naming the
// surface keeps the transport independent of how a registry is built, and lets
// the endpoint's own behaviour -- serialization, drain ordering, refusal shapes
// -- be exercised without a live provider session.
type Capabilities interface {
	Definitions() []capability.Definition
	Lookup(name string) (capability.Capability, bool)
	TurnEnded(ctx context.Context)
}

// No Go file imports this package yet, so nothing else would notice if the
// registry stopped satisfying the seam: signature drift in internal/capability
// would compile clean here and surface only when a backend is wired to it. This
// assertion is what makes the seam's claim -- that the production registry is
// the interface, and the tests substitute for it rather than the reverse -- hold
// permanently.
var _ Capabilities = (*capability.Registry)(nil)

// bounds are the two waits Revoke performs. They are per-server fields rather
// than referenced constants only so this package's own tests can drive an
// expired bound without a multi-minute wait; nothing outside this package can
// set or observe them, and Listen always installs the constants above.
type bounds struct{ drain, finalize time.Duration }

// Server is the single loopback MCP endpoint. One listener serves every
// concurrent session: capacity is small and sessions are separated by their
// bearer tokens, not by their ports, so the daemon opens exactly one listening
// socket for its lifetime.
type Server struct {
	http   *http.Server
	url    string
	bounds bounds

	mu            sync.Mutex
	closed        bool
	registrations []*Registration
}

// Listen binds the endpoint and starts serving it.
//
// The advertised host is the literal address the listener actually bound, read
// back from the listener and never the name "localhost": Node >= 17 resolves
// names in verbatim DNS order, so "localhost" can yield ::1 first and never
// reach an IPv4-only listener -- and that failure arrives as a 30-second MCP
// connect timeout rather than as a connection error anyone can read.
func Listen() (*Server, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("bind mcp capability endpoint: %w", err)
	}
	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		_ = listener.Close()
		return nil, errors.New("mcp capability endpoint bound a non-TCP address")
	}
	s := &Server{
		url:    "http://" + net.JoinHostPort(addr.IP.String(), strconv.Itoa(addr.Port)) + endpointPath,
		bounds: bounds{drain: drainTimeout, finalize: finalizeTimeout},
	}
	s.http = &http.Server{
		Handler:           http.HandlerFunc(s.serve),
		ReadHeaderTimeout: readHeaderTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
	}
	// Serve only ever returns once the listener is closed, which Close does.
	// There is nothing to report: a dead endpoint makes every capability call
	// fail as a connect error, which the CLI's own stream already surfaces.
	go func() { _ = s.http.Serve(listener) }()
	return s, nil
}

// Close stops serving and unlinks every registration, marking each one inactive
// so nothing can start a further invocation on it.
//
// It deliberately does not revoke them: draining in-flight work and firing a
// registry's turn-ended finalizer belong to the owning session, and doing it
// here would run every session's finalizer on a shutting-down context. A
// registration still live at this point is therefore a missed Revoke, which
// silently leaks that session's deferred Merging -> In Review transition, so
// Close reports the count rather than swallowing it -- the same reasoning that
// makes Register refuse a nil sink at wiring time. Shutdown's own wait for
// active requests is bounded by ctx.
func (s *Server) Close(ctx context.Context) error {
	s.mu.Lock()
	s.closed = true
	unrevoked := s.registrations
	s.registrations = nil
	s.mu.Unlock()
	for _, registration := range unrevoked {
		registration.mu.Lock()
		registration.active = false
		registration.mu.Unlock()
	}
	err := s.http.Shutdown(ctx)
	if len(unrevoked) > 0 {
		return errors.Join(err, fmt.Errorf("mcp capability endpoint closed with %d unrevoked registration(s)", len(unrevoked)))
	}
	return err
}

// Register binds one session's capability registry to a fresh bearer token.
//
// sessionCtx is the context every invocation this registration runs will be
// given. It must outlive the agent's turn, and it is deliberately not the HTTP
// request's context: a killed child closes its connections, net/http cancels the
// request context immediately, and an invocation running on it would abort a
// merge already in flight against GitHub.
func (s *Server) Register(sessionCtx context.Context, capabilities Capabilities, sink func(domain.Event)) (*Registration, error) {
	if sessionCtx == nil {
		return nil, errors.New("mcp registration requires a session context")
	}
	if capabilities == nil {
		return nil, errors.New("mcp registration requires a capability registry")
	}
	if sink == nil {
		// A nil sink would silently swallow terminal capability outcomes -- a
		// resolved or waiting landing -- which are what end the logical run.
		// Refusing at wiring time fails closed instead.
		return nil, errors.New("mcp registration requires an event sink")
	}
	token, err := newToken()
	if err != nil {
		return nil, err
	}
	registration := &Registration{
		server:       s,
		capabilities: capabilities,
		sink:         sink,
		sessionCtx:   sessionCtx,
		url:          s.url,
		token:        token,
		bounds:       s.bounds,
		active:       true,
		idle:         make(chan struct{}, 1),
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errors.New("mcp capability endpoint is closed")
	}
	s.registrations = append(s.registrations, registration)
	return registration, nil
}

// authenticate resolves a presented bearer token to its registration. Every
// registration is compared in constant time and the loop never breaks early, so
// neither the comparison nor the number of comparisons depends on which token
// was presented or how much of it was right.
func (s *Server) authenticate(presented string) *Registration {
	if presented == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var matched *Registration
	for _, registration := range s.registrations {
		if subtle.ConstantTimeCompare([]byte(registration.token), []byte(presented)) == 1 {
			matched = registration
		}
	}
	return matched
}

func (s *Server) remove(target *Registration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, registration := range s.registrations {
		if registration == target {
			s.registrations = append(s.registrations[:i], s.registrations[i+1:]...)
			return
		}
	}
}

// newToken mints a per-registration bearer token. 256 bits from crypto/rand is
// not guessable, and base64url keeps it safe to carry in an HTTP header and in
// an environment variable.
func newToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate mcp endpoint token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// Registration is one session's authorization to reach one capability registry.
// A token resolves to exactly one registration, so a session can never see
// another session's capabilities, provider sessions, or idempotency state.
type Registration struct {
	server       *Server
	capabilities Capabilities
	sink         func(domain.Event)
	sessionCtx   context.Context
	url          string
	token        string
	bounds       bounds

	mu sync.Mutex
	// active is cleared by Revoke before anything is drained, so no further
	// invocation can start once the turn is ending. It deliberately does not
	// gate emitting: see retired.
	active bool
	// retired is set only once Revoke has drained, so a call that was already
	// running when the turn ended can still deliver the outcome it produced.
	// Clearing active would gate that outcome out, which is exactly the event
	// the drain was waiting for.
	retired bool
	// inFlight is the single-invocation gate. See beginCall.
	inFlight bool
	// idle wakes a draining Revoke when an invocation finishes. It is buffered
	// so a finishing call never blocks on a Revoke that is not waiting.
	idle       chan struct{}
	revokeOnce sync.Once
	// revokeErr is written inside revokeOnce and read after it, so every caller
	// of an idempotent Revoke sees the same outcome.
	revokeErr error
}

// URL is the endpoint address to hand the agent. It is the same for every
// registration: the token, not the path, is the credential.
func (g *Registration) URL() string { return g.url }

// Token is the bearer token that authorizes this registration. It belongs in
// the child's environment, and in no log line, event, or command argument.
func (g *Registration) Token() string { return g.token }

// ErrDrainExpired and ErrFinalizerExpired report that Revoke gave up waiting.
// Both mean the registration was retired while work it was supposed to settle
// first was still running, which is a state the caller cannot otherwise see:
// nothing in this package logs, so an unreported expiry would be invisible.
// Neither carries any part of a URL, token, argument, or result.
var (
	// ErrDrainExpired means an invocation was still in flight when the drain
	// bound expired. Its consequences are concrete: the turn-ended finalizer ran
	// concurrently with that invocation, the terminal outcome the invocation is
	// about to produce will be dropped, and its goroutine survives holding the
	// invocation slot until the session context ends.
	ErrDrainExpired = errors.New("mcp registration revoked with an invocation still in flight")
	// ErrFinalizerExpired means the registry's turn-ended finalizer had not
	// returned when its bound expired. It is still running -- see finalize.
	ErrFinalizerExpired = errors.New("mcp registration turn-ended finalizer did not complete")
)

// Revoke retires this registration at the end of an agent turn, in a fixed
// order that the capability state depends on.
//
// First the registration is marked inactive and unlinked, so no new invocation
// can start and a revoked token authenticates against nothing. Then any
// invocation already running is drained -- including the terminal event it
// produces, which is emitted before the invocation releases the slot, so a
// landing's own outcome cannot be lost to the revocation that waited for it.
// Only then does the registry's turn-ended finalizer run, because that finalizer
// performs the deferred Merging -> In Review transition: running it while a
// landing call is still in flight would let the transition interleave with the
// merge it is the fallback for.
//
// The drain honours only its own bound, never ctx: a cancelled context is
// exactly the case where the child was killed mid-call, which is when skipping
// the drain would do the damage.
//
// Revoke is idempotent and always returns within its own bounds. It returns nil
// when it drained and finalized in order, and otherwise ErrDrainExpired,
// ErrFinalizerExpired, or both joined -- each of which means an invariant this
// function exists to hold was knowingly given up on. ctx is the context the
// finalizer runs on, so it must still be live.
func (g *Registration) Revoke(ctx context.Context) error {
	g.revokeOnce.Do(func() {
		g.mu.Lock()
		g.active = false
		g.mu.Unlock()
		g.server.remove(g)
		drained := g.drain()
		g.mu.Lock()
		g.retired = true
		g.mu.Unlock()
		var problems []error
		if !drained {
			problems = append(problems, ErrDrainExpired)
		}
		if !g.finalize(ctx) {
			problems = append(problems, ErrFinalizerExpired)
		}
		g.revokeErr = errors.Join(problems...)
	})
	return g.revokeErr
}

// drain waits for an in-flight invocation to finish, and reports whether it did.
func (g *Registration) drain() bool {
	bound := time.NewTimer(g.bounds.drain)
	defer bound.Stop()
	for {
		g.mu.Lock()
		busy := g.inFlight
		g.mu.Unlock()
		if !busy {
			return true
		}
		select {
		case <-g.idle:
		case <-bound.C:
			return false
		}
	}
}

// finalize runs the registry's turn-ended finalizer under its own bound and
// reports whether it returned inside it.
//
// The finalizer runs on its own goroutine because it can block before it
// consults any context: the GitHub session's finalizer takes that session's
// mutex as its first statement, and a landing holds the same mutex across Git
// children bounded only by the session context. Calling it inline would give
// Revoke the lifetime of a network-hung git process, and passing a bounded or
// already-cancelled ctx would not help, because the block happens before ctx is
// read.
//
// An expired finalizer is not abandoned: it is idempotent, the deferred
// Merging -> In Review transition it may still perform must happen, and the
// session context it runs on will end it. Revoke stops waiting and says so
// instead of waiting for it.
func (g *Registration) finalize(ctx context.Context) bool {
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		g.capabilities.TurnEnded(ctx)
	}()
	bound := time.NewTimer(g.bounds.finalize)
	defer bound.Stop()
	select {
	case <-finished:
		return true
	case <-bound.C:
		return false
	}
}

// errRevoked and errBusy are the two reasons an invocation may not start. Both
// become tool-level refusals: the model can read them and decide what to do.
var (
	errRevoked = errors.New("registration revoked")
	errBusy    = errors.New("invocation already in flight")
)

// beginCall claims the registration's single invocation slot: exactly one
// invocation runs at a time per registration, and a parallel second call is
// refused rather than queued.
//
// This is not what makes concurrent entry safe. Every provider entry point
// behind the registry already holds its own session mutex for the whole call
// (Publish, Context, Land, FinalizeLanding, LandingResolved), so parallel HTTP
// requests would serialize there regardless, and a second parallel landing would
// be declined by the resolved-landing latch it reads on entry.
//
// What the gate buys is the difference between refusing in milliseconds and
// waiting minutes. Without it, a parallel call parks an HTTP goroutine on that
// session mutex behind a Land that can run for several bounded provider requests
// plus a Git push, and then runs against state the first call has already
// changed -- while the model waits, learning nothing. Refusing hands the model
// something it can act on immediately. It also keeps at most one provider
// operation per session outstanding, which is what makes Revoke's drain a single
// bounded wait rather than a race with however many goroutines the child chose
// to open.
//
// Note what this does not cover: Prepare runs before the gate is taken, so
// argument validation is genuinely concurrent here where the Codex transport's
// inline dispatch serialized it. That is harmless only because every Prepare is
// pure parsing. A Prepare that ever touches provider session state must move
// inside the gate.
func (g *Registration) beginCall() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.active {
		return errRevoked
	}
	if g.inFlight {
		return errBusy
	}
	g.inFlight = true
	return nil
}

func (g *Registration) endCall() {
	g.mu.Lock()
	g.inFlight = false
	g.mu.Unlock()
	select {
	case g.idle <- struct{}{}:
	default:
	}
}

// emit forwards a terminal capability outcome to the owning session's sink.
//
// It is gated on retirement, not on activity, and the difference is the whole
// point. A revocation clears active and then waits for the invocation already
// running -- whose terminal outcome is precisely what the consumer is waiting
// for, since a landing that reports waiting or resolved is what ends the logical
// run. Gating on active would drop that event and strand the issue: no delayed
// landing retry, and no deferred transition either, because the provider's own
// finalizer sees the waiting outcome and returns. Retirement is set only after
// the drain, so an in-flight call's outcome always gets through and only a truly
// late event -- one from an invocation that outlived an expired drain, which
// Revoke reports as ErrDrainExpired -- is dropped.
//
// The sink is called without the lock held, because it is the session's code and
// may block.
func (g *Registration) emit(event domain.Event) {
	g.mu.Lock()
	retired := g.retired
	g.mu.Unlock()
	if retired {
		return
	}
	g.sink(event)
}
