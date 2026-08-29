// Package github implements Symphony's optional, fixed-scope GitHub PR
// lifecycle. It deliberately exposes no general GitHub API surface.
package github

import (
	"context"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/domain"
	"github.com/pmrrasmussen/symphony/internal/linear"
	"github.com/pmrrasmussen/symphony/internal/observability"
)

var unsafeBranch = regexp.MustCompile(`[^a-z0-9._-]+`)

// Manager owns linked PR polling and the exactly-once completion guard.
type Manager struct {
	settings func() config.Settings
	client   *http.Client
	git      gitRunner
	logger   *slog.Logger
	mu       sync.Mutex
	// fetchMu serializes every base-ref fetch this manager issues.
	// refs/remotes/origin/<base> and packed-refs live in the shared Git common
	// directory, not in any one session's worktree, so two sessions racing
	// RefreshBaseRef (PMR-141) at once are racing the same repository-wide
	// ref, not independent state the per-Session mu already guards.
	fetchMu sync.Mutex
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
	// for their exact scope and idempotency guarantees. RefuseLanding's reason
	// is the fixed or repository-config derived gate string the caller refused
	// landing for, recorded on the transition log record (PMR-159).
	EnsureMergeState(context.Context, string) error
	RefuseLanding(ctx context.Context, mergeState, reason string) (bool, error)
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

func New(settings func() config.Settings, logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{settings: settings, client: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, git: execGit{settings: settings}, logger: logger, linked: map[string]*link{}}
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
		m.logger.Warn("GitHub landing verification failed", "issue_id", issue.ID, "issue_identifier", issue.Identifier, "repository", s.Owner+"/"+s.Repository, "error", observability.Text(err.Error()))
		return false, err
	}
	if !found || !(pr.Merged || pr.MergedAt != nil) || !strings.EqualFold(strings.TrimSpace(pr.Head.SHA), commit) {
		m.logger.Info("GitHub landing unverified; workspace commits are preserved", "issue_id", issue.ID, "issue_identifier", issue.Identifier, "repository", s.Owner+"/"+s.Repository, "workspace_commit", shortSHA(commit))
		return false, nil
	}
	m.logger.Info("GitHub landing verified for workspace cleanup", "issue_id", issue.ID, "issue_identifier", issue.Identifier, "repository", s.Owner+"/"+s.Repository, "pr_number", pr.Number, "workspace_commit", shortSHA(commit))
	return true, nil
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
