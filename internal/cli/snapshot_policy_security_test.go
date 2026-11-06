package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
	"github.com/TNG/oh-my-agentic-coder/internal/registry"
	"github.com/TNG/oh-my-agentic-coder/internal/skilltrust"
	"github.com/TNG/oh-my-agentic-coder/internal/supervisor"
)

// A legacy approval still matches even when its snapshot contains excluded
// bytes. Rehashing cannot make it safe; both spawn gates must reject it until
// host registration builds a snapshot under the current copy policy.
func TestSecurityLegacySnapshotRequiresHostReRegistration(t *testing.T) {
	isolateHome(t)
	const name = "legacy-policy"
	workdir := stageWorkdir(t, name)
	src := filepath.Join(workdir, ".opencode", "skills", name)
	register := func() {
		t.Helper()
		env := makeEnv(workdir)
		defer env.Stdout.Close()
		if code := runRegister([]string{name, "--no-secrets", "--no-fields"}, env); code != ExitOK {
			t.Fatalf("host register exit %d", code)
		}
	}
	manifest := "name: legacy-policy\ntype: skill\nsidecar:\n  command: [\"python3\", \"sidecar.py\"]\n  mount: legacy-policy\n"
	for rel, body := range map[string]string{
		config.MetaFileName: manifest,
		"sidecar.py":        "print('reviewed')\n",
	} {
		if err := os.WriteFile(filepath.Join(src, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	h := bundleHashOf(t, src)
	register()
	snap, ok := skilltrust.SnapshotPath(name, h)
	if !ok {
		t.Fatal("initial snapshot missing")
	}
	// Construct the old on-disk layout without depending on today's copier.
	stash := filepath.Join(t.TempDir(), "snapshot")
	if err := os.Rename(snap, stash); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(registry.GlobalDir(), "skills", name, strings.TrimPrefix(h, "sha256:"))
	if err := os.MkdirAll(filepath.Dir(legacy), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(stash, legacy); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "module.pyc"), []byte("unapproved bytecode"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := bundleHashOf(t, legacy); got != h {
		t.Fatalf("legacy hash control = %s, want %s", got, h)
	}
	storePath := filepath.Join(registry.GlobalDir(), "approvals.json")
	before, err := os.ReadFile(storePath)
	if err != nil {
		t.Fatal(err)
	}
	if approved, err := skilltrust.IsApproved(name, h); err != nil || !approved {
		t.Fatalf("legacy approval control = %v, %v", approved, err)
	}
	if firstApprovalUpgrade() {
		t.Fatal("snapshot-policy upgrade reopened automatic approval migration")
	}
	if _, present := skilltrust.SnapshotPath(name, h); present {
		t.Error("lookup returned a legacy-policy snapshot")
	}
	if got, err := approvedSpawnDir(name, src, h); err == nil || got != "" {
		t.Errorf("preflight accepted legacy snapshot: %q, %v", got, err)
	} else {
		msg := err.Error()
		if !strings.Contains(msg, "omac register "+name) {
			t.Errorf("refusal lacks host-registration remedy: %v", err)
		}
		// The skill is approved; only its snapshot predates the current
		// policy. Saying "not host-approved" sends the user to the wrong
		// mental model and hides that rebuilding is all that is needed.
		if !strings.Contains(msg, "older copy policy") {
			t.Errorf("refusal must blame the old snapshot policy, got: %v", err)
		}
		if strings.Contains(msg, "not host-approved") {
			t.Errorf("approved skill must not be reported as unapproved: %v", err)
		}
	}
	fromLegacy := supervisor.SidecarSpec{SkillName: name, SkillDir: legacy}
	if err := skillSpawnAuthorizer(fromLegacy); err == nil {
		t.Error("spawn backstop accepted legacy snapshot")
	}
	if err := skillSpawnAuthorizer(supervisor.SidecarSpec{SkillName: name, SkillDir: src}); err == nil {
		t.Error("spawn backstop fell back to the workdir")
	}
	if after, err := os.ReadFile(storePath); err != nil || !bytes.Equal(before, after) {
		t.Fatalf("lookup/spawn checks modified approvals: %v", err)
	}
	// A stale requested hash cannot authorize different source while rebuilding.
	if err := os.WriteFile(filepath.Join(src, "sidecar.py"), []byte("print('changed')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := skilltrust.Approve(name, h, src); err == nil {
		t.Error("same-hash registration silently accepted mutated source instead of validating the staged copy")
	}
	if got, err := approvedSpawnDir(name, src, ""); err == nil || got != "" {
		t.Errorf("mutated source spawned: %q, %v", got, err)
	}
	if _, present := skilltrust.SnapshotPath(name, h); present {
		t.Error("failed rebuild left a usable snapshot")
	}
	if err := os.WriteFile(filepath.Join(src, "sidecar.py"), []byte("print('reviewed')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Excluded files may still be present at host registration; they must not
	// enter the replacement snapshot or change the approval's content key.
	if err := os.WriteFile(filepath.Join(src, "module.pyc"), []byte("unapproved bytecode"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := bundleHashOf(t, src); got != h {
		t.Fatalf("excluded-file hash control = %s, want %s", got, h)
	}
	register()
	rebuilt, err := approvedSpawnDir(name, src, h)
	if err != nil {
		t.Fatalf("rebuilt snapshot refused: %v", err)
	}
	if rebuilt == legacy {
		t.Error("host registration reused legacy-policy directory")
	}
	if _, err := os.Lstat(filepath.Join(rebuilt, "module.pyc")); !os.IsNotExist(err) {
		t.Errorf("rebuilt snapshot retained excluded bytes: %v", err)
	}
	if got := bundleHashOf(t, rebuilt); got != h {
		t.Errorf("rebuilt snapshot changed approved hash: %s", got)
	}
	if err := skillSpawnAuthorizer(supervisor.SidecarSpec{SkillName: name, SkillDir: rebuilt}); err != nil {
		t.Errorf("backstop refused rebuilt snapshot: %v", err)
	}
	if err := skillSpawnAuthorizer(fromLegacy); err == nil {
		t.Error("host registration re-enabled the legacy snapshot")
	}
	meta, err := snapshotMeta(name, rebuilt)
	if err != nil || meta == nil || meta.Sidecar == nil || len(meta.Sidecar.Command) != 2 || meta.Sidecar.Command[1] != "sidecar.py" {
		t.Fatalf("snapshot manifest unavailable: %v, %v", meta, err)
	}
	if after, err := os.ReadFile(storePath); err != nil || !bytes.Equal(before, after) {
		t.Errorf("same-hash rebuild changed existing approval: %v", err)
	}
	if err := skilltrust.Approve(name, h, src); err != nil {
		t.Fatal(err)
	}
	if again, ok := skilltrust.SnapshotPath(name, h); !ok || again != rebuilt {
		t.Errorf("current-policy reuse not idempotent: %q, %v", again, ok)
	}
	if _, err := skilltrust.Revoke(name, h); err != nil {
		t.Fatal(err)
	}
	if _, ok := skilltrust.SnapshotPath(name, h); ok || skilltrust.IsSnapshotPath(rebuilt) {
		t.Error("revocation left the current snapshot executable")
	}
}
