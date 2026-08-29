package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"

	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/domain"
)

const maxResponse = 1 << 20

// maxPages bounds how many pages of one paginated GitHub collection a single
// read may follow. At the per_page=100 every paginated caller asks for, the
// cap is 1000 items -- far past any real pull request, so it is a runaway
// guard rather than a working limit. It must stay a guard and not a silent
// answer: a caller whose gate depends on completeness reports the shortfall
// (see fetchChecks, reviews, and reviewThreads) instead of treating the
// truncated page set as the whole collection.
const maxPages = 10

type gitRunner interface {
	Run(context.Context, string, []string, []string) (string, error)
}

type execGit struct{}

// Run returns the real git output as the error's text on failure, rather than
// a generic placeholder: Publish's push gate and Land's stale-branch push
// gate are the two callers that surface this detail, and only to the host
// log (via observability.Text), never to the agent -- see the comment on
// each push failure below.
func (execGit) Run(ctx context.Context, dir string, args, extraEnv []string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), extraEnv...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(out))
		if detail == "" {
			detail = err.Error()
		}
		return "", errors.New(detail)
	}
	return strings.TrimSpace(string(out)), nil
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
//
// It re-runs findPull itself rather than reusing Publish's pre-push lookup:
// the push in between is a network round trip during which the pull request
// can be merged, and a merge in that window must be caught here or Publish
// would PATCH an already-merged pull request's body and hand it off as a
// normal publish.
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

func (m *Manager) request(ctx context.Context, s config.GitHub, method, path string, body any, out any) error {
	_, err := m.requestWithHeader(ctx, s, method, path, body, out)
	return err
}

// paginate walks one GitHub collection across Link-header pages, decoding each
// page into a fresh T and handing it to collect. It reports whether the
// collection was read to its end: false means the page cap stopped the walk
// with a rel="next" link still outstanding, so the collected items are a
// prefix of the collection and not the whole of it.
func paginate[T any](ctx context.Context, m *Manager, s config.GitHub, path string, collect func(T)) (bool, error) {
	for page := 0; page < maxPages; page++ {
		var decoded T
		header, err := m.requestWithHeader(ctx, s, http.MethodGet, path, nil, &decoded)
		if err != nil {
			return false, err
		}
		collect(decoded)
		path = nextPagePath(header, s.Endpoint)
		if path == "" {
			return true, nil
		}
	}
	return false, nil
}

// nextPagePath returns the endpoint-relative path of the Link header's
// rel="next" entry, or "" when there is none. A next URL that does not sit
// under the configured endpoint is not followed: every request carries the
// bearer token, so only the configured host may ever receive one.
func nextPagePath(header http.Header, endpoint string) string {
	prefix := strings.TrimSuffix(endpoint, "/")
	for _, value := range header.Values("Link") {
		for _, link := range strings.Split(value, ",") {
			parts := strings.Split(link, ";")
			if len(parts) < 2 {
				continue
			}
			target := strings.TrimSpace(parts[0])
			if !strings.HasPrefix(target, "<") || !strings.HasSuffix(target, ">") {
				continue
			}
			isNext := false
			for _, param := range parts[1:] {
				if strings.EqualFold(strings.TrimSpace(param), `rel="next"`) {
					isNext = true
				}
			}
			if !isNext {
				continue
			}
			target = strings.TrimSuffix(strings.TrimPrefix(target, "<"), ">")
			if !strings.HasPrefix(target, prefix+"/") {
				return ""
			}
			return strings.TrimPrefix(target, prefix)
		}
	}
	return ""
}

func (m *Manager) requestWithHeader(ctx context.Context, s config.GitHub, method, path string, body any, out any) (http.Header, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, errors.New("encode github request")
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.Endpoint+path, reader)
	if err != nil {
		return nil, errors.New("build github request")
	}
	req.Header.Set("Authorization", "Bearer "+s.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := m.client.Do(req)
	if err != nil {
		return nil, errors.New("github request failed")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("github request failed with status %d", response.StatusCode)
	}
	limited := io.LimitReader(response.Body, maxResponse+1)
	data, err := io.ReadAll(limited)
	if err != nil || len(data) > maxResponse {
		return nil, errors.New("github response was invalid")
	}
	if out != nil && json.Unmarshal(data, out) != nil {
		return nil, errors.New("github response was invalid")
	}
	return response.Header, nil
}
