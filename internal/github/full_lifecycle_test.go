package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/coordinator"
	"github.com/pmrrasmussen/symphony/internal/domain"
	"github.com/pmrrasmussen/symphony/internal/linear"
)

// This file exercises the PMR-38 canonical lifecycle end to end --
// Todo -> In Progress -> In Review -> Rework -> In Review -> Merging -> Done
// -- against a fake Linear GraphQL server and the fake GitHub REST/GraphQL
// server already used by this package's other tests (apiFixture, fakeGit).
// It drives the real coordinator, the real linear.Tracker/linear.Handoff, and
// the real github.Manager; only the Codex app-server JSON-RPC protocol itself
// is replaced, by a fake domain.AgentBackend that invokes the same session
// capabilities (github_publish_pr and github_land_pr) a live Codex session
// would call, while the host owns the Todo -> In Progress start transition.

// lifecycleLinearStates are the canonical lifecycle's fixed team states. Real
// IDs do not matter, only that they are stable and distinct.
var lifecycleLinearStates = map[string]string{
	"Todo": "state-todo", "In Progress": "state-in-progress", "In Review": "state-in-review",
	"Rework": "state-rework", "Merging": "state-merging", "Done": "state-done", "Canceled": "state-canceled",
}

// lifecycleLinearFixture is a fake Linear GraphQL server serving exactly the
// fixed queries/mutations internal/linear issues: the candidate/refresh
// polling queries and the handoff read/states/transition/comment operations.
// It holds one mutable issue plus its accumulated comments.
type lifecycleLinearFixture struct {
	t      *testing.T
	server *httptest.Server

	mu                                sync.Mutex
	issueID, identifier, title, descr string
	url, projectID, projectSlugID     string
	teamID, stateName                 string
	comments                          []string
}

func newLifecycleLinearFixture(t *testing.T) *lifecycleLinearFixture {
	f := &lifecycleLinearFixture{
		issueID: "issue-100", identifier: "PMR-27", title: "Full lifecycle", descr: "Exercise the canonical lifecycle",
		url: "https://linear.app/issue/PMR-27", projectID: "project-id-1", projectSlugID: "project-1",
		teamID: "team-1", stateName: "Todo",
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}

func (f *lifecycleLinearFixture) settings() map[string]any {
	return map[string]any{"api_key": "test-token", "project_slug_id": f.projectSlugID, "endpoint": f.server.URL}
}

func (f *lifecycleLinearFixture) setState(name string) {
	if _, ok := lifecycleLinearStates[name]; !ok {
		f.t.Fatalf("unknown lifecycle state %q", name)
	}
	f.mu.Lock()
	f.stateName = name
	f.mu.Unlock()
}

func (f *lifecycleLinearFixture) issueNode() map[string]any {
	return map[string]any{
		"id": f.issueID, "identifier": f.identifier, "title": f.title, "description": f.descr,
		"priority": 2, "state": map[string]string{"name": f.stateName},
		"branchName": "", "url": f.url,
		"labels":           map[string]any{"nodes": []any{}},
		"inverseRelations": map[string]any{"nodes": []any{}, "pageInfo": map[string]any{"hasNextPage": false}},
	}
}

func (f *lifecycleLinearFixture) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var body struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		f.t.Fatalf("decode linear request: %v", err)
	}
	w.Header().Set("Content-Type", "application/json")
	q := body.Query
	switch {
	case strings.Contains(q, "SymphonyLinearPoll") || strings.Contains(q, "SymphonyLinearIssuesByID"):
		f.writeIssuesPage(w)
	case strings.Contains(q, "SymphonyLinearHandoffIssue"):
		f.writeHandoffIssue(w)
	case strings.Contains(q, "SymphonyLinearHandoffStates"):
		f.writeStates(w, body.Variables)
	case strings.Contains(q, "SymphonyLinearHandoffTransition"):
		f.applyTransition(w, body.Variables)
	case strings.Contains(q, "SymphonyLinearHandoffComments"):
		f.writeComments(w)
	case strings.Contains(q, "SymphonyLinearHandoffComment"):
		f.addComment(w, body.Variables)
	default:
		f.t.Fatalf("unexpected linear query: %s", q)
	}
}

func (f *lifecycleLinearFixture) writeIssuesPage(w http.ResponseWriter) {
	_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"issues": map[string]any{
		"nodes":    []any{f.issueNode()},
		"pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""},
	}}})
}

func (f *lifecycleLinearFixture) writeHandoffIssue(w http.ResponseWriter) {
	_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"issue": map[string]any{
		"id": f.issueID, "identifier": f.identifier, "title": f.title, "description": f.descr, "url": f.url,
		"project": map[string]any{"id": f.projectID, "slugId": f.projectSlugID},
		"team":    map[string]any{"id": f.teamID},
		"state":   map[string]any{"id": lifecycleLinearStates[f.stateName], "name": f.stateName},
	}}})
}

func (f *lifecycleLinearFixture) writeStates(w http.ResponseWriter, variables map[string]any) {
	teamID, _ := variables["teamID"].(string)
	if teamID == "" {
		teamID = f.teamID
	}
	nodes := make([]map[string]any, 0, len(lifecycleLinearStates))
	for name, id := range lifecycleLinearStates {
		nodes = append(nodes, map[string]any{"id": id, "name": name})
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"team": map[string]any{
		"id": teamID, "states": map[string]any{"nodes": nodes},
	}}})
}

func (f *lifecycleLinearFixture) applyTransition(w http.ResponseWriter, variables map[string]any) {
	stateID, _ := variables["stateID"].(string)
	success := false
	for name, id := range lifecycleLinearStates {
		if id == stateID {
			f.stateName = name
			success = true
			break
		}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"issueUpdate": map[string]any{"success": success}}})
}

func (f *lifecycleLinearFixture) addComment(w http.ResponseWriter, variables map[string]any) {
	body, _ := variables["body"].(string)
	f.comments = append(f.comments, body)
	_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"commentCreate": map[string]any{"success": true}}})
}

func (f *lifecycleLinearFixture) writeComments(w http.ResponseWriter) {
	nodes := make([]map[string]any, 0, len(f.comments))
	for _, c := range f.comments {
		nodes = append(nodes, map[string]any{"body": c})
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"issue": map[string]any{
		"id": f.issueID, "project": map[string]any{"slugId": f.projectSlugID}, "team": map[string]any{"id": f.teamID},
		"comments": map[string]any{"nodes": nodes, "pageInfo": map[string]any{"hasNextPage": false, "endCursor": nil}},
	}}})
}

// lifecycleWorkspace is a minimal domain.WorkspaceExecutor: this test cares
// only about coordinator dispatch and the real Linear/GitHub adapters, not
// about real Git worktrees.
type lifecycleWorkspace struct {
	afterRun chan struct{}
}

func (w *lifecycleWorkspace) Prepare(context.Context, domain.Issue) (domain.Workspace, error) {
	return domain.Workspace{Path: "/fake/workspace"}, nil
}
func (w *lifecycleWorkspace) BeforeRun(context.Context, domain.Workspace, domain.Issue) error {
	return nil
}
func (w *lifecycleWorkspace) AfterRun(context.Context, domain.Workspace, domain.Issue) {
	w.afterRun <- struct{}{}
}
func (w *lifecycleWorkspace) Cleanup(context.Context, domain.Issue) (domain.CleanupOutcome, error) {
	return domain.CleanupClean, nil
}
func (w *lifecycleWorkspace) Execute(context.Context, domain.Workspace, string, []string) ([]byte, error) {
	return nil, errors.New("unexpected execute")
}

// lifecycleAgentBackend stands in for a live Codex session: for the issue's
// current state, it invokes exactly the session capability WORKFLOW.md's
// prompt instructs a real Codex session to call (github_publish_pr or
// github_land_pr), using the real handoff/GitHub sessions built the same way
// internal/codex/backend.go builds them. The Todo -> In Progress start
// transition is host-owned (the coordinator), so it is applied here with the
// host Linear tracker, not by the agent.
type lifecycleAgentBackend struct {
	settings       func() config.Settings
	handoffFactory *linear.Handoff
	githubManager  *Manager
	tracker        *linear.Tracker

	mu           sync.Mutex
	sessionIssue map[string]string
	publishCalls int
	landCalls    int
	lastErr      error
}

func (a *lifecycleAgentBackend) actOn(ctx context.Context, issue domain.Issue) error {
	settings := a.settings()
	handoffSession, err := a.handoffFactory.PrepareWithSettings(ctx, settings, issue)
	if err != nil {
		return fmt.Errorf("prepare handoff: %w", err)
	}
	if handoffSession == nil {
		return errors.New("handoff session unavailable")
	}
	switch strings.ToLower(strings.TrimSpace(issue.State)) {
	case "todo":
		// Host-owned dispatch-time start transition (the coordinator's job), not
		// an agent capability: move it with the host Linear tracker credential.
		if err := a.tracker.Transition(ctx, issue, "In Progress"); err != nil {
			return fmt.Errorf("transition to In Progress: %w", err)
		}
	case "in progress", "rework":
		githubSession := a.githubManager.PrepareWithSettings(settings.GitHub, issue, "/fake/workspace", handoffSession)
		if githubSession == nil {
			return errors.New("github session unavailable")
		}
		if _, err := githubSession.Publish(ctx, PublishInput{Why: "Fix the issue", WhatChanged: "Implemented and validated the change", OnCall: "none"}); err != nil {
			return fmt.Errorf("publish pull request: %w", err)
		}
		a.mu.Lock()
		a.publishCalls++
		a.mu.Unlock()
	case "merging":
		githubSession := a.githubManager.PrepareWithSettings(settings.GitHub, issue, "/fake/workspace", handoffSession)
		if githubSession == nil {
			return errors.New("github session unavailable")
		}
		result, err := githubSession.Land(ctx)
		if err != nil {
			return fmt.Errorf("land pull request: %w", err)
		}
		a.mu.Lock()
		a.landCalls++
		a.mu.Unlock()
		if result.Status != LandMerged {
			return fmt.Errorf("unexpected land status %q", result.Status)
		}
	default:
		// In Review, Done, and Canceled are never dispatched; nothing to do.
	}
	return nil
}

func (a *lifecycleAgentBackend) recordErr(err error) {
	a.mu.Lock()
	a.lastErr = err
	a.mu.Unlock()
}

func (a *lifecycleAgentBackend) eventFor(err error) domain.Event {
	if err != nil {
		return domain.Event{Kind: domain.EventBlocked, Message: err.Error()}
	}
	return domain.Event{Kind: domain.EventCompleted}
}

func (a *lifecycleAgentBackend) Start(ctx context.Context, r domain.AgentRequest) (domain.AgentSession, <-chan domain.Event, error) {
	err := a.actOn(ctx, r.Issue)
	a.recordErr(err)
	sessionID := "session-" + r.Issue.ID
	a.mu.Lock()
	a.sessionIssue[sessionID] = r.Issue.ID
	a.mu.Unlock()
	events := make(chan domain.Event, 1)
	events <- a.eventFor(err)
	close(events)
	return domain.AgentSession{ID: sessionID, ThreadID: "thread-" + r.Issue.ID, TurnID: "1"}, events, nil
}

func (a *lifecycleAgentBackend) Continue(ctx context.Context, s domain.AgentSession, prompt string) (<-chan domain.Event, error) {
	a.mu.Lock()
	issueID := a.sessionIssue[s.ID]
	a.mu.Unlock()
	fresh, err := a.tracker.GetIssues(ctx, []string{issueID})
	if err != nil {
		return nil, fmt.Errorf("continue: refresh issue: %w", err)
	}
	if len(fresh) != 1 {
		return nil, errors.New("continue: issue no longer found")
	}
	actErr := a.actOn(ctx, fresh[0])
	a.recordErr(actErr)
	events := make(chan domain.Event, 1)
	events <- a.eventFor(actErr)
	close(events)
	return events, nil
}

func (a *lifecycleAgentBackend) Cancel(context.Context, domain.AgentSession) error { return nil }

func TestFullCanonicalLifecycleAgainstFakeLinearAndGitHubServers(t *testing.T) {
	linearFixture := newLifecycleLinearFixture(t)
	provider := linearFixture.settings()

	api := newAPI(t)
	api.prSHA = "head" // matches fakeGit's fixed HEAD, so no republish push is exercised.
	passingRequiredChecks(api, "ci")
	readyToLand(api)

	githubSettings := api.settings()
	githubSettings.MergeState = "Merging"
	githubSettings.MergeMethod = "merge"
	githubSettings.RequiredChecks = []string{"ci"}

	settings := config.Settings{
		Tracker: config.Tracker{
			Provider:        provider,
			ActiveStates:    []string{"Todo", "In Progress", "Rework", "Merging"},
			TerminalStates:  []string{"Done", "Canceled"},
			HandoffState:    "In Review",
			HostTransitions: config.HostTransitions{Start: map[string]string{"Todo": "In Progress"}, RefuseLanding: map[string]string{"Merging": "In Review"}},
		},
		Polling:   config.Polling{Interval: time.Hour},
		Workspace: config.Workspace{Root: "/fake"},
		Agent:     config.Agent{MaxConcurrent: 1, MaxTurns: 8, MaxRetryBackoff: time.Second, ByState: map[string]int{"merging": 1}},
		Codex:     config.Codex{Command: "test", TurnTimeout: time.Second, ReadTimeout: time.Second},
		GitHub:    githubSettings,
		Prompt:    "Work on {{.issue.identifier}}",
	}
	settingsFunc := func() config.Settings { return settings }

	tracker := linear.New(settingsFunc)
	handoffFactory := linear.NewHandoff(settingsFunc)
	manager := New(settingsFunc, slog.New(slog.NewTextHandler(io.Discard, nil)))
	manager.git = &fakeGit{}

	backend := &lifecycleAgentBackend{settings: settingsFunc, handoffFactory: handoffFactory, githubManager: manager, tracker: tracker, sessionIssue: map[string]string{}}
	ws := &lifecycleWorkspace{afterRun: make(chan struct{}, 1)}

	c := coordinator.New(tracker, backend, ws, settingsFunc, nil)
	ctx := context.Background()

	assertState := func(t *testing.T, want string) {
		t.Helper()
		issues, err := tracker.GetIssues(ctx, []string{linearFixture.issueID})
		if err != nil || len(issues) != 1 {
			t.Fatalf("refresh issue: issues=%v err=%v", issues, err)
		}
		if issues[0].State != want {
			t.Fatalf("issue state=%q, want %q", issues[0].State, want)
		}
	}

	// Todo -> In Progress -> In Review: the agent starts implementation, then
	// publishes a structured pull request and hands off to review.
	c.Tick(ctx)
	<-ws.afterRun
	backend.mu.Lock()
	lastErr := backend.lastErr
	backend.mu.Unlock()
	if lastErr != nil {
		t.Fatalf("Todo phase failed: %v", lastErr)
	}
	assertState(t, "In Review")

	// A human moves the issue back to Rework: the agent resumes in the same
	// worktree/branch/pull request and republishes to hand it back to review.
	linearFixture.setState("Rework")
	c.Tick(ctx)
	<-ws.afterRun
	backend.mu.Lock()
	lastErr = backend.lastErr
	backend.mu.Unlock()
	if lastErr != nil {
		t.Fatalf("Rework phase failed: %v", lastErr)
	}
	assertState(t, "In Review")

	// A human moves the issue to Merging: moving it there is itself the
	// approval to land, and a successful merge reconciles the issue to Done.
	linearFixture.setState("Merging")
	c.Tick(ctx)
	<-ws.afterRun
	backend.mu.Lock()
	lastErr = backend.lastErr
	backend.mu.Unlock()
	if lastErr != nil {
		t.Fatalf("Merging phase failed: %v", lastErr)
	}
	assertState(t, "Done")

	if err := c.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}

	backend.mu.Lock()
	publishCalls, landCalls := backend.publishCalls, backend.landCalls
	backend.mu.Unlock()
	if publishCalls != 2 {
		t.Fatalf("publishCalls=%d, want 2 (initial handoff and Rework republish)", publishCalls)
	}
	if landCalls != 1 {
		t.Fatalf("landCalls=%d, want 1", landCalls)
	}
	api.mu.Lock()
	merges, created := api.merges, api.created
	api.mu.Unlock()
	if created != 1 {
		t.Fatalf("created=%d pull requests, want exactly one deterministic pull request reused throughout", created)
	}
	if merges != 1 {
		t.Fatalf("merges=%d, want exactly one merge call", merges)
	}
}
