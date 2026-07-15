//go:build e2e

package e2e

import "testing"

// TestPinnedPackageOverride covers the precedence between the hardcoded
// harnessVersions map, a per-harness E2E_VERSION_* override (wired from the
// e2e workflow's workflow_dispatch *_version inputs), and E2E_USE_LATEST.
// No live agent involved — fast unit test.
func TestPinnedPackageOverride(t *testing.T) {
	t.Setenv("E2E_USE_LATEST", "")
	t.Setenv("E2E_VERSION_OPENCODE", "")

	if got, want := pinnedPackage("opencode"), harnessVersions["opencode"]; got != want {
		t.Errorf("with no override set: pinnedPackage(opencode) = %q, want %q (pinned map)", got, want)
	}

	t.Setenv("E2E_VERSION_OPENCODE", "opencode-ai@9.9.9")
	if got, want := pinnedPackage("opencode"), "opencode-ai@9.9.9"; got != want {
		t.Errorf("with override set: pinnedPackage(opencode) = %q, want %q", got, want)
	}

	t.Setenv("E2E_USE_LATEST", "1")
	if got, want := pinnedPackage("opencode"), "opencode-ai"; got != want {
		t.Errorf("use_latest should win over the override and strip the version: pinnedPackage(opencode) = %q, want %q", got, want)
	}
}

// TestVersionEnvVarCompleteness guards against a harness being registered
// (allHarnesses(), harnessVersions) without also wiring its workflow_dispatch
// override — the class of bug fixed for pi, which shipped with a pinned
// version but no E2E_VERSION_PI entry, so its version couldn't be overridden
// for a manual run the way every other harness's could.
func TestVersionEnvVarCompleteness(t *testing.T) {
	for _, h := range allHarnesses() {
		if _, ok := harnessVersions[h.Name]; !ok {
			t.Errorf("harness %q has no entry in harnessVersions", h.Name)
		}
		if _, ok := versionEnvVar[h.Name]; !ok {
			t.Errorf("harness %q has no entry in versionEnvVar (add its *_version workflow_dispatch input in .github/workflows/e2e.yml too)", h.Name)
		}
	}
}
