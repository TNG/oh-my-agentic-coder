//go:build vuln

package cli

import (
	"slices"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
)

// TestSecuritySessionIDCannotBecomeHarnessFlag asserts that a session id
// beginning with "-" cannot be interpreted as a flag by the harness's own
// argument parser when omac resumes it.
//
// No session-id grammar is validated anywhere in the tree: session ids are
// read straight from filenames (session.List), forwarded into
// buildResumeInnerArgs, and ultimately become elements of the sandboxed
// child's argv. codex's ResumeByIDArgs places the id as a bare positional
// token after "resume" with no "--" fence, so an id equal to a real codex
// flag (e.g. its own "--dangerously-bypass-approvals-and-sandbox") is
// indistinguishable, to codex's own parser, from that flag actually being
// passed.
func TestSecuritySessionIDCannotBecomeHarnessFlag(t *testing.T) {
	codex, ok := config.LookupHarness("codex")
	if !ok || codex.Session == nil || codex.Session.ResumeByIDArgs == nil {
		t.Fatal("codex harness or its resume-by-id builder is missing: the fixture is broken, not the security property")
	}

	// Control: an ordinary session id passes through as a plain positional
	// argument, so the mechanism itself works and a fix that breaks normal
	// resume wouldn't make this test pass for the wrong reason.
	normal := buildResumeInnerArgs(codex.Session, "018f2c3a-normal-id", nil)
	if !slices.Contains(normal, "018f2c3a-normal-id") {
		t.Fatalf("control: an ordinary session id was not passed through (%v): the fixture is broken, not the security property", normal)
	}

	hostile := "--dangerously-bypass-approvals-and-sandbox"
	got := buildResumeInnerArgs(codex.Session, hostile, nil)

	fenced := slices.Contains(got, "--")
	if !fenced && slices.Contains(got, hostile) {
		t.Errorf("buildResumeInnerArgs placed a flag-shaped session id %q into codex's argv with no \"--\" fence separating it from flag parsing: %v", hostile, got)
	}
}
