package github

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pmrrasmussen/symphony/internal/config"
)

// TestNoHostCredentialReachesTheGitChild is execGit's share of the guarantee
// config.ReservedSecretEnvNames documents. Every git this package runs runs in
// the agent's own worktree, over repository configuration the agent can reach,
// so it is a child on the same terms as a workspace hook and gets the same
// filter (PMR-175). It has no session, so filter 4 has no case here.
//
// It also pins the other half: extraEnv is appended after filtering, because the
// authenticated push hands its own credential over deliberately and no filter
// may strip it.
func TestNoHostCredentialReachesTheGitChild(t *testing.T) {
	environment := filepath.Join(t.TempDir(), "git-environment")
	binDir := t.TempDir()
	script := fmt.Sprintf("#!/bin/sh\n/usr/bin/env > %q\n", environment)
	if err := os.WriteFile(filepath.Join(binDir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GITHUB_TOKEN", "reserved-forge-token-value")
	t.Setenv("SYMPHONY_LINEAR_API_KEY_FILE", "/private/reserved-linear-key-path")
	t.Setenv("PMR175_CONFIGURED_NAME", "configured-name-value")
	t.Setenv("PMR175_INHERITED_CONFIGURED", "Bearer configured-secret-value")
	t.Setenv("PMR175_KEPT", "ordinary-value")

	settings := config.Settings{
		HostSecretEnvNames: []string{"PMR175_CONFIGURED_NAME"},
		HostSecretValues:   []string{"configured-secret-value"},
	}
	git := execGit{settings: func() config.Settings { return settings }}
	if _, err := git.Run(context.Background(), t.TempDir(), []string{"status"}, []string{"PMR175_HANDED_OVER=extra-header-value"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(environment)
	if err != nil {
		t.Fatal(err)
	}
	child := string(data)
	for _, leaked := range []string{"reserved-forge-token-value", "/private/reserved-linear-key-path",
		"configured-name-value", "configured-secret-value"} {
		if strings.Contains(child, leaked) {
			t.Fatalf("git child environment retained %q", leaked)
		}
	}
	if !strings.Contains(child, "PMR175_KEPT=ordinary-value") {
		t.Fatal("the host credential filter removed unrelated variables")
	}
	if !strings.Contains(child, "PMR175_HANDED_OVER=extra-header-value") {
		t.Fatal("git child lost the credential its caller handed over deliberately")
	}
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
