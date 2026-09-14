//go:build vuln

package sandboxprofile

import (
	"slices"
	"testing"
)

// The shared host temp directory.
//
// Every sandbox gets a private per-launch temp dir, exported as $TMPDIR and
// granted read-write. That is what a confined process needs, and it is
// isolated: one launch cannot see another's.
//
// The baseline additionally grants the host's own /tmp read-write, which is
// not isolated from anything. Sandboxed processes run as the user's own uid,
// so a grant on the shared temp root means a confined agent can read, alter,
// or delete whatever any other omac session — or any other program on the
// machine — left there. omac's own per-workdir runtime directory lives there
// too, holding the bridge socket and sidecar pid files, which turns "shared
// scratch space" into a way to intercept another session's facade traffic.
//
// It is also unnecessary. When /tmp is not granted, the sandbox mounts a
// private tmpfs over it, so code that hardcodes /tmp keeps working without
// reaching the host's copy.
//
// Asserted against both platform baselines regardless of the host GOOS: this
// is a property of what omac ships, and a Linux developer must not be able to
// weaken the macOS grants without a test noticing.

// TestSecurityBaselineDoesNotGrantHostTmp asserts that no platform baseline
// grants write access to the shared host temp root.
func TestSecurityBaselineDoesNotGrantHostTmp(t *testing.T) {
	// "/private/tmp" is the same directory as "/tmp" on macOS, where /tmp is
	// a symlink; both spellings appear in the grant list and both have to go.
	shared := []string{"/tmp", "/private/tmp"}

	for name, b := range map[string]Baseline{
		"linux":  linuxBaseline(),
		"darwin": darwinBaseline(),
	} {
		// Control: the private per-launch temp dir stays writable. Without
		// it a baseline that granted no temp access at all would look like a
		// pass while breaking every harness that needs scratch space.
		if !slices.Contains(b.Write, "$TMPDIR") {
			t.Errorf("%s baseline no longer grants $TMPDIR write: the sandbox has no private scratch space, which is not the fix", name)
		}

		for _, p := range shared {
			if slices.Contains(b.Write, p) {
				t.Errorf("%s baseline grants write on %s: the confined agent shares the host temp directory with every other session on the machine, including omac's own runtime sockets", name, p)
			}
		}
	}
}
