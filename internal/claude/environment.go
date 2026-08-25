package claude

import (
	"os"
	"strings"

	"github.com/pmrrasmussen/symphony/internal/config"
)

// reservedSecretEnvNames are never passed to a Claude child regardless of
// configuration. They are the host's own tracker and forge credentials: an agent
// reaches Symphony's providers through bounded capabilities, never directly.
var reservedSecretEnvNames = []string{
	"LINEAR_API_KEY",
	"SYMPHONY_LINEAR_API_KEY_FILE",
	"GITHUB_TOKEN",
	"SYMPHONY_GITHUB_TOKEN",
	"SYMPHONY_GITHUB_TOKEN_FILE",
}

// filteredEnv is the child environment with host secrets removed by name and by
// value, plus the one variable this launcher adds.
//
// Three filters, and each one exists because the others do not cover it.
// reservedSecretEnvNames and the configured names remove the host's own tracker
// and forge credentials wherever they are known by name. The configured values
// remove them under any other name, because an inherited variable Symphony has
// never heard of can still carry a configured credential. secretMatcher is the
// provider filter: a resolved credential a provider session holds -- a GitHub
// token read from a file at session build, say -- has no configured name and no
// configured value, so neither of the other two filters can see it. The Codex
// backend has always passed that matcher; this one did not, which meant a
// provider-resolved credential was removed from a Codex child and inherited by a
// Claude child. Binding providers to this backend is what makes that reachable.
//
// endpoint is the only variable added. The token is appended after filtering
// rather than merged before it, so no filter can strip the credential this
// launcher is deliberately handing over -- and Go's exec keeps the last value
// for a duplicated key, so an operator's own stale SYMPHONY_MCP_TOKEN cannot win
// over this turn's. The name is blocked unconditionally as well, so a session
// with no capability endpoint cannot inherit a token from the host either: the
// variable's only source is this function.
//
// Everything else is inherited on purpose: the CLI authenticates through the
// operator's own login, which lives in the home directory it reads from.
func filteredEnv(extraNames []string, settings func() config.Settings, secretMatcher func(string) bool, endpoint *capabilityEndpoint) []string {
	var s config.Settings
	if settings != nil {
		s = settings()
	}
	blocked := map[string]bool{endpointTokenEnvName: true}
	for _, name := range reservedSecretEnvNames {
		blocked[name] = true
	}
	for _, name := range extraNames {
		if name = strings.TrimSpace(name); name != "" {
			blocked[name] = true
		}
	}
	for _, name := range s.HostSecretEnvNames {
		if name = strings.TrimSpace(name); name != "" {
			blocked[name] = true
		}
	}
	values := make([]string, 0, len(s.HostSecretValues))
	for _, value := range s.HostSecretValues {
		if value != "" {
			values = append(values, value)
		}
	}

	environment := os.Environ()
	filtered := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		name, value, found := strings.Cut(entry, "=")
		if !found || blocked[name] {
			continue
		}
		if containsAny(value, values) || (secretMatcher != nil && secretMatcher(value)) {
			continue
		}
		filtered = append(filtered, entry)
	}
	if endpoint != nil && endpoint.token != "" {
		filtered = append(filtered, endpointTokenEnvName+"="+endpoint.token)
	}
	return filtered
}

func containsAny(value string, secrets []string) bool {
	for _, secret := range secrets {
		if strings.Contains(value, secret) {
			return true
		}
	}
	return false
}
