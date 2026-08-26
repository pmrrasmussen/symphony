package linear

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

func TestLoadedWorkflowPreservesCaseSensitiveStateFilter(t *testing.T) {
	var stateNames []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request := decodeRequest(t, r)
		stateNames = stringSlice(request["variables"].(map[string]any)["stateNames"])
		writeJSON(t, w, issuePage([]any{issue("one", "PMR-1", "First", "Todo", "", nil, nil)}, false, ""))
	}))
	defer server.Close()

	path := filepath.Join(t.TempDir(), "WORKFLOW.md")
	content := "---\ntracker:\n  kind: linear\n  provider: {api_key: test-token, project_slug_id: project-1, endpoint: " + server.URL + "}\n  active_states: [Todo, In Progress]\n  terminal_states: [Done]\n---\nprompt"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	workflow, err := config.Load(path, "")
	if err != nil {
		t.Fatal(err)
	}
	issues, err := New(func() config.Settings { return workflow.Config }).ListCandidates(context.Background(), workflow.Config.Tracker.ActiveStates)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := stateNames, []string{"Todo", "In Progress"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("state filter=%v want %v", got, want)
	}
	if len(issues) != 1 || issues[0].State != "Todo" {
		t.Fatalf("issues=%#v", issues)
	}
}

func TestListCandidatesFreezesSettingsAcrossPages(t *testing.T) {
	var requests int
	var settingsCalls int
	var settings config.Settings
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		request := decodeRequest(t, r)
		variables := request["variables"].(map[string]any)
		if got := r.Header.Get("Authorization"); got != "first-token" {
			t.Errorf("authorization=%q", got)
		}
		if got := variables["projectSlug"]; got != "first-project" {
			t.Errorf("projectSlug=%v", got)
		}
		if got, want := stringSlice(variables["stateNames"]), []string{"Todo"}; !reflect.DeepEqual(got, want) {
			t.Errorf("stateNames=%v want %v", got, want)
		}
		if requests == 1 {
			settings.Tracker.Provider = map[string]any{"api_key": "second-token", "project_slug_id": "second-project", "endpoint": "http://" + r.Host}
			settings.Tracker.ActiveStates = []string{"In Progress"}
			settings.Tracker.TerminalStates = []string{"Canceled"}
			writeJSON(t, w, issuePage(nil, true, "next"))
			return
		}
		writeJSON(t, w, issuePage([]any{issue("one", "PMR-1", "First", "Todo", "owner", nil, []relation{{Type: "blocks", ID: "done", Identifier: "PMR-0", State: "Done"}})}, false, ""))
	}))
	defer server.Close()
	settings = config.Settings{Tracker: config.Tracker{
		Provider:       map[string]any{"api_key": "first-token", "project_slug_id": "first-project", "endpoint": server.URL, "assignee": "owner"},
		ActiveStates:   []string{"Todo"},
		TerminalStates: []string{"Done"},
	}}

	issues, err := New(func() config.Settings {
		settingsCalls++
		return settings
	}).ListCandidates(context.Background(), []string{"Todo"})
	if err != nil {
		t.Fatal(err)
	}
	if settingsCalls != 1 || requests != 2 || len(issues) != 1 || !issues[0].Dispatchable {
		t.Fatalf("settings calls=%d requests=%d issues=%#v", settingsCalls, requests, issues)
	}
}

func TestOversizedPageRetriesSmallerAndMalformedPollRecovers(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		variables := decodeRequest(t, r)["variables"].(map[string]any)
		switch requests {
		case 1:
			if got := variables["first"]; got != float64(pageSize) {
				t.Errorf("first=%v", got)
			}
			_, _ = w.Write(bytes.Repeat([]byte("x"), maxResponseSize+1))
		case 2:
			if got := variables["first"]; got != float64(pageSize/2) {
				t.Errorf("reduced first=%v", got)
			}
			writeJSON(t, w, issuePage([]any{issue("one", "PMR-1", "First", "Todo", "", nil, nil)}, false, ""))
		case 3:
			if got := variables["first"]; got != float64(pageSize) {
				t.Errorf("new poll first=%v", got)
			}
			_, _ = w.Write([]byte(`{"data":`))
		default:
			writeJSON(t, w, issuePage([]any{issue("one", "PMR-1", "First", "Todo", "", nil, nil)}, false, ""))
		}
	}))
	defer server.Close()
	tracker := newTestTracker(server.URL, "")

	issues, err := tracker.ListCandidates(context.Background(), []string{"Todo"})
	if err != nil || len(issues) != 1 {
		t.Fatalf("oversized recovery issues=%#v err=%v", issues, err)
	}
	issues, err = tracker.ListCandidates(context.Background(), []string{"Todo"})
	if issues != nil {
		t.Fatalf("malformed poll returned partial issues=%#v", issues)
	}
	assertCategory(t, err, "tracker_response")
	issues, err = tracker.ListCandidates(context.Background(), []string{"Todo"})
	if err != nil || len(issues) != 1 {
		t.Fatalf("recovery issues=%#v err=%v", issues, err)
	}
}

// ListTerminal is the query startup workspace cleanup runs before scheduling
// begins: it asks the same project-scoped list for the configured terminal
// states, so a Done issue is returned rather than filtered out as undispatchable.
func TestListTerminalReturnsTerminalProjectIssues(t *testing.T) {
	var variables map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request := decodeRequest(t, r)
		variables = request["variables"].(map[string]any)
		writeJSON(t, w, issuePage([]any{
			issue("one", "PMR-1", "Landed", "Done", "", nil, nil),
			issue("two", "PMR-2", "Dropped", "Canceled", "", nil, nil),
		}, false, ""))
	}))
	defer server.Close()

	issues, err := newTestTracker(server.URL, "").ListTerminal(context.Background(), []string{"Done", "Canceled"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := stringSlice(variables["stateNames"]), []string{"Done", "Canceled"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("state filter=%v want %v", got, want)
	}
	if got, want := variables["projectSlug"], "project-1"; got != want {
		t.Fatalf("projectSlug=%v want %v", got, want)
	}
	if len(issues) != 2 || issues[0].Identifier != "PMR-1" || issues[1].State != "Canceled" {
		t.Fatalf("issues=%#v", issues)
	}
}

func TestRateLimitUsesLatestRedactedReset(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "10")
		w.Header().Set("X-RateLimit-Requests-Reset", strconv.FormatInt(now.Add(90*time.Second).UnixMilli(), 10))
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"errors":[{"message":"test-token and private payload"}]}`))
	}))
	defer server.Close()
	tracker := newTestTracker(server.URL, "")
	tracker.now = func() time.Time { return now }

	_, err := tracker.ListCandidates(context.Background(), []string{"Todo"})
	var trackerErr *Error
	if !errors.As(err, &trackerErr) || trackerErr.Category != "tracker_rate_limited" || trackerErr.RetryDelay() != 90*time.Second {
		t.Fatalf("error=%#v", err)
	}
	if strings.Contains(err.Error(), "test-token") || strings.Contains(err.Error(), "private payload") {
		t.Fatalf("rate-limit error leaked response: %v", err)
	}
}

func TestTransitionMovesIssueToStartedStateAndIsIdempotent(t *testing.T) {
	// state is the issue's current Linear state; the handler flips it to the
	// target after a successful mutation so a second call observes the
	// idempotent no-op path.
	state := "Todo"
	var transitions int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request := decodeRequest(t, r)
		query := request["query"].(string)
		variables := request["variables"].(map[string]any)
		switch {
		case strings.Contains(query, "SymphonyLinearHandoffIssue"):
			writeJSON(t, w, map[string]any{"data": map[string]any{"issue": map[string]any{
				"id": "active", "identifier": "PMR-5", "title": "Start", "project": map[string]string{"slugId": "project-1"},
				"team": map[string]string{"id": "team-1"}, "state": map[string]string{"id": strings.ToLower(strings.ReplaceAll(state, " ", "-")), "name": state},
			}}})
		case strings.Contains(query, "SymphonyLinearHandoffStates"):
			writeJSON(t, w, map[string]any{"data": map[string]any{"team": map[string]any{"id": "team-1", "states": map[string]any{"nodes": []any{
				map[string]string{"id": "todo", "name": "Todo"}, map[string]string{"id": "in-progress", "name": "In Progress"},
			}}}}})
		case strings.Contains(query, "SymphonyLinearHandoffTransition"):
			transitions++
			if got := variables["stateID"]; got != "in-progress" {
				t.Errorf("transition stateID=%v, want in-progress", got)
			}
			state = "In Progress"
			writeJSON(t, w, map[string]any{"data": map[string]any{"issueUpdate": map[string]bool{"success": true}}})
		default:
			t.Errorf("unexpected query: %s", query)
		}
	}))
	defer server.Close()
	settings := config.Settings{Tracker: config.Tracker{
		Provider:     map[string]any{"api_key": "test-token", "project_slug_id": "project-1", "endpoint": server.URL},
		ActiveStates: []string{"Todo", "In Progress"}, TerminalStates: []string{"Done", "Canceled"},
	}}
	tracker := New(func() config.Settings { return settings })

	if err := tracker.Transition(context.Background(), domain.Issue{ID: "active", State: "Todo"}, "In Progress"); err != nil {
		t.Fatalf("first transition: %v", err)
	}
	if transitions != 1 {
		t.Fatalf("transitions=%d after first move, want 1", transitions)
	}
	// The issue is now In Progress; a second call must be an idempotent no-op
	// that issues no mutation (the restart / turn-limit re-dispatch case).
	if err := tracker.Transition(context.Background(), domain.Issue{ID: "active", State: "Todo"}, "In Progress"); err != nil {
		t.Fatalf("idempotent transition: %v", err)
	}
	if transitions != 1 {
		t.Fatalf("transitions=%d after idempotent call, want still 1", transitions)
	}
}

func TestTransitionLogsHostSideEdgeAndSkip(t *testing.T) {
	state := "Todo"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request := decodeRequest(t, r)
		query := request["query"].(string)
		switch {
		case strings.Contains(query, "SymphonyLinearHandoffIssue"):
			writeJSON(t, w, map[string]any{"data": map[string]any{"issue": map[string]any{
				"id": "active", "identifier": "PMR-5", "title": "Start", "project": map[string]string{"slugId": "project-1"},
				"team": map[string]string{"id": "team-1"}, "state": map[string]string{"id": strings.ToLower(strings.ReplaceAll(state, " ", "-")), "name": state},
			}}})
		case strings.Contains(query, "SymphonyLinearHandoffStates"):
			writeJSON(t, w, map[string]any{"data": map[string]any{"team": map[string]any{"id": "team-1", "states": map[string]any{"nodes": []any{
				map[string]string{"id": "todo", "name": "Todo"}, map[string]string{"id": "in-progress", "name": "In Progress"},
			}}}}})
		case strings.Contains(query, "SymphonyLinearHandoffTransition"):
			state = "In Progress"
			writeJSON(t, w, map[string]any{"data": map[string]any{"issueUpdate": map[string]bool{"success": true}}})
		default:
			t.Errorf("unexpected query: %s", query)
		}
	}))
	defer server.Close()
	settings := config.Settings{Tracker: config.Tracker{
		Provider:     map[string]any{"api_key": "test-token", "project_slug_id": "project-1", "endpoint": server.URL},
		ActiveStates: []string{"Todo", "In Progress"}, TerminalStates: []string{"Done"},
	}}
	tracker := New(func() config.Settings { return settings })
	var logs bytes.Buffer
	tracker.SetLogger(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))

	if err := tracker.Transition(context.Background(), domain.Issue{ID: "active", State: "Todo"}, "In Progress"); err != nil {
		t.Fatalf("transition: %v", err)
	}
	edge := findLogRecord(t, &logs, "Linear transition")
	if edge["operation"] != "transition" || edge["from_state"] != "Todo" || edge["to_state"] != "In Progress" || edge["issue_identifier"] != "PMR-5" {
		t.Fatalf("edge record missing operation/from/to/issue: %v", edge)
	}

	// The issue is now In Progress; an idempotent call must record a skip at
	// debug level, still carrying the operation and issue for reconstruction.
	if err := tracker.Transition(context.Background(), domain.Issue{ID: "active", State: "Todo"}, "In Progress"); err != nil {
		t.Fatalf("idempotent transition: %v", err)
	}
	skip := findLogRecord(t, &logs, "Linear transition skipped")
	if skip["operation"] != "transition" || skip["to_state"] != "In Progress" || skip["issue_identifier"] != "PMR-5" {
		t.Fatalf("skip record missing operation/to/issue: %v", skip)
	}
}

// findLogRecord returns the first JSON log line in buf whose msg equals want.
func findLogRecord(t *testing.T, buf *bytes.Buffer, want string) map[string]any {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("decode log line %q: %v", line, err)
		}
		if record["msg"] == want {
			return record
		}
	}
	t.Fatalf("no %q log record in: %s", want, buf.String())
	return nil
}

func TestTransitionRejectsIssueOutsideConfiguredProject(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]any{"data": map[string]any{"issue": map[string]any{
			"id": "active", "identifier": "PMR-5", "title": "Start", "project": map[string]string{"slugId": "wrong"},
			"team": map[string]string{"id": "team-1"}, "state": map[string]string{"id": "todo", "name": "Todo"},
		}}})
	}))
	defer server.Close()
	settings := config.Settings{Tracker: config.Tracker{
		Provider:     map[string]any{"api_key": "test-token", "project_slug_id": "project-1", "endpoint": server.URL},
		ActiveStates: []string{"Todo", "In Progress"}, TerminalStates: []string{"Done"},
	}}
	err := New(func() config.Settings { return settings }).Transition(context.Background(), domain.Issue{ID: "active", State: "Todo"}, "In Progress")
	assertCategory(t, err, "transition_scope")
}

func TestHandoffRejectsHumanTerminalChangeBeforeMutation(t *testing.T) {
	var reads, mutations int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request := decodeRequest(t, r)
		query := request["query"].(string)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(query, "SymphonyLinearHandoffIssue"):
			reads++
			state := "Todo"
			if reads > 1 {
				state = "Done" // a human completed it after session setup
			}
			writeJSON(t, w, map[string]any{"data": map[string]any{"issue": map[string]any{
				"id": "active", "identifier": "PMR-5", "title": "Handoff", "project": map[string]string{"slugId": "project-1"}, "team": map[string]string{"id": "team-1"}, "state": map[string]string{"id": strings.ToLower(strings.ReplaceAll(state, " ", "-")), "name": state},
			}}})
		case strings.Contains(query, "SymphonyLinearHandoffStates"):
			writeJSON(t, w, map[string]any{"data": map[string]any{"team": map[string]any{"id": "team-1", "states": map[string]any{"nodes": []any{map[string]string{"id": "review", "name": "In Review"}}}}}})
		default:
			mutations++
			writeJSON(t, w, map[string]any{"data": map[string]any{"issueUpdate": map[string]bool{"success": true}}})
		}
	}))
	defer server.Close()
	settings := config.Settings{Tracker: config.Tracker{Provider: map[string]any{"api_key": "test-token", "project_slug_id": "project-1", "endpoint": server.URL}, ActiveStates: []string{"todo"}, TerminalStates: []string{"done"}, HandoffState: "In Review"}}
	session, err := NewHandoff(func() config.Settings { return settings }).Prepare(context.Background(), domain.Issue{ID: "active"})
	if err != nil {
		t.Fatal(err)
	}
	session.handoffMu.Lock()
	err = session.handoffLocked(context.Background())
	session.handoffMu.Unlock()
	if err == nil {
		t.Fatal("handoff succeeded after a human completed the issue")
	}
	if mutations != 0 {
		t.Fatalf("mutations=%d want 0", mutations)
	}
}

func TestHandoffRejectsIssueOutsideConfiguredProject(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]any{"data": map[string]any{"issue": map[string]any{
			"id": "active", "identifier": "PMR-5", "title": "Handoff", "project": map[string]string{"slugId": "wrong"}, "team": map[string]string{"id": "team-1"}, "state": map[string]string{"id": "todo", "name": "Todo"},
		}}})
	}))
	defer server.Close()
	settings := config.Settings{Tracker: config.Tracker{Provider: map[string]any{"api_key": "test-token", "project_slug_id": "project-1", "endpoint": server.URL}, HandoffState: "In Review"}}
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

	tracker := newTestTracker(server.URL, "me")
	issues, err := tracker.ListCandidates(context.Background(), []string{"Todo"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tracker.ListCandidates(context.Background(), []string{"Todo"}); err != nil {
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

func TestAssigneeMeInvalidatesViewerForTrackerAndPolicyChanges(t *testing.T) {
	var viewerCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request := decodeRequest(t, r)
		if strings.Contains(request["query"].(string), "SymphonyLinearViewer") {
			viewerCalls++
			writeJSON(t, w, map[string]any{"data": map[string]any{"viewer": map[string]any{"id": r.Header.Get("Authorization") + "-viewer"}}})
			return
		}
		writeJSON(t, w, issuePage([]any{issue("one", "PMR-1", "Mine", "Todo", r.Header.Get("Authorization")+"-viewer", nil, nil)}, false, ""))
	}))
	defer server.Close()

	settings := config.Settings{Tracker: config.Tracker{
		Provider:       map[string]any{"api_key": "first-token", "project_slug_id": "first-project", "endpoint": server.URL, "assignee": "me", "api_key_file": "first-key-file"},
		ActiveStates:   []string{"Todo"},
		TerminalStates: []string{"Done"},
	}}
	tracker := New(func() config.Settings { return settings })
	assertPoll := func(wantDispatchable bool) {
		t.Helper()
		issues, err := tracker.ListCandidates(context.Background(), []string{"Todo"})
		if err != nil || len(issues) != 1 || issues[0].Dispatchable != wantDispatchable {
			t.Fatalf("issues=%#v err=%v want dispatchable=%v", issues, err, wantDispatchable)
		}
	}

	assertPoll(true)
	settings.Tracker.Provider = map[string]any{"api_key": "second-token", "project_slug_id": "second-project", "endpoint": server.URL, "assignee": "me", "api_key_file": "second-key-file"}
	assertPoll(true)
	if viewerCalls != 2 {
		t.Fatalf("viewer calls after tracker change=%d want 2", viewerCalls)
	}

	settings.Tracker.Provider["assignee"] = "fixed-viewer"
	assertPoll(false)
	delete(settings.Tracker.Provider, "assignee")
	assertPoll(true)
	if viewerCalls != 2 {
		t.Fatalf("fixed and absent assignee unexpectedly resolved viewer: %d", viewerCalls)
	}

	settings.Tracker.Provider["assignee"] = "me"
	assertPoll(true)
	if viewerCalls != 3 {
		t.Fatalf("viewer calls after policy restoration=%d want 3", viewerCalls)
	}

	settings.Tracker.Provider["api_key"] = ""
	if issues, err := tracker.ListCandidates(context.Background(), []string{"Todo"}); issues != nil {
		t.Fatalf("invalid tracker configuration returned issues: %#v", issues)
	} else {
		assertCategory(t, err, "missing_tracker_secret")
	}
	settings.Tracker.Provider["api_key"] = "second-token"
	assertPoll(true)
	if viewerCalls != 4 {
		t.Fatalf("viewer calls after invalid configuration recovery=%d want 4", viewerCalls)
	}
}

func TestAssigneeMeViewerFailureIsNotCached(t *testing.T) {
	var viewerCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request := decodeRequest(t, r)
		if strings.Contains(request["query"].(string), "SymphonyLinearViewer") {
			viewerCalls++
			viewerID := ""
			if viewerCalls > 1 {
				viewerID = "viewer-id"
			}
			writeJSON(t, w, map[string]any{"data": map[string]any{"viewer": map[string]any{"id": viewerID}}})
			return
		}
		writeJSON(t, w, issuePage(nil, false, ""))
	}))
	defer server.Close()

	tracker := newTestTracker(server.URL, "me")
	if issues, err := tracker.ListCandidates(context.Background(), []string{"Todo"}); issues != nil {
		t.Fatalf("failed viewer lookup returned issues: %#v", issues)
	} else {
		assertCategory(t, err, "tracker_response")
	}
	if _, err := tracker.ListCandidates(context.Background(), []string{"Todo"}); err != nil {
		t.Fatal(err)
	}
	if _, err := tracker.ListCandidates(context.Background(), []string{"Todo"}); err != nil {
		t.Fatal(err)
	}
	if viewerCalls != 2 {
		t.Fatalf("viewer calls=%d want failed lookup plus one cached success", viewerCalls)
	}
}

func TestAssigneeMeConcurrentPollsShareViewerResolution(t *testing.T) {
	const polls = 8
	var mu sync.Mutex
	viewerCalls := 0
	viewerStarted := make(chan struct{})
	releaseViewer := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request := decodeRequest(t, r)
		if strings.Contains(request["query"].(string), "SymphonyLinearViewer") {
			mu.Lock()
			viewerCalls++
			if viewerCalls == 1 {
				close(viewerStarted)
			}
			mu.Unlock()
			<-releaseViewer
			writeJSON(t, w, map[string]any{"data": map[string]any{"viewer": map[string]any{"id": "viewer-id"}}})
			return
		}
		writeJSON(t, w, issuePage(nil, false, ""))
	}))
	defer server.Close()

	tracker := newTestTracker(server.URL, "me")
	start := make(chan struct{})
	errs := make(chan error, polls)
	var workers sync.WaitGroup
	for range polls {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			_, err := tracker.ListCandidates(context.Background(), []string{"Todo"})
			errs <- err
		}()
	}
	close(start)
	<-viewerStarted
	close(releaseViewer)
	workers.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if viewerCalls != 1 {
		t.Fatalf("viewer calls=%d want 1", viewerCalls)
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
	provider := map[string]any{"api_key": "test-token", "project_slug_id": "project-1", "endpoint": endpoint}
	if assignee != "" {
		provider["assignee"] = assignee
	}
	settings := config.Settings{Tracker: config.Tracker{Provider: provider, ActiveStates: []string{"Todo"}, TerminalStates: []string{"Done", "Canceled"}}}
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
