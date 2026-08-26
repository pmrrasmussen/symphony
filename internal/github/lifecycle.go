// Package github implements Symphony's optional, fixed-scope GitHub PR
// lifecycle. It deliberately exposes no general GitHub API surface.
package github

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/domain"
	"github.com/pmrrasmussen/symphony/internal/linear"
)

const maxResponse = 1 << 20

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

var unsafeBranch = regexp.MustCompile(`[^a-z0-9._-]+`)

type gitRunner interface {
	Run(context.Context, string, []string, []string) (string, error)
}

type execGit struct{}

func (execGit) Run(ctx context.Context, dir string, args, extraEnv []string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), extraEnv...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", errors.New("git operation failed")
	}
	return strings.TrimSpace(string(out)), nil
}

// Manager owns linked PR polling and the exactly-once completion guard.
type Manager struct {
	settings func() config.Settings
	client   *http.Client
	git      gitRunner
	logger   *slog.Logger
	mu       sync.Mutex
	// linked holds, by issue ID, every pull request this manager still has a
	// reason to request. Symphony runs for weeks, so the table is deliberately
	// bounded by a defined end of life rather than by process lifetime
	// (PMR-112): a link leaves it as soon as polling can learn nothing further
	// -- the pull request was observed merged (and reconciled) or closed without
	// merge, or Forget reported the issue terminal. Nothing else evicts. An
	// open, unsettled pull request keeps being polled however old its link is,
	// because evicting one on age would silently stop reconciling a merge that
	// happens later.
	linked map[string]*link
}

type link struct {
	issueID, identifier string
	prNumber            int
	prURL               string
	settings            config.GitHub
	linear              linearLifecycle
	// settled marks the end of this link's life: the pull request reached a
	// terminal observation, so Poll sweeps the link out and no later tick
	// requests it again. It is also the exactly-once completion guard, because
	// the merged branch sets it only after ReconcileMerged returned: a
	// reconciliation that failed leaves the link live and is retried next tick.
	settled bool
}

type linearLifecycle interface {
	EnsureActive(context.Context) error
	LinkAndHandoff(context.Context, string) error
	Complete(context.Context) (bool, error)
	// ReconcileMerged reconciles a poll-observed merged PR to Done from either
	// the review handoff target or the configured Merging state, idempotently
	// and human-wins. See internal/linear.HandoffSession for its exact scope.
	ReconcileMerged(context.Context, string) (bool, error)
	// EnsureMergeState, RefuseLanding, and CompleteLanding back the
	// github_land_pr capability (PMR-37). See internal/linear.HandoffSession
	// for their exact scope and idempotency guarantees.
	EnsureMergeState(context.Context, string) error
	RefuseLanding(context.Context, string) (bool, error)
	CompleteLanding(context.Context, string) (bool, error)
	// LandComment adds a bounded, host-generated audit comment to the bound
	// issue (pushed commit SHAs during a fix turn, and the last failed gate
	// when landing is finally refused). The body is never Codex-supplied.
	LandComment(context.Context, string) error
}

// The one production implementation of that seam. Session preparation already
// binds the concrete type, so this compiles today either way; stating it at the
// interface keeps the claim visible where the seam is defined -- this package's
// tests substitute a fake for internal/linear.HandoffSession, never the reverse
// -- and keeps it asserted if preparation ever stops taking the concrete type.
var _ linearLifecycle = (*linear.HandoffSession)(nil)

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

func New(settings func() config.Settings, logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{settings: settings, client: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, git: execGit{}, logger: logger, linked: map[string]*link{}}
}

func (m *Manager) Enabled() bool { return m.settings().GitHub.Enabled }

// MatchesSecret allows a launcher to strip the GitHub token and any inherited
// value containing it from the child environment. It is the fallback
// capability.SecretMatcher uses for a run that bound this manager but prepared
// no Session, which is every run with github.enabled and no Linear handoff.
func (m *Manager) MatchesSecret(candidate string) bool {
	token := m.settings().GitHub.Token
	return token != "" && strings.Contains(candidate, token)
}

// Prepare freezes all authority for one active issue and worktree.
func (m *Manager) Prepare(issue domain.Issue, workspace string, handoff *linear.HandoffSession) *Session {
	return m.PrepareWithSettings(m.settings().GitHub, issue, workspace, handoff)
}

// PrepareWithSettings freezes the host capability to the same configuration
// snapshot that selected the other Codex session capabilities.
func (m *Manager) PrepareWithSettings(s config.GitHub, issue domain.Issue, workspace string, handoff *linear.HandoffSession) *Session {
	if !s.Enabled || handoff == nil || strings.TrimSpace(issue.ID) == "" || strings.TrimSpace(issue.Identifier) == "" || strings.TrimSpace(workspace) == "" {
		return nil
	}
	branch, ok := issueBranch(issue)
	if !ok {
		return nil
	}
	return &Session{manager: m, settings: s, issue: issue, workspace: workspace, branch: branch, linear: handoff}
}

// issueBranch derives the one deterministic branch name Symphony ever uses for
// an issue. It is the single definition shared by session preparation and the
// read-only landing verification, so neither can look at a different branch.
func issueBranch(issue domain.Issue) (string, bool) {
	branch := "symphony/" + strings.Trim(unsafeBranch.ReplaceAllString(strings.ToLower(issue.Identifier), "-"), "-.")
	if branch == "symphony/" {
		return "", false
	}
	return branch, true
}

// VerifyLanded implements domain.LandingVerifier for terminal workspace
// cleanup. It is strictly read-only: it resolves the one deterministic pull
// request for the issue's bound branch in the configured repository and reports
// true only when that pull request is merged and its head commit is exactly the
// commit still checked out in the workspace. A pull request's head commit is the
// source branch tip, which GitHub does not rewrite when it squashes or rebases
// onto the base branch, so this holds under every github.merge_method. A
// locally amended or rebased HEAD, a commit never pushed to the bound branch, a
// closed-unmerged or missing pull request, a disabled GitHub integration, and
// any request failure all report false or an error, so cleanup keeps the
// committed work for manual review.
func (m *Manager) VerifyLanded(ctx context.Context, issue domain.Issue, commit string) (bool, error) {
	s := m.settings().GitHub
	commit = strings.TrimSpace(commit)
	if !s.Enabled || commit == "" {
		return false, nil
	}
	branch, ok := issueBranch(issue)
	if !ok {
		return false, nil
	}
	pr, found, err := m.findPull(ctx, s, branch)
	if err != nil {
		m.logger.Warn("GitHub landing verification failed", "issue_id", issue.ID, "issue_identifier", issue.Identifier, "repository", s.Owner+"/"+s.Repository)
		return false, err
	}
	if !found || !(pr.Merged || pr.MergedAt != nil) || !strings.EqualFold(strings.TrimSpace(pr.Head.SHA), commit) {
		m.logger.Info("GitHub landing unverified; workspace commits are preserved", "issue_id", issue.ID, "issue_identifier", issue.Identifier, "repository", s.Owner+"/"+s.Repository, "workspace_commit", shortSHA(commit))
		return false, nil
	}
	m.logger.Info("GitHub landing verified for workspace cleanup", "issue_id", issue.ID, "issue_identifier", issue.Identifier, "repository", s.Owner+"/"+s.Repository, "pr_number", pr.Number, "workspace_commit", shortSHA(commit))
	return true, nil
}

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

// Publish verifies a clean committed worktree, publishes only HEAD to the
// deterministic issue branch, creates/reuses its PR with the canonical
// structured body, and performs the bound Linear link/review handoff.
func (s *Session) Publish(ctx context.Context, input PublishInput) (Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.linear.EnsureActive(ctx); err != nil {
		return Result{}, err
	}
	origin, err := s.manager.git.Run(ctx, s.workspace, []string{"remote", "get-url", "origin"}, nil)
	if err != nil || !matchesRepository(origin, s.settings.Owner, s.settings.Repository) {
		return Result{}, errors.New("github publish worktree origin does not match the configured repository")
	}
	status, err := s.manager.git.Run(ctx, s.workspace, []string{"status", "--porcelain"}, nil)
	if err != nil || status != "" {
		return Result{}, errors.New("github publish requires a clean worktree")
	}
	head, err := s.manager.git.Run(ctx, s.workspace, []string{"rev-parse", "HEAD"}, nil)
	if err != nil {
		return Result{}, errors.New("github publish requires a committed HEAD")
	}
	base, err := s.manager.git.Run(ctx, s.workspace, []string{"rev-parse", "refs/remotes/origin/" + s.settings.BaseBranch}, nil)
	if err != nil || head == base {
		return Result{}, errors.New("github publish requires committed changes")
	}
	if _, err := s.manager.git.Run(ctx, s.workspace, []string{"merge-base", "--is-ancestor", base, head}, nil); err != nil {
		return Result{}, errors.New("github publish HEAD is not based on the configured base branch")
	}
	auth := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + s.settings.Token))
	env := []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.https://github.com/.extraheader", "GIT_CONFIG_VALUE_0=AUTHORIZATION: basic " + auth}
	remote := "https://github.com/" + s.settings.Owner + "/" + s.settings.Repository + ".git"
	if _, err := s.manager.git.Run(ctx, s.workspace, []string{"push", remote, "HEAD:refs/heads/" + s.branch}, env); err != nil {
		return Result{}, err
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

// ChecksResult is the bounded, redacted status of the commit under review.
type ChecksResult struct {
	OverallState string     `json:"overall_state"`
	Runs         []CheckRun `json:"runs"`
	Total        int        `json:"total"`
	Truncated    bool       `json:"truncated"`
}

type CheckRun struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

type ReviewExcerpt struct {
	Author      string `json:"author"`
	State       string `json:"state"`
	Body        string `json:"body_excerpt"`
	SubmittedAt string `json:"submitted_at"`
}

type CommentExcerpt struct {
	Author    string `json:"author"`
	Body      string `json:"body_excerpt"`
	CreatedAt string `json:"created_at"`
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

// Land merges the pull request already bound to this issue, repository,
// base, and branch, using the configured merge method, once required checks
// pass, the effective review state is not changes_requested, no review
// thread is unresolved, the pull request is open and mergeable, and the
// configured base has not moved. It accepts no input: repository, branch,
// PR, method, and Linear state are all fixed by the bound session and
// configuration, never by tool arguments.
//
// A hard gate (failing checks, a changes-requested review, an unresolved
// thread, a stale base, a merge conflict, or a closed/mismatched pull
// request) refuses landing and attempts the configured Merging -> In Review
// fallback transition, which is itself a no-op once the issue is no longer
// exactly in the configured Merging state. When UpdateStaleBranch is enabled,
// a clean stale base instead gets one deterministic update-branch attempt and
// then waits for checks on its new head. Pending checks or undetermined
// mergeability return a non-terminal LandWaiting result without mutating
// Linear. A pull request GitHub already reports merged -- discovered up
// front, immediately before the merge call, or because the merge call itself
// raced with a concurrent merge -- reconciles the bound issue to Done
// idempotently instead of attempting another merge.
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
				return LandResult{}, err
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
	waiting := false
	var failing []string
	for _, name := range s.settings.RequiredChecks {
		switch outcomes[strings.ToLower(strings.TrimSpace(name))] {
		case checkMissing, checkPending:
			waiting = true
		case checkFailed:
			failing = append(failing, name)
		}
	}
	if len(failing) > 0 {
		return s.gate(ctx, "github required checks failed: "+strings.Join(failing, ", "), true)
	}
	if waiting {
		return s.waiting(fresh.Number, fresh.URL, "required checks are pending"), nil
	}

	// Moving the issue to Merging is the human approval to land (see policy
	// in the issue): no additional approving review is required here, only
	// the absence of an effective changes-requested review.
	reviewState, _, _, err := s.manager.reviews(ctx, s.settings, fresh.Number)
	if err != nil {
		return LandResult{}, err
	}
	if reviewState == "changes_requested" {
		return s.refuse(ctx, "github pull request has an effective changes-requested review")
	}
	unresolved, _, _, err := s.manager.reviewThreads(ctx, s.settings, fresh.Number)
	if err != nil {
		return LandResult{}, err
	}
	if unresolved > 0 {
		return s.gate(ctx, "github pull request has unresolved review threads", true)
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
	if _, err := s.linear.RefuseLanding(ctx, s.settings.MergeState); err != nil {
		s.manager.logger.Warn("GitHub land Merging fallback transition failed", "issue_id", s.issue.ID, "issue_identifier", s.issue.Identifier, "reason", reason)
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
	if _, err := s.linear.RefuseLanding(ctx, s.settings.MergeState); err != nil {
		s.manager.logger.Warn("GitHub land deferred Merging fallback transition failed", "issue_id", s.issue.ID, "issue_identifier", s.issue.Identifier)
	}
	if err := s.linear.LandComment(ctx, landingRefusalComment(s.lastFailedGate)); err != nil {
		s.manager.logger.Warn("GitHub land refusal comment failed", "issue_id", s.issue.ID, "issue_identifier", s.issue.Identifier)
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
// which one carries the weight will otherwise get it wrong.
//
// The feature check is unreachable by construction and kept only as a statement
// of intent: retryableGateHit is set exclusively by gate(), which returns through
// refuse() before setting it whenever the feature is off, so no session can ever
// reach here with the feature off and a gate on record.
//
// The landed check is genuinely redundant with fireDeferredRefusal's own, and
// deliberately so: this is the guard a reader of this function needs to see, and
// the case it covers -- a gate hit, then a fix turn's retry that merged -- is the
// highest-consequence mistake available here, since firing would walk a merged,
// Done issue back to review with a comment claiming fix attempts were exhausted.
// Both transports assert that case end to end.
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
		s.manager.logger.Warn("GitHub land push audit Linear comment failed", "issue_id", s.issue.ID, "issue_identifier", s.issue.Identifier, "pr_number", prNumber)
	}
	if err := s.manager.commentPR(ctx, s.settings, prNumber, body); err != nil {
		s.manager.logger.Warn("GitHub land push audit PR comment failed", "issue_id", s.issue.ID, "issue_identifier", s.issue.Identifier, "pr_number", prNumber)
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

func shortSHA(sha string) string {
	sha = strings.TrimSpace(sha)
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func matchesRepository(remote, owner, repository string) bool {
	remote = strings.TrimSpace(remote)
	if strings.HasPrefix(remote, "git@github.com:") {
		return matchesRepositoryPath(strings.TrimPrefix(remote, "git@github.com:"), owner, repository)
	}
	parsed, err := url.Parse(remote)
	if err != nil || !strings.EqualFold(parsed.Hostname(), "github.com") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	switch parsed.Scheme {
	case "https":
		if parsed.User != nil || parsed.Port() != "" {
			return false
		}
	case "ssh":
		if parsed.User == nil || parsed.User.Username() != "git" || parsed.User.String() != "git" || parsed.Port() != "" {
			return false
		}
	default:
		return false
	}
	return matchesRepositoryPath(strings.TrimPrefix(parsed.EscapedPath(), "/"), owner, repository)
}

func matchesRepositoryPath(path, owner, repository string) bool {
	if strings.Contains(path, "%") {
		return false
	}
	path = strings.TrimSuffix(strings.TrimSuffix(path, "/"), ".git")
	parts := strings.Split(path, "/")
	return len(parts) == 2 && strings.EqualFold(parts[0], owner) && strings.EqualFold(parts[1], repository)
}

type pull struct {
	Number   int    `json:"number"`
	URL      string `json:"html_url"`
	State    string `json:"state"`
	Merged   bool   `json:"merged"`
	MergedAt any    `json:"merged_at"`
	Body     string `json:"body"`
	// Mergeable and MergeableState are only ever populated by the single
	// pull-request GET used by github_land_pr; GitHub's list endpoint never
	// returns them, so they stay nil/zero everywhere else.
	Mergeable      *bool  `json:"mergeable"`
	MergeableState string `json:"mergeable_state"`
	Head           struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

// findPull resolves at most the one deterministic pull request for the
// bound branch. It performs no mutation and no creation; a mismatched head,
// base, repository, or an ambiguous result is rejected rather than reused.
func (m *Manager) findPull(ctx context.Context, s config.GitHub, branch string) (pull, bool, error) {
	path := fmt.Sprintf("/repos/%s/%s/pulls?state=all&head=%s%%3A%s&base=%s", s.Owner, s.Repository, s.Owner, branch, s.BaseBranch)
	var pulls []pull
	if err := m.request(ctx, s, http.MethodGet, path, nil, &pulls); err != nil {
		return pull{}, false, err
	}
	if len(pulls) == 0 {
		return pull{}, false, nil
	}
	if len(pulls) > 1 {
		return pull{}, false, errors.New("github returned more than one pull request for the bound branch")
	}
	pr := pulls[0]
	if !validPull(s, branch, pr) {
		return pull{}, false, errors.New("github returned a mismatched pull request")
	}
	return pr, true, nil
}

// publishPullRequest creates the deterministic pull request when none
// exists, reopens an issue-bound pull request that was closed without being
// merged, and updates the body only when the canonical structured fields
// changed. A pull request already merged is irrecoverable and rejected.
func (m *Manager) publishPullRequest(ctx context.Context, s config.GitHub, branch string, issue domain.Issue, body string) (pull, bool, error) {
	existing, found, err := m.findPull(ctx, s, branch)
	if err != nil {
		return pull{}, false, err
	}
	if found {
		if existing.Merged || existing.MergedAt != nil {
			return pull{}, false, errors.New("github pull request for this issue was already merged and cannot be reused")
		}
		if strings.EqualFold(existing.State, "closed") {
			reopened, err := m.setState(ctx, s, existing.Number, "open")
			if err != nil {
				return pull{}, false, errors.New("github pull request for this issue is closed and could not be reopened")
			}
			if !validPull(s, branch, reopened) {
				return pull{}, false, errors.New("github returned a mismatched pull request")
			}
			existing = reopened
		}
		if existing.Body == body {
			return existing, false, nil
		}
		updated, err := m.updateBody(ctx, s, existing.Number, body)
		if err != nil {
			return pull{}, false, err
		}
		if !validPull(s, branch, updated) {
			return pull{}, false, errors.New("github returned a mismatched pull request")
		}
		return updated, true, nil
	}
	requestBody := map[string]any{"title": issue.Identifier + ": " + issue.Title, "head": branch, "base": s.BaseBranch, "body": body}
	var created pull
	if err := m.request(ctx, s, http.MethodPost, fmt.Sprintf("/repos/%s/%s/pulls", s.Owner, s.Repository), requestBody, &created); err != nil {
		return pull{}, false, err
	}
	if !validPull(s, branch, created) {
		return pull{}, false, errors.New("github returned an invalid pull request")
	}
	return created, true, nil
}

func (m *Manager) setState(ctx context.Context, s config.GitHub, number int, state string) (pull, error) {
	var updated pull
	if err := m.request(ctx, s, http.MethodPatch, fmt.Sprintf("/repos/%s/%s/pulls/%d", s.Owner, s.Repository, number), map[string]any{"state": state}, &updated); err != nil {
		return pull{}, err
	}
	return updated, nil
}

func (m *Manager) updateBody(ctx context.Context, s config.GitHub, number int, body string) (pull, error) {
	var updated pull
	if err := m.request(ctx, s, http.MethodPatch, fmt.Sprintf("/repos/%s/%s/pulls/%d", s.Owner, s.Repository, number), map[string]any{"body": body}, &updated); err != nil {
		return pull{}, err
	}
	return updated, nil
}

// commentPR posts a bounded, host-generated issue-level comment on the pull
// request. It is used only for the github_land_pr fix-turn audit trail; the
// body is never Codex-supplied.
func (m *Manager) commentPR(ctx context.Context, s config.GitHub, number int, body string) error {
	return m.request(ctx, s, http.MethodPost, fmt.Sprintf("/repos/%s/%s/issues/%d/comments", s.Owner, s.Repository, number), map[string]any{"body": body}, nil)
}

// getPull reads the single pull request by number. Unlike findPull (which
// lists by head/base to resolve the one deterministic PR), this is the only
// GitHub endpoint that reports mergeable/mergeable_state, so github_land_pr
// uses it for its immediate pre-merge read.
func (m *Manager) getPull(ctx context.Context, s config.GitHub, number int) (pull, error) {
	var pr pull
	if err := m.request(ctx, s, http.MethodGet, fmt.Sprintf("/repos/%s/%s/pulls/%d", s.Owner, s.Repository, number), nil, &pr); err != nil {
		return pull{}, err
	}
	return pr, nil
}

// mergePull performs the one irreversible landing mutation: merging the pull
// request with the configured method, pinned to the exact expected head
// commit so GitHub rejects the call if the head changed underneath it.
func (m *Manager) mergePull(ctx context.Context, s config.GitHub, number int, method, sha string) (bool, error) {
	var response struct {
		Merged  bool   `json:"merged"`
		Message string `json:"message"`
	}
	if err := m.request(ctx, s, http.MethodPut, fmt.Sprintf("/repos/%s/%s/pulls/%d/merge", s.Owner, s.Repository, number), map[string]any{"merge_method": method, "sha": sha}, &response); err != nil {
		return false, err
	}
	return response.Merged, nil
}

// updatePullBranch invokes GitHub's deterministic update-branch API. The
// endpoint merges the current base into the PR head and accepts only when the
// branch still points at sha.
func (m *Manager) updatePullBranch(ctx context.Context, s config.GitHub, number int, sha string) error {
	return m.request(ctx, s, http.MethodPut, fmt.Sprintf("/repos/%s/%s/pulls/%d/update-branch", s.Owner, s.Repository, number), map[string]any{"expected_head_sha": sha}, nil)
}

// checkOutcome classifies one required check's state for github_land_pr
// gating: missing (never reported), pending (queued/in-progress/pending),
// passed (success/neutral), or failed (anything else, including a
// cancelled, timed-out, or action-required check run).
type checkOutcome int

const (
	checkMissing checkOutcome = iota
	checkPending
	checkPassed
	checkFailed
)

func statusStateOutcome(state string) checkOutcome {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "success":
		return checkPassed
	case "pending":
		return checkPending
	default:
		return checkFailed
	}
}

func checkRunOutcome(status, conclusion string) checkOutcome {
	if !strings.EqualFold(strings.TrimSpace(status), "completed") {
		return checkPending
	}
	switch strings.ToLower(strings.TrimSpace(conclusion)) {
	case "success", "neutral":
		return checkPassed
	default:
		return checkFailed
	}
}

// requiredCheckOutcomes reads the exact-named required checks configured by
// github.required_checks and classifies each by name (case-insensitively)
// against both the combined-status and check-run tables. A required name
// that never appears in either table stays checkMissing (treated the same
// as pending: github_land_pr waits rather than refuses).
func (m *Manager) requiredCheckOutcomes(ctx context.Context, s config.GitHub, sha string, required []string) (map[string]checkOutcome, error) {
	outcomes := make(map[string]checkOutcome, len(required))
	for _, name := range required {
		outcomes[strings.ToLower(strings.TrimSpace(name))] = checkMissing
	}
	combined, runsResponse, err := m.fetchChecks(ctx, s, sha)
	if err != nil {
		return nil, err
	}
	for _, status := range combined.Statuses {
		key := strings.ToLower(strings.TrimSpace(status.Context))
		if _, wanted := outcomes[key]; wanted {
			outcomes[key] = statusStateOutcome(status.State)
		}
	}
	for _, run := range runsResponse.CheckRuns {
		key := strings.ToLower(strings.TrimSpace(run.Name))
		if _, wanted := outcomes[key]; wanted {
			outcomes[key] = checkRunOutcome(run.Status, run.Conclusion)
		}
	}
	return outcomes, nil
}

func validPull(settings config.GitHub, branch string, pr pull) bool {
	parsed, err := url.Parse(pr.URL)
	if err != nil || parsed.Scheme != "https" || !strings.EqualFold(parsed.Host, "github.com") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	want := fmt.Sprintf("/%s/%s/pull/%d", settings.Owner, settings.Repository, pr.Number)
	if pr.Number <= 0 || parsed.EscapedPath() != want {
		return false
	}
	if strings.TrimSpace(pr.Head.Ref) != branch || strings.TrimSpace(pr.Base.Ref) != settings.BaseBranch {
		return false
	}
	return true
}

type combinedStatus struct {
	State    string `json:"state"`
	Statuses []struct {
		Context string `json:"context"`
		State   string `json:"state"`
	} `json:"statuses"`
}

type checkRunsResponse struct {
	CheckRuns []struct {
		Name       string `json:"name"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
	} `json:"check_runs"`
}

// fetchChecks reads the combined commit status and check-run tables for one
// commit. Both github_pr_context (bounded/redacted display) and
// github_land_pr (exact-name required-check gating) build on this single
// fetch.
func (m *Manager) fetchChecks(ctx context.Context, s config.GitHub, sha string) (combinedStatus, checkRunsResponse, error) {
	if strings.TrimSpace(sha) == "" {
		return combinedStatus{}, checkRunsResponse{}, errors.New("github pull request has no evaluated commit")
	}
	var combined combinedStatus
	if err := m.request(ctx, s, http.MethodGet, fmt.Sprintf("/repos/%s/%s/commits/%s/status", s.Owner, s.Repository, sha), nil, &combined); err != nil {
		return combinedStatus{}, checkRunsResponse{}, err
	}
	var runsResponse checkRunsResponse
	if err := m.request(ctx, s, http.MethodGet, fmt.Sprintf("/repos/%s/%s/commits/%s/check-runs", s.Owner, s.Repository, sha), nil, &runsResponse); err != nil {
		return combinedStatus{}, checkRunsResponse{}, err
	}
	return combined, runsResponse, nil
}

// checks reads the combined commit status and check-run summary for the
// pull request's head commit, bounded and redacted to name/status/conclusion.
func (m *Manager) checks(ctx context.Context, s config.GitHub, sha string) (ChecksResult, error) {
	combined, runsResponse, err := m.fetchChecks(ctx, s, sha)
	if err != nil {
		return ChecksResult{}, err
	}
	total := len(combined.Statuses) + len(runsResponse.CheckRuns)
	runs := make([]CheckRun, 0, total)
	for _, status := range combined.Statuses {
		runs = append(runs, CheckRun{Name: boundedText(status.Context), Status: "status", Conclusion: boundedText(status.State)})
	}
	for _, run := range runsResponse.CheckRuns {
		runs = append(runs, CheckRun{Name: boundedText(run.Name), Status: boundedText(run.Status), Conclusion: boundedText(run.Conclusion)})
	}
	truncated := len(runs) > contextMaxItems
	if truncated {
		runs = runs[:contextMaxItems]
	}
	return ChecksResult{OverallState: boundedText(combined.State), Runs: runs, Total: total, Truncated: truncated}, nil
}

// reviews reads pull request reviews and computes the effective review state
// from each reviewer's most recent review, mirroring GitHub's own
// approve/changes-requested precedence.
func (m *Manager) reviews(ctx context.Context, s config.GitHub, number int) (string, []ReviewExcerpt, bool, error) {
	var raw []struct {
		User struct {
			Login string `json:"login"`
		} `json:"user"`
		State       string `json:"state"`
		Body        string `json:"body"`
		SubmittedAt string `json:"submitted_at"`
	}
	if err := m.request(ctx, s, http.MethodGet, fmt.Sprintf("/repos/%s/%s/pulls/%d/reviews?per_page=100", s.Owner, s.Repository, number), nil, &raw); err != nil {
		return "", nil, false, err
	}
	latestIndex := map[string]int{}
	order := make([]string, 0, len(raw))
	for i, review := range raw {
		login := strings.TrimSpace(review.User.Login)
		if login == "" {
			continue
		}
		if _, exists := latestIndex[login]; !exists {
			order = append(order, login)
		}
		latestIndex[login] = i
	}
	changesRequested, approved := false, false
	for _, login := range order {
		switch strings.ToUpper(strings.TrimSpace(raw[latestIndex[login]].State)) {
		case "CHANGES_REQUESTED":
			changesRequested = true
		case "APPROVED":
			approved = true
		}
	}
	state := "pending"
	switch {
	case changesRequested:
		state = "changes_requested"
	case approved:
		state = "approved"
	}
	start := 0
	truncated := len(raw) > contextMaxItems
	if truncated {
		start = len(raw) - contextMaxItems
	}
	excerpts := make([]ReviewExcerpt, 0, len(raw)-start)
	for _, review := range raw[start:] {
		excerpts = append(excerpts, ReviewExcerpt{
			Author:      boundedText(review.User.Login),
			State:       boundedText(review.State),
			Body:        boundedText(review.Body),
			SubmittedAt: boundedText(review.SubmittedAt),
		})
	}
	return state, excerpts, truncated, nil
}

// comments reads issue-level pull request comments, bounded to the most
// recent contextMaxItems with redacted, size-capped excerpts.
func (m *Manager) comments(ctx context.Context, s config.GitHub, number int) ([]CommentExcerpt, bool, error) {
	var raw []struct {
		User struct {
			Login string `json:"login"`
		} `json:"user"`
		Body      string `json:"body"`
		CreatedAt string `json:"created_at"`
	}
	if err := m.request(ctx, s, http.MethodGet, fmt.Sprintf("/repos/%s/%s/issues/%d/comments?per_page=100", s.Owner, s.Repository, number), nil, &raw); err != nil {
		return nil, false, err
	}
	start := 0
	truncated := len(raw) > contextMaxItems
	if truncated {
		start = len(raw) - contextMaxItems
	}
	out := make([]CommentExcerpt, 0, len(raw)-start)
	for _, comment := range raw[start:] {
		out = append(out, CommentExcerpt{Author: boundedText(comment.User.Login), Body: boundedText(comment.Body), CreatedAt: boundedText(comment.CreatedAt)})
	}
	return out, truncated, nil
}

// reviewThreads reads unresolved review-thread metadata over the GitHub
// GraphQL API, the only surface that exposes thread resolution state. The
// query is fixed; owner, repository, and number always come from the bound
// session, never from tool input.
func (m *Manager) reviewThreads(ctx context.Context, s config.GitHub, number int) (unresolved, total int, truncated bool, err error) {
	payload := map[string]any{
		"query":     reviewThreadsQuery,
		"variables": map[string]any{"owner": s.Owner, "repository": s.Repository, "number": number},
	}
	var response struct {
		Data struct {
			Repository struct {
				PullRequest struct {
					ReviewThreads struct {
						TotalCount int `json:"totalCount"`
						Nodes      []struct {
							IsResolved bool `json:"isResolved"`
						} `json:"nodes"`
					} `json:"reviewThreads"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := m.request(ctx, s, http.MethodPost, "/graphql", payload, &response); err != nil {
		return 0, 0, false, err
	}
	if len(response.Errors) > 0 {
		return 0, 0, false, errors.New("github graphql request failed")
	}
	nodes := response.Data.Repository.PullRequest.ReviewThreads.Nodes
	for _, node := range nodes {
		if !node.IsResolved {
			unresolved++
		}
	}
	total = response.Data.Repository.PullRequest.ReviewThreads.TotalCount
	return unresolved, total, total > len(nodes), nil
}

const reviewThreadsQuery = `query($owner: String!, $repository: String!, $number: Int!) { repository(owner: $owner, name: $repository) { pullRequest(number: $number) { reviewThreads(first: 100) { totalCount nodes { isResolved } } } } }`

// boundedText trims and rune-caps a redacted excerpt so no single upstream
// field can inflate the child-visible response.
func boundedText(value string) string {
	value = strings.TrimSpace(value)
	runes := []rune(value)
	if len(runes) <= contextExcerptRunes {
		return value
	}
	return string(runes[:contextExcerptRunes]) + "…(truncated)"
}

func (m *Manager) request(ctx context.Context, s config.GitHub, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return errors.New("encode github request")
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.Endpoint+path, reader)
	if err != nil {
		return errors.New("build github request")
	}
	req.Header.Set("Authorization", "Bearer "+s.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := m.client.Do(req)
	if err != nil {
		return errors.New("github request failed")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("github request failed with status %d", response.StatusCode)
	}
	limited := io.LimitReader(response.Body, maxResponse+1)
	data, err := io.ReadAll(limited)
	if err != nil || len(data) > maxResponse {
		return errors.New("github response was invalid")
	}
	if out != nil && json.Unmarshal(data, out) != nil {
		return errors.New("github response was invalid")
	}
	return nil
}

// track begins polling the pull request just published for an issue. It is
// idempotent for a live link -- republication reuses the same deterministic
// pull request, so the first link already describes it -- and deliberately not
// idempotent for a settled one: an issue whose link settled (a closed-unmerged
// pull request this publication just reopened) or was forgotten is tracked
// afresh rather than left pointing at a link Poll is about to sweep.
func (m *Manager) track(issue domain.Issue, pr pull, settings config.GitHub, handoff linearLifecycle) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing := m.linked[issue.ID]; existing != nil && !existing.settled {
		return
	}
	m.linked[issue.ID] = &link{issueID: issue.ID, identifier: issue.Identifier, prNumber: pr.Number, prURL: pr.URL, settings: settings, linear: handoff}
}

// Forget drops the linked pull request for an issue that reached a terminal
// tracker state and will never be dispatched again. It is the host's explicit
// end-of-life signal for a link whose pull request is still open: Symphony
// never evicts one on age, because a merge that lands later must still
// reconcile. Unknown issue IDs are ignored, and a later republication for the
// same issue re-tracks it.
func (m *Manager) Forget(issueID string) {
	m.mu.Lock()
	forgotten := m.linked[issueID]
	delete(m.linked, issueID)
	m.mu.Unlock()
	if forgotten != nil {
		m.logger.Info("GitHub pull request polling stopped for terminal issue", "issue_id", forgotten.issueID, "issue_identifier", forgotten.identifier, "pr_number", forgotten.prNumber)
	}
}

// Poll observes only PRs created/reused by this manager. It never merges. The
// walk ends by sweeping out every link that settled during it, so a terminal
// pull request is observed once more and then never requested again.
func (m *Manager) Poll(ctx context.Context) {
	m.mu.Lock()
	links := make([]*link, 0, len(m.linked))
	for _, linked := range m.linked {
		links = append(links, linked)
	}
	m.mu.Unlock()
	for _, linked := range links {
		m.pollOne(ctx, linked)
	}
	m.sweep(links)
}

// sweep removes the links that settled during one walk. It matches on pointer
// identity rather than issue ID, so a republication that re-tracked the same
// issue mid-walk keeps its fresh link instead of losing it to the settled one
// this walk observed.
func (m *Manager) sweep(links []*link) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, linked := range links {
		if linked.settled && m.linked[linked.issueID] == linked {
			delete(m.linked, linked.issueID)
		}
	}
}

func (m *Manager) pollOne(ctx context.Context, linked *link) {
	m.mu.Lock()
	if linked.settled {
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()
	pr, err := m.getPull(ctx, linked.settings, linked.prNumber)
	if err != nil {
		m.logger.Warn("GitHub pull request poll failed", "issue_id", linked.issueID, "issue_identifier", linked.identifier, "pr_number", linked.prNumber)
		return
	}
	if pr.Merged || pr.MergedAt != nil {
		// Reconcile to Done from either the review handoff target or, when
		// landing is configured, the Merging state. The Merging path is
		// fail-closed: an unconfigured landing block (empty MergeState) keeps
		// the reconciliation to the review-target state alone.
		changed, err := linked.linear.ReconcileMerged(ctx, linked.settings.MergeState)
		if err != nil {
			m.logger.Warn("GitHub merge Linear completion failed", "issue_id", linked.issueID, "issue_identifier", linked.identifier, "pr_number", linked.prNumber)
			return
		}
		m.mu.Lock()
		linked.settled = true
		m.mu.Unlock()
		if changed {
			m.logger.Info("GitHub merge completed Linear issue", "issue_id", linked.issueID, "issue_identifier", linked.identifier, "pr_number", linked.prNumber)
		}
		return
	}
	if strings.EqualFold(pr.State, "closed") {
		// Closed without merge is terminal for polling too: a pull request
		// Symphony will not reopen on its own cannot reach merged through any
		// path Symphony still drives. Rework republishes, which reopens it and
		// re-tracks the issue afresh; a human who instead reopens and merges it
		// out of band leaves the issue in review for a human to finish on the
		// board, which is the hand this warning already puts it in. The warning
		// therefore fires exactly once without a log-suppression flag.
		m.mu.Lock()
		linked.settled = true
		m.mu.Unlock()
		m.logger.Warn("GitHub pull request closed without merge; Linear issue remains in review", "issue_id", linked.issueID, "issue_identifier", linked.identifier, "pr_number", linked.prNumber)
	}
}

func (m *Manager) Run(ctx context.Context) {
	for {
		interval := m.settings().GitHub.PollInterval
		if interval <= 0 {
			interval = 30 * time.Second
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			m.Poll(ctx)
		}
	}
}
