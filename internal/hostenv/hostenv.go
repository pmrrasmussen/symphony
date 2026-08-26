// Package hostenv applies the host credential filter to the environment of a
// process Symphony spawns.
//
// It is the one implementation of the policy config.ReservedSecretEnvNames
// describes, and it lives outside both backends because that policy is a
// property of the trust boundary rather than of any one child. Every child that
// inherits the daemon's environment -- the Codex app-server, each Claude turn,
// each WORKFLOW.md workspace hook -- reaches it through Filter, so a filter
// cannot hold for one child and not another. That was not a hypothetical twice
// over: PMR-94 was Claude children inheriting the provider secrets Codex
// children stripped, from two implementations that each carried a comment
// asserting the other matched it, and PMR-113 was hooks inheriting the daemon's
// environment whole because a doctrine written about agent backends never named
// the third child.
//
// It depends only on internal/config, so a caller with no session and no
// capability registry -- a workspace hook -- can use it.
package hostenv

import (
	"strings"

	"github.com/pmrrasmussen/symphony/internal/config"
)

// Filter returns the entries a spawned process may inherit, applying all four
// parts of the filter config.ReservedSecretEnvNames documents in one pass:
//
//  1. config.ReservedSecretEnvNames, whatever the workflow configures.
//  2. extraNames, the caller's own blocked names -- the ones its constructor
//     was given, plus any variable it means to be the sole source of.
//  3. s.HostSecretEnvNames and s.HostSecretValues, the names and values this
//     workflow's credential references resolved to.
//  4. matcher, the credentials the providers bound to this run actually hold.
//
// entries is the caller's environment, normally os.Environ(). It is a parameter
// rather than read here because it is the only way a test can present an entry
// os.Environ() cannot be made to hold, and both such entries matter: an entry
// carrying no "=" is dropped rather than forwarded, and only a value is ever
// offered to the value filters. A malformed entry conveys nothing to a child,
// and running a whole entry through the value filters would let a variable's own
// *name* trip a credential match and silently strip an unrelated variable.
//
// matcher may be nil, and a caller that has no session must pass nil rather
// than invent one: capability.SecretMatcher is built from one session's
// bindings, and a caller spawned outside a session -- a workspace hook -- has no
// bindings to build it from. Such a caller gets filters 1 through 3, which are
// derived from settings alone and cover every credential a loaded workflow
// resolves. Filter 4 is what it forgoes: a credential held only by a live
// provider session, under a name and value no configuration mentions.
//
// Names in extraNames and s.HostSecretEnvNames are trimmed, and blank ones
// dropped, so a hand-assembled Settings carrying " NAME " blocks the same
// variable for every caller. A blank configured value is skipped too: it is
// contained in every value, and honouring it would empty the child environment.
//
// A credential the caller deliberately hands over is not this function's
// business: append it to the result, so no filter can strip it. That is what
// internal/claude does with the capability endpoint token.
func Filter(entries, extraNames []string, s config.Settings, matcher func(string) bool) []string {
	blocked := make(map[string]bool, len(extraNames)+len(s.HostSecretEnvNames)+len(config.ReservedSecretEnvNames()))
	block := func(names []string) {
		for _, name := range names {
			if name = strings.TrimSpace(name); name != "" {
				blocked[name] = true
			}
		}
	}
	block(config.ReservedSecretEnvNames())
	block(extraNames)
	block(s.HostSecretEnvNames)

	values := make([]string, 0, len(s.HostSecretValues))
	for _, value := range s.HostSecretValues {
		if value != "" {
			values = append(values, value)
		}
	}

	filtered := make([]string, 0, len(entries))
	for _, entry := range entries {
		name, value, found := strings.Cut(entry, "=")
		if !found || blocked[name] {
			continue
		}
		if containsAny(value, values) || (matcher != nil && matcher(value)) {
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
