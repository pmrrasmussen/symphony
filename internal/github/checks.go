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
//
// The check-run table is read after the combined-status table and
// overwrites whatever that table set, so a name reported in both wins by
// its check-run outcome, not its commit status. This is the only case where
// the two tables can disagree for the same required name, and it is
// deliberate rather than incidental: GitHub Actions reports the same job as
// both a check run and a legacy commit status for backward compatibility,
// and the check run is the richer, purpose-built representation.
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
//
// Both tables are paginated to their end rather than read one page deep.
// GitHub defaults either endpoint to 30 items, and a required check sitting on
// page 2 of a head commit's matrix jobs, bots, and scanners is indistinguishable
// from one that never reported: landing would wait on it for the daemon's
// lifetime while pointing the operator at a required_checks typo (PMR-190).
func (m *Manager) fetchChecks(ctx context.Context, s config.GitHub, sha string) (combinedStatus, checkRunsResponse, error) {
	if strings.TrimSpace(sha) == "" {
		return combinedStatus{}, checkRunsResponse{}, errors.New("github pull request has no evaluated commit")
	}
	var combined combinedStatus
	first := true
	complete, err := paginate(ctx, m, s, fmt.Sprintf("/repos/%s/%s/commits/%s/status?per_page=100", s.Owner, s.Repository, sha), func(page combinedStatus) {
		// Only the first page's overall state is the commit's; later pages
		// repeat it, but the field belongs to the collection, not the page.
		if first {
			combined.State = page.State
			first = false
		}
		combined.Statuses = append(combined.Statuses, page.Statuses...)
	})
	if err != nil {
		return combinedStatus{}, checkRunsResponse{}, err
	}
	m.warnIfCapped(complete, "commit statuses", sha)
	var runsResponse checkRunsResponse
	complete, err = paginate(ctx, m, s, fmt.Sprintf("/repos/%s/%s/commits/%s/check-runs?per_page=100", s.Owner, s.Repository, sha), func(page checkRunsResponse) {
		runsResponse.CheckRuns = append(runsResponse.CheckRuns, page.CheckRuns...)
	})
	if err != nil {
		return combinedStatus{}, checkRunsResponse{}, err
	}
	m.warnIfCapped(complete, "check runs", sha)
	return combined, runsResponse, nil
}

// warnIfCapped records the one case where a paginated gate input was read
// short: the page cap stopped the walk, so a required check or a
// changes-requested review beyond it is invisible to the gate. Nothing here
// carries a provider payload -- only the fixed collection name and the commit
// or pull request the read was for.
func (m *Manager) warnIfCapped(complete bool, collection string, subject any) {
	if complete {
		return
	}
	m.logger.Warn("GitHub paginated read stopped at the page cap", "collection", collection, "subject", subject, "max_pages", maxPages)
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
// from each reviewer's most recent state-bearing review, mirroring GitHub's own
// approve/changes-requested precedence.
//
// The listing is oldest-first and paginated to its end, and every page is
// accumulated before the fold below: a CHANGES_REQUESTED past the first page
// is a gate the landing path must see, and a later page's APPROVED from the
// same reviewer must still supersede an earlier page's CHANGES_REQUESTED
// (PMR-174, PMR-190).
//
// The returned truncated flag is the excerpt cap, not a completeness signal --
// it says only that the excerpts show the last contextMaxItems of a longer
// listing. It is not a landing gate, and must never be treated as one: every
// pull request with more than contextMaxItems reviews sets it.
func (m *Manager) reviews(ctx context.Context, s config.GitHub, number int) (string, []ReviewExcerpt, bool, error) {
	type review struct {
		User struct {
			Login string `json:"login"`
		} `json:"user"`
		State       string `json:"state"`
		Body        string `json:"body"`
		SubmittedAt string `json:"submitted_at"`
	}
	var raw []review
	complete, err := paginate(ctx, m, s, fmt.Sprintf("/repos/%s/%s/pulls/%d/reviews?per_page=100", s.Owner, s.Repository, number), func(page []review) {
		raw = append(raw, page...)
	})
	if err != nil {
		return "", nil, false, err
	}
	m.warnIfCapped(complete, "pull request reviews", number)
	// A reviewer's changes-requested stands until that same reviewer files a
	// new state-bearing review; a COMMENTED (or unsubmitted PENDING) review
	// carries no state and must not supersede it. DISMISSED is state-bearing
	// because GitHub rewrites the dismissed review's own state in place, so
	// the dismissal is the reviewer's latest review and clears nothing else.
	latestState := map[string]string{}
	order := make([]string, 0, len(raw))
	for _, review := range raw {
		login := strings.TrimSpace(review.User.Login)
		state := strings.ToUpper(strings.TrimSpace(review.State))
		if login == "" || state == "" || state == "COMMENTED" || state == "PENDING" {
			continue
		}
		if _, exists := latestState[login]; !exists {
			order = append(order, login)
		}
		latestState[login] = state
	}
	changesRequested, approved := false, false
	for _, login := range order {
		switch latestState[login] {
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
//
// It walks the connection's cursor to the end, bounded by maxPages. Unlike the
// excerpt caps elsewhere in this file, the returned truncated flag is a genuine
// completeness signal -- threads this call never saw -- computed against the
// connection's own totalCount so a server that reports no next page but fewer
// nodes than it counts is still reported short. github_land_pr waits on it
// rather than merging past a hard gate it could not read (PMR-190).
func (m *Manager) reviewThreads(ctx context.Context, s config.GitHub, number int) (unresolved, total int, truncated bool, err error) {
	seen := 0
	var cursor any
	for page := 0; page < maxPages; page++ {
		var response struct {
			Data struct {
				Repository struct {
					PullRequest struct {
						ReviewThreads struct {
							TotalCount int `json:"totalCount"`
							PageInfo   struct {
								HasNextPage bool   `json:"hasNextPage"`
								EndCursor   string `json:"endCursor"`
							} `json:"pageInfo"`
							Nodes []struct {
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
		payload := map[string]any{
			"query":     reviewThreadsQuery,
			"variables": map[string]any{"owner": s.Owner, "repository": s.Repository, "number": number, "cursor": cursor},
		}
		if err := m.request(ctx, s, http.MethodPost, "/graphql", payload, &response); err != nil {
			return 0, 0, false, err
		}
		if len(response.Errors) > 0 {
			return 0, 0, false, errors.New("github graphql request failed")
		}
		threads := response.Data.Repository.PullRequest.ReviewThreads
		for _, node := range threads.Nodes {
			if !node.IsResolved {
				unresolved++
			}
		}
		seen += len(threads.Nodes)
		total = threads.TotalCount
		if !threads.PageInfo.HasNextPage || strings.TrimSpace(threads.PageInfo.EndCursor) == "" {
			break
		}
		cursor = threads.PageInfo.EndCursor
	}
	if total > seen {
		m.logger.Warn("GitHub review thread listing was incomplete", "pr_number", number, "threads_total", total, "threads_read", seen, "max_pages", maxPages)
		return unresolved, total, true, nil
	}
	return unresolved, total, false, nil
}

const reviewThreadsQuery = `query($owner: String!, $repository: String!, $number: Int!, $cursor: String) { repository(owner: $owner, name: $repository) { pullRequest(number: $number) { reviewThreads(first: 100, after: $cursor) { totalCount pageInfo { hasNextPage endCursor } nodes { isResolved } } } } }`

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
