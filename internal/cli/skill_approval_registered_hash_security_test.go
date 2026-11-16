package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
	"github.com/TNG/oh-my-agentic-coder/internal/registry"
	"github.com/TNG/oh-my-agentic-coder/internal/skilltrust"
)

// Trust-on-first-upgrade must anchor on the registry record.
//
// The one-time migration transfers already-registered user-global skills into
// the host-only approval store. The registry entry is the only record of the
// content a human reviewed at `omac register` time, so it — and only it — is
// what the migration may approve: a skill whose current bytes still match its
// recorded hash. Content that changed after registration, and entries recorded
// without a hash, must be left to the ordinary spawn gate, which refuses them
// and names the host-side remedy.

// writeGlobalSkillForRegister writes a minimal skill outside any workdir and
// returns its directory.
func writeGlobalSkillForRegister(t *testing.T, name, code string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir global skill: %v", err)
	}
	meta := "name: " + name + "\ntype: skill\nsidecar:\n  command: [\"python3\", \"server.py\"]\n  mount: " + name + "\n"
	if err := os.WriteFile(filepath.Join(dir, config.MetaFileName), []byte(meta), 0o644); err != nil {
		t.Fatalf("write %s: %v", config.MetaFileName, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "server.py"), []byte(code), 0o644); err != nil {
		t.Fatalf("write server.py: %v", err)
	}
	return dir
}

func registeredBundleHash(t *testing.T, dir string) string {
	t.Helper()
	h, err := config.BundleHash(dir)
	if err != nil {
		t.Fatalf("bundle hash %s: %v", dir, err)
	}
	return h
}

// TestSecurityGrandfatherAnchorsRegisteredHash pins the migration's security
// property: it approves a registered user-global skill only under the hash
// recorded at registration time, and only when the bytes on disk still match
// that record.
func TestSecurityGrandfatherAnchorsRegisteredHash(t *testing.T) {
	isolateHome(t)

	if !firstApprovalUpgrade() {
		t.Fatal("precondition: the approval store must not exist yet for the migration to run")
	}

	// steady: on-disk content still matches its recorded hash.
	steadyDir := writeGlobalSkillForRegister(t, "steady", "print(\"steady v1\")\n")
	steadyHash := registeredBundleHash(t, steadyDir)

	// drifted: registered with one hash, its bytes change afterwards.
	driftedDir := writeGlobalSkillForRegister(t, "drifted", "print(\"drifted v1\")\n")
	driftedHash := registeredBundleHash(t, driftedDir)

	// legacy: recorded without a hash.
	legacyDir := writeGlobalSkillForRegister(t, "legacy", "print(\"legacy v1\")\n")
	legacyHash := registeredBundleHash(t, legacyDir)

	// Register all three in the user-global registry, as `omac register` did.
	if err := registry.WithGlobalLock(func() error {
		reg, err := registry.LoadGlobal()
		if err != nil {
			return err
		}
		reg.Upsert(registry.Entry{Name: "steady", SkillDir: steadyDir, BundleHash: steadyHash, RegisteredAt: time.Now().UTC()})
		reg.Upsert(registry.Entry{Name: "drifted", SkillDir: driftedDir, BundleHash: driftedHash, RegisteredAt: time.Now().UTC()})
		reg.Upsert(registry.Entry{Name: "legacy", SkillDir: legacyDir, RegisteredAt: time.Now().UTC()})
		return registry.SaveGlobal(reg)
	}); err != nil {
		t.Fatalf("register global skills: %v", err)
	}

	// The drifted skill's content changes after registration.
	if err := os.WriteFile(filepath.Join(driftedDir, "server.py"), []byte("print(\"drifted v2\")\n"), 0o644); err != nil {
		t.Fatalf("rewrite server.py: %v", err)
	}
	driftedOnDisk := registeredBundleHash(t, driftedDir)
	if driftedOnDisk == driftedHash {
		t.Fatal("precondition: rewriting the code must change the bundle hash")
	}

	// First launch of an upgraded build runs the migration.
	gReg, err := registry.LoadGlobal()
	if err != nil {
		t.Fatalf("load global registry: %v", err)
	}
	if _, err := grandfatherOnce(grandfatherScope{reg: gReg}); err != nil {
		t.Fatalf("grandfather: %v", err)
	}

	// The unchanged skill is approved under the hash it was registered with;
	// without this control, a migration that approved nothing would look like a fix.
	if ok, err := skilltrust.IsApproved("steady", steadyHash); err != nil || !ok {
		t.Errorf("skill whose content matches its recorded hash must be migrated: approved=%v err=%v", ok, err)
	}
	if refusal := approvalRefusal("steady", steadyDir, ""); refusal != nil {
		t.Errorf("unchanged registered skill must remain spawnable after the migration: %v", refusal)
	}

	// The skill whose content changed after registration is approved under
	// neither hash: not the recorded one, not the one now on disk.
	if ok, _ := skilltrust.IsApproved("drifted", driftedHash); ok {
		t.Error("recorded hash of a changed skill must not be approved")
	}
	if ok, _ := skilltrust.IsApproved("drifted", driftedOnDisk); ok {
		t.Error("changed skill must not be approved under its current on-disk hash")
	}

	// The spawn gate refuses it and names the host-side remedy.
	if _, refusal := approvedSpawnDir("drifted", driftedDir, ""); refusal == nil {
		t.Error("skill whose content changed since registration must be refused by the spawn gate")
	} else if !strings.Contains(refusal.Error(), "omac register") {
		t.Errorf("refusal must name the re-register remedy, got: %v", refusal)
	}

	// An entry recorded without a hash is skipped, not approved.
	if ok, _ := skilltrust.IsApproved("legacy", legacyHash); ok {
		t.Error("skill registered without a recorded hash must not be approved by the migration")
	}
}
