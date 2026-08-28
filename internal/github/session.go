package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/domain"
	"github.com/pmrrasmussen/symphony/internal/observability"
)

// Structured-handoff input bounds. These are the single source for both the
// parser below and the advertised github_publish_pr schema, which reads them
// from internal/capability; an invariant test asserts the two agree.
const (
	MaxPublishWhyBytes         = 4 << 10
	MaxPublishWhatChangedBytes = 8 << 10
	MaxPublishOnCallBytes      = 2 << 10
)

// Bounds applied to github_pr_context output so a large upstream history
// cannot inflate the child-visible result.
const (
	contextMaxItems     = 20
	contextExcerptRunes = 240
)

type Session struct {
	manager   *Manager
	settings  config.GitHub
	issue     domain.Issue
	workspace string
	branch    string
	linear    linearLifecycle
	// staleBaseUpdated permits one deterministic GitHub update-branch request
	// during this landing session. A later base movement remains a hard gate.
	staleBaseUpdated         bool
	staleBaseOriginalHeadSHA string
	updatedHeadSHA           string
	// Bounded-fix state (PMR-46), all guarded by mu. landAttempts counts the
	// non-terminal fix requests already granted this session; retryableGateHit
	// records that at least one retryable gate deferred its Merging -> In
	// Review transition; lastFailedGate is the fixed reason of the most recent
	// retryable gate; landed is set once the pull request is merged; and
	// deferredFired guards the deferred transition + comment so it happens at
	// most once.
	landAttempts     int
	retryableGateHit bool
	lastFailedGate   string
	landed           bool
	deferredFired    bool
	// Landing-outcome state (PMR-78), also guarded by mu. landingResolved is
	// set once landing reached its terminal outcome (merged and reconciled to
	// Done), so the tool-dispatch path can refuse a second github_land_pr
	// invocation in the same logical run; Land itself stays idempotent as a
	// recovery safeguard. waitingOutcome records that the most recent landing
	// outcome was a non-terminal wait, which supersedes any earlier deferred
	// refusal: while checks or mergeability are genuinely pending the issue must
	// stay in the configured Merging state for the coordinator's delayed retry.
	landingResolved bool
	waitingOutcome  bool
	mu              sync.Mutex
}

// LandingResolved reports whether this session's landing already reached its
// terminal outcome. The Codex tool-dispatch path uses it to refuse a second
// github_land_pr invocation in the same logical run (PMR-78); Land itself
// remains idempotent so a recovery path (a merge whose Linear completion
// failed, or a session restarted after a crash) can still reconcile.
func (s *Session) LandingResolved() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.landingResolved
}

func (s *Session) MatchesSecret(candidate string) bool {
	return s.settings.Token != "" && strings.Contains(candidate, s.settings.Token)
}

type Result struct {
	Branch, URL string
	Number      int
	// BodyUpdated is true only when this call created the pull request or
	// changed its body to match newly supplied structured handoff fields.
	BodyUpdated bool
}

// PublishInput is the bounded structured handoff content a Codex session
// supplies for github_publish_pr. There is deliberately no repository,
// issue, or branch field: the session already fixes those.
type PublishInput struct {
	Why, WhatChanged, OnCall string
}

// ParsePublishInput decodes and bounds-checks github_publish_pr tool
// arguments. It rejects a non-object payload, unsupported fields, missing or
// non-string values, and content outside the fixed size limits.
func ParsePublishInput(arguments json.RawMessage) (PublishInput, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(arguments, &raw); err != nil || raw == nil {
		return PublishInput{}, errors.New("github publish arguments must be a JSON object")
	}
	for key := range raw {
		if key != "why" && key != "what_changed" && key != "on_call" {
			return PublishInput{}, errors.New("github publish arguments contain an unsupported field")
		}
	}
	var input struct {
		Why         *string `json:"why"`
		WhatChanged *string `json:"what_changed"`
		OnCall      *string `json:"on_call"`
	}
	if err := json.Unmarshal(arguments, &input); err != nil || input.Why == nil || input.WhatChanged == nil || input.OnCall == nil {
		return PublishInput{}, errors.New("github publish requires why, what_changed, and on_call strings")
	}
	why := strings.TrimSpace(*input.Why)
	whatChanged := strings.TrimSpace(*input.WhatChanged)
	onCall := strings.TrimSpace(*input.OnCall)
	if why == "" || len([]byte(why)) > MaxPublishWhyBytes {
		return PublishInput{}, errors.New("github publish why is empty or too large")
	}
	if whatChanged == "" || len([]byte(whatChanged)) > MaxPublishWhatChangedBytes {
		return PublishInput{}, errors.New("github publish what_changed is empty or too large")
	}
	if len([]byte(onCall)) > MaxPublishOnCallBytes {
		return PublishInput{}, errors.New("github publish on_call is too large")
	}
	return PublishInput{Why: why, WhatChanged: whatChanged, OnCall: onCall}, nil
}

// canonicalBody is the deterministic pull request body. Repeat publication
// with the same structured fields must render byte-identical output so a
// reused pull request is left untouched.
func canonicalBody(input PublishInput, issueURL string) string {
	return fmt.Sprintf("## Why\n%s\n\n## What changed\n%s\n\n## On Call\n%s\n\nLinear: %s\n", input.Why, input.WhatChanged, input.OnCall, strings.TrimSpace(issueURL))
}

// RefreshBaseRef fetches the configured base branch from origin into this
// worktree's shared refs/remotes/origin/<base> -- the same refspec workspace
// creation uses -- and returns its resolved commit. It is the host-mediated
// stand-in for a fetch the sandboxed agent cannot perform itself: updating
// refs/remotes/origin/<base> writes the Git common directory, which is
// outside every path the agent's own worktree grant covers (PMR-141). A
// fetch failure is returned as an error so the capability layer can refuse
// the call and let the run proceed against whatever base ref it already has,
// rather than ending the run. The fetch itself runs under manager.fetchMu,
// because refs/remotes/origin/<base> and packed-refs are shared across every
// session's worktree: at raised agent.max_concurrent_agents, an unserialized
// fetch would race another session's fetch on that same repository-wide ref.
func (s *Session) RefreshBaseRef(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.manager.fetchMu.Lock()
	defer s.manager.fetchMu.Unlock()
	refspec := "+refs/heads/" + s.settings.BaseBranch + ":refs/remotes/origin/" + s.settings.BaseBranch
	if _, err := s.manager.git.Run(ctx, s.workspace, []string{"fetch", "--no-tags", "origin", refspec}, nil); err != nil {
		return "", errors.New("github base ref refresh could not fetch the configured base branch")
	}
	base, err := s.manager.git.Run(ctx, s.workspace, []string{"rev-parse", "--verify", "refs/remotes/origin/" + s.settings.BaseBranch + "^{commit}"}, nil)
	if err != nil {
		return "", errors.New("github base ref refresh requires the configured base branch")
	}
	return base, nil
}

// logPublishRefused records a Warn-level entry for a github_publish_pr
// refusal, naming the fixed gate reason (or, for EnsureActive, the Linear
// error it returned) an operator can otherwise learn only by rediscovering it
// by hand: before this, a refusal here produced no host-side record at all, so
// a run that consumed its whole turn budget hitting the same refusal repeatedly
// left no trace of why (PMR-163). head is the worktree HEAD when the gate that
// refused had already resolved one, empty otherwise; extra is appended
// verbatim, so a caller passes only fixed keys and observability-bounded
// values, exactly like every other diagnostic in this package.
func (s *Session) logPublishRefused(reason, head string, extra ...any) {
	attrs := []any{
		"operation", observability.OperationPublishRefused,
		"issue_id", s.issue.ID,
		"issue_identifier", s.issue.Identifier,
		"branch", s.branch,
		"reason", observability.Text(reason),
	}
	if head != "" {
		attrs = append(attrs, "head", shortSHA(head))
	}
	attrs = append(attrs, extra...)
	s.manager.logger.Warn("GitHub publish refused", attrs...)
}

// Publish verifies a clean committed worktree, publishes only HEAD to the
// deterministic issue branch, creates/reuses its PR with the canonical
// structured body, and performs the bound Linear link/review handoff.
func (s *Session) Publish(ctx context.Context, input PublishInput) (Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.linear.EnsureActive(ctx); err != nil {
		s.logPublishRefused(err.Error(), "")
		return Result{}, err
	}
	origin, err := s.manager.git.Run(ctx, s.workspace, []string{"remote", "get-url", "origin"}, nil)
	if err != nil || !matchesRepository(origin, s.settings.Owner, s.settings.Repository) {
		reason := "github publish worktree origin does not match the configured repository"
		s.logPublishRefused(reason, "")
		return Result{}, errors.New(reason)
	}
	status, err := s.manager.git.Run(ctx, s.workspace, []string{"status", "--porcelain"}, nil)
	if err != nil || status != "" {
		reason := "github publish requires a clean worktree"
		s.logPublishRefused(reason, "")
		return Result{}, errors.New(reason)
	}
	head, err := s.manager.git.Run(ctx, s.workspace, []string{"rev-parse", "HEAD"}, nil)
	if err != nil {
		reason := "github publish requires a committed HEAD"
		s.logPublishRefused(reason, "")
		return Result{}, errors.New(reason)
	}
	base, err := s.manager.git.Run(ctx, s.workspace, []string{"rev-parse", "refs/remotes/origin/" + s.settings.BaseBranch}, nil)
	if err != nil || head == base {
		reason := "github publish requires committed changes"
		s.logPublishRefused(reason, head)
		return Result{}, errors.New(reason)
	}
	if _, err := s.manager.git.Run(ctx, s.workspace, []string{"merge-base", "--is-ancestor", base, head}, nil); err != nil {
		reason := "github publish HEAD is not based on the configured base branch"
		s.logPublishRefused(reason, head)
		return Result{}, errors.New(reason)
	}
	existing, found, err := s.manager.findPull(ctx, s.settings, s.branch)
	if err != nil {
		return Result{}, err
	}
	leaseAgainst := ""
	if found && existing.Head.SHA != "" && existing.Head.SHA != head {
		if _, err := s.manager.git.Run(ctx, s.workspace, []string{"cat-file", "-e", existing.Head.SHA + "^{commit}"}, nil); err != nil {
			// The remote head commit was never fetched into this worktree (for
			// example, a human pushed a suggested change through GitHub's UI),
			// so the ancestry check below cannot run and would fail
			// indistinguishably from a rebase. Naming the rebase cause without
			// having established it would hand the agent an instruction that
			// cannot resolve the divergence, so this case is refused with a
			// cause that stops at what is actually known instead.
			reason := "github publish remote branch " + s.branch + " has a head commit this worktree has not fetched, so the cause of the divergence cannot be established here"
			s.logPublishRefused(reason, head)
			return Result{}, errors.New(reason)
		}
		// A published pull request's remote head that HEAD no longer descends
		// from means this worktree rebased instead of merging: a plain push
		// below would be a non-fast-forward Git rejects outright. s.branch is
		// Symphony's own deterministic per-issue branch that nothing else
		// writes, so pushing under --force-with-lease bound to the remote
		// head this call just observed is safe: the lease still fails closed,
		// rather than overwriting, if the branch has moved to anything else
		// by the time the push below runs (PMR-137).
		if _, err := s.manager.git.Run(ctx, s.workspace, []string{"merge-base", "--is-ancestor", existing.Head.SHA, head}, nil); err != nil {
			leaseAgainst = existing.Head.SHA
		}
	}
	auth := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + s.settings.Token))
	env := []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.https://github.com/.extraheader", "GIT_CONFIG_VALUE_0=AUTHORIZATION: basic " + auth}
	remote := "https://github.com/" + s.settings.Owner + "/" + s.settings.Repository + ".git"
	pushArgs := []string{"push"}
	if leaseAgainst != "" {
		pushArgs = append(pushArgs, "--force-with-lease=refs/heads/"+s.branch+":"+leaseAgainst)
	}
	pushArgs = append(pushArgs, remote, "HEAD:refs/heads/"+s.branch)
	if _, err := s.manager.git.Run(ctx, s.workspace, pushArgs, env); err != nil {
		// The agent-facing message stays this fixed, host-authored hint rather
		// than the raw git/GitHub text: every cause diagnosable ahead of the
		// push (dirty worktree, stale base, non-fast-forward) was already
		// refused above, so what reaches here is the remainder -- a transient
		// or remote-side rejection retrying may clear, or a repository push
		// restriction retrying will not -- and the agent cannot act on
		// provider-shaped text any more precisely than on this hint. The real
		// git error (for example GitHub's own "without `workflow` scope"
		// rejection, PMR-163) is not discarded, though: it is attached to the
		// refusal log record below via push_error, so an operator reads the
		// actual diagnosis instead of reconstructing it by hand.
		reason := "github publish could not push branch " + s.branch + " to the configured repository; retry once, and if it persists check the repository's push permissions and branch protection rules"
		s.logPublishRefused(reason, head, "push_error", observability.Text(err.Error()))
		return Result{}, errors.New(reason)
	}
	s.manager.logger.Info("GitHub issue branch published", "issue_id", s.issue.ID, "issue_identifier", s.issue.Identifier, "repository", s.settings.Owner+"/"+s.settings.Repository, "branch", s.branch)
	body := canonicalBody(input, s.issue.URL)
	pr, updated, err := s.manager.publishPullRequest(ctx, s.settings, s.branch, s.issue, body)
	if err != nil {
		return Result{}, err
	}
	s.manager.logger.Info("GitHub pull request reconciled", "issue_id", s.issue.ID, "issue_identifier", s.issue.Identifier, "repository", s.settings.Owner+"/"+s.settings.Repository, "branch", s.branch, "pr_number", pr.Number, "body_updated", updated)
	if err := s.linear.LinkAndHandoff(ctx, pr.URL); err != nil {
		return Result{}, err
	}
	s.manager.track(s.issue, pr, s.settings, s.linear)
	s.manager.logger.Info("GitHub pull request handoff", "issue_id", s.issue.ID, "issue_identifier", s.issue.Identifier, "repository", s.settings.Owner+"/"+s.settings.Repository, "branch", s.branch, "pr_number", pr.Number)
	return Result{Branch: s.branch, URL: pr.URL, Number: pr.Number, BodyUpdated: updated}, nil
}

// ContextResult is the complete bounded, redacted github_pr_context payload.
// Every field is either a fixed identifier already known to the session or a
// size-capped, truncation-flagged summary of upstream GitHub state; it never
// carries raw provider payloads or credentials.
type ContextResult struct {
	Branch      string `json:"branch"`
	PullRequest string `json:"pull_request"`
	Number      int    `json:"number"`
	State       string `json:"state"`

	Checks ChecksResult `json:"checks"`

	ReviewState      string          `json:"review_state"`
	Reviews          []ReviewExcerpt `json:"reviews"`
	ReviewsTruncated bool            `json:"reviews_truncated"`

	Comments          []CommentExcerpt `json:"comments"`
	CommentsTruncated bool             `json:"comments_truncated"`

	UnresolvedThreads int  `json:"unresolved_threads"`
	ThreadsTotal      int  `json:"threads_total"`
	ThreadsTruncated  bool `json:"threads_truncated"`
}

// Context reads bounded check, review, comment, and unresolved-thread state
// for the pull request already bound to this issue, repository, and branch.
// It performs no mutation: a closed or merged pull request is reported as
// found rather than reopened or recreated.
func (s *Session) Context(ctx context.Context) (ContextResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pr, found, err := s.manager.findPull(ctx, s.settings, s.branch)
	if err != nil {
		return ContextResult{}, err
	}
	if !found {
		return ContextResult{}, errors.New("no pull request has been published for this issue yet")
	}
	checks, err := s.manager.checks(ctx, s.settings, pr.Head.SHA)
	if err != nil {
		return ContextResult{}, err
	}
	reviewState, reviews, reviewsTruncated, err := s.manager.reviews(ctx, s.settings, pr.Number)
	if err != nil {
		return ContextResult{}, err
	}
	comments, commentsTruncated, err := s.manager.comments(ctx, s.settings, pr.Number)
	if err != nil {
		return ContextResult{}, err
	}
	unresolved, total, threadsTruncated, err := s.manager.reviewThreads(ctx, s.settings, pr.Number)
	if err != nil {
		return ContextResult{}, err
	}
	return ContextResult{
		Branch: s.branch, PullRequest: pr.URL, Number: pr.Number, State: pr.State,
		Checks:            checks,
		ReviewState:       reviewState,
		Reviews:           reviews,
		ReviewsTruncated:  reviewsTruncated,
		Comments:          comments,
		CommentsTruncated: commentsTruncated,
		UnresolvedThreads: unresolved,
		ThreadsTotal:      total,
		ThreadsTruncated:  threadsTruncated,
	}, nil
}
