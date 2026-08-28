package github

import (
	"context"
	"testing"

	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/domain"
)

// TestForgetStopsPollingAndRepublicationTracksAgain covers the retention rule
// for a link that is neither merged nor closed: only the host's explicit
// terminal-issue signal evicts it, and doing so must not make the issue
// un-trackable if it publishes again.
func TestForgetStopsPollingAndRepublicationTracksAgain(t *testing.T) {
	api, git, linear := newAPI(t), &fakeGit{}, &fakeLinear{}
	m, session := testSession(t, api, git, linear, nil)
	if _, err := session.Publish(context.Background(), testInput()); err != nil {
		t.Fatal(err)
	}
	m.Forget("no-such-issue")
	if tracked(m) != 1 {
		t.Fatalf("an unknown issue ID evicted a live link: tracked=%d", tracked(m))
	}
	m.Forget("issue-27")
	reads := api.pullReads()
	m.Poll(context.Background())
	if tracked(m) != 0 || api.pullReads() != reads {
		t.Fatalf("forgotten issue still polled: tracked=%d reads=%d after, %d before", tracked(m), api.pullReads(), reads)
	}
	if _, err := session.Publish(context.Background(), testInput()); err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	api.prMerged = true
	api.mu.Unlock()
	m.Poll(context.Background())
	if tracked(m) != 0 || linear.reconciliations() != 1 {
		t.Fatalf("re-tracked issue did not poll again: tracked=%d reconciliations=%d", tracked(m), linear.reconciliations())
	}
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
