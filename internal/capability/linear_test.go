package capability

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/pmrrasmussen/symphony/internal/domain"
	"github.com/pmrrasmussen/symphony/internal/linear"
)

// TestFollowupRefusalForwardsAValidationReasonAndNothingElse pins the split
// PMR-183 introduced. Before it, every reason -- an invalid title, a missing
// field, a scope that had moved -- reached the model as one generic sentence,
// so a 300-rune title bought a retry of the identical call instead of a fix.
func TestFollowupRefusalForwardsAValidationReasonAndNothingElse(t *testing.T) {
	for name, test := range map[string]struct {
		err  error
		want string
	}{
		"invalid title": {
			&linear.Error{Category: "handoff_request", Message: "follow-up issue title is invalid"},
			"Linear follow-up issue creation needs a fix: follow-up issue title is invalid.",
		},
		"missing body fields": {
			&linear.Error{Category: "handoff_request", Message: "follow-up issue description and acceptance criteria are required"},
			"Linear follow-up issue creation needs a fix: follow-up issue description and acceptance criteria are required.",
		},
		"issue left scope": {
			&linear.Error{Category: "handoff_scope", Message: "active issue state changed after session setup"},
			"Linear follow-up issue creation needs a fix: active issue state changed after session setup.",
		},
		"unresolved scope": {
			&linear.Error{Category: "invalid_tracker_config", Message: "linear follow-up issue scope could not be resolved"},
			"Linear follow-up issue creation needs a fix: linear follow-up issue scope could not be resolved.",
		},
		// A round trip the agent cannot act on stays generic, and so does anything
		// that is not a tracker error at all: neither may become a channel for
		// provider text.
		"transport failure": {
			&linear.Error{Category: "tracker_transport", Message: "Linear request failed"},
			"Linear follow-up issue creation was rejected.",
		},
		"provider response": {
			&linear.Error{Category: "handoff_response", Message: "Linear did not accept the follow-up issue"},
			"Linear follow-up issue creation was rejected.",
		},
		"foreign error": {
			context.DeadlineExceeded,
			"Linear follow-up issue creation was rejected.",
		},
	} {
		t.Run(name, func(t *testing.T) {
			refusal := followupRefusal(test.err)
			if refusal.Message != test.want {
				t.Fatalf("refusal = %q, want %q", refusal.Message, test.want)
			}
			if refusal.Outcome != domain.ItemFailed {
				t.Fatalf("refusal outcome = %q, want %q", refusal.Outcome, domain.ItemFailed)
			}
		})
	}
}

// TestAFollowupValidationRefusalSurvivesDispatchToBothTransports runs the real
// capability, the real provider validation, and the real dispatch, under both
// transport shapes: the plain one the Codex app-server adapter supplies, and
// the gated one internal/mcpbridge supplies. Both frame Outcome.Refusal by
// writing its Message, so a reason that reaches the response here is a reason
// the model reads.
//
// The binding is an unprepared handoff session, whose CreateFollowupIssue
// refuses in the provider before any round trip -- the one validation refusal
// this package can reach without a Linear server. internal/linear's own tests
// cover the rest of them.
func TestAFollowupValidationRefusalSurvivesDispatchToBothTransports(t *testing.T) {
	registry := Build(bindings(true, false, "In Progress", ""))
	arguments := json.RawMessage(`{"title":"Split off the client change","description":"d","acceptance_criteria":"a"}`)
	const want = "Linear follow-up issue creation needs a fix: follow-up issue creation is not enabled."

	for name, transport := range map[string]func(*recorder) Transport{
		"codex app-server": (*recorder).transport,
		"mcp endpoint":     func(r *recorder) Transport { return r.gated(true) },
	} {
		t.Run(name, func(t *testing.T) {
			var records recorder
			Dispatch(context.Background(), registry, transport(&records), NameCreateFollowupIssue, arguments)
			if message := records.refusal(t); message != want {
				t.Fatalf("refusal = %q, want %q", message, want)
			}
			// The reason is the provider's own sentence and nothing more: no
			// arguments, no issue content, no provider error text.
			if strings.Contains(records.refusal(t), "Split off the client change") {
				t.Fatalf("refusal echoed the call's arguments: %q", records.refusal(t))
			}
		})
	}
}
