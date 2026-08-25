package claude

import (
	"os"
	"strings"

	"github.com/pmrrasmussen/symphony/internal/config"
)

// filteredEnv is the child environment with host credentials removed, plus the
// one variable this launcher adds.
//
// The filter itself is not this backend's own: config.ReservedSecretEnvNames
// documents all four filters -- the reserved names, the configured names, the
// configured values, and the bound providers' secret matcher -- and what each
// one covers that the others do not. internal/codex applies exactly the same
// four, and its filterEntries matches this one entry for entry.
//
// What is specific to this launcher is the endpoint. It is the only variable
// added. The token is appended after filtering rather than merged before it, so
// no filter can strip the credential this launcher is deliberately handing over
// -- and Go's exec keeps the last value for a duplicated key, so an operator's
// own stale SYMPHONY_MCP_TOKEN cannot win over this turn's. The name is blocked
// unconditionally as well, so a session with no capability endpoint cannot
// inherit a token from the host either: the variable's only source is this
// function.
func filteredEnv(extraNames []string, settings func() config.Settings, secretMatcher func(string) bool, endpoint *capabilityEndpoint) []string {
	var s config.Settings
	if settings != nil {
		s = settings()
	}
	blocked := map[string]bool{endpointTokenEnvName: true}
	for _, name := range config.ReservedSecretEnvNames() {
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

	filtered := filterEntries(os.Environ(), blocked, values, secretMatcher)
	if endpoint != nil && endpoint.token != "" {
		filtered = append(filtered, endpointTokenEnvName+"="+endpoint.token)
	}
	return filtered
}

// filterEntries is the environment loop over an explicit entry list, which is
// the only way a test can present an entry os.Environ() cannot be made to hold.
//
// An entry carrying no "=" is dropped rather than forwarded, and only the value
// is ever offered to the value filters: a malformed entry conveys nothing to a
// child, and running a whole entry through them would let a variable's own
// *name* trip a credential match and silently strip an unrelated variable.
// internal/codex's filterEntries does the same with the same entry.
func filterEntries(entries []string, blocked map[string]bool, values []string, secretMatcher func(string) bool) []string {
	filtered := make([]string, 0, len(entries)+1)
	for _, entry := range entries {
		name, value, found := strings.Cut(entry, "=")
		if !found || blocked[name] {
			continue
		}
		if containsAny(value, values) || (secretMatcher != nil && secretMatcher(value)) {
			continue
		}
		filtered = append(filtered, entry)
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
