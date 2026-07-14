//go:build e2e

package e2e

import "testing"

// These tests exercise, against synthetic probe output (no live
// agent/sandbox), the exact marker-absence checks that back
// assertFilesystemReadDenied/assertFilesystemWriteDenied/
// assertSymlinkEscapeDenied/assertFilesystemAllowed
// (fsReadLeaked/fsWriteLeaked/symlinkEscapeLeaked/fsAllowDenied in
// e2e_test.go). Before this fix, those assertions passed as long as ANY
// denial substring appeared ANYWHERE in the whole probe section — so one
// leaked path among many probed ones slipped through undetected as long as
// a different path in the same section was denied. That is exactly the
// kind of silently-neutered assertion issue #66's "mutation-test the
// tests" idea warns about: these cases mutation-test the decision logic
// itself against a single-path regression. fsAllowDenied is the positive
// mirror of the same lesson: one legitimate path silently losing access
// must not be masked by the other legitimate paths still working.
//
// They call the pure fsReadLeaked/fsWriteLeaked/symlinkEscapeLeaked/
// fsAllowDenied predicates directly (not the *testing.T-based assert*
// wrappers) so a fixture that is expected to represent a leak/denial
// doesn't itself make this test fail — that boolean/string is exactly
// what's under test here.

func TestFsReadLeakedCatchesSingleLeak(t *testing.T) {
	allDenied := "=== PROBE: fs_read ===\n" +
		"--- /etc/shadow ---: cat: /etc/shadow: Permission denied\n" +
		"--- ~/.ssh/id_rsa ---: cat: /home/u/.ssh/id_rsa: Permission denied\n" +
		"--- /var/run/docker.sock ---: cat: /var/run/docker.sock: No such file or directory\n" +
		"=== END: fs_read ===\n"
	if fsReadLeaked(allDenied) {
		t.Error("expected no leak detected when every probed path is denied")
	}

	// One path leaked (READABLE) while the others in the same section
	// are still denied. A whole-section "any denial substring present"
	// check would have missed this.
	oneLeaked := "=== PROBE: fs_read ===\n" +
		"--- /etc/shadow ---: cat: /etc/shadow: Permission denied\n" +
		"--- ~/.ssh/id_rsa ---: READABLE (sandbox did not block)\n" +
		"--- /var/run/docker.sock ---: cat: /var/run/docker.sock: No such file or directory\n" +
		"=== END: fs_read ===\n"
	if !fsReadLeaked(oneLeaked) {
		t.Error("expected a leak to be detected when one of several probed paths leaked")
	}

	// A READABLE substring outside the fs_read section (e.g. in a later
	// probe's own commentary) must not cause a false positive — the
	// check must only look inside the extracted fs_read section.
	outsideSection := "=== PROBE: fs_read ===\n" +
		"--- /etc/shadow ---: cat: /etc/shadow: Permission denied\n" +
		"=== END: fs_read ===\n" +
		"=== PROBE: env ===\n" +
		"unrelated mention of READABLE here\n" +
		"=== END: env ===\n"
	if fsReadLeaked(outsideSection) {
		t.Error("a READABLE substring outside the fs_read section must not be treated as a leak")
	}
}

func TestFsWriteLeakedCatchesSingleLeak(t *testing.T) {
	allDenied := "=== PROBE: fs_write ===\n" +
		"--- write /etc/omac-audit-test ---: sh: 1: cannot create /etc/omac-audit-test: Permission denied\n" +
		"--- write /usr/omac-audit-test ---: sh: 1: cannot create /usr/omac-audit-test: Read-only file system\n" +
		"=== END: fs_write ===\n"
	if fsWriteLeaked(allDenied) {
		t.Error("expected no leak detected when every probed write is denied")
	}

	// A successful write is silent at the shell level (no denial message
	// to find), so the pre-fix "any denial substring present" check had
	// no leak signal to see at all here. The WRITABLE marker (from
	// probe_write) is what makes the leak detectable in the first place.
	oneLeaked := "=== PROBE: fs_write ===\n" +
		"--- write /etc/omac-audit-test ---: WRITABLE (sandbox did not block)\n" +
		"--- write /usr/omac-audit-test ---: sh: 1: cannot create /usr/omac-audit-test: Read-only file system\n" +
		"=== END: fs_write ===\n"
	if !fsWriteLeaked(oneLeaked) {
		t.Error("expected a leak to be detected when one of several probed writes leaked")
	}
}

func TestFsAllowDeniedCatchesSingleDenial(t *testing.T) {
	labels := []string{
		"--- write workdir file ---",
		"--- read workdir file ---",
		"--- write $HOME/.cache file ---",
		"--- write ${TMPDIR:-/tmp} file ---",
	}

	allAllowed := "=== PROBE: fs_allow ===\n" +
		"--- write workdir file ---: WRITABLE (sandbox did not block)\n" +
		"--- read workdir file ---: READABLE (sandbox did not block)\n" +
		"--- write $HOME/.cache file ---: WRITABLE (sandbox did not block)\n" +
		"--- write ${TMPDIR:-/tmp} file ---: WRITABLE (sandbox did not block)\n" +
		"=== END: fs_allow ===\n"
	if denied := fsAllowDenied(allAllowed, labels); denied != "" {
		t.Errorf("expected no denial when every legitimate path succeeded, got %q", denied)
	}

	// One path denied (e.g. an over-broad ProtectedPaths change shadowed
	// $HOME/.cache) while the other three still succeed. A whole-section
	// "any WRITABLE/READABLE marker present" check would have missed
	// this, mirroring the exact bug fsReadLeaked/fsWriteLeaked fixed for
	// the negative assertions.
	oneDenied := "=== PROBE: fs_allow ===\n" +
		"--- write workdir file ---: WRITABLE (sandbox did not block)\n" +
		"--- read workdir file ---: READABLE (sandbox did not block)\n" +
		"--- write $HOME/.cache file ---: sh: 1: cannot create /home/u/.cache/omac-audit-allow-test: Permission denied\n" +
		"--- write ${TMPDIR:-/tmp} file ---: WRITABLE (sandbox did not block)\n" +
		"=== END: fs_allow ===\n"
	if denied := fsAllowDenied(oneDenied, labels); denied == "" {
		t.Error("expected a denial to be detected when one of several legitimate paths was blocked")
	}
}

func TestSymlinkEscapeLeakedCatchesEitherHalf(t *testing.T) {
	bothDenied := "=== PROBE: symlink ===\n" +
		"--- read via symlink to ~/.ssh/id_rsa ---: cat: ./omac-audit-symlink-ssh: Permission denied\n" +
		"--- write via symlink to /etc/omac-audit-test ---: sh: 1: cannot create ./omac-audit-symlink-write: Permission denied\n" +
		"=== END: symlink ===\n"
	if readLeaked, writeLeaked := symlinkEscapeLeaked(bothDenied); readLeaked || writeLeaked {
		t.Errorf("expected no leak when both halves are denied, got read=%v write=%v", readLeaked, writeLeaked)
	}

	readLeakedOutput := "=== PROBE: symlink ===\n" +
		"--- read via symlink to ~/.ssh/id_rsa ---: READABLE (sandbox did not block)\n" +
		"--- write via symlink to /etc/omac-audit-test ---: sh: 1: cannot create ./omac-audit-symlink-write: Permission denied\n" +
		"=== END: symlink ===\n"
	if readLeaked, writeLeaked := symlinkEscapeLeaked(readLeakedOutput); !readLeaked || writeLeaked {
		t.Errorf("expected read leak only, got read=%v write=%v", readLeaked, writeLeaked)
	}

	writeLeakedOutput := "=== PROBE: symlink ===\n" +
		"--- read via symlink to ~/.ssh/id_rsa ---: cat: ./omac-audit-symlink-ssh: Permission denied\n" +
		"--- write via symlink to /etc/omac-audit-test ---: WRITABLE (sandbox did not block)\n" +
		"=== END: symlink ===\n"
	if readLeaked, writeLeaked := symlinkEscapeLeaked(writeLeakedOutput); readLeaked || !writeLeaked {
		t.Errorf("expected write leak only, got read=%v write=%v", readLeaked, writeLeaked)
	}
}
