package claude

import (
	"os"

	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/hostenv"
)

// filteredEnv is the child environment with host credentials removed, plus the
// one variable this launcher adds.
//
// The filter itself is not this backend's own: hostenv.Filter is the one
// implementation every child Symphony spawns shares, and
// config.ReservedSecretEnvNames describes what it removes and why.
//
// What is specific to this launcher is the endpoint. It is the only variable
// added. The token is appended after filtering rather than merged before it, so
// no filter can strip the credential this launcher is deliberately handing over
// -- and Go's exec keeps the last value for a duplicated key, so an operator's
// own stale SYMPHONY_MCP_TOKEN cannot win over this turn's. The name is passed
// to the filter as a blocked name unconditionally as well, so a session with no
// capability endpoint cannot inherit a token from the host either: the
// variable's only source is this function.
func filteredEnv(extraNames []string, settings func() config.Settings, secretMatcher func(string) bool, endpoint *capabilityEndpoint) []string {
	var s config.Settings
	if settings != nil {
		s = settings()
	}
	blocked := append([]string{endpointTokenEnvName}, extraNames...)
	filtered := hostenv.Filter(os.Environ(), blocked, s, secretMatcher)
	if endpoint != nil && endpoint.token != "" {
		filtered = append(filtered, endpointTokenEnvName+"="+endpoint.token)
	}
	return filtered
}
