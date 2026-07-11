package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/tngtech/oh-my-agentic-coder/internal/registry"
)

// stageHarnessSkillBody drops an omac.yaml with a caller-supplied body
// under <workdir>/.<base>/skills/<name>/. Distinct bodies produce
// distinct bundle hashes, which is what the multi-harness guard test
// below relies on to reproduce the original bug.
func stageHarnessSkillBody(t *testing.T, workdir, base, name, extra string) {
	t.Helper()
	dir := filepath.Join(workdir, "."+base, "skills", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "name: " + name + "\nsidecar:\n  command: [\"true\"]\n  mount: " + name + "\n" + extra
	if err := os.WriteFile(filepath.Join(dir, "omac.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestRegister_SameSkillTwoHarnesses pins the fix for issue #57: the
// same skill name, installed as two separate on-disk copies (one per
// harness) with DIFFERENT bundle hashes, must register under a second
// harness WITHOUT --force. The registry keys entries by (Name, Harness),
// so the two registrations are not in conflict — but a name-only
// pre-check would compare the incoming claude copy against the existing
// opencode entry and reject it with a false "different bundle_hash"
// error.
func TestRegister_SameSkillTwoHarnesses(t *testing.T) {
	isolateHome(t)
	wd := t.TempDir()
	// Same name under both harness dirs, with different bodies so the
	// bundle hashes genuinely differ (the crux of the repro).
	stageHarnessSkillBody(t, wd, "opencode", "slack", "# opencode copy\n")
	stageHarnessSkillBody(t, wd, "claude", "slack", "# claude copy, different bytes\n")
	env := makeEnv(wd)

	// First: register under opencode.
	if code := runRegister([]string{"slack", "--harness", "opencode"}, env); code != ExitOK {
		t.Fatalf("register opencode: code = %d, want ExitOK", code)
	}

	// Second: register the (different-hash) claude copy WITHOUT --force.
	// Before the fix this returned ExitIOError with "already registered
	// with a different bundle_hash".
	if code := runRegister([]string{"slack", "--harness", "claude"}, env); code != ExitOK {
		t.Fatalf("register claude without --force: code = %d, want ExitOK", code)
	}

	// Both entries must coexist, each scoped to its own harness.
	reg, err := registry.Load(wd)
	if err != nil {
		t.Fatalf("registry.Load: %v", err)
	}
	oc, _ := reg.FindForHarness("slack", "opencode")
	cc, _ := reg.FindForHarness("slack", "claude-code")
	if oc == nil {
		t.Error("opencode entry missing after both registrations")
	}
	if cc == nil {
		t.Error("claude-code entry missing after both registrations")
	}
	if oc != nil && cc != nil && oc.BundleHash == cc.BundleHash {
		t.Errorf("expected distinct bundle hashes, both are %q", oc.BundleHash)
	}
}

// TestRegister_SameHarnessChangedBundleStillGuarded ensures the fix
// narrowed the guard rather than removing it: re-registering under the
// SAME harness with a changed bundle hash must still be refused without
// --force, and accepted with it.
func TestRegister_SameHarnessChangedBundleStillGuarded(t *testing.T) {
	isolateHome(t)
	wd := t.TempDir()
	stageHarnessSkillBody(t, wd, "opencode", "slack", "# v1\n")
	env := makeEnv(wd)

	if code := runRegister([]string{"slack", "--harness", "opencode"}, env); code != ExitOK {
		t.Fatalf("initial register: code = %d, want ExitOK", code)
	}

	// Mutate the skill so its bundle hash changes, then re-register the
	// same (name, harness) without --force: must be refused.
	stageHarnessSkillBody(t, wd, "opencode", "slack", "# v2 changed\n")
	if code := runRegister([]string{"slack", "--harness", "opencode"}, env); code != ExitIOError {
		t.Fatalf("re-register changed bundle without --force: code = %d, want ExitIOError", code)
	}

	// With --force it goes through.
	if code := runRegister([]string{"slack", "--harness", "opencode", "--force"}, env); code != ExitOK {
		t.Fatalf("re-register changed bundle with --force: code = %d, want ExitOK", code)
	}
}
