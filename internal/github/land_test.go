package github

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/domain"
)

func TestLandWaitsWhileRequiredChecksAreMissingOrPending(t *testing.T) {
	for _, test := range []struct {
		name       string
		configure  func(*apiFixture)
		wantReason string
	}{
		{name: "missing", configure: func(api *apiFixture) {}, wantReason: "required checks have not reported: ci/build"},
		{name: "pending", configure: func(api *apiFixture) {
			api.checkRuns = append(api.checkRuns, map[string]any{"name": "ci/build", "status": "in_progress", "conclusion": nil})
		}, wantReason: "required checks are pending"},
	} {
		t.Run(test.name, func(t *testing.T) {
			api, git, linear := newAPI(t), &fakeGit{}, &fakeLinear{}
			api.prExists = true
			test.configure(api)
			_, session := testLandingSession(t, api, git, linear, []string{"ci/build"}, "merge")
			result, err := session.Land(context.Background())
			if err != nil {
				t.Fatalf("waiting on pending checks must not be an error: %v", err)
			}
			if result.Status != LandWaiting || result.Reason != test.wantReason {
				t.Fatalf("result=%+v want reason=%q", result, test.wantReason)
			}
			if linear.refused != 0 || linear.landCompleted != 0 || api.merges != 0 {
				t.Fatalf("waiting mutated Linear or GitHub: refused=%d completed=%d merges=%d", linear.refused, linear.landCompleted, api.merges)
			}
		})
	}
}

// TestLandWaitReasonMixedMissingAndPending asserts that a mix of a
// never-reported check and a genuinely pending one keeps the original
// "required checks are pending" reason: with only a single GitHub snapshot,
// a name that has not yet reported cannot be distinguished from one that is
// merely slow to start, so mixing in even one confirmed-pending check must
// not produce a false-positive "missing" diagnosis for the whole wait.
func TestLandWaitReasonMixedMissingAndPending(t *testing.T) {
	api, git, linear := newAPI(t), &fakeGit{}, &fakeLinear{}
	api.prExists = true
	api.checkRuns = append(api.checkRuns, map[string]any{"name": "ci/build", "status": "in_progress", "conclusion": nil})
	_, session := testLandingSession(t, api, git, linear, []string{"ci/build", "ci/lint"}, "merge")
	result, err := session.Land(context.Background())
	if err != nil {
		t.Fatalf("waiting on mixed checks must not be an error: %v", err)
	}
	if result.Status != LandWaiting || result.Reason != "required checks are pending" {
		t.Fatalf("result=%+v", result)
	}
}

func TestLandRefusesOnFailingRequiredChecks(t *testing.T) {
	api, git, linear := newAPI(t), &fakeGit{}, &fakeLinear{}
	api.prExists = true
	api.checkRuns = []map[string]any{{"name": "ci/build", "status": "completed", "conclusion": "failure"}}
	_, session := testLandingSession(t, api, git, linear, []string{"ci/build"}, "merge")
	if _, err := session.Land(context.Background()); err == nil || !strings.Contains(err.Error(), "required checks failed") {
		t.Fatalf("failing required check error=%v", err)
	}
	if linear.refused != 1 || linear.refusedDestState != "Merging" {
		t.Fatalf("hard gate did not attempt the Merging fallback: refused=%d dest=%q", linear.refused, linear.refusedDestState)
	}
	// The refusal must name which gate fired, not merely that landing was
	// refused, so an operator reading the Linear transition log record alone
	// can tell this apart from every other hard gate (PMR-159).
	if !strings.Contains(linear.refusedReason, "required checks failed") {
		t.Fatalf("refused reason did not name the failing-checks gate: %q", linear.refusedReason)
	}
	if api.merges != 0 || linear.landCompleted != 0 {
		t.Fatalf("failing checks must never merge: merges=%d completed=%d", api.merges, linear.landCompleted)
	}
}

// TestLandRefusalReasonCarriesNoProviderOrCredentialText plants a secret in
// the GitHub credential and in provider-authored free text (the pull request
// body and a review body) that a hard gate's detection reads past, then
// drives two distinct hard gates. The recorded refusal reason must never
// carry that secret: every reason string is fixed or repository-config
// derived (required check names), never provider response content or a
// credential (PMR-159).
func TestLandRefusalReasonCarriesNoProviderOrCredentialText(t *testing.T) {
	const secret = "wire-secret-should-never-reach-the-log"

	t.Run("failing required checks", func(t *testing.T) {
		api, git, linear := newAPI(t), &fakeGit{}, &fakeLinear{}
		api.prExists, api.prBody = true, secret
		api.checkRuns = failingChecks("ci/build")
		_, session := testLandingSession(t, api, git, linear, []string{"ci/build"}, "merge")
		session.settings.Token = secret
		if _, err := session.Land(context.Background()); err == nil {
			t.Fatal("expected refusal")
		}
		if strings.Contains(linear.refusedReason, secret) {
			t.Fatalf("refused reason leaked provider or credential text: %q", linear.refusedReason)
		}
		if !strings.Contains(linear.refusedReason, "ci/build") {
			t.Fatalf("refused reason missing configured check name: %q", linear.refusedReason)
		}
	})

	t.Run("merge conflicts", func(t *testing.T) {
		api, git, linear := newAPI(t), &fakeGit{}, &fakeLinear{}
		api.prExists, api.prBody = true, secret
		passingRequiredChecks(api, "ci/build")
		api.reviews = []map[string]any{{"user": map[string]any{"login": "alice"}, "state": "APPROVED", "body": secret, "submitted_at": "t1"}}
		api.mergeable = boolPtr(false)
		_, session := testLandingSession(t, api, git, linear, []string{"ci/build"}, "merge")
		session.settings.Token = secret
		if _, err := session.Land(context.Background()); err == nil {
			t.Fatal("expected refusal")
		}
		if strings.Contains(linear.refusedReason, secret) {
			t.Fatalf("refused reason leaked provider or credential text: %q", linear.refusedReason)
		}
		if !strings.Contains(linear.refusedReason, "merge conflicts") {
			t.Fatalf("refused reason missing conflict gate: %q", linear.refusedReason)
		}
	})
}

func TestLandRefusesOnEffectiveChangesRequestedReview(t *testing.T) {
	api, git, linear := newAPI(t), &fakeGit{}, &fakeLinear{}
	api.prExists = true
	passingRequiredChecks(api, "ci/build")
	api.reviews = []map[string]any{
		{"user": map[string]any{"login": "alice"}, "state": "APPROVED", "body": "lgtm", "submitted_at": "t1"},
		{"user": map[string]any{"login": "bob"}, "state": "CHANGES_REQUESTED", "body": "no", "submitted_at": "t2"},
	}
	api.mergeable = boolPtr(true)
	_, session := testLandingSession(t, api, git, linear, []string{"ci/build"}, "merge")
	if _, err := session.Land(context.Background()); err == nil || !strings.Contains(err.Error(), "changes-requested") {
		t.Fatalf("changes-requested error=%v", err)
	}
	if linear.refused != 1 || api.merges != 0 {
		t.Fatalf("refused=%d merges=%d", linear.refused, api.merges)
	}
}

// A reviewer who follows their own Request Changes with a plain Comment
// review has not withdrawn it: GitHub still shows changes requested, and so
// must the landing gate.
func TestLandRefusesWhenSameReviewerCommentsAfterRequestingChanges(t *testing.T) {
	api, git, linear := newAPI(t), &fakeGit{}, &fakeLinear{}
	api.prExists = true
	passingRequiredChecks(api, "ci/build")
	api.reviews = []map[string]any{
		{"user": map[string]any{"login": "bob"}, "state": "CHANGES_REQUESTED", "body": "no", "submitted_at": "t1"},
		{"user": map[string]any{"login": "bob"}, "state": "COMMENTED", "body": "still need X before merge", "submitted_at": "t2"},
	}
	api.mergeable = boolPtr(true)
	_, session := testLandingSession(t, api, git, linear, []string{"ci/build"}, "merge")
	if _, err := session.Land(context.Background()); err == nil || !strings.Contains(err.Error(), "changes-requested") {
		t.Fatalf("changes-requested error=%v", err)
	}
	if linear.refused != 1 || api.merges != 0 {
		t.Fatalf("refused=%d merges=%d", linear.refused, api.merges)
	}
}

// The same reviewer's later APPROVED does supersede their changes-requested,
// so the gate must not latch on a review the reviewer has withdrawn.
func TestLandProceedsWhenSameReviewerApprovesAfterRequestingChanges(t *testing.T) {
	api, git, linear := newAPI(t), &fakeGit{}, &fakeLinear{}
	api.prExists = true
	passingRequiredChecks(api, "ci/build")
	api.reviews = []map[string]any{
		{"user": map[string]any{"login": "bob"}, "state": "CHANGES_REQUESTED", "body": "no", "submitted_at": "t1"},
		{"user": map[string]any{"login": "bob"}, "state": "COMMENTED", "body": "one more thing", "submitted_at": "t2"},
		{"user": map[string]any{"login": "bob"}, "state": "APPROVED", "body": "fixed", "submitted_at": "t3"},
	}
	api.mergeable = boolPtr(true)
	api.mergeableState = "clean"
	_, session := testLandingSession(t, api, git, linear, []string{"ci/build"}, "merge")
	result, err := session.Land(context.Background())
	if err != nil {
		t.Fatalf("land error=%v", err)
	}
	if result.Status != LandMerged || api.merges != 1 {
		t.Fatalf("result=%+v merges=%d", result, api.merges)
	}
}

func TestLandRefusesOnUnresolvedReviewThreads(t *testing.T) {
	api, git, linear := newAPI(t), &fakeGit{}, &fakeLinear{}
	api.prExists = true
	passingRequiredChecks(api, "ci/build")
	readyToLand(api)
	api.threads = []map[string]any{{"isResolved": false}}
	_, session := testLandingSession(t, api, git, linear, []string{"ci/build"}, "merge")
	if _, err := session.Land(context.Background()); err == nil || !strings.Contains(err.Error(), "unresolved review threads") {
		t.Fatalf("unresolved threads error=%v", err)
	}
	if linear.refused != 1 || api.merges != 0 {
		t.Fatalf("refused=%d merges=%d", linear.refused, api.merges)
	}
}

// TestLandReadsGatesPastTheFirstPage drives the three gate inputs that used to
// be read one page deep, each with its deciding item placed past that page.
// Landing must reach the same outcome it would have reached had the item been
// on page one: the required check lands, the unresolved thread and the
// changes-requested review do not (PMR-190).
func TestLandReadsGatesPastTheFirstPage(t *testing.T) {
	for _, test := range []struct {
		name      string
		configure func(*apiFixture)
		wantMerge bool
		wantGate  string
	}{
		{
			name: "required check on the second page lands",
			configure: func(api *apiFixture) {
				passingRequiredChecks(api, "noise/a", "noise/b", "ci/build")
			},
			wantMerge: true,
		},
		{
			name: "unresolved thread past the first page does not merge",
			configure: func(api *apiFixture) {
				passingRequiredChecks(api, "ci/build")
				api.threadPageSize = 2
				api.threads = []map[string]any{{"isResolved": true}, {"isResolved": true}, {"isResolved": false}}
			},
			wantGate: "unresolved review threads",
		},
		{
			name: "changes-requested review past the first page does not merge",
			configure: func(api *apiFixture) {
				passingRequiredChecks(api, "ci/build")
				api.reviews = append(api.reviews,
					map[string]any{"user": map[string]any{"login": "carol"}, "state": "COMMENTED", "body": "one thought", "submitted_at": "t2"},
					map[string]any{"user": map[string]any{"login": "bob"}, "state": "CHANGES_REQUESTED", "body": "no", "submitted_at": "t3"})
			},
			wantGate: "effective changes-requested review",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			api, git, linear := newAPI(t), &fakeGit{}, &fakeLinear{}
			api.prExists = true
			readyToLand(api)
			// Two per page: every collection this test configures puts its
			// deciding entry third or later, so page one alone cannot decide.
			api.pageSize = 2
			test.configure(api)
			_, session := testLandingSession(t, api, git, linear, []string{"ci/build"}, "merge")
			result, err := session.Land(context.Background())
			if test.wantMerge {
				if err != nil {
					t.Fatalf("landing failed: %v", err)
				}
				if result.Status != LandMerged || api.merges != 1 {
					t.Fatalf("result=%+v merges=%d", result, api.merges)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantGate) {
				t.Fatalf("error=%v want a %q gate", err, test.wantGate)
			}
			if api.merges != 0 || linear.landCompleted != 0 {
				t.Fatalf("a gate past the first page merged anyway: merges=%d completed=%d", api.merges, linear.landCompleted)
			}
		})
	}
}

// TestLandWaitsWhenTheReviewThreadListingIsIncomplete pins the fail-closed
// direction of the one gate whose completeness the adapter can only report,
// not repair: threads exist that the bounded cursor walk never read, so every
// thread it did see being resolved does not prove there is no unresolved one.
// Landing waits -- keeping the issue in Merging for a human -- rather than
// merging past a hard gate it could not read (PMR-190).
func TestLandWaitsWhenTheReviewThreadListingIsIncomplete(t *testing.T) {
	api, git, linear := newAPI(t), &fakeGit{}, &fakeLinear{}
	api.prExists = true
	passingRequiredChecks(api, "ci/build")
	readyToLand(api)
	// Every thread served is resolved, but the connection counts more than the
	// walk could read, which is what exceeding the page cap looks like.
	api.threads = []map[string]any{{"isResolved": true}}
	api.threadsTotal = 5000
	_, session := testLandingSession(t, api, git, linear, []string{"ci/build"}, "merge")
	result, err := session.Land(context.Background())
	if err != nil {
		t.Fatalf("an unreadable thread listing must wait, not error: %v", err)
	}
	if result.Status != LandWaiting || result.Reason != "github review threads could not be read completely" {
		t.Fatalf("result=%+v", result)
	}
	if api.merges != 0 || linear.landCompleted != 0 || linear.refused != 0 {
		t.Fatalf("waiting mutated GitHub or Linear: merges=%d completed=%d refused=%d", api.merges, linear.landCompleted, linear.refused)
	}
}

// TestLandDoesNotWaitOnTheReviewExcerptCap guards against the inverse mistake:
// reviews' third return is an excerpt cap, not a completeness signal, so a
// pull request with more than contextMaxItems reviews must land normally
// instead of parking in a permanent wait (PMR-190).
func TestLandDoesNotWaitOnTheReviewExcerptCap(t *testing.T) {
	api, git, linear := newAPI(t), &fakeGit{}, &fakeLinear{}
	api.prExists = true
	passingRequiredChecks(api, "ci/build")
	readyToLand(api)
	for range contextMaxItems + 5 {
		api.reviews = append(api.reviews, map[string]any{"user": map[string]any{"login": "carol"}, "state": "COMMENTED", "body": "thoughts", "submitted_at": "t2"})
	}
	_, session := testLandingSession(t, api, git, linear, []string{"ci/build"}, "merge")
	result, err := session.Land(context.Background())
	if err != nil {
		t.Fatalf("landing failed: %v", err)
	}
	if result.Status != LandMerged || api.merges != 1 {
		t.Fatalf("result=%+v merges=%d", result, api.merges)
	}
}

func TestLandWaitsWhileMergeabilityIsUndetermined(t *testing.T) {
	api, git, linear := newAPI(t), &fakeGit{}, &fakeLinear{}
	api.prExists = true
	passingRequiredChecks(api, "ci/build")
	api.reviews = []map[string]any{{"user": map[string]any{"login": "alice"}, "state": "APPROVED", "body": "lgtm", "submitted_at": "t1"}}
	// api.mergeable stays nil: GitHub has not yet computed mergeability.
	_, session := testLandingSession(t, api, git, linear, []string{"ci/build"}, "merge")
	result, err := session.Land(context.Background())
	if err != nil {
		t.Fatalf("undetermined mergeability must not be an error: %v", err)
	}
	if result.Status != LandWaiting {
		t.Fatalf("result=%+v", result)
	}
	if linear.refused != 0 || api.merges != 0 {
		t.Fatalf("waiting on mergeability mutated state: refused=%d merges=%d", linear.refused, api.merges)
	}
}

func TestLandRefusesOnMergeConflicts(t *testing.T) {
	api, git, linear := newAPI(t), &fakeGit{}, &fakeLinear{}
	api.prExists = true
	passingRequiredChecks(api, "ci/build")
	api.reviews = []map[string]any{{"user": map[string]any{"login": "alice"}, "state": "APPROVED", "body": "lgtm", "submitted_at": "t1"}}
	api.mergeable = boolPtr(false)
	api.mergeableState = "dirty"
	_, session := testLandingSession(t, api, git, linear, []string{"ci/build"}, "merge")
	if _, err := session.Land(context.Background()); err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("merge conflict error=%v", err)
	}
	if linear.refused != 1 || api.merges != 0 {
		t.Fatalf("refused=%d merges=%d", linear.refused, api.merges)
	}
}

func TestLandRefusesOnStaleBase(t *testing.T) {
	api, linear := newAPI(t), &fakeLinear{}
	api.prExists = true
	passingRequiredChecks(api, "ci/build")
	readyToLand(api)
	git := &staleBaseGit{fakeGit: &fakeGit{}}
	_, session := testLandingSession(t, api, git, linear, []string{"ci/build"}, "merge")
	if _, err := session.Land(context.Background()); err == nil || !strings.Contains(err.Error(), "base branch changed") {
		t.Fatalf("stale base error=%v", err)
	}
	if linear.refused != 1 || api.merges != 0 {
		t.Fatalf("refused=%d merges=%d", linear.refused, api.merges)
	}
}

func TestLandUpdatesCleanStaleBranchThenWaitsForNewChecks(t *testing.T) {
	api, linear := newAPI(t), &fakeLinear{}
	api.prExists = true
	passingRequiredChecks(api, "ci/build")
	readyToLand(api)
	api.clearChecksOnBranchUpdate = true
	git := &staleBaseGit{fakeGit: &fakeGit{}}
	manager, session := testLandingSession(t, api, git, linear, []string{"ci/build"}, "merge")
	session.settings.UpdateStaleBranch = true
	var log bytes.Buffer
	manager.logger = slog.New(slog.NewJSONHandler(&log, nil))

	result, err := session.Land(context.Background())
	if err != nil || result.Status != LandWaiting || !strings.Contains(result.Reason, "branch was updated") {
		t.Fatalf("update result=%+v err=%v", result, err)
	}
	if api.updateBranchCalls != 1 || api.merges != 0 || linear.refused != 0 {
		t.Fatalf("updates=%d merges=%d refused=%d", api.updateBranchCalls, api.merges, linear.refused)
	}
	if !strings.Contains(log.String(), `"issue_identifier":"PMR-27"`) || !strings.Contains(log.String(), `"pr_number":7`) || !strings.Contains(log.String(), `"head_sha":"updated-head"`) || strings.Contains(log.String(), `"issue_id"`) || strings.Contains(log.String(), `"repository"`) {
		t.Fatalf("update log=%q", log.String())
	}

	result, err = session.Land(context.Background())
	if err != nil || result.Status != LandWaiting || result.Reason != "required checks have not reported: ci/build" {
		t.Fatalf("new-head checks result=%+v err=%v", result, err)
	}
	passingRequiredChecks(api, "ci/build")
	result, err = session.Land(context.Background())
	if err != nil || result.Status != LandMerged || api.merges != 1 || linear.refused != 0 {
		t.Fatalf("landing result=%+v err=%v merges=%d refused=%d", result, err, api.merges, linear.refused)
	}
}

func TestLandRefusesConflictedStaleBranchWithoutUpdating(t *testing.T) {
	api, linear := newAPI(t), &fakeLinear{}
	api.prExists = true
	passingRequiredChecks(api, "ci/build")
	readyToLand(api)
	api.mergeable = boolPtr(false)
	api.mergeableState = "dirty"
	_, session := testLandingSession(t, api, &staleBaseGit{fakeGit: &fakeGit{}}, linear, []string{"ci/build"}, "merge")
	session.settings.UpdateStaleBranch = true
	if _, err := session.Land(context.Background()); err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("conflicted stale branch error=%v", err)
	}
	if api.updateBranchCalls != 0 || linear.refused != 1 {
		t.Fatalf("updates=%d refused=%d", api.updateBranchCalls, linear.refused)
	}
}

func TestLandRefusesWhenStaleBranchUpdateFails(t *testing.T) {
	api, linear := newAPI(t), &fakeLinear{}
	api.prExists = true
	passingRequiredChecks(api, "ci/build")
	readyToLand(api)
	api.updateBranchFails = true
	_, session := testLandingSession(t, api, &staleBaseGit{fakeGit: &fakeGit{}}, linear, []string{"ci/build"}, "merge")
	session.settings.UpdateStaleBranch = true
	if _, err := session.Land(context.Background()); err == nil || !strings.Contains(err.Error(), "could not update stale") {
		t.Fatalf("update failure error=%v", err)
	}
	if api.updateBranchCalls != 1 || linear.refused != 1 || api.merges != 0 {
		t.Fatalf("updates=%d refused=%d merges=%d", api.updateBranchCalls, linear.refused, api.merges)
	}
}

func TestLandRefusesWhenBaseMovesAgainAfterStaleBranchUpdate(t *testing.T) {
	api, linear := newAPI(t), &fakeLinear{}
	api.prExists = true
	passingRequiredChecks(api, "ci/build")
	readyToLand(api)
	git := &alwaysStaleBaseGit{fakeGit: &fakeGit{}}
	_, session := testLandingSession(t, api, git, linear, []string{"ci/build"}, "merge")
	session.settings.UpdateStaleBranch = true
	result, err := session.Land(context.Background())
	if err != nil || result.Status != LandWaiting {
		t.Fatalf("initial update result=%+v err=%v", result, err)
	}
	if _, err := session.Land(context.Background()); err == nil || !strings.Contains(err.Error(), "base branch changed") {
		t.Fatalf("repeated staleness error=%v", err)
	}
	if api.updateBranchCalls != 1 || linear.refused != 1 || api.merges != 0 {
		t.Fatalf("updates=%d refused=%d merges=%d", api.updateBranchCalls, linear.refused, api.merges)
	}
}

// TestLandFeatureOffRefusesRetryableGateImmediately pins feature-off parity: a
// gate that would be retryable when the bounded-fix feature is on must, with it
// off, refuse immediately exactly as before -- a plain error, one Merging
// fallback transition, no fix counter, and no audit/refusal comments.
func TestLandFeatureOffRefusesRetryableGateImmediately(t *testing.T) {
	api, git, linear := newAPI(t), &fakeGit{}, &fakeLinear{}
	api.prExists = true
	api.checkRuns = failingChecks("ci/build")
	_, session := testLandingSession(t, api, git, linear, []string{"ci/build"}, "merge")
	_, err := session.Land(context.Background())
	var gate *LandGateError
	if err == nil || errors.As(err, &gate) {
		t.Fatalf("feature-off gate must be a plain immediate refusal, got err=%v", err)
	}
	if !strings.Contains(err.Error(), "required checks failed") {
		t.Fatalf("error=%v", err)
	}
	if linear.refused != 1 || linear.refusedDestState != "Merging" {
		t.Fatalf("refused=%d dest=%q", linear.refused, linear.refusedDestState)
	}
	if len(linear.landComments) != 0 || len(api.prComments) != 0 {
		t.Fatalf("feature-off posted comments: linear=%v pr=%v", linear.landComments, api.prComments)
	}
	if session.landAttempts != 0 || session.retryableGateHit {
		t.Fatalf("feature-off touched fix state: attempts=%d hit=%v", session.landAttempts, session.retryableGateHit)
	}
}

// TestLandFailingCheckFixSucceeds exercises a retryable failing-check gate: the
// first call returns a non-terminal LandGateError naming the gate without any
// transition, and after the checks turn green the same session lands.
func TestLandFailingCheckFixSucceeds(t *testing.T) {
	api, git, linear := newAPI(t), &fakeGit{}, &fakeLinear{}
	api.prExists = true
	api.checkRuns = failingChecks("ci/build")
	readyToLand(api)
	_, session := testLandingSession(t, api, git, linear, []string{"ci/build"}, "merge")
	session.settings.LandFixEnabled = true
	session.settings.MaxLandAttempts = 2

	_, err := session.Land(context.Background())
	var gate *LandGateError
	if !errors.As(err, &gate) || !gate.Retryable || !strings.Contains(gate.Reason, "required checks failed") {
		t.Fatalf("first attempt must be a retryable gate, got err=%v", err)
	}
	if linear.refused != 0 || api.merges != 0 {
		t.Fatalf("retryable gate must defer the transition: refused=%d merges=%d", linear.refused, api.merges)
	}

	api.mu.Lock()
	api.checkRuns = []map[string]any{{"name": "ci/build", "status": "completed", "conclusion": "success"}}
	api.mu.Unlock()
	result, err := session.Land(context.Background())
	if err != nil || result.Status != LandMerged {
		t.Fatalf("fixed landing result=%+v err=%v", result, err)
	}
	if api.merges != 1 || linear.landCompleted != 1 || linear.refused != 0 {
		t.Fatalf("merges=%d completed=%d refused=%d", api.merges, linear.landCompleted, linear.refused)
	}
	if len(linear.landComments) != 0 || len(api.prComments) != 0 {
		t.Fatalf("no commits were pushed, so no audit comment should exist: linear=%v pr=%v", linear.landComments, api.prComments)
	}
}

// TestLandConflictFixSucceedsWithAuditComment exercises the conflict path
// (retryable only with allow_conflict_resolution): the first call defers, the
// fix pushes new commits, and the push is audited to both the Linear issue and
// the GitHub PR before the merge.
func TestLandConflictFixSucceedsWithAuditComment(t *testing.T) {
	api, linear := newAPI(t), &fakeLinear{}
	api.prExists = true
	passingRequiredChecks(api, "ci/build")
	readyToLand(api)
	api.mergeable = boolPtr(false)
	api.mergeableState = "dirty"
	git := &fixPushGit{fakeGit: &fakeGit{}, api: api, head: "head"}
	_, session := testLandingSession(t, api, git, linear, []string{"ci/build"}, "merge")
	session.settings.LandFixEnabled = true
	session.settings.MaxLandAttempts = 2
	session.settings.AllowConflictResolution = true

	_, err := session.Land(context.Background())
	var gate *LandGateError
	if !errors.As(err, &gate) || !gate.Retryable || !strings.Contains(gate.Reason, "conflicts") {
		t.Fatalf("conflict must be a retryable gate here, got err=%v", err)
	}
	if linear.refused != 0 || api.merges != 0 {
		t.Fatalf("refused=%d merges=%d", linear.refused, api.merges)
	}

	api.mu.Lock()
	api.mergeable = boolPtr(true)
	api.mergeableState = "clean"
	api.mu.Unlock()
	git.head = "fixedhead"

	result, err := session.Land(context.Background())
	if err != nil || result.Status != LandMerged {
		t.Fatalf("fixed landing result=%+v err=%v", result, err)
	}
	if api.merges != 1 || linear.landCompleted != 1 {
		t.Fatalf("merges=%d completed=%d", api.merges, linear.landCompleted)
	}
	if len(linear.landComments) != 1 || len(api.prComments) != 1 {
		t.Fatalf("expected one audit comment each: linear=%v pr=%v", linear.landComments, api.prComments)
	}
	if linear.landComments[0] != api.prComments[0] {
		t.Fatalf("audit comment bodies differ: linear=%q pr=%q", linear.landComments[0], api.prComments[0])
	}
	if !strings.Contains(linear.landComments[0], "PMR-27") || !strings.Contains(linear.landComments[0], "fixedhead") {
		t.Fatalf("audit comment missing identifier or SHA: %q", linear.landComments[0])
	}
}

// TestLandConflictRefusesImmediatelyWhenResolutionNotAllowed confirms a merge
// conflict stays a hard immediate refusal (never a retryable gate) unless
// allow_conflict_resolution is set, even with the fix feature enabled.
func TestLandConflictRefusesImmediatelyWhenResolutionNotAllowed(t *testing.T) {
	api, git, linear := newAPI(t), &fakeGit{}, &fakeLinear{}
	api.prExists = true
	passingRequiredChecks(api, "ci/build")
	readyToLand(api)
	api.mergeable = boolPtr(false)
	api.mergeableState = "dirty"
	_, session := testLandingSession(t, api, git, linear, []string{"ci/build"}, "merge")
	session.settings.LandFixEnabled = true
	session.settings.MaxLandAttempts = 2
	// AllowConflictResolution stays false.
	_, err := session.Land(context.Background())
	var gate *LandGateError
	if err == nil || errors.As(err, &gate) || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("conflict without resolution opt-in must refuse immediately, got err=%v", err)
	}
	if linear.refused != 1 || len(linear.landComments) != 0 {
		t.Fatalf("refused=%d comments=%v", linear.refused, linear.landComments)
	}
}

// TestLandExhaustionRefusesOnceWithGateComment drives a retryable gate past the
// attempt budget: the final call fires the Merging -> In Review transition once
// plus a comment naming the last failed gate, and never merges.
func TestLandExhaustionRefusesOnceWithGateComment(t *testing.T) {
	api, git, linear := newAPI(t), &fakeGit{}, &fakeLinear{}
	api.prExists = true
	api.checkRuns = failingChecks("ci/build")
	_, session := testLandingSession(t, api, git, linear, []string{"ci/build"}, "merge")
	session.settings.LandFixEnabled = true
	session.settings.MaxLandAttempts = 1

	_, err := session.Land(context.Background())
	var gate *LandGateError
	if !errors.As(err, &gate) {
		t.Fatalf("first attempt must be granted as a retryable gate, got err=%v", err)
	}
	if linear.refused != 0 {
		t.Fatalf("granted attempt must not transition: refused=%d", linear.refused)
	}

	_, err = session.Land(context.Background())
	if err == nil || errors.As(err, &gate) {
		t.Fatalf("exhausted attempt must be a plain refusal, got err=%v", err)
	}
	if linear.refused != 1 || linear.refusedDestState != "Merging" {
		t.Fatalf("refused=%d dest=%q", linear.refused, linear.refusedDestState)
	}
	// The deferred refusal path (exhausted retry budget) must also record which
	// gate caused it, not only the immediate refusal path (PMR-159).
	if !strings.Contains(linear.refusedReason, "required checks failed") {
		t.Fatalf("deferred refusal reason did not name the gate: %q", linear.refusedReason)
	}
	if len(linear.landComments) != 1 || !strings.Contains(linear.landComments[0], "required checks failed") {
		t.Fatalf("exhaustion comment=%v", linear.landComments)
	}
	if api.merges != 0 {
		t.Fatalf("exhaustion must never merge: merges=%d", api.merges)
	}
}

// TestLandFinalizeAfterTurnEnd covers a turn that ends after a retryable gate
// but before landing: FinalizeLanding fires the deferred transition and comment
// exactly once and is a safe no-op on repeat.
func TestLandFinalizeAfterTurnEnd(t *testing.T) {
	api, git, linear := newAPI(t), &fakeGit{}, &fakeLinear{}
	api.prExists = true
	api.checkRuns = failingChecks("ci/build")
	_, session := testLandingSession(t, api, git, linear, []string{"ci/build"}, "merge")
	session.settings.LandFixEnabled = true
	session.settings.MaxLandAttempts = 2

	_, err := session.Land(context.Background())
	var gate *LandGateError
	if !errors.As(err, &gate) {
		t.Fatalf("attempt must be a retryable gate, got err=%v", err)
	}
	if linear.refused != 0 {
		t.Fatalf("gate deferred the transition, yet refused=%d", linear.refused)
	}

	session.FinalizeLanding(context.Background())
	if linear.refused != 1 || linear.refusedDestState != "Merging" {
		t.Fatalf("finalize did not fire the transition: refused=%d", linear.refused)
	}
	if !strings.Contains(linear.refusedReason, "required checks failed") {
		t.Fatalf("finalize reason did not name the gate: %q", linear.refusedReason)
	}
	if len(linear.landComments) != 1 || !strings.Contains(linear.landComments[0], "required checks failed") {
		t.Fatalf("finalize comment=%v", linear.landComments)
	}
	session.FinalizeLanding(context.Background())
	if linear.refused != 1 || len(linear.landComments) != 1 {
		t.Fatalf("finalize must be idempotent: refused=%d comments=%v", linear.refused, linear.landComments)
	}
}

// TestLandWaitAfterRetryableGateKeepsIssueInMerging covers PMR-78: a fix turn
// whose retry finds the required check running again is genuinely pending, so
// the wait supersedes the deferred Merging -> In Review refusal. The issue stays
// in Merging for the coordinator's delayed landing retry.
func TestLandWaitAfterRetryableGateKeepsIssueInMerging(t *testing.T) {
	api, git, linear := newAPI(t), &fakeGit{}, &fakeLinear{}
	api.prExists = true
	api.checkRuns = failingChecks("ci/build")
	_, session := testLandingSession(t, api, git, linear, []string{"ci/build"}, "merge")
	session.settings.LandFixEnabled = true
	session.settings.MaxLandAttempts = 1

	_, err := session.Land(context.Background())
	var gate *LandGateError
	if !errors.As(err, &gate) {
		t.Fatalf("failing check must be granted as a retryable gate, got err=%v", err)
	}
	api.mu.Lock()
	api.checkRuns = []map[string]any{{"name": "ci/build", "status": "in_progress", "conclusion": nil}}
	api.mu.Unlock()

	result, err := session.Land(context.Background())
	if err != nil || result.Status != LandWaiting {
		t.Fatalf("re-running check must wait: result=%+v err=%v", result, err)
	}
	session.FinalizeLanding(context.Background())
	if linear.refused != 0 || len(linear.landComments) != 0 || api.merges != 0 {
		t.Fatalf("a pending check must leave the issue in Merging: refused=%d comments=%v merges=%d", linear.refused, linear.landComments, api.merges)
	}
	// A genuine hard gate after the wait still applies the fallback exactly once.
	api.mu.Lock()
	api.checkRuns = failingChecks("ci/build")
	api.mu.Unlock()
	if _, err := session.Land(context.Background()); err == nil {
		t.Fatal("a failing check after the wait must still refuse")
	}
	if linear.refused != 1 || len(linear.landComments) != 1 {
		t.Fatalf("exhausted gate after a wait must refuse once: refused=%d comments=%v", linear.refused, linear.landComments)
	}
	session.FinalizeLanding(context.Background())
	if linear.refused != 1 || len(linear.landComments) != 1 {
		t.Fatalf("finalize after the refusal must be a no-op: refused=%d comments=%v", linear.refused, linear.landComments)
	}
}

// TestLandingResolvedClosesTheCapabilityOnlyOnTerminalSuccess covers the
// PMR-78 duplicate-call guard the Codex tool dispatch consults: a terminal,
// fully reconciled landing closes github_land_pr for the run, while a wait or a
// merge whose Linear completion failed must stay open for recovery.
func TestLandingResolvedClosesTheCapabilityOnlyOnTerminalSuccess(t *testing.T) {
	t.Run("waiting keeps the capability open", func(t *testing.T) {
		api, git, linear := newAPI(t), &fakeGit{}, &fakeLinear{}
		api.prExists = true
		_, session := testLandingSession(t, api, git, linear, []string{"ci/build"}, "merge")
		result, err := session.Land(context.Background())
		if err != nil || result.Status != LandWaiting {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		if session.LandingResolved() {
			t.Fatal("a waiting landing must not close the capability")
		}
	})
	t.Run("terminal landing closes the capability", func(t *testing.T) {
		api, git, linear := newAPI(t), &fakeGit{}, &fakeLinear{}
		api.prExists = true
		passingRequiredChecks(api, "ci/build")
		readyToLand(api)
		_, session := testLandingSession(t, api, git, linear, []string{"ci/build"}, "merge")
		if session.LandingResolved() {
			t.Fatal("a fresh session must not report a resolved landing")
		}
		result, err := session.Land(context.Background())
		if err != nil || result.Status != LandMerged {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		if !session.LandingResolved() {
			t.Fatal("a merged and reconciled landing must close the capability for this run")
		}
		if api.merges != 1 || linear.landCompleted != 1 {
			t.Fatalf("merges=%d completions=%d, want exactly one of each", api.merges, linear.landCompleted)
		}
	})
	t.Run("failed linear completion keeps the recovery path open", func(t *testing.T) {
		api, git, linear := newAPI(t), &fakeGit{}, &fakeLinear{}
		api.prExists = true
		passingRequiredChecks(api, "ci/build")
		readyToLand(api)
		linear.completeErr = errors.New("linear unavailable")
		_, session := testLandingSession(t, api, git, linear, []string{"ci/build"}, "merge")
		if _, err := session.Land(context.Background()); err == nil {
			t.Fatal("a failed Linear completion must be reported")
		}
		if session.LandingResolved() {
			t.Fatal("an unreconciled merge must keep the capability open for recovery")
		}
		linear.completeErr = nil
		if result, err := session.Land(context.Background()); err != nil || result.Status != LandMerged {
			t.Fatalf("recovery landing result=%+v err=%v", result, err)
		}
		if !session.LandingResolved() || api.merges != 1 {
			t.Fatalf("resolved=%v merges=%d", session.LandingResolved(), api.merges)
		}
	})
}

// TestLandNonRetryableGateRefusesImmediatelyEvenWithFixEnabled confirms a
// non-retryable gate (changes-requested) refuses immediately with the feature
// on, grants no fix attempt, and leaves FinalizeLanding a no-op.
func TestLandNonRetryableGateRefusesImmediatelyEvenWithFixEnabled(t *testing.T) {
	api, git, linear := newAPI(t), &fakeGit{}, &fakeLinear{}
	api.prExists = true
	passingRequiredChecks(api, "ci/build")
	api.reviews = []map[string]any{
		{"user": map[string]any{"login": "alice"}, "state": "APPROVED", "body": "lgtm", "submitted_at": "t1"},
		{"user": map[string]any{"login": "bob"}, "state": "CHANGES_REQUESTED", "body": "no", "submitted_at": "t2"},
	}
	api.mergeable = boolPtr(true)
	_, session := testLandingSession(t, api, git, linear, []string{"ci/build"}, "merge")
	session.settings.LandFixEnabled = true
	session.settings.MaxLandAttempts = 2
	session.settings.AllowConflictResolution = true

	_, err := session.Land(context.Background())
	var gate *LandGateError
	if err == nil || errors.As(err, &gate) || !strings.Contains(err.Error(), "changes-requested") {
		t.Fatalf("non-retryable gate must refuse immediately, got err=%v", err)
	}
	if linear.refused != 1 || session.landAttempts != 0 || session.retryableGateHit {
		t.Fatalf("refused=%d attempts=%d hit=%v", linear.refused, session.landAttempts, session.retryableGateHit)
	}
	session.FinalizeLanding(context.Background())
	if linear.refused != 1 || len(linear.landComments) != 0 {
		t.Fatalf("finalize after an immediate refusal must be a no-op: refused=%d comments=%v", linear.refused, linear.landComments)
	}
}

func TestLandRefusesOnDivergedWorktreeHead(t *testing.T) {
	api, linear := newAPI(t), &fakeLinear{}
	api.prExists, api.prSHA = true, "old-head"
	passingRequiredChecks(api, "ci/build")
	readyToLand(api)
	git := &divergedHeadGit{fakeGit: &fakeGit{}}
	settings := api.settings()
	settings.MergeState, settings.MergeMethod, settings.RequiredChecks = "Merging", "merge", []string{"ci/build"}
	m := New(func() config.Settings { return config.Settings{GitHub: settings} }, slog.Default())
	m.git = git
	session := &Session{manager: m, settings: settings, issue: domain.Issue{ID: "issue-27", Identifier: "PMR-27", Title: "Lifecycle", URL: "https://linear.app/issue/PMR-27"}, workspace: t.TempDir(), branch: "symphony/pmr-27", linear: linear}
	if _, err := session.Land(context.Background()); err == nil || !strings.Contains(err.Error(), "diverged") {
		t.Fatalf("diverged head error=%v", err)
	}
	if linear.refused != 1 || api.merges != 0 {
		t.Fatalf("refused=%d merges=%d", linear.refused, api.merges)
	}
	// A diverged worktree head is a different gate from failing required
	// checks and must be named distinctly in the recorded reason (PMR-159).
	if !strings.Contains(linear.refusedReason, "diverged") || strings.Contains(linear.refusedReason, "required checks failed") {
		t.Fatalf("refused reason did not name the diverged-head gate: %q", linear.refusedReason)
	}
}

func TestLandPushesNewLocalCommitsBeforeLanding(t *testing.T) {
	api, linear := newAPI(t), &fakeLinear{}
	api.prExists, api.prSHA = true, "old-head"
	passingRequiredChecks(api, "ci/build")
	readyToLand(api)
	base := &fakeGit{}
	git := &pushSyncGit{fakeGit: base, api: api, newHead: "head"}
	_, session := testLandingSession(t, api, git, linear, []string{"ci/build"}, "merge")
	result, err := session.Land(context.Background())
	if err != nil {
		t.Fatalf("push-then-land failed: %v", err)
	}
	if result.Status != LandMerged {
		t.Fatalf("result=%+v", result)
	}
	base.mu.Lock()
	foundPush := false
	for _, call := range base.calls {
		if call[0] == "push" {
			foundPush = true
		}
	}
	base.mu.Unlock()
	if !foundPush {
		t.Fatal("new local commits were not pushed before landing")
	}
	// Feature-off parity: pushing new commits during landing must not post any
	// audit comment. The bounded-fix audit trail is gated behind LandFixEnabled.
	if len(linear.landComments) != 0 || len(api.prComments) != 0 {
		t.Fatalf("feature-off push posted comments: linear=%v pr=%v", linear.landComments, api.prComments)
	}
}

// TestLandPushFailureForwardsNoRawGitTextToTheAgent pins the review fix on
// PMR-163: execGit.Run now returns real git/GitHub output on failure (so
// Publish's push gate can log it), and Land's own stale-branch push at
// land.go's push-before-land path shares that same gitRunner. Before this
// fix, Land forwarded that error verbatim to the agent through
// internal/capability/github.go's Land failure message -- exactly the raw,
// possibly credential-shaped text Publish's push gate deliberately keeps off
// the agent-facing path. The refusal reason reaching the caller must stay the
// fixed, host-authored hint, and the raw git error must reach only the host
// log.
func TestLandPushFailureForwardsNoRawGitTextToTheAgent(t *testing.T) {
	api, linear := newAPI(t), &fakeLinear{}
	api.prExists, api.prSHA = true, "old-head"
	passingRequiredChecks(api, "ci/build")
	readyToLand(api)
	const pushErr = "refusing to allow a Personal Access Token to create or update workflow `.github/workflows/ci.yml` without `workflow` scope"
	base := &fakeGit{}
	git := &failingGit{fakeGit: base, failArgs: []string{"push", "https://github.com/owner/repo.git", "HEAD:refs/heads/symphony/pmr-27"}, message: pushErr}
	manager, session := testLandingSession(t, api, git, linear, []string{"ci/build"}, "merge")
	var log bytes.Buffer
	manager.logger = slog.New(slog.NewJSONHandler(&log, nil))

	_, err := session.Land(context.Background())
	if err == nil || strings.Contains(err.Error(), pushErr) {
		t.Fatalf("agent-facing land error = %v, want only the fixed hint", err)
	}
	if !strings.Contains(err.Error(), "could not push branch symphony/pmr-27") {
		t.Fatalf("agent-facing land error missing the fixed hint: %v", err)
	}
	output := log.String()
	if !strings.Contains(output, `"push_error"`) || !strings.Contains(output, pushErr) {
		t.Fatalf("host log missing the underlying git push error: %s", output)
	}
	if linear.refused != 1 {
		t.Fatalf("refused=%d, want the push failure to refuse landing", linear.refused)
	}
}

func TestLandRefusesOnHumanLinearStateOverride(t *testing.T) {
	api, git := newAPI(t), &fakeGit{}
	api.prExists = true
	passingRequiredChecks(api, "ci/build")
	readyToLand(api)
	linear := &fakeLinear{mergeStateErr: errors.New("issue moved by a human")}
	_, session := testLandingSession(t, api, git, linear, []string{"ci/build"}, "merge")
	if _, err := session.Land(context.Background()); err == nil {
		t.Fatal("human Linear state override was not refused")
	}
	if api.merges != 0 {
		t.Fatalf("human override must never merge: merges=%d", api.merges)
	}
}

func TestLandRefusesOnClosedOrMismatchedPullRequest(t *testing.T) {
	for _, test := range []struct {
		name      string
		configure func(*apiFixture)
		wantText  string
	}{
		{name: "closed", configure: func(api *apiFixture) { api.prState = "closed" }, wantText: "closed"},
		{name: "mismatched base", configure: func(api *apiFixture) { api.prBaseRef = "develop" }, wantText: "mismatched"},
		{name: "ambiguous", configure: func(api *apiFixture) { api.multiplePulls = true }, wantText: "more than one"},
	} {
		t.Run(test.name, func(t *testing.T) {
			api, git, linear := newAPI(t), &fakeGit{}, &fakeLinear{}
			api.prExists = true
			test.configure(api)
			_, session := testLandingSession(t, api, git, linear, []string{"ci/build"}, "merge")
			if _, err := session.Land(context.Background()); err == nil || !strings.Contains(err.Error(), test.wantText) {
				t.Fatalf("error=%v want substring %q", err, test.wantText)
			}
			if api.merges != 0 {
				t.Fatalf("mismatched pull request must never merge: merges=%d", api.merges)
			}
		})
	}
}

func TestLandRequiresNoPublishedPullRequest(t *testing.T) {
	api, git, linear := newAPI(t), &fakeGit{}, &fakeLinear{}
	_, session := testLandingSession(t, api, git, linear, []string{"ci/build"}, "merge")
	if _, err := session.Land(context.Background()); err == nil || !strings.Contains(err.Error(), "existing pull request") {
		t.Fatalf("error=%v", err)
	}
}

func TestLandMergesWithEveryConfiguredMethod(t *testing.T) {
	for _, method := range []string{"merge", "squash", "rebase"} {
		t.Run(method, func(t *testing.T) {
			api, git, linear := newAPI(t), &fakeGit{}, &fakeLinear{}
			api.prExists = true
			passingRequiredChecks(api, "ci/build")
			readyToLand(api)
			_, session := testLandingSession(t, api, git, linear, []string{"ci/build"}, method)
			result, err := session.Land(context.Background())
			if err != nil {
				t.Fatalf("landing failed: %v", err)
			}
			if result.Status != LandMerged || result.Method != method || result.Number != api.prNumber {
				t.Fatalf("result=%+v", result)
			}
			if api.merges != 1 || len(api.mergeMethods) != 1 || api.mergeMethods[0] != method {
				t.Fatalf("merges=%d methods=%v", api.merges, api.mergeMethods)
			}
			if linear.landCompleted != 1 {
				t.Fatalf("linear completion=%d", linear.landCompleted)
			}
		})
	}
}

func TestLandDuplicateCallAfterMergeIsIdempotent(t *testing.T) {
	api, git, linear := newAPI(t), &fakeGit{}, &fakeLinear{}
	api.prExists = true
	passingRequiredChecks(api, "ci/build")
	readyToLand(api)
	_, session := testLandingSession(t, api, git, linear, []string{"ci/build"}, "merge")
	first, err := session.Land(context.Background())
	if err != nil || first.Status != LandMerged {
		t.Fatalf("first landing failed: result=%+v err=%v", first, err)
	}
	second, err := session.Land(context.Background())
	if err != nil || second.Status != LandMerged {
		t.Fatalf("duplicate landing failed: result=%+v err=%v", second, err)
	}
	if api.merges != 1 {
		t.Fatalf("duplicate landing merged again: merges=%d", api.merges)
	}
	if linear.landCompleted != 1 {
		t.Fatalf("duplicate landing completion=%d", linear.landCompleted)
	}
}

// TestLandCompletionRefreshesBaseRefAfterMerge asserts a successful
// github_land_pr merge refreshes refs/remotes/origin/<base>. Landing merges
// through the GitHub API and otherwise never fetches afterwards, so this is
// the one remaining way local state reliably goes stale (PMR-135).
func TestLandCompletionRefreshesBaseRefAfterMerge(t *testing.T) {
	api := newAPI(t)
	api.prExists = true
	passingRequiredChecks(api, "ci/build")
	readyToLand(api)
	git := &fakeGit{}
	linear := &fakeLinear{}
	_, session := testLandingSession(t, api, git, linear, []string{"ci/build"}, "merge")
	result, err := session.Land(context.Background())
	if err != nil || result.Status != LandMerged {
		t.Fatalf("landing failed: result=%+v err=%v", result, err)
	}
	fetches := 0
	for _, call := range git.calls {
		if len(call) == 3 && call[0] == "fetch" && call[1] == "origin" && call[2] == "main" {
			fetches++
		}
	}
	if fetches != 3 {
		t.Fatalf("git fetch origin main calls=%d, want exactly 3 (two pre-merge stale-base checks plus the post-merge refresh)", fetches)
	}
}

// TestLandPostMergeBaseRefRefreshFailureIsLoggedNotFatal asserts a failed
// post-merge fetch does not undo an already-succeeded, irreversible GitHub
// merge: it is logged and Land still reports LandMerged (PMR-135).
func TestLandPostMergeBaseRefRefreshFailureIsLoggedNotFatal(t *testing.T) {
	api := newAPI(t)
	api.prExists = true
	passingRequiredChecks(api, "ci/build")
	readyToLand(api)
	git := &countingFailFetchGit{fakeGit: &fakeGit{}, failOnFetch: 3}
	linear := &fakeLinear{}
	manager, session := testLandingSession(t, api, git, linear, []string{"ci/build"}, "merge")
	var log bytes.Buffer
	manager.logger = slog.New(slog.NewJSONHandler(&log, nil))

	result, err := session.Land(context.Background())
	if err != nil {
		t.Fatalf("post-land base ref refresh failure must not fail landing: %v", err)
	}
	if result.Status != LandMerged {
		t.Fatalf("result=%+v", result)
	}
	if linear.landCompleted != 1 {
		t.Fatalf("linear completion=%d", linear.landCompleted)
	}
	if !strings.Contains(log.String(), "post-land base ref refresh failed") {
		t.Fatalf("log=%q, want the post-land refresh failure logged", log.String())
	}
}

func TestLandReconcilesWhenGitHubSucceedsButLinearCompletionFails(t *testing.T) {
	api, git, linear := newAPI(t), &fakeGit{}, &fakeLinear{}
	api.prExists = true
	passingRequiredChecks(api, "ci/build")
	readyToLand(api)
	linear.completeErr = errors.New("linear unavailable")
	_, session := testLandingSession(t, api, git, linear, []string{"ci/build"}, "merge")
	if _, err := session.Land(context.Background()); err == nil {
		t.Fatal("Linear completion failure after a successful merge must be reported")
	}
	if api.merges != 1 {
		t.Fatalf("merge must have happened exactly once: merges=%d", api.merges)
	}
	// Recovery: GitHub already reports the PR merged, so a retry must not
	// merge again and must reconcile Linear to Done idempotently.
	linear.completeErr = nil
	result, err := session.Land(context.Background())
	if err != nil {
		t.Fatalf("recovery landing failed: %v", err)
	}
	if result.Status != LandMerged {
		t.Fatalf("result=%+v", result)
	}
	if api.merges != 1 {
		t.Fatalf("recovery must not merge again: merges=%d", api.merges)
	}
	if linear.landCompleted != 1 {
		t.Fatalf("recovery completion=%d", linear.landCompleted)
	}
}
