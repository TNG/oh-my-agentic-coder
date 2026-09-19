package cli

// Security regression tests for serve-mode skill route wiring.
//
// A READY serve-mode route must serve SKILL.md from the frozen host-only
// approval snapshot, not from the agent-writable workdir skill directory.
// Start mode already does this (start.go); serve mode must not diverge.

import (
	"testing"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/audit"
	"github.com/TNG/oh-my-agentic-coder/internal/facade"
	"github.com/TNG/oh-my-agentic-coder/internal/skilltrust"
	"github.com/TNG/oh-my-agentic-coder/internal/supervisor"
)

func TestSecurityServeModeSkillDirIsSnapshot(t *testing.T) {
	requireWorkingPython3(t)

	s := newServeServerForTest(t) // isolates HOME/XDG
	workdir := t.TempDir()

	// Stage a workdir-local skill with a live sidecar and approve it so
	// activation reaches the READY state.
	secretPath, _ := stageOutsideSecret(t)
	skillDir, bundle := stageAgentAuthoredSkill(t, workdir, "snapcheck", secretPath)
	if err := skilltrust.Approve("snapcheck", bundle, skillDir); err != nil {
		t.Fatalf("approve: %v", err)
	}

	s.sup = supervisor.New(
		[]string{"PATH", "HOME", "LANG", "LC_ALL"},
		audit.Nop(),
		skillSpawnAuthorizer,
	)
	t.Cleanup(func() { s.sup.ShutdownAll(2 * time.Second) })

	manifest, err := s.activate(workdir)
	if err != nil {
		t.Fatalf("activate: %v", err)
	}

	// The skill must have reached READY — otherwise the SkillDir assertion
	// below is meaningless (non-ready routes don't serve SKILL.md).
	skills, _ := manifest["skills"].([]map[string]any)
	var ready bool
	for _, sk := range skills {
		if sk["name"] != "snapcheck" {
			continue
		}
		if sk["state"] != string(facade.RouteReady) {
			t.Fatalf("skill state = %v, want %s", sk["state"], facade.RouteReady)
		}
		ready = true
	}
	if !ready {
		t.Fatal("snapcheck skill not found in manifest")
	}

	// Inspect the installed route's SkillDir directly.
	s.mu.RLock()
	d := s.dirs[workdir]
	s.mu.RUnlock()
	if d == nil {
		t.Fatal("no dirState for activated workdir")
	}
	d.mu.Lock()
	sr := d.Skills["snapcheck"]
	d.mu.Unlock()
	if sr == nil {
		t.Fatal("no skillRoute for snapcheck")
	}

	if !skilltrust.IsSnapshotPath(sr.SkillDir) {
		t.Errorf("READY route SkillDir = %q; must point at the host-only approval snapshot, not the workdir skill dir", sr.SkillDir)
	}
	if sr.SkillDir == skillDir {
		t.Errorf("READY route SkillDir = workdir skill dir %q; must be the frozen snapshot", sr.SkillDir)
	}
}
