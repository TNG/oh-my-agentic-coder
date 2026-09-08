package buildrun

import "testing"

// TestIsShimDir pins the shim-dir detection contract so the marker logic
// (resolved-path markers + bare-shims parent check) can be consolidated
// without changing behavior. The truth set is worked by hand:
//   - any resolved path passing through a version-manager tree is stripped
//     (over-match is harmless: the real JDK bin is prepended separately
//     after symlink resolution);
//   - a bare "shims"/"shims-bin" PATH entry qualifies only under a
//     version-manager parent (the same marker set as the path scan);
//   - unrelated dirs named shims (/usr/shims) are NOT shim dirs.
func TestIsShimDir(t *testing.T) {
	for _, tc := range []struct {
		dir  string
		want bool
	}{
		// Resolved-path markers.
		{"/home/u/.jenv/shims", true},
		{"/home/u/.jenv/versions/17/bin", true}, // over-match: passes through /.jenv/
		{"/home/u/.asdf/shims", true},
		{"/home/u/.sdkman/candidates/java/current/bin", true},
		{"/opt/sdkman/candidates/java/bin", true},
		// Bare shims dirs: detected via the parent check. The parent check
		// matches ONLY the dotted manager roots (.jenv/.asdf/.sdkman) and
		// the sdkman/candidates tree — NOT a bare "sdkman" substring, so a
		// nonstandard /opt/sdkman prefix on a shims dir is NOT stripped
		// (its java would fail to exec under the sandbox — the honest
		// failure surfacing an unusual layout, rather than a silent PATH
		// rewrite).
		{"/home/u/.jenv/shims-bin", true},
		{"/opt/sdkman/shims-bin", false},
		{"/home/u/.sdkman-anywhere/shims", false},
		// Not shim dirs.
		{"/usr/shims", false},
		{"/usr/local/bin", false},
		{"/opt/shims/tools", false}, // basename match applies to the entry itself, not an ancestor
	} {
		if got := isShimDir(tc.dir); got != tc.want {
			t.Errorf("isShimDir(%q) = %v, want %v", tc.dir, got, tc.want)
		}
	}
}
