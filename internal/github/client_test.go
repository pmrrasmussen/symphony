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
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/observability"
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

// requestFailureManager points a Manager at a test server whose one handler is
// the test's, logging into logs as JSONL. The handler is built from the
// endpoint rather than closing over the started server, so a paginating test
// can write an absolute rel="next" Link the walk will follow without racing
// the server's own goroutine for the address.
func requestFailureManager(t *testing.T, logs *bytes.Buffer, handler func(endpoint string) http.HandlerFunc) (*Manager, config.GitHub) {
	t.Helper()
	server := httptest.NewUnstartedServer(nil)
	endpoint := "http://" + server.Listener.Addr().String()
	server.Config.Handler = handler(endpoint)
	server.Start()
	t.Cleanup(server.Close)
	settings := config.GitHub{Enabled: true, Owner: "owner", Repository: "repo", BaseBranch: "main", Token: "private-token", Endpoint: endpoint}
	return New(func() config.Settings { return config.Settings{GitHub: settings} }, slog.New(slog.NewJSONHandler(logs, nil))), settings
}

// failureRecords decodes the JSONL buffer and returns every "GitHub request
// failed" record, so a test can assert both what one carries and how many
// there are.
func failureRecords(t *testing.T, logs string) []map[string]any {
	t.Helper()
	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("invalid log line %q: %v", line, err)
		}
		if record["msg"] == "GitHub request failed" {
			records = append(records, record)
		}
	}
	return records
}

// TestFailedRequestLogsTheProviderDiagnosisHostSideOnly pins PMR-184's whole
// point: GitHub puts the reason a merge-time 405 happened in the response body
// ("At least 1 approving review is required"), and discarding it parked an
// issue in Merging with nothing but a status code in the log. The excerpt now
// reaches the operator log, bounded and scrubbed through observability.Text
// like every other diagnostic, while the agent-facing string stays the fixed
// one -- the same split session.go's and land.go's push gates already make.
func TestFailedRequestLogsTheProviderDiagnosisHostSideOnly(t *testing.T) {
	const message = "At least 1 approving review is required by reviewers with write access."
	// The body is provider text this package does not author, so it is scrubbed
	// on the same terms as a git push error: a credential form appearing in it
	// must not reach the log verbatim.
	body := `{"message":"` + message + ` (authorization: Bearer leaked-credential-value)","documentation_url":"` +
		strings.Repeat("x", 2*observability.MaxDiagnosticBytes) + `"}`
	var logs bytes.Buffer
	m, settings := requestFailureManager(t, &logs, func(string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusMethodNotAllowed)
			fmt.Fprint(w, body)
		}
	})

	err := m.request(context.Background(), settings, http.MethodPut, "/repos/owner/repo/pulls/7/merge", map[string]any{"sha": "head"}, nil)
	if err == nil || err.Error() != "github request failed with status 405" {
		t.Fatalf("agent-facing error = %v, want the fixed status-only string", err)
	}

	records := failureRecords(t, logs.String())
	if len(records) != 1 {
		t.Fatalf("failure records = %d, want exactly 1: %s", len(records), logs.String())
	}
	excerpt, _ := records[0]["response_excerpt"].(string)
	if !strings.Contains(excerpt, message) {
		t.Fatalf("record dropped GitHub's explanation: %q", excerpt)
	}
	if strings.Contains(excerpt, "leaked-credential-value") || !strings.Contains(excerpt, "[REDACTED]") {
		t.Fatalf("record was not redacted: %q", excerpt)
	}
	// Bounded exactly as observability.Text bounds it: the padded body is far
	// longer than the cap, so a truncation marker must be what ends the field.
	if len(excerpt) > observability.MaxDiagnosticBytes+len("…[truncated]") {
		t.Fatalf("record excerpt was not bounded: %d bytes", len(excerpt))
	}
	if !strings.HasSuffix(excerpt, "…[truncated]") {
		t.Fatalf("oversized body was not truncated: %q", excerpt)
	}
	if status, _ := records[0]["status"].(float64); int(status) != http.StatusMethodNotAllowed {
		t.Fatalf("record status = %v, want 405", records[0]["status"])
	}
}

// TestManagerLoggerRedactsWhatTheCallSiteDidNot is PMR-181's coverage half
// seen from the package that used to be outside it. This package logs through a
// *slog.Logger it is handed, so before the redaction became a handler
// middleware its only protection was every call site remembering
// observability.Text: an attribute named `token` was written verbatim, and a
// credential shaped as a JSON member survived Text itself. Both are asserted
// here rather than in internal/observability, because what changed is that
// internal/github is covered at all.
func TestManagerLoggerRedactsWhatTheCallSiteDidNot(t *testing.T) {
	var logs bytes.Buffer
	m, settings := requestFailureManager(t, &logs, func(string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"message":"Bad credentials","token":"ghp_do-not-log-this"}`)
		}
	})

	// A call site of this package's own logger that forgets observability.Text
	// entirely -- the failure the per-call-site convention had no backstop for.
	m.logger.Warn("GitHub diagnostic", "token", "ghp_do-not-log-this",
		"error", errors.New(`{"api_key":"sk-do-not-log-this"}`))

	if err := m.request(context.Background(), settings, http.MethodGet, "/repos/owner/repo/pulls/7", nil, nil); err == nil {
		t.Fatal("a 401 response produced no error")
	}
	if strings.Contains(logs.String(), "do-not-log-this") {
		t.Fatalf("a secret-shaped value reached internal/github's log: %s", logs.String())
	}
	records := failureRecords(t, logs.String())
	if len(records) != 1 {
		t.Fatalf("failure records = %d, want exactly 1: %s", len(records), logs.String())
	}
	// The record still diagnoses the failure: redaction masks the credential
	// member, not the provider's explanation next to it.
	if excerpt, _ := records[0]["response_excerpt"].(string); !strings.Contains(excerpt, "Bad credentials") {
		t.Fatalf("redaction ate the provider's explanation: %q", excerpt)
	}
}

// TestFailedRequestLogsTheTransportError covers the other discard: client.Do's
// error was dropped without even being logged, so a DNS, TLS, or timeout
// failure was indistinguishable from any other "github request failed".
func TestFailedRequestLogsTheTransportError(t *testing.T) {
	var logs bytes.Buffer
	m, settings := requestFailureManager(t, &logs, func(string) http.HandlerFunc {
		return func(http.ResponseWriter, *http.Request) {}
	})
	// Point the manager at a port nothing is listening on, so the round trip
	// fails before any response exists.
	settings.Endpoint = "http://127.0.0.1:1"

	err := m.request(context.Background(), settings, http.MethodGet, "/repos/owner/repo/pulls/7", nil, nil)
	if err == nil || err.Error() != "github request failed" {
		t.Fatalf("agent-facing error = %v, want the fixed transport string", err)
	}
	records := failureRecords(t, logs.String())
	if len(records) != 1 {
		t.Fatalf("failure records = %d, want exactly 1: %s", len(records), logs.String())
	}
	transport, _ := records[0]["transport_error"].(string)
	if transport == "" || len(transport) > observability.MaxDiagnosticBytes+len("…[truncated]") {
		t.Fatalf("record transport_error = %q, want a bounded non-empty diagnosis", transport)
	}
}

// TestFailedPaginatedRequestLogsOneRecord pins the bound PMR-190's pagination
// put on this: requestWithHeader now runs once per page, so a failure partway
// through a walk must still leave the operator one record for the one bad
// request rather than one per remaining page.
func TestFailedPaginatedRequestLogsOneRecord(t *testing.T) {
	var logs bytes.Buffer
	var pages atomic.Int32
	m, settings := requestFailureManager(t, &logs, func(endpoint string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if pages.Add(1) > 1 {
				w.WriteHeader(http.StatusInternalServerError)
				fmt.Fprint(w, `{"message":"Server Error"}`)
				return
			}
			w.Header().Set("Link", fmt.Sprintf("<%s%s?page=2>; rel=\"next\"", endpoint, r.URL.Path))
			fmt.Fprint(w, `[]`)
		}
	})
	collected := 0
	complete, err := paginate(context.Background(), m, settings, "/repos/owner/repo/pulls/7/reviews?per_page=100", func([]json.RawMessage) { collected++ })
	if err == nil || complete {
		t.Fatalf("paginate() = %v, %v; want the failing page to abandon the walk", complete, err)
	}
	// One collected page proves the failure really was mid-walk rather than on
	// the first request, which is the case the per-page log could multiply.
	if collected != 1 {
		t.Fatalf("collected pages = %d, want the walk to have failed on its second page", collected)
	}
	if records := failureRecords(t, logs.String()); len(records) != 1 {
		t.Fatalf("failure records = %d, want exactly 1 for one failed page: %s", len(records), logs.String())
	}
}
