package capability

import (
	"context"
	"fmt"
	"strings"

	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/domain"
	githubhost "github.com/pmrrasmussen/symphony/internal/github"
	"github.com/pmrrasmussen/symphony/internal/linear"
)

// Preparer binds this process's host providers into one dispatch's capability
// set. It is the only caller of Build outside a test, and it runs host-side,
// before the session exists (PMR-182).
//
// Where it runs is the whole point. Both backends used to prepare their own,
// which meant the prompt that promises which bounded tools exist was rendered by
// the coordinator from one settings snapshot and the registry that grants them
// was built by the backend from a later one: a reload in between produced a
// promise no session could keep, on an ordinary issue, and the only thing that
// could see it was a launch-time cross-check comparing the rendered prompt
// against the registry. Preparing here, from the snapshot the caller rendered
// that prompt with, removes the divergence instead of detecting it -- and
// removes the second copy of this sequence, which had already drifted once.
//
// The providers are the process-wide ones. Neither belongs to a backend, and a
// nil one leaves its capabilities unbound exactly as an unconfigured integration
// does. See docs/architecture.md's "One GitHub manager per process".
type Preparer struct {
	handoff *linear.Handoff
	github  *githubhost.Manager
}

// NewPreparer binds already-built host providers to the preparation. settings is
// deliberately not held here: the snapshot to prepare from is the caller's, and
// passing it per dispatch is what keeps the registry and the prompt one decision.
func NewPreparer(handoff *linear.Handoff, github *githubhost.Manager) *Preparer {
	return &Preparer{handoff: handoff, github: github}
}

// GitHubManager reports the manager every session this preparer prepares is
// bound to, so the host reads its poll loop and landing-verifier target back out
// of the component that actually binds it rather than keeping a local of its own.
// There is deliberately no constructor that mints a manager here: every such call
// would produce a second linked-pull-request table no poll loop walks.
func (p *Preparer) GitHubManager() *githubhost.Manager {
	if p == nil {
		return nil
	}
	return p.github
}

// Prepare builds one dispatch's provider sessions and capability registry from
// the settings snapshot the caller rendered its prompt with, and returns what
// rides on domain.AgentRequest.
//
// It returns the opaque domain handle rather than *Session because that is what
// the request carries and what the scheduler forwards; From narrows it back at
// the two places that serve it.
//
// The registry is per dispatch and holds the provider session pointers prepared
// here, because every per-run idempotency latch -- landing attempts, the
// resolved-landing latch, a stale-base update -- lives in those pointers. One
// snapshot decides all of it, so which capabilities exist, the config.GitHub the
// session is bound to, and the credentials the child is stripped of cannot
// describe different runs.
func (p *Preparer) Prepare(ctx context.Context, settings config.Settings, issue domain.Issue, workspace string) (domain.SessionCapabilities, error) {
	var handoff *linear.HandoffSession
	var err error
	if p.handoff != nil && settings.LinearSessionCapabilityEnabled() {
		handoff, err = p.handoff.PrepareWithSettings(ctx, settings, issue)
		if err != nil {
			return nil, fmt.Errorf("prepare Linear handoff: %w", err)
		}
	}
	var github *githubhost.Session
	if p.github != nil {
		github = p.github.PrepareWithSettings(settings.GitHub, issue, workspace, handoff)
	}
	// One set of bindings drives both the registry and the secret matcher, so the
	// providers this session can reach and the credentials it strips cannot
	// disagree -- and neither can the two transports, which are handed the result
	// rather than deriving it.
	bindings := Bindings{Settings: settings, Issue: issue, Handoff: handoff, GitHub: github}
	registry := Build(bindings)
	if err := verifyPromise(settings, issue, registry); err != nil {
		return nil, err
	}
	return &Session{registry: registry, secrets: SecretMatcher(bindings, p.github)}, nil
}

// Session is one dispatch's prepared capabilities: the registry both transports
// serve, and the credential matcher built from the same bindings. The two travel
// together because they are two halves of one preparation -- a provider that
// grants a capability also has a credential that must not reach the child.
//
// Its accessors are nil-safe, so a backend given a request with no preparation at
// all reads a nil registry (whose own methods are nil-safe: nothing advertised,
// nothing resolvable, no finalizer) and no matcher, which is exactly the
// unwired-integration behaviour.
type Session struct {
	registry *Registry
	secrets  func(string) bool
}

// Registry is the capability set this session serves.
func (s *Session) Registry() *Registry {
	if s == nil {
		return nil
	}
	return s.registry
}

// SecretMatcher is filter 4 of the host credential filter for this session: it
// recognizes a provider-resolved credential by value, so a launcher can remove an
// inherited variable that carries one even though it has no configured name. It
// is nil when nothing is bound, which every launcher reads as "no provider
// filter". See SecretMatcher and docs/architecture.md's "Filter 4".
func (s *Session) SecretMatcher() func(string) bool {
	if s == nil {
		return nil
	}
	return s.secrets
}

// From narrows what a request carries back to the session the host prepared for
// it. It is the one type assertion in this seam: domain.AgentRequest cannot name
// this type (see domain.SessionCapabilities), so this is what stands in for the
// compiler.
//
// A request carrying nothing yields a nil session, which every accessor above
// answers as "no capability, no matcher" -- the same thing an unwired provider
// produces, and what a backend started outside the scheduler gets. A request
// carrying something else is a wiring error and is refused: silently unbinding
// would start a session whose prompt promises tools it cannot serve, which is the
// exact failure this seam exists to make impossible.
func From(r domain.AgentRequest) (*Session, error) {
	switch prepared := r.Capabilities.(type) {
	case nil:
		return nil, nil
	case *Session:
		return prepared, nil
	default:
		return nil, fmt.Errorf("agent request carries %T as its capabilities, which is not a prepared capability session", r.Capabilities)
	}
}

// verifyPromise refuses a session whose registry cannot keep the delivery mode
// this same settings snapshot renders into the prompt. Three refusals, and they
// are three different failures.
//
// The first is a promise with nothing to serve it. It survives the hoist because
// it is not a snapshot divergence: an issue whose identifier holds no branch-safe
// character has no deterministic branch, so github.Manager prepares no session for
// it and the registry advertises no GitHub capability, while these very settings
// promise host-side publish. Without the refusal the turn runs, the model finds no
// publish tool, and the run ends completed with committed, unpublished work --
// every gate green.
//
// The second is its landing counterpart: a run dispatched in the configured merge
// state is told landing is the whole run, so a session advertising no
// github_land_pr would leave it holding no tool that can merge (PMR-169). Both
// terms read GitHub.LandingDispatch, the one predicate Build advertises on and
// DeliveryInstructions branches on, rather than a paraphrase of it.
//
// The third is the more damaging direction: publish advertised with no handoff
// state to publish into. LinkAndHandoff comments the pull request onto the issue
// and only then discovers it has no target state, so the refusal would land after
// an irreversible GitHub mutation.
//
// All three are transport-neutral, which is why they live here rather than in a
// backend: the promise is in the prompt either transport carries, and the grant is
// in the registry both are handed.
func verifyPromise(s config.Settings, issue domain.Issue, registry *Registry) error {
	landing := s.HostSidePublishPromised() && s.GitHub.LandingDispatch(issue.State)
	serves := registry.advertises(NameGitHubPublishPR)
	if s.HostSidePublishPromised() && !landing && !serves {
		return fmt.Errorf("capability preparation refused: host-side publish is promised for this run but this session advertises no %s capability", NameGitHubPublishPR)
	}
	if landing && !registry.advertises(NameGitHubLandPR) {
		return fmt.Errorf("capability preparation refused: landing is promised for this run but this session advertises no %s capability", NameGitHubLandPR)
	}
	if serves && strings.TrimSpace(s.Tracker.HandoffState) == "" {
		return fmt.Errorf("capability preparation refused: this session advertises %s with no tracker.provider.handoff_state, so a publish would leave the pull request created and the issue untransitioned", NameGitHubPublishPR)
	}
	return nil
}
