package codex

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/domain"
)

// This file covers the wiring from a turn ending to the deferred
// Merging -> In Review landing transition (PMR-46, PMR-78): the fallback that
// returns an issue to human review when a turn ended after a retryable landing
// gate without a landing. internal/github asserts the fallback itself by calling
// FinalizeLanding directly; what is only true of a real session is that a turn
// ending reaches that call at all, and on which of its several endings.
//
// Nothing here substitutes a capability, a registry, or a provider: the backend
// builds its own registry from a real linear.Handoff and a real github.Manager,
// and the only stand-ins are the two remotes (one httptest server) and the two
// child processes (the scripted app-server, and a scripted git -- see
// writeFakeGit).

// landingStates are the fixed Linear states this fixture's team has. Real IDs do
// not matter, only that they are stable and distinct.
var landingStates = map[string]string{
	"Merging": "state-merging", "In Review": "state-in-review", "Done": "state-done",
}

// landingRemote is the pair of remotes a deferred landing transition touches,
// served by one httptest server: the GitHub REST/GraphQL API whose required
// check decides whether the landing gate is retryable, and the Linear GraphQL
// API the deferred transition and its comment are issued against. The Linear
// side is stateful because RefuseLanding re-reads the issue after transitioning
// it and refuses to claim success unless the state really moved.
type landingRemote struct {
	t      *testing.T
	server *httptest.Server

	mu          sync.Mutex
	state       string   // the bound issue's current Linear state name
	transitions []string // every state Linear was moved to, in order
	comments    []string // every Linear comment body, in order
	checkRuns   []map[string]any
	// checkRunsAfterFirstRead, when set, is served from the second required-check
	// read onwards. It is what turns one session into "gate hit, then the fix
	// turn's retry finds the check green", which no single fixed table can be.
	checkRunsAfterFirstRead []map[string]any
	checkReads              int
	reviews                 []any
	mergeable               any
	merged                  bool
	merges                  int
}

func newLandingRemote(t *testing.T) *landingRemote {
	f := &landingRemote{t: t, state: "Merging"}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}

// failingRequiredCheck is the retryable hard gate this file is built on: a
// completed, failed required check with fix attempts remaining defers the
// transition instead of applying it, which is the only state in which a turn
// ending has anything left to do.
func (f *landingRemote) failingRequiredCheck() {
	f.mu.Lock()
	f.checkRuns = []map[string]any{{"name": "ci/build", "status": "completed", "conclusion": "failure"}}
	f.mu.Unlock()
}

// retryableGateThenReady is the sequence the deferred fallback's landed guard
// exists for, and the only state in which that guard is what decides the outcome:
// the first landing attempt hits the retryable gate, and the fix turn's retry
// finds the check green and merges. The turn then ends with a gate on record and
// a merged pull request.
func (f *landingRemote) retryableGateThenReady() {
	f.failingRequiredCheck()
	f.mu.Lock()
	f.checkRunsAfterFirstRead = []map[string]any{{"name": "ci/build", "status": "completed", "conclusion": "success"}}
	f.reviews = []any{map[string]any{"user": map[string]any{"login": "alice"}, "state": "APPROVED", "body": "lgtm", "submitted_at": "t1"}}
	f.mergeable = true
	f.mu.Unlock()
}

// readyToLand makes every landing gate pass, so the landing merges and resolves.
func (f *landingRemote) readyToLand() {
	f.mu.Lock()
	f.checkRuns = []map[string]any{{"name": "ci/build", "status": "completed", "conclusion": "success"}}
	f.reviews = []any{map[string]any{"user": map[string]any{"login": "alice"}, "state": "APPROVED", "body": "lgtm", "submitted_at": "t1"}}
	f.mergeable = true
	f.mu.Unlock()
}

func (f *landingRemote) observed() (transitions, comments []string, merges int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.transitions...), append([]string(nil), f.comments...), f.merges
}

// settings is the whole configuration the landing path reads: an active Merging
// issue, the configured refuse_landing edge the fallback needs, and the
// bounded-fix feature that makes a failing required check retryable.
func (f *landingRemote) settings() config.Settings {
	return config.Settings{
		Tracker: config.Tracker{
			Provider:        map[string]any{"api_key": "linear-token", "project_slug_id": "project-1", "endpoint": f.server.URL},
			ActiveStates:    []string{"Merging"},
			TerminalStates:  []string{"Done"},
			HandoffState:    "In Review",
			HostTransitions: config.HostTransitions{RefuseLanding: map[string]string{"merging": "In Review"}},
		},
		GitHub: config.GitHub{
			Enabled: true, Owner: "owner", Repository: "repo", BaseBranch: "main",
			Token: "github-token", Endpoint: f.server.URL,
			MergeState: "Merging", MergeMethod: "merge", RequiredChecks: []string{"ci/build"},
			LandFixEnabled: true, MaxLandAttempts: 2,
		},
	}
}

func (f *landingRemote) issue() domain.Issue {
	return domain.Issue{ID: "issue-27", Identifier: "PMR-27", Title: "Landing", State: "Merging", URL: "https://linear.app/issue/PMR-27"}
}

func (f *landingRemote) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if strings.HasPrefix(r.URL.Path, "/repos/") {
		f.serveGitHub(w, r)
		return
	}
	f.serveLinear(w, r)
}

func (f *landingRemote) serveGitHub(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/pulls":
		f.write(w, []any{f.pullJSON()})
	case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/pulls/7":
		f.write(w, f.pullJSON())
	case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/commits/head/status":
		f.write(w, map[string]any{"state": "", "statuses": []any{}})
	case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/commits/head/check-runs":
		f.checkReads++
		runs := f.checkRuns
		if f.checkRunsAfterFirstRead != nil && f.checkReads > 1 {
			runs = f.checkRunsAfterFirstRead
		}
		f.write(w, map[string]any{"check_runs": runs})
	case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/pulls/7/reviews":
		f.write(w, f.reviews)
	case r.Method == http.MethodPut && r.URL.Path == "/repos/owner/repo/pulls/7/merge":
		f.merges++
		f.merged = true
		f.write(w, map[string]any{"merged": true, "sha": "head", "message": "merged"})
	default:
		f.t.Errorf("unexpected GitHub request %s %s", r.Method, r.URL.String())
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *landingRemote) pullJSON() map[string]any {
	state := "open"
	if f.merged {
		state = "closed"
	}
	return map[string]any{
		"number": 7, "html_url": "https://github.com/owner/repo/pull/7",
		"state": state, "merged": f.merged, "mergeable": f.mergeable,
		"head": map[string]any{"ref": "symphony/pmr-27", "sha": "head"},
		"base": map[string]any{"ref": "main"},
	}
}

func (f *landingRemote) serveLinear(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		f.t.Errorf("decode Linear request: %v", err)
		return
	}
	switch {
	case r.URL.Path == "/graphql":
		// The GitHub review-thread query, reached only once required checks pass.
		f.write(w, map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{
			"reviewThreads": map[string]any{"totalCount": 0, "nodes": []any{}}}}}})
	case strings.Contains(body.Query, "SymphonyLinearHandoffIssue"):
		f.write(w, map[string]any{"data": map[string]any{"issue": map[string]any{
			"id": "issue-27", "identifier": "PMR-27", "title": "Landing", "description": "safe",
			"url":     "https://linear.app/issue/PMR-27",
			"project": map[string]any{"id": "project-id-1", "slugId": "project-1"},
			"team":    map[string]any{"id": "team-1"},
			"state":   map[string]any{"id": landingStates[f.state], "name": f.state},
		}}})
	case strings.Contains(body.Query, "SymphonyLinearHandoffStates"):
		nodes := make([]map[string]any, 0, len(landingStates))
		for name, id := range landingStates {
			nodes = append(nodes, map[string]any{"id": id, "name": name})
		}
		f.write(w, map[string]any{"data": map[string]any{"team": map[string]any{"id": "team-1", "states": map[string]any{"nodes": nodes}}}})
	case strings.Contains(body.Query, "SymphonyLinearHandoffTransition"):
		id, _ := body.Variables["stateID"].(string)
		for name, stateID := range landingStates {
			if stateID == id {
				f.state = name
				f.transitions = append(f.transitions, name)
			}
		}
		f.write(w, map[string]any{"data": map[string]any{"issueUpdate": map[string]any{"success": true}}})
	case strings.Contains(body.Query, "SymphonyLinearHandoffComment"):
		text, _ := body.Variables["body"].(string)
		f.comments = append(f.comments, text)
		f.write(w, map[string]any{"data": map[string]any{"commentCreate": map[string]any{"success": true}}})
	default:
		f.t.Errorf("unexpected Linear query: %s", body.Query)
	}
}

func (f *landingRemote) write(w http.ResponseWriter, payload any) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		f.t.Errorf("encode response: %v", err)
		return
	}
	_, _ = w.Write(encoded)
}

// writeFakeGit puts a scripted git first on PATH. The landing path shells out to
// git before it can reach any gate, and internal/github's own git runner is
// package-private, so a test outside that package can only substitute the
// executable -- the same way every test in this package substitutes the
// app-server. The worktree it reports is the ordinary one landing expects: the
// configured repository as origin, nothing uncommitted, and a HEAD that already
// matches the published pull request head, so no push path is involved.
//
// It is a stub, not a fake worktree, and nothing here asserts anything about the
// git surface itself: it matches on the first two arguments only, ignores the
// directory it is run in, and answers the base-branch rev-parse with a constant,
// so the base-moved gate can never fire through it. internal/github owns the
// tests for what landing asks git and what it does with the answers.
func writeFakeGit(t *testing.T, dir string) {
	t.Helper()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
case "$1 $2" in
"remote get-url") printf '%s\n' 'https://github.com/owner/repo.git' ;;
"status --porcelain") ;;
"rev-parse HEAD") printf '%s\n' 'head' ;;
"rev-parse refs/remotes/origin/main") printf '%s\n' 'base' ;;
"fetch origin") ;;
*) printf 'unexpected git %s\n' "$*" >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// landingScript is the app-server transcript every test here starts from: it
// completes the handshake, calls github_land_pr once, asserts the retryable-gate
// refusal came back, records that it did where the test can wait for it, and
// then ends the turn however the caller asked.
func landingScript(dir, wantSuccess, ending string) string {
	return `
IFS= read -r line
printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{}}'
IFS= read -r line
IFS= read -r line
case "$line" in *github_land_pr*) ;; *) exit 30;; esac
printf '%s\n' '{"jsonrpc":"2.0","id":2,"result":{"thread":{"id":"thread-1"}}}'
IFS= read -r line
printf '%s\n' '{"jsonrpc":"2.0","id":3,"result":{"turn":{"id":"turn-1"}}}'
printf '%s\n' '{"jsonrpc":"2.0","id":99,"method":"item/tool/call","params":{"tool":"github_land_pr","arguments":{}}}'
IFS= read -r line
case "$line" in *'"success":` + wantSuccess + `'*) ;; *) exit 31;; esac
printf 'done\n' > ` + filepath.Join(dir, "landed") + `
` + ending
}

// landRetry is a second github_land_pr call in the same turn, which is what a fix
// turn does after a retryable gate. It goes where an ending goes, so it composes:
// landingScript(dir, "false", landRetry(dir, "true", turnCompleted)).
func landRetry(dir, wantSuccess, ending string) string {
	return `printf '%s\n' '{"jsonrpc":"2.0","id":100,"method":"item/tool/call","params":{"tool":"github_land_pr","arguments":{}}}'
IFS= read -r line
case "$line" in *'"success":` + wantSuccess + `'*) ;; *) exit 32;; esac
printf 'retried\n' > ` + filepath.Join(dir, "retried") + `
` + ending
}

const (
	turnCompleted = `printf '%s\n' '{"jsonrpc":"2.0","method":"turn/completed","params":{}}'` + "\n"
	turnFailed    = `printf '%s\n' '{"jsonrpc":"2.0","method":"turn/failed","params":{}}'` + "\n"
	turnCancelled = `printf '%s\n' '{"jsonrpc":"2.0","method":"turn/cancelled","params":{}}'` + "\n"
	turnHangs     = "sleep 120\n"
)

// landingRequest is a request bound to the fixture's Merging issue. TurnTimeout
// is generous by default; the timeout case shortens it.
func landingRequest(f *landingRemote, dir, script string) domain.AgentRequest {
	r := request(dir, script)
	r.Issue = f.issue()
	return r
}

// waitForLanding blocks until the child has had its github_land_pr call refused,
// which is what puts the session in the only state a turn end has work to do in.
func waitForLanding(t *testing.T, dir string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(dir, "landed")); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the child never reported a refused github_land_pr call")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// settle ends the session the way the coordinator always does, and waits for the
// child to be reaped so a test's temporary directory outlives it.
//
// On a path that has already cancelled the session it is a pure no-op -- Cancel
// deleted the session from the backend's map and the second call returns on the
// nil client -- so it asserts nothing there. Where it does assert something is
// after a protocol turn end: Cancel is a second turn-end path over the same
// session, and every count in this file is still "exactly one" after it.
func settle(t *testing.T, b *Backend, session domain.AgentSession) {
	t.Helper()
	if err := b.Cancel(context.Background(), session); err != nil {
		t.Fatalf("cancel: %v", err)
	}
}

// drainLanding drains a turn's stream under a bound. A bare `for range events`
// would turn a stream that never closes into a package-timeout hang with no
// failing test named, which is the one failure mode a wiring test must not have.
// The patience is generous because a turn end here does real work before the
// stream closes -- up to three tracker round trips -- and because what these
// tests assert is what the tracker was told, never how quickly.
func drainLanding(t *testing.T, events <-chan domain.Event) []domain.Event {
	t.Helper()
	var collected []domain.Event
	timeout := time.After(45 * time.Second)
	for {
		select {
		case event, ok := <-events:
			if !ok {
				return collected
			}
			collected = append(collected, event)
		case <-timeout:
			t.Fatalf("event stream did not close; collected %d events", len(collected))
		}
	}
}

// TestEveryTurnEndPathFiresTheDeferredLandingTransition is the coverage PMR-86
// exists for. A turn that hit a retryable landing gate and then ended without
// landing must return the issue to human review, and every way that turn can end
// has to do it.
//
// The case set is this transport's own, and it is deliberately not the Claude
// file's: the app-server ends a turn with one of three protocol notifications,
// which that transport has no equivalent of, and a hard Cancel is the one ending
// both share. The turn timeout is covered separately, because on this transport
// it is not a turn end at all -- see
// TestATimedOutTurnFinalizesOnlyWhenTheRunIsStopped.
func TestEveryTurnEndPathFiresTheDeferredLandingTransition(t *testing.T) {
	for name, test := range map[string]struct {
		ending string
		cancel bool
	}{
		"turn/completed": {ending: turnCompleted},
		"turn/failed":    {ending: turnFailed},
		"turn/cancelled": {ending: turnCancelled},
		"hard cancel":    {ending: turnHangs, cancel: true},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			remote := newLandingRemote(t)
			remote.failingRequiredCheck()
			writeFakeGit(t, dir)
			script := writeAppServer(t, dir, landingScript(dir, "false", test.ending))
			b := integratedBackend(func() config.Settings { return remote.settings() })
			session, events, err := b.Start(context.Background(), landingRequest(remote, dir, script))
			if err != nil {
				t.Fatal(err)
			}
			waitForLanding(t, dir)
			if test.cancel {
				// Cancel is what the coordinator calls for a run it is
				// stopping. It must have finalized the landing by the time it
				// returns: the coordinator has no other handle on the session
				// left afterwards.
				settle(t, b, session)
				transitions, _, _ := remote.observed()
				if len(transitions) != 1 || transitions[0] != "In Review" {
					t.Fatalf("cancel returned with Linear transitions=%v, want exactly one to In Review", transitions)
				}
			}
			drainLanding(t, events)
			settle(t, b, session)
			transitions, comments, merges := remote.observed()
			if len(transitions) != 1 || transitions[0] != "In Review" {
				t.Fatalf("Linear transitions=%v, want exactly one to In Review", transitions)
			}
			if len(comments) != 1 || !strings.Contains(comments[0], "required checks failed") {
				t.Fatalf("Linear comments=%v, want exactly one naming the failed gate", comments)
			}
			if merges != 0 {
				t.Fatalf("a refused landing merged %d times", merges)
			}
		})
	}
}

// TestATurnEndAfterAResolvedLandingLeavesTheIssueDone is the first negative: the
// deferred fallback exists for a turn that ended *without* landing, so a turn
// that merged must never be walked back to review. Getting this wrong would move
// a Done issue to In Review with its pull request already merged.
func TestATurnEndAfterAResolvedLandingLeavesTheIssueDone(t *testing.T) {
	dir := t.TempDir()
	remote := newLandingRemote(t)
	remote.readyToLand()
	writeFakeGit(t, dir)
	script := writeAppServer(t, dir, landingScript(dir, "true", turnCompleted))
	b := integratedBackend(func() config.Settings { return remote.settings() })
	session, events, err := b.Start(context.Background(), landingRequest(remote, dir, script))
	if err != nil {
		t.Fatal(err)
	}
	drainLanding(t, events)
	settle(t, b, session)
	transitions, comments, merges := remote.observed()
	if merges != 1 {
		t.Fatalf("merges=%d, want exactly one", merges)
	}
	if len(transitions) != 1 || transitions[0] != "Done" {
		t.Fatalf("Linear transitions=%v, want exactly one to Done", transitions)
	}
	if len(comments) != 0 {
		t.Fatalf("a resolved landing posted a refusal comment: %v", comments)
	}
}

// TestATurnEndWithTheBoundedFixOffDefersNothing is the second negative. With the
// feature off, a failing required check is not a retryable gate at all: landing
// refuses inline, applies the fallback there, and leaves the turn end with
// nothing to do. The refusal comment is the deferred path's own fingerprint, so
// its absence is what says the deferred path did not run.
func TestATurnEndWithTheBoundedFixOffDefersNothing(t *testing.T) {
	dir := t.TempDir()
	remote := newLandingRemote(t)
	remote.failingRequiredCheck()
	writeFakeGit(t, dir)
	script := writeAppServer(t, dir, landingScript(dir, "false", turnCompleted))
	settings := remote.settings()
	settings.GitHub.LandFixEnabled = false
	b := integratedBackend(func() config.Settings { return settings })
	session, events, err := b.Start(context.Background(), landingRequest(remote, dir, script))
	if err != nil {
		t.Fatal(err)
	}
	drainLanding(t, events)
	settle(t, b, session)
	transitions, comments, _ := remote.observed()
	if len(transitions) != 1 || transitions[0] != "In Review" {
		t.Fatalf("Linear transitions=%v, want exactly the one inline refusal", transitions)
	}
	if len(comments) != 0 {
		t.Fatalf("the bounded-fix feature is off, yet the deferred comment was posted: %v", comments)
	}
}

// TestATurnEndWithoutALandingAttemptTransitionsNothing is the third negative and
// the one that says the deferred transition is a landing fallback rather than a
// turn-end habit: a session that never hit a landing gate must leave the tracker
// exactly as it found it, however the turn ended.
func TestATurnEndWithoutALandingAttemptTransitionsNothing(t *testing.T) {
	dir := t.TempDir()
	remote := newLandingRemote(t)
	remote.failingRequiredCheck()
	writeFakeGit(t, dir)
	script := writeAppServer(t, dir, `
IFS= read -r line
printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{}}'
IFS= read -r line
IFS= read -r line
printf '%s\n' '{"jsonrpc":"2.0","id":2,"result":{"thread":{"id":"thread-1"}}}'
IFS= read -r line
printf '%s\n' '{"jsonrpc":"2.0","id":3,"result":{"turn":{"id":"turn-1"}}}'
`+turnCompleted)
	b := integratedBackend(func() config.Settings { return remote.settings() })
	session, events, err := b.Start(context.Background(), landingRequest(remote, dir, script))
	if err != nil {
		t.Fatal(err)
	}
	drainLanding(t, events)
	settle(t, b, session)
	transitions, comments, merges := remote.observed()
	if len(transitions) != 0 || len(comments) != 0 || merges != 0 {
		t.Fatalf("a turn that never attempted a landing produced transitions=%v comments=%v merges=%d", transitions, comments, merges)
	}
}

// TestTheDeferredTransitionSurvivesRunCancellation is PMR-95. The coordinator
// stops a run by cancelling the very context Start was given and only then
// cancelling the session (Coordinator.stopRun, and Shutdown), so by the time the
// turn-ended finalizer runs, the run context is already done. A finalizer that
// inherits it cannot issue the transition at all, and the issue stays in Merging
// holding a capacity slot with nothing scheduled to move it.
//
// The cancellation order here is stopRun's, exactly: cancel the run context,
// then Cancel the session on a fresh one.
func TestTheDeferredTransitionSurvivesRunCancellation(t *testing.T) {
	dir := t.TempDir()
	remote := newLandingRemote(t)
	remote.failingRequiredCheck()
	writeFakeGit(t, dir)
	script := writeAppServer(t, dir, landingScript(dir, "false", turnHangs))
	b := integratedBackend(func() config.Settings { return remote.settings() })
	runCtx, cancelRun := context.WithCancel(context.Background())
	session, events, err := b.Start(runCtx, landingRequest(remote, dir, script))
	if err != nil {
		t.Fatal(err)
	}
	waitForLanding(t, dir)
	cancelRun()
	if err := b.Cancel(context.Background(), session); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	drainLanding(t, events)
	transitions, comments, _ := remote.observed()
	if len(transitions) != 1 || transitions[0] != "In Review" {
		t.Fatalf("Linear transitions=%v, want exactly one to In Review after a cancelled run", transitions)
	}
	if len(comments) != 1 {
		t.Fatalf("Linear comments=%v, want exactly one", comments)
	}
}

// TestATimedOutTurnFinalizesOnlyWhenTheRunIsStopped is this transport's turn
// timeout, and it is a separate test because the timeout is not a turn end here.
// client.turn's timer emits a terminal failure and kills the child; it does not
// finalize. So the timeout leaves the deferred transition owed, and the Cancel
// the coordinator issues for that terminal event is what pays it -- against a
// session whose process is already gone, which is the state no other case here
// produces.
//
// Both halves are asserted, because only the pair says where the transition came
// from: nothing after the timeout, everything after the Cancel. The bound is far
// larger than it needs to be so that the child's landing call -- a git child plus
// several real HTTP round trips, which a loaded machine can stretch by an order of
// magnitude -- cannot still be in flight when the timer fires. If it ever is,
// waitForLanding fails loudly rather than letting this pass vacuously.
func TestATimedOutTurnFinalizesOnlyWhenTheRunIsStopped(t *testing.T) {
	dir := t.TempDir()
	remote := newLandingRemote(t)
	remote.failingRequiredCheck()
	writeFakeGit(t, dir)
	script := writeAppServer(t, dir, landingScript(dir, "false", turnHangs))
	r := landingRequest(remote, dir, script)
	r.TurnTimeout = 10 * time.Second
	b := integratedBackend(func() config.Settings { return remote.settings() })
	session, events, err := b.Start(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	waitForLanding(t, dir)

	// The stream closes when the turn timeout fires, so draining it is how this
	// waits for the timeout rather than for a clock.
	collected := drainLanding(t, events)
	if len(collected) == 0 || collected[len(collected)-1].Kind != domain.EventFailed {
		t.Fatalf("the turn timeout did not end the turn: %v", collected)
	}
	if transitions, comments, _ := remote.observed(); len(transitions) != 0 || len(comments) != 0 {
		t.Fatalf("the timeout itself finalized: transitions=%v comments=%v", transitions, comments)
	}

	settle(t, b, session)
	transitions, comments, merges := remote.observed()
	if len(transitions) != 1 || transitions[0] != "In Review" {
		t.Fatalf("Linear transitions=%v, want exactly one to In Review", transitions)
	}
	if len(comments) != 1 || !strings.Contains(comments[0], "required checks failed") {
		t.Fatalf("Linear comments=%v, want exactly one naming the failed gate", comments)
	}
	if merges != 0 {
		t.Fatalf("a refused landing merged %d times", merges)
	}
}

// TestATurnEndAfterAGateThenAMergeLeavesTheIssueDone is the negative the landed
// guard actually exists for, and the one the resolved-landing case above cannot
// reach: there, no gate is ever hit, so the finalizer short-circuits on
// retryableGateHit and never reads landed at all.
//
// Here the gate is hit, the fix turn's retry merges, and the turn ends with both
// facts on record. Getting it wrong is the worst outcome in this area: a merged,
// Done issue walked back to In Review carrying a comment that says landing fix
// attempts were exhausted.
func TestATurnEndAfterAGateThenAMergeLeavesTheIssueDone(t *testing.T) {
	dir := t.TempDir()
	remote := newLandingRemote(t)
	remote.retryableGateThenReady()
	writeFakeGit(t, dir)
	// Two landing calls in one turn: the gate, then the retry that merges.
	script := writeAppServer(t, dir, landingScript(dir, "false", landRetry(dir, "true", turnCompleted)))
	b := integratedBackend(func() config.Settings { return remote.settings() })
	session, events, err := b.Start(context.Background(), landingRequest(remote, dir, script))
	if err != nil {
		t.Fatal(err)
	}
	drainLanding(t, events)
	settle(t, b, session)
	transitions, comments, merges := remote.observed()
	if merges != 1 {
		t.Fatalf("merges=%d, want exactly one", merges)
	}
	if len(transitions) != 1 || transitions[0] != "Done" {
		t.Fatalf("Linear transitions=%v, want exactly one to Done", transitions)
	}
	if len(comments) != 0 {
		t.Fatalf("a merged landing was commented on as a refusal: %v", comments)
	}
}
