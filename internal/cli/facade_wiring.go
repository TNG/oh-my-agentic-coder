package cli

import (
	"github.com/tngtech/oh-my-agentic-coder/internal/config"
	"github.com/tngtech/oh-my-agentic-coder/internal/facade"
	"github.com/tngtech/oh-my-agentic-coder/internal/intent"
	"github.com/tngtech/oh-my-agentic-coder/internal/sandboxprofile"
	"github.com/tngtech/oh-my-agentic-coder/internal/sandboxrun"
)

// wireFacadeSandbox attaches the sandbox-aware facade endpoints
// (/sandbox/denied and /sandbox/intent) to f. Shared by `start` and
// `serve` so the two entry points cannot drift.
//
// When the sandbox is active it re-resolves the profile — cheap (path
// expansion only, no existence walks) since the full grant resolution
// happens inside the `omac sandbox run` child — to build the
// protected-path checker the agent queries via GET /sandbox/denied?path=X
// to tell a sandbox denial from a genuinely missing file. A resolve
// failure is reported via warn rather than silently disabling the
// endpoint. The intent registry is always wired: in-memory,
// session-scoped, written by the agent via POST /sandbox/intent and read
// by the popup via GET.
//
// prof is the EFFECTIVE launcher profile (lc.Sandbox.Profiles[profName]
// with the compiled-in default when absent). For a native `{{self}}
// sandbox run` profile (builtin), the filesystem profile the child
// resolves is the referenced `--profile` (e.g. "default") — NOT the
// launcher-profile name ("builtin" has no file). Resolving the child
// ref keeps the checker in lock-step with the enforcement the agent
// actually runs under; a non-native profile (nono) is opaque, so the
// name-based filesystem resolve is tried as a best effort.
func wireFacadeSandbox(f *facade.Facade, noSandbox bool, profName string, prof config.SandboxProfile, warn func(format string, args ...any)) {
	if !noSandbox {
		ref := profName
		if profileRunsNativeSandbox(prof) {
			ref = "default"
			if r, ok := inspectBuiltinProfileRef(prof.Command); ok {
				ref = r
			}
		}
		if p, _, err := sandboxprofile.ResolveReadOnly(ref); err == nil {
			f.ProtectedPathChecker = sandboxrun.NewProtectedPathSet(p)
			if p.Denial != nil && p.Denial.FacadeNote != "" {
				f.DenialNote = p.Denial.FacadeNote
			}
		} else {
			warn("omac: sandbox profile %q could not be resolved: %v; GET /sandbox/denied disabled", ref, err)
		}
	}
	f.IntentRegistry = intent.New(intent.DefaultTTL)
}
