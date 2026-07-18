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
	s := m.settings().GitHub
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
}

// Publish verifies a clean committed worktree, publishes only HEAD to the
// deterministic issue branch, creates/reuses its PR, and performs the bound
// Linear link/review handoff.
func (s *Session) Publish(ctx context.Context) (Result, error) {
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
	pr, err := s.manager.findOrCreate(ctx, s.settings, s.branch, s.issue)
	if err != nil {
		return Result{}, err
	}
	s.manager.logger.Info("GitHub pull request reconciled", "issue_id", s.issue.ID, "issue_identifier", s.issue.Identifier, "repository", s.settings.Owner+"/"+s.settings.Repository, "branch", s.branch, "pr_number", pr.Number)
	if err := s.linear.LinkAndHandoff(ctx, pr.URL); err != nil {
		return Result{}, err
	}
	s.manager.track(s.issue, pr, s.settings, s.linear)
	s.manager.logger.Info("GitHub pull request handoff", "issue_id", s.issue.ID, "issue_identifier", s.issue.Identifier, "repository", s.settings.Owner+"/"+s.settings.Repository, "branch", s.branch, "pr_number", pr.Number)
	return Result{Branch: s.branch, URL: pr.URL, Number: pr.Number}, nil
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
}

func (m *Manager) findOrCreate(ctx context.Context, s config.GitHub, branch string, issue domain.Issue) (pull, error) {
	path := fmt.Sprintf("/repos/%s/%s/pulls?state=all&head=%s%%3A%s&base=%s", s.Owner, s.Repository, s.Owner, branch, s.BaseBranch)
	var pulls []pull
	if err := m.request(ctx, s, http.MethodGet, path, nil, &pulls); err != nil {
		return pull{}, err
	}
	if len(pulls) > 0 {
		if !validPull(s, pulls[0]) {
			return pull{}, errors.New("github returned an invalid pull request")
		}
		return pulls[0], nil
	}
	body := map[string]any{"title": issue.Identifier + ": " + issue.Title, "head": branch, "base": s.BaseBranch, "body": issue.URL}
	var created pull
	if err := m.request(ctx, s, http.MethodPost, fmt.Sprintf("/repos/%s/%s/pulls", s.Owner, s.Repository), body, &created); err != nil {
		return pull{}, err
	}
	if !validPull(s, created) {
		return pull{}, errors.New("github returned an invalid pull request")
	}
	return created, nil
}

func validPull(settings config.GitHub, pr pull) bool {
	parsed, err := url.Parse(pr.URL)
	if err != nil || parsed.Scheme != "https" || !strings.EqualFold(parsed.Host, "github.com") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	want := fmt.Sprintf("/%s/%s/pull/%d", settings.Owner, settings.Repository, pr.Number)
	return pr.Number > 0 && parsed.EscapedPath() == want
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
