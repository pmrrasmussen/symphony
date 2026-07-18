package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/domain"
)

type fakeGit struct {
	mu       sync.Mutex
	dirty    bool
	noChange bool
	calls    [][]string
	envs     [][]string
}

func (g *fakeGit) Run(_ context.Context, _ string, args, env []string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, append([]string(nil), args...))
	g.envs = append(g.envs, append([]string(nil), env...))
	switch args[0] {
	case "status":
		if g.dirty {
			return " M file.go", nil
		}
	case "rev-parse":
		if args[1] == "HEAD" {
			return "head", nil
		}
		if g.noChange {
			return "head", nil
		}
		return "base", nil
	case "merge-base", "push":
		return "", nil
	}
	return "", nil
}

type fakeLinear struct {
	mu        sync.Mutex
	activeErr error
	links     []string
	completed int
}

func (l *fakeLinear) EnsureActive(context.Context) error { return l.activeErr }
func (l *fakeLinear) LinkAndHandoff(_ context.Context, url string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, existing := range l.links {
		if existing == url {
			return nil
		}
	}
	l.links = append(l.links, url)
	return nil
}
func (l *fakeLinear) Complete(context.Context) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.completed > 0 {
		return false, nil
	}
	l.completed++
	return true, nil
}

type apiFixture struct {
	t       *testing.T
	server  *httptest.Server
	mu      sync.Mutex
	created int
	merged  bool
	closed  bool
	auth    []string
}

func newAPI(t *testing.T) *apiFixture {
	f := &apiFixture{t: t}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}

func (f *apiFixture) settings() config.GitHub {
	return config.GitHub{Enabled: true, Owner: "owner", Repository: "repo", BaseBranch: "main", Token: "private-token", Endpoint: f.server.URL, PollInterval: time.Millisecond}
}

func (f *apiFixture) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/pulls":
		if r.URL.Query().Get("head") != "owner:symphony/pmr-27" || r.URL.Query().Get("base") != "main" {
			f.t.Errorf("query=%s", r.URL.RawQuery)
		}
		if f.created == 0 {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		_, _ = w.Write([]byte(`[ {"number":7,"html_url":"https://github.com/owner/repo/pull/7","state":"open"} ]`))
	case r.Method == http.MethodPost && r.URL.Path == "/repos/owner/repo/pulls":
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil || body["head"] != "symphony/pmr-27" || body["base"] != "main" {
			f.t.Errorf("create body=%v", body)
		}
		f.created++
		_, _ = w.Write([]byte(`{"number":7,"html_url":"https://github.com/owner/repo/pull/7","state":"open"}`))
	case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/pulls/7":
		state := "open"
		if f.closed {
			state = "closed"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 7, "html_url": "https://github.com/owner/repo/pull/7", "state": state, "merged": f.merged})
	default:
		f.t.Errorf("unexpected GitHub request %s %s", r.Method, r.URL.String())
		w.WriteHeader(http.StatusNotFound)
	}
}

func testSession(t *testing.T, api *apiFixture, git *fakeGit, linear *fakeLinear, log *bytes.Buffer) (*Manager, *Session) {
	t.Helper()
	settings := api.settings()
	logger := slog.Default()
	if log != nil {
		logger = slog.New(slog.NewJSONHandler(log, nil))
	}
	m := New(func() config.Settings { return config.Settings{GitHub: settings} }, logger)
	m.git = git
	s := &Session{manager: m, settings: settings, issue: domain.Issue{ID: "issue-27", Identifier: "PMR-27", Title: "Lifecycle", URL: "https://linear.app/issue/PMR-27"}, workspace: t.TempDir(), branch: "symphony/pmr-27", linear: linear}
	return m, s
}

func TestPublishCreatesThenReusesDeterministicPullRequest(t *testing.T) {
	api, git, linear := newAPI(t), &fakeGit{}, &fakeLinear{}
	m, session := testSession(t, api, git, linear, nil)
	first, err := session.Publish(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second, err := session.Publish(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first != second || first.Branch != "symphony/pmr-27" || api.created != 1 || len(linear.links) != 1 {
		t.Fatalf("first=%+v second=%+v created=%d links=%v", first, second, api.created, linear.links)
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
			if _, err := session.Publish(context.Background()); err == nil {
				t.Fatal("unsafe publish succeeded")
			}
			if api.created != 0 || len(linear.links) != 0 {
				t.Fatalf("created=%d links=%v", api.created, linear.links)
			}
		})
	}
}

func TestPollMergedCompletesOnceAndClosedUnmergedOnlyWarns(t *testing.T) {
	api, git, linear := newAPI(t), &fakeGit{}, &fakeLinear{}
	var logs bytes.Buffer
	m, session := testSession(t, api, git, linear, &logs)
	if _, err := session.Publish(context.Background()); err != nil {
		t.Fatal(err)
	}
	api.merged = true
	m.Poll(context.Background())
	m.Poll(context.Background())
	if linear.completed != 1 {
		t.Fatalf("completions=%d", linear.completed)
	}

	api2, linear2 := newAPI(t), &fakeLinear{}
	var closedLogs bytes.Buffer
	m2, session2 := testSession(t, api2, &fakeGit{}, linear2, &closedLogs)
	if _, err := session2.Publish(context.Background()); err != nil {
		t.Fatal(err)
	}
	api2.closed = true
	m2.Poll(context.Background())
	m2.Poll(context.Background())
	if linear2.completed != 0 || strings.Count(closedLogs.String(), "closed without merge") != 1 {
		t.Fatalf("completed=%d logs=%s", linear2.completed, closedLogs.String())
	}
	if strings.Contains(logs.String()+closedLogs.String(), "private-token") {
		t.Fatal("logs exposed credential")
	}
}
