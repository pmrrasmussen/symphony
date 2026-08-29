package github

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/pmrrasmussen/symphony/internal/config"
)

// TestPollSettlesAndSweepsBothTerminalPullRequestStates covers the two ends of
// a link's life. Merged reconciles Linear exactly once; closed-unmerged only
// warns. Both are terminal for polling: the link leaves the table on the walk
// that observed it, so every later tick issues no request at all for it, which
// is what keeps a process that runs for weeks from polling and holding on to
// every issue it ever published (PMR-112).
func TestPollSettlesAndSweepsBothTerminalPullRequestStates(t *testing.T) {
	api, git, linear := newAPI(t), &fakeGit{}, &fakeLinear{}
	var logs bytes.Buffer
	m, session := testSession(t, api, git, linear, &logs)
	if _, err := session.Publish(context.Background(), testInput()); err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	api.prMerged = true
	api.mu.Unlock()
	m.Poll(context.Background())
	reads := api.pullReads()
	m.Poll(context.Background())
	if linear.reconciled != 1 {
		t.Fatalf("reconciliations=%d", linear.reconciled)
	}
	if strings.Count(logs.String(), "GitHub merge completed Linear issue") != 1 {
		t.Fatalf("merge completion log=%s", logs.String())
	}
	if tracked(m) != 0 {
		t.Fatalf("merged link retained: tracked=%d", tracked(m))
	}
	if api.pullReads() != reads {
		t.Fatalf("merged link still polled: reads=%d after the sweep, %d before", api.pullReads(), reads)
	}

	api2, linear2 := newAPI(t), &fakeLinear{}
	var closedLogs bytes.Buffer
	m2, session2 := testSession(t, api2, &fakeGit{}, linear2, &closedLogs)
	if _, err := session2.Publish(context.Background(), testInput()); err != nil {
		t.Fatal(err)
	}
	api2.mu.Lock()
	api2.prState = "closed"
	api2.mu.Unlock()
	m2.Poll(context.Background())
	closedReads := api2.pullReads()
	m2.Poll(context.Background())
	if linear2.reconciled != 0 || strings.Count(closedLogs.String(), "closed without merge") != 1 {
		t.Fatalf("reconciled=%d logs=%s", linear2.reconciled, closedLogs.String())
	}
	if tracked(m2) != 0 {
		t.Fatalf("closed-unmerged link retained: tracked=%d", tracked(m2))
	}
	if api2.pullReads() != closedReads {
		t.Fatalf("closed-unmerged link still polled: reads=%d after the sweep, %d before", api2.pullReads(), closedReads)
	}
	if strings.Contains(logs.String()+closedLogs.String(), "private-token") {
		t.Fatal("logs exposed credential")
	}
}

// TestPollRetainsAMergedLinkWhoseLinearReconciliationFailed pins the one thing
// the sweep must not do: drop a merged pull request whose Linear completion
// call failed. Nothing else would ever reconcile that issue to Done.
func TestPollRetainsAMergedLinkWhoseLinearReconciliationFailed(t *testing.T) {
	api, git := newAPI(t), &fakeGit{}
	linear := &fakeLinear{reconcileErr: errors.New("linear unavailable")}
	m, session := testSession(t, api, git, linear, nil)
	if _, err := session.Publish(context.Background(), testInput()); err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	api.prMerged = true
	api.mu.Unlock()
	m.Poll(context.Background())
	if tracked(m) != 1 {
		t.Fatalf("failed reconciliation dropped the link: tracked=%d", tracked(m))
	}
	linear.mu.Lock()
	linear.reconcileErr = nil
	linear.mu.Unlock()
	m.Poll(context.Background())
	if linear.reconciliations() != 1 || tracked(m) != 0 {
		t.Fatalf("retried reconciliation=%d tracked=%d, want 1 and 0", linear.reconciliations(), tracked(m))
	}
}

// TestPollFailureLogsTheUnderlyingError pins PMR-154: a poll failure must be
// attributable from the log alone -- a 404 on a deleted pull request needs a
// different operator response than a network blip, and neither is
// distinguishable from "something failed, once, somewhere" without the error.
func TestPollFailureLogsTheUnderlyingError(t *testing.T) {
	api, git, linear := newAPI(t), &fakeGit{}, &fakeLinear{}
	var logs bytes.Buffer
	m, session := testSession(t, api, git, linear, &logs)
	if _, err := session.Publish(context.Background(), testInput()); err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	api.failMethod, api.failPath, api.failStatus = http.MethodGet, "/repos/owner/repo/pulls/7", http.StatusNotFound
	api.mu.Unlock()
	m.Poll(context.Background())

	errText := logRecordError(t, logs.String(), "GitHub pull request poll failed")
	if errText == "" || !strings.Contains(errText, "404") {
		t.Fatalf("poll failure did not carry the underlying cause: error=%q logs=%s", errText, logs.String())
	}
}

// TestPollFailureLogDoesNotLeakProviderResponseBody pins the property PR #104's
// review established by inspection (PMR-132): every GitHub HTTP error is a
// fixed string or interpolates only a numeric status code, so attaching it to
// the poll-failure record cannot forward a response body or credential-derived
// text into the log.
//
// The response body is not unlogged, though: the HTTP layer records its own
// bounded, redacted excerpt against "GitHub request failed", because the reason
// a call failed exists only there (PMR-184). What this test pins is that the
// excerpt stays in that one record -- every error that propagates outward,
// here onto the poll-failure record, still carries nothing but the fixed
// string -- and that no record anywhere carries the credential.
func TestPollFailureLogDoesNotLeakProviderResponseBody(t *testing.T) {
	const secret = "wire-secret-should-never-reach-the-log"
	api, git, linear := newAPI(t), &fakeGit{}, &fakeLinear{}
	var logs bytes.Buffer
	m, session := testSession(t, api, git, linear, &logs)
	if _, err := session.Publish(context.Background(), testInput()); err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	api.failMethod, api.failPath, api.failStatus, api.failBody = http.MethodGet, "/repos/owner/repo/pulls/7", http.StatusInternalServerError, secret
	api.mu.Unlock()
	m.Poll(context.Background())

	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if strings.Contains(line, `"msg":"GitHub request failed"`) {
			continue
		}
		if strings.Contains(line, secret) {
			t.Fatalf("a record outside the HTTP layer leaked the provider response body: %s", line)
		}
	}
	if strings.Contains(logs.String(), "private-token") {
		t.Fatalf("poll failure log leaked the credential: %s", logs.String())
	}
	errText := logRecordError(t, logs.String(), "GitHub pull request poll failed")
	if errText != "github request failed with status 500" {
		t.Fatalf("poll failure error = %q", errText)
	}
}

func TestPollMergedReconcilesWithConfiguredMergeStateAndFailsClosedWithout(t *testing.T) {
	for _, test := range []struct {
		name       string
		mergeState string
	}{
		{name: "landing configured", mergeState: "Merging"},
		{name: "landing not configured", mergeState: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			api, git, linear := newAPI(t), &fakeGit{}, &fakeLinear{}
			m, session := testSession(t, api, git, linear, nil)
			// The link snapshots the session settings, so configuring MergeState
			// here is what the fail-closed poll branch reads.
			session.settings.MergeState = test.mergeState
			if _, err := session.Publish(context.Background(), testInput()); err != nil {
				t.Fatal(err)
			}
			api.mu.Lock()
			api.prMerged = true
			api.mu.Unlock()
			m.Poll(context.Background())
			m.Poll(context.Background())
			if linear.reconciled != 1 {
				t.Fatalf("reconciliations=%d", linear.reconciled)
			}
			if linear.reconciledState != test.mergeState {
				t.Fatalf("poll passed mergeState=%q want %q", linear.reconciledState, test.mergeState)
			}
		})
	}
}

// TestPollAuthenticatesWithTheRotatedTokenNotThePublicationSnapshot pins
// PMR-197. A link is evicted only when polling can learn nothing further, so it
// routinely outlives the credential it was published under. Polling with the
// frozen token would fail every remaining poll of every un-settled link once
// per tick forever, and never observe the merge below that settles this one.
func TestPollAuthenticatesWithTheRotatedTokenNotThePublicationSnapshot(t *testing.T) {
	api, linear := newAPI(t), &fakeLinear{}
	var logs bytes.Buffer
	m, session, live := testSessionWithLiveSettings(t, api, linear, &logs)
	if _, err := session.Publish(context.Background(), testInput()); err != nil {
		t.Fatal(err)
	}
	// The credential rotates after publication: the token the link snapshotted
	// is revoked, and the pull request merges while only the live one works.
	api.mu.Lock()
	api.requireToken, api.prMerged = "rotated-token", true
	api.mu.Unlock()
	live.update(func(g *config.GitHub) { g.Token = "rotated-token" })

	m.Poll(context.Background())
	if strings.Contains(logs.String(), "GitHub pull request poll failed") {
		t.Fatalf("poll used the revoked snapshot credential: %s", logs.String())
	}
	if linear.reconciled != 1 || tracked(m) != 0 {
		t.Fatalf("poll after rotation did not observe the merge: reconciliations=%d tracked=%d logs=%s", linear.reconciled, tracked(m), logs.String())
	}
	if strings.Contains(logs.String(), "rotated-token") {
		t.Fatalf("logs exposed the rotated credential: %s", logs.String())
	}
}

// The other half of that rule: a live configuration that stopped describing the
// repository the link's pull request lives in holds no credential for it, so
// the snapshot's own token is what the poll must keep presenting.
func TestPollKeepsTheSnapshotTokenWhenConfigurationNoLongerDescribesTheRepository(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*config.GitHub)
	}{
		{name: "integration disabled", mutate: func(g *config.GitHub) { *g = config.GitHub{} }},
		{name: "repointed at another repository", mutate: func(g *config.GitHub) { g.Repository, g.Token = "other", "other-repository-token" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			api, linear := newAPI(t), &fakeLinear{}
			var logs bytes.Buffer
			m, session, live := testSessionWithLiveSettings(t, api, linear, &logs)
			if _, err := session.Publish(context.Background(), testInput()); err != nil {
				t.Fatal(err)
			}
			api.mu.Lock()
			api.requireToken, api.prMerged = "private-token", true
			api.mu.Unlock()
			live.update(test.mutate)

			m.Poll(context.Background())
			if linear.reconciled != 1 || tracked(m) != 0 {
				t.Fatalf("poll abandoned the snapshot credential: reconciliations=%d tracked=%d logs=%s", linear.reconciled, tracked(m), logs.String())
			}
		})
	}
}

func TestPollStillOpenPullRequestDoesNotReconcileAndKeepsBeingPolled(t *testing.T) {
	api, git, linear := newAPI(t), &fakeGit{}, &fakeLinear{}
	m, session := testSession(t, api, git, linear, nil)
	if _, err := session.Publish(context.Background(), testInput()); err != nil {
		t.Fatal(err)
	}
	// The pull request stays open (default fixture state): no merge observed.
	m.Poll(context.Background())
	m.Poll(context.Background())
	if linear.reconciled != 0 {
		t.Fatalf("open pull request reconciled: reconciliations=%d", linear.reconciled)
	}
	// An unsettled link is never swept, however many ticks pass over it: the
	// merge this poll exists to observe may still be ahead of it.
	if tracked(m) != 1 || api.pullReads() != 2 {
		t.Fatalf("open pull request stopped being polled: tracked=%d reads=%d, want 1 and 2", tracked(m), api.pullReads())
	}
}

// TestRunPollsOnItsIntervalUntilCancelled exercises the loop the daemon
// actually starts: it must keep polling on the configured interval, reconcile
// a merge it observes there, and return as soon as its context is cancelled.
func TestRunPollsOnItsIntervalUntilCancelled(t *testing.T) {
	api, git, linear := newAPI(t), &fakeGit{}, &fakeLinear{}
	m, session := testSession(t, api, git, linear, nil)
	if _, err := session.Publish(context.Background(), testInput()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.Run(ctx)
	}()
	// The pull request is still open, so the loop must come back for it. Only
	// then is the merge published, so the reconciliation below can only have
	// come from a later tick of this same loop.
	waitUntil(t, "the poll loop read the open pull request twice", func() bool { return api.pullReads() >= 2 })
	api.mu.Lock()
	api.prMerged = true
	api.mu.Unlock()
	waitUntil(t, "the poll loop reconciled the merge", func() bool { return linear.reconciliations() == 1 })
	waitUntil(t, "the poll loop swept the settled link", func() bool { return tracked(m) == 0 })
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
}

// An unconfigured poll interval must fall back to the built-in default rather
// than spin, and cancellation must still be observed while that timer waits.
func TestRunWithNoConfiguredIntervalWaitsAndStopsOnCancellation(t *testing.T) {
	m := New(func() config.Settings { return config.Settings{GitHub: config.GitHub{Enabled: true}} }, slog.Default())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.Run(ctx)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not observe cancellation while waiting out its default interval")
	}
}
