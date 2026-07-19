package github

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
	"time"

	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/domain"
)

type fakeGit struct {
	mu       sync.Mutex
	dirty    bool
	noChange bool
	calls    [][]string
	envs     [][]string
}

func (g *fakeGit) Run(_ context.Context, _ string, args, env []string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, append([]string(nil), args...))
	g.envs = append(g.envs, append([]string(nil), env...))
	switch args[0] {
	case "remote":
		return "https://github.com/owner/repo.git", nil
	case "status":
		if g.dirty {
			return " M file.go", nil
		}
	case "rev-parse":
		if args[1] == "HEAD" {
			return "head", nil
		}
		if g.noChange {
			return "head", nil
		}
		return "base", nil
	case "merge-base", "push":
		return "", nil
	}
	return "", nil
}

type fakeLinear struct {
	mu        sync.Mutex
	activeErr error
	links     []string
	completed int
}

func (l *fakeLinear) EnsureActive(context.Context) error { return l.activeErr }
func (l *fakeLinear) LinkAndHandoff(_ context.Context, url string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, existing := range l.links {
		if existing == url {
			return nil
		}
	}
	l.links = append(l.links, url)
	return nil
}
func (l *fakeLinear) Complete(context.Context) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.completed > 0 {
		return false, nil
	}
	l.completed++
	return true, nil
}

// apiFixture is a fake GitHub REST/GraphQL server. Every table used by
// github_pr_context (checks, reviews, comments, review threads) is
// independently configurable so tests can exercise bounds and redaction
// without a live GitHub dependency.
type apiFixture struct {
	t      *testing.T
	server *httptest.Server
	mu     sync.Mutex

	auth []string

	// Pull request state. prExists lets a test seed a pull request without
	// going through Publish, so mismatched/closed/merged reuse can be tested
	// in isolation.
	prExists      bool
	prNumber      int
	prState       string
	prMerged      bool
	prBody        string
	prHeadRef     string
	prBaseRef     string
	prSHA         string
	created       int
	patches       []map[string]any
	multiplePulls bool

	statuses     []map[string]any
	overall      string
	checkRuns    []map[string]any
	reviews      []map[string]any
	comments     []map[string]any
	threads      []map[string]any
	threadsTotal int
	graphqlErr   bool
}

func newAPI(t *testing.T) *apiFixture {
	f := &apiFixture{t: t, prNumber: 7, prState: "open", prHeadRef: "symphony/pmr-27", prBaseRef: "main", prSHA: "sha1"}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}

func (f *apiFixture) settings() config.GitHub {
	return config.GitHub{Enabled: true, Owner: "owner", Repository: "repo", BaseBranch: "main", Token: "private-token", Endpoint: f.server.URL, PollInterval: time.Millisecond}
}

func (f *apiFixture) pullJSON() map[string]any {
	return map[string]any{
		"number": f.prNumber, "html_url": "https://github.com/owner/repo/pull/7",
		"state": f.prState, "merged": f.prMerged, "body": f.prBody,
		"head": map[string]any{"ref": f.prHeadRef, "sha": f.prSHA},
		"base": map[string]any{"ref": f.prBaseRef},
	}
}

func (f *apiFixture) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/pulls":
		if r.URL.Query().Get("head") != "owner:symphony/pmr-27" || r.URL.Query().Get("base") != "main" {
			f.t.Errorf("query=%s", r.URL.RawQuery)
		}
		if !f.prExists {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		list := []map[string]any{f.pullJSON()}
		if f.multiplePulls {
			list = append(list, f.pullJSON())
		}
		encoded, _ := json.Marshal(list)
		_, _ = w.Write(encoded)
	case r.Method == http.MethodPost && r.URL.Path == "/repos/owner/repo/pulls":
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil || body["head"] != "symphony/pmr-27" || body["base"] != "main" {
			f.t.Errorf("create body=%v", body)
		}
		f.created++
		f.prExists = true
		f.prState = "open"
		f.prMerged = false
		f.prBody, _ = body["body"].(string)
		encoded, _ := json.Marshal(f.pullJSON())
		_, _ = w.Write(encoded)
	case r.Method == http.MethodPatch && r.URL.Path == "/repos/owner/repo/pulls/7":
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			f.t.Errorf("patch body decode failed")
		}
		f.patches = append(f.patches, body)
		if state, ok := body["state"].(string); ok {
			f.prState = state
		}
		if text, ok := body["body"].(string); ok {
			f.prBody = text
		}
		encoded, _ := json.Marshal(f.pullJSON())
		_, _ = w.Write(encoded)
	case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/pulls/7":
		encoded, _ := json.Marshal(f.pullJSON())
		_, _ = w.Write(encoded)
	case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/commits/sha1/status":
		encoded, _ := json.Marshal(map[string]any{"state": f.overall, "statuses": f.statuses})
		_, _ = w.Write(encoded)
	case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/commits/sha1/check-runs":
		encoded, _ := json.Marshal(map[string]any{"check_runs": f.checkRuns})
		_, _ = w.Write(encoded)
	case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/pulls/7/reviews":
		encoded, _ := json.Marshal(f.reviews)
		_, _ = w.Write(encoded)
	case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/issues/7/comments":
		encoded, _ := json.Marshal(f.comments)
		_, _ = w.Write(encoded)
	case r.Method == http.MethodPost && r.URL.Path == "/graphql":
		if f.graphqlErr {
			encoded, _ := json.Marshal(map[string]any{"errors": []map[string]any{{"message": "boom"}}})
			_, _ = w.Write(encoded)
			return
		}
		total := f.threadsTotal
		if total == 0 {
			total = len(f.threads)
		}
		encoded, _ := json.Marshal(map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{"reviewThreads": map[string]any{"totalCount": total, "nodes": f.threads}}}}})
		_, _ = w.Write(encoded)
	default:
		f.t.Errorf("unexpected GitHub request %s %s", r.Method, r.URL.String())
		w.WriteHeader(http.StatusNotFound)
	}
}

func testSession(t *testing.T, api *apiFixture, git *fakeGit, linear *fakeLinear, log *bytes.Buffer) (*Manager, *Session) {
	t.Helper()
	settings := api.settings()
	logger := slog.Default()
	if log != nil {
		logger = slog.New(slog.NewJSONHandler(log, nil))
	}
	m := New(func() config.Settings { return config.Settings{GitHub: settings} }, logger)
	m.git = git
	s := &Session{manager: m, settings: settings, issue: domain.Issue{ID: "issue-27", Identifier: "PMR-27", Title: "Lifecycle", URL: "https://linear.app/issue/PMR-27"}, workspace: t.TempDir(), branch: "symphony/pmr-27", linear: linear}
	return m, s
}

func testInput() PublishInput {
	return PublishInput{Why: "Fix a bug", WhatChanged: "Adjusted the handler", OnCall: "no rotation"}
}

func TestPublishCreatesThenReusesDeterministicPullRequest(t *testing.T) {
	api, git, linear := newAPI(t), &fakeGit{}, &fakeLinear{}
	m, session := testSession(t, api, git, linear, nil)
	first, err := session.Publish(context.Background(), testInput())
	if err != nil {
		t.Fatal(err)
	}
	if !first.BodyUpdated {
		t.Fatal("initial publish must report the body as created")
	}
	second, err := session.Publish(context.Background(), testInput())
	if err != nil {
		t.Fatal(err)
	}
	if second.BodyUpdated {
		t.Fatal("repeat publication with unchanged fields must not report a body update")
	}
	if first.Branch != second.Branch || first.URL != second.URL || first.Number != second.Number || first.Branch != "symphony/pmr-27" || api.created != 1 || len(linear.links) != 1 {
		t.Fatalf("first=%+v second=%+v created=%d links=%v", first, second, api.created, linear.links)
	}
	wantBody := "## Why\nFix a bug\n\n## What changed\nAdjusted the handler\n\n## On Call\nno rotation\n\nLinear: https://linear.app/issue/PMR-27\n"
	if api.prBody != wantBody {
		t.Fatalf("canonical body=%q", api.prBody)
	}
	if len(api.patches) != 0 {
		t.Fatalf("repeat publication with unchanged fields issued an update: %v", api.patches)
	}
	if len(m.linked) != 1 {
		t.Fatalf("tracked=%d", len(m.linked))
	}
	for _, auth := range api.auth {
		if auth != "Bearer private-token" {
			t.Fatalf("auth=%q", auth)
		}
	}
	git.mu.Lock()
	defer git.mu.Unlock()
	foundPush := false
	for index, call := range git.calls {
		if call[0] == "push" {
			foundPush = strings.Join(call, " ") == "push https://github.com/owner/repo.git HEAD:refs/heads/symphony/pmr-27"
			if strings.Contains(strings.Join(call, " "), "private-token") || !strings.Contains(strings.Join(git.envs[index], " "), "AUTHORIZATION: basic") {
				t.Fatal("push did not isolate credential to host environment")
			}
		}
	}
	if !foundPush {
		t.Fatal("deterministic branch was not pushed")
	}
}

func TestPublishUpdatesBodyWhenStructuredFieldsChange(t *testing.T) {
	api, git, linear := newAPI(t), &fakeGit{}, &fakeLinear{}
	_, session := testSession(t, api, git, linear, nil)
	if _, err := session.Publish(context.Background(), testInput()); err != nil {
		t.Fatal(err)
	}
	changed := testInput()
	changed.WhatChanged = "Adjusted the handler and added a regression test"
	result, err := session.Publish(context.Background(), changed)
	if err != nil {
		t.Fatal(err)
	}
	if !result.BodyUpdated {
		t.Fatal("changed structured fields must update the pull request body")
	}
	if api.created != 1 {
		t.Fatalf("changed fields created a second pull request: created=%d", api.created)
	}
	if len(api.patches) != 1 || api.patches[0]["body"] == nil {
		t.Fatalf("expected exactly one body update patch: %v", api.patches)
	}
	if !strings.Contains(api.prBody, "regression test") {
		t.Fatalf("updated body=%q", api.prBody)
	}
}

func TestPublishRejectsDirtyNoChangeAndStaleIssueBeforeMutation(t *testing.T) {
	for _, test := range []struct {
		name      string
		git       *fakeGit
		activeErr error
	}{
		{name: "dirty", git: &fakeGit{dirty: true}},
		{name: "no changes", git: &fakeGit{noChange: true}},
		{name: "stale issue", git: &fakeGit{}, activeErr: errors.New("stale")},
	} {
		t.Run(test.name, func(t *testing.T) {
			api, linear := newAPI(t), &fakeLinear{activeErr: test.activeErr}
			_, session := testSession(t, api, test.git, linear, nil)
			if _, err := session.Publish(context.Background(), testInput()); err == nil {
				t.Fatal("unsafe publish succeeded")
			}
			if api.created != 0 || len(linear.links) != 0 {
				t.Fatalf("created=%d links=%v", api.created, linear.links)
			}
		})
	}
}

func TestPublishRejectsCrossRepositoryOriginBeforeAnyMutation(t *testing.T) {
	api, linear := newAPI(t), &fakeLinear{}
	git := &fakeGit{}
	_, session := testSession(t, api, git, linear, nil)
	session.manager.git = originGit{fakeGit: git, origin: "git@github.com:someone/other.git"}
	if _, err := session.Publish(context.Background(), testInput()); err == nil || !strings.Contains(err.Error(), "origin") {
		t.Fatalf("cross-repository publish error=%v", err)
	}
	if api.created != 0 || len(api.auth) != 0 || len(linear.links) != 0 || linear.completed != 0 {
		t.Fatalf("GitHub/Linear mutation occurred: created=%d requests=%d links=%v completed=%d", api.created, len(api.auth), linear.links, linear.completed)
	}
	git.mu.Lock()
	defer git.mu.Unlock()
	for _, call := range git.calls {
		if call[0] == "push" {
			t.Fatalf("cross-repository worktree was pushed: %v", call)
		}
	}
}

type originGit struct {
	*fakeGit
	origin string
}

func (g originGit) Run(ctx context.Context, dir string, args, env []string) (string, error) {
	if args[0] == "remote" {
		g.fakeGit.mu.Lock()
		g.fakeGit.calls = append(g.fakeGit.calls, append([]string(nil), args...))
		g.fakeGit.envs = append(g.fakeGit.envs, append([]string(nil), env...))
		g.fakeGit.mu.Unlock()
		return g.origin, nil
	}
	return g.fakeGit.Run(ctx, dir, args, env)
}

func TestRepositoryOriginAcceptsOnlyCanonicalCredentialFreeGitHubForms(t *testing.T) {
	for _, remote := range []string{
		"https://github.com/owner/repo.git",
		"https://github.com/OWNER/REPO",
		"git@github.com:owner/repo.git",
		"ssh://git@github.com/owner/repo.git",
	} {
		if !matchesRepository(remote, "owner", "repo") {
			t.Errorf("canonical remote rejected: %q", remote)
		}
	}
	for _, remote := range []string{
		"https://token@github.com/owner/repo.git",
		"https://github.com/owner/other.git",
		"git@github.com:other/repo.git",
		"ssh://user@github.com/owner/repo.git",
		"git://github.com/owner/repo.git",
		"https://example.com/owner/repo.git",
		"https://github.com/owner/repo.git?token=secret",
		"https://github.com/owner%2Frepo.git",
	} {
		if matchesRepository(remote, "owner", "repo") {
			t.Errorf("unsafe remote accepted: %q", remote)
		}
	}
}

func TestPublishRejectsMergedPullRequestAsIrrecoverable(t *testing.T) {
	api, git, linear := newAPI(t), &fakeGit{}, &fakeLinear{}
	api.prExists, api.prState, api.prMerged, api.prBody = true, "closed", true, "old body"
	_, session := testSession(t, api, git, linear, nil)
	if _, err := session.Publish(context.Background(), testInput()); err == nil || !strings.Contains(err.Error(), "merged") {
		t.Fatalf("merged pull request reuse error=%v", err)
	}
	if api.created != 0 || len(api.patches) != 0 || len(linear.links) != 0 {
		t.Fatalf("merged pull request was mutated: created=%d patches=%v links=%v", api.created, api.patches, linear.links)
	}
}

func TestPublishReopensClosedUnmergedPullRequest(t *testing.T) {
	api, git, linear := newAPI(t), &fakeGit{}, &fakeLinear{}
	api.prExists, api.prState, api.prBody = true, "closed", "old body"
	_, session := testSession(t, api, git, linear, nil)
	result, err := session.Publish(context.Background(), testInput())
	if err != nil {
		t.Fatal(err)
	}
	if !result.BodyUpdated {
		t.Fatal("reopened pull request with a new body must report an update")
	}
	if api.prState != "open" {
		t.Fatalf("pull request was not reopened: state=%s", api.prState)
	}
	foundReopen := false
	for _, patch := range api.patches {
		if patch["state"] == "open" {
			foundReopen = true
		}
	}
	if !foundReopen {
		t.Fatalf("no reopen patch recorded: %v", api.patches)
	}
	if api.created != 0 {
		t.Fatalf("reopening a closed pull request must not create a new one: created=%d", api.created)
	}
}

func TestPublishRejectsMismatchedPullRequest(t *testing.T) {
	api, git, linear := newAPI(t), &fakeGit{}, &fakeLinear{}
	api.prExists, api.prBaseRef = true, "develop"
	_, session := testSession(t, api, git, linear, nil)
	if _, err := session.Publish(context.Background(), testInput()); err == nil || !strings.Contains(err.Error(), "mismatched") {
		t.Fatalf("mismatched pull request error=%v", err)
	}
	if len(api.patches) != 0 || len(linear.links) != 0 {
		t.Fatalf("mismatched pull request was mutated: patches=%v links=%v", api.patches, linear.links)
	}
}

func TestPublishRejectsAmbiguousPullRequestList(t *testing.T) {
	api, git, linear := newAPI(t), &fakeGit{}, &fakeLinear{}
	api.prExists, api.multiplePulls = true, true
	_, session := testSession(t, api, git, linear, nil)
	if _, err := session.Publish(context.Background(), testInput()); err == nil || !strings.Contains(err.Error(), "more than one") {
		t.Fatalf("ambiguous pull request list error=%v", err)
	}
}

func TestParsePublishInputRejectsInvalidArguments(t *testing.T) {
	valid := `{"why":"a","what_changed":"b","on_call":"c"}`
	if _, err := ParsePublishInput(json.RawMessage(valid)); err != nil {
		t.Fatalf("valid input rejected: %v", err)
	}
	for name, arguments := range map[string]string{
		"not an object":      `["why"]`,
		"unsupported field":  `{"why":"a","what_changed":"b","on_call":"c","branch":"main"}`,
		"missing why":        `{"what_changed":"b","on_call":"c"}`,
		"non-string why":     `{"why":1,"what_changed":"b","on_call":"c"}`,
		"empty why":          `{"why":"  ","what_changed":"b","on_call":"c"}`,
		"empty what_changed": `{"why":"a","what_changed":"","on_call":"c"}`,
		"oversized why":      `{"why":"` + strings.Repeat("x", maxPublishWhyBytes+1) + `","what_changed":"b","on_call":"c"}`,
		"oversized on_call":  `{"why":"a","what_changed":"b","on_call":"` + strings.Repeat("x", maxPublishOnCallBytes+1) + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParsePublishInput(json.RawMessage(arguments)); err == nil {
				t.Fatalf("invalid input accepted: %s", arguments)
			}
		})
	}
}

func TestParsePublishInputAllowsBlankOnCallForHumanFillIn(t *testing.T) {
	input, err := ParsePublishInput(json.RawMessage(`{"why":"a","what_changed":"b","on_call":""}`))
	if err != nil || input.OnCall != "" {
		t.Fatalf("blank on_call rejected: input=%+v err=%v", input, err)
	}
}

func TestPollMergedCompletesOnceAndClosedUnmergedOnlyWarns(t *testing.T) {
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
	m.Poll(context.Background())
	if linear.completed != 1 {
		t.Fatalf("completions=%d", linear.completed)
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
	m2.Poll(context.Background())
	if linear2.completed != 0 || strings.Count(closedLogs.String(), "closed without merge") != 1 {
		t.Fatalf("completed=%d logs=%s", linear2.completed, closedLogs.String())
	}
	if strings.Contains(logs.String()+closedLogs.String(), "private-token") {
		t.Fatal("logs exposed credential")
	}
}

func TestContextRejectsWhenNoPullRequestExists(t *testing.T) {
	api, git, linear := newAPI(t), &fakeGit{}, &fakeLinear{}
	_, session := testSession(t, api, git, linear, nil)
	if _, err := session.Context(context.Background()); err == nil {
		t.Fatal("context succeeded with no published pull request")
	}
}

func TestContextReturnsBoundedChecksReviewsCommentsAndUnresolvedThreads(t *testing.T) {
	api, git, linear := newAPI(t), &fakeGit{}, &fakeLinear{}
	_, session := testSession(t, api, git, linear, nil)
	if _, err := session.Publish(context.Background(), testInput()); err != nil {
		t.Fatal(err)
	}

	api.mu.Lock()
	api.overall = "failure"
	api.statuses = []map[string]any{{"context": "ci/lint", "state": "success"}}
	longName := strings.Repeat("n", contextExcerptRunes+50)
	checkRuns := make([]map[string]any, 0, 25)
	for i := 0; i < 25; i++ {
		name := "check"
		if i == 0 {
			name = longName
		}
		checkRuns = append(checkRuns, map[string]any{"name": name, "status": "completed", "conclusion": "failure"})
	}
	api.checkRuns = checkRuns
	longBody := strings.Repeat("z", contextExcerptRunes+50)
	api.reviews = []map[string]any{
		{"user": map[string]any{"login": "alice"}, "state": "APPROVED", "body": "looks good", "submitted_at": "t1"},
		{"user": map[string]any{"login": "bob"}, "state": "CHANGES_REQUESTED", "body": longBody, "submitted_at": "t2"},
		{"user": map[string]any{"login": "alice"}, "state": "APPROVED", "body": "still good after fix", "submitted_at": "t3"},
	}
	comments := make([]map[string]any, 0, 25)
	for i := 0; i < 25; i++ {
		comments = append(comments, map[string]any{"user": map[string]any{"login": "carol"}, "body": "comment", "created_at": "t"})
	}
	api.comments = comments
	api.threads = []map[string]any{{"isResolved": false}, {"isResolved": true}, {"isResolved": false}}
	api.mu.Unlock()

	result, err := session.Context(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Number != 7 || result.PullRequest != "https://github.com/owner/repo/pull/7" || result.Branch != "symphony/pmr-27" {
		t.Fatalf("identity=%+v", result)
	}
	if result.Checks.OverallState != "failure" || result.Checks.Total != 26 || !result.Checks.Truncated || len(result.Checks.Runs) != contextMaxItems {
		t.Fatalf("checks=%+v", result.Checks)
	}
	if result.Checks.Runs[1].Name == longName {
		t.Fatalf("check name was not bounded: %q", result.Checks.Runs[1].Name)
	}
	// alice's latest review is the deciding one for her, but bob's later
	// CHANGES_REQUESTED must still win the effective state.
	if result.ReviewState != "changes_requested" {
		t.Fatalf("review_state=%q", result.ReviewState)
	}
	for _, review := range result.Reviews {
		if len([]rune(review.Body)) > contextExcerptRunes+len("…(truncated)") {
			t.Fatalf("review body excerpt not bounded: %q", review.Body)
		}
	}
	if result.CommentsTruncated != true || len(result.Comments) != contextMaxItems {
		t.Fatalf("comments truncated=%v count=%d", result.CommentsTruncated, len(result.Comments))
	}
	if result.UnresolvedThreads != 2 || result.ThreadsTotal != 3 {
		t.Fatalf("threads unresolved=%d total=%d", result.UnresolvedThreads, result.ThreadsTotal)
	}
}

func TestContextDoesNotMutateClosedOrMergedPullRequest(t *testing.T) {
	api, git, linear := newAPI(t), &fakeGit{}, &fakeLinear{}
	api.prExists, api.prState, api.prMerged, api.prBody = true, "closed", true, "already merged"
	_, session := testSession(t, api, git, linear, nil)
	result, err := session.Context(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.State != "closed" {
		t.Fatalf("state=%q", result.State)
	}
	if len(api.patches) != 0 {
		t.Fatalf("read-only context mutated the pull request: %v", api.patches)
	}
}

func TestContextRejectsGraphQLFailureWithoutLeakingRawPayload(t *testing.T) {
	api, git, linear := newAPI(t), &fakeGit{}, &fakeLinear{}
	_, session := testSession(t, api, git, linear, nil)
	if _, err := session.Publish(context.Background(), testInput()); err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	api.graphqlErr = true
	api.mu.Unlock()
	if _, err := session.Context(context.Background()); err == nil {
		t.Fatal("graphql failure was not surfaced as an error")
	}
}
