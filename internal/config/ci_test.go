package config

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestRequiredChecksNameJobsCIProduces guards against the stuck-in-Merging
// failure mode where a job in ci.yml is renamed but WORKFLOW.md's
// github.required_checks still names the old check, so GitHub never reports
// a check by that name and github_land_pr waits on it forever.
func TestRequiredChecksNameJobsCIProduces(t *testing.T) {
	dir := t.TempDir()
	for variable, name := range map[string]string{"SYMPHONY_LINEAR_API_KEY_FILE": "linear-key", "SYMPHONY_GITHUB_TOKEN_FILE": "github-token"} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("test-secret"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv(variable, path)
	}
	w, err := Load(filepath.Join("..", "..", "WORKFLOW.md"), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(w.Config.GitHub.RequiredChecks) == 0 {
		t.Fatal("WORKFLOW.md names no github.required_checks")
	}

	raw, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			Name string `yaml:"name"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &workflow); err != nil {
		t.Fatalf("ci.yml did not parse as YAML: %v", err)
	}
	jobNames := make(map[string]struct{}, len(workflow.Jobs))
	for _, job := range workflow.Jobs {
		if job.Name != "" {
			jobNames[job.Name] = struct{}{}
		}
	}
	for _, check := range w.Config.GitHub.RequiredChecks {
		if _, ok := jobNames[check]; !ok {
			t.Fatalf("WORKFLOW.md required_checks names %q, which is not a job name .github/workflows/ci.yml produces (job names: %v)", check, jobNames)
		}
	}
}
