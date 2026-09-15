//go:build vuln

package audit

import (
	"os"
	"testing"
)

// TestSecurityAuditSinkDoesNotStaySilentlyDead asserts that once a fail-open
// sink has recorded one write failure, restoring the ability to write does
// not leave every subsequent event silently dropped for the rest of the
// process.
//
// fileSink.write sets broken=true on any error and, once set, returns
// errBroken unconditionally forever — no code path ever clears it. A single
// transient write failure (a momentarily full disk, a rotated-away log
// file) permanently disables auditing for the session, with only the one
// deduped warning TestFailOpenWarnsOnce pins as ever seen.
func TestSecurityAuditSinkDoesNotStaySilentlyDead(t *testing.T) {
	a, path := newTestAuditor(t, Config{Strict: false})
	au := a.(*auditor)

	_ = au.fileSink.f.Close()
	au.Emit(ControlMutation("reload", "/x", "ok"))

	// Restore a healthy writer at the same path, simulating the underlying
	// cause (full disk, rotated file) being resolved mid-session.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer f.Close()
	au.fileSink.f = f

	// Control: writing to the fresh handle directly (bypassing the sink's
	// own broken flag) succeeds, so the file itself is healthy and any
	// failure below is the sink's own bookkeeping, not the fixture.
	if _, werr := f.WriteString("{}\n"); werr != nil {
		t.Fatalf("control: the restored file handle itself cannot be written to: %v", werr)
	}

	err = au.fileSink.write([]byte(`{"probe":true}`))
	if err == nil {
		return // recovered — the property holds
	}
	if err == errBroken {
		t.Errorf("the sink still reports errBroken after its underlying file was restored to a healthy, writable handle: " +
			"once broken=true is set it is never cleared, so a transient failure disables auditing for the rest of the process")
	}
}
