// Package hostenv applies the host credential filter to the environment of a
// process Symphony spawns.
//
// It is the one implementation of the policy config.ReservedSecretEnvNames
// names, and it lives outside both backends because that policy is a property
// of the trust boundary rather than of any one child. Every child that inherits
// the daemon's environment -- the Codex app-server, each Claude turn, each
// WORKFLOW.md workspace hook, each host-side git -- reaches it through Filter,
// so a filter cannot hold for one child and not another.
//
// It depends only on internal/config, so a caller with no session and no
// capability registry -- a workspace hook -- can use it.
//
// docs/architecture.md's "The host credential filter" section is the one
// description of the policy: what each of the four parts covers that the others
// cannot, the two occasions this package exists because of, and which test
// proves each part.
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
// entries is the caller's environment, normally os.Environ(); it is a parameter
// so a test can present entries os.Environ() cannot be made to hold. matcher may
// be nil, and a caller that has no session must pass nil rather than invent one:
// such a caller gets filters 1 through 3 and forgoes only filter 4.
//
// A credential the caller deliberately hands over is not this function's
// business: append it to the result, so no filter can strip it.
//
// docs/architecture.md's "The host credential filter" section states why the
// parameters have these shapes and what a nil matcher gives up.
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
