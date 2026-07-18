package codex

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/domain"
)

func TestStartNormalizesAppServerLifecycle(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-app-server.sh")
	body := `#!/bin/sh
IFS= read -r line
printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{}}'
IFS= read -r line
IFS= read -r line
printf '%s\n' '{"jsonrpc":"2.0","id":2,"result":{"thread":{"id":"thread-1"}}}'
IFS= read -r line
printf '%s\n' '{"jsonrpc":"2.0","id":3,"result":{"turn":{"id":"turn-1"}}}'
printf '%s\n' '{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"thread-1","turn":{"id":"turn-1","usage":{"inputTokens":4,"outputTokens":6,"totalTokens":10}}}}'
`
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	b := New()
	session, events, err := b.Start(context.Background(), domain.AgentRequest{Workspace: dir, Prompt: "work", Command: "sh " + script, ApprovalPolicy: "never", ThreadSandbox: "workspace-write", TurnTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if session.ID != "thread-1-turn-1" {
		t.Fatalf("session=%+v", session)
	}
	seen := map[domain.EventKind]bool{}
	for event := range events {
		seen[event.Kind] = true
	}
	if !seen[domain.EventSessionStarted] || !seen[domain.EventCompleted] {
		t.Fatalf("events=%v", seen)
	}
}

func TestEnabledLinearHandoffIsAdvertisedAndUsesOnlyBoundIssue(t *testing.T) {
	var graphQLCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		graphQLCalls++
		query := request["query"].(string)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(query, "SymphonyLinearHandoffIssue"):
			_, _ = w.Write([]byte(`{"data":{"issue":{"id":"active","identifier":"PMR-5","title":"Handoff","description":"safe","url":"https://linear.app/issue/PMR-5","project":{"slugId":"project-1"},"team":{"id":"team-1"},"state":{"name":"Todo"}}}}`))
		case strings.Contains(query, "SymphonyLinearHandoffStates"):
			_, _ = w.Write([]byte(`{"data":{"team":{"id":"team-1","states":{"nodes":[{"id":"review","name":"In Review"}]}}}}`))
		default:
			t.Fatalf("unexpected query: %s", query)
		}
	}))
	defer server.Close()
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-app-server.sh")
	body := `#!/bin/sh
IFS= read -r line
printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{}}'
IFS= read -r line
IFS= read -r line
case "$line" in *linear_graphql*) ;; *) exit 20;; esac
printf '%s\n' '{"jsonrpc":"2.0","id":2,"result":{"thread":{"id":"thread-1"}}}'
IFS= read -r line
printf '%s\n' '{"jsonrpc":"2.0","id":3,"result":{"turn":{"id":"turn-1"}}}'
printf '%s\n' '{"jsonrpc":"2.0","id":99,"method":"item/tool/call","params":{"threadId":"thread-1","turnId":"turn-1","callId":"call-1","tool":"linear_graphql","arguments":{"operation":"read"}}}'
IFS= read -r line
case "$line" in *'"success":true'*PMR-5*) ;; *) exit 21;; esac
printf '%s\n' '{"jsonrpc":"2.0","method":"turn/completed","params":{}}'
`
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	settings := config.Settings{Tracker: config.Tracker{
		Provider:     map[string]any{"api_key": "test-token", "project_slug": "project-1", "endpoint": server.URL},
		ActiveStates: []string{"todo"}, HandoffState: "In Review",
	}}
	b := NewWithLinearHandoff(func() config.Settings { return settings }, "LINEAR_API_KEY")
	_, events, err := b.Start(context.Background(), domain.AgentRequest{Issue: domain.Issue{ID: "active"}, Workspace: dir, Prompt: "work", Command: "sh " + script, ApprovalPolicy: "never", ThreadSandbox: "workspace-write", TurnTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}
	if graphQLCalls != 2 {
		t.Fatalf("GraphQL calls=%d want prepare-only 2", graphQLCalls)
	}
}

func TestDisabledLinearHandoffIsNotAdvertisedAndRemainsUnsupported(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-app-server.sh")
	body := `#!/bin/sh
IFS= read -r line
printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{}}'
IFS= read -r line
IFS= read -r line
case "$line" in *linear_graphql*) exit 20;; *) ;; esac
printf '%s\n' '{"jsonrpc":"2.0","id":2,"result":{"thread":{"id":"thread-1"}}}'
IFS= read -r line
printf '%s\n' '{"jsonrpc":"2.0","id":3,"result":{"turn":{"id":"turn-1"}}}'
printf '%s\n' '{"jsonrpc":"2.0","id":99,"method":"item/tool/call","params":{"tool":"linear_graphql","arguments":{"operation":"read"}}}'
IFS= read -r line
case "$line" in *'"success":false'*) ;; *) exit 21;; esac
printf '%s\n' '{"jsonrpc":"2.0","method":"turn/completed","params":{}}'
`
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	b := NewWithLinearHandoff(func() config.Settings { return config.Settings{} })
	_, events, err := b.Start(context.Background(), domain.AgentRequest{Workspace: dir, Prompt: "work", Command: "sh " + script, ApprovalPolicy: "never", ThreadSandbox: "workspace-write", TurnTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	seenBlocked := false
	for event := range events {
		seenBlocked = seenBlocked || event.Kind == domain.EventBlocked
	}
	if !seenBlocked {
		t.Fatal("disabled handoff did not retain unsupported-tool behavior")
	}
}

func TestFilteredEnvRemovesConfiguredSecretByNameAndValue(t *testing.T) {
	t.Setenv("PMR5_TOKEN_BY_NAME", "visible-if-broken")
	t.Setenv("PMR5_TOKEN_BY_VALUE", "linear-secret")
	t.Setenv("PMR5_TOKEN_WITH_PREFIX", "Bearer linear-secret")
	t.Setenv("PMR5_TOKEN_WITH_SUFFIX", "linear-secret:suffix")
	for _, value := range filteredEnv([]string{"PMR5_TOKEN_BY_NAME"}, func(candidate string) bool { return strings.Contains(candidate, "linear-secret") }) {
		if strings.HasPrefix(value, "PMR5_TOKEN_BY_NAME=") || strings.HasPrefix(value, "PMR5_TOKEN_BY_VALUE=") {
			t.Fatalf("child environment retained Linear secret: %q", value)
		}
		if strings.HasPrefix(value, "PMR5_TOKEN_WITH_PREFIX=") || strings.HasPrefix(value, "PMR5_TOKEN_WITH_SUFFIX=") {
			t.Fatalf("child environment retained embedded Linear secret: %q", value)
		}
	}
}
