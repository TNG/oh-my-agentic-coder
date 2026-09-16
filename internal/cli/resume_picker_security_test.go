package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/session"
)

// TestSecurityResumePickerStripsControlSequences asserts that a session's
// title or id — read from the harness's own on-disk session store, which a
// prior (possibly compromised or foreign) session in the same harness can
// have written — cannot inject terminal control sequences into the resume
// picker's output.
func TestSecurityResumePickerStripsControlSequences(t *testing.T) {
	env, out, _, drain := newPipeEnv(t, "")
	env.Workdir = "/work"
	st := newStyler(env.Stdout)

	sessions := []session.Session{
		{ID: "ses_1", Title: "normal session", When: time.Now()},
		{ID: "ses_2\x1b[2J", Title: "hijacked\x1b[2J title", When: time.Now()},
	}
	renderSessions(env, st, "opencode", sessions)
	drain()
	rendered := out.String()

	// Control: both sessions are actually rendered (the fixture reaches the
	// vulnerable code, not a no-op), so a fix that renders nothing wouldn't
	// make this test pass for the wrong reason.
	if !strings.Contains(rendered, "normal session") || !strings.Contains(rendered, "hijacked") {
		t.Fatalf("control: expected session content missing from output: the fixture is broken, not the security property\n%q", rendered)
	}

	if strings.ContainsRune(rendered, 0x1b) {
		t.Errorf("resume picker output contains a raw ANSI escape from a session id/title: %q", rendered)
	}
}
