package claude

import (
	"context"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pmrrasmussen/symphony/internal/agenttest"
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
// What is host-side about that is asserted once, in internal/agenttest's shared
// landing suite, which this file runs against a fixture of this transport's.
// What is left here is this transport's own finalize trigger: capability-endpoint
// registration retirement, which is what runs the finalizer, on each of the ways
// a --print turn can end.
//
// The rest of this package's turn-end coverage substitutes the registry, which
// is what makes "the finalizer ran" observable at all. What that cannot say is
// whether running it does anything: the finalizer's whole job is a Linear
// transition, issued through a real capability registry, a real github.Session,
// and a real linear.HandoffSession. So nothing is substituted here except the two
// remotes (one httptest server), the CLI, and git -- and the child reaches the
// landing capability the only way a real one can, over loopback HTTP with the
// token it found in its own environment.

// landingRegistry is the real registry a Merging session gets: the host's own
// providers, bound to the fixture's remotes. Build decides advertisement, so the
// returned names are what the launch contract and the init echo must agree on.
func landingRegistry(t *testing.T, f *agenttest.LandingRemote, workspace string) (*capability.Registry, []string) {
	t.Helper()
	settings := f.Settings()
	snapshot := func() config.Settings { return settings }
	handoff, err := linear.NewHandoff(snapshot).PrepareWithSettings(context.Background(), settings, f.Issue())
	if err != nil {
		t.Fatalf("prepare Linear handoff: %v", err)
	}
	session := githubhost.New(snapshot, nil).PrepareWithSettings(settings.GitHub, f.Issue(), workspace, handoff)
	if session == nil {
		t.Fatal("the fixture's configuration produced no GitHub session")
	}
	registry := capability.Build(capability.Bindings{Settings: settings, Issue: f.Issue(), Handoff: handoff, GitHub: session})
	names := advertisedNames(registry)
	if len(names) == 0 {
		t.Fatal("a Merging session advertised no capability")
	}
	return registry, names
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

// landingChildBody makes the fake CLI a real MCP client: it initializes, then
// calls the landing capability once per requested outcome, recording each answer
// where a test can wait for it. The endpoint URL comes out of its own argument
// vector and the bearer token out of its own environment: the only two places a
// real client can find them.
func landingChildBody(dir string, calls []bool) string {
	at := func(name string) string { return filepath.Join(dir, name) }
	post := func(step, body string) string {
		return "printf '%s' '" + body + "' > " + at(step+".body") + "\n" +
			"curl -sS -X POST -H \"Authorization: Bearer $" + endpointTokenEnvName + "\"" +
			" -H 'Content-Type: application/json' --data-binary @" + at(step+".body") +
			" -o " + at(step+".json") + " -w '%{http_code}' \"$url\" > " + at(step+".status") + "\n"
	}
	body := "url=$(grep -o 'http://[0-9.]*:[0-9]*/mcp' " + at("args.txt") + " | head -1)\n" +
		post("initialize", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"fake-claude","version":"1"}}}`)
	for i := range calls {
		step := "land-" + strconv.Itoa(i)
		body += post(step, `{"jsonrpc":"2.0","id":`+strconv.Itoa(2+i)+`,"method":"tools/call","params":{"name":"`+
			capability.NameGitHubLandPR+`","arguments":{}}}`) +
			"cp " + at(step+".json") + " " + landingMarker(dir, i) + "\n"
	}
	return body
}

// landingMarker is where the child records the answer to one landing call.
func landingMarker(dir string, call int) string {
	return filepath.Join(dir, "landed-"+strconv.Itoa(call))
}

// awaitLandingCalls blocks until every landing call the case asked for has come
// back with the outcome it asked for, which is what puts the session in the only
// state a turn end has work to do in.
//
// The outcome is asserted, not merely awaited: a JSON-RPC result rather than a
// transport error is what says the child really reached the capability -- a
// refused landing is an isError result, not a protocol failure -- and a case that
// meant to hit a gate and instead landed would otherwise run its whole assertion
// set against the wrong session.
func awaitLandingCalls(t *testing.T, dir string, calls []bool) {
	t.Helper()
	for i, wantSuccess := range calls {
		want := `"isError":` + strconv.FormatBool(!wantSuccess)
		agenttest.AwaitFile(t, landingMarker(dir, i),
			func(body string) bool { return strings.Contains(body, want) },
			"the child's landing call never came back with "+want)
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
func landingSession(t *testing.T, ctx context.Context, f *agenttest.LandingRemote, dir string, calls []bool, ending string) (*Backend, *agenttest.FakeTimer, domain.AgentSession, <-chan domain.Event) {
	t.Helper()
	agenttest.WriteFakeGit(t, dir)
	registry, names := landingRegistry(t, f, workspaceOf(dir))
	script := writeFakeClaude(t, dir, landingChildBody(dir, calls)+
		"cat <<'EOF'\n"+landingInitLine(dir, names)+"\nEOF\n"+ending)
	backend, _ := backendWithEndpoint(t)
	timer := timedBackend(t, backend)
	r := request(t, dir, script)
	r.Issue = f.Issue()
	// The budget is named here so a case can elapse exactly it. Its size is
	// irrelevant now that no test waits for it -- what matters is that it is the
	// one budget a landing turn schedules.
	r.TurnTimeout = landingTurnBudget
	session, events, err := startWithRegistry(t, backend, ctx, r, registry)
	if err != nil {
		t.Fatal(err)
	}
	return backend, timer, session, events
}

// turnCompleted and turnFailed are the two ways a --print turn ends by itself: a
// closing result line that is or is not an error. turnHangs is a child that never
// produces one, so only the turn budget or a cancellation can end it.
var (
	turnCompleted = "cat <<'EOF'\n" + resultLine(false, "") + "\nEOF\n"
	turnFailed    = "cat <<'EOF'\n" + resultLine(true, `"terminal_reason":"api_error"`) + "\nEOF\n"
)

const (
	turnHangs = "sleep 120\n"
	// landingTurnBudget is the turn budget every landing session here is given.
	landingTurnBudget = 90 * time.Second
)

// landingBackend is this transport's fixture for the shared landing suite.
type landingBackend struct{}

func (landingBackend) StartLandingSession(t *testing.T, ctx context.Context, remote *agenttest.LandingRemote, dir string, spec agenttest.LandingSpec) agenttest.LandingRun {
	t.Helper()
	if len(spec.Calls) > 0 {
		// A case whose child never calls the capability needs no MCP client, and
		// curl is how this fixture's child is one.
		requireCurl(t)
	}
	ending := turnCompleted
	if spec.Ending == agenttest.EndsOpen {
		ending = turnHangs
	}
	backend, _, session, events := landingSession(t, ctx, remote, dir, spec.Calls, ending)
	return agenttest.LandingRun{
		Events:            events,
		AwaitLandingCalls: func(t *testing.T) { awaitLandingCalls(t, dir, spec.Calls) },
		Stop: func(t *testing.T) {
			if err := backend.Cancel(context.Background(), session); err != nil {
				t.Fatalf("cancel: %v", err)
			}
		},
	}
}

// TestTheHostSideLandingBehaviourHoldsOverThePrintStream runs the shared landing
// suite against this transport. The suite is what asserts the host-side half
// once; this test is what says a real --print session reaches it.
func TestTheHostSideLandingBehaviourHoldsOverThePrintStream(t *testing.T) {
	agenttest.RunLandingSuite(t, landingBackend{})
}

// TestEveryTurnEndPathFiresTheDeferredLandingTransition is this transport's own
// finalize trigger, and the coverage PMR-86 exists for. Here the finalizer runs
// from registration retirement (see turn.stream's deferred retireEndpoint), so
// every way a turn can end has to reach that retirement -- otherwise the issue
// sits in Merging holding a state-aware capacity slot with nothing scheduled to
// move it.
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
		timeout bool
		cancel  bool
	}{
		"completion": {ending: turnCompleted},
		"failure":    {ending: turnFailed},
		// The child holds the turn open past its own budget, which the test
		// elapses once the landing call has come back. Elapsing it is what makes
		// that ordering exact: a timeout that fired mid-call would leave no gate
		// hit and nothing to assert, and the ten real seconds this used to allow
		// for made the ordering likely rather than certain.
		"turn timeout": {ending: turnHangs, timeout: true},
		"hard cancel":  {ending: turnHangs, cancel: true},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			remote := agenttest.NewLandingRemote(t)
			calls := []bool{false}
			backend, timer, session, events := landingSession(t, context.Background(), remote, dir, calls, test.ending)
			awaitLandingCalls(t, dir, calls)
			if test.timeout {
				timer.Elapse(t, landingTurnBudget)
			}
			if test.cancel {
				// A returned Cancel has already finalized: the coordinator has
				// no other handle on the session left afterwards.
				if err := backend.Cancel(context.Background(), session); err != nil {
					t.Fatalf("cancel: %v", err)
				}
				transitions, _, _ := remote.Observed()
				if len(transitions) != 1 || transitions[0] != "In Review" {
					t.Fatalf("cancel returned with Linear transitions=%v, want exactly one to In Review", transitions)
				}
			}
			collected := agenttest.DrainEvents(t, events)
			if test.timeout {
				failure := collected[len(collected)-1]
				if failure.Kind != domain.EventFailed || !strings.Contains(failure.Message, "timeout") {
					t.Fatalf("the elapsed turn budget did not end the turn as a timeout: %+v", failure)
				}
			}
			remote.RequireDeferredRefusal(t)
		})
	}
}

// timedBackend is a backend whose turn budgets elapse when this test says so
// rather than when a real clock says so. See claude.Timer and
// agenttest.FakeTimer.
func timedBackend(t *testing.T, b *Backend) *agenttest.FakeTimer {
	t.Helper()
	timer := agenttest.NewFakeTimer()
	b.timer = timer
	return timer
}
