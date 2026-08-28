package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/pmrrasmussen/symphony/internal/config"
)

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
// that never appears in either table stays checkMissing: github_land_pr
// still waits rather than refuses (a genuinely slow check and a name that
// will never report are both possible from a single snapshot), but
// requiredCheckWaitReason surfaces the distinction in the wait reason so a
// stuck landing is diagnosable instead of indistinguishable from a slow one.
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

// requiredCheckWaitReason distinguishes a required check that has never
// reported -- most likely a typo in required_checks, a renamed CI job, or a
// workflow whose job is skipped on this path, none of which will ever
// resolve on their own -- from one that is genuinely still running. Only the
// purely-missing case gets the more specific reason: any pending check keeps
// the original "required checks are pending" reason, since a name that has
// not yet reported cannot be told apart from a job that is merely slow to
// start. Both strings stay fixed and configuration-derived (check names
// only), preserving the existing bounded, secret-free property of landing
// wait reasons.
func requiredCheckWaitReason(missing, pending []string) string {
	if len(missing) > 0 && len(pending) == 0 {
		return "required checks have not reported: " + strings.Join(missing, ", ")
	}
	return "required checks are pending"
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
