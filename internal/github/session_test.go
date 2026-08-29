package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/domain"
)

// TestRefreshBaseRefFetchesTheConfiguredBaseBranch asserts the exact refspec
// addWorktree already uses, driven by github.base_branch rather than a
// literal "main", and that the resolved commit reaches the caller.
func TestRefreshBaseRefFetchesTheConfiguredBaseBranch(t *testing.T) {
	git := &fakeGit{}
	s := refreshBaseRefSession(t, git, "develop")
	commit, err := s.RefreshBaseRef(context.Background())
	if err != nil {
		t.Fatalf("RefreshBaseRef returned %v", err)
	}
	if commit != "base" {
		t.Fatalf("resolved base commit = %q, want %q", commit, "base")
	}
	if len(git.calls) != 2 {
		t.Fatalf("git calls = %#v, want exactly a fetch then a rev-parse", git.calls)
	}
	wantFetch := []string{"fetch", "--no-tags", "origin", "+refs/heads/develop:refs/remotes/origin/develop"}
	if strings.Join(git.calls[0], " ") != strings.Join(wantFetch, " ") {
		t.Fatalf("fetch call = %#v, want %#v", git.calls[0], wantFetch)
	}
	wantRevParse := []string{"rev-parse", "--verify", "refs/remotes/origin/develop^{commit}"}
	if strings.Join(git.calls[1], " ") != strings.Join(wantRevParse, " ") {
		t.Fatalf("rev-parse call = %#v, want %#v", git.calls[1], wantRevParse)
	}
}

// TestRefreshBaseRefFailureIsARefusalNotAFatalError asserts a failed fetch
// returns an error the capability layer turns into a non-terminal refusal,
// rather than panicking or fabricating a commit.
func TestRefreshBaseRefFailureIsARefusalNotAFatalError(t *testing.T) {
	git := &fakeGit{failFetch: true}
	s := refreshBaseRefSession(t, git, "main")
	commit, err := s.RefreshBaseRef(context.Background())
	if err == nil {
		t.Fatal("RefreshBaseRef succeeded despite a failed fetch")
	}
	if commit != "" {
		t.Fatalf("resolved base commit = %q on failure, want empty", commit)
	}
}

// TestRefreshBaseRefSerializesConcurrentFetches asserts two sessions sharing
// one Manager never fetch the shared refs/remotes/origin/<base> at the same
// time: at raised agent.max_concurrent_agents, unserialized concurrent
// fetches would race the same repository-wide ref and its packed-refs
// (PMR-141).
func TestRefreshBaseRefSerializesConcurrentFetches(t *testing.T) {
	git := &overlapTrackingGit{}
	settings := config.GitHub{Enabled: true, Owner: "owner", Repository: "repo", BaseBranch: "main", Token: "private-token"}
	m := New(func() config.Settings { return config.Settings{GitHub: settings} }, slog.Default())
	m.git = git
	s1 := &Session{manager: m, settings: settings, issue: domain.Issue{ID: "issue-27", Identifier: "PMR-27"}, workspace: t.TempDir(), branch: "symphony/pmr-27"}
	s2 := &Session{manager: m, settings: settings, issue: domain.Issue{ID: "issue-28", Identifier: "PMR-28"}, workspace: t.TempDir(), branch: "symphony/pmr-28"}

	var wg sync.WaitGroup
	wg.Add(2)
	for _, s := range []*Session{s1, s2} {
		s := s
		go func() {
			defer wg.Done()
			if _, err := s.RefreshBaseRef(context.Background()); err != nil {
				t.Errorf("RefreshBaseRef returned %v", err)
			}
		}()
	}
	wg.Wait()

	if git.maxActive != 1 {
		t.Fatalf("max concurrent fetches = %d, want 1 (unserialized)", git.maxActive)
	}
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

// TestPublishRefusalCausesAreDistinctAgentActionableMessages pins each of
// Publish's causes to its own message, so the capability layer's passthrough
// (PMR-132) is meaningful and a future edit cannot silently collapse two
// causes onto the same text.
func TestPublishRefusalCausesAreDistinctAgentActionableMessages(t *testing.T) {
	for _, test := range []struct {
		name     string
		base     *fakeGit
		wrap     func(*fakeGit) gitRunner
		prExists bool
		want     string
	}{
		{
			name: "origin mismatch",
			base: &fakeGit{},
			wrap: func(g *fakeGit) gitRunner { return originGit{fakeGit: g, origin: "git@github.com:someone/other.git"} },
			want: "github publish worktree origin does not match the configured repository",
		},
		{
			name: "dirty worktree",
			base: &fakeGit{dirty: true},
			want: "github publish requires a clean worktree",
		},
		{
			name: "no committed HEAD",
			base: &fakeGit{},
			wrap: func(g *fakeGit) gitRunner { return &failingGit{fakeGit: g, failArgs: []string{"rev-parse", "HEAD"}} },
			want: "github publish requires a committed HEAD",
		},
		{
			name: "no committed changes",
			base: &fakeGit{noChange: true},
			want: "github publish requires committed changes",
		},
		{
			name: "HEAD not based on the configured base branch",
			base: &fakeGit{},
			wrap: func(g *fakeGit) gitRunner {
				return &failingGit{fakeGit: g, failArgs: []string{"merge-base", "--is-ancestor", "base", "head"}}
			},
			want: "github publish HEAD is not based on the configured base branch",
		},
		{
			name: "remote head not fetched",
			base: &fakeGit{},
			wrap: func(g *fakeGit) gitRunner {
				return &failingGit{fakeGit: g, failArgs: []string{"cat-file", "-e", "sha1^{commit}"}}
			},
			prExists: true,
			want:     "github publish remote branch symphony/pmr-27 has a head commit this worktree has not fetched, so the cause of the divergence cannot be established here",
		},
		{
			name: "push failure",
			base: &fakeGit{},
			wrap: func(g *fakeGit) gitRunner {
				return &failingGit{fakeGit: g, failArgs: []string{"push", "https://github.com/owner/repo.git", "HEAD:refs/heads/symphony/pmr-27"}}
			},
			want: "github publish could not push branch symphony/pmr-27 to the configured repository; retry once, and if it persists check the repository's push permissions and branch protection rules",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			api, linear := newAPI(t), &fakeLinear{}
			api.prExists = test.prExists
			_, session := testSession(t, api, test.base, linear, nil)
			if test.wrap != nil {
				session.manager.git = test.wrap(test.base)
			}
			_, err := session.Publish(context.Background(), testInput())
			if err == nil {
				t.Fatal("unsafe publish succeeded")
			}
			if err.Error() != test.want {
				t.Fatalf("publish refusal = %q, want %q", err.Error(), test.want)
			}
		})
	}
	seen := map[string]bool{}
	for _, test := range []string{
		"github publish worktree origin does not match the configured repository",
		"github publish requires a clean worktree",
		"github publish requires a committed HEAD",
		"github publish requires committed changes",
		"github publish HEAD is not based on the configured base branch",
		"github publish remote branch symphony/pmr-27 has a head commit this worktree has not fetched, so the cause of the divergence cannot be established here",
		"github publish could not push branch symphony/pmr-27 to the configured repository; retry once, and if it persists check the repository's push permissions and branch protection rules",
	} {
		if seen[test] {
			t.Fatalf("duplicate publish refusal message: %q", test)
		}
		seen[test] = true
	}
}

// TestPublishRefusalLogsAWarnRecordNamingTheGate pins PMR-163: before this, a
// publish refusal produced no host-side record at all, so a run that spent an
// entire turn budget hitting the same refusal repeatedly left no trace of why.
// Every one of Publish's nine refusal paths must log exactly one Warn
// "GitHub publish refused" record naming the gate that fired -- distinctly
// enough that, for example, the stale-base ancestor gate and a dirty worktree
// are never confused for one another -- so a newly added, silent gate fails
// this test rather than shipping unnoticed.
func TestPublishRefusalLogsAWarnRecordNamingTheGate(t *testing.T) {
	for _, test := range []struct {
		name       string
		base       *fakeGit
		wrap       func(*fakeGit) gitRunner
		activeErr  error
		prExists   bool
		landing    bool
		wantReason string
	}{
		{
			// The gate that makes a Merging dispatch's outcome deterministic
			// (PMR-169). It is first because it refuses before EnsureActive, which
			// would otherwise accept this session: the configured merge state is
			// the state it was bound to, so it is its own active state, and
			// LinkAndHandoff would then walk an approved issue back to review.
			name:       "landing dispatch",
			base:       &fakeGit{},
			landing:    true,
			wantReason: "github publish is not available for an issue in the configured Merging state",
		},
		{
			name:       "stale issue",
			base:       &fakeGit{},
			activeErr:  errors.New("issue is no longer active"),
			wantReason: "issue is no longer active",
		},
		{
			name:       "origin mismatch",
			base:       &fakeGit{},
			wrap:       func(g *fakeGit) gitRunner { return originGit{fakeGit: g, origin: "git@github.com:someone/other.git"} },
			wantReason: "github publish worktree origin does not match the configured repository",
		},
		{
			name:       "dirty worktree",
			base:       &fakeGit{dirty: true},
			wantReason: "github publish requires a clean worktree",
		},
		{
			name:       "no committed HEAD",
			base:       &fakeGit{},
			wrap:       func(g *fakeGit) gitRunner { return &failingGit{fakeGit: g, failArgs: []string{"rev-parse", "HEAD"}} },
			wantReason: "github publish requires a committed HEAD",
		},
		{
			name:       "no committed changes",
			base:       &fakeGit{noChange: true},
			wantReason: "github publish requires committed changes",
		},
		{
			name: "stale base ancestor gate",
			base: &fakeGit{},
			wrap: func(g *fakeGit) gitRunner {
				return &failingGit{fakeGit: g, failArgs: []string{"merge-base", "--is-ancestor", "base", "head"}}
			},
			wantReason: "github publish HEAD is not based on the configured base branch",
		},
		{
			name: "remote head not fetched",
			base: &fakeGit{},
			wrap: func(g *fakeGit) gitRunner {
				return &failingGit{fakeGit: g, failArgs: []string{"cat-file", "-e", "sha1^{commit}"}}
			},
			prExists:   true,
			wantReason: "github publish remote branch symphony/pmr-27 has a head commit this worktree has not fetched",
		},
		{
			name: "push failure",
			base: &fakeGit{},
			wrap: func(g *fakeGit) gitRunner {
				return &failingGit{fakeGit: g, failArgs: []string{"push", "https://github.com/owner/repo.git", "HEAD:refs/heads/symphony/pmr-27"}}
			},
			wantReason: "github publish could not push branch symphony/pmr-27",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var log bytes.Buffer
			api, linear := newAPI(t), &fakeLinear{activeErr: test.activeErr}
			api.prExists = test.prExists
			_, session := testSession(t, api, test.base, linear, &log)
			if test.wrap != nil {
				session.manager.git = test.wrap(test.base)
			}
			if test.landing {
				session.settings.MergeState = "Merging"
				session.issue.State = "Merging"
			}
			if _, err := session.Publish(context.Background(), testInput()); err == nil {
				t.Fatal("unsafe publish succeeded")
			}
			if test.landing && (len(linear.links) != 0 || test.base.calls != nil) {
				t.Fatalf("a landing dispatch's publish reached GitHub or Linear: links=%v git=%v", linear.links, test.base.calls)
			}
			output := log.String()
			if strings.Count(output, `"msg":"GitHub publish refused"`) != 1 {
				t.Fatalf("expected exactly one publish refusal record, got: %s", output)
			}
			if !strings.Contains(output, `"operation":"publish_refused"`) {
				t.Fatalf("refusal record missing operation: %s", output)
			}
			if !strings.Contains(output, `"issue_identifier":"PMR-27"`) || !strings.Contains(output, `"branch":"symphony/pmr-27"`) {
				t.Fatalf("refusal record missing issue/branch: %s", output)
			}
			if !strings.Contains(output, test.wantReason) {
				t.Fatalf("refusal record missing reason %q: %s", test.wantReason, output)
			}
		})
	}
}

// TestPublishRefusalPushGateRecordCarriesTheUnderlyingGitError pins the live
// PMR-124 case: GitHub's own push rejection text -- for example its "without
// `workflow` scope" message -- is the entire diagnosis for an unrecoverable
// push, and discarding it in favor of only the fixed hint left an operator to
// reconstruct it by hand. The refusal record must carry that underlying error
// (bounded through observability.Text like every other diagnostic), while the
// agent-facing error stays the fixed, generic hint: the agent cannot act on
// provider-shaped text any more precisely than on the hint, and the raw text
// is not vetted for what an agent should read.
func TestPublishRefusalPushGateRecordCarriesTheUnderlyingGitError(t *testing.T) {
	var log bytes.Buffer
	api, linear := newAPI(t), &fakeLinear{}
	base := &fakeGit{}
	const pushErr = "refusing to allow a Personal Access Token to create or update workflow `.github/workflows/ci.yml` without `workflow` scope"
	git := &failingGit{fakeGit: base, failArgs: []string{"push", "https://github.com/owner/repo.git", "HEAD:refs/heads/symphony/pmr-27"}, message: pushErr}
	_, session := testSession(t, api, base, linear, &log)
	session.manager.git = git
	_, err := session.Publish(context.Background(), testInput())
	if err == nil || strings.Contains(err.Error(), pushErr) {
		t.Fatalf("agent-facing push error = %v, want only the fixed hint", err)
	}
	output := log.String()
	if !strings.Contains(output, `"push_error"`) || !strings.Contains(output, pushErr) {
		t.Fatalf("refusal record missing the underlying git push error: %s", output)
	}
	if !strings.Contains(output, "could not push branch symphony/pmr-27") {
		t.Fatalf("refusal record missing the fixed hint alongside the git error: %s", output)
	}
}

// TestPublishRefusalRecordNeverCarriesProviderOrCredentialText asserts the
// refusal record itself is bounded and scrubbed exactly like every other
// diagnostic in this package (observability.Text): a credential-shaped
// EnsureActive error is redacted rather than logged verbatim, and the push
// gate's now-attached underlying git error still cannot smuggle a credential
// through even though it is otherwise logged in full.
func TestPublishRefusalRecordNeverCarriesProviderOrCredentialText(t *testing.T) {
	t.Run("EnsureActive error", func(t *testing.T) {
		var log bytes.Buffer
		api := newAPI(t)
		linear := &fakeLinear{activeErr: errors.New("linear request failed: token=leaked-credential-value")}
		_, session := testSession(t, api, &fakeGit{}, linear, &log)
		if _, err := session.Publish(context.Background(), testInput()); err == nil {
			t.Fatal("unsafe publish succeeded")
		}
		output := log.String()
		if strings.Contains(output, "leaked-credential-value") {
			t.Fatalf("refusal record leaked a credential-shaped value: %s", output)
		}
		if !strings.Contains(output, "[REDACTED]") {
			t.Fatalf("refusal record did not redact the credential assignment: %s", output)
		}
	})

	t.Run("push gate error", func(t *testing.T) {
		var log bytes.Buffer
		api, linear := newAPI(t), &fakeLinear{}
		base := &fakeGit{}
		git := &failingGit{fakeGit: base, failArgs: []string{"push", "https://github.com/owner/repo.git", "HEAD:refs/heads/symphony/pmr-27"}, message: "remote rejected: authorization: bearer leaked-push-credential"}
		_, session := testSession(t, api, base, linear, &log)
		session.manager.git = git
		if _, err := session.Publish(context.Background(), testInput()); err == nil {
			t.Fatal("unsafe publish succeeded")
		}
		output := log.String()
		if strings.Contains(output, "leaked-push-credential") {
			t.Fatalf("refusal record leaked a credential-shaped push error: %s", output)
		}
	})
}

// TestPublishForwardedFailuresAtEveryCallSiteCarryNoProviderOrWireDecodedText
// guards the forwarding surface PMR-132 widened: the capability layer now
// forwards err.Error() for every error Publish can return, not only the local
// gate checks pinned above, so a failure at EnsureActive, findPull,
// publishPullRequest, or LinkAndHandoff must still reach the agent as a
// bounded message free of provider or wire-decoded text (PMR-149). findPull
// and publishPullRequest are exercised directly against a fixture that plants
// a secret in the response body; EnsureActive and LinkAndHandoff are Linear
// operations behind an interface, so this only pins that Publish forwards
// their result verbatim -- internal/linear's own tests are what establish
// that the real implementation never returns wire-decoded text there.
func TestPublishForwardedFailuresAtEveryCallSiteCarryNoProviderOrWireDecodedText(t *testing.T) {
	const secret = "wire-secret-should-never-reach-the-agent"

	t.Run("EnsureActive", func(t *testing.T) {
		api, git := newAPI(t), &fakeGit{}
		linear := &fakeLinear{activeErr: errors.New(secret)}
		_, session := testSession(t, api, git, linear, nil)
		if _, err := session.Publish(context.Background(), testInput()); err == nil || err.Error() != secret {
			t.Fatalf("EnsureActive failure not forwarded verbatim: %v", err)
		}
		if api.created != 0 {
			t.Fatalf("EnsureActive failure still created a pull request: created=%d", api.created)
		}
	})

	t.Run("findPull", func(t *testing.T) {
		api, git, linear := newAPI(t), &fakeGit{}, &fakeLinear{}
		api.failMethod, api.failPath, api.failStatus, api.failBody = http.MethodGet, "/repos/owner/repo/pulls", http.StatusInternalServerError, secret
		_, session := testSession(t, api, git, linear, nil)
		_, err := session.Publish(context.Background(), testInput())
		if err == nil || strings.Contains(err.Error(), secret) {
			t.Fatalf("findPull failure leaked provider text: %v", err)
		}
		if err.Error() != "github request failed with status 500" {
			t.Fatalf("findPull failure message = %q", err.Error())
		}
	})

	t.Run("publishPullRequest", func(t *testing.T) {
		api, git, linear := newAPI(t), &fakeGit{}, &fakeLinear{}
		api.failMethod, api.failPath, api.failStatus, api.failBody = http.MethodPost, "/repos/owner/repo/pulls", http.StatusInternalServerError, secret
		_, session := testSession(t, api, git, linear, nil)
		_, err := session.Publish(context.Background(), testInput())
		if err == nil || strings.Contains(err.Error(), secret) {
			t.Fatalf("publishPullRequest failure leaked provider text: %v", err)
		}
		if err.Error() != "github request failed with status 500" {
			t.Fatalf("publishPullRequest failure message = %q", err.Error())
		}
		if api.created != 0 {
			t.Fatalf("publishPullRequest failure still created a pull request: created=%d", api.created)
		}
	})

	t.Run("LinkAndHandoff", func(t *testing.T) {
		api, git := newAPI(t), &fakeGit{}
		linear := &fakeLinear{linkErr: errors.New(secret)}
		_, session := testSession(t, api, git, linear, nil)
		if _, err := session.Publish(context.Background(), testInput()); err == nil || err.Error() != secret {
			t.Fatalf("LinkAndHandoff failure not forwarded verbatim: %v", err)
		}
	})
}

// TestPublishForceWithLeasePushesARewrittenBranch asserts a worktree whose
// HEAD no longer descends from the published pull request's remote head --
// because it rebased instead of merged -- is still publishable. s.branch is
// Symphony's own deterministic per-issue branch that nothing else writes, so
// the push below runs under --force-with-lease bound to the remote head this
// call just observed rather than being refused outright (PMR-137).
func TestPublishForceWithLeasePushesARewrittenBranch(t *testing.T) {
	api, linear := newAPI(t), &fakeLinear{}
	api.prExists = true
	base := &fakeGit{}
	git := &failingGit{fakeGit: base, failArgs: []string{"merge-base", "--is-ancestor", "sha1", "head"}}
	_, session := testSession(t, api, base, linear, nil)
	session.manager.git = git
	if _, err := session.Publish(context.Background(), testInput()); err != nil {
		t.Fatalf("rewritten-branch publish under lease failed: %v", err)
	}
	if len(linear.links) != 1 {
		t.Fatalf("rewritten-branch publish did not hand off: links=%v", linear.links)
	}
	git.mu.Lock()
	defer git.mu.Unlock()
	wantPush := "push --force-with-lease=refs/heads/symphony/pmr-27:sha1 https://github.com/owner/repo.git HEAD:refs/heads/symphony/pmr-27"
	found := false
	for _, call := range git.calls {
		if call[0] == "push" {
			found = true
			if strings.Join(call, " ") != wantPush {
				t.Fatalf("push call = %q, want %q", strings.Join(call, " "), wantPush)
			}
		}
	}
	if !found {
		t.Fatal("rewritten-branch publish never pushed")
	}
}

// TestPublishForceWithLeaseFailsClosedOnUnexpectedRemoteState asserts that
// when the remote branch has moved since Publish observed it, so the lease no
// longer matches, the push is refused rather than allowed to overwrite
// whatever is now on the branch, and neither GitHub nor Linear is mutated
// (PMR-137).
func TestPublishForceWithLeaseFailsClosedOnUnexpectedRemoteState(t *testing.T) {
	api, linear := newAPI(t), &fakeLinear{}
	api.prExists = true
	base := &fakeGit{}
	git := &nonFastForwardThenRacedPushGit{fakeGit: base}
	_, session := testSession(t, api, base, linear, nil)
	session.manager.git = git
	if _, err := session.Publish(context.Background(), testInput()); err == nil {
		t.Fatal("racing push under a stale lease succeeded")
	}
	if api.created != 0 || len(linear.links) != 0 {
		t.Fatalf("racing push under a stale lease mutated GitHub or Linear: created=%d links=%v", api.created, linear.links)
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

// TestPublishRefusesPullRequestMergedDuringThePushWindow pins the case PMR-149
// item 4 identified: the push is a network round trip during which the pull
// request's state can change, so Publish must re-check that state after the
// push rather than reuse its pre-push lookup, or a pull request merged while
// the push was in flight gets PATCHed and handed off as a normal publish.
func TestPublishRefusesPullRequestMergedDuringThePushWindow(t *testing.T) {
	api, git, linear := newAPI(t), &fakeGit{}, &fakeLinear{}
	api.prExists, api.prState, api.prBody = true, "open", "old body"
	git.onPush = func() {
		api.mu.Lock()
		defer api.mu.Unlock()
		api.prMerged = true
		api.prState = "closed"
	}
	_, session := testSession(t, api, git, linear, nil)
	if _, err := session.Publish(context.Background(), testInput()); err == nil || !strings.Contains(err.Error(), "already merged") {
		t.Fatalf("merge-during-push error=%v", err)
	}
	if len(api.patches) != 0 || len(linear.links) != 0 {
		t.Fatalf("pull request merged during the push window was mutated: patches=%v links=%v", api.patches, linear.links)
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
		"oversized why":      `{"why":"` + strings.Repeat("x", MaxPublishWhyBytes+1) + `","what_changed":"b","on_call":"c"}`,
		"oversized on_call":  `{"why":"a","what_changed":"b","on_call":"` + strings.Repeat("x", MaxPublishOnCallBytes+1) + `"}`,
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

// Effective review state follows GitHub's own precedence: only a
// state-bearing review supersedes a reviewer's earlier one.
func TestContextEffectiveReviewStateFollowsGitHubPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name    string
		reviews []map[string]any
		want    string
	}{
		{
			name: "comment after changes requested does not clear it",
			reviews: []map[string]any{
				{"user": map[string]any{"login": "bob"}, "state": "CHANGES_REQUESTED", "body": "no", "submitted_at": "t1"},
				{"user": map[string]any{"login": "bob"}, "state": "COMMENTED", "body": "still need X", "submitted_at": "t2"},
			},
			want: "changes_requested",
		},
		{
			name: "approval after changes requested clears it",
			reviews: []map[string]any{
				{"user": map[string]any{"login": "bob"}, "state": "CHANGES_REQUESTED", "body": "no", "submitted_at": "t1"},
				{"user": map[string]any{"login": "bob"}, "state": "APPROVED", "body": "fixed", "submitted_at": "t2"},
			},
			want: "approved",
		},
		{
			// A dismissal rewrites the original review's state in place, so
			// the changes-requested review is simply no longer present.
			name: "dismissed changes requested no longer counts",
			reviews: []map[string]any{
				{"user": map[string]any{"login": "bob"}, "state": "DISMISSED", "body": "no", "submitted_at": "t1"},
				{"user": map[string]any{"login": "alice"}, "state": "APPROVED", "body": "lgtm", "submitted_at": "t2"},
			},
			want: "approved",
		},
		{
			name: "comment-only review leaves the state pending",
			reviews: []map[string]any{
				{"user": map[string]any{"login": "bob"}, "state": "COMMENTED", "body": "a thought", "submitted_at": "t1"},
			},
			want: "pending",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api, git, linear := newAPI(t), &fakeGit{}, &fakeLinear{}
			_, session := testSession(t, api, git, linear, nil)
			if _, err := session.Publish(context.Background(), testInput()); err != nil {
				t.Fatal(err)
			}
			api.mu.Lock()
			api.reviews = tc.reviews
			api.mu.Unlock()
			result, err := session.Context(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if result.ReviewState != tc.want {
				t.Fatalf("review_state=%q want %q", result.ReviewState, tc.want)
			}
		})
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
