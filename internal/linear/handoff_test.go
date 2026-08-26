package linear

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/domain"
)

type handoffFixture struct {
	t                   *testing.T
	mu                  sync.Mutex
	server              *httptest.Server
	stateID             string
	stateName           string
	project             string
	team                string
	comments            []string
	commentAttempts     int
	transitionAttempts  int
	failComment         bool
	failTransition      bool
	ambiguousComment    bool
	ambiguousTransition bool
	commentIssue        string
	commentProject      string
	commentTeam         string
	readAttempts        int
	failRead            bool
	failReadBody        string
	graphqlErrorRead    bool
	changeOnRead        int
	changedStateID      string
	changedStateName    string
	postTransitionID    string
	postTransitionName  string
}

func newHandoffFixture(t *testing.T) *handoffFixture {
	t.Helper()
	f := &handoffFixture{t: t, stateID: "todo", stateName: "Todo", project: "project-1", team: "team-1"}
	f.server = httptest.NewServer(http.HandlerFunc(f.serveHTTP))
	t.Cleanup(f.server.Close)
	return f
}

func (f *handoffFixture) settings() config.Settings {
	return config.Settings{Tracker: config.Tracker{
		Provider:               map[string]any{"api_key": "test-token", "project_slug_id": "project-1", "endpoint": f.server.URL},
		ActiveStates:           []string{"Todo", "In Progress"},
		TerminalStates:         []string{"Done"},
		HandoffState:           "In Review",
		HandoffCommentTemplate: "  Ready {{.issue.identifier}}  ",
	}}
}

func (f *handoffFixture) session(t *testing.T, logger *slog.Logger) *HandoffSession {
	t.Helper()
	h := NewHandoff(f.settings)
	if logger != nil {
		h.logger = logger
	}
	session, err := h.Prepare(context.Background(), domain.Issue{ID: "active"})
	if err != nil {
		t.Fatal(err)
	}
	return session
}

func (f *handoffFixture) serveHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	request := decodeRequest(f.t, r)
	query := request["query"].(string)
	variables := request["variables"].(map[string]any)
	switch {
	case strings.Contains(query, "SymphonyLinearHandoffIssue"):
		f.readAttempts++
		if f.failRead {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(f.failReadBody))
			return
		}
		if f.graphqlErrorRead {
			writeJSON(f.t, w, map[string]any{"errors": []map[string]any{{"message": "private agent payload test-token"}}})
			return
		}
		if f.changeOnRead == f.readAttempts {
			f.stateID, f.stateName = f.changedStateID, f.changedStateName
		}
		writeJSON(f.t, w, map[string]any{"data": map[string]any{"issue": map[string]any{
			"id": "active", "identifier": "PMR-18", "title": "Handoff", "description": "private agent payload", "url": "https://linear.app/issue/PMR-18",
			"project": map[string]string{"slugId": f.project}, "team": map[string]string{"id": f.team}, "state": map[string]string{"id": f.stateID, "name": f.stateName},
		}}})
	case strings.Contains(query, "SymphonyLinearHandoffStates"):
		writeJSON(f.t, w, map[string]any{"data": map[string]any{"team": map[string]any{"id": "team-1", "states": map[string]any{"nodes": []any{
			map[string]string{"id": "todo", "name": "Todo"}, map[string]string{"id": "in-progress", "name": "In Progress"}, map[string]string{"id": "merging", "name": "Merging"}, map[string]string{"id": "review", "name": "In Review"}, map[string]string{"id": "done", "name": "Done"},
		}}}}})
	case strings.Contains(query, "SymphonyLinearHandoffComments"):
		nodes := make([]any, 0, len(f.comments))
		for _, body := range f.comments {
			nodes = append(nodes, map[string]any{"body": body})
		}
		issueID, project, team := "active", "project-1", "team-1"
		if f.commentIssue != "" {
			issueID = f.commentIssue
		}
		if f.commentProject != "" {
			project = f.commentProject
		}
		if f.commentTeam != "" {
			team = f.commentTeam
		}
		writeJSON(f.t, w, map[string]any{"data": map[string]any{"issue": map[string]any{"id": issueID, "project": map[string]string{"slugId": project}, "team": map[string]string{"id": team}, "comments": map[string]any{"nodes": nodes, "pageInfo": map[string]any{"hasNextPage": false, "endCursor": nil}}}}})
	case strings.Contains(query, "SymphonyLinearHandoffComment"):
		f.commentAttempts++
		if got := variables["issueID"]; got != "active" {
			f.t.Errorf("comment issueID=%v", got)
		}
		if got := variables["body"]; got != "Ready PMR-18" && got != "bounded comment" && got != "https://github.com/owner/repo/pull/7" {
			f.t.Errorf("comment body=%q", got)
		}
		if f.failComment {
			f.failComment = false
			writeJSON(f.t, w, map[string]any{"data": map[string]any{"commentCreate": map[string]bool{"success": false}}})
			return
		}
		f.comments = append(f.comments, variables["body"].(string))
		if f.ambiguousComment {
			f.ambiguousComment = false
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"errors":[{"message":"private agent payload test-token"}]}`))
			return
		}
		writeJSON(f.t, w, map[string]any{"data": map[string]any{"commentCreate": map[string]bool{"success": true}}})
	case strings.Contains(query, "SymphonyLinearHandoffTransition"):
		f.transitionAttempts++
		if got := variables["issueID"]; got != "active" {
			f.t.Errorf("transition issueID=%v", got)
		}
		if got := variables["stateID"]; got != "review" && got != "done" && got != "in-progress" {
			f.t.Errorf("transition stateID=%v", got)
		}
		if f.failTransition {
			f.failTransition = false
			writeJSON(f.t, w, map[string]any{"data": map[string]any{"issueUpdate": map[string]bool{"success": false}}})
			return
		}
		if variables["stateID"] == "done" {
			f.stateID, f.stateName = "done", "Done"
		} else if variables["stateID"] == "in-progress" {
			f.stateID, f.stateName = "in-progress", "In Progress"
		} else {
			f.stateID, f.stateName = "review", "In Review"
		}
		if f.postTransitionID != "" {
			f.stateID, f.stateName = f.postTransitionID, f.postTransitionName
		}
		if f.ambiguousTransition {
			f.ambiguousTransition = false
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"errors":[{"message":"private agent payload test-token"}]}`))
			return
		}
		writeJSON(f.t, w, map[string]any{"data": map[string]any{"issueUpdate": map[string]bool{"success": true}}})
	default:
		f.t.Fatalf("unexpected GraphQL operation: %s", query)
	}
}

// callHandoff drives the host-side review handoff (comment + transition to the
// configured handoff state) through the same locked path github_publish_pr's
// LinkAndHandoff uses. There is no agent-invocable handoff; this exercises the
// host reconciliation logic directly.
func callHandoff(session *HandoffSession) error {
	session.handoffMu.Lock()
	defer session.handoffMu.Unlock()
	return session.handoffLocked(context.Background())
}

// EnsureActive is the revalidation github_publish_pr performs immediately
// before it creates a pull request, so these tests drive the real scoped read
// and the real scope check rather than internal/github's lifecycle fake.

func TestEnsureActiveAcceptsTheBoundInitialAndHandoffTargetStates(t *testing.T) {
	for name, state := range map[string][2]string{
		"initial state":  {"todo", "Todo"},
		"handoff target": {"review", "In Review"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newHandoffFixture(t)
			session := f.session(t, nil)
			f.stateID, f.stateName = state[0], state[1]
			if err := session.EnsureActive(context.Background()); err != nil {
				t.Fatal(err)
			}
			// Revalidation is a read: it must never write the tracker itself.
			if f.commentAttempts != 0 || f.transitionAttempts != 0 {
				t.Fatalf("revalidation mutated the issue: comments=%d transitions=%d", f.commentAttempts, f.transitionAttempts)
			}
		})
	}
}

func TestEnsureActiveRefusesAStateAHumanChangedAfterSessionSetup(t *testing.T) {
	f := newHandoffFixture(t)
	session := f.session(t, nil)
	// Neither the state the session was bound to nor the review handoff target:
	// a pull request must not be created for an issue that has moved.
	f.stateID, f.stateName = "in-progress", "In Progress"
	err := session.EnsureActive(context.Background())
	if err == nil {
		t.Fatal("a stale session was cleared to perform an irreversible mutation")
	}
	assertCategory(t, err, "handoff_scope")
}

func TestEnsureActivePropagatesAScopedReadFailure(t *testing.T) {
	f := newHandoffFixture(t)
	session := f.session(t, nil)
	// An unavailable tracker leaves the current state unknown, which must fail
	// closed rather than be read as "unchanged".
	f.failRead = true
	err := session.EnsureActive(context.Background())
	if err == nil {
		t.Fatal("an unreadable issue was cleared to perform an irreversible mutation")
	}
	assertCategory(t, err, "tracker_status")
}

// TestEnsureActiveAndLinkAndHandoffErrorsCarryNoProviderWireContent guards the
// sanitization github.Session.Publish and, downstream, the capability layer
// rely on when they forward EnsureActive's and LinkAndHandoff's err.Error()
// straight to the agent (PMR-149): whatever text a malicious or misbehaving
// Linear response plants in the wire payload -- an HTTP error body or a
// GraphQL errors entry -- must never appear in the resulting message.
func TestEnsureActiveAndLinkAndHandoffErrorsCarryNoProviderWireContent(t *testing.T) {
	const secret = "wire-secret-should-never-reach-the-agent"

	t.Run("EnsureActive non-2xx response body", func(t *testing.T) {
		f := newHandoffFixture(t)
		session := f.session(t, nil)
		f.failRead, f.failReadBody = true, secret
		err := session.EnsureActive(context.Background())
		if err == nil || strings.Contains(err.Error(), secret) {
			t.Fatalf("EnsureActive leaked provider response body: %v", err)
		}
		if err.Error() != "linear tracker_status: Linear returned HTTP status 503" {
			t.Fatalf("EnsureActive error = %q", err.Error())
		}
	})

	t.Run("EnsureActive GraphQL errors payload", func(t *testing.T) {
		f := newHandoffFixture(t)
		session := f.session(t, nil)
		f.graphqlErrorRead = true
		err := session.EnsureActive(context.Background())
		if err == nil || strings.Contains(err.Error(), "private agent payload") {
			t.Fatalf("EnsureActive leaked a GraphQL errors payload: %v", err)
		}
		if err.Error() != "linear tracker_response: Linear returned GraphQL errors" {
			t.Fatalf("EnsureActive error = %q", err.Error())
		}
	})

	t.Run("LinkAndHandoff ambiguous comment mutation", func(t *testing.T) {
		f := newHandoffFixture(t)
		f.ambiguousComment = true
		session := f.session(t, nil)
		err := session.LinkAndHandoff(context.Background(), "https://github.com/owner/repo/pull/7")
		if err == nil || strings.Contains(err.Error(), "private agent payload") {
			t.Fatalf("LinkAndHandoff leaked provider response text: %v", err)
		}
		if err.Error() != "linear tracker_status: Linear returned HTTP status 500" {
			t.Fatalf("LinkAndHandoff error = %q", err.Error())
		}
	})
}

// The following tests exercise the PMR-37 github_land_pr Linear surface:
// EnsureMergeState (exact-state preflight/postflight gate), RefuseLanding
// (the Merging -> In Review hard-gate fallback), and CompleteLanding (the
// Merging -> Done landing completion).

func mergingSession(t *testing.T, f *handoffFixture, refuseLanding map[string]string) *HandoffSession {
	t.Helper()
	f.stateID, f.stateName = "merging", "Merging"
	settings := f.settings()
	// A landing session always coexists with the review handoff state in
	// production (it is what enables the bound session); Merging must be an
	// active state for that binding. The refuse_landing fallback edge is the
	// host-owned Merging -> In Review policy RefuseLanding consumes.
	settings.Tracker.ActiveStates = []string{"Todo", "In Progress", "Merging"}
	settings.Tracker.HandoffState = "In Review"
	settings.Tracker.HostTransitions = config.HostTransitions{RefuseLanding: refuseLanding}
	session, err := NewHandoff(func() config.Settings { return settings }).Prepare(context.Background(), domain.Issue{ID: "active"})
	if err != nil {
		t.Fatal(err)
	}
	return session
}

func TestEnsureMergeStateRequiresExactCurrentState(t *testing.T) {
	f := newHandoffFixture(t)
	session := mergingSession(t, f, map[string]string{"Merging": "In Review"})
	if err := session.EnsureMergeState(context.Background(), "Merging"); err != nil {
		t.Fatalf("still in Merging: %v", err)
	}
	f.stateID, f.stateName = "review", "In Review"
	if err := session.EnsureMergeState(context.Background(), "Merging"); err == nil {
		t.Fatal("human state change away from Merging was not detected")
	}
}

func TestRefuseLandingAppliesConfiguredFallbackOnlyFromExactMergeState(t *testing.T) {
	f := newHandoffFixture(t)
	session := mergingSession(t, f, map[string]string{"Merging": "In Review"})
	changed, err := session.RefuseLanding(context.Background(), "Merging")
	if err != nil || !changed || f.stateName != "In Review" || f.transitionAttempts != 1 {
		t.Fatalf("changed=%v err=%v state=%s transitions=%d", changed, err, f.stateName, f.transitionAttempts)
	}
	// The issue is no longer exactly in Merging (a prior call already moved
	// it, or a human did): a retry must be a safe no-op, not another edge.
	changed, err = session.RefuseLanding(context.Background(), "Merging")
	if err != nil || changed || f.transitionAttempts != 1 {
		t.Fatalf("second call changed=%v err=%v transitions=%d", changed, err, f.transitionAttempts)
	}
}

func TestRefuseLandingIsANoOpWithoutAConfiguredFallbackEdge(t *testing.T) {
	f := newHandoffFixture(t)
	// No "Merging" source configured: only an unrelated edge exists.
	session := mergingSession(t, f, map[string]string{"Todo": "In Progress"})
	changed, err := session.RefuseLanding(context.Background(), "Merging")
	if err != nil || changed || f.transitionAttempts != 0 {
		t.Fatalf("changed=%v err=%v transitions=%d", changed, err, f.transitionAttempts)
	}
	if f.stateName != "Merging" {
		t.Fatalf("state changed without a configured edge: %s", f.stateName)
	}
}

func TestCompleteLandingTransitionsMergingToDoneIdempotently(t *testing.T) {
	f := newHandoffFixture(t)
	session := mergingSession(t, f, map[string]string{"Merging": "In Review"})
	changed, err := session.CompleteLanding(context.Background(), "Merging")
	if err != nil || !changed || f.stateName != "Done" || f.transitionAttempts != 1 {
		t.Fatalf("changed=%v err=%v state=%s transitions=%d", changed, err, f.stateName, f.transitionAttempts)
	}
	// Duplicate landing / GitHub-success-then-Linear-failure recovery: a
	// retry once already Done must not attempt another mutation.
	changed, err = session.CompleteLanding(context.Background(), "Merging")
	if err != nil || changed || f.transitionAttempts != 1 {
		t.Fatalf("duplicate completion changed=%v err=%v transitions=%d", changed, err, f.transitionAttempts)
	}
}

func TestCompleteLandingRejectsIssueNoLongerInTheMergingState(t *testing.T) {
	f := newHandoffFixture(t)
	session := mergingSession(t, f, map[string]string{"Merging": "In Review"})
	// A human (or an earlier hard-gate refusal) already moved the issue away
	// from Merging before this completion call arrives.
	f.stateID, f.stateName = "review", "In Review"
	if _, err := session.CompleteLanding(context.Background(), "Merging"); err == nil {
		t.Fatal("completion from an unexpected state was not rejected")
	}
	if f.transitionAttempts != 0 {
		t.Fatalf("mutation attempts=%d", f.transitionAttempts)
	}
}

// LandComment carries the landing audit trail (pushed fix-turn commit SHAs and
// the last failed gate on a deferred refusal). Its body is composed host-side,
// but it is still bounded here, at the only place that posts it.

func TestLandCommentPostsAHostGeneratedBodyWithoutTransitioning(t *testing.T) {
	f := newHandoffFixture(t)
	session := mergingSession(t, f, map[string]string{"Merging": "In Review"})
	if err := session.LandComment(context.Background(), "bounded comment"); err != nil {
		t.Fatal(err)
	}
	if len(f.comments) != 1 || f.comments[0] != "bounded comment" || f.transitionAttempts != 0 {
		t.Fatalf("comments=%v transitions=%d", f.comments, f.transitionAttempts)
	}
}

func TestLandCommentRejectsBodiesOutsideTheValidatedBound(t *testing.T) {
	for name, body := range map[string]string{
		"empty":     "   ",
		"too large": strings.Repeat("a", maxHandoffCommentBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			f := newHandoffFixture(t)
			session := mergingSession(t, f, map[string]string{"Merging": "In Review"})
			err := session.LandComment(context.Background(), body)
			if err == nil {
				t.Fatal("an unbounded audit comment was accepted")
			}
			assertCategory(t, err, "handoff_request")
			if f.commentAttempts != 0 {
				t.Fatalf("a rejected body still reached Linear: attempts=%d", f.commentAttempts)
			}
		})
	}
}

func TestLandCommentPropagatesAMutationFailure(t *testing.T) {
	f := newHandoffFixture(t)
	f.failComment = true
	session := mergingSession(t, f, map[string]string{"Merging": "In Review"})
	err := session.LandComment(context.Background(), "bounded comment")
	if err == nil {
		t.Fatal("a refused comment mutation was reported as success")
	}
	assertCategory(t, err, "handoff_response")
	if len(f.comments) != 0 {
		t.Fatalf("comments=%v", f.comments)
	}
}

// The following tests exercise the PMR-44 poll reconciliation surface:
// ReconcileMerged moves a poll-observed merged pull request's issue to Done
// from either the review handoff target or the configured Merging state,
// idempotently and human-wins.

func TestReconcileMergedFromMergeStateTransitionsToDone(t *testing.T) {
	f := newHandoffFixture(t)
	session := mergingSession(t, f, map[string]string{"Merging": "In Review"})
	changed, err := session.ReconcileMerged(context.Background(), "Merging")
	if err != nil || !changed || f.stateName != "Done" || f.transitionAttempts != 1 {
		t.Fatalf("changed=%v err=%v state=%s transitions=%d", changed, err, f.stateName, f.transitionAttempts)
	}
	// Idempotent: a second poll once the issue is already Done is a quiet no-op.
	changed, err = session.ReconcileMerged(context.Background(), "Merging")
	if err != nil || changed || f.transitionAttempts != 1 {
		t.Fatalf("duplicate reconcile changed=%v err=%v transitions=%d", changed, err, f.transitionAttempts)
	}
}

func TestReconcileMergedFromReviewStateTransitionsToDone(t *testing.T) {
	f := newHandoffFixture(t)
	session := f.session(t, nil)
	// The issue has already been handed off to the review target state.
	f.stateID, f.stateName = "review", "In Review"
	// The review-target path is independent of landing configuration, so it
	// reconciles even when no Merging state is passed (landing unconfigured).
	changed, err := session.ReconcileMerged(context.Background(), "")
	if err != nil || !changed || f.stateName != "Done" || f.transitionAttempts != 1 {
		t.Fatalf("changed=%v err=%v state=%s transitions=%d", changed, err, f.stateName, f.transitionAttempts)
	}
}

func TestReconcileMergedIsANoOpWhenAHumanMovedTheIssue(t *testing.T) {
	for name, mutate := range map[string]func(*handoffFixture){
		"already done": func(f *handoffFixture) { f.stateID, f.stateName = "done", "Done" },
		"other state":  func(f *handoffFixture) { f.stateID, f.stateName = "in-progress", "In Progress" },
	} {
		t.Run(name, func(t *testing.T) {
			f := newHandoffFixture(t)
			session := mergingSession(t, f, map[string]string{"Merging": "In Review"})
			mutate(f)
			changed, err := session.ReconcileMerged(context.Background(), "Merging")
			if err != nil || changed || f.transitionAttempts != 0 {
				t.Fatalf("changed=%v err=%v transitions=%d", changed, err, f.transitionAttempts)
			}
		})
	}
}

func TestReconcileMergedIsFailClosedForMergeStateWhenLandingUnconfigured(t *testing.T) {
	f := newHandoffFixture(t)
	// The issue sits in Merging, but no merge state is passed (landing is not
	// configured): the Merging path must not fire.
	session := mergingSession(t, f, map[string]string{"Merging": "In Review"})
	changed, err := session.ReconcileMerged(context.Background(), "")
	if err != nil || changed || f.transitionAttempts != 0 {
		t.Fatalf("changed=%v err=%v transitions=%d", changed, err, f.transitionAttempts)
	}
	if f.stateName != "Merging" {
		t.Fatalf("fail-closed reconcile moved the issue: state=%s", f.stateName)
	}
}

func TestHandoffSuccessAndDuplicateDeliveryAreIdempotent(t *testing.T) {
	f := newHandoffFixture(t)
	session := f.session(t, nil)
	if err := callHandoff(session); err != nil {
		t.Fatal(err)
	}
	if err := callHandoff(session); err != nil {
		t.Fatal(err)
	}
	if len(f.comments) != 1 || f.commentAttempts != 1 || f.transitionAttempts != 1 {
		t.Fatalf("comments=%v comment attempts=%d transition attempts=%d", f.comments, f.commentAttempts, f.transitionAttempts)
	}
}

func TestHandoffConcurrentDuplicateDeliveryIsIdempotent(t *testing.T) {
	f := newHandoffFixture(t)
	session := f.session(t, nil)
	errs := make(chan error, 8)
	var calls sync.WaitGroup
	for range 8 {
		calls.Add(1)
		go func() {
			defer calls.Done()
			errs <- callHandoff(session)
		}()
	}
	calls.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(f.comments) != 1 || f.commentAttempts != 1 || f.transitionAttempts != 1 {
		t.Fatalf("comments=%v comment attempts=%d transition attempts=%d", f.comments, f.commentAttempts, f.transitionAttempts)
	}
}

func TestPullRequestLinkAndMergeCompletionAreScopedAndIdempotent(t *testing.T) {
	f := newHandoffFixture(t)
	session := f.session(t, nil)
	url := "https://github.com/owner/repo/pull/7"
	if err := session.LinkAndHandoff(context.Background(), url); err != nil {
		t.Fatal(err)
	}
	if err := session.LinkAndHandoff(context.Background(), url); err != nil {
		t.Fatal(err)
	}
	if len(f.comments) != 2 || f.comments[0] != url || f.commentAttempts != 2 || f.transitionAttempts != 1 || f.stateName != "In Review" {
		t.Fatalf("comments=%v comment attempts=%d transitions=%d state=%s", f.comments, f.commentAttempts, f.transitionAttempts, f.stateName)
	}
	changed, err := session.Complete(context.Background())
	if err != nil || !changed {
		t.Fatalf("first completion changed=%t err=%v", changed, err)
	}
	changed, err = session.Complete(context.Background())
	if err != nil || changed {
		t.Fatalf("duplicate completion changed=%t err=%v", changed, err)
	}
	if f.transitionAttempts != 2 || f.stateName != "Done" {
		t.Fatalf("transitions=%d state=%s", f.transitionAttempts, f.stateName)
	}
}

func TestHandoffCommentFailureRetriesWithoutTransition(t *testing.T) {
	f := newHandoffFixture(t)
	f.failComment = true
	session := f.session(t, nil)
	if err := callHandoff(session); err == nil {
		t.Fatal("comment failure succeeded")
	}
	if f.transitionAttempts != 0 || len(f.comments) != 0 {
		t.Fatalf("transition=%d comments=%v", f.transitionAttempts, f.comments)
	}
	if err := callHandoff(session); err != nil {
		t.Fatal(err)
	}
	if len(f.comments) != 1 || f.transitionAttempts != 1 {
		t.Fatalf("transition=%d comments=%v", f.transitionAttempts, f.comments)
	}
}

func TestHandoffTransitionFailureReconcilesWithoutDuplicateComment(t *testing.T) {
	f := newHandoffFixture(t)
	f.failTransition = true
	session := f.session(t, nil)
	if err := callHandoff(session); err == nil {
		t.Fatal("transition failure succeeded")
	}
	if err := callHandoff(session); err != nil {
		t.Fatal(err)
	}
	if len(f.comments) != 1 || f.commentAttempts != 1 || f.transitionAttempts != 2 {
		t.Fatalf("comments=%v comment attempts=%d transition attempts=%d", f.comments, f.commentAttempts, f.transitionAttempts)
	}
}

func TestHandoffReconcilesAmbiguousMutationResults(t *testing.T) {
	t.Run("comment applied", func(t *testing.T) {
		f := newHandoffFixture(t)
		f.ambiguousComment = true
		session := f.session(t, nil)
		if err := callHandoff(session); err == nil {
			t.Fatal("ambiguous comment succeeded")
		}
		if err := callHandoff(session); err != nil {
			t.Fatal(err)
		}
		if len(f.comments) != 1 || f.commentAttempts != 1 || f.transitionAttempts != 1 {
			t.Fatalf("fixture=%+v", f)
		}
	})
	t.Run("transition applied", func(t *testing.T) {
		f := newHandoffFixture(t)
		f.ambiguousTransition = true
		session := f.session(t, nil)
		if err := callHandoff(session); err == nil {
			t.Fatal("ambiguous transition succeeded")
		}
		if err := callHandoff(session); err != nil {
			t.Fatal(err)
		}
		if len(f.comments) != 1 || f.commentAttempts != 1 || f.transitionAttempts != 1 {
			t.Fatalf("fixture=%+v", f)
		}
	})
}

func TestHandoffReconcilesTargetStateMissingComment(t *testing.T) {
	f := newHandoffFixture(t)
	session := f.session(t, nil)
	f.stateID, f.stateName = "review", "In Review"
	if err := callHandoff(session); err != nil {
		t.Fatal(err)
	}
	if len(f.comments) != 1 || f.transitionAttempts != 0 {
		t.Fatalf("comments=%v transition=%d", f.comments, f.transitionAttempts)
	}
}

// The host handoff posts the bound comment only to the active issue; a comment
// that surfaces on a different issue, project, or team is a scope violation and
// must fail the handoff rather than proceed.
func TestHandoffRejectsCrossScopeComment(t *testing.T) {
	f := newHandoffFixture(t)
	session := f.session(t, nil)
	for name, mutate := range map[string]func(){
		"issue":   func() { f.commentIssue = "other" },
		"project": func() { f.commentProject = "other" },
		"team":    func() { f.commentTeam = "other" },
	} {
		t.Run(name, func(t *testing.T) {
			f.comments = []string{"Ready PMR-18"}
			f.commentIssue, f.commentProject, f.commentTeam = "", "", ""
			mutate()
			if err := callHandoff(session); err == nil {
				t.Fatal("cross-scope comment accepted")
			}
		})
	}
}

// Enabled, SetLogger, and MatchesSecret are the handoff's wiring surface: a
// launcher reads them before any session tool call happens, so no lifecycle
// test reaches them.
func TestHandoffWiringReportsCapabilityLoggerAndSecretMatcher(t *testing.T) {
	f := newHandoffFixture(t)
	h := NewHandoff(f.settings)
	if !h.Enabled() {
		t.Fatal("a configured handoff state must enable the Linear session capability")
	}
	disabled := NewHandoff(func() config.Settings {
		s := f.settings()
		s.Tracker.HandoffState = ""
		return s
	})
	if disabled.Enabled() {
		t.Fatal("no handoff state and no follow-up creation must leave the capability off")
	}
	logger := slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil))
	h.SetLogger(logger)
	h.SetLogger(nil) // A nil logger must not drop the operator handler.
	session, err := h.Prepare(context.Background(), domain.Issue{ID: "active"})
	if err != nil {
		t.Fatal(err)
	}
	if session.logger != logger {
		t.Fatal("the prepared session did not inherit the operator log handler")
	}
	// The launcher filters inherited values containing the credential without
	// ever holding the credential itself.
	if !session.MatchesSecret("LINEAR_TOKEN=test-token") || session.MatchesSecret("LINEAR_TOKEN=other") {
		t.Fatal("secret matcher did not bound the configured credential")
	}
}

func TestHandoffLogsOperationAndEdge(t *testing.T) {
	f := newHandoffFixture(t)
	var logs bytes.Buffer
	session := f.session(t, slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	if err := callHandoff(session); err != nil {
		t.Fatal(err)
	}
	edge := findLogRecord(t, &logs, "Linear transition")
	if edge["operation"] != "handoff" || edge["from_state"] != "Todo" || edge["to_state"] != "In Review" || edge["issue_identifier"] != "PMR-18" {
		t.Fatalf("handoff edge missing operation/from/to/issue: %v", edge)
	}
	// The edge must be redaction-safe: state names only, no rendered comment,
	// issue description, or credential.
	for _, secret := range []string{"test-token", "private agent payload", "Ready PMR-18"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("handoff edge log leaked %q: %s", secret, logs.String())
		}
	}
}
