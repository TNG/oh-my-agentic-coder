package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"

	"github.com/TNG/oh-my-agentic-coder/internal/approver"
	"github.com/TNG/oh-my-agentic-coder/internal/netprompt"
)

// newSidecarApprover builds the Approver the facade asks for endpoints
// declared approval: required. Without a dialog backend it still returns an
// Approver, which then denies every such request as unavailable, and says so
// once. scopeFor maps a route namespace to what verdicts are bound to.
func newSidecarApprover(stderr io.Writer, prefix string, scopeFor func(namespace string) string) *approver.Approver {
	cfg := approver.Config{
		Store:    approver.NewStore(""),
		ScopeFor: scopeFor,
		Logf:     func(format string, args ...any) { fmt.Fprintf(stderr, prefix+": "+format+"\n", args...) },
	}
	if ch, ok := netprompt.NewChooser(); ok {
		cfg.Chooser = ch
	} else {
		fmt.Fprintf(stderr, "%s: notice: no dialog backend available; sidecar endpoints declared approval: required are denied\n", prefix)
	}
	return approver.New(cfg)
}

// workdirScope is the identity "always" verdicts are bound to for a skill
// that runs in one project: a short hash of the absolute workdir.
func workdirScope(workdir string) string {
	h := sha256.Sum256([]byte(workdir))
	return "wd-" + hex.EncodeToString(h[:6])
}
