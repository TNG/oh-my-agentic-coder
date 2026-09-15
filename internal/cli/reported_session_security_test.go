//go:build vuln

package cli

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// TestSecurityReportedSessionIDRejectsControlSequences asserts that a
// session id posted to the harness-plugin-facing /__omac__/session endpoint
// cannot carry terminal control sequences into the post-exit continue hint.
//
// handleSession stores body.Session verbatim with no validation at all —
// not even a charset check — and reportedSession hands it straight back to
// whatever prints the resume hint.
//
// Driven via httptest.NewRecorder()/NewRequest() rather than a bound
// listener: handleSession is an ordinary http.HandlerFunc, so no socket is
// needed to exercise it.
func TestSecurityReportedSessionIDRejectsControlSequences(t *testing.T) {
	r := newStartReloaderForTest(t)

	// Control: an ordinary session id is recorded and returned, so the
	// mechanism itself works and a fix that rejects everything wouldn't
	// make this test pass for the wrong reason.
	ok := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/__omac__/session", strings.NewReader(`{"session":"ses_normal"}`))
	r.handleSession(ok, req)
	if got := r.reportedSession(); got != "ses_normal" {
		t.Fatalf("control: an ordinary session id was not recorded (%q): the fixture is broken, not the security property", got)
	}

	hostile := "ses_x\x1b[2J\rFAKE — approved"
	w := httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/__omac__/session", strings.NewReader(`{"session":`+jsonQuote(hostile)+`}`))
	r.handleSession(w, req)

	got := r.reportedSession()
	if strings.ContainsRune(got, 0x1b) || strings.ContainsRune(got, '\r') {
		t.Errorf("reportedSession() returned a session id with raw control bytes intact (%q): it is printed verbatim in the post-exit continue hint", got)
	}
}

func jsonQuote(s string) string {
	// Minimal JSON string quoting sufficient for this fixture's payload
	// (contains only ESC and CR beyond ASCII printable).
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '\r':
			b.WriteString(`\r`)
		case 0x1b:
			b.WriteString(`\u001b`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
