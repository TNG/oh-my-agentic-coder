package sandboxprofile

import (
	"sort"
	"strings"
)

// dangerousEnvExact are always dropped from the child environment,
// even when matched by allow_vars (nono's env_sanitization list plus
// the 1Password meta-secrets).
var dangerousEnvExact = map[string]bool{
	"BASH_ENV":                 true,
	"ENV":                      true,
	"CDPATH":                   true,
	"GLOBIGNORE":               true,
	"PROMPT_COMMAND":           true,
	"IFS":                      true,
	"PYTHONSTARTUP":            true,
	"PYTHONPATH":               true,
	"NODE_OPTIONS":             true,
	"NODE_PATH":                true,
	"PERL5OPT":                 true,
	"PERL5LIB":                 true,
	"RUBYOPT":                  true,
	"RUBYLIB":                  true,
	"GEM_PATH":                 true,
	"GEM_HOME":                 true,
	"JAVA_TOOL_OPTIONS":        true,
	"_JAVA_OPTIONS":            true,
	"DOTNET_STARTUP_HOOKS":     true,
	"GOFLAGS":                  true,
	"OP_SERVICE_ACCOUNT_TOKEN": true,
	"OP_CONNECT_TOKEN":         true,
	"OP_CONNECT_HOST":          true,
}

var dangerousEnvPrefixes = []string{
	"LD_",
	"DYLD_",
	"BASH_FUNC_",
	"OP_SESSION_",
}

// IsDangerousEnvVar reports whether key is on the always-drop blocklist.
func IsDangerousEnvVar(key string) bool {
	if dangerousEnvExact[key] {
		return true
	}
	for _, p := range dangerousEnvPrefixes {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}

// DangerousEnvBlocklist returns sorted copies of the always-drop env
// blocklist: exact names (e.g. "BASH_ENV") and prefix families (e.g.
// "LD_"). Provenance uses this to display the effective deny set; the
// returned slices are copies so callers cannot mutate the package
// state.
func DangerousEnvBlocklist() (exact []string, prefixes []string) {
	exact = make([]string, 0, len(dangerousEnvExact))
	for k := range dangerousEnvExact {
		exact = append(exact, k)
	}
	sort.Strings(exact)
	prefixes = append([]string(nil), dangerousEnvPrefixes...)
	sort.Strings(prefixes)
	return exact, prefixes
}

// envVarAllowed checks key against the allow_vars list (exact names or
// trailing-* prefixes). An empty list allows everything.
func envVarAllowed(key string, allowVars []string) bool {
	if len(allowVars) == 0 {
		return true
	}
	for _, pat := range allowVars {
		if pat == "*" {
			return true
		}
		if strings.HasSuffix(pat, "*") {
			if strings.HasPrefix(key, pat[:len(pat)-1]) {
				return true
			}
			continue
		}
		if key == pat {
			return true
		}
	}
	return false
}

// FilterEnv builds the child environment from scratch:
//  1. drop blocklisted vars,
//  2. apply the optional allow_vars allowlist,
//  3. overlay injected vars (which bypass both filters and win over
//     inherited values).
//
// environ entries are "KEY=VALUE" as from os.Environ().
func FilterEnv(environ []string, allowVars []string, injected map[string]string) []string {
	out := make([]string, 0, len(environ)+len(injected))
	for _, kv := range environ {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			continue
		}
		key := kv[:eq]
		if IsDangerousEnvVar(key) {
			continue
		}
		if !envVarAllowed(key, allowVars) {
			continue
		}
		if _, overridden := injected[key]; overridden {
			continue // injected value wins
		}
		out = append(out, kv)
	}
	for k, v := range injected {
		out = append(out, k+"="+v)
	}
	return out
}
