package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	mu        sync.Mutex
	dirty     bool
	noChange  bool
	failFetch bool
	calls     [][]string
	envs      [][]string

	// onPush lets a test simulate provider-side state changing during the
	// push's network round trip -- for example a pull request merging while
	// Publish is still pushing the branch (PMR-149).
	onPush func()
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
	case "fetch":
		if g.failFetch {
			return "", errors.New("git fetch failed")
		}
	case "rev-parse":
		if args[1] == "HEAD" {
			return "head", nil
		}
		if g.noChange {
			return "head", nil
		}
		return "base", nil
	case "merge-base":
		return "", nil
	case "push":
		if g.onPush != nil {
			g.onPush()
		}
		return "", nil
	}
	return "", nil
}

type fakeLinear struct {
	mu        sync.Mutex
	activeErr error
	linkErr   error
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
	refusedReason    string
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
	if l.linkErr != nil {
		return l.linkErr
	}
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

func (l *fakeLinear) RefuseLanding(_ context.Context, mergeState, reason string) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.refuseErr != nil {
		return false, l.refuseErr
	}
	l.refused++
	l.refusedDestState = mergeState
	l.refusedReason = reason
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

	// failMethod/failPath/failStatus/failBody let a test make exactly one
	// endpoint respond with an arbitrary status and body instead of its usual
	// handling, so a guard test can plant provider wire content in that body
	// and assert it never reaches an agent-visible error message (PMR-149).
	failMethod string
	failPath   string
	failStatus int
	failBody   string

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
	// pullGets counts single-pull-request reads, the one request the linked
	// pull request poll loop issues per tick. A test asserts it stops growing
	// once a link settles (PMR-112).
	pullGets int

	statuses     []map[string]any
	overall      string
	checkRuns    []map[string]any
	reviews      []map[string]any
	comments     []map[string]any
	threads      []map[string]any
	threadsTotal int
	graphqlErr   bool

	// pageSize > 0 makes the paginated REST collections (commit statuses,
	// check runs, reviews) serve at most pageSize items per response with a
	// rel="next" Link header, exactly as GitHub does, so a test can place a
	// gate-relevant item past the first page (PMR-190). threadPageSize does
	// the same for the GraphQL review-thread connection's cursor.
	pageSize       int
	threadPageSize int

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

// pullReads reports how many single-pull-request GETs the fixture has served.
func (f *apiFixture) pullReads() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pullGets
}

// reconciliations reads the merged-poll completion count under the fake's own
// mutex, so a test may observe it while Manager.Run polls on another goroutine.
func (l *fakeLinear) reconciliations() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.reconciled
}

// tracked reports how many linked pull requests the manager still polls.
func tracked(m *Manager) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.linked)
}

func (f *apiFixture) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	w.Header().Set("Content-Type", "application/json")
	if f.failStatus != 0 && r.Method == f.failMethod && r.URL.Path == f.failPath {
		w.WriteHeader(f.failStatus)
		_, _ = w.Write([]byte(f.failBody))
		return
	}
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
		f.pullGets++
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
		page := f.paginate(w, r, f.statuses)
		encoded, _ := json.Marshal(map[string]any{"state": f.overall, "statuses": page})
		_, _ = w.Write(encoded)
	case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/commits/"+f.prSHA+"/check-runs":
		page := f.paginate(w, r, f.checkRuns)
		encoded, _ := json.Marshal(map[string]any{"check_runs": page})
		_, _ = w.Write(encoded)
	case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/pulls/7/reviews":
		encoded, _ := json.Marshal(f.paginate(w, r, f.reviews))
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
		var request struct {
			Variables struct {
				Cursor string `json:"cursor"`
			} `json:"variables"`
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil {
			f.t.Errorf("graphql body decode failed")
		}
		nodes, pageInfo := f.threadPage(request.Variables.Cursor)
		encoded, _ := json.Marshal(map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{"reviewThreads": map[string]any{"totalCount": total, "pageInfo": pageInfo, "nodes": nodes}}}}})
		_, _ = w.Write(encoded)
	default:
		f.t.Errorf("unexpected GitHub request %s %s", r.Method, r.URL.String())
		w.WriteHeader(http.StatusNotFound)
	}
}

// paginate serves the page of items the request's page query parameter asks
// for, setting the rel="next" Link header when more remain. pageSize == 0
// keeps every item on one unpaginated page, which is what every test that
// does not care about pagination gets.
func (f *apiFixture) paginate(w http.ResponseWriter, r *http.Request, items []map[string]any) []map[string]any {
	if f.pageSize <= 0 {
		return items
	}
	page, err := strconv.Atoi(r.URL.Query().Get("page"))
	if err != nil || page < 1 {
		page = 1
	}
	start := min(len(items), (page-1)*f.pageSize)
	end := min(len(items), start+f.pageSize)
	if end < len(items) {
		query := r.URL.Query()
		query.Set("page", strconv.Itoa(page+1))
		w.Header().Set("Link", fmt.Sprintf("<%s%s?%s>; rel=\"next\", <%s%s?page=99>; rel=\"last\"", f.server.URL, r.URL.Path, query.Encode(), f.server.URL, r.URL.Path))
	}
	return items[start:end]
}

// threadPage serves one page of the GraphQL review-thread connection, reading
// the cursor as the index of the first thread the caller has not yet seen.
func (f *apiFixture) threadPage(cursor string) ([]map[string]any, map[string]any) {
	if f.threadPageSize <= 0 {
		return f.threads, map[string]any{"hasNextPage": false, "endCursor": nil}
	}
	start, _ := strconv.Atoi(cursor)
	start = min(len(f.threads), max(0, start))
	end := min(len(f.threads), start+f.threadPageSize)
	return f.threads[start:end], map[string]any{"hasNextPage": end < len(f.threads), "endCursor": strconv.Itoa(end)}
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

// refreshBaseRefSession builds a Session for RefreshBaseRef alone: it needs
// no GitHub REST/GraphQL fixture, only the configured base branch and a
// gitRunner, so it starts no httptest server (PMR-141).
func refreshBaseRefSession(t *testing.T, git *fakeGit, baseBranch string) *Session {
	t.Helper()
	settings := config.GitHub{Enabled: true, Owner: "owner", Repository: "repo", BaseBranch: baseBranch, Token: "private-token"}
	m := New(func() config.Settings { return config.Settings{GitHub: settings} }, slog.Default())
	m.git = git
	return &Session{manager: m, settings: settings, issue: domain.Issue{ID: "issue-27", Identifier: "PMR-27"}, workspace: t.TempDir(), branch: "symphony/pmr-27"}
}

// overlapTrackingGit records the highest number of concurrent "fetch"
// invocations it ever observed, holding each one open briefly so two
// goroutines calling it at once are actually likely to overlap absent
// serialization.
type overlapTrackingGit struct {
	mu        sync.Mutex
	active    int
	maxActive int
}

func (g *overlapTrackingGit) Run(_ context.Context, _ string, args, _ []string) (string, error) {
	if args[0] != "fetch" {
		return "base", nil
	}
	g.mu.Lock()
	g.active++
	if g.active > g.maxActive {
		g.maxActive = g.active
	}
	g.mu.Unlock()
	time.Sleep(20 * time.Millisecond)
	g.mu.Lock()
	g.active--
	g.mu.Unlock()
	return "", nil
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

// countingFailFetchGit fails exactly the nth "fetch" invocation (1-indexed)
// and otherwise delegates to fakeGit, letting a test isolate the post-land
// base ref refresh (PMR-135) from the two fetches Land already performs
// earlier for its own stale-base gate.
type countingFailFetchGit struct {
	*fakeGit
	failOnFetch int
	fetchCount  int
}

func (g *countingFailFetchGit) Run(ctx context.Context, dir string, args, env []string) (string, error) {
	if args[0] == "fetch" {
		g.mu.Lock()
		g.fetchCount++
		n := g.fetchCount
		g.mu.Unlock()
		if n == g.failOnFetch {
			return "", errors.New("boom")
		}
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

// failingGit fails exactly the one git invocation whose full argument list
// matches failArgs, and otherwise delegates to fakeGit's defaults. It lets a
// single table of Publish refusal causes pin an exact message per underlying
// git failure without a bespoke fake type per case. message defaults to
// "boom" when unset, so existing callers that only care that the call failed
// need not name one.
type failingGit struct {
	*fakeGit
	failArgs []string
	message  string
}

func (g *failingGit) Run(ctx context.Context, dir string, args, env []string) (string, error) {
	if len(args) == len(g.failArgs) {
		match := true
		for i, a := range g.failArgs {
			if args[i] != a {
				match = false
				break
			}
		}
		if match {
			message := g.message
			if message == "" {
				message = "boom"
			}
			return "", errors.New(message)
		}
	}
	return g.fakeGit.Run(ctx, dir, args, env)
}

// nonFastForwardThenRacedPushGit simulates a worktree that rebased (so the
// ancestor check below fails, and Publish falls back to a leased push) where
// the remote branch then moved again before the leased push ran -- the lease
// no longer matches what Publish observed via findPull.
type nonFastForwardThenRacedPushGit struct{ *fakeGit }

func (g *nonFastForwardThenRacedPushGit) Run(ctx context.Context, dir string, args, env []string) (string, error) {
	if len(args) == 4 && args[0] == "merge-base" && args[1] == "--is-ancestor" && args[2] == "sha1" && args[3] == "head" {
		return "", errors.New("not an ancestor")
	}
	if args[0] == "push" {
		return "", errors.New("stale info; the remote branch was updated since the last fetch")
	}
	return g.fakeGit.Run(ctx, dir, args, env)
}

// logRecordError decodes a JSONL log buffer and returns the "error" attribute
// of the first record whose "msg" matches, or "" if no such record exists (a
// present-but-empty attribute is indistinguishable from an absent one for
// this issue's purposes: both leave an operator without a cause).
func logRecordError(t *testing.T, logs, msg string) string {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(logs), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("invalid log line %q: %v", line, err)
		}
		if record["msg"] != msg {
			continue
		}
		errText, _ := record["error"].(string)
		return errText
	}
	return ""
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

// waitUntil polls a condition the poll loop satisfies on another goroutine.
func waitUntil(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting until %s", what)
}
