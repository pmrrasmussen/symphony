package capability

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/domain"
	githubhost "github.com/pmrrasmussen/symphony/internal/github"
	"github.com/pmrrasmussen/symphony/internal/linear"
)

// fakeProviderAPI serves the two Linear queries a handoff preparation makes, and
// nothing else. Both providers are pointed at it, so a preparation that reached
// GitHub for anything would fail loudly here rather than silently.
func fakeProviderAPI(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var query struct {
			Query string `json:"query"`
		}
		if err := json.NewDecoder(r.Body).Decode(&query); err != nil {
			t.Error(err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(query.Query, "SymphonyLinearHandoffIssue"):
			_, _ = w.Write([]byte(`{"data":{"issue":{"id":"issue-1","identifier":"PMR-182","title":"Hoist","description":"safe",` +
				`"url":"https://linear.app/issue/x","project":{"id":"project-uuid","slugId":"project-1"},` +
				`"team":{"id":"team-1"},"state":{"id":"merging","name":"Merging"}}}}`))
		case strings.Contains(query.Query, "SymphonyLinearHandoffStates"):
			_, _ = w.Write([]byte(`{"data":{"team":{"id":"team-1","states":{"nodes":[{"id":"review","name":"In Review"}]}}}}`))
		default:
			t.Errorf("unexpected query: %s", query.Query)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// boundSettings are settings that bind both providers: a Linear handoff state and
// a resolvable GitHub integration, with Merging as the state that grants the
// landing tool.
func boundSettings(endpoint string) config.Settings {
	return config.Settings{
		Tracker: config.Tracker{
			Provider:     map[string]any{"api_key": "linear-api-secret", "project_slug_id": "project-1", "endpoint": endpoint},
			ActiveStates: []string{"In Progress", "Merging"},
			HandoffState: "In Review",
		},
		GitHub: config.GitHub{Enabled: true, Owner: "owner", Repository: "repo", BaseBranch: "main",
			Token: "github-token-secret", Endpoint: endpoint, MergeState: "Merging", MergeMethod: "merge"},
	}
}

// TestPrepareBindsTheGivenProvidersIntoOneSessionsCapabilities is the behavioural
// half of the provider-ownership seam: the providers handed to the constructor,
// not providers anything built for itself, are what decide which capabilities a
// dispatch has and which credentials its child is stripped of.
//
// Both halves are asserted from the one returned value, because they are one
// preparation: a provider that grants a capability also holds a credential that
// must not reach the child, and a preparation that bound one without the other is
// exactly the divergence Bindings exists to prevent.
func TestPrepareBindsTheGivenProvidersIntoOneSessionsCapabilities(t *testing.T) {
	api := fakeProviderAPI(t)
	settings := boundSettings(api.URL)
	snapshot := func() config.Settings { return settings }
	preparer := NewPreparer(linear.NewHandoff(snapshot), githubhost.New(snapshot, nil))

	carried, err := preparer.Prepare(context.Background(), settings,
		domain.Issue{ID: "issue-1", Identifier: "PMR-182", State: "Merging"}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := From(domain.AgentRequest{Capabilities: carried})
	if err != nil {
		t.Fatal(err)
	}
	// The GitHub manager reached Build (every GitHub tool a landing dispatch is
	// served, land included for the configured Merging state) and so did the Linear
	// handoff: without a prepared handoff no GitHub session is prepared at all.
	advertised := map[string]bool{}
	for _, definition := range prepared.Registry().Definitions() {
		advertised[definition.Name] = true
	}
	for _, name := range []string{NameGitHubRefreshBaseRef, NameGitHubPRContext, NameGitHubLandPR} {
		if !advertised[name] {
			t.Fatalf("the bound providers did not advertise %s: %v", name, advertised)
		}
	}
	// Publish is the one GitHub capability a Merging dispatch is deliberately not
	// told about, so that landing is its only delivery (PMR-169).
	if advertised[NameGitHubPublishPR] {
		t.Fatalf("a landing dispatch advertised %s: %v", NameGitHubPublishPR, advertised)
	}
	// Follow-up issue creation is off in these settings, so the capability is
	// bound and unadvertised: advertisement is the settings' decision, not the
	// preparation's.
	if advertised[NameCreateFollowupIssue] {
		t.Fatal("create_followup_issue was advertised with followup_issue_creation off")
	}
	if _, ok := prepared.Registry().Lookup(NameCreateFollowupIssue); !ok {
		t.Fatal("the prepared handoff bound no create_followup_issue capability")
	}
	matcher := prepared.SecretMatcher()
	if matcher == nil {
		t.Fatal("bound providers produced no secret matcher, so their credentials would reach the child")
	}
	for _, credential := range []string{"linear-api-secret", "github-token-secret"} {
		if !matcher("prefix-" + credential + "-suffix") {
			t.Fatalf("the matcher does not recognize %s, which a child could then inherit", credential)
		}
	}
	if matcher("ordinary-value") {
		t.Fatal("the matcher matches unrelated values")
	}
}

// TestPrepareBindsNothingWithoutProviders pins the unwired case: a preparation
// with no providers is not an error, it is a dispatch with no bounded capability
// and no provider filter -- exactly what an unconfigured integration produces.
func TestPrepareBindsNothingWithoutProviders(t *testing.T) {
	carried, err := NewPreparer(nil, nil).Prepare(context.Background(), config.Settings{},
		domain.Issue{ID: "issue-1", Identifier: "PMR-182", State: "In Progress"}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := From(domain.AgentRequest{Capabilities: carried})
	if err != nil {
		t.Fatal(err)
	}
	if definitions := prepared.Registry().Definitions(); len(definitions) != 0 {
		t.Fatalf("an unwired preparation advertised %v", definitions)
	}
	if prepared.SecretMatcher() != nil {
		t.Fatal("an unwired preparation produced a provider filter")
	}
}

// TestGitHubManagerReportsTheInstanceBoundIntoEverySession is the identity half
// of the same seam. The host reads its poll loop and landing-verifier target back
// out of the preparation, so the instance it polls cannot drift from the instance
// sessions write into: a second manager would own a second linked-pull-request
// table, and a merged pull request would leave its issue unreconciled.
func TestGitHubManagerReportsTheInstanceBoundIntoEverySession(t *testing.T) {
	settings := func() config.Settings { return config.Settings{} }
	manager := githubhost.New(settings, nil)
	if got := NewPreparer(linear.NewHandoff(settings), manager).GitHubManager(); got != manager {
		t.Fatal("the preparation reported a GitHub manager other than the one it binds")
	}
	// A nil provider stays nil: an unconfigured integration must not be
	// implicitly constructed, because Prepare treats a bound manager as consent
	// to prepare a GitHub session.
	if got := NewPreparer(linear.NewHandoff(settings), nil).GitHubManager(); got != nil {
		t.Fatal("the Linear-only preparation minted a GitHub manager")
	}
}

// TestFromNarrowsWhatTheRequestCarries covers the one type assertion in this
// seam, in all three of its cases. domain.AgentRequest cannot name *Session
// without closing an import cycle, so this is what stands in for the compiler --
// and the refusal is what keeps a wiring mistake from quietly starting a session
// with no capabilities against a prompt that promises them.
func TestFromNarrowsWhatTheRequestCarries(t *testing.T) {
	prepared := &Session{registry: Build(Bindings{}), secrets: func(string) bool { return true }}
	got, err := From(domain.AgentRequest{Capabilities: prepared})
	if err != nil || got != prepared {
		t.Fatalf("From returned %v, %v, want the prepared session", got, err)
	}

	// Nothing carried is the unprepared request a backend started outside the
	// scheduler holds. It must read as "no capability, no matcher" rather than
	// panic, because both accessors are consulted unconditionally.
	empty, err := From(domain.AgentRequest{})
	if err != nil {
		t.Fatalf("an unprepared request was refused: %v", err)
	}
	if empty.Registry() != nil || empty.SecretMatcher() != nil {
		t.Fatal("an unprepared request produced capabilities or a provider filter")
	}
	if definitions := empty.Registry().Definitions(); len(definitions) != 0 {
		t.Fatalf("a nil registry advertised %v", definitions)
	}
	empty.Registry().TurnEnded(context.Background())

	if _, err := From(domain.AgentRequest{Capabilities: "not a session"}); err == nil {
		t.Fatal("a request carrying something other than a prepared session was accepted")
	}
}

// TestPreparationRefusesAPromiseTheSessionCannotKeep is the delivery-promise
// cross-check, in the place it belongs now that one snapshot decides both halves.
//
// It replaced the launch-time guard's first two refusals (PMR-182). Those
// compared a settings snapshot against a registry built from a later one, which
// is a comparison a hoisted preparation makes unnecessary -- but the divergence
// itself survives the hoist, because whether a GitHub session exists depends on
// the bound issue and not only on configuration, and because a hand-assembled
// Settings can advertise publish with nowhere to hand off to.
//
// The rows are the ones the claude guard's table asserted, driven here through
// the registry rather than through a list of names.
func TestPreparationRefusesAPromiseTheSessionCannotKeep(t *testing.T) {
	bound := config.Settings{
		Tracker: config.Tracker{HandoffState: "In Review"},
		GitHub:  config.GitHub{Enabled: true},
	}
	issue := domain.Issue{ID: "issue-1", Identifier: "PMR-182", State: "In Progress"}
	// landing is the same configuration bound to an issue sitting in the merge
	// state, which is the one dispatch shape that is promised the land tool and
	// deliberately advertised no publish tool (PMR-169).
	landing := bound
	landing.GitHub.MergeState = "Merging"
	landingIssue := domain.Issue{ID: "issue-1", Identifier: "PMR-182", State: "Merging"}
	for name, tc := range map[string]struct {
		settings config.Settings
		issue    domain.Issue
		github   bool
		want     string
	}{
		"a fully bound session is accepted":                {settings: bound, github: true},
		"a manual run with nothing advertised is accepted": {settings: config.Settings{}},
		"a follow-up-only session is accepted": {
			settings: config.Settings{Tracker: config.Tracker{FollowupIssueCreation: true}},
		},
		// The settings term: this snapshot promises publish and the session serves
		// none, which is the degenerate-identifier route -- an issue whose
		// identifier has no branch-safe character gets no GitHub session at all.
		"settings promise publish with nothing advertised": {
			settings: bound, want: "advertises no " + NameGitHubPublishPR,
		},
		// The reverse direction: publish reachable with no state to hand off to.
		// LinkAndHandoff mutates GitHub before it discovers there is no target.
		"publish advertised with no handoff state": {
			settings: config.Settings{GitHub: config.GitHub{Enabled: true}}, github: true,
			want: "no tracker.provider.handoff_state",
		},
		// A whitespace-only handoff state promises nothing and prepares nothing, so
		// it must not be refused: without the TrimSpace in HostSidePublishPromised
		// every launch would refuse here, with retry and backoff.
		"a whitespace handoff state neither promises nor refuses": {
			settings: config.Settings{GitHub: config.GitHub{Enabled: true}, Tracker: config.Tracker{HandoffState: "   "}},
		},
		// The landing term. A landing dispatch advertises no publish capability by
		// design, so the publish row above must not fire on it -- and the tool it
		// *is* promised must be there, or the run is told merging is the whole job
		// while holding nothing that can merge.
		"a landing dispatch that advertises land is accepted": {
			settings: landing, issue: landingIssue, github: true,
		},
		"a landing dispatch with nothing advertised": {
			settings: landing, issue: landingIssue,
			want: "advertises no " + NameGitHubLandPR,
		},
	} {
		t.Run(name, func(t *testing.T) {
			boundIssue := issue
			if tc.issue.ID != "" {
				boundIssue = tc.issue
			}
			b := Bindings{Settings: tc.settings, Issue: boundIssue}
			if tc.settings.LinearSessionCapabilityEnabled() || tc.github {
				b.Handoff = &linear.HandoffSession{}
			}
			if tc.github {
				b.GitHub = &githubhost.Session{}
			}
			err := verifyPromise(tc.settings, boundIssue, Build(b))
			if tc.want == "" {
				if err != nil {
					t.Fatalf("a consistent session was refused: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("a divergent session was accepted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("refusal %q does not report %q", err, tc.want)
			}
		})
	}
}

// TestPrepareRefusesRatherThanReturningASessionThatCannotPublish drives the same
// refusal through the whole preparation, on the divergence that reaches it
// without a hand-built binding: an issue whose identifier holds no branch-safe
// character has no deterministic branch, so github.Manager prepares no session
// for it while these settings still promise host-side publish.
//
// Without the refusal the dispatch starts, the model finds no publish tool, and
// the run ends completed with committed, unpublished work -- every gate green.
func TestPrepareRefusesRatherThanReturningASessionThatCannotPublish(t *testing.T) {
	api := fakeProviderAPI(t)
	settings := boundSettings(api.URL)
	snapshot := func() config.Settings { return settings }
	if !settings.HostSidePublishPromised() {
		t.Fatal("these settings do not promise host-side publish, so the refusal is not under test")
	}
	preparer := NewPreparer(linear.NewHandoff(snapshot), githubhost.New(snapshot, nil))

	carried, err := preparer.Prepare(context.Background(), settings,
		domain.Issue{ID: "issue-1", Identifier: "###", State: "In Progress"}, t.TempDir())
	if err == nil {
		t.Fatalf("a session that cannot publish was prepared anyway: %+v", carried)
	}
	if !strings.Contains(err.Error(), NameGitHubPublishPR) {
		t.Fatalf("the refusal does not name the missing capability: %v", err)
	}
	if carried != nil {
		t.Fatal("a refused preparation still returned capabilities")
	}
}
