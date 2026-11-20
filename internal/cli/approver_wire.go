package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"path/filepath"

	"github.com/TNG/oh-my-agentic-coder/internal/approver"
	"github.com/TNG/oh-my-agentic-coder/internal/netprompt"
)

// newSidecarApprover builds the Approver the facade asks for requests that
// ask: endpoints declared approval: required, and routes an approvals block
// does not list. Without a dialog backend it still returns an
// Approver, which then denies every such request as unavailable, and says so
// once. scopeFor maps a route namespace to what answers are bound to.
func newSidecarApprover(stderr io.Writer, prefix string, scopeFor func(namespace string) approver.Scope) *approver.Approver {
	cfg := approver.Config{
		Store:    approver.NewStore(""),
		ScopeFor: scopeFor,
		Logf:     func(format string, args ...any) { fmt.Fprintf(stderr, prefix+": "+format+"\n", args...) },
	}
	ch, ok := netprompt.NewChooser()
	if stub := ch.Stub(); stub != "" {
		// A test hook. Left exported by accident, it would answer every
		// dialog silently, so say so on every start.
		fmt.Fprintf(stderr, "%s: warning: OMAC_APPROVAL_STUB is set; every sidecar approval is answered %q without a dialog\n", prefix, stub)
	}
	if ok {
		cfg.Chooser = ch
	} else {
		fmt.Fprintf(stderr, "%s: notice: no dialog backend available; sidecar requests that need approval are denied\n", prefix)
	}
	return approver.New(cfg)
}

// projectScope is what "always" answers are bound to for a skill that runs
// in one project: a hash of the project directory, with symlinks resolved so
// that `omac start` and `omac serve` agree on one project however its path
// was spelled. `omac start` mounts every skill for the one project it runs
// in, user-global skills too, so all its answers use this scope.
func projectScope(dir string) approver.Scope {
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	h := sha256.Sum256([]byte(dir))
	return approver.Scope{Key: "wd-" + hex.EncodeToString(h[:12]), Dir: dir}
}

// globalScope is the scope of a user-global skill in `omac serve`: one route
// serves every project, so its answers are shared by all of them.
var globalScope = approver.Scope{Key: "global"}
