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

type childIssueFixture struct {
	t         *testing.T
	mu        sync.Mutex
	server    *httptest.Server
	project   string
	projectID string
	team      string
	stateID   string
	stateName string
	labels    map[string]string // lowercase name -> id
	nextSeq   int
	created   []childIssueRef
	blocks    []struct{ Blocker, Blocked string }
	failCreate,
	failBlock bool
}

func newChildIssueFixture(t *testing.T) *childIssueFixture {
	t.Helper()
	f := &childIssueFixture{
		t: t, project: "project-1", projectID: "project-id-1", team: "team-1",
		stateID: "in-progress", stateName: "In Progress",
		labels: map[string]string{"bug": "label-bug", "chore": "label-chore"},
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.serveHTTP))
	t.Cleanup(f.server.Close)
	return f
}

func (f *childIssueFixture) settings() config.Settings {
	return config.Settings{Tracker: config.Tracker{
		Provider:           map[string]any{"api_key": "test-token", "project_slug_id": "project-1", "endpoint": f.server.URL},
		ActiveStates:       []string{"Todo", "In Progress"},
		TerminalStates:     []string{"Done"},
		ChildIssueCreation: true,
	}}
}

func (f *childIssueFixture) session(t *testing.T) *HandoffSession {
	return f.sessionWithSettings(t, f.settings())
}

func (f *childIssueFixture) sessionWithSettings(t *testing.T, settings config.Settings) *HandoffSession {
	t.Helper()
	session, err := NewHandoff(func() config.Settings { return settings }).Prepare(context.Background(), domain.Issue{ID: "active"})
	if err != nil {
		t.Fatal(err)
	}
	return session
}

func (f *childIssueFixture) serveHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	request := decodeRequest(f.t, r)
	query := request["query"].(string)
	variables, _ := request["variables"].(map[string]any)
	switch {
	case strings.Contains(query, "SymphonyLinearHandoffIssue"):
		project := map[string]string{"id": f.projectID, "slugId": f.project}
		writeJSON(f.t, w, map[string]any{"data": map[string]any{"issue": map[string]any{
			"id": "active", "identifier": "PMR-41", "title": "Decompose task", "description": "private agent payload", "url": "https://linear.app/issue/PMR-41",
			"project": project, "team": map[string]string{"id": f.team}, "state": map[string]string{"id": f.stateID, "name": f.stateName},
		}}})
	case strings.Contains(query, "SymphonyLinearChildIssueLabels"):
		nodes := make([]any, 0, len(f.labels))
		for name, id := range f.labels {
			nodes = append(nodes, map[string]string{"id": id, "name": name})
		}
		writeJSON(f.t, w, map[string]any{"data": map[string]any{"team": map[string]any{"id": f.team, "labels": map[string]any{
			"nodes": nodes, "pageInfo": map[string]any{"hasNextPage": false, "endCursor": nil},
		}}}})
	case strings.Contains(query, "SymphonyLinearCreateChildIssueBlock"):
		if got := variables["issueID"]; got == nil || strings.TrimSpace(got.(string)) == "" {
			f.t.Errorf("block issueID=%v", got)
		}
		if f.failBlock {
			f.failBlock = false
			writeJSON(f.t, w, map[string]any{"data": map[string]any{"issueRelationCreate": map[string]bool{"success": false}}})
			return
		}
		f.blocks = append(f.blocks, struct{ Blocker, Blocked string }{variables["issueID"].(string), variables["relatedIssueID"].(string)})
		writeJSON(f.t, w, map[string]any{"data": map[string]any{"issueRelationCreate": map[string]bool{"success": true}}})
	case strings.Contains(query, "SymphonyLinearCreateChildIssue"):
		if got := variables["teamID"]; got != f.team {
			f.t.Errorf("create teamID=%v", got)
		}
		if got := variables["projectID"]; got != f.projectID {
			f.t.Errorf("create projectID=%v", got)
		}
		if got := variables["parentID"]; got != "active" {
			f.t.Errorf("create parentID=%v", got)
		}
		if f.failCreate {
			f.failCreate = false
			writeJSON(f.t, w, map[string]any{"data": map[string]any{"issueCreate": map[string]any{"success": false}}})
			return
		}
		f.nextSeq++
		ref := childIssueRef{ID: "child-id-" + itoa(f.nextSeq), Identifier: "PMR-41-" + itoa(f.nextSeq), URL: "https://linear.app/issue/child-" + itoa(f.nextSeq)}
		f.created = append(f.created, ref)
		writeJSON(f.t, w, map[string]any{"data": map[string]any{"issueCreate": map[string]any{"success": true, "issue": map[string]string{
			"id": ref.ID, "identifier": ref.Identifier, "url": ref.URL,
		}}}})
	default:
		f.t.Fatalf("unexpected GraphQL operation: %s", query)
	}
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

func createChildIssue(session *HandoffSession, args map[string]any) (ToolResult, error) {
	body, err := json.Marshal(args)
	if err != nil {
		return ToolResult{}, err
	}
	return session.CreateChildIssue(context.Background(), body)
}

func TestCreateChildIssueDisabledUnlessConfigured(t *testing.T) {
	f := newChildIssueFixture(t)
	settings := f.settings()
	settings.Tracker.ChildIssueCreation = false
	// Keep the session bound via a different capability so Prepare still
	// returns a session; create_child_issue must still be independently
	// gated by ChildIssueCreation.
	settings.Tracker.AgentTransitions = map[string]string{"In Progress": "Merging"}
	session := f.sessionWithSettings(t, settings)
	if _, err := createChildIssue(session, map[string]any{"title": "Split work"}); err == nil {
		t.Fatal("child issue creation succeeded while disabled")
	}
	if len(f.created) != 0 {
		t.Fatalf("created issues while disabled: %+v", f.created)
	}
}

func TestCreateChildIssueUnconfiguredMeansNoSession(t *testing.T) {
	f := newChildIssueFixture(t)
	settings := f.settings()
	settings.Tracker.ChildIssueCreation = false
	session, err := NewHandoff(func() config.Settings { return settings }).Prepare(context.Background(), domain.Issue{ID: "active"})
	if err != nil {
		t.Fatal(err)
	}
	if session != nil {
		t.Fatal("no Linear session should be prepared when no capability is configured")
	}
}

func TestCreateChildIssueRequiresResolvableProject(t *testing.T) {
	f := newChildIssueFixture(t)
	f.projectID = ""
	settings := f.settings()
	if _, err := NewHandoff(func() config.Settings { return settings }).Prepare(context.Background(), domain.Issue{ID: "active"}); err == nil {
		t.Fatal("session prepared with an unresolved project")
	}
}

func TestCreateChildIssueSetsParentTeamAndProjectAndReturnsIdentifier(t *testing.T) {
	f := newChildIssueFixture(t)
	session := f.session(t)
	priority := 2
	result, err := createChildIssue(session, map[string]any{
		"title": "Extract client PR", "description": "Split off the client changes", "priority": priority,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Success {
		t.Fatalf("result=%+v", result)
	}
	data, ok := result.Data.(map[string]any)
	if !ok {
		t.Fatalf("data=%#v", result.Data)
	}
	if data["identifier"] != "PMR-41-1" || data["parent_issue"] != "PMR-41" || data["id"] != "child-id-1" {
		t.Fatalf("data=%+v", data)
	}
	if len(f.created) != 1 {
		t.Fatalf("created=%+v", f.created)
	}
}

func TestCreateChildIssueResolvesConfiguredLabelsAndRejectsUnknownLabel(t *testing.T) {
	f := newChildIssueFixture(t)
	session := f.session(t)
	if _, err := createChildIssue(session, map[string]any{"title": "Bug fix", "labels": []string{"Bug"}}); err != nil {
		t.Fatal(err)
	}
	if len(f.created) != 1 {
		t.Fatalf("created=%+v", f.created)
	}
	if _, err := createChildIssue(session, map[string]any{"title": "Bad label", "labels": []string{"NotConfigured"}}); err == nil {
		t.Fatal("accepted an unconfigured label")
	}
	if len(f.created) != 1 {
		t.Fatalf("issue created despite invalid label: %+v", f.created)
	}
}

func TestCreateChildIssueDependsOnOnlyAllowsSessionCreatedChildren(t *testing.T) {
	f := newChildIssueFixture(t)
	session := f.session(t)
	first, err := createChildIssue(session, map[string]any{"title": "Foundation change"})
	if err != nil {
		t.Fatal(err)
	}
	firstData := first.Data.(map[string]any)

	if _, err := createChildIssue(session, map[string]any{"title": "Depends on unknown", "depends_on": []string{"PMR-999"}}); err == nil {
		t.Fatal("accepted a dependency outside this session")
	}
	if len(f.created) != 1 {
		t.Fatalf("issue created despite invalid dependency: %+v", f.created)
	}

	second, err := createChildIssue(session, map[string]any{"title": "Depends on first", "depends_on": []string{firstData["identifier"].(string)}})
	if err != nil {
		t.Fatal(err)
	}
	secondData := second.Data.(map[string]any)
	if len(f.blocks) != 1 || f.blocks[0].Blocker != firstData["id"] || f.blocks[0].Blocked != secondData["id"] {
		t.Fatalf("blocks=%+v", f.blocks)
	}

	// A dependency may also be referenced by the created issue's ID, not only
	// its human identifier.
	third, err := createChildIssue(session, map[string]any{"title": "Depends on second by ID", "depends_on": []string{secondData["id"].(string)}})
	if err != nil {
		t.Fatal(err)
	}
	thirdData := third.Data.(map[string]any)
	if len(f.blocks) != 2 || f.blocks[1].Blocker != secondData["id"] || f.blocks[1].Blocked != thirdData["id"] {
		t.Fatalf("blocks=%+v", f.blocks)
	}
}

func TestCreateChildIssueRejectsInvalidAndUnsupportedInput(t *testing.T) {
	f := newChildIssueFixture(t)
	session := f.session(t)
	for name, arguments := range map[string]string{
		"missing title":     `{}`,
		"blank title":       `{"title":"   "}`,
		"unsupported field": `{"title":"ok","issue":"other"}`,
		"priority too low":  `{"title":"ok","priority":-1}`,
		"priority too high": `{"title":"ok","priority":5}`,
		"too many labels":   `{"title":"ok","labels":["a","b","c","d","e","f","g","h","i","j","k","l","m","n","o","p","q","r","s","t","u"]}`,
		"empty label":       `{"title":"ok","labels":[""]}`,
		"empty dependency":  `{"title":"ok","depends_on":[""]}`,
		"non-object body":   `[]`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := session.CreateChildIssue(context.Background(), json.RawMessage(arguments)); err == nil {
				t.Fatalf("accepted invalid input: %s", arguments)
			}
		})
	}
	if len(f.created) != 0 {
		t.Fatalf("created issues from invalid input: %+v", f.created)
	}
}

func TestCreateChildIssueRequiresActiveIssueStillInScope(t *testing.T) {
	f := newChildIssueFixture(t)
	session := f.session(t)
	f.stateID, f.stateName = "done", "Done"
	if _, err := createChildIssue(session, map[string]any{"title": "Too late"}); err == nil {
		t.Fatal("child issue creation succeeded after the active issue left scope")
	}
	if len(f.created) != 0 {
		t.Fatalf("created=%+v", f.created)
	}
}

func TestCreateChildIssueRejectsProjectChangeAfterSessionSetup(t *testing.T) {
	f := newChildIssueFixture(t)
	session := f.session(t)
	f.project, f.projectID = "other-project", "other-project-id"
	if _, err := createChildIssue(session, map[string]any{"title": "Project moved"}); err == nil {
		t.Fatal("child issue creation succeeded after the active issue project changed")
	}
	if len(f.created) != 0 {
		t.Fatalf("created=%+v", f.created)
	}
}

func TestCreateChildIssuePropagatesCreateAndBlockFailures(t *testing.T) {
	t.Run("create failure", func(t *testing.T) {
		f := newChildIssueFixture(t)
		f.failCreate = true
		session := f.session(t)
		if _, err := createChildIssue(session, map[string]any{"title": "Will fail"}); err == nil {
			t.Fatal("create failure was not surfaced")
		}
		if len(f.created) != 0 {
			t.Fatalf("created=%+v", f.created)
		}
	})
	t.Run("block failure", func(t *testing.T) {
		f := newChildIssueFixture(t)
		session := f.session(t)
		first, err := createChildIssue(session, map[string]any{"title": "Foundation"})
		if err != nil {
			t.Fatal(err)
		}
		firstIdentifier := first.Data.(map[string]any)["identifier"].(string)
		f.failBlock = true
		if _, err := createChildIssue(session, map[string]any{"title": "Depends on foundation", "depends_on": []string{firstIdentifier}}); err == nil {
			t.Fatal("relation failure was not surfaced")
		}
		// The issue itself is created before the relation call; only the
		// relation mutation fails.
		if len(f.created) != 2 {
			t.Fatalf("created=%+v", f.created)
		}
		if len(f.blocks) != 0 {
			t.Fatalf("blocks=%+v", f.blocks)
		}
	})
}

func TestCreateChildIssueLogsOnlySafeIdentifiers(t *testing.T) {
	f := newChildIssueFixture(t)
	var logs bytes.Buffer
	handoff := NewHandoff(f.settings)
	handoff.logger = slog.New(slog.NewTextHandler(&logs, nil))
	session, err := handoff.Prepare(context.Background(), domain.Issue{ID: "active"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := createChildIssue(session, map[string]any{"title": "Secret plan", "description": "confidential details"}); err != nil {
		t.Fatal(err)
	}
	logged := logs.String()
	if !strings.Contains(logged, "parent_issue_id=active") || !strings.Contains(logged, "parent_issue_identifier=PMR-41") {
		t.Fatalf("missing safe parent context: %s", logged)
	}
	if !strings.Contains(logged, "child_issue_id=child-id-1") || !strings.Contains(logged, "child_issue_identifier=PMR-41-1") {
		t.Fatalf("missing safe child context: %s", logged)
	}
	for _, secret := range []string{"Secret plan", "confidential details", "test-token", "private agent payload"} {
		if strings.Contains(logged, secret) {
			t.Fatalf("log leaked %q: %s", secret, logged)
		}
	}
}
