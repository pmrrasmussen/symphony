package capability

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/pmrrasmussen/symphony/internal/domain"
	"github.com/pmrrasmussen/symphony/internal/linear"
)

// followupIssueCapability captures meaningful out-of-scope work as a new
// Backlog issue without letting the agent choose its scope or initial state.
type followupIssueCapability struct{ handoff *linear.HandoffSession }

// Lifecycle is false: this capability is a single bounded tracker round trip and
// is deliberately not reported as a dynamicToolCall item, unlike the GitHub
// capabilities whose upstream round trips can be slow enough to need a visible
// outstanding operation. Keeping it unreported also keeps argument validation,
// which the provider performs, from being reported as a started call.
func (c followupIssueCapability) Lifecycle() bool { return false }

func (c followupIssueCapability) Definition() Definition {
	return Definition{
		Name:        NameCreateFollowupIssue,
		Description: "Capture meaningful out-of-scope work as a new Backlog Linear issue in the active issue's configured project and team, then continue the current issue. The follow-up is not a child and is not dispatchable until a human promotes it. relationship may only relate it to the current issue or mark it blocked by the current issue.",
		InputSchema: map[string]any{
			"type": "object", "additionalProperties": false,
			"properties": map[string]any{
				// The title bound is the provider's own. The description and
				// acceptance-criteria bounds are per field, while the provider
				// bounds the rendered body they are combined into; the invariant
				// test asserts they cannot sum past it. Every bound below and every
				// bound the provider enforces counts code points, the unit a schema
				// maxLength counts, so a schema-valid non-ASCII body cannot be
				// refused by a bound the agent was never told about (PMR-183).
				"title":               map[string]any{"type": "string", "minLength": 1, "maxLength": linear.MaxFollowupIssueTitleRunes},
				"description":         map[string]any{"type": "string", "minLength": 1, "maxLength": 16000},
				"acceptance_criteria": map[string]any{"type": "string", "minLength": 1, "maxLength": 4000},
				"relationship":        map[string]any{"type": "string", "enum": []string{"related", "blocked_by_current"}},
			},
			"required": []string{"title", "description", "acceptance_criteria"},
		},
	}
}

// Prepare performs no validation of its own: the provider owns argument
// validation for this capability, and rejecting there keeps a single copy of the
// rules that bound the created issue.
func (c followupIssueCapability) Prepare(arguments json.RawMessage) (Invocation, *Failure) {
	return func(ctx context.Context) (Result, *Failure) {
		result, err := c.handoff.CreateFollowupIssue(ctx, arguments)
		if err != nil {
			return Result{}, followupRefusal(err)
		}
		return Result{Success: result.Success, Payload: result.Data}, nil
	}, nil
}

// followupRefusal splits the provider's refusals the way every GitHub
// capability already splits its own (PMR-132, PMR-149). The provider owns this
// capability's argument validation, so its request refusals -- an invalid
// title, a missing field, an exceeded bound, a scope that no longer holds --
// are the only account of what was wrong with the call, and each is a fixed
// host-authored string (linear.Error.RefusesRequest). Collapsing them left a
// model with nothing to act on but the identical call it had just made
// (PMR-183), so they are forwarded.
//
// Everything else -- a transport failure, a provider response -- keeps the
// generic message: no provider error, issue data, or credential-derived value
// reaches the child, and knowing the round trip failed is enough for the model
// to choose another path.
func followupRefusal(err error) *Failure {
	var refusal *linear.Error
	if errors.As(err, &refusal) && refusal.RefusesRequest() {
		return &Failure{Message: "Linear follow-up issue creation needs a fix: " + refusal.Message + ".", Outcome: domain.ItemFailed}
	}
	return &Failure{Message: "Linear follow-up issue creation was rejected.", Outcome: domain.ItemFailed}
}
