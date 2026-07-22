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

// BaseAllowVars is the always-on tier of the env allowlist: the
// operational minimum a sandboxed process needs to run at all and which
// is never desirable to withhold. EffectiveAllowVars merges these into
// every restrictive profile's allow_vars, so a profile author cannot
// accidentally strip them (COLORTERM was the motivating gap — issue
// #139). A profile can ADD to the allowlist; it cannot subtract from
// this base. All entries are non-secret.
//
// Entries are exact names or trailing-* prefixes (see envVarAllowed).
// Injected variables (HTTP(S)_PROXY, OMAC_* skill bases) bypass the
// allowlist entirely via the injected overlay in FilterEnv, but OMAC_*
// is listed too because omac sets several OMAC_* vars in its own process
// environment before exec (see internal/cli/start.go), and those reach
// the child through inheritance rather than the injected overlay.
// COLORTERM is the truecolor-capability hint, paired with TERM.
func BaseAllowVars() []string {
	return []string{
		"OMAC_*",
		"HOME",
		"PATH",
		"PWD",
		"TMPDIR",
		"LANG",
		"LC_*",
		"TERM",
		"COLORTERM",
	}
}

// convenienceAllowVars is the removable tier scaffolded into default.json
// on top of BaseAllowVars. Unlike the base, a profile MAY omit these:
// USER/LOGNAME (default identity for git/npm/ssh — also forwarded to the
// facade via FacadeConfig.BaseEnvPassthrough), TZ (date formatting),
// EDITOR/VISUAL (harnesses that shell out to an editor), SHELL, the
// XDG_*_HOME config/data/state dirs, and NPM_CONFIG_PREFIX. All are
// non-secret.
func convenienceAllowVars() []string {
	return []string{
		"SHELL",
		"USER",
		"LOGNAME",
		"TZ",
		"EDITOR",
		"VISUAL",
		"XDG_CONFIG_HOME",
		"XDG_DATA_HOME",
		"XDG_STATE_HOME",
		"NPM_CONFIG_PREFIX",
	}
}

// DefaultAllowVars is the full allowlist compiled into DefaultProfile and
// scaffolded into default.json: the always-on BaseAllowVars followed by
// the removable convenience vars. Harness-specific auth variables (e.g.
// the LLM provider token) are NOT here — they are merged in per-harness
// at launch via Harness.SandboxEnvAllow (mirroring how SandboxDirs are
// injected).
func DefaultAllowVars() []string {
	return append(BaseAllowVars(), convenienceAllowVars()...)
}

// EffectiveAllowVars returns the allow_vars actually granted for a
// profile — the allow side only; removals are applied separately by
// FilterEnv via deny_vars (see FilterEnv and EnvVarMatches).
//
//   - an explicit "*" wildcard means "grant every non-blocklisted var"
//     and is returned unchanged — the only sanctioned inherit-all opt-in;
//   - an EMPTY list is NOT inherit-all. Since #102/#111 an empty allow_vars
//     is a misconfiguration that resolves to the operational defaults
//     (DefaultAllowVars), never the pre-#102 inherit-everything. cmd/serve
//     and cmd/start already seed this at launch; resolving it here too
//     closes the gap for a bare `omac sandbox run` against an empty
//     profile, so the policy holds regardless of entry point;
//   - a non-empty list has the full DefaultAllowVars merged in, so every
//     operational default (base + convenience) is granted by default
//     regardless of what the profile itself lists (COLORTERM was the
//     motivating gap — issue #139). A profile removes a default it does
//     not want with deny_vars, not by omitting it here.
//
// The returned slice is a fresh copy — default entries first, then the
// profile's own additions with duplicates removed — so callers may retain
// or mutate it without touching the profile.
func EffectiveAllowVars(profileAllow []string) []string {
	for _, v := range profileAllow {
		if v == "*" {
			return profileAllow
		}
	}
	if len(profileAllow) == 0 {
		return DefaultAllowVars()
	}
	defs := DefaultAllowVars()
	seen := make(map[string]bool, len(defs)+len(profileAllow))
	out := make([]string, 0, len(defs)+len(profileAllow))
	for _, v := range defs {
		seen[v] = true
		out = append(out, v)
	}
	for _, v := range profileAllow {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// DeniedBaseVars returns the BaseAllowVars entries that denyVars would
// remove. Denying a base var is permitted (deny_vars always wins — see
// FilterEnv) but is almost never intended: the base is the operational
// minimum a sandboxed process needs to run. Callers (launch path, doctor)
// use this to warn rather than block. Matching is pattern-overlap so a
// prefix deny (e.g. "LC_*", or a bare "*") flags the base entries it
// shadows.
func DeniedBaseVars(denyVars []string) []string {
	if len(denyVars) == 0 {
		return nil
	}
	var out []string
	for _, b := range BaseAllowVars() {
		for _, d := range denyVars {
			if patternsOverlap(d, b) {
				out = append(out, b)
				break
			}
		}
	}
	return out
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

// EnvVarMatches reports whether key matches any pattern in pats. Each
// pattern is an exact name ("HOME") or a trailing-* prefix ("OMAC_*",
// matched from the key start); a bare "*" matches everything. An empty
// pats list matches nothing.
func EnvVarMatches(key string, pats []string) bool {
	for _, pat := range pats {
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

// patternsOverlap reports whether two allow/deny patterns can match a
// common key. Used by DeniedBaseVars to detect a deny that shadows a
// base entry even when either side is a trailing-* prefix.
func patternsOverlap(a, b string) bool {
	if a == "*" || b == "*" {
		return true
	}
	aStar := strings.HasSuffix(a, "*")
	bStar := strings.HasSuffix(b, "*")
	switch {
	case aStar && bStar:
		pa, pb := a[:len(a)-1], b[:len(b)-1]
		return strings.HasPrefix(pa, pb) || strings.HasPrefix(pb, pa)
	case aStar:
		return strings.HasPrefix(b, a[:len(a)-1])
	case bStar:
		return strings.HasPrefix(a, b[:len(b)-1])
	default:
		return a == b
	}
}

// envVarAllowed checks key against the allow_vars list. An empty list
// allows everything (the low-level inherit-all primitive; the policy
// layer in EffectiveAllowVars never hands FilterEnv an empty list).
func envVarAllowed(key string, allowVars []string) bool {
	if len(allowVars) == 0 {
		return true
	}
	return EnvVarMatches(key, allowVars)
}

// FilterEnv builds the child environment from scratch:
//  1. drop the hardcoded danger blocklist,
//  2. apply the optional allow_vars allowlist,
//  3. overlay injected vars (which win over inherited values),
//  4. LAST, drop everything matching denyVars.
//
// deny_vars is applied last, over the whole result, so it wins over the
// allowlist, over "*", AND over the injected overlay: a profile can drop
// an injected var (e.g. HTTP_PROXY or a tool-cache path) just as it drops
// an inherited one. This is deliberately powerful — denying the proxy or
// cache injections can break networking or cache isolation — so it is the
// profile author's explicit choice (the launch path and `omac doctor`
// warn when deny_vars removes a base operational var; see DeniedBaseVars).
//
// environ entries are "KEY=VALUE" as from os.Environ().
func FilterEnv(environ []string, allowVars, denyVars []string, injected map[string]string) []string {
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
	if len(denyVars) == 0 {
		return out
	}
	kept := out[:0]
	for _, kv := range out {
		eq := strings.IndexByte(kv, '=')
		if eq > 0 && EnvVarMatches(kv[:eq], denyVars) {
			continue
		}
		kept = append(kept, kv)
	}
	return kept
}
