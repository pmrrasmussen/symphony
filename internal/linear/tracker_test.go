package linear

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/domain"
)

func TestListCandidatesPaginatesScopesAndNormalizes(t *testing.T) {
	var requests []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "test-token" {
			t.Errorf("authorization=%q", got)
		}
		request := decodeRequest(t, r)
		requests = append(requests, request)
		variables := request["variables"].(map[string]any)
		if got, want := variables["projectSlug"], "project-1"; got != want {
			t.Errorf("projectSlug=%v want %v", got, want)
		}
		if got, want := variables["first"], float64(pageSize); got != want {
			t.Errorf("first=%v want %v", got, want)
		}
		if variables["after"] == nil {
			writeJSON(t, w, issuePage([]any{
				issue("one", "PMR-1", " First ", "Todo", "owner", []string{" Feature ", "bug", "feature", " "}, []relation{{Type: "blocks", ID: "blocker", Identifier: "PMR-0", State: "In Progress"}}),
			}, true, "cursor-1"))
			return
		}
		if got, want := variables["after"], "cursor-1"; got != want {
			t.Errorf("after=%v want %v", got, want)
		}
		writeJSON(t, w, issuePage([]any{
			issue("two", "PMR-2", "Second", "Todo", "owner", nil, []relation{{Type: "blocks", ID: "done", Identifier: "PMR-3", State: "Done"}}),
		}, false, ""))
	}))
	defer server.Close()

	tracker := newTestTracker(server.URL, "")
	issues, err := tracker.ListCandidates(context.Background(), []string{"Todo"})
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 {
		t.Fatalf("requests=%d", len(requests))
	}
	if !strings.Contains(requests[0]["query"].(string), "project: {slugId") || !strings.Contains(requests[0]["query"].(string), "inverseRelations") {
		t.Fatalf("candidate query is not project-scoped with inverse relations: %s", requests[0]["query"])
	}
	if got, want := []string{issues[0].ID, issues[1].ID}, []string{"one", "two"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ids=%v want %v", got, want)
	}
	if issues[0].Title != "First" || !reflect.DeepEqual(issues[0].Labels, []string{"bug", "feature"}) || issues[0].Dispatchable {
		t.Fatalf("first issue was not normalized/blocked: %#v", issues[0])
	}
	if got := issues[0].BlockedBy; len(got) != 1 || got[0].ID != "blocker" || got[0].State != "In Progress" {
		t.Fatalf("blockers=%#v", got)
	}
	if !issues[1].Dispatchable {
		t.Fatalf("completed blocker must not prevent dispatch: %#v", issues[1])
	}
}

func TestHandoffBindsIssueProjectTeamAndFixedOperations(t *testing.T) {
	var calls []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request := decodeRequest(t, r)
		calls = append(calls, request)
		query := request["query"].(string)
		variables := request["variables"].(map[string]any)
		switch {
		case strings.Contains(query, "SymphonyLinearHandoffIssue"):
			if got := variables["issueID"]; got != "active" {
				t.Errorf("read issueID=%v", got)
			}
			writeJSON(t, w, map[string]any{"data": map[string]any{"issue": map[string]any{
				"id": "active", "identifier": "PMR-5", "title": "Handoff", "description": "safe", "url": "https://linear.app/issue/PMR-5",
				"project": map[string]string{"slugId": "project-1"}, "team": map[string]string{"id": "team-1"}, "state": map[string]string{"name": "Todo"},
			}}})
		case strings.Contains(query, "SymphonyLinearHandoffStates"):
			if got := variables["teamID"]; got != "team-1" {
				t.Errorf("teamID=%v", got)
			}
			writeJSON(t, w, map[string]any{"data": map[string]any{"team": map[string]any{"id": "team-1", "states": map[string]any{"nodes": []any{
				map[string]string{"id": "todo", "name": "Todo"}, map[string]string{"id": "review", "name": "In Review"},
			}}}}})
		case strings.Contains(query, "SymphonyLinearHandoffTransition"):
			if got := variables["issueID"]; got != "active" {
				t.Errorf("transition issueID=%v", got)
			}
			if got := variables["stateID"]; got != "review" {
				t.Errorf("transition stateID=%v", got)
			}
			writeJSON(t, w, map[string]any{"data": map[string]any{"issueUpdate": map[string]bool{"success": true}}})
		case strings.Contains(query, "SymphonyLinearHandoffComment"):
			if got := variables["issueID"]; got != "active" {
				t.Errorf("comment issueID=%v", got)
			}
			if got := variables["body"]; got != "Ready PMR-5" && got != "A bounded active issue comment" {
				t.Errorf("unexpected comment=%v", got)
			}
			writeJSON(t, w, map[string]any{"data": map[string]any{"commentCreate": map[string]bool{"success": true}}})
		default:
			t.Errorf("unexpected query: %s", query)
		}
	}))
	defer server.Close()
	settings := config.Settings{Tracker: config.Tracker{
		Provider:     map[string]any{"api_key": "test-token", "project_slug": "project-1", "endpoint": server.URL},
		ActiveStates: []string{"todo"}, TerminalStates: []string{"done"}, HandoffState: "In Review", HandoffCommentTemplate: "Ready {{.issue.identifier}}",
	}}
	handoff := NewHandoff(func() config.Settings { return settings })
	session, err := handoff.Prepare(context.Background(), domain.Issue{ID: "active"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.Call(context.Background(), json.RawMessage(`{"operation":"read"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Call(context.Background(), json.RawMessage(`{"operation":"handoff"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Call(context.Background(), json.RawMessage(`{"operation":"comment","body":"A bounded active issue comment"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Call(context.Background(), json.RawMessage(`{"operation":"handoff","issueID":"other"}`)); err == nil {
		t.Fatal("arbitrary issue argument was accepted")
	}
	if len(calls) != 5 { // prepare read+state, then transition+fixed comment+active comment
		t.Fatalf("calls=%d", len(calls))
	}
}

func TestHandoffRejectsIssueOutsideConfiguredProject(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]any{"data": map[string]any{"issue": map[string]any{
			"id": "active", "identifier": "PMR-5", "title": "Handoff", "project": map[string]string{"slugId": "wrong"}, "team": map[string]string{"id": "team-1"}, "state": map[string]string{"name": "Todo"},
		}}})
	}))
	defer server.Close()
	settings := config.Settings{Tracker: config.Tracker{Provider: map[string]any{"api_key": "test-token", "project_slug": "project-1", "endpoint": server.URL}, HandoffState: "In Review"}}
	_, err := NewHandoff(func() config.Settings { return settings }).Prepare(context.Background(), domain.Issue{ID: "active"})
	assertCategory(t, err, "handoff_scope")
}

func TestListCandidatesRejectsBrokenPaginationAtomically(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, issuePage([]any{issue("one", "PMR-1", "First", "Todo", "", nil, nil)}, true, "same-cursor"))
	}))
	defer server.Close()

	issues, err := newTestTracker(server.URL, "").ListCandidates(context.Background(), []string{"Todo"})
	if issues != nil {
		t.Fatalf("partial issues returned: %#v", issues)
	}
	assertCategory(t, err, "tracker_pagination")
}

func TestGetIssuesScopesBatchesAndPreservesRequestOrder(t *testing.T) {
	var mu sync.Mutex
	var batches [][]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request := decodeRequest(t, r)
		query := request["query"].(string)
		if !strings.Contains(query, "project: {slugId") {
			t.Errorf("refresh query is not project-scoped: %s", query)
		}
		variables := request["variables"].(map[string]any)
		ids := stringSlice(variables["ids"])
		mu.Lock()
		batches = append(batches, ids)
		mu.Unlock()
		nodes := make([]any, 0, len(ids))
		for i := len(ids) - 1; i >= 0; i-- {
			nodes = append(nodes, issue(ids[i], "PMR-"+ids[i], "Issue "+ids[i], "Todo", "", nil, nil))
		}
		writeJSON(t, w, issuePage(nodes, false, ""))
	}))
	defer server.Close()

	ids := make([]string, 0, 52)
	for i := 0; i < 51; i++ {
		ids = append(ids, "id-"+strconvItoa(i))
	}
	ids = append(ids, "id-2")
	issues, err := newTestTracker(server.URL, "").GetIssues(context.Background(), ids)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(batches), 2; got != want || len(batches[0]) != pageSize || len(batches[1]) != 1 {
		t.Fatalf("batches=%#v", batches)
	}
	if got, want := len(issues), 51; got != want {
		t.Fatalf("issues=%d want %d", got, want)
	}
	for i, issue := range issues {
		if want := "id-" + strconvItoa(i); issue.ID != want {
			t.Fatalf("issues[%d]=%q want %q", i, issue.ID, want)
		}
	}
}

func TestGetIssuesFailsForMalformedRequestedRecord(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, issuePage([]any{issue("one", "PMR-1", "", "Todo", "", nil, nil)}, false, ""))
	}))
	defer server.Close()

	issues, err := newTestTracker(server.URL, "").GetIssues(context.Background(), []string{"one"})
	if issues != nil {
		t.Fatalf("malformed refresh returned issues: %#v", issues)
	}
	assertCategory(t, err, "tracker_response")
}

func TestAssigneeMeAndFixedPolicy(t *testing.T) {
	var viewerCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request := decodeRequest(t, r)
		if strings.Contains(request["query"].(string), "SymphonyLinearViewer") {
			viewerCalls++
			writeJSON(t, w, map[string]any{"data": map[string]any{"viewer": map[string]any{"id": "viewer-id"}}})
			return
		}
		writeJSON(t, w, issuePage([]any{
			issue("mine", "PMR-1", "Mine", "Todo", "viewer-id", nil, nil),
			issue("other", "PMR-2", "Other", "Todo", "other-id", nil, nil),
		}, false, ""))
	}))
	defer server.Close()

	issues, err := newTestTracker(server.URL, "me").ListCandidates(context.Background(), []string{"Todo"})
	if err != nil {
		t.Fatal(err)
	}
	if viewerCalls != 1 || !issues[0].Dispatchable || issues[1].Dispatchable {
		t.Fatalf("viewerCalls=%d issues=%#v", viewerCalls, issues)
	}
	issues, err = newTestTracker(server.URL, "other-id").ListCandidates(context.Background(), []string{"Todo"})
	if err != nil {
		t.Fatal(err)
	}
	if issues[0].Dispatchable || !issues[1].Dispatchable {
		t.Fatalf("fixed assignee policy was not applied: %#v", issues)
	}
}

func TestTodoWithTruncatedBlockersIsNotDispatchable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		item := issue("one", "PMR-1", "First", "Todo", "", nil, nil)
		item["inverseRelations"] = map[string]any{
			"nodes":    []any{},
			"pageInfo": map[string]any{"hasNextPage": true},
		}
		writeJSON(t, w, issuePage([]any{item}, false, ""))
	}))
	defer server.Close()

	issues, err := newTestTracker(server.URL, "").ListCandidates(context.Background(), []string{"Todo"})
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 1 || issues[0].Dispatchable {
		t.Fatalf("truncated blockers must conservatively block dispatch: %#v", issues)
	}
}

func TestEndpointRequiresHTTPSOutsideLocalTestHosts(t *testing.T) {
	for _, endpoint := range []string{
		"http://example.com/graphql",
		"http://localhost.example.com/graphql",
	} {
		if err := newTestTracker(endpoint, "").Validate(); err == nil {
			t.Fatalf("endpoint %q unexpectedly validated", endpoint)
		} else {
			assertCategory(t, err, "invalid_tracker_config")
		}
	}
	for _, endpoint := range []string{
		"https://example.com/graphql",
		"http://localhost:8080/graphql",
		"http://127.0.0.1:8080/graphql",
		"http://[::1]:8080/graphql",
	} {
		if err := newTestTracker(endpoint, "").Validate(); err != nil {
			t.Fatalf("endpoint %q validation failed: %v", endpoint, err)
		}
	}
}

func TestRedirectNeverForwardsBearerTokenToPlaintext(t *testing.T) {
	var receivedAuthorization string
	plaintext := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuthorization = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer plaintext.Close()
	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plaintext.URL, http.StatusFound)
	}))
	defer secure.Close()

	tracker := newTestTracker(secure.URL, "")
	tracker.client = newHTTPClient(secure.Client().Transport)
	_, err := tracker.ListCandidates(context.Background(), []string{"Todo"})
	assertCategory(t, err, "tracker_status")
	if receivedAuthorization != "" {
		t.Fatalf("bearer token was forwarded to plaintext redirect target: %q", receivedAuthorization)
	}
}

func TestGraphQLErrorsAreClassifiedAndRedacted(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]any{"errors": []map[string]string{{"message": "test-token must never be exposed"}}})
	}))
	defer server.Close()

	_, err := newTestTracker(server.URL, "").ListCandidates(context.Background(), []string{"Todo"})
	assertCategory(t, err, "tracker_response")
	if strings.Contains(err.Error(), "test-token") {
		t.Fatalf("secret leaked in error: %v", err)
	}
}

func newTestTracker(endpoint, assignee string) *Tracker {
	provider := map[string]any{"api_key": "test-token", "project_slug": "project-1", "endpoint": endpoint}
	if assignee != "" {
		provider["assignee"] = assignee
	}
	settings := config.Settings{Tracker: config.Tracker{Provider: provider, TerminalStates: []string{"done", "canceled"}}}
	return New(func() config.Settings { return settings })
}

func decodeRequest(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	defer r.Body.Close()
	var request map[string]any
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		t.Fatal(err)
	}
	return request
}

func writeJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatal(err)
	}
}

func assertCategory(t *testing.T, err error, category string) {
	t.Helper()
	var trackerErr *Error
	if !errors.As(err, &trackerErr) {
		t.Fatalf("error=%v is not a tracker error", err)
	}
	if trackerErr.Category != category {
		t.Fatalf("error=%v category=%q want %q", err, trackerErr.Category, category)
	}
}

type relation struct{ Type, ID, Identifier, State string }

func issue(id, identifier, title, state, assignee string, labels []string, relations []relation) map[string]any {
	labelNodes := make([]map[string]string, 0, len(labels))
	for _, label := range labels {
		labelNodes = append(labelNodes, map[string]string{"name": label})
	}
	relationNodes := make([]map[string]any, 0, len(relations))
	for _, relation := range relations {
		relationNodes = append(relationNodes, map[string]any{
			"type":  relation.Type,
			"issue": map[string]any{"id": relation.ID, "identifier": relation.Identifier, "state": map[string]string{"name": relation.State}},
		})
	}
	value := map[string]any{
		"id": id, "identifier": identifier, "title": title, "description": " description ", "priority": 2,
		"state": map[string]string{"name": state}, "branchName": " branch ", "url": " https://linear.app/issue ",
		"labels": map[string]any{"nodes": labelNodes}, "inverseRelations": map[string]any{"nodes": relationNodes, "pageInfo": map[string]any{"hasNextPage": false}},
		"createdAt": "invalid-timestamp", "updatedAt": time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC).Format(time.RFC3339),
	}
	if assignee == "" {
		value["assignee"] = nil
	} else {
		value["assignee"] = map[string]string{"id": assignee}
	}
	return value
}

func issuePage(nodes []any, hasNext bool, cursor string) map[string]any {
	return map[string]any{"data": map[string]any{"issues": map[string]any{
		"nodes": nodes, "pageInfo": map[string]any{"hasNextPage": hasNext, "endCursor": cursor},
	}}}
}

func stringSlice(value any) []string {
	items, _ := value.([]any)
	out := make([]string, 0, len(items))
	for _, item := range items {
		if text, ok := item.(string); ok {
			out = append(out, text)
		}
	}
	return out
}

func strconvItoa(value int) string { return strconv.Itoa(value) }
