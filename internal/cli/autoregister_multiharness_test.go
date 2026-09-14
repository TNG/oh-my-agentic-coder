package cli

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/registry"
	"github.com/TNG/oh-my-agentic-coder/internal/skillsource"
)

func seedHarnessKeyedEntry(t *testing.T, workdir, name, skillDir, bundle string) {
	t.Helper()
	if err := registry.WithLock(workdir, func() error {
		reg, err := registry.Load(workdir)
		if err != nil {
			return err
		}
		reg.Upsert(registry.Entry{
			Name:         name,
			Harness:      "opencode",
			SkillDir:     skillDir,
			BundleHash:   bundle,
			RegisteredAt: time.Now().UTC(),
		})
		return registry.Save(workdir, reg)
	}); err != nil {
		t.Fatalf("seed registry: %v", err)
	}
}

func assertUnclobbered(t *testing.T, workdir, bundle string) {
	t.Helper()
	reg, err := registry.Load(workdir)
	if err != nil {
		t.Fatalf("registry.Load: %v", err)
	}
	oc, _ := reg.FindForHarness("slack", "opencode")
	if oc == nil {
		t.Fatal("opencode-keyed entry was clobbered by autoRegister")
	}
	if oc.SkillDir != ".opencode/skills/slack" {
		t.Errorf("opencode entry SkillDir = %q, want .opencode/skills/slack", oc.SkillDir)
	}
	if oc.BundleHash != bundle {
		t.Errorf("opencode entry BundleHash = %q, want %q", oc.BundleHash, bundle)
	}
}

func stageMultiHarnessSlack(t *testing.T, workdir string) (sharedDir, ocBundle string) {
	t.Helper()
	stageHarnessSkillBody(t, workdir, "opencode", "slack", "# opencode copy\n")
	stageHarnessSkillBody(t, workdir, "agents", "slack", "# shared copy, different bytes\n")
	return filepath.Join(workdir, ".agents", "skills", "slack"),
		bundleHashOf(t, filepath.Join(workdir, ".opencode", "skills", "slack"))
}

// Bypasses the name-only guard on purpose: the guard reads the registry
// outside the lock, so a concurrent register can put a harness-keyed
// entry of the same name in place before autoRegister's Upsert runs.
func TestServeAutoRegisterDoesNotClobberHarnessKeyedEntry(t *testing.T) {
	isolateHome(t)
	wd := t.TempDir()
	sharedDir, ocBundle := stageMultiHarnessSlack(t, wd)
	seedHarnessKeyedEntry(t, wd, "slack", ".opencode/skills/slack", ocBundle)

	s := &serveServer{harness: claudeHarness(t)}
	ne, err := s.autoRegister(wd, skillsource.Entry{Name: "slack", Dir: sharedDir, Kind: "workdir"})
	if err != nil {
		t.Fatalf("autoRegister: %v", err)
	}
	if ne == nil || ne.Harness != "claude-code" {
		t.Errorf("autoRegister returned entry %v, want a claude-code-keyed entry", ne)
	}
	assertUnclobbered(t, wd, ocBundle)
}

func TestStartAutoRegisterOneDoesNotClobberHarnessKeyedEntry(t *testing.T) {
	isolateHome(t)
	wd := t.TempDir()
	sharedDir, ocBundle := stageMultiHarnessSlack(t, wd)
	seedHarnessKeyedEntry(t, wd, "slack", ".opencode/skills/slack", ocBundle)

	ne, err := startAutoRegisterOne(wd, claudeHarness(t), skillsource.Entry{Name: "slack", Dir: sharedDir, Kind: "workdir"})
	if err != nil {
		t.Fatalf("startAutoRegisterOne: %v", err)
	}
	if ne == nil || ne.Harness != "claude-code" {
		t.Errorf("startAutoRegisterOne returned entry %v, want a claude-code-keyed entry", ne)
	}
	assertUnclobbered(t, wd, ocBundle)
}

// Pins the name-only guard: an entry under ANY harness key counts as
// registered. Switching the guard to FindForHarness must stay
// consistent with the harness-keyed Upserts.
func TestStartAutoRegisterGuardSkipsCrossHarnessNames(t *testing.T) {
	isolateHome(t)
	wd := t.TempDir()
	_, _ = stageMultiHarnessSlack(t, wd)

	reg := &registry.Registry{Registered: []registry.Entry{{
		Name:       "slack",
		Harness:    "opencode",
		SkillDir:   ".opencode/skills/slack",
		BundleHash: "stale",
	}}}
	done, errs := runAutoRegisterWorkdirSkills(t, makeEnv(wd), claudeHarness(t), reg, false)
	if len(errs) > 0 {
		t.Fatalf("startAutoRegisterWorkdirSkills diagnostics: %v", errs)
	}
	for _, name := range done {
		if name == "slack" {
			t.Error("slack was re-registered although an opencode-keyed entry exists")
		}
	}
	if cc, _ := reg.FindForHarness("slack", "claude-code"); cc != nil {
		t.Error("guard registered a claude-code entry for an already-registered name")
	}
}
