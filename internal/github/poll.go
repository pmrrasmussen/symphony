package github

import (
	"context"
	"strings"
	"time"

	"github.com/pmrrasmussen/symphony/internal/observability"
)

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
		m.logger.Warn("GitHub pull request poll failed", "issue_id", linked.issueID, "issue_identifier", linked.identifier, "pr_number", linked.prNumber, "error", observability.Text(err.Error()))
		return
	}
	if pr.Merged || pr.MergedAt != nil {
		// Reconcile to Done from either the review handoff target or, when
		// landing is configured, the Merging state. The Merging path is
		// fail-closed: an unconfigured landing block (empty MergeState) keeps
		// the reconciliation to the review-target state alone.
		changed, err := linked.linear.ReconcileMerged(ctx, linked.settings.MergeState)
		if err != nil {
			m.logger.Warn("GitHub merge Linear completion failed", "issue_id", linked.issueID, "issue_identifier", linked.identifier, "pr_number", linked.prNumber, "error", observability.Text(err.Error()))
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
		m.logger.Warn("GitHub pull request closed without merge; Linear issue remains in review", "issue_id", linked.issueID, "issue_identifier", linked.identifier, "pr_number", linked.prNumber, "pr_url", linked.prURL)
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
