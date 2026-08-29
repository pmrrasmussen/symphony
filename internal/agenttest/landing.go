// Package agenttest holds the test support Symphony's two agent backends share:
// the fake Linear/GitHub boundary a landing session runs against, the shared
// host-side landing suite each backend drives through a fixture of its own, and
// the fake timer both backends' timing seams accept.
//
// It exists because the behaviour under test in all of that is host-side and
// backend-neutral. A deferred Merging -> In Review transition is issued by
// internal/github and internal/capability, and what genuinely differs per
// backend is only *when* a turn end reaches the finalizer -- Codex from
// client.finalizeLanding, Claude through capability-endpoint registration
// retirement. Asserting the shared half twice, against two hand-built fakes, is
// what this package replaced.
//
// It is a normal package rather than a _test.go file because two packages import
// it, and nothing outside a test may: it takes *testing.T, so a non-test caller
// has nothing to hand it.
package agenttest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/domain"
)

// landingStates are the fixed Linear states this fixture's team has. Real IDs do
// not matter, only that they are stable and distinct.
var landingStates = map[string]string{
	"Merging": "state-merging", "In Review": "state-in-review", "Done": "state-done",
}

// LandingRemote is the pair of remotes a deferred landing transition touches,
// served by one httptest server: the GitHub REST/GraphQL API whose required
// check decides whether the landing gate is retryable, and the Linear GraphQL
// API the deferred transition and its comment are issued against. The Linear
// side is stateful because RefuseLanding re-reads the issue after transitioning
// it and refuses to claim success unless the state really moved.
//
// Nothing here substitutes a capability, a registry, or a provider: a backend
// under test builds its own registry from a real linear.Handoff and a real
// github.Manager, and the only stand-ins are these two remotes, the agent child,
// and a scripted git (see WriteFakeGit).
type LandingRemote struct {
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
	// landFixOff turns the bounded-fix feature off, which makes the same failing
	// required check an immediate refusal instead of a retryable gate.
	landFixOff bool
	// mergeReached and mergeHold park the merge request where a test can hold
	// it. See PauseAtMerge.
	mergeReached chan struct{}
	mergeHold    chan struct{}
}

// NewLandingRemote returns a fixture whose required check has already failed:
// with the bounded-fix feature on and an attempt remaining, that is the one
// state in which a turn ending has a deferred transition left to perform.
func NewLandingRemote(t *testing.T) *LandingRemote {
	t.Helper()
	f := &LandingRemote{t: t, state: "Merging",
		checkRuns: []map[string]any{{"name": "ci/build", "status": "completed", "conclusion": "failure"}}}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}

// RetryableGateThenReady is the sequence the deferred fallback's landed guard
// exists for, and the only state in which that guard is what decides the outcome:
// the first landing attempt hits the retryable gate, and the fix turn's retry
// finds the check green and merges. The turn then ends with a gate on record and
// a merged pull request.
func (f *LandingRemote) RetryableGateThenReady() {
	f.mu.Lock()
	f.checkRunsAfterFirstRead = []map[string]any{{"name": "ci/build", "status": "completed", "conclusion": "success"}}
	f.reviews = []any{map[string]any{"user": map[string]any{"login": "alice"}, "state": "APPROVED", "body": "lgtm", "submitted_at": "t1"}}
	f.mergeable = true
	f.mu.Unlock()
}

// ReadyToLand makes every landing gate pass, so the landing merges and resolves.
func (f *LandingRemote) ReadyToLand() {
	f.mu.Lock()
	f.checkRuns = []map[string]any{{"name": "ci/build", "status": "completed", "conclusion": "success"}}
	f.reviews = []any{map[string]any{"user": map[string]any{"login": "alice"}, "state": "APPROVED", "body": "lgtm", "submitted_at": "t1"}}
	f.mergeable = true
	f.mu.Unlock()
}

// PauseAtMerge parks the landing inside its merge request until the returned
// release is called, and closes the returned channel when it gets there. It is
// what makes "the turn budget expired while the landing was mid-merge" an exact
// ordering rather than a likely one: a test can hold the invocation in flight,
// end the turn underneath it, and let the merge succeed only afterwards.
//
// It must be called before the session starts, and it parks exactly one merge:
// a landing that reaches the merge twice would be a different fixture's case.
func (f *LandingRemote) PauseAtMerge() (reached <-chan struct{}, release func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mergeReached, f.mergeHold = make(chan struct{}), make(chan struct{})
	hold := f.mergeHold
	return f.mergeReached, sync.OnceFunc(func() { close(hold) })
}

// pauseLocked parks a merge request until the test releases it, when a test
// asked for that and nothing has claimed the pause yet. It is called with the
// fixture's mutex held and returns holding it again, because everything around
// the merge -- the test's own observations while the request is parked, and the
// transition the landing issues once it resumes -- needs the fixture too.
func (f *LandingRemote) pauseLocked() {
	reached, hold := f.mergeReached, f.mergeHold
	f.mergeReached, f.mergeHold = nil, nil
	if hold == nil {
		return
	}
	f.mu.Unlock()
	close(reached)
	<-hold
	f.mu.Lock()
}

// DisableLandFix turns the bounded-fix feature off in the settings this fixture
// serves, which makes the failing required check an immediate refusal rather
// than a retryable gate.
func (f *LandingRemote) DisableLandFix() {
	f.mu.Lock()
	f.landFixOff = true
	f.mu.Unlock()
}

// Observed reports everything the two remotes were told, in order.
func (f *LandingRemote) Observed() (transitions, comments []string, merges int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.transitions...), append([]string(nil), f.comments...), f.merges
}

// RequireDeferredRefusal fails unless the deferred fallback ran exactly once and
// did what it exists to do: one transition to the configured handoff state, one
// comment naming the gate that deferred it, and no merge. Exactly once matters as
// much as at least once -- two finalizer runs are two attempts at the same
// transition against a live tracker.
func (f *LandingRemote) RequireDeferredRefusal(t *testing.T) {
	t.Helper()
	transitions, comments, merges := f.Observed()
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

// Settings is the whole configuration the landing path reads: an active Merging
// issue, the configured refuse_landing edge the fallback needs, and the
// bounded-fix feature that makes a failing required check retryable.
func (f *LandingRemote) Settings() config.Settings {
	f.mu.Lock()
	landFixEnabled := !f.landFixOff
	f.mu.Unlock()
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
			LandFixEnabled: landFixEnabled, MaxLandAttempts: 2,
		},
	}
}

// SettingsFunc is Settings as the snapshot callback a backend is built from.
func (f *LandingRemote) SettingsFunc() func() config.Settings {
	return func() config.Settings { return f.Settings() }
}

// Issue is the Merging issue every landing session here is bound to.
func (f *LandingRemote) Issue() domain.Issue {
	return domain.Issue{ID: "issue-27", Identifier: "PMR-27", Title: "Landing", State: "Merging", URL: "https://linear.app/issue/PMR-27"}
}

func (f *LandingRemote) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if strings.HasPrefix(r.URL.Path, "/repos/") {
		f.serveGitHub(w, r)
		return
	}
	f.serveLinear(w, r)
}

func (f *LandingRemote) serveGitHub(w http.ResponseWriter, r *http.Request) {
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
		f.pauseLocked()
		f.merges++
		f.merged = true
		f.write(w, map[string]any{"merged": true, "sha": "head", "message": "merged"})
	default:
		f.t.Errorf("unexpected GitHub request %s %s", r.Method, r.URL.String())
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *LandingRemote) pullJSON() map[string]any {
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

func (f *LandingRemote) serveLinear(w http.ResponseWriter, r *http.Request) {
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

func (f *LandingRemote) write(w http.ResponseWriter, payload any) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		f.t.Errorf("encode response: %v", err)
		return
	}
	_, _ = w.Write(encoded)
}

// WriteFakeGit puts a scripted git first on PATH. The landing path shells out to
// git before it can reach any gate, and internal/github's own git runner is
// package-private, so a test outside that package can only substitute the
// executable -- the same way a backend test substitutes the agent binary. The
// worktree it reports is the ordinary one landing expects: the configured
// repository as origin, nothing uncommitted, and a HEAD that already matches the
// published pull request head, so no push path is involved.
//
// It is a stub, not a fake worktree, and nothing here asserts anything about the
// git surface itself: it matches on the first two arguments only, ignores the
// directory it is run in, and answers the base-branch rev-parse with a constant,
// so the base-moved gate can never fire through it. internal/github owns the
// tests for what landing asks git and what it does with the answers.
func WriteFakeGit(t *testing.T, dir string) {
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
