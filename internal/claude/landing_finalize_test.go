package claude

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pmrrasmussen/symphony/internal/capability"
	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/domain"
	githubhost "github.com/pmrrasmussen/symphony/internal/github"
	"github.com/pmrrasmussen/symphony/internal/linear"
)

// This file covers the wiring from a turn ending to the deferred
// Merging -> In Review landing transition (PMR-46, PMR-78) on this transport:
// the fallback that returns an issue to human review when a turn ended after a
// retryable landing gate without a landing.
//
// The rest of this package's turn-end coverage substitutes the registry, which
// is what makes "the finalizer ran" observable at all. What that cannot say is
// whether running it does anything: the finalizer's whole job is a Linear
// transition, issued through a real capability registry, a real github.Session,
// and a real linear.HandoffSession. So nothing is substituted here except the two
// remotes (one httptest server), the CLI, and git -- and the child reaches the
// landing capability the only way a real one can, over loopback HTTP with the
// token it found in its own environment.

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
	// landFixOff turns the bounded-fix feature off, which makes the same failing
	// required check an immediate refusal instead of a retryable gate.
	landFixOff bool
}

// newLandingRemote returns a fixture whose required check has already failed:
// with the bounded-fix feature on and an attempt remaining, that is the one
// state in which a turn ending has a deferred transition left to perform.
func newLandingRemote(t *testing.T) *landingRemote {
	f := &landingRemote{t: t, state: "Merging",
		checkRuns: []map[string]any{{"name": "ci/build", "status": "completed", "conclusion": "failure"}}}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}

// retryableGateThenReady is the sequence the deferred fallback's landed guard
// exists for, and the only state in which that guard is what decides the outcome:
// the first landing attempt hits the retryable gate, and the fix turn's retry
// finds the check green and merges. The turn then ends with a gate on record and
// a merged pull request.
func (f *landingRemote) retryableGateThenReady() {
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
			LandFixEnabled: !f.landFixOff, MaxLandAttempts: 2,
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

// landingRegistry is the real registry a Merging session gets: the host's own
// providers, bound to the fixture's remotes. Build decides advertisement, so the
// returned names are what the launch contract and the init echo must agree on.
func landingRegistry(t *testing.T, f *landingRemote, workspace string) (*capability.Registry, []string) {
	t.Helper()
	settings := f.settings()
	snapshot := func() config.Settings { return settings }
	handoff, err := linear.NewHandoff(snapshot).PrepareWithSettings(context.Background(), settings, f.issue())
	if err != nil {
		t.Fatalf("prepare Linear handoff: %v", err)
	}
	session := githubhost.New(snapshot, nil).PrepareWithSettings(settings.GitHub, f.issue(), workspace, handoff)
	if session == nil {
		t.Fatal("the fixture's configuration produced no GitHub session")
	}
	registry := capability.Build(capability.Bindings{Settings: settings, Issue: f.issue(), Handoff: handoff, GitHub: session})
	names := advertisedNames(registry)
	if len(names) == 0 {
		t.Fatal("a Merging session advertised no capability")
	}
	return registry, names
}

// writeFakeGit puts a scripted git first on PATH. The landing path shells out to
// git before it can reach any gate, and internal/github's git runner is
// package-private, so a test outside that package can only substitute the
// executable -- the same way this package substitutes the CLI. The worktree it
// reports is the ordinary one landing expects: the configured repository as
// origin, nothing uncommitted, and a HEAD that already matches the published
// pull request head, so no push path is involved.
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

// landingInitLine is the init echo a Merging session must produce: the endpoint
// connected, and exactly the coding tools plus the advertised capabilities.
func landingInitLine(dir string, names []string) string {
	quoted := make([]string, 0, len(codingTools)+len(names))
	for _, tool := range append(append([]string(nil), codingTools...), prefixed(names)...) {
		quoted = append(quoted, `"`+tool+`"`)
	}
	return `{"type":"system","subtype":"init","session_id":"x","cwd":"` + workspaceOf(dir) +
		`","permissionMode":"dontAsk","tools":[` + strings.Join(quoted, ",") +
		`],"mcp_servers":[{"name":"` + mcpServerName + `","status":"connected"}]}`
}

// landingChildBody makes the fake CLI a real MCP client that calls the landing
// capability, then records that the call came back where a test can wait for it.
// The endpoint URL comes out of its own argument vector and the bearer token out
// of its own environment: the only two places a real client can find them.
func landingChildBody(dir string) string {
	at := func(name string) string { return filepath.Join(dir, name) }
	post := func(step, body string) string {
		return "printf '%s' '" + body + "' > " + at(step+".body") + "\n" +
			"curl -sS -X POST -H \"Authorization: Bearer $" + endpointTokenEnvName + "\"" +
			" -H 'Content-Type: application/json' --data-binary @" + at(step+".body") +
			" -o " + at(step+".json") + " -w '%{http_code}' \"$url\" > " + at(step+".status") + "\n"
	}
	return "url=$(grep -o 'http://[0-9.]*:[0-9]*/mcp' " + at("args.txt") + " | head -1)\n" +
		post("initialize", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"fake-claude","version":"1"}}}`) +
		post("land", `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"`+capability.NameGitHubLandPR+`","arguments":{}}}`) +
		"cp " + at("land.json") + " " + at("landed") + "\n"
}

// landRetryBody is a second github_land_pr call in the same turn, which is what a
// fix turn does after a retryable gate. It is appended to landingChildBody, so the
// marker landingChildBody writes is overwritten by this call's result and
// waitForLanding waits for the retry rather than for the gate.
func landRetryBody(dir string) string {
	at := func(name string) string { return filepath.Join(dir, name) }
	return "printf '%s' '" + `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"` +
		capability.NameGitHubLandPR + `","arguments":{}}}` + "' > " + at("retry.body") + "\n" +
		"curl -sS -X POST -H \"Authorization: Bearer $" + endpointTokenEnvName + "\"" +
		" -H 'Content-Type: application/json' --data-binary @" + at("retry.body") +
		" -o " + at("retry.json") + " -w '%{http_code}' \"$url\" > " + at("retry.status") + "\n" +
		"cp " + at("retry.json") + " " + at("landed") + "\n"
}

// waitForLanding blocks until the child's github_land_pr call has come back,
// which is what puts the session in the only state a turn end has work to do in.
func waitForLanding(t *testing.T, dir string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		if body, err := os.ReadFile(filepath.Join(dir, "landed")); err == nil && len(body) > 0 {
			// A JSON-RPC result rather than a transport error is what says the
			// child really reached the capability: a refused landing is an
			// isError result, not a protocol failure.
			if !strings.Contains(string(body), `"result"`) {
				t.Fatalf("the child's landing call did not reach the capability: %s", body)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the child never completed a github_land_pr call")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// drainLanding drains a turn's stream with more patience than this package's
// shared drain. A turn timeout kills the child while the landing call it made
// may still be in flight, and the revocation that follows drains that call and
// then runs the finalizer before the stream closes -- three real round trips
// after the kill. Twenty seconds is enough for that on an idle machine and not
// on a loaded one, and a patience that expires is a false failure: what these
// tests assert is what the tracker was told, never how quickly.
//
// It is not much more patience, though. A wedge reported as a two-minute stall is
// a worse diagnostic than the same wedge reported in seconds, so this is roughly
// twice the worst observed drain-then-finalize rather than an order of magnitude
// above it.
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

func requireCurl(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl is needed for the child to reach the capability endpoint as a real MCP client")
	}
}

// landingSession wires a backend to a real endpoint and starts one turn against
// the real registry, returning everything a test needs to end that turn.
func landingSession(t *testing.T, ctx context.Context, f *landingRemote, dir, ending string, turnTimeout time.Duration) (*Backend, domain.AgentSession, <-chan domain.Event) {
	t.Helper()
	return landingSessionWith(t, ctx, f, dir, landingChildBody(dir), ending, turnTimeout)
}

// landingSessionWith is landingSession with the child's MCP work spelled out, for
// the one case that needs two landing calls in a turn.
func landingSessionWith(t *testing.T, ctx context.Context, f *landingRemote, dir, body, ending string, turnTimeout time.Duration) (*Backend, domain.AgentSession, <-chan domain.Event) {
	t.Helper()
	writeFakeGit(t, dir)
	registry, names := landingRegistry(t, f, workspaceOf(dir))
	script := writeFakeClaude(t, dir, body+
		"cat <<'EOF'\n"+landingInitLine(dir, names)+"\nEOF\n"+ending)
	backend, _ := backendWithEndpoint(t)
	r := request(t, dir, script)
	r.Issue = f.issue()
	if turnTimeout > 0 {
		r.TurnTimeout = turnTimeout
	}
	session, events, err := startWithRegistry(t, backend, ctx, r, registry)
	if err != nil {
		t.Fatal(err)
	}
	return backend, session, events
}

// TestEveryTurnEndPathFiresTheDeferredLandingTransition is the coverage PMR-86
// exists for on this transport. A turn that hit a retryable landing gate and then
// ended without landing must return the issue to human review, and every way that
// turn can end has to do it -- otherwise the issue sits in Merging holding a
// state-aware capacity slot with nothing scheduled to move it.
//
// Exactly once matters as much as at least once: two finalizer runs are two
// attempts at the same transition against a live tracker.
//
// The case set is this transport's own, and it is deliberately not the Codex
// file's: a --print turn ends by closing its stream, so there is no protocol
// notification to enumerate, and the turn timeout genuinely is a turn end here
// (the turn's own shutdown retires the endpoint) where on Codex it is only a kill.
// A hard Cancel is the one ending both transports share.
func TestEveryTurnEndPathFiresTheDeferredLandingTransition(t *testing.T) {
	requireCurl(t)
	for name, test := range map[string]struct {
		ending  string
		timeout time.Duration
		cancel  bool
	}{
		"completion": {ending: "cat <<'EOF'\n" + resultLine(false, "") + "\nEOF\n"},
		"failure":    {ending: "cat <<'EOF'\n" + resultLine(true, `"terminal_reason":"api_error"`) + "\nEOF\n"},
		// The child holds the turn open past its own bound. The bound is
		// deliberately far larger than it needs to be: the child's landing call
		// has to finish before the timeout fires -- a timeout that fired
		// mid-call would leave no gate hit and nothing to assert -- and that
		// call is a git child plus several real HTTP round trips, which a loaded
		// machine can stretch by an order of magnitude. It fails loudly rather
		// than vacuously if ten seconds is ever not enough: waitForLanding says
		// the call never came back.
		"turn timeout": {ending: "sleep 120\n", timeout: 10 * time.Second},
		"hard cancel":  {ending: "sleep 120\n", cancel: true},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			remote := newLandingRemote(t)
			backend, session, events := landingSession(t, context.Background(), remote, dir, test.ending, test.timeout)
			waitForLanding(t, dir)
			if test.cancel {
				// A returned Cancel has already finalized: the coordinator has
				// no other handle on the session left afterwards.
				if err := backend.Cancel(context.Background(), session); err != nil {
					t.Fatalf("cancel: %v", err)
				}
				transitions, _, _ := remote.observed()
				if len(transitions) != 1 || transitions[0] != "In Review" {
					t.Fatalf("cancel returned with Linear transitions=%v, want exactly one to In Review", transitions)
				}
			}
			drainLanding(t, events)
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
	requireCurl(t)
	dir := t.TempDir()
	remote := newLandingRemote(t)
	remote.readyToLand()
	_, _, events := landingSession(t, context.Background(), remote, dir, "cat <<'EOF'\n"+resultLine(false, "")+"\nEOF\n", 0)
	waitForLanding(t, dir)
	drainLanding(t, events)
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
	requireCurl(t)
	dir := t.TempDir()
	remote := newLandingRemote(t)
	remote.landFixOff = true
	_, _, events := landingSession(t, context.Background(), remote, dir, "cat <<'EOF'\n"+resultLine(false, "")+"\nEOF\n", 0)
	waitForLanding(t, dir)
	drainLanding(t, events)
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
// turn-end habit: a session that never called the landing capability must leave
// the tracker exactly as it found it. It needs no MCP client for that reason.
func TestATurnEndWithoutALandingAttemptTransitionsNothing(t *testing.T) {
	dir := t.TempDir()
	remote := newLandingRemote(t)
	writeFakeGit(t, dir)
	registry, names := landingRegistry(t, remote, workspaceOf(dir))
	script := writeFakeClaude(t, dir, "cat <<'EOF'\n"+landingInitLine(dir, names)+"\n"+resultLine(false, "")+"\nEOF\n")
	backend, _ := backendWithEndpoint(t)
	r := request(t, dir, script)
	r.Issue = remote.issue()
	_, events, err := startWithRegistry(t, backend, context.Background(), r, registry)
	if err != nil {
		t.Fatal(err)
	}
	if lastKind(t, drainLanding(t, events)).Kind != domain.EventCompleted {
		t.Fatal("the turn did not complete")
	}
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
// then Cancel the session on a fresh one. Every other test in this package
// starts a session on context.Background(), which is never cancelled, so none of
// them can observe this at all.
func TestTheDeferredTransitionSurvivesRunCancellation(t *testing.T) {
	requireCurl(t)
	dir := t.TempDir()
	remote := newLandingRemote(t)
	runCtx, cancelRun := context.WithCancel(context.Background())
	backend, session, events := landingSession(t, runCtx, remote, dir, "sleep 120\n", 0)
	waitForLanding(t, dir)
	cancelRun()
	if err := backend.Cancel(context.Background(), session); err != nil {
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
	requireCurl(t)
	dir := t.TempDir()
	remote := newLandingRemote(t)
	remote.retryableGateThenReady()
	_, _, events := landingSessionWith(t, context.Background(), remote, dir,
		landingChildBody(dir)+landRetryBody(dir),
		"cat <<'EOF'\n"+resultLine(false, "")+"\nEOF\n", 0)
	waitForLanding(t, dir)
	drainLanding(t, events)
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
