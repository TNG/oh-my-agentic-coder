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
