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
// value. Filtering by value matters because an inherited variable under any
// other name can still carry a configured credential.
//
// Everything else is inherited on purpose: the CLI authenticates through the
// operator's own login, which lives in the home directory it reads from.
func filteredEnv(extraNames []string, settings func() config.Settings) []string {
	var s config.Settings
	if settings != nil {
		s = settings()
	}
	blocked := map[string]bool{}
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
	filtered := make([]string, 0, len(environment))
	for _, entry := range environment {
		name, value, found := strings.Cut(entry, "=")
		if !found || blocked[name] {
			continue
		}
		if containsAny(value, values) {
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
