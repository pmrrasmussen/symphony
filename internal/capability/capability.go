// Package capability holds Symphony's bounded session capabilities in an
// agent-neutral form: what is advertised to an agent, how wire arguments are
// validated, how an invocation runs, and what typed result or refusal it
// produces. Nothing here knows how a particular agent transport frames a tool
// call or writes a response back, so a second backend can reuse the same
// definitions, scope checks, and refusals without importing the Codex adapter.
//
// A registry is built per session and holds the same provider session pointers
// the launcher prepared, because all per-run idempotency state (landing
// attempts, the resolved-landing latch, a stale-base update) lives in those
// provider sessions. A process-wide registry would share or reset that state.
package capability

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/domain"
	githubhost "github.com/pmrrasmussen/symphony/internal/github"
	"github.com/pmrrasmussen/symphony/internal/linear"
)

// Capability names are registry-owned constants. They are the only capability
// strings that may reach a log or an event: the "tool" value decoded from an
// agent's call is never logged, so a hostile name cannot be echoed anywhere.
const (
	NameCreateFollowupIssue = "create_followup_issue"
	NameGitHubPublishPR     = "github_publish_pr"
	NameGitHubPRContext     = "github_pr_context"
	NameGitHubLandPR        = "github_land_pr"
)

// Definition is the transport-neutral advertisement of one capability. The
// schema is plain JSON Schema data; wrapping it in a protocol envelope is the
// adapter's job.
type Definition struct {
	Name        string
	Description string
	InputSchema map[string]any
}

// Result is a successful invocation's typed outcome. Payload is encoded by the
// adapter, so a capability never chooses a wire format. Terminal is set only
// when the outcome ends the whole run, and Reason is then a fixed or
// configuration-derived, bounded, secret-free string owned by the provider.
type Result struct {
	Success  bool
	Payload  any
	Terminal domain.EventKind
	Reason   string
}

// Failure is a non-terminal refusal. Message is returned to the agent verbatim
// and must stay generic: no provider error, issue data, or credential-derived
// value belongs in it. Outcome is the item outcome to record when the refusal
// happens after the call was already reported as started.
type Failure struct {
	Message string
	Outcome string
}

// Invocation is a validated, ready-to-run capability call.
type Invocation func(ctx context.Context) (Result, *Failure)

// Capability is one bounded, session-scoped operation.
type Capability interface {
	Definition() Definition
	// Lifecycle reports whether invocations are reported as dynamicToolCall
	// item records.
	Lifecycle() bool
	// Prepare validates wire arguments before any observable work happens and
	// binds the invocation. A refusal here precedes the call, so it is never
	// reported as one.
	Prepare(arguments json.RawMessage) (Invocation, *Failure)
}

// unsupported is the refusal for an unknown capability, and for arguments that
// are not even shaped like a call this capability accepts. It deliberately
// reveals nothing about what is configured.
func unsupported() *Failure {
	return &Failure{Message: "Unsupported client-side tool.", Outcome: domain.ItemFailed}
}

// decodeNoInput accepts only an empty JSON object, which is how every
// zero-argument capability is declared. Anything else -- absent arguments, a
// non-object, or any field at all -- is refused before the capability runs.
func decodeNoInput(arguments json.RawMessage) *Failure {
	var fields map[string]json.RawMessage
	if json.Unmarshal(arguments, &fields) != nil || len(fields) != 0 {
		return unsupported()
	}
	return nil
}

// entry pairs a capability with whether this session advertises it.
//
// Advertisement and dispatch are deliberately distinct here. Advertising is a
// coarse filter over what an agent is told about; Lookup stays open to every
// capability the session is bound to, because the provider re-validates its own
// preconditions immediately before mutating anything. Narrowing Lookup to the
// advertised set would move authority into the advertisement and skip that
// re-validation.
//
// That is a statement about this registry, not about every transport over it. It
// holds where advertisement is the agent's only route to a call, which is true of
// the Codex app-server. It is not true of the MCP endpoint, whose address and
// token the agent's own shell holds, so internal/mcpbridge refuses an
// unadvertised name at the transport instead -- see Registration.advertises. The
// re-validation above is why that refusal closes an observability gap rather
// than an authority hole.
type entry struct {
	capability Capability
	advertised bool
}

// Registry is the per-session set of capabilities.
type Registry struct {
	entries []entry
	github  *githubhost.Session
}

// Bindings are the session-scoped inputs that decide which capabilities exist.
type Bindings struct {
	Settings config.Settings
	Issue    domain.Issue
	Handoff  *linear.HandoffSession
	GitHub   *githubhost.Session
}

// Build assembles the capabilities for one session. Order is stable and is part
// of the advertised contract.
func Build(b Bindings) *Registry {
	r := &Registry{github: b.GitHub}
	if b.Handoff != nil {
		r.entries = append(r.entries, entry{
			capability: followupIssueCapability{handoff: b.Handoff},
			advertised: b.Settings.Tracker.FollowupIssueCreation,
		})
	}
	if b.GitHub != nil {
		r.entries = append(r.entries,
			entry{capability: publishCapability{session: b.GitHub}, advertised: true},
			entry{capability: contextCapability{session: b.GitHub}, advertised: true},
			// github_land_pr is advertised only for a session bound to an issue
			// currently in the exact configured Merging state; Land itself
			// re-validates that Linear state immediately before any mutation, so
			// this is a coarse dispatch-time filter, not the authority.
			entry{capability: landCapability{session: b.GitHub}, advertised: landAdvertised(b)},
		)
	}
	return r
}

func landAdvertised(b Bindings) bool {
	mergeState := strings.TrimSpace(b.Settings.GitHub.MergeState)
	return mergeState != "" && strings.EqualFold(strings.TrimSpace(b.Issue.State), mergeState)
}

// Definitions returns the advertised capabilities in registration order.
func (r *Registry) Definitions() []Definition {
	if r == nil {
		return nil
	}
	var definitions []Definition
	for _, e := range r.entries {
		if e.advertised {
			definitions = append(definitions, e.capability.Definition())
		}
	}
	return definitions
}

// Lookup resolves a name to a bound capability. It intentionally ignores
// advertisement: see the entry type's comment.
func (r *Registry) Lookup(name string) (Capability, bool) {
	if r == nil {
		return nil, false
	}
	for _, e := range r.entries {
		if e.capability.Definition().Name == name {
			return e.capability, true
		}
	}
	return nil, false
}

// TurnEnded settles capability state that outlives a single call once an
// agent turn finishes, however it finished. It fires the deferred
// Merging -> In Review transition when a turn ended after a retryable landing
// gate without a successful landing. It is idempotent and a no-op when there is
// no bound GitHub session, when the bounded-fix feature is off, or when landing
// already resolved.
func (r *Registry) TurnEnded(ctx context.Context) {
	if r == nil || r.github == nil {
		return
	}
	r.github.FinalizeLanding(ctx)
}
