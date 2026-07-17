package cli

import (
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
func wireFacadeSandbox(f *facade.Facade, noSandbox bool, profName string, warn func(format string, args ...any)) {
	if !noSandbox {
		if prof, _, err := sandboxprofile.Resolve(profName); err == nil {
			f.ProtectedPathChecker = sandboxrun.NewProtectedPathSet(prof)
			if prof.Denial != nil && prof.Denial.FacadeNote != "" {
				f.DenialNote = prof.Denial.FacadeNote
			}
		} else {
			warn("omac: sandbox profile %q could not be resolved: %v; GET /sandbox/denied disabled", profName, err)
		}
	}
	f.IntentRegistry = intent.New(intent.DefaultTTL)
}
