package e2e

import (
	"regexp"
	"testing"
)

// safePackageSpec matches "<name>@<version>", the only shape a pinned
// install spec should ever take: a package name followed by an explicit
// version, nothing else. A bare git/tarball URL, a foreign package name, or
// a shell-metacharacter-bearing string all fail this.
var safePackageSpec = regexp.MustCompile(`^[a-zA-Z0-9_.\/@-]+@[a-zA-Z0-9_.+-]+$`)

// TestSecurityPinnedPackageRejectsCallerChosenSpec asserts that
// pinnedPackage never hands a workflow_dispatch-controlled string straight
// to `npm install -g`/`bun install -g` without it matching the pinned
// package's own name and an explicit version.
//
// pinnedPackage returns os.Getenv(ev) verbatim when E2E_VERSION_<HARNESS> is
// set, and every harnesses.go install site passes that string directly to
// a package manager's install command. A CI job accepting a
// workflow_dispatch string input can set this env var to an arbitrary
// value — a different package entirely, or a git/tarball URL — installed
// with the same secrets-bearing job's credentials.
func TestSecurityPinnedPackageRejectsCallerChosenSpec(t *testing.T) {
	t.Setenv("E2E_USE_LATEST", "")

	// Control: the pinned default (no override set) already matches the
	// safe shape, so the regex itself isn't the reason a caller-supplied
	// value would fail.
	t.Setenv("E2E_VERSION_OPENCODE", "")
	pinned := pinnedPackage("opencode")
	if !safePackageSpec.MatchString(pinned) {
		t.Fatalf("control: the pinned default %q does not match the safe package-spec shape: the fixture is broken, not the security property", pinned)
	}

	hostile := "git+https://attacker.example/malicious-package.git#main"
	t.Setenv("E2E_VERSION_OPENCODE", hostile)
	got := pinnedPackage("opencode")

	if got == hostile {
		t.Errorf("pinnedPackage(\"opencode\") returned the caller-supplied override %q verbatim: a workflow_dispatch input reaches `npm/bun install -g` with no validation that it names the pinned package at all", got)
	}
}
