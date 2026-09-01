package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// workflowWithCacheRoot writes a minimal loadable workflow whose workspace
// roots are real directories, so the only thing under test is agent.cache_root.
func workflowWithCacheRoot(t *testing.T, cacheRoot string) (string, string) {
	t.Helper()
	d := t.TempDir()
	source := filepath.Join(d, "source")
	work := filepath.Join(d, "work")
	for _, dir := range []string{source, work} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	body := "---\ntracker: {kind: linear, active_states: [Todo], terminal_states: [Done]}\n" +
		"workspace: {root: " + work + ", source_root: " + source + "}\n" +
		"agent: {cache_root: " + cacheRoot + "}\n---\nprompt"
	p := filepath.Join(d, "WORKFLOW.md")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p, d
}

// The grant is off by default: a workflow that does not mention cache_root must
// carry no cache root at all, so adding this field cannot widen the boundary
// for any workflow written before it existed.
func TestCacheRootAbsentByDefault(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "WORKFLOW.md")
	body := "---\ntracker: {kind: linear, active_states: [Todo], terminal_states: [Done]}\n---\nprompt"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Load(p, "")
	if err != nil {
		t.Fatal(err)
	}
	if s.Config.Agent.CacheRoot != "" {
		t.Fatalf("cache_root=%q, want empty when unconfigured", s.Config.Agent.CacheRoot)
	}
}

// A configured cache root is normalized to an absolute path, like every other
// path in the front matter, and resolves against the workflow file's directory.
func TestCacheRootIsNormalized(t *testing.T) {
	p, d := workflowWithCacheRoot(t, "caches")
	s, err := Load(p, "")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(d, "caches"); s.Config.Agent.CacheRoot != want {
		t.Fatalf("cache_root=%q, want %q", s.Config.Agent.CacheRoot, want)
	}
}

// The overlap rules are the whole security argument for this field: a cache
// root that reaches the workspace or the source repository would hand a session
// another issue's worktree or the repository the post-run integrity check
// assumes it cannot write.
func TestCacheRootRejectsOverlapAndDegenerateRoots(t *testing.T) {
	d := t.TempDir()
	source := filepath.Join(d, "source")
	work := filepath.Join(d, "work")
	for _, dir := range []string{source, work} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	for name, cacheRoot := range map[string]string{
		"equals workspace root":   work,
		"inside workspace root":   filepath.Join(work, "cache"),
		"contains workspace root": d,
		"equals source root":      source,
		"inside source root":      filepath.Join(source, "cache"),
		"filesystem root":         string(filepath.Separator),
		"home directory":          home,
	} {
		t.Run(name, func(t *testing.T) {
			body := "---\ntracker: {kind: linear, active_states: [Todo], terminal_states: [Done]}\n" +
				"workspace: {root: " + work + ", source_root: " + source + "}\n" +
				"agent: {cache_root: " + cacheRoot + "}\n---\nprompt"
			p := filepath.Join(t.TempDir(), "WORKFLOW.md")
			if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := Load(p, "")
			if err == nil {
				t.Fatalf("cache_root %q was accepted, want rejection", cacheRoot)
			}
			if !strings.Contains(err.Error(), "agent.cache_root") {
				t.Fatalf("error %v does not name agent.cache_root", err)
			}
		})
	}
}

// A sibling of both roots is the intended shape and must load.
func TestCacheRootAcceptsSiblingDirectory(t *testing.T) {
	p, d := workflowWithCacheRoot(t, filepath.Join(t.TempDir(), "shared-cache"))
	if _, err := Load(p, ""); err != nil {
		t.Fatalf("a cache root outside both workspace roots was rejected: %v", err)
	}
	_ = d
}
