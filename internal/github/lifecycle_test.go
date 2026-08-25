package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
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

	// Landing (PMR-37) fake state. mergeStateErr/mergeState let a test
	// simulate a human moving the issue away from Merging. refuseErr and
	// completeErr let a test simulate the fallback transition or the Done
	// completion call itself failing independently of the substantive gate.
	mergeStateErr    error
	refuseErr        error
	refused          int
	refusedDestState string
	completeErr      error
	landCompleted    int

	// Poll reconciliation (PMR-44) fake state. reconcileErr simulates the
	// Linear completion call itself failing; reconciledState records the
	// mergeState the poll loop passed, so a test can assert fail-closed gating.
	reconcileErr    error
	reconciled      int
	reconciledState string

	// Bounded-fix (PMR-46) audit/refusal comments captured via LandComment.
	commentErr   error
	landComments []string
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

func (l *fakeLinear) ReconcileMerged(_ context.Context, mergeState string) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.reconciledState = mergeState
	if l.reconcileErr != nil {
		return false, l.reconcileErr
	}
	if l.reconciled > 0 {
		return false, nil
	}
	l.reconciled++
	return true, nil
}

func (l *fakeLinear) EnsureMergeState(context.Context, string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.mergeStateErr
}

func (l *fakeLinear) RefuseLanding(_ context.Context, mergeState string) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.refuseErr != nil {
		return false, l.refuseErr
	}
	l.refused++
	l.refusedDestState = mergeState
	return true, nil
}

func (l *fakeLinear) LandComment(_ context.Context, body string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.commentErr != nil {
		return l.commentErr
	}
	l.landComments = append(l.landComments, body)
	return nil
}

func (l *fakeLinear) CompleteLanding(context.Context, string) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.completeErr != nil {
		return false, l.completeErr
	}
	if l.landCompleted > 0 {
		return false, nil
	}
	l.landCompleted++
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

	// Landing (PMR-37) fixture state.
	mergeable                 *bool
	mergeableState            string
	merges                    int
	mergeMethods              []string
	mergeFails                bool
	updateBranchCalls         int
	updateBranchFails         bool
	updateBranchHead          string
	clearChecksOnBranchUpdate bool

	// Bounded-fix (PMR-46) audit comments posted to the PR issue thread.
	prComments []string
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
		"state": f.prState, "merged": f.prMerged, "merged_at": f.mergedAt(), "body": f.prBody,
		"mergeable": f.mergeable, "mergeable_state": f.mergeableState,
		"head": map[string]any{"ref": f.prHeadRef, "sha": f.prSHA},
		"base": map[string]any{"ref": f.prBaseRef},
	}
}

// mergedAt mirrors GitHub's pull-request-simple schema, which the list
// endpoint returns: it carries merged_at but no merged field. Landing
// verification treats either as merged, and merged_at is the one that is
// actually present in production, so the fixture must emit it.
func (f *apiFixture) mergedAt() any {
	if !f.prMerged {
		return nil
	}
	return "2026-08-25T09:00:00Z"
}

func boolPtr(b bool) *bool { return &b }

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
	case r.Method == http.MethodPut && r.URL.Path == "/repos/owner/repo/pulls/7/update-branch":
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil || body["expected_head_sha"] != f.prSHA {
			f.t.Errorf("update branch body=%v want expected_head_sha=%q", body, f.prSHA)
		}
		f.updateBranchCalls++
		if f.updateBranchFails {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"message":"Pull Request is not mergeable"}`))
			return
		}
		if f.updateBranchHead == "" {
			f.updateBranchHead = "updated-head"
		}
		f.prSHA = f.updateBranchHead
		if f.clearChecksOnBranchUpdate {
			f.checkRuns = nil
			f.statuses = nil
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"message":"Updating pull request branch."}`))
	case r.Method == http.MethodPut && r.URL.Path == "/repos/owner/repo/pulls/7/merge":
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			f.t.Errorf("merge body decode failed")
		}
		f.merges++
		if method, ok := body["merge_method"].(string); ok {
			f.mergeMethods = append(f.mergeMethods, method)
		}
		if f.mergeFails {
			w.WriteHeader(http.StatusMethodNotAllowed)
			_, _ = w.Write([]byte(`{"message":"Pull Request is not mergeable"}`))
			return
		}
		f.prMerged = true
		f.prState = "closed"
		encoded, _ := json.Marshal(map[string]any{"merged": true, "sha": f.prSHA, "message": "merged"})
		_, _ = w.Write(encoded)
	case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/commits/"+f.prSHA+"/status":
		encoded, _ := json.Marshal(map[string]any{"state": f.overall, "statuses": f.statuses})
		_, _ = w.Write(encoded)
	case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/commits/"+f.prSHA+"/check-runs":
		encoded, _ := json.Marshal(map[string]any{"check_runs": f.checkRuns})
		_, _ = w.Write(encoded)
	case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/pulls/7/reviews":
		encoded, _ := json.Marshal(f.reviews)
		_, _ = w.Write(encoded)
	case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/issues/7/comments":
		encoded, _ := json.Marshal(f.comments)
		_, _ = w.Write(encoded)
	case r.Method == http.MethodPost && r.URL.Path == "/repos/owner/repo/issues/7/comments":
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			f.t.Errorf("issue comment body decode failed")
		}
		text, _ := body["body"].(string)
		f.prComments = append(f.prComments, text)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":1}`))
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

// testLandingSession builds a Session configured for github_land_pr: the
// fixture's pull request head SHA is set to "head" so it matches fakeGit's
// default HEAD, meaning no push is exercised unless a test overrides it.
func testLandingSession(t *testing.T, api *apiFixture, git gitRunner, linear *fakeLinear, requiredChecks []string, mergeMethod string) (*Manager, *Session) {
	t.Helper()
	// newAPI's default prSHA ("sha1") never matches fakeGit's default HEAD
	// ("head"); align them here so ordinary landing tests do not exercise
	// the push-before-land path unless a test explicitly sets a different
	// prSHA before calling this helper (see TestLandPushesNewLocalCommitsBeforeLanding).
	if api.prSHA == "sha1" {
		api.prSHA = "head"
	}
	settings := api.settings()
	settings.MergeState = "Merging"
	settings.MergeMethod = mergeMethod
	settings.RequiredChecks = requiredChecks
	m := New(func() config.Settings { return config.Settings{GitHub: settings} }, slog.Default())
	m.git = git
	s := &Session{manager: m, settings: settings, issue: domain.Issue{ID: "issue-27", Identifier: "PMR-27", Title: "Lifecycle", URL: "https://linear.app/issue/PMR-27"}, workspace: t.TempDir(), branch: "symphony/pmr-27", linear: linear}
	return m, s
}

// passingRequiredChecks marks every name as a completed, successful check
// run so a landing test's gates other than "checks" can be exercised.
func passingRequiredChecks(api *apiFixture, names ...string) {
	api.mu.Lock()
	defer api.mu.Unlock()
	for _, name := range names {
		api.checkRuns = append(api.checkRuns, map[string]any{"name": name, "status": "completed", "conclusion": "success"})
	}
}

// readyToLand configures a fixture so every landing gate other than checks
// passes: approved review, no unresolved threads, and a clean mergeable PR.
func readyToLand(api *apiFixture) {
	api.mu.Lock()
	api.reviews = []map[string]any{{"user": map[string]any{"login": "alice"}, "state": "APPROVED", "body": "lgtm", "submitted_at": "t1"}}
	api.mergeable = boolPtr(true)
	api.mergeableState = "clean"
	api.mu.Unlock()
}

// staleBaseGit reports a different base-branch commit on the second
// rev-parse of the configured base ref, simulating a concurrent push to the
// base branch between github_land_pr's early and immediate pre-merge reads.
type staleBaseGit struct {
	*fakeGit
	baseCalls int
}

func (g *staleBaseGit) Run(ctx context.Context, dir string, args, env []string) (string, error) {
	if args[0] == "rev-parse" && len(args) > 1 && strings.HasPrefix(args[1], "refs/remotes/origin/") {
		g.baseCalls++
		if g.baseCalls == 1 {
			return "base-before", nil
		}
		return "base-after", nil
	}
	return g.fakeGit.Run(ctx, dir, args, env)
}

// divergedHeadGit reports that the worktree HEAD is not a descendant of the
// published pull request's head, simulating a worktree whose local branch
// diverged from what was last published.
type divergedHeadGit struct{ *fakeGit }

func (g *divergedHeadGit) Run(ctx context.Context, dir string, args, env []string) (string, error) {
	if args[0] == "merge-base" {
		return "", errors.New("not an ancestor")
	}
	return g.fakeGit.Run(ctx, dir, args, env)
}

// pushSyncGit keeps the fixture's fake GitHub PR head in sync with a
// simulated push, so a "new local commits need pushing before landing" test
// can assert the whole flow proceeds to a fresh, matching head.
type pushSyncGit struct {
	*fakeGit
	api     *apiFixture
	newHead string
}

func (g *pushSyncGit) Run(ctx context.Context, dir string, args, env []string) (string, error) {
	if args[0] == "push" {
		g.api.mu.Lock()
		g.api.prSHA = g.newHead
		g.api.mu.Unlock()
	}
	return g.fakeGit.Run(ctx, dir, args, env)
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
	if linear.reconciled != 1 {
		t.Fatalf("reconciliations=%d", linear.reconciled)
	}
	if strings.Count(logs.String(), "GitHub merge completed Linear issue") != 1 {
		t.Fatalf("merge completion log=%s", logs.String())
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
	if linear2.reconciled != 0 || strings.Count(closedLogs.String(), "closed without merge") != 1 {
		t.Fatalf("reconciled=%d logs=%s", linear2.reconciled, closedLogs.String())
	}
	if strings.Contains(logs.String()+closedLogs.String(), "private-token") {
		t.Fatal("logs exposed credential")
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

func TestPollStillOpenPullRequestDoesNotReconcile(t *testing.T) {
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

func TestLandWaitsWhileRequiredChecksAreMissingOrPending(t *testing.T) {
	for _, test := range []struct {
		name      string
		configure func(*apiFixture)
	}{
		{name: "missing", configure: func(api *apiFixture) {}},
		{name: "pending", configure: func(api *apiFixture) {
			api.checkRuns = append(api.checkRuns, map[string]any{"name": "ci/build", "status": "in_progress", "conclusion": nil})
		}},
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
			if result.Status != LandWaiting {
				t.Fatalf("result=%+v", result)
			}
			if linear.refused != 0 || linear.landCompleted != 0 || api.merges != 0 {
				t.Fatalf("waiting mutated Linear or GitHub: refused=%d completed=%d merges=%d", linear.refused, linear.landCompleted, api.merges)
			}
		})
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
	if api.merges != 0 || linear.landCompleted != 0 {
		t.Fatalf("failing checks must never merge: merges=%d completed=%d", api.merges, linear.landCompleted)
	}
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
	if err != nil || result.Status != LandWaiting || result.Reason != "required checks are pending" {
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

// alwaysStaleBaseGit makes every base read observe a new commit, including
// after a successful update-branch call.
type alwaysStaleBaseGit struct {
	*fakeGit
	baseCalls int
}

func (g *alwaysStaleBaseGit) Run(ctx context.Context, dir string, args, env []string) (string, error) {
	if args[0] == "rev-parse" && len(args) > 1 && strings.HasPrefix(args[1], "refs/remotes/origin/") {
		g.baseCalls++
		return "base-" + strconv.Itoa(g.baseCalls), nil
	}
	return g.fakeGit.Run(ctx, dir, args, env)
}

// fixPushGit simulates a Codex fix turn whose worktree HEAD (head) advances
// between landing attempts; a push then syncs the fixture PR head to it, so a
// retry after a fix exercises the push-before-land + audit-comment path.
type fixPushGit struct {
	*fakeGit
	api  *apiFixture
	head string
}

func (g *fixPushGit) Run(ctx context.Context, dir string, args, env []string) (string, error) {
	if args[0] == "rev-parse" && len(args) > 1 && args[1] == "HEAD" {
		return g.head, nil
	}
	if args[0] == "push" {
		g.api.mu.Lock()
		g.api.prSHA = g.head
		g.api.mu.Unlock()
	}
	return g.fakeGit.Run(ctx, dir, args, env)
}

func failingChecks(name string) []map[string]any {
	return []map[string]any{{"name": name, "status": "completed", "conclusion": "failure"}}
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

// verifyLandedManager builds a read-only Manager for the terminal workspace
// cleanup verification path, with no session, worktree, or Linear handoff: the
// verifier is host-owned and must never need any of them.
func verifyLandedManager(t *testing.T, api *apiFixture, mutate func(*config.GitHub)) *Manager {
	t.Helper()
	settings := api.settings()
	if mutate != nil {
		mutate(&settings)
	}
	return New(func() config.Settings { return config.Settings{GitHub: settings} }, slog.Default())
}

func TestVerifyLandedConfirmsOnlyTheMergedPullRequestHead(t *testing.T) {
	const workspaceHead = "landed-head"
	tests := []struct {
		name      string
		configure func(*apiFixture)
		mutate    func(*config.GitHub)
		commit    string
		want      bool
		wantErr   bool
		wantCalls bool
	}{
		{
			name:      "merged head commit",
			configure: func(api *apiFixture) { api.prExists, api.prMerged, api.prSHA = true, true, workspaceHead },
			commit:    workspaceHead,
			want:      true,
			wantCalls: true,
		},
		{
			name:      "merged pull request with a rewritten head",
			configure: func(api *apiFixture) { api.prExists, api.prMerged, api.prSHA = true, true, "rewritten-head" },
			commit:    workspaceHead,
			wantCalls: true,
		},
		{
			name:      "open pull request",
			configure: func(api *apiFixture) { api.prExists, api.prSHA = true, workspaceHead },
			commit:    workspaceHead,
			wantCalls: true,
		},
		{
			name:      "closed unmerged pull request",
			configure: func(api *apiFixture) { api.prExists, api.prState, api.prSHA = true, "closed", workspaceHead },
			commit:    workspaceHead,
			wantCalls: true,
		},
		{
			name:      "no pull request",
			configure: func(api *apiFixture) {},
			commit:    workspaceHead,
			wantCalls: true,
		},
		{
			name:      "ambiguous pull requests",
			configure: func(api *apiFixture) { api.prExists, api.multiplePulls, api.prMerged = true, true, true },
			commit:    workspaceHead,
			wantErr:   true,
			wantCalls: true,
		},
		{
			name:      "github integration disabled",
			configure: func(api *apiFixture) { api.prExists, api.prMerged, api.prSHA = true, true, workspaceHead },
			mutate:    func(s *config.GitHub) { s.Enabled = false },
			commit:    workspaceHead,
		},
		{
			name:      "no recorded commit",
			configure: func(api *apiFixture) { api.prExists, api.prMerged, api.prSHA = true, true, workspaceHead },
			commit:    "   ",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			api := newAPI(t)
			test.configure(api)
			m := verifyLandedManager(t, api, test.mutate)
			issue := domain.Issue{ID: "issue-27", Identifier: "PMR-27"}

			landed, err := m.VerifyLanded(context.Background(), issue, test.commit)
			if test.wantErr != (err != nil) {
				t.Fatalf("VerifyLanded error=%v, wantErr=%t", err, test.wantErr)
			}
			if landed != test.want {
				t.Fatalf("VerifyLanded=%t, want %t", landed, test.want)
			}
			api.mu.Lock()
			defer api.mu.Unlock()
			if (len(api.auth) > 0) != test.wantCalls {
				t.Fatalf("GitHub requests=%d, want any=%t", len(api.auth), test.wantCalls)
			}
			if api.created != 0 || api.merges != 0 || len(api.patches) != 0 || api.updateBranchCalls != 0 {
				t.Fatalf("verification mutated GitHub: created=%d merges=%d patches=%v update_branch=%d", api.created, api.merges, api.patches, api.updateBranchCalls)
			}
		})
	}
}

// A branch name Symphony would never have produced must not be verified
// against some other repository branch.
func TestVerifyLandedRejectsAnIssueWithoutADerivableBranch(t *testing.T) {
	api := newAPI(t)
	api.prExists, api.prMerged = true, true
	m := verifyLandedManager(t, api, nil)
	landed, err := m.VerifyLanded(context.Background(), domain.Issue{ID: "issue-27", Identifier: "-.-"}, api.prSHA)
	if landed || err != nil {
		t.Fatalf("VerifyLanded=%t err=%v, want false and no error", landed, err)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.auth) != 0 {
		t.Fatalf("undecidable branch issued %d GitHub requests", len(api.auth))
	}
}
