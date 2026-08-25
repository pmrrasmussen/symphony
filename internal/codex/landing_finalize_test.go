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
	reviews     []any
	mergeable   any
	merged      bool
	merges      int
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
		f.write(w, map[string]any{"check_runs": f.checkRuns})
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
// child to be reaped so a test's temporary directory outlives it. It doubles as
// an idempotency assertion: Cancel is another turn-end path, and every count this
// file checks is "exactly one" after it has also run.
func settle(t *testing.T, b *Backend, session domain.AgentSession) {
	t.Helper()
	if err := b.Cancel(context.Background(), session); err != nil {
		t.Fatalf("cancel: %v", err)
	}
}

// TestEveryTurnEndPathFiresTheDeferredLandingTransition is the coverage PMR-86
// exists for. A turn that hit a retryable landing gate and then ended without
// landing must return the issue to human review, and every way that turn can end
// has to do it -- otherwise the issue sits in Merging holding a state-aware
// capacity slot with nothing scheduled to move it.
//
// The turn timeout is deliberately expressed as "timeout, then the coordinator's
// Cancel". On this transport the timeout itself only kills the child (see
// client.turn); the transition comes from the Cancel the coordinator issues for
// the terminal event that produces, and that is the path this asserts.
func TestEveryTurnEndPathFiresTheDeferredLandingTransition(t *testing.T) {
	for name, test := range map[string]struct {
		ending  string
		timeout time.Duration
		cancel  bool
	}{
		"turn/completed": {ending: turnCompleted},
		"turn/failed":    {ending: turnFailed},
		"turn/cancelled": {ending: turnCancelled},
		"hard cancel":    {ending: turnHangs, cancel: true},
		// The bound is generous because the landing call the child makes
		// first is a real HTTP round trip against two remotes and a git
		// child: a timeout that could fire mid-call would test nothing.
		"turn timeout": {ending: turnHangs, timeout: 3 * time.Second, cancel: true},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			remote := newLandingRemote(t)
			remote.failingRequiredCheck()
			writeFakeGit(t, dir)
			script := writeAppServer(t, dir, landingScript(dir, "false", test.ending))
			r := landingRequest(remote, dir, script)
			if test.timeout > 0 {
				r.TurnTimeout = test.timeout
			}
			b := integratedBackend(func() config.Settings { return remote.settings() })
			session, events, err := b.Start(context.Background(), r)
			if err != nil {
				t.Fatal(err)
			}
			waitForLanding(t, dir)
			if test.cancel {
				// Cancel is what the coordinator calls for a run it is stopping,
				// whether the turn hung or timed out. It must have finalized the
				// landing by the time it returns: the coordinator has no other
				// handle on the session left afterwards.
				settle(t, b, session)
			}
			for range events {
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
	for range events {
	}
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
	for range events {
	}
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
	for range events {
	}
	settle(t, b, session)
	transitions, comments, merges := remote.observed()
	if len(transitions) != 0 || len(comments) != 0 || merges != 0 {
		t.Fatalf("a turn that never attempted a landing produced transitions=%v comments=%v merges=%d", transitions, comments, merges)
	}
}
