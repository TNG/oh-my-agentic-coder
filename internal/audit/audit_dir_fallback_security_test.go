package audit

import "testing"

// TestSecurityAuditDirNeverFallsBackToSharedTmp asserts that when no home
// directory can be resolved, New refuses to fall back to a shared, world-
// accessible temp root rather than silently writing the audit trail there.
//
// DefaultDir already reports this case via its second return value
// (persistNotGuaranteed=true), and New does warn — but it still proceeds to
// open the file under os.TempDir(), which on a shared multi-user host is
// exactly the class of writable-by-anyone location the runtime-dir and
// tool-cache findings in this suite are about.
func TestSecurityAuditDirNeverFallsBackToSharedTmp(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("XDG_STATE_HOME", "")

	dir, notGuaranteed := DefaultDir()

	// Control: DefaultDir does at least flag this case via its own return
	// value, so a fixture where nothing signals the fallback wouldn't be
	// testing the right thing.
	if !notGuaranteed {
		t.Fatalf("control: DefaultDir did not report persistNotGuaranteed with no home resolvable: the fixture is broken, not the security property")
	}

	a, err := New(Config{Enabled: true, Mode: ModeStart})
	if a != nil {
		defer a.Close()
	}

	if err == nil {
		t.Errorf("New succeeded with no home directory resolvable, writing the audit trail under the shared temp root %s "+
			"instead of refusing: a fail-open warning is not the same as refusing to write audit records where another local user can reach them", dir)
	}
}
