package linear

import (
	"bytes"
	"context"
	"encoding/json"
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
		Provider:               map[string]any{"api_key": "test-token", "project_slug": "project-1", "endpoint": f.server.URL},
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
		writeJSON(f.t, w, map[string]any{"data": map[string]any{"issue": map[string]any{
			"id": "active", "identifier": "PMR-18", "title": "Handoff", "description": "private agent payload", "url": "https://linear.app/issue/PMR-18",
			"project": map[string]string{"slugId": f.project}, "team": map[string]string{"id": f.team}, "state": map[string]string{"id": f.stateID, "name": f.stateName},
		}}})
	case strings.Contains(query, "SymphonyLinearHandoffStates"):
		writeJSON(f.t, w, map[string]any{"data": map[string]any{"team": map[string]any{"id": "team-1", "states": map[string]any{"nodes": []any{
			map[string]string{"id": "todo", "name": "Todo"}, map[string]string{"id": "review", "name": "In Review"}, map[string]string{"id": "done", "name": "Done"},
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
		if got := variables["stateID"]; got != "review" && got != "done" {
			f.t.Errorf("transition stateID=%v", got)
		}
		if f.failTransition {
			f.failTransition = false
			writeJSON(f.t, w, map[string]any{"data": map[string]any{"issueUpdate": map[string]bool{"success": false}}})
			return
		}
		if variables["stateID"] == "done" {
			f.stateID, f.stateName = "done", "Done"
		} else {
			f.stateID, f.stateName = "review", "In Review"
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

func callHandoff(session *HandoffSession) error {
	_, err := session.Call(context.Background(), json.RawMessage(`{"operation":"handoff"}`))
	return err
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

func TestHandoffRejectsInvalidAndCrossScopeInputs(t *testing.T) {
	f := newHandoffFixture(t)
	session := f.session(t, nil)
	for _, arguments := range []string{
		`null`, `{}`, `{"operation":7}`, `{"operation":"handoff","body":"payload"}`,
		`{"operation":"handoff","issueID":"other"}`, `{"operation":"query","query":"mutation { anything }"}`,
	} {
		if _, err := session.Call(context.Background(), json.RawMessage(arguments)); err == nil {
			t.Errorf("accepted %s", arguments)
		}
	}
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

func TestHandoffLogsOnlySafeContextAndTrimsCommentInput(t *testing.T) {
	f := newHandoffFixture(t)
	var logs bytes.Buffer
	session := f.session(t, slog.New(slog.NewTextHandler(&logs, nil)))
	if _, err := session.Call(context.Background(), json.RawMessage(`{"operation":"comment","body":"  bounded comment  "}`)); err != nil {
		t.Fatal(err)
	}
	if got := f.comments[len(f.comments)-1]; got != "bounded comment" {
		t.Fatalf("comment=%q", got)
	}
	if err := callHandoff(session); err != nil {
		t.Fatal(err)
	}
	logged := logs.String()
	if !strings.Contains(logged, "issue_id=active") || !strings.Contains(logged, "issue_identifier=PMR-18") {
		t.Fatalf("missing safe context: %s", logged)
	}
	for _, secret := range []string{"test-token", "private agent payload", "Ready PMR-18", "bounded comment"} {
		if strings.Contains(logged, secret) {
			t.Fatalf("log leaked %q: %s", secret, logged)
		}
	}
}
