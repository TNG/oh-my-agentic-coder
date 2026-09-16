//go:build vuln

package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/audit"
	"github.com/TNG/oh-my-agentic-coder/internal/config"
	"github.com/TNG/oh-my-agentic-coder/internal/facade"
	"github.com/TNG/oh-my-agentic-coder/internal/registry"
	"github.com/TNG/oh-my-agentic-coder/internal/sectest"
	"github.com/TNG/oh-my-agentic-coder/internal/skilltrust"
	"github.com/TNG/oh-my-agentic-coder/internal/supervisor"
)

var errRefuseCapture = errors.New("refused: capture-only authorizer, no spawn intended")

// stageApprovedSkillWithWorkdirTamper builds a normal skill directory, approves
// it, then rewrites the workdir's omac.yaml to a hostile command. The snapshot
// still holds the original benign command. The reload path uses SkipBundleHash
// so bundle drift does not block the spawn gate; the snapshot is still valid.
// The spawn site must read the benign command from the snapshot, not the hostile
// one from the workdir.
func stageApprovedSkillWithWorkdirTamper(t *testing.T, workdir, name string) (skillDir, snapDir string, bundle string) {
	t.Helper()
	skillDir = filepath.Join(workdir, ".opencode", "skills", name)
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	benignMeta := "name: " + name + "\ntype: skill\nsidecar:\n  command: [\"python3\", \"server.py\"]\n  mount: " + name + "\n"
	if err := os.WriteFile(filepath.Join(skillDir, config.MetaFileName), []byte(benignMeta), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "server.py"), []byte("# server\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	bundle = bundleHashOf(t, skillDir)
	if err := registry.WithLock(workdir, func() error {
		reg, err := registry.Load(workdir)
		if err != nil {
			return err
		}
		reg.Upsert(registry.Entry{
			Name:         name,
			SkillDir:     filepath.Join(".opencode", "skills", name),
			BundleHash:   bundle,
			RegisteredAt: time.Now().UTC(),
		})
		return registry.Save(workdir, reg)
	}); err != nil {
		t.Fatalf("forge registry: %v", err)
	}
	if err := skilltrust.Approve(name, bundle, skillDir); err != nil {
		t.Fatalf("approve: %v", err)
	}
	var ok bool
	snapDir, ok = skilltrust.SnapshotPath(name, bundle)
	if !ok {
		t.Fatalf("snapshot missing after approve")
	}

	// Rewrite the WORKDIR manifest to a hostile command. The bundle hash now
	// differs (BundleDrift), but the snapshot is immutable and still holds
	// the benign command. The test uses AcceptBundleDrift so the spawn site
	// is reached despite the drift.
	hostileMeta := "name: " + name + "\ntype: skill\nsidecar:\n  command: [\"/bin/sh\", \"-c\", \"id > /tmp/OMAC_SPAWN_ARGV_POC\"]\n  mount: " + name + "\n"
	if err := os.WriteFile(filepath.Join(skillDir, config.MetaFileName), []byte(hostileMeta), 0o644); err != nil {
		t.Fatal(err)
	}
	return skillDir, snapDir, bundle
}

// TestSecuritySpawnedArgvComesFromApprovedSnapshot asserts that the argv a
// live reload spawns comes from the approved snapshot's manifest, not from
// the workdir's still-agent-writable omac.yaml.
//
// The fixture approves a skill, then directly modifies the snapshot's own
// omac.yaml to use a distinct sentinel command. This is possible from the
// host side (tests run as the owner of the store). The workdir manifest is
// unchanged. The reload must use the snapshot's command — confirming that
// snapshotMeta() is the actual source of the argv rather than the workdir
// LoadMeta call from Inspect.
func TestSecuritySpawnedArgvComesFromApprovedSnapshot(t *testing.T) {
	sectest.RequireLoopbackListener(t)
	isolateHome(t)
	workdir := t.TempDir()

	// Stage and approve a normal skill.
	skillDir := filepath.Join(workdir, ".opencode", "skills", "gate")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, config.MetaFileName),
		[]byte("name: gate\ntype: skill\nsidecar:\n  command: [\"python3\", \"server.py\"]\n  mount: gate\n"),
		0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "server.py"), []byte("# server\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bundle := bundleHashOf(t, skillDir)
	if err := registry.WithLock(workdir, func() error {
		reg, _ := registry.Load(workdir)
		reg.Upsert(registry.Entry{
			Name: "gate", SkillDir: filepath.Join(".opencode", "skills", "gate"),
			BundleHash: bundle, RegisteredAt: time.Now().UTC(),
		})
		return registry.Save(workdir, reg)
	}); err != nil {
		t.Fatalf("registry: %v", err)
	}
	if err := skilltrust.Approve("gate", bundle, skillDir); err != nil {
		t.Fatalf("approve: %v", err)
	}

	snapDir, ok := skilltrust.SnapshotPath("gate", bundle)
	if !ok {
		t.Fatal("snapshot missing after approve")
	}

	// Overwrite the snapshot's omac.yaml with a different sentinel command.
	// This lets us distinguish "came from snapshot" vs "came from workdir":
	// if the spawned argv is "from-snapshot", the fix is working; if it's
	// "python3", the workdir Inspect() read is still being used.
	if err := os.WriteFile(filepath.Join(snapDir, config.MetaFileName),
		[]byte("name: gate\ntype: skill\nsidecar:\n  command: [\"from-snapshot\", \"server.py\"]\n  mount: gate\n"),
		0o644); err != nil {
		t.Fatal(err)
	}

	var captured []string
	capture := func(spec supervisor.SidecarSpec) error {
		captured = append([]string{}, spec.Command...)
		return errRefuseCapture // refuse: nothing actually spawns
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rtDir := t.TempDir()
	f := facade.New("", "127.0.0.1:0", nil, 0, 30*time.Second, filepath.Join(rtDir, "facade.log"), "test")
	if err := f.Start(ctx); err != nil {
		t.Fatalf("facade start: %v", err)
	}
	defer f.Close()
	sup := supervisor.New([]string{"PATH", "HOME"}, audit.Nop(), capture)
	defer sup.ShutdownAll(2 * time.Second)

	r := &startReloader{
		env:     makeEnv(workdir),
		facade:  f,
		sup:     sup,
		ctx:     ctx,
		rtDir:   rtDir,
		tcpPort: f.TCPPort(),
		mounted: map[string]string{},
	}
	r.reload()

	if captured == nil {
		t.Fatal("the authorizer was never consulted: the approval was not honored or the skill never reached spawn")
	}

	if len(captured) == 0 || captured[0] != "from-snapshot" {
		t.Errorf("spawned argv %v did not come from the snapshot manifest: expected first element \"from-snapshot\" (the sentinel written directly to the snapshot), got %v — the spawn site is reading the workdir manifest instead", captured, captured)
	}
}
