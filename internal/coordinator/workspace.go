package coordinator

import (
	"context"
	"strings"
	"time"

	"github.com/pmrrasmussen/symphony/internal/domain"
)

// workspaceCleanupTimeout bounds cleanupWorkspaceAtRunEnd's detached context
// (PMR-130), so a wedged git invocation cannot hold its goroutine forever once
// it no longer inherits the run's own cancellation.
const workspaceCleanupTimeout = 15 * time.Second

// cleanupWorkspace releases what Symphony still holds for an issue it has just
// decided is finished. It is the single place that decision is made, so it is
// also where the host's IssueForgetter is told (PMR-112) -- an issue that will
// never be dispatched again needs no linked pull request polled on its behalf.
//
// It then removes the issue's workspace and reports the lifecycle outcome an
// operator needs to know without reading the workspace package's own error
// text: a clean removal, a removal that discarded local commits Symphony
// verified as merged, or why the workspace was kept (uncommitted/untracked
// changes, or local commits ahead of the recorded base revision that a human
// should review before it is discarded). Call sites that run on a context
// reconciliation already holds live -- the poll loop's own stopTerminal branch
// and a redispatch retry's refresh -- are authoritative: their failure is
// always reported at WARN.
func (c *Coordinator) cleanupWorkspace(ctx context.Context, issue domain.Issue) {
	c.finalizeWorkspace(ctx, issue, nil)
}

// cleanupWorkspaceAtRunEnd releases a workspace from inside runTurns, at the
// moment a run decides its own issue is done (landing resolved, or the issue
// went terminal between turns). That decision races the poll loop's own
// reconcile pass, which can concurrently reach the same conclusion about the
// same issue and call stopRun -- cancelling the very context runTurns was
// about to clean up on and turning a healthy landing into a killed git
// subprocess (PMR-130). So this attempt runs on a context detached from the
// run's own cancellation (bounded by workspaceCleanupTimeout instead), and if
// r.stopped is stopTerminal once it finishes, reconcile's own stopTerminal
// branch holds -- or is about to hold -- an authoritative attempt on its own
// live context right after stopRun returns; this attempt's failure is then a
// duplicate, not a call to action, and is reported below WARN. Any other stop
// reason (ineligible, stalled) does not carry that guarantee -- reconcile
// only re-cleans up on stopTerminal -- so a failure raced by one of those must
// still reach WARN, or a genuine leak is swallowed as a duplicate that never
// actually gets retried.
func (c *Coordinator) cleanupWorkspaceAtRunEnd(ctx context.Context, r *running, issue domain.Issue) {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), workspaceCleanupTimeout)
	defer cancel()
	c.finalizeWorkspace(cctx, issue, r)
}

// finalizeWorkspace is cleanupWorkspace's and cleanupWorkspaceAtRunEnd's
// shared implementation. r is nil for an authoritative caller and set for the
// run-end caller whose failure reporting depends on whether reconciliation
// raced it.
func (c *Coordinator) finalizeWorkspace(ctx context.Context, issue domain.Issue, r *running) {
	if c.forget != nil {
		c.forget.Forget(issue.ID)
	}
	outcome, err := c.workspaces.Cleanup(ctx, issue)
	status := cleanupStatus(outcome, err)
	attrs := []any{"issue_id", issue.ID, "issue_identifier", issue.Identifier, "status", status}
	if err == nil {
		c.log.Info("workspace cleanup", attrs...)
		return
	}
	attrs = append(attrs, "error", err)
	if r != nil {
		c.mu.Lock()
		superseded := r.stopped == stopTerminal
		c.mu.Unlock()
		if superseded {
			c.log.Info("workspace cleanup", attrs...)
			return
		}
	}
	c.log.Warn("workspace cleanup", attrs...)
}

// cleanupStatus classifies a Cleanup result into the fixed
// clean/landed/dirty/committed/blocked/failed vocabulary the workspace
// package's own outcome and refusal messages already describe. It only ever
// reports a workspace-owned outcome constant or matches fixed, secret-free
// substrings the workspace package controls, never issue or workspace
// content.
//
// blocked is reserved for a verified refusal: Cleanup inspected the workspace
// and is declining to discard it, and every such refusal names itself with
// "refusing" (dirty and committed are just its two classified cases). Any
// other failure -- Cleanup could not even reach a refusal, for example a
// killed subprocess or an unreadable path -- is failed instead, so an
// operator can tell "your work is safe but unmerged" from "we could not run
// git" and only walk to a terminal for the former.
func cleanupStatus(outcome domain.CleanupOutcome, err error) string {
	if err == nil {
		if outcome == domain.CleanupLanded {
			return string(domain.CleanupLanded)
		}
		return string(domain.CleanupClean)
	}
	switch msg := err.Error(); {
	case strings.Contains(msg, "uncommitted or untracked changes"):
		return "dirty"
	case strings.Contains(msg, "differs from recorded base commit"):
		return "committed"
	case strings.Contains(msg, "refusing"):
		return "blocked"
	default:
		return "failed"
	}
}
