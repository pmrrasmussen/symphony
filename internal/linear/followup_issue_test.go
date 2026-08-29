package linear

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/domain"
)

type followupIssueFixture struct {
	t                  *testing.T
	mu                 sync.Mutex
	server             *httptest.Server
	project, projectID string
	team               string
	stateID, stateName string
	backlogID          string
	nextSeq            int
	created            []map[string]any
	relations          []struct{ Origin, Followup, Kind string }
	failCreate         bool
	failRelation       bool
	failRead           bool
	failReadBody       string
	createdParentID    string
	createdStateName   string
}

func newFollowupIssueFixture(t *testing.T) *followupIssueFixture {
	t.Helper()
	f := &followupIssueFixture{
		t: t, project: "project-1", projectID: "project-id-1", team: "team-1",
		stateID: "in-progress", stateName: "In Progress", backlogID: "backlog",
		createdStateName: "Backlog",
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.serveHTTP))
	t.Cleanup(f.server.Close)
	return f
}

func (f *followupIssueFixture) settings() config.Settings {
	return config.Settings{Tracker: config.Tracker{
		Provider:              map[string]any{"api_key": "test-token", "project_slug_id": "project-1", "endpoint": f.server.URL},
		ActiveStates:          []string{"Todo", "In Progress"},
		TerminalStates:        []string{"Done"},
		FollowupIssueCreation: true,
	}}
}

func (f *followupIssueFixture) session(t *testing.T) *HandoffSession {
	return f.sessionWithSettings(t, f.settings())
}

func (f *followupIssueFixture) sessionWithSettings(t *testing.T, settings config.Settings) *HandoffSession {
	t.Helper()
	session, err := NewHandoff(func() config.Settings { return settings }).Prepare(context.Background(), domain.Issue{ID: "active"})
	if err != nil {
		t.Fatal(err)
	}
	return session
}

func (f *followupIssueFixture) serveHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	request := decodeRequest(f.t, r)
	query := request["query"].(string)
	variables, _ := request["variables"].(map[string]any)
	switch {
	case strings.Contains(query, "SymphonyLinearHandoffIssue"):
		if f.failRead {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(f.failReadBody))
			return
		}
		writeJSON(f.t, w, map[string]any{"data": map[string]any{"issue": map[string]any{
			"id": "active", "identifier": "PMR-41", "title": "Current task", "description": "private agent payload", "url": "https://linear.app/issue/PMR-41",
			"project": map[string]string{"id": f.projectID, "slugId": f.project}, "team": map[string]string{"id": f.team},
			"state": map[string]string{"id": f.stateID, "name": f.stateName},
		}}})
	case strings.Contains(query, "SymphonyLinearHandoffStates"):
		writeJSON(f.t, w, map[string]any{"data": map[string]any{"team": map[string]any{
			"id": f.team, "states": map[string]any{"nodes": []any{
				map[string]string{"id": f.backlogID, "name": "Backlog"},
				map[string]string{"id": "review", "name": "In Review"},
			}},
		}}})
	case strings.Contains(query, "SymphonyLinearCreateFollowupIssueRelated"):
		f.recordRelation(w, variables, "related")
	case strings.Contains(query, "SymphonyLinearCreateFollowupIssueBlockedByCurrent"):
		f.recordRelation(w, variables, "blocks")
	case strings.Contains(query, "SymphonyLinearCreateFollowupIssue"):
		if variables["teamID"] != f.team || variables["projectID"] != f.projectID || variables["stateID"] != f.backlogID {
			f.t.Errorf("unexpected create scope: %#v", variables)
		}
		if _, exists := variables["parentID"]; exists {
			f.t.Errorf("create exposed parentID: %#v", variables)
		}
		if f.failCreate {
			f.failCreate = false
			writeJSON(f.t, w, map[string]any{"data": map[string]any{"issueCreate": map[string]any{"success": false}}})
			return
		}
		f.nextSeq++
		id, identifier := "followup-id-"+itoa(f.nextSeq), "PMR-"+itoa(100+f.nextSeq)
		f.created = append(f.created, variables)
		issue := map[string]any{
			"id": id, "identifier": identifier, "url": "https://linear.app/issue/" + identifier,
			"project": map[string]string{"id": f.projectID}, "team": map[string]string{"id": f.team},
			"state": map[string]string{"id": f.backlogID, "name": f.createdStateName},
		}
		if f.createdParentID != "" {
			issue["parent"] = map[string]string{"id": f.createdParentID}
		}
		writeJSON(f.t, w, map[string]any{"data": map[string]any{"issueCreate": map[string]any{"success": true, "issue": issue}}})
	default:
		f.t.Fatalf("unexpected GraphQL operation: %s", query)
	}
}

func (f *followupIssueFixture) recordRelation(w http.ResponseWriter, variables map[string]any, kind string) {
	if f.failRelation {
		f.failRelation = false
		writeJSON(f.t, w, map[string]any{"data": map[string]any{"issueRelationCreate": map[string]bool{"success": false}}})
		return
	}
	f.relations = append(f.relations, struct{ Origin, Followup, Kind string }{
		Origin: variables["issueID"].(string), Followup: variables["relatedIssueID"].(string), Kind: kind,
	})
	writeJSON(f.t, w, map[string]any{"data": map[string]any{"issueRelationCreate": map[string]bool{"success": true}}})
}

func itoa(n int) string {
	digits := "0123456789"
	if n == 0 {
		return "0"
	}
	out := []byte{}
	for n > 0 {
		out = append([]byte{digits[n%10]}, out...)
		n /= 10
	}
	return string(out)
}

func createFollowupIssue(session *HandoffSession, args map[string]any) (ToolResult, error) {
	body, err := json.Marshal(args)
	if err != nil {
		return ToolResult{}, err
	}
	return session.CreateFollowupIssue(context.Background(), body)
}

func validFollowupArgs() map[string]any {
	return map[string]any{
		"title": "Extract retry improvements", "description": "The current work exposed a separate retry concern.",
		"acceptance_criteria": "- Retry delays are bounded.\n- Focused tests cover rate limiting.",
	}
}

func TestCreateFollowupIssueDisabledUnlessConfigured(t *testing.T) {
	f := newFollowupIssueFixture(t)
	settings := f.settings()
	settings.Tracker.FollowupIssueCreation = false
	settings.Tracker.HandoffState = "In Review"
	session := f.sessionWithSettings(t, settings)
	if _, err := createFollowupIssue(session, validFollowupArgs()); err == nil {
		t.Fatal("follow-up issue creation succeeded while disabled")
	}
	if len(f.created) != 0 {
		t.Fatalf("created issues while disabled: %+v", f.created)
	}
}

func TestCreateFollowupIssueUnconfiguredMeansNoSession(t *testing.T) {
	f := newFollowupIssueFixture(t)
	settings := f.settings()
	settings.Tracker.FollowupIssueCreation = false
	session, err := NewHandoff(func() config.Settings { return settings }).Prepare(context.Background(), domain.Issue{ID: "active"})
	if err != nil {
		t.Fatal(err)
	}
	if session != nil {
		t.Fatal("no Linear session should be prepared when no capability is configured")
	}
}

func TestCreateFollowupIssueRequiresResolvableProjectAndBacklog(t *testing.T) {
	t.Run("project", func(t *testing.T) {
		f := newFollowupIssueFixture(t)
		f.projectID = ""
		if _, err := NewHandoff(f.settings).Prepare(context.Background(), domain.Issue{ID: "active"}); err == nil {
			t.Fatal("session prepared with an unresolved project")
		}
	})
	t.Run("Backlog", func(t *testing.T) {
		f := newFollowupIssueFixture(t)
		f.backlogID = ""
		if _, err := NewHandoff(f.settings).Prepare(context.Background(), domain.Issue{ID: "active"}); err == nil {
			t.Fatal("session prepared without a Backlog state")
		}
	})
}

func TestCreateFollowupIssueSetsBacklogTeamAndProjectWithoutParent(t *testing.T) {
	f := newFollowupIssueFixture(t)
	result, err := createFollowupIssue(f.session(t), validFollowupArgs())
	if err != nil {
		t.Fatal(err)
	}
	data := result.Data.(map[string]any)
	if !result.Success || data["identifier"] != "PMR-101" || data["state"] != "Backlog" || data["originating_issue"] != "PMR-41" {
		t.Fatalf("result=%+v", result)
	}
	if data["relationship"] != "" || len(f.created) != 1 || len(f.relations) != 0 {
		t.Fatalf("created=%+v relations=%+v data=%+v", f.created, f.relations, data)
	}
	description := f.created[0]["description"].(string)
	if !strings.Contains(description, "separate retry concern") || !strings.Contains(description, "## Acceptance criteria") || !strings.Contains(description, "Retry delays are bounded") {
		t.Fatalf("description=%q", description)
	}
}

func TestCreateFollowupIssueSupportsOnlyBoundedOriginRelationships(t *testing.T) {
	for _, test := range []struct{ relationship, kind string }{{"related", "related"}, {"blocked_by_current", "blocks"}} {
		t.Run(test.relationship, func(t *testing.T) {
			f := newFollowupIssueFixture(t)
			args := validFollowupArgs()
			args["relationship"] = test.relationship
			if _, err := createFollowupIssue(f.session(t), args); err != nil {
				t.Fatal(err)
			}
			if len(f.relations) != 1 || f.relations[0].Origin != "active" || f.relations[0].Followup != "followup-id-1" || f.relations[0].Kind != test.kind {
				t.Fatalf("relations=%+v", f.relations)
			}
		})
	}
}

func TestCreateFollowupIssueRejectsInvalidAndUnsupportedInput(t *testing.T) {
	f := newFollowupIssueFixture(t)
	session := f.session(t)
	for name, arguments := range map[string]string{
		"missing fields":          `{"title":"ok"}`,
		"blank title":             `{"title":" ","description":"d","acceptance_criteria":"a"}`,
		"blank description":       `{"title":"t","description":" ","acceptance_criteria":"a"}`,
		"blank criteria":          `{"title":"t","description":"d","acceptance_criteria":" "}`,
		"arbitrary project":       `{"title":"t","description":"d","acceptance_criteria":"a","project":"other"}`,
		"arbitrary initial state": `{"title":"t","description":"d","acceptance_criteria":"a","state":"Todo"}`,
		"arbitrary relation":      `{"title":"t","description":"d","acceptance_criteria":"a","relationship":"blocks_other"}`,
		"non-object body":         `[]`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := session.CreateFollowupIssue(context.Background(), json.RawMessage(arguments)); err == nil {
				t.Fatalf("accepted invalid input: %s", arguments)
			}
		})
	}
	if len(f.created) != 0 {
		t.Fatalf("created issues from invalid input: %+v", f.created)
	}
}

// TestCreateFollowupIssueRefusalsNameWhatWasWrongAndAreForwardable is the
// provider half of PMR-183: internal/capability forwards the message of a
// refusal whose category RefusesRequest accepts, so each of these sentences is
// what the model reads and has to act on. Pinning them here is what makes that
// forwarding safe to widen -- a message that starts carrying provider text
// fails this test before it reaches an agent.
func TestCreateFollowupIssueRefusalsNameWhatWasWrongAndAreForwardable(t *testing.T) {
	f := newFollowupIssueFixture(t)
	session := f.session(t)
	oversizedTitle, _ := json.Marshal(map[string]any{
		"title": strings.Repeat("t", MaxFollowupIssueTitleRunes+1), "description": "d", "acceptance_criteria": "a",
	})
	oversizedBody, _ := json.Marshal(map[string]any{
		"title": "t", "description": strings.Repeat("d", MaxFollowupIssueBodyRunes), "acceptance_criteria": "a",
	})
	for name, test := range map[string]struct{ arguments, message string }{
		"non-object arguments": {`[]`, "tool arguments must be a JSON object"},
		"unsupported field":    {`{"title":"t","description":"d","acceptance_criteria":"a","project":"other"}`, "tool arguments contain an unsupported field"},
		"wrong field type":     {`{"title":7,"description":"d","acceptance_criteria":"a"}`, "tool arguments have invalid field types"},
		"oversized title":      {string(oversizedTitle), "follow-up issue title is invalid"},
		"blank description":    {`{"title":"t","description":" ","acceptance_criteria":"a"}`, "follow-up issue description and acceptance criteria are required"},
		"oversized body":       {string(oversizedBody), "follow-up issue description is too large"},
		"invalid relationship": {`{"title":"t","description":"d","acceptance_criteria":"a","relationship":"blocks_other"}`, "follow-up issue relationship is invalid"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := session.CreateFollowupIssue(context.Background(), json.RawMessage(test.arguments))
			var refusal *Error
			if !errors.As(err, &refusal) {
				t.Fatalf("accepted invalid input %s: %v", test.arguments, err)
			}
			if !refusal.RefusesRequest() {
				t.Fatalf("category %q is not forwardable, so %q never reaches the agent", refusal.Category, refusal.Message)
			}
			if refusal.Message != test.message {
				t.Fatalf("message = %q, want %q", refusal.Message, test.message)
			}
		})
	}

	// A scope refusal is forwardable for the same reason: the agent can read
	// that the issue moved and stop retrying.
	f.stateID, f.stateName = "done", "Done"
	_, err := createFollowupIssue(session, validFollowupArgs())
	var refusal *Error
	if !errors.As(err, &refusal) || !refusal.RefusesRequest() || refusal.Message != "active issue state changed after session setup" {
		t.Fatalf("scope refusal = %v", err)
	}
	if len(f.created) != 0 {
		t.Fatalf("created issues from refused calls: %+v", f.created)
	}
}

// TestCreateFollowupIssueRoundTripFailuresAreNotForwardable is the other half:
// a failure that describes a round trip rather than the request must answer
// false, so its caller keeps a generic refusal. The read below plants a secret
// in the response body to prove that what is withheld is exactly the class of
// message a provider can influence.
func TestCreateFollowupIssueRoundTripFailuresAreNotForwardable(t *testing.T) {
	t.Run("create rejected by Linear", func(t *testing.T) {
		f := newFollowupIssueFixture(t)
		f.failCreate = true
		_, err := createFollowupIssue(f.session(t), validFollowupArgs())
		var refusal *Error
		if !errors.As(err, &refusal) || refusal.Category != "handoff_response" || refusal.RefusesRequest() {
			t.Fatalf("create failure = %v", err)
		}
	})

	t.Run("transport failure", func(t *testing.T) {
		f := newFollowupIssueFixture(t)
		session := f.session(t)
		f.failRead, f.failReadBody = true, "wire-secret-should-never-reach-the-agent"
		_, err := createFollowupIssue(session, validFollowupArgs())
		var refusal *Error
		if !errors.As(err, &refusal) || refusal.RefusesRequest() {
			t.Fatalf("transport failure = %v", err)
		}
		if strings.Contains(refusal.Message, "wire-secret") {
			t.Fatalf("message carried the response body: %q", refusal.Message)
		}
	})
}

// TestCreateFollowupIssueBoundsTheBodyInTheUnitTheSchemaAdvertises covers the
// units half of PMR-183: the advertised per-field bounds are code points, so a
// description that fills them with non-ASCII text -- which used to exceed a
// byte bound the agent was never told about -- is created, not refused.
func TestCreateFollowupIssueBoundsTheBodyInTheUnitTheSchemaAdvertises(t *testing.T) {
	f := newFollowupIssueFixture(t)
	args := validFollowupArgs()
	// The advertised maxLength of each field, in the unit maxLength counts,
	// filled with runes that are two and three bytes wide.
	args["description"] = strings.Repeat("æ", 16000)
	args["acceptance_criteria"] = strings.Repeat("→", 4000)
	if _, err := createFollowupIssue(f.session(t), args); err != nil {
		t.Fatalf("a schema-valid non-ASCII body was refused: %v", err)
	}
	if len(f.created) != 1 {
		t.Fatalf("created=%d", len(f.created))
	}
	if body := f.created[0]["description"].(string); len([]rune(body)) > MaxFollowupIssueBodyRunes || len([]byte(body)) <= MaxFollowupIssueBodyRunes {
		t.Fatalf("body is %d runes / %d bytes, which does not exercise the unit difference", len([]rune(body)), len([]byte(body)))
	}
}

func TestCreateFollowupIssueRequiresOriginStillInScope(t *testing.T) {
	f := newFollowupIssueFixture(t)
	session := f.session(t)
	f.stateID, f.stateName = "done", "Done"
	if _, err := createFollowupIssue(session, validFollowupArgs()); err == nil {
		t.Fatal("follow-up issue creation succeeded after the active issue left scope")
	}
	f.stateID, f.stateName = "in-progress", "In Progress"
	f.project, f.projectID = "other-project", "other-project-id"
	if _, err := createFollowupIssue(session, validFollowupArgs()); err == nil {
		t.Fatal("follow-up issue creation succeeded after the active issue project changed")
	}
	if len(f.created) != 0 {
		t.Fatalf("created=%+v", f.created)
	}
}

func TestCreateFollowupIssueRejectsProviderScopeMismatchAndFailures(t *testing.T) {
	t.Run("created parent", func(t *testing.T) {
		f := newFollowupIssueFixture(t)
		f.createdParentID = "active"
		if _, err := createFollowupIssue(f.session(t), validFollowupArgs()); err == nil {
			t.Fatal("accepted a parented follow-up")
		}
	})
	t.Run("created active state", func(t *testing.T) {
		f := newFollowupIssueFixture(t)
		f.createdStateName = "Todo"
		if _, err := createFollowupIssue(f.session(t), validFollowupArgs()); err == nil {
			t.Fatal("accepted a non-Backlog follow-up")
		}
	})
	t.Run("create failure", func(t *testing.T) {
		f := newFollowupIssueFixture(t)
		f.failCreate = true
		if _, err := createFollowupIssue(f.session(t), validFollowupArgs()); err == nil {
			t.Fatal("create failure was not surfaced")
		}
	})
	t.Run("relation failure", func(t *testing.T) {
		f := newFollowupIssueFixture(t)
		f.failRelation = true
		args := validFollowupArgs()
		args["relationship"] = "related"
		if _, err := createFollowupIssue(f.session(t), args); err == nil {
			t.Fatal("relation failure was not surfaced")
		}
		if len(f.created) != 1 || len(f.relations) != 0 {
			t.Fatalf("created=%+v relations=%+v", f.created, f.relations)
		}
	})
}

func TestCreateFollowupIssueLogsOnlySafeIdentifiers(t *testing.T) {
	f := newFollowupIssueFixture(t)
	var logs bytes.Buffer
	handoff := NewHandoff(f.settings)
	handoff.logger = slog.New(slog.NewTextHandler(&logs, nil))
	session, err := handoff.Prepare(context.Background(), domain.Issue{ID: "active"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := createFollowupIssue(session, validFollowupArgs()); err != nil {
		t.Fatal(err)
	}
	logged := logs.String()
	if !strings.Contains(logged, "originating_issue_id=active") || !strings.Contains(logged, "originating_issue_identifier=PMR-41") ||
		!strings.Contains(logged, "followup_issue_id=followup-id-1") || !strings.Contains(logged, "followup_issue_identifier=PMR-101") {
		t.Fatalf("missing safe context: %s", logged)
	}
	for _, secret := range []string{"retry concern", "Retry delays", "test-token", "private agent payload"} {
		if strings.Contains(logged, secret) {
			t.Fatalf("log leaked %q: %s", secret, logged)
		}
	}
}
