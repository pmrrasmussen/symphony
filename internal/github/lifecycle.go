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

// Structured-handoff input bounds. These mirror the maxLength values in the
// github_publish_pr Codex tool schema (internal/codex/backend.go); both sides
// must be kept in sync the same way the linear_graphql comment bound is.
const (
	maxPublishWhyBytes         = 4 << 10
	maxPublishWhatChangedBytes = 8 << 10
	maxPublishOnCallBytes      = 2 << 10
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
	linked   map[string]*link
}

type link struct {
	issueID, identifier  string
	prNumber             int
	prURL                string
	settings             config.GitHub
	linear               linearLifecycle
	completed, closedLog bool
}

type linearLifecycle interface {
	EnsureActive(context.Context) error
	LinkAndHandoff(context.Context, string) error
	Complete(context.Context) (bool, error)
}

func New(settings func() config.Settings, logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{settings: settings, client: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, git: execGit{}, logger: logger, linked: map[string]*link{}}
}

func (m *Manager) Enabled() bool { return m.settings().GitHub.Enabled }

// MatchesSecret allows the Codex launcher to strip the GitHub token and any
// inherited value containing it from the child environment.
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
	branch := "symphony/" + strings.Trim(unsafeBranch.ReplaceAllString(strings.ToLower(issue.Identifier), "-"), "-.")
	if branch == "symphony/" {
		return nil
	}
	return &Session{manager: m, settings: s, issue: issue, workspace: workspace, branch: branch, linear: handoff}
}

type Session struct {
	manager   *Manager
	settings  config.GitHub
	issue     domain.Issue
	workspace string
	branch    string
	linear    linearLifecycle
	mu        sync.Mutex
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
	if why == "" || len([]byte(why)) > maxPublishWhyBytes {
		return PublishInput{}, errors.New("github publish why is empty or too large")
	}
	if whatChanged == "" || len([]byte(whatChanged)) > maxPublishWhatChangedBytes {
		return PublishInput{}, errors.New("github publish what_changed is empty or too large")
	}
	if len([]byte(onCall)) > maxPublishOnCallBytes {
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
	Head     struct {
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

// checks reads the combined commit status and check-run summary for the
// pull request's head commit, bounded and redacted to name/status/conclusion.
func (m *Manager) checks(ctx context.Context, s config.GitHub, sha string) (ChecksResult, error) {
	if strings.TrimSpace(sha) == "" {
		return ChecksResult{}, errors.New("github pull request has no evaluated commit")
	}
	var combined struct {
		State    string `json:"state"`
		Statuses []struct {
			Context string `json:"context"`
			State   string `json:"state"`
		} `json:"statuses"`
	}
	if err := m.request(ctx, s, http.MethodGet, fmt.Sprintf("/repos/%s/%s/commits/%s/status", s.Owner, s.Repository, sha), nil, &combined); err != nil {
		return ChecksResult{}, err
	}
	var runsResponse struct {
		CheckRuns []struct {
			Name       string `json:"name"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
		} `json:"check_runs"`
	}
	if err := m.request(ctx, s, http.MethodGet, fmt.Sprintf("/repos/%s/%s/commits/%s/check-runs", s.Owner, s.Repository, sha), nil, &runsResponse); err != nil {
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

func (m *Manager) track(issue domain.Issue, pr pull, settings config.GitHub, handoff linearLifecycle) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing := m.linked[issue.ID]; existing != nil {
		return
	}
	m.linked[issue.ID] = &link{issueID: issue.ID, identifier: issue.Identifier, prNumber: pr.Number, prURL: pr.URL, settings: settings, linear: handoff}
}

// Poll observes only PRs created/reused by this manager. It never merges.
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
}

func (m *Manager) pollOne(ctx context.Context, linked *link) {
	m.mu.Lock()
	if linked.completed {
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()
	var pr pull
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d", linked.settings.Owner, linked.settings.Repository, linked.prNumber)
	if err := m.request(ctx, linked.settings, http.MethodGet, path, nil, &pr); err != nil {
		m.logger.Warn("GitHub pull request poll failed", "issue_id", linked.issueID, "issue_identifier", linked.identifier, "pr_number", linked.prNumber)
		return
	}
	if pr.Merged || pr.MergedAt != nil {
		changed, err := linked.linear.Complete(ctx)
		if err != nil {
			m.logger.Warn("GitHub merge Linear completion failed", "issue_id", linked.issueID, "issue_identifier", linked.identifier, "pr_number", linked.prNumber)
			return
		}
		m.mu.Lock()
		linked.completed = true
		m.mu.Unlock()
		if changed {
			m.logger.Info("GitHub merge completed Linear issue", "issue_id", linked.issueID, "issue_identifier", linked.identifier, "pr_number", linked.prNumber)
		}
		return
	}
	if strings.EqualFold(pr.State, "closed") {
		m.mu.Lock()
		first := !linked.closedLog
		linked.closedLog = true
		m.mu.Unlock()
		if first {
			m.logger.Warn("GitHub pull request closed without merge; Linear issue remains in review", "issue_id", linked.issueID, "issue_identifier", linked.identifier, "pr_number", linked.prNumber)
		}
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
