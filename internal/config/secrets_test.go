package config

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestHostSecretEnvNamesIncludeCredentialReferencesEvenWhenGitHubIsDisabled(t *testing.T) {
	d := t.TempDir()
	linearFile := filepath.Join(d, "linear-key")
	githubFile := filepath.Join(d, "github-token")
	for path, value := range map[string]string{linearFile: "linear-file-secret", githubFile: "github-file-secret"} {
		if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PMR29_LINEAR_KEY", "linear-env-secret")
	t.Setenv("PMR29_LINEAR_FILE", linearFile)
	t.Setenv("PMR29_GITHUB_TOKEN", "github-env-secret")
	t.Setenv("PMR29_GITHUB_FILE", githubFile)
	workflow := filepath.Join(d, "WORKFLOW.md")
	content := "---\ntracker: {kind: linear, provider: {project_slug_id: project, api_key: $PMR29_LINEAR_KEY, api_key_file: $PMR29_LINEAR_FILE}, active_states: [Todo], terminal_states: [Done]}\ngithub: {token: $PMR29_GITHUB_TOKEN, token_file: $PMR29_GITHUB_FILE}\n---\nprompt"
	if err := os.WriteFile(workflow, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(workflow, "")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Config.GitHub.Enabled {
		t.Fatal("incomplete optional GitHub configuration was enabled")
	}
	want := []string{"PMR29_GITHUB_FILE", "PMR29_GITHUB_TOKEN", "PMR29_LINEAR_FILE", "PMR29_LINEAR_KEY"}
	if got := loaded.Config.HostSecretEnvNames; !reflect.DeepEqual(got, want) {
		t.Fatalf("secret environment names=%v want %v", got, want)
	}
	wantValues := []string{"github-file-secret", "linear-file-secret"}
	if got := loaded.Config.HostSecretValues; !reflect.DeepEqual(got, wantValues) {
		t.Fatalf("secret values=%v want %v", got, wantValues)
	}
	for _, value := range []string{"linear-env-secret", "linear-file-secret", "github-env-secret", "github-file-secret"} {
		if strings.Contains(strings.Join(loaded.Config.HostSecretEnvNames, ","), value) {
			t.Fatalf("secret metadata exposed resolved value %q", value)
		}
	}
}

func TestReservedSecretEnvNamesReturnsAnIndependentCopy(t *testing.T) {
	// The expected values are copied here, not aliased: a first attempt at this
	// test held ReservedSecretEnvNames()'s own result and compared it against
	// itself, so an aliasing regression corrupted both sides equally and the
	// test passed. That is exactly the mutation it is supposed to catch.
	want := append([]string(nil), ReservedSecretEnvNames()...)
	if len(want) == 0 {
		t.Fatal("no reserved names")
	}
	appended := append(ReservedSecretEnvNames(), "SOMEONE_ELSES_NAME")
	ReservedSecretEnvNames()[0] = "OVERWRITTEN"

	if got := ReservedSecretEnvNames(); !slices.Equal(got, want) {
		t.Fatalf("the reserved list is now %v, want %v: a caller's write or append reached the package's own slice", got, want)
	}
	if appended[len(appended)-1] != "SOMEONE_ELSES_NAME" {
		t.Fatal("append did not land on the returned slice")
	}
}
