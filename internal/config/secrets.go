package config

import (
	"sort"
	"strings"
)

// reservedSecretEnvNames is the fixed half of the host credential filter: names
// that never reach a child Symphony spawns, whatever a workflow says. They are
// the documented variables Symphony's own tracker and forge credentials are
// read from, and an agent reaches those providers through bounded capabilities,
// never directly.
//
// It lives beside HostSecretEnvNames and HostSecretValues, and beside the
// loader that derives both, because all of them are one policy -- see
// ReservedSecretEnvNames -- and that policy is a property of the trust boundary,
// not of any one child. One function reads it, hostenv.Filter, so a name added
// or removed cannot apply to one child Symphony spawns and not another.
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
// This comment is also the one description of how host credentials are kept out
// of a process Symphony spawns. hostenv.Filter is the one implementation of it,
// and nothing else documents it separately: the description lives here, beside
// the names and the loader that derives the rest, and the loop lives there,
// where every launcher can reach it without depending on a session.
//
// "Child" here means every process Symphony starts, not only an agent backend.
// There are three: the Codex app-server, each Claude turn, and a WORKFLOW.md
// workspace hook (workspace.Local.hook -- after_create, before_run, after_run,
// before_remove). The hook is the one that reads as an exception and is not:
// its script is repository-owned policy, but it runs in the agent's own
// worktree, so it can invoke anything the agent committed there, outside the
// agent sandbox. It ran with the daemon's complete environment until PMR-113,
// which is what a doctrine that enumerated only the backends could not make
// visible. Adding a fourth kind of child means adding it here too.
//
// A caller filters the inherited environment through four filters, and each
// one exists because the others cannot cover it:
//
//  1. ReservedSecretEnvNames removes these five documented names, whatever the
//     workflow configures. It is the only filter that applies to a workflow
//     with no credential reference at all -- one whose tracker key is passed in
//     some other way, so filters 2 and 3 are both empty -- and it covers the two
//     documented *_FILE names even when nothing references them.
//  2. Settings.HostSecretEnvNames, plus whatever names the caller blocks of
//     its own, removes the variables this workflow actually references. Filter
//     1 cannot: those names are repository-chosen. It is also the only filter
//     of any kind that covers a repository-chosen credential *file path*: for
//     the api_key_file and token_file forms the variable holds a path, not the
//     credential, so filter 3 never matches it and filter 1 does not know its
//     name (PMR-80).
//  3. Settings.HostSecretValues removes any variable whose value *contains* a
//     configured credential, under any name. Neither name filter can: an
//     inherited variable Symphony has never heard of can still carry the
//     credential, plain or wrapped in something like "Bearer <token>". For a
//     Settings produced by Load this is the broadest filter of the four --
//     resolveProvider writes resolved api_key_file contents back into
//     provider["api_key"], and decodeGitHub disables the integration outright
//     for a literal inline token, so hostSecretValues sees both credentials in
//     resolved form.
//  4. capability.SecretMatcher removes the credentials the providers bound to
//     this run actually hold, asking every one of them. Because of what filter 3
//     just resolved, no *loadable* configuration makes this the only filter that
//     catches a credential today: for now it is defence-in-depth against a
//     divergence between the two, and against a Settings assembled by anything
//     other than Load, which carries no HostSecretValues at all. It stops being
//     merely that as soon as a backend relaunches per turn with providers bound:
//     a launcher re-reads the live settings callback for filters 2 and 3 on
//     every turn, while a provider *session* holds the credential it froze at
//     session build, so a reload that rotates a credential mid-run leaves the
//     value the frozen session still authenticates with covered by nothing else.
//     internal/claude spawns one process per turn, which is exactly that shape.
//     Both sides of that divergence are covered rather than one: a bound
//     githubhost.Manager is asked too, and it reads its callback live, so the
//     rotated value is stripped alongside the frozen one. See SecretMatcher for
//     why the Linear side has no such pair. It is the one filter a caller may
//     omit, because it is the one that needs a session: a process Symphony
//     spawns outside any session -- a workspace hook -- has no bindings to build
//     a matcher from and passes none, so it gets filters 1 through 3 and forgoes
//     only a credential held by a live provider under a name and value no
//     configuration mentions.
//
// A credential the caller deliberately hands over -- the Claude backend's
// capability endpoint token -- is appended after filtering, so no filter can
// strip it and no inherited value can pre-empt it. That is the caller's own
// business, not the filter's.
//
// Everything else is inherited on purpose: both CLIs authenticate through the
// operator's own login, which lives in the home directory they read.
//
// Each numbered filter is proven once over hostenv.Filter, in
// TestFilterAppliesEveryPartOfTheHostCredentialFilter, and each launcher is
// proven to reach it with the whole of what it must contribute, because a hole
// would be silent either way: see TestNoHostCredentialReachesTheChildEnvironment
// in internal/codex, TestHostSecretsNeverReachTheChild plus
// TestStartBindsTheHostProvidersAndTheirSecrets in internal/claude, and
// TestNoHostCredentialReachesAHook in internal/workspace. Filter 1's
// names are also the names internal/service writes into the LaunchAgent plist;
// TestReservedNamesCoverTheServiceCredentialVariables holds those two lists
// together.
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
