//go:build vuln

package sandboxprofile

import (
	"slices"
	"testing"
)

// The shared host temp directory.
//
// On Linux every sandbox gets a private tmpfs over /tmp so confined code that
// hardcodes /tmp writes to a private namespace, not the shared host root. The
// per-launch sandbox scratch dir ($TMPDIR, e.g. /tmp/omac-sandbox-tmp-*) is
// granted explicitly via {{tmpdir_flags}}, so the Linux baseline no longer
// needs $TMPDIR in its Write list.
//
// On macOS there is no tmpfs equivalent via Seatbelt, so the baseline still
// grants $TMPDIR write (which resolves to the per-launch dir). The test
// asserts both: Linux drops the blanket $TMPDIR baseline write, macOS keeps
// it — and neither platform grants bare /tmp or /private/tmp write.

// TestSecurityBaselineDoesNotGrantHostTmp asserts that no platform baseline
// grants write access to the shared host temp root.
func TestSecurityBaselineDoesNotGrantHostTmp(t *testing.T) {
	// "/private/tmp" is the same directory as "/tmp" on macOS, where /tmp is
	// a symlink; both spellings appear in the grant list and both have to go.
	shared := []string{"/tmp", "/private/tmp"}

	// Control for macOS: $TMPDIR write stays (no tmpfs on macOS). A darwin
	// baseline without it would leave harnesses with no writable scratch dir.
	if !slices.Contains(darwinBaseline().Write, "$TMPDIR") {
		t.Fatal("darwin baseline no longer grants $TMPDIR write: harnesses on macOS have no scratch space")
	}
	// Control for Linux: $TMPDIR write is intentionally absent — scratch space
	// comes from the explicit per-launch {{tmpdir_flags}} grant, and bwrap
	// mounts a private tmpfs over /tmp for everything else.
	if slices.Contains(linuxBaseline().Write, "$TMPDIR") {
		t.Error("linux baseline grants $TMPDIR write: this re-exposes the host /tmp " +
			"(when $TMPDIR=/tmp) and suppresses the private --tmpfs /tmp that protects it")
	}

	for name, b := range map[string]Baseline{
		"linux":  linuxBaseline(),
		"darwin": darwinBaseline(),
	} {
		for _, p := range shared {
			if slices.Contains(b.Write, p) {
				t.Errorf("%s baseline grants write on %s: the confined agent shares the host temp directory with every other session on the machine, including omac's own runtime sockets", name, p)
			}
		}
	}
}
