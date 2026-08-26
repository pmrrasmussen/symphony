package hostenv

import (
	"slices"
	"strings"
	"testing"

	"github.com/pmrrasmussen/symphony/internal/config"
)

// TestFilterAppliesEveryPartOfTheHostCredentialFilter is the one proof of what
// Filter removes, for every child Symphony spawns. Each case names the filter
// part it exercises, and each credential is reachable by exactly that part: a
// value no other part matches, under a name no other part blocks, so a deleted
// part cannot be covered by a surviving one.
//
// The reserved names are written out here rather than read from
// config.ReservedSecretEnvNames, deliberately: a test that iterates the list
// asserts nothing about its contents, and dropping an entry would leave it
// green.
func TestFilterAppliesEveryPartOfTheHostCredentialFilter(t *testing.T) {
	// Every case filters the same entries, so a case that removes something is
	// visibly the only one that removes it.
	entries := []string{
		"LINEAR_API_KEY=reserved-linear-key-value",
		"SYMPHONY_LINEAR_API_KEY_FILE=/private/reserved-linear-key-path",
		"GITHUB_TOKEN=reserved-forge-token-value",
		"SYMPHONY_GITHUB_TOKEN=reserved-symphony-forge-token-value",
		"SYMPHONY_GITHUB_TOKEN_FILE=/private/reserved-forge-token-path",
		"CALLER_BLOCKED=caller-blocked-value",
		"CONFIGURED_NAME=configured-name-value",
		"PADDED_NAME=padded-name-value",
		"CONFIGURED_FILE=/private/configured-key-path",
		"INNOCENT_LOOKING=Bearer configured-secret-value",
		"PROVIDER_HELD=prefix-provider-token-suffix",
		"KEPT=ordinary-value",
	}
	settings := config.Settings{
		// The padded name and the blank one are the hand-assembled-Settings
		// cases: a Settings that never went through config.Load can carry
		// either, and a caller must not be able to block a variable on one
		// child and inherit it on another because of whitespace.
		HostSecretEnvNames: []string{"CONFIGURED_NAME", "  PADDED_NAME  ", "   ", "CONFIGURED_FILE"},
		HostSecretValues:   []string{"configured-secret-value"},
	}
	provider := func(candidate string) bool { return strings.Contains(candidate, "provider-token") }

	for _, tc := range []struct {
		name       string
		extraNames []string
		settings   config.Settings
		matcher    func(string) bool
		// removed are the entries this case's filter parts must drop; every
		// other entry must survive.
		removed []string
	}{
		{
			name: "filter 1 blocks the reserved names whatever the workflow configures",
			removed: []string{
				"LINEAR_API_KEY=reserved-linear-key-value",
				"SYMPHONY_LINEAR_API_KEY_FILE=/private/reserved-linear-key-path",
				"GITHUB_TOKEN=reserved-forge-token-value",
				"SYMPHONY_GITHUB_TOKEN=reserved-symphony-forge-token-value",
				"SYMPHONY_GITHUB_TOKEN_FILE=/private/reserved-forge-token-path",
			},
		},
		{
			name:       "filter 2 blocks the caller's own extra names",
			extraNames: []string{"CALLER_BLOCKED", "  PADDED_NAME  ", "   "},
			removed: []string{
				"LINEAR_API_KEY=reserved-linear-key-value",
				"SYMPHONY_LINEAR_API_KEY_FILE=/private/reserved-linear-key-path",
				"GITHUB_TOKEN=reserved-forge-token-value",
				"SYMPHONY_GITHUB_TOKEN=reserved-symphony-forge-token-value",
				"SYMPHONY_GITHUB_TOKEN_FILE=/private/reserved-forge-token-path",
				"CALLER_BLOCKED=caller-blocked-value",
				"PADDED_NAME=padded-name-value",
			},
		},
		{
			// The file-form credential reference is what only a name filter can
			// reach: the variable holds a path, so no value filter matches it
			// and no reserved name knows it (PMR-80).
			name:     "filters 2 and 3 block the configured names and values",
			settings: settings,
			removed: []string{
				"LINEAR_API_KEY=reserved-linear-key-value",
				"SYMPHONY_LINEAR_API_KEY_FILE=/private/reserved-linear-key-path",
				"GITHUB_TOKEN=reserved-forge-token-value",
				"SYMPHONY_GITHUB_TOKEN=reserved-symphony-forge-token-value",
				"SYMPHONY_GITHUB_TOKEN_FILE=/private/reserved-forge-token-path",
				"CONFIGURED_NAME=configured-name-value",
				"PADDED_NAME=padded-name-value",
				"CONFIGURED_FILE=/private/configured-key-path",
				"INNOCENT_LOOKING=Bearer configured-secret-value",
			},
		},
		{
			// Filter 4 alone: the credential a bound provider holds, under a
			// name no list mentions and with no configured value at all.
			name:    "filter 4 blocks a value only the bound providers know",
			matcher: provider,
			removed: []string{
				"LINEAR_API_KEY=reserved-linear-key-value",
				"SYMPHONY_LINEAR_API_KEY_FILE=/private/reserved-linear-key-path",
				"GITHUB_TOKEN=reserved-forge-token-value",
				"SYMPHONY_GITHUB_TOKEN=reserved-symphony-forge-token-value",
				"SYMPHONY_GITHUB_TOKEN_FILE=/private/reserved-forge-token-path",
				"PROVIDER_HELD=prefix-provider-token-suffix",
			},
		},
		{
			name:       "all four parts together leave only what a child is meant to inherit",
			extraNames: []string{"CALLER_BLOCKED"},
			settings:   settings,
			matcher:    provider,
			removed: []string{
				"LINEAR_API_KEY=reserved-linear-key-value",
				"SYMPHONY_LINEAR_API_KEY_FILE=/private/reserved-linear-key-path",
				"GITHUB_TOKEN=reserved-forge-token-value",
				"SYMPHONY_GITHUB_TOKEN=reserved-symphony-forge-token-value",
				"SYMPHONY_GITHUB_TOKEN_FILE=/private/reserved-forge-token-path",
				"CALLER_BLOCKED=caller-blocked-value",
				"CONFIGURED_NAME=configured-name-value",
				"PADDED_NAME=padded-name-value",
				"CONFIGURED_FILE=/private/configured-key-path",
				"INNOCENT_LOOKING=Bearer configured-secret-value",
				"PROVIDER_HELD=prefix-provider-token-suffix",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kept := Filter(entries, tc.extraNames, tc.settings, tc.matcher)
			for _, entry := range entries {
				want := !slices.Contains(tc.removed, entry)
				if got := slices.Contains(kept, entry); got != want {
					if want {
						t.Fatalf("the filter removed %q, which no part of it covers", entry)
					}
					t.Fatalf("the child would inherit %q", entry)
				}
			}
			// Nothing is invented: the result is a subset of what came in, so a
			// caller cannot be handed a credential the host never held.
			for _, entry := range kept {
				if !slices.Contains(entries, entry) {
					t.Fatalf("the filter produced an entry no caller supplied: %q", entry)
				}
			}
		})
	}
}

// TestANilMatcherIsASupportedInput pins the contract PMR-113's workspace hooks
// depend on: a caller spawned outside a session has no capability bindings to
// build filter 4 from, and must be able to say so rather than discover by
// accident that nil happens to work. Filters 1 through 3 still apply in full;
// only the provider-held credential goes uncovered, which is what having no
// session means.
func TestANilMatcherIsASupportedInput(t *testing.T) {
	entries := []string{
		"GITHUB_TOKEN=reserved-forge-token-value",
		"CONFIGURED_NAME=configured-name-value",
		"INNOCENT_LOOKING=Bearer configured-secret-value",
		"KEPT=ordinary-value",
	}
	kept := Filter(entries, nil, config.Settings{
		HostSecretEnvNames: []string{"CONFIGURED_NAME"},
		HostSecretValues:   []string{"configured-secret-value"},
	}, nil)
	if !slices.Equal(kept, []string{"KEPT=ordinary-value"}) {
		t.Fatalf("kept=%v want only the ordinary variable", kept)
	}
}

// TestABlankConfiguredValueDoesNotEmptyTheEnvironment covers the one input that
// turns a value filter into a deny-all: "" is contained in every value, so
// honouring it would hand every child an empty environment and break the CLI
// logins that must be inherited.
func TestABlankConfiguredValueDoesNotEmptyTheEnvironment(t *testing.T) {
	kept := Filter([]string{"KEPT=ordinary-value"}, nil,
		config.Settings{HostSecretValues: []string{"", "unmatched-secret"}}, nil)
	if !slices.Equal(kept, []string{"KEPT=ordinary-value"}) {
		t.Fatalf("kept=%v want the ordinary variable to survive a blank configured value", kept)
	}
}

// TestAMalformedEntryIsDroppedAndOnlyValuesReachTheMatcher pins the two cases
// that are not reachable through os.Environ(), which is why Filter takes an
// explicit entry list rather than reading the environment itself.
//
// The name half matters beyond tidiness: a matcher fed a whole entry would strip
// any variable whose *name* happened to contain a credential-shaped string,
// silently removing something the child needs.
func TestAMalformedEntryIsDroppedAndOnlyValuesReachTheMatcher(t *testing.T) {
	var offered []string
	kept := Filter(
		[]string{"MALFORMED_NO_EQUALS", "PMR94_KEEP=ordinary", "PMR94_SECRET=carries-the-token"},
		nil,
		config.Settings{HostSecretValues: []string{"unmatched-secret"}},
		func(candidate string) bool {
			offered = append(offered, candidate)
			return strings.Contains(candidate, "the-token")
		},
	)
	if slices.Contains(kept, "MALFORMED_NO_EQUALS") {
		t.Fatalf("an entry carrying no \"=\" was forwarded to the child: %v", kept)
	}
	if slices.Contains(kept, "PMR94_SECRET=carries-the-token") {
		t.Fatalf("a matched value survived: %v", kept)
	}
	if !slices.Contains(kept, "PMR94_KEEP=ordinary") {
		t.Fatalf("an ordinary variable was dropped: %v", kept)
	}
	// Only values, never names or whole entries: "MALFORMED_NO_EQUALS" must not
	// appear, and neither must "PMR94_SECRET=carries-the-token".
	for _, candidate := range offered {
		if strings.Contains(candidate, "PMR94_") || candidate == "MALFORMED_NO_EQUALS" {
			t.Fatalf("the matcher was offered a name or a whole entry: %q", candidate)
		}
	}
}

// TestFilterDoesNotRetainTheCallersSlices keeps one caller's blocked names from
// becoming another's. Both backends pass a slice they keep -- the constructor's
// secret names -- and internal/claude prepends its own name to it on every turn,
// so a filter that appended to either input would grow it turn after turn.
func TestFilterDoesNotRetainTheCallersSlices(t *testing.T) {
	extraNames := []string{"CALLER_BLOCKED"}
	settings := config.Settings{HostSecretEnvNames: []string{"CONFIGURED_NAME"}, HostSecretValues: []string{"secret"}}
	Filter([]string{"KEPT=ordinary-value"}, extraNames, settings, nil)
	if !slices.Equal(extraNames, []string{"CALLER_BLOCKED"}) ||
		!slices.Equal(settings.HostSecretEnvNames, []string{"CONFIGURED_NAME"}) ||
		!slices.Equal(settings.HostSecretValues, []string{"secret"}) {
		t.Fatalf("the filter mutated its caller's inputs: %v %+v", extraNames, settings)
	}
}
