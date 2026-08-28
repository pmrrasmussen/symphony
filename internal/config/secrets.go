package config

import (
	"sort"
	"strings"
)

// reservedSecretEnvNames is the fixed half of the host credential filter: names
// that never reach a child Symphony spawns, whatever a workflow says. They are
// the documented variables Symphony's own tracker and forge credentials are
// read from, and an agent reaches those providers through bounded capabilities,
// never directly. It lives beside HostSecretEnvNames and HostSecretValues, and
// beside the loader that derives both, because all of them are one policy -- see
// ReservedSecretEnvNames.
var reservedSecretEnvNames = []string{
	"LINEAR_API_KEY",
	"SYMPHONY_LINEAR_API_KEY_FILE",
	"GITHUB_TOKEN",
	"SYMPHONY_GITHUB_TOKEN",
	"SYMPHONY_GITHUB_TOKEN_FILE",
}

// ReservedSecretEnvNames returns the names no child Symphony spawns may inherit
// under any configuration. A copy is returned because its one caller blocks
// names of its own alongside these.
//
// The five names here are the fixed half of a four-part filter, and
// docs/architecture.md's "The host credential filter" section is the one
// description of the whole of it: what each part covers that the others cannot,
// which of the three kinds of child forgoes which part, and which test proves
// each. hostenv.Filter is the one implementation, so a name added or removed
// here cannot apply to one child Symphony spawns and not another.
func ReservedSecretEnvNames() []string { return append([]string(nil), reservedSecretEnvNames...) }

// hostSecretEnvNames extracts only environment variable names from credential
// references. It deliberately inspects the repository-owned raw fields so an
// optional GitHub integration that is currently disabled cannot accidentally
// leak its credential into a future Codex child process.
func hostSecretEnvNames(provider, github map[string]any) []string {
	names := map[string]struct{}{}
	collect := func(source map[string]any, keys ...string) {
		for _, key := range keys {
			value, ok := source[key].(string)
			if !ok {
				continue
			}
			if name, ok := environmentReferenceName(value); ok {
				names[name] = struct{}{}
			}
		}
	}
	collect(provider, "api_key", "api_key_file")
	collect(github, "token", "token_file")
	if len(names) == 0 {
		return nil
	}
	out := make([]string, 0, len(names))
	for name := range names {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// hostSecretValues keeps the resolved credentials needed to remove inherited
// values from the Codex environment. It deliberately includes an optional
// GitHub token even when the GitHub integration is disabled: configuration
// validity must not decide whether a host credential crosses the boundary.
func hostSecretValues(provider, github map[string]any, base string, sources *sourceSnapshot) []string {
	values := map[string]struct{}{}
	add := func(value string) {
		if value = strings.TrimSpace(value); value != "" {
			values[value] = struct{}{}
		}
	}
	if value, ok := provider["api_key"].(string); ok {
		add(value)
	}
	if github != nil {
		if file, ok := github["token_file"].(string); ok {
			if expanded, err := sources.expand(file, "github.token_file"); err == nil && strings.TrimSpace(expanded) != "" {
				if content, err := sources.readFile(normalizePath(expanded, base)); err == nil {
					add(string(content))
				}
			}
		} else if token, ok := github["token"].(string); ok && strings.HasPrefix(token, "$") {
			if expanded, err := sources.expand(token, "github.token"); err == nil {
				add(expanded)
			}
		}
	}
	if len(values) == 0 {
		return nil
	}
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func environmentReferenceName(value string) (string, bool) {
	if !strings.HasPrefix(value, "$") {
		return "", false
	}
	name := strings.TrimPrefix(value, "$")
	return name, ValidEnvironmentName(name)
}

// ValidEnvironmentName reports whether name is a legal $VARNAME reference: a
// non-empty run of letters, digits, and underscores that does not start with a
// digit.
func ValidEnvironmentName(name string) bool {
	if name == "" {
		return false
	}
	for index, character := range name {
		if character == '_' || character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || index > 0 && character >= '0' && character <= '9' {
			continue
		}
		return false
	}
	return true
}
