package claude

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/pmrrasmussen/symphony/internal/domain"
	"github.com/pmrrasmussen/symphony/internal/mcpbridge"
)

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

// retire retires this turn's own registration and reports what the revocation
// gave up on, if anything, to this turn's stream. It is idempotent: the
// registration's latch performs the revocation once and tells only that caller
// what happened, so the losing call reports nothing.
//
// It retires only this turn's own registration: by the time it runs the session
// may already hold the next turn's, and revoking that one would strip a live
// turn of its authority mid-run.
func (t *turn) retire(s *session) {
	reportRetirement(t.sink, s.retireEndpoint(t.registration))
}

// retireEndpoint revokes whatever registration the session currently holds and
// reports the outcome, which is only ever non-nil when Revoke gave up on an
// invariant it exists to hold: an invocation still in flight when the drain
// expired, or a turn-ended finalizer that had not returned. Both are
// operator-visible facts with no URL, token, argument, or result in them.
//
// The context passed to the revocation is the run's, and it carries values and
// nothing else -- the endpoint derives the finalizer's own budgeted context after
// its drain (see mcpbridge.finalizerBudget). That is also why nothing here has a
// cancel to defer: a defer that fired when the revocation returned could cut a
// still-running transition short.
//
// only, when non-nil, retires that registration and nothing else. The turn's own
// shutdown passes its own registration because by then the session may already
// hold the next turn's, and retiring that one would revoke a live turn's
// authority mid-run.
//
// The nil form means "whatever this session currently holds", so it is only ever
// correct before the caller has stored a registration of its own. Calling it
// after would revoke the turn it just launched, and the child would simply start
// getting 401s with nothing reporting why.
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
