package github

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"

	"github.com/pmrrasmussen/symphony/internal/observability"
)

// LandGateError is returned by Land for a retryable hard gate when the
// bounded-fix feature is enabled and attempts remain. It is non-terminal: the
// backend surfaces Reason to Codex so it can fix, push, and call github_land_pr
// again within the same turn. Every Reason is a fixed or repository-config
// derived, bounded, secret-free string.
type LandGateError struct {
	Reason    string
	Retryable bool
}

func (e *LandGateError) Error() string { return e.Reason }

// LandStatus is the closed set of non-error outcomes for one github_land_pr
// call.
type LandStatus string

const (
	// LandMerged reports that the pull request is merged (by this call or a
	// prior one) and the bound Linear issue has been reconciled to Done.
	LandMerged LandStatus = "merged"
	// LandWaiting reports that required checks or GitHub's own mergeability
	// computation have not yet settled. It is non-terminal: the issue stays
	// in the configured Merging state, the current run ends without spending
	// another model turn, and the coordinator redispatches landing after a
	// bounded delay (PMR-78).
	LandWaiting LandStatus = "waiting"
)

// LandResult is the bounded github_land_pr response.
type LandResult struct {
	Status LandStatus `json:"status"`
	Number int        `json:"number"`
	URL    string     `json:"pull_request"`
	Method string     `json:"merge_method,omitempty"`
	Reason string     `json:"reason,omitempty"`
}

// Land merges the pull request already bound to this issue, repository, base,
// and branch, using the configured merge method. It accepts no input:
// repository, branch, PR, method, and Linear state are all fixed by the bound
// session and configuration, never by tool arguments.
//
// Three outcomes, and they are not interchangeable. A hard gate (failing checks,
// a changes-requested review, an unresolved thread, a stale base, a merge
// conflict, or a closed/mismatched pull request) refuses landing and attempts
// the configured Merging -> In Review fallback. Pending checks or undetermined
// mergeability return a non-terminal LandWaiting result without mutating Linear.
// A pull request GitHub already reports merged -- discovered up front,
// immediately before the merge call, or because the merge call itself raced with
// a concurrent merge -- reconciles the bound issue to Done idempotently instead
// of attempting another merge.
//
// See docs/architecture.md for the full gate list and the update-stale-branch
// behaviour.
func (s *Session) Land(ctx context.Context) (LandResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.TrimSpace(s.settings.MergeState) == "" {
		return LandResult{}, errors.New("github landing is not configured for this repository")
	}
	pr, found, err := s.manager.findPull(ctx, s.settings, s.branch)
	if err != nil {
		return LandResult{}, err
	}
	if !found {
		return LandResult{}, errors.New("github land requires an existing pull request for this issue")
	}
	if pr.Merged || pr.MergedAt != nil {
		return s.completeLanding(ctx, pr)
	}
	if !strings.EqualFold(pr.State, "open") {
		return s.refuse(ctx, "github pull request for this issue is closed")
	}
	if s.staleBaseUpdated && s.updatedHeadSHA == "" {
		// GitHub's update-branch endpoint is asynchronous. Do not merge the
		// old head while its accepted merge-from-base commit is still pending.
		if pr.Head.SHA == s.staleBaseOriginalHeadSHA {
			return s.waiting(pr.Number, pr.URL, "pull request branch update is pending"), nil
		}
		s.recordUpdatedHead(pr.Number, pr.Head.SHA)
	}

	origin, err := s.manager.git.Run(ctx, s.workspace, []string{"remote", "get-url", "origin"}, nil)
	if err != nil || !matchesRepository(origin, s.settings.Owner, s.settings.Repository) {
		return LandResult{}, errors.New("github land worktree origin does not match the configured repository")
	}
	status, err := s.manager.git.Run(ctx, s.workspace, []string{"status", "--porcelain"}, nil)
	if err != nil || status != "" {
		return LandResult{}, errors.New("github land requires a clean worktree")
	}
	head, err := s.manager.git.Run(ctx, s.workspace, []string{"rev-parse", "HEAD"}, nil)
	if err != nil {
		return LandResult{}, errors.New("github land requires a committed HEAD")
	}
	if _, err := s.manager.git.Run(ctx, s.workspace, []string{"fetch", "origin", s.settings.BaseBranch}, nil); err != nil {
		return LandResult{}, errors.New("github land could not fetch the configured base branch")
	}
	base1, err := s.manager.git.Run(ctx, s.workspace, []string{"rev-parse", "refs/remotes/origin/" + s.settings.BaseBranch}, nil)
	if err != nil {
		return LandResult{}, errors.New("github land requires the configured base branch")
	}

	// "Expected head" is the commit that must land: either the worktree's
	// already-pushed HEAD, or (when new local commits exist) HEAD itself
	// once it has been pushed to the deterministic issue branch.
	expectedHead := pr.Head.SHA
	if head != expectedHead {
		if s.staleBaseUpdated && s.updatedHeadSHA == expectedHead {
			// GitHub created the approved merge-from-base commit, so the local
			// worktree intentionally remains at its former (now ancestor) HEAD.
			if _, err := s.manager.git.Run(ctx, s.workspace, []string{"merge-base", "--is-ancestor", head, expectedHead}, nil); err != nil {
				return s.refuse(ctx, "github pull request head changed before landing")
			}
		} else {
			if _, err := s.manager.git.Run(ctx, s.workspace, []string{"merge-base", "--is-ancestor", expectedHead, head}, nil); err != nil {
				return s.refuse(ctx, "github land worktree head diverged from the published pull request")
			}
			auth := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + s.settings.Token))
			env := []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.https://github.com/.extraheader", "GIT_CONFIG_VALUE_0=AUTHORIZATION: basic " + auth}
			remote := "https://github.com/" + s.settings.Owner + "/" + s.settings.Repository + ".git"
			if _, err := s.manager.git.Run(ctx, s.workspace, []string{"push", remote, "HEAD:refs/heads/" + s.branch}, env); err != nil {
				// As with Publish's push gate above, execGit.Run now returns real
				// git/GitHub output on failure (PMR-163), and that can include the
				// push invocation's own AUTHORIZATION header on a transport error.
				// Forwarding err verbatim, as this line did before that change,
				// would hand the agent that raw text; log it to the host only and
				// return the same fixed, retryable gate reason gate/refuse always
				// return to callers.
				s.manager.logger.Warn("GitHub land could not push branch", "issue_id", s.issue.ID, "issue_identifier", s.issue.Identifier, "branch", s.branch, "push_error", observability.Text(err.Error()))
				return s.gate(ctx, "github land could not push branch "+s.branch+" to the configured repository; retry once, and if it persists check the repository's push permissions and branch protection rules", true)
			}
			expectedHead = head
			// A fix turn pushed new commits: record the delta on both the Linear
			// issue and the GitHub PR before the merge so it is inspectable. The
			// audit trail is part of the bounded-fix feature; with it off, landing
			// stays byte-for-byte as before and posts no comment.
			if s.settings.LandFixEnabled {
				s.auditPushedCommits(ctx, pr.Number, head)
			}
		}
	}

	if err := s.linear.EnsureMergeState(ctx, s.settings.MergeState); err != nil {
		return s.refuse(ctx, "github land active issue is no longer in the configured Merging state")
	}

	// Immediate pre-merge reads: required checks, effective review state,
	// unresolved threads, PR state, mergeability, and current base, all
	// re-read right before the irreversible merge call.
	fresh, err := s.manager.getPull(ctx, s.settings, pr.Number)
	if err != nil {
		return LandResult{}, err
	}
	if fresh.Merged || fresh.MergedAt != nil {
		return s.completeLanding(ctx, fresh)
	}
	if !strings.EqualFold(fresh.State, "open") {
		return s.refuse(ctx, "github pull request for this issue is closed")
	}
	if !validPull(s.settings, s.branch, fresh) {
		return s.refuse(ctx, "github returned a mismatched pull request")
	}
	if strings.TrimSpace(fresh.Head.SHA) != expectedHead {
		return s.refuse(ctx, "github pull request head changed before landing")
	}

	outcomes, err := s.manager.requiredCheckOutcomes(ctx, s.settings, fresh.Head.SHA, s.settings.RequiredChecks)
	if err != nil {
		return LandResult{}, err
	}
	var missing, pending, failing []string
	for _, name := range s.settings.RequiredChecks {
		switch outcomes[strings.ToLower(strings.TrimSpace(name))] {
		case checkMissing:
			missing = append(missing, name)
		case checkPending:
			pending = append(pending, name)
		case checkFailed:
			failing = append(failing, name)
		}
	}
	if len(failing) > 0 {
		return s.gate(ctx, "github required checks failed: "+strings.Join(failing, ", "), true)
	}
	if len(missing) > 0 || len(pending) > 0 {
		return s.waiting(fresh.Number, fresh.URL, requiredCheckWaitReason(missing, pending)), nil
	}

	// Moving the issue to Merging is the human approval to land (see policy
	// in the issue): no additional approving review is required here, only
	// the absence of an effective changes-requested review. The discarded
	// third return is the excerpt cap, not a completeness signal -- reviews
	// itself paginates, and gating on that flag would park every pull request
	// with more than contextMaxItems reviews in a permanent wait (PMR-190).
	reviewState, _, _, err := s.manager.reviews(ctx, s.settings, fresh.Number)
	if err != nil {
		return LandResult{}, err
	}
	if reviewState == "changes_requested" {
		return s.refuse(ctx, "github pull request has an effective changes-requested review")
	}
	unresolved, _, threadsTruncated, err := s.manager.reviewThreads(ctx, s.settings, fresh.Number)
	if err != nil {
		return LandResult{}, err
	}
	if unresolved > 0 {
		return s.gate(ctx, "github pull request has unresolved review threads", true)
	}
	// Every thread this read did see is resolved, but it did not see them all,
	// so "no unresolved threads" is unproven rather than true. Wait instead of
	// merging past the gate: the thread listing is paginated to a bounded cap,
	// which makes this reachable only for a pull request with more review
	// threads than that cap can hold, and waiting keeps the issue in Merging
	// for a human rather than landing on an unread gate (PMR-190).
	if threadsTruncated {
		return s.waiting(fresh.Number, fresh.URL, "github review threads could not be read completely"), nil
	}
	if fresh.Mergeable == nil {
		return s.waiting(fresh.Number, fresh.URL, "github has not yet computed mergeability"), nil
	}
	if !*fresh.Mergeable {
		// A merge conflict is retryable only when conflict resolution is opted
		// in; otherwise it refuses immediately exactly as before.
		return s.gate(ctx, "github pull request has merge conflicts", s.settings.AllowConflictResolution)
	}

	if _, err := s.manager.git.Run(ctx, s.workspace, []string{"fetch", "origin", s.settings.BaseBranch}, nil); err != nil {
		return LandResult{}, errors.New("github land could not fetch the configured base branch")
	}
	base2, err := s.manager.git.Run(ctx, s.workspace, []string{"rev-parse", "refs/remotes/origin/" + s.settings.BaseBranch}, nil)
	if err != nil {
		return LandResult{}, errors.New("github land requires the configured base branch")
	}
	if base2 != base1 {
		if s.settings.UpdateStaleBranch && !s.staleBaseUpdated {
			return s.updateStaleBranch(ctx, fresh)
		}
		return s.refuse(ctx, "github land configured base branch changed before landing")
	}

	merged, err := s.manager.mergePull(ctx, s.settings, fresh.Number, s.settings.MergeMethod, expectedHead)
	if err != nil {
		if recheck, recheckErr := s.manager.getPull(ctx, s.settings, fresh.Number); recheckErr == nil && (recheck.Merged || recheck.MergedAt != nil) {
			return s.completeLanding(ctx, recheck)
		}
		return LandResult{}, err
	}
	if !merged {
		return s.refuse(ctx, "github merge request was not accepted")
	}
	s.manager.logger.Info("GitHub pull request merged", "issue_id", s.issue.ID, "issue_identifier", s.issue.Identifier, "repository", s.settings.Owner+"/"+s.settings.Repository, "pr_number", fresh.Number, "merge_method", s.settings.MergeMethod)
	return s.completeLanding(ctx, fresh)
}

// updateStaleBranch asks GitHub to create exactly one merge-from-base commit
// on the already-approved pull-request branch. GitHub pins the mutation to
// fresh.Head.SHA, so a concurrent head change is rejected rather than merged.
func (s *Session) updateStaleBranch(ctx context.Context, fresh pull) (LandResult, error) {
	if strings.TrimSpace(fresh.Head.SHA) == "" {
		return s.refuse(ctx, "github returned a pull request without a head commit")
	}
	if err := s.manager.updatePullBranch(ctx, s.settings, fresh.Number, fresh.Head.SHA); err != nil {
		return s.refuse(ctx, "github land could not update stale pull request branch")
	}
	s.staleBaseUpdated = true
	s.staleBaseOriginalHeadSHA = fresh.Head.SHA
	updated, err := s.manager.getPull(ctx, s.settings, fresh.Number)
	if err != nil {
		return LandResult{}, err
	}
	if !validPull(s.settings, s.branch, updated) || !strings.EqualFold(updated.State, "open") || strings.TrimSpace(updated.Head.SHA) == "" {
		return s.refuse(ctx, "github returned an invalid pull request after branch update")
	}
	if updated.Head.SHA != fresh.Head.SHA {
		s.recordUpdatedHead(fresh.Number, updated.Head.SHA)
	}
	return s.waiting(fresh.Number, fresh.URL, "pull request branch was updated; required checks are pending"), nil
}

func (s *Session) recordUpdatedHead(number int, sha string) {
	if strings.TrimSpace(sha) == "" || s.updatedHeadSHA != "" {
		return
	}
	s.updatedHeadSHA = sha
	s.manager.logger.Info("GitHub pull request branch updated", "issue_identifier", s.issue.Identifier, "pr_number", number, "head_sha", sha)
}

// waiting records and returns a non-terminal landing wait. A wait supersedes
// any deferred hard-gate refusal: checks or mergeability are genuinely pending,
// so the issue must stay in the configured Merging state for the coordinator's
// own delayed landing retry rather than be returned to review at turn end
// (PMR-78). It assumes s.mu is held.
func (s *Session) waiting(number int, url, reason string) LandResult {
	s.waitingOutcome = true
	return LandResult{Status: LandWaiting, Number: number, URL: url, Reason: reason}
}

// completeLanding reconciles the bound Linear issue to Done for an already
// (or just-now) merged pull request. It is the single idempotent recovery
// path for duplicate landing calls and for a GitHub merge that succeeded but
// whose Linear completion previously failed.
func (s *Session) completeLanding(ctx context.Context, pr pull) (LandResult, error) {
	// Reaching completeLanding means the pull request is merged, so no deferred
	// Merging -> In Review refusal must fire even if the Linear completion call
	// below fails and is retried.
	s.landed = true
	s.waitingOutcome = false
	// github_land_pr merges through the GitHub API and never fetches
	// afterwards, so refs/remotes/origin/<base> -- shared across every
	// worktree, since it lives in the Git common directory rather than any one
	// session's worktree -- stays stale until something else fetches. Refresh
	// it here, best effort: a merge that already succeeded must still be
	// reconciled to Done even if this fetch fails, so failure is logged rather
	// than returned (PMR-135).
	s.manager.fetchMu.Lock()
	if _, err := s.manager.git.Run(ctx, s.workspace, []string{"fetch", "origin", s.settings.BaseBranch}, nil); err != nil {
		s.manager.logger.Warn("GitHub post-land base ref refresh failed", "issue_id", s.issue.ID, "issue_identifier", s.issue.Identifier, "repository", s.settings.Owner+"/"+s.settings.Repository, "error", observability.Text(err.Error()))
	}
	s.manager.fetchMu.Unlock()
	if _, err := s.linear.CompleteLanding(ctx, s.settings.MergeState); err != nil {
		return LandResult{}, err
	}
	// Only a fully reconciled landing closes the capability for this run: a
	// merge whose Linear completion failed above must remain retryable.
	s.landingResolved = true
	return LandResult{Status: LandMerged, Number: pr.Number, URL: pr.URL, Method: s.settings.MergeMethod}, nil
}

// refuse attempts the configured Merging -> In Review fallback transition
// (best effort: a failure here does not override the substantive hard-gate
// reason) and returns the hard-gate refusal as a structured tool failure. An
// immediate refusal supersedes any deferred transition, so deferredFired is set
// to keep FinalizeLanding a no-op.
func (s *Session) refuse(ctx context.Context, reason string) (LandResult, error) {
	s.deferredFired = true
	s.waitingOutcome = false
	if _, err := s.linear.RefuseLanding(ctx, s.settings.MergeState, reason); err != nil {
		s.manager.logger.Warn("GitHub land Merging fallback transition failed", "issue_id", s.issue.ID, "issue_identifier", s.issue.Identifier, "reason", reason, "error", observability.Text(err.Error()))
	}
	return LandResult{}, errors.New(reason)
}

// gate handles a hard gate. When the bounded-fix feature is enabled, the gate
// is retryable, and a fix attempt remains, it defers the Merging -> In Review
// transition and returns a non-terminal LandGateError so the same Codex turn
// can fix, push, and retry. When attempts are exhausted it fires the deferred
// transition plus the comment naming the gate. In every other case (feature
// off, non-retryable gate) it refuses immediately, byte-for-byte as before.
func (s *Session) gate(ctx context.Context, reason string, retryable bool) (LandResult, error) {
	if !retryable || !s.settings.LandFixEnabled {
		return s.refuse(ctx, reason)
	}
	s.retryableGateHit = true
	s.lastFailedGate = reason
	s.waitingOutcome = false
	if s.landAttempts >= s.settings.MaxLandAttempts {
		s.fireDeferredRefusal(ctx)
		return LandResult{}, errors.New(reason)
	}
	s.landAttempts++
	return LandResult{}, &LandGateError{Reason: reason, Retryable: true}
}

// fireDeferredRefusal performs the deferred Merging -> In Review transition and
// the comment naming the last failed gate at most once per session. It assumes
// s.mu is held.
func (s *Session) fireDeferredRefusal(ctx context.Context) {
	if s.deferredFired || s.landed {
		return
	}
	s.deferredFired = true
	if _, err := s.linear.RefuseLanding(ctx, s.settings.MergeState, s.lastFailedGate); err != nil {
		s.manager.logger.Warn("GitHub land deferred Merging fallback transition failed", "issue_id", s.issue.ID, "issue_identifier", s.issue.Identifier, "error", observability.Text(err.Error()))
	}
	if err := s.linear.LandComment(ctx, landingRefusalComment(s.lastFailedGate)); err != nil {
		s.manager.logger.Warn("GitHub land refusal comment failed", "issue_id", s.issue.ID, "issue_identifier", s.issue.Identifier, "error", observability.Text(err.Error()))
	}
}

// FinalizeLanding fires the deferred Merging -> In Review transition (and its
// comment) once when an agent turn ends after a retryable landing gate was hit
// but landing neither succeeded nor was already refused. It is a safe no-op
// when the feature is off, when no retryable gate was hit, when landing
// succeeded, when the last landing outcome was a non-terminal wait (the issue
// stays in Merging for the coordinator's delayed retry), or when the deferred
// transition already fired.
//
// Two of those conditions are worth naming precisely, because a reader checking
// which one carries the weight will otherwise get it wrong. The feature check is
// unreachable by construction and kept only as a statement of intent:
// retryableGateHit is set exclusively by gate(), which returns through refuse()
// before setting it whenever the feature is off. The landed check is genuinely
// redundant with fireDeferredRefusal's own, and deliberately so: it covers the
// highest-consequence mistake available here -- a gate hit, then a fix turn's
// retry that merged, where firing would walk a merged, Done issue back to review
// with a comment claiming fix attempts were exhausted. Both transports assert
// that case end to end.
func (s *Session) FinalizeLanding(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.settings.LandFixEnabled || !s.retryableGateHit || s.landed || s.waitingOutcome {
		return
	}
	s.fireDeferredRefusal(ctx)
}

// auditPushedCommits records a fix turn's just-pushed head on both the Linear
// issue and the GitHub pull request. It is best-effort: a comment failure is
// logged but never blocks landing.
func (s *Session) auditPushedCommits(ctx context.Context, prNumber int, sha string) {
	body := landingPushComment(s.issue.Identifier, sha)
	if err := s.linear.LandComment(ctx, body); err != nil {
		s.manager.logger.Warn("GitHub land push audit Linear comment failed", "issue_id", s.issue.ID, "issue_identifier", s.issue.Identifier, "pr_number", prNumber, "error", observability.Text(err.Error()))
	}
	if err := s.manager.commentPR(ctx, s.settings, prNumber, body); err != nil {
		s.manager.logger.Warn("GitHub land push audit PR comment failed", "issue_id", s.issue.ID, "issue_identifier", s.issue.Identifier, "pr_number", prNumber, "error", observability.Text(err.Error()))
	}
}

// landingPushComment and landingRefusalComment are the fixed, bounded audit
// bodies. They contain only the bound issue identifier, a short head SHA, and
// the fixed/config-derived gate reason -- never a credential or provider
// payload.
func landingPushComment(identifier, sha string) string {
	return "Symphony landing pushed new commit(s) for " + strings.TrimSpace(identifier) + ". New pull request head: " + shortSHA(sha) + "."
}

func landingRefusalComment(gate string) string {
	gate = strings.TrimSpace(gate)
	if gate == "" {
		gate = "a landing gate"
	}
	return "Symphony returned this issue to review after exhausting landing fix attempts. Last failed gate: " + gate + "."
}
