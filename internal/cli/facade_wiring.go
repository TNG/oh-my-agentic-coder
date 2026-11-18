package cli

import (
	"github.com/TNG/oh-my-agentic-coder/internal/facade"
	"github.com/TNG/oh-my-agentic-coder/internal/intent"
	"github.com/TNG/oh-my-agentic-coder/internal/sandboxrun"
)

// wireFacadeSandbox attaches the sandbox-aware facade endpoints
// (/sandbox/denied and /sandbox/intent) to f. Shared by `start` and
// `serve` so the two entry points cannot drift.
//
// When the sandbox is active it builds the protected-path checker from the
// plan's already-resolved policy profile — the set the agent queries via
// GET /sandbox/denied?path=X to tell a sandbox denial from a genuinely
// missing file. Taking the plan rather than a profile name is the fix for
// #173: the callers hold a LAUNCHER profile name ("builtin"), which is not
// a POLICY reference ("default"), and resolving the former as the latter
// always failed — leaving the endpoint 404 on every default launch. A plan
// without a usable policy is reported via warn rather than silently
// disabling the endpoint. The intent registry is always wired: in-memory,
// session-scoped, written by the agent via POST /sandbox/intent and read
// by the popup via GET.
//
// learnMode (`omac serve --learn`) is a launch fact the policy file cannot
// carry: it lifts every filesystem restriction in the child, so the static
// protected set must not be reported for that session.
func wireFacadeSandbox(f *facade.Facade, noSandbox, learnMode bool, plan sandboxPlan, warn func(format string, args ...any)) {
	if !noSandbox {
		protected := sessionProtectedPaths(learnMode, plan)
		switch {
		case protected != nil:
			f.ProtectedPathChecker = protected
			if learnMode {
				break
			}
			if d := plan.Policy.Denial; d != nil && d.FacadeNote != "" {
				f.DenialNote = d.FacadeNote
			}
		case plan.PolicyErr != nil:
			warn("omac: sandbox profile %q could not be resolved: %v; GET /sandbox/denied disabled",
				plan.PolicyRef, plan.PolicyErr)
		default:
			// An external launcher (nono), the no-sandbox debug shell, or
			// an unknown profile name: omac cannot see the policy, so it
			// cannot say which paths that sandbox protects.
			warn("omac: sandbox profile %q does not run omac's native sandbox; GET /sandbox/denied disabled",
				plan.Name)
		}
	}
	f.IntentRegistry = intent.New(intent.DefaultTTL)
}

// sessionProtectedPaths returns the paths the sandbox of a session with
// plan protects, or nil when omac cannot see that sandbox's policy. In a
// learn session nothing is protected.
func sessionProtectedPaths(learnMode bool, plan sandboxPlan) *sandboxrun.ProtectedPathSet {
	switch {
	case learnMode:
		return sandboxrun.UnrestrictedProtectedPathSet()
	case plan.Policy != nil:
		return sandboxrun.NewProtectedPathSet(plan.Policy)
	default:
		return nil
	}
}
