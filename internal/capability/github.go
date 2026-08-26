package capability

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/pmrrasmussen/symphony/internal/domain"
	githubhost "github.com/pmrrasmussen/symphony/internal/github"
)

// refreshBaseRefCapability fetches the configured base branch into this
// worktree's shared refs/remotes/origin/<base>, host-side, so a session whose
// base went stale mid-run can clear it without the write access to the
// source repository's common Git directory that fetching itself would
// require (PMR-141).
type refreshBaseRefCapability struct{ session *githubhost.Session }

func (c refreshBaseRefCapability) Lifecycle() bool { return true }

func (c refreshBaseRefCapability) Definition() Definition {
	return Definition{
		Name:        NameGitHubRefreshBaseRef,
		Description: "Fetch the configured base branch from origin into refs/remotes/origin/<base>, refreshing a base that moved since dispatch, and return its resolved commit. Call this before merging or rebasing onto the base branch. No input; a fetch failure is refused so the run can proceed against the base ref it already has.",
		InputSchema: map[string]any{"type": "object", "additionalProperties": false},
	}
}

func (c refreshBaseRefCapability) Prepare(arguments json.RawMessage) (Invocation, *Failure) {
	if failure := decodeNoInput(arguments); failure != nil {
		return nil, failure
	}
	return func(ctx context.Context) (Result, *Failure) {
		commit, err := c.session.RefreshBaseRef(ctx)
		if err != nil {
			return Result{}, &Failure{Message: "GitHub base branch refresh was rejected.", Outcome: domain.ItemFailed}
		}
		return Result{Success: true, Payload: map[string]any{"base_commit": commit}}, nil
	}, nil
}

// publishCapability hands a committed, clean worktree to human review.
type publishCapability struct{ session *githubhost.Session }

func (c publishCapability) Lifecycle() bool { return true }

func (c publishCapability) Definition() Definition {
	return Definition{
		Name:        NameGitHubPublishPR,
		Description: "Publish the current committed clean worktree to its fixed issue branch, create or reuse its pull request with a structured Why/What changed/On Call body, and hand the active Linear issue to review.",
		InputSchema: map[string]any{
			"type": "object", "additionalProperties": false,
			"properties": map[string]any{
				// The bounds are the provider's own, so the advertised schema and
				// the parser that enforces it cannot drift apart.
				"why":          map[string]any{"type": "string", "minLength": 1, "maxLength": githubhost.MaxPublishWhyBytes},
				"what_changed": map[string]any{"type": "string", "minLength": 1, "maxLength": githubhost.MaxPublishWhatChangedBytes},
				"on_call":      map[string]any{"type": "string", "maxLength": githubhost.MaxPublishOnCallBytes},
			},
			"required": []string{"why", "what_changed", "on_call"},
		},
	}
}

func (c publishCapability) Prepare(arguments json.RawMessage) (Invocation, *Failure) {
	input, err := githubhost.ParsePublishInput(arguments)
	if err != nil {
		return nil, &Failure{Message: "GitHub pull request publication arguments were rejected.", Outcome: domain.ItemFailed}
	}
	return func(ctx context.Context) (Result, *Failure) {
		result, err := c.session.Publish(ctx, input)
		if err != nil {
			// Every reason Publish returns is a fixed, repository-config-derived,
			// secret-free string (unclean worktree, stale base, non-fast-forward
			// remote, and so on), so it is passed straight through: the agent can
			// act on it and retry, instead of looping on a refusal it has no way
			// to diagnose (PMR-132).
			return Result{}, &Failure{Message: "GitHub publish needs a fix: " + err.Error() + ".", Outcome: domain.ItemFailed}
		}
		// The payload shape is built explicitly rather than marshaled from the
		// provider result, so the field names an agent sees stay pinned here.
		return Result{Success: true, Payload: map[string]any{
			"branch": result.Branch, "pull_request": result.URL,
			"number": result.Number, "body_updated": result.BodyUpdated,
		}}, nil
	}, nil
}

// contextCapability reads bounded review context for the bound pull request.
type contextCapability struct{ session *githubhost.Session }

func (c contextCapability) Lifecycle() bool { return true }

func (c contextCapability) Definition() Definition {
	return Definition{
		Name:        NameGitHubPRContext,
		Description: "Read bounded check status, effective review state, comment/review excerpts, and unresolved review-thread counts for the pull request already bound to this issue, repository, and branch. Read-only; it cannot select another repository, issue, branch, or pull request.",
		InputSchema: map[string]any{"type": "object", "additionalProperties": false},
	}
}

func (c contextCapability) Prepare(arguments json.RawMessage) (Invocation, *Failure) {
	if failure := decodeNoInput(arguments); failure != nil {
		return nil, failure
	}
	return func(ctx context.Context) (Result, *Failure) {
		result, err := c.session.Context(ctx)
		if err != nil {
			// Every reason Context returns is a fixed, secret-free string (most
			// commonly "no pull request has been published for this issue yet"),
			// so it is passed straight through rather than discarded (PMR-132).
			return Result{}, &Failure{Message: "GitHub pull request context was rejected: " + err.Error() + ".", Outcome: domain.ItemFailed}
		}
		return Result{Success: true, Payload: result}, nil
	}, nil
}

// landCapability merges the bound pull request once every gate passes.
type landCapability struct{ session *githubhost.Session }

func (c landCapability) Lifecycle() bool { return true }

func (c landCapability) Definition() Definition {
	return Definition{
		Name:        NameGitHubLandPR,
		Description: "Merge the pull request already bound to this issue, repository, base, and branch using the configured merge method, once required checks pass, reviews have no effective changes-requested state, and no review thread is unresolved. Returns a non-terminal waiting result while required checks or GitHub's mergeability computation are pending; with github.update_stale_branch enabled, one clean stale-base update also waits for checks on its new head. A waiting result ends this run and Symphony itself redispatches landing later; a merged result ends this run for good. Never call this tool again after either outcome. Other hard gates (failing checks, requested changes, unresolved threads, a stale base, conflicts, or a closed/mismatched PR) refuse landing. No repository, issue, branch, PR, method, state, or credential input.",
		InputSchema: map[string]any{"type": "object", "additionalProperties": false},
	}
}

func (c landCapability) Prepare(arguments json.RawMessage) (Invocation, *Failure) {
	if failure := decodeNoInput(arguments); failure != nil {
		return nil, failure
	}
	return func(ctx context.Context) (Result, *Failure) {
		if c.session.LandingResolved() {
			// Landing already reached its terminal outcome in this run, so the
			// capability is closed: refuse without invoking it again (PMR-78).
			// The call was already reported as started, so this is a decline.
			return Result{}, &Failure{Message: "GitHub landing already completed for this run.", Outcome: domain.ItemDeclined}
		}
		result, err := c.session.Land(ctx)
		if err != nil {
			// A retryable landing gate is non-terminal: name the exact gate so
			// the agent can fix it, push, and land again in this turn. Every
			// reason is a fixed or configuration-derived, bounded, secret-free
			// string defined in the github package. Any other error keeps the
			// generic refusal message.
			var gate *githubhost.LandGateError
			if errors.As(err, &gate) && gate.Retryable {
				return Result{}, &Failure{Message: "GitHub landing needs a fix: " + gate.Reason + ".", Outcome: domain.ItemFailed}
			}
			// A non-retryable gate and every other Land error are also fixed,
			// secret-free strings (see githubhost.Session.Land), so the terminal
			// reason is passed through too instead of being discarded (PMR-132).
			return Result{}, &Failure{Message: "GitHub pull request landing was rejected: " + err.Error() + ".", Outcome: domain.ItemFailed}
		}
		// A settled landing decision ends the run: no further model turn or tool
		// call can advance it, so it is reported as a terminal outcome instead of
		// letting the session spend turns on repeated landing calls (PMR-78). Only
		// a waiting outcome carries a reason.
		terminal, reason := landingOutcome(result)
		return Result{Success: true, Payload: result, Terminal: terminal, Reason: reason}, nil
	}, nil
}

func landingOutcome(result githubhost.LandResult) (domain.EventKind, string) {
	switch result.Status {
	case githubhost.LandWaiting:
		return domain.EventLandingWaiting, result.Reason
	case githubhost.LandMerged:
		return domain.EventLandingResolved, ""
	}
	return "", ""
}
