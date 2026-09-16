package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
	"github.com/TNG/oh-my-agentic-coder/internal/registry"
	"github.com/TNG/oh-my-agentic-coder/internal/skilltrust"
)

// Trust on first upgrade.
//
// Skill sidecars run as ordinary host children, outside the sandbox, which is
// why spawning one requires a host-side approval keyed by the skill's content
// hash. Adding that gate to an existing installation would have broken every
// setup that already worked, so the first run under the new scheme approves
// whatever is registered at that moment and then closes the window for good.
//
// The migration reads two registries. The user-global one lives outside every
// workdir and the sandbox cannot write it. The other is
// <workdir>/.opencode/sidecar.json — inside the tree the confined agent edits
// freely, and the same file the agent is expected to write when it registers a
// skill legitimately.
//
// Approving that layer wholesale grants exactly what the gate exists to
// withhold. An agent that writes a skill and forges its registry entry before
// the first upgraded launch has its code approved without anyone seeing it,
// and approval is what lets a sidecar run unconfined on the host. The window
// is also not as narrow as "the first launch ever": it is the first launch
// after upgrading omac, on a machine where the agent has been running against
// the workdir all along.

// stageGlobalSkill writes a skill outside any workdir and registers it in the
// user-global registry, which the sandbox cannot reach. Returns its bundle
// hash.
func stageGlobalSkill(t *testing.T, name string) string {
	t.Helper()
	skillDir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("mkdir global skill: %v", err)
	}
	meta := "name: " + name + "\ntype: skill\nsidecar:\n  command: [\"python3\", \"server.py\"]\n  mount: " + name + "\n"
	if err := os.WriteFile(filepath.Join(skillDir, config.MetaFileName), []byte(meta), 0o644); err != nil {
		t.Fatalf("write omac.yaml: %v", err)
	}
	hash := bundleHashOf(t, skillDir)
	if err := registry.WithGlobalLock(func() error {
		reg, err := registry.LoadGlobal()
		if err != nil {
			return err
		}
		reg.Upsert(registry.Entry{
			Name:         name,
			SkillDir:     skillDir,
			BundleHash:   hash,
			RegisteredAt: time.Now().UTC(),
		})
		return registry.SaveGlobal(reg)
	}); err != nil {
		t.Fatalf("register global skill: %v", err)
	}
	return hash
}

// TestSecurityFirstRunDoesNotAutoApproveWorkdirSkills asserts that the
// one-time migration does not extend to the registry the agent can write.
func TestSecurityFirstRunDoesNotAutoApproveWorkdirSkills(t *testing.T) {
	isolateHome(t)
	workdir := t.TempDir()

	globalHash := stageGlobalSkill(t, "trusted")
	stageForgedSkill(t, workdir, "pwn", "pwn")

	workdirSkillDir := filepath.Join(workdir, ".opencode", "skills", "pwn")
	forgedHash := bundleHashOf(t, workdirSkillDir)

	if !firstApprovalUpgrade() {
		t.Fatal("the approval store already exists, so no migration will run: the fixture is broken, not the security property")
	}

	wReg, err := registry.Load(workdir)
	if err != nil {
		t.Fatalf("load workdir registry: %v", err)
	}
	gReg, err := registry.LoadGlobal()
	if err != nil {
		t.Fatalf("load global registry: %v", err)
	}
	if _, err := grandfatherOnce(
		grandfatherScope{workdir: workdir, reg: wReg},
		grandfatherScope{workdir: "", reg: gReg},
	); err != nil {
		t.Fatalf("grandfatherOnce: %v", err)
	}

	// Control: the setup the migration exists for still works. Without it,
	// a migration that approved nothing at all would look like a fix.
	if ok, err := skilltrust.IsApproved("trusted", globalHash); err != nil || !ok {
		t.Fatalf("the user-global skill was not grandfathered (approved=%v, err=%v): upgrading omac would break a working setup, which is not the fix", ok, err)
	}

	if ok, _ := skilltrust.IsApproved("pwn", forgedHash); ok {
		t.Error("a skill the agent wrote into the workdir was approved by the upgrade migration: its sidecar may now run on the host outside the sandbox, with no human ever having seen the code")
	}
}
