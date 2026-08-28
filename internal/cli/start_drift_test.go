package cli

// Tests for the drift-detection helpers in start.go:
//   - autoDeregisterMissing: prunes registry entries whose skill dir
//     no longer exists, leaves the rest alone.
//   - findUnregisteredSkills: finds top-level dirs under
//     .opencode/skills/ that contain a omac.yaml but aren't registered.

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/tngtech/oh-my-agentic-coder/internal/config"
	"github.com/tngtech/oh-my-agentic-coder/internal/registry"
)

// isolateHome points HOME and XDG_CONFIG_HOME at empty temp dirs so
// findUnregisteredSkills's user-global scan doesn't pick up real
// skills installed under the developer's actual ~/.config/opencode.
// All tests that don't deliberately stage user-global content should
// call this; otherwise their assertions become machine-dependent.
func isolateHome(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
}

// stageWorkdir creates a workdir layout suitable for the drift
// helpers: .opencode/ exists, with optional skill directories under
// .opencode/skills/<name>/ each containing a omac.yaml.
func stageWorkdir(t *testing.T, skills ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range skills {
		skillDir := filepath.Join(dir, ".opencode", "skills", name)
		if err := os.MkdirAll(skillDir, 0o755); err != nil {
			t.Fatalf("MkdirAll %s: %v", skillDir, err)
		}
		if err := os.WriteFile(filepath.Join(skillDir, "omac.yaml"), []byte("name: "+name+"\n"), 0o644); err != nil {
			t.Fatalf("write omac.yaml: %v", err)
		}
	}
	return dir
}

// stageUserGlobalSkill creates a user-global skill source under
// $XDG_CONFIG_HOME/opencode/skills/<name>/omac.yaml (falling back to
// $HOME/.config when XDG_CONFIG_HOME is unset). Callers must have
// already pointed HOME / XDG_CONFIG_HOME at a temp dir.
func stageUserGlobalSkill(t *testing.T, name string) string {
	t.Helper()
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		base = filepath.Join(os.Getenv("HOME"), ".config")
	}
	skillDir := filepath.Join(base, "opencode", "skills", name)
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("MkdirAll %s: %v", skillDir, err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "omac.yaml"), []byte("name: "+name+"\n"), 0o644); err != nil {
		t.Fatalf("write omac.yaml: %v", err)
	}
	return skillDir
}

func makeEnv(workdir string) *Env {
	null, _ := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	return &Env{
		Workdir: workdir,
		Stdout:  null,
		Stderr:  null,
		Stdin:   null,
		Version: "test",
	}
}

func TestAutoDeregisterMissing_DeletesGoneSkills(t *testing.T) {
	dir := stageWorkdir(t, "alpha") // alpha exists on disk; bravo doesn't
	reg := &registry.Registry{Registered: []registry.Entry{
		{Name: "alpha", SkillDir: ".opencode/skills/alpha"},
		{Name: "bravo", SkillDir: ".opencode/skills/bravo"}, // never created
	}}
	if err := registry.Save(dir, reg); err != nil {
		t.Fatalf("Save: %v", err)
	}

	pruned, err := autoDeregisterMissing(makeEnv(dir), reg, false)
	if err != nil {
		t.Fatalf("autoDeregisterMissing: %v", err)
	}
	if !reflect.DeepEqual(pruned, []string{"bravo"}) {
		t.Errorf("pruned = %v, want [bravo]", pruned)
	}

	// Caller's view must be updated: only alpha remains.
	if len(reg.Registered) != 1 || reg.Registered[0].Name != "alpha" {
		t.Errorf("in-memory reg after prune = %+v, want only alpha", reg.Registered)
	}
	// Persisted registry must agree.
	persisted, err := registry.Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(persisted.Registered) != 1 || persisted.Registered[0].Name != "alpha" {
		t.Errorf("persisted reg after prune = %+v, want only alpha", persisted.Registered)
	}
}

func TestAutoDeregisterMissing_NoOpWhenAllPresent(t *testing.T) {
	dir := stageWorkdir(t, "alpha", "bravo")
	reg := &registry.Registry{Registered: []registry.Entry{
		{Name: "alpha", SkillDir: ".opencode/skills/alpha"},
		{Name: "bravo", SkillDir: ".opencode/skills/bravo"},
	}}
	pruned, err := autoDeregisterMissing(makeEnv(dir), reg, false)
	if err != nil {
		t.Fatalf("autoDeregisterMissing: %v", err)
	}
	if len(pruned) != 0 {
		t.Errorf("pruned = %v, want []", pruned)
	}
	if len(reg.Registered) != 2 {
		t.Errorf("reg should still have both skills, got %+v", reg.Registered)
	}
}

func TestAutoDeregisterMissing_EmptyRegistry(t *testing.T) {
	dir := stageWorkdir(t)
	reg := &registry.Registry{}
	pruned, err := autoDeregisterMissing(makeEnv(dir), reg, false)
	if err != nil {
		t.Fatalf("autoDeregisterMissing: %v", err)
	}
	if pruned != nil {
		t.Errorf("pruned = %v, want nil", pruned)
	}
}

func TestFindUnregisteredSkills_FindsNew(t *testing.T) {
	isolateHome(t)
	dir := stageWorkdir(t, "alpha", "bravo", "charlie")
	reg := &registry.Registry{Registered: []registry.Entry{
		{Name: "alpha"}, // bravo and charlie are unregistered
	}}
	got, err := findUnregisteredSkills(dir, config.DefaultHarness(), reg)
	if err != nil {
		t.Fatalf("findUnregisteredSkills: %v", err)
	}
	want := []string{"bravo", "charlie"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("findUnregisteredSkills = %v, want %v", got, want)
	}
}

func TestFindUnregisteredSkills_AllRegistered(t *testing.T) {
	isolateHome(t)
	dir := stageWorkdir(t, "alpha", "bravo")
	reg := &registry.Registry{Registered: []registry.Entry{
		{Name: "alpha"},
		{Name: "bravo"},
	}}
	got, err := findUnregisteredSkills(dir, config.DefaultHarness(), reg)
	if err != nil {
		t.Fatalf("findUnregisteredSkills: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("findUnregisteredSkills = %v, want []", got)
	}
}

func TestFindUnregisteredSkills_SkipsDirsWithoutMetaYaml(t *testing.T) {
	isolateHome(t)
	dir := stageWorkdir(t, "alpha")
	// Stage a directory under skills/ but without a omac.yaml. It's
	// an incidental subdirectory (e.g. _template/), not a real skill,
	// so the helper must NOT flag it as unregistered.
	if err := os.MkdirAll(filepath.Join(dir, ".opencode", "skills", "_template"), 0o755); err != nil {
		t.Fatal(err)
	}
	reg := &registry.Registry{Registered: []registry.Entry{{Name: "alpha"}}}
	got, err := findUnregisteredSkills(dir, config.DefaultHarness(), reg)
	if err != nil {
		t.Fatalf("findUnregisteredSkills: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("findUnregisteredSkills = %v, want [] (template dir lacks omac.yaml)", got)
	}
}

func TestFindUnregisteredSkills_NoSkillsDir(t *testing.T) {
	// Workdir without an .opencode/skills/ at all should yield nil
	// (no error). This is the fresh-clone case before any skill has
	// been installed.
	isolateHome(t)
	dir := t.TempDir()
	got, err := findUnregisteredSkills(dir, config.DefaultHarness(), &registry.Registry{})
	if err != nil {
		t.Fatalf("findUnregisteredSkills: %v", err)
	}
	if got != nil {
		t.Errorf("findUnregisteredSkills with no skills dir = %v, want nil", got)
	}
}

// TestFindUnregisteredSkills_SeesUserGlobal proves that an
// unregistered skill in the user-global layer is surfaced exactly
// like an unregistered workdir-local one. The user must explicitly
// `omac register` it before `omac start` is willing to proceed.
func TestFindUnregisteredSkills_SeesUserGlobal(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))

	wd := stageWorkdir(t, "alpha") // alpha registered below
	globalRoot := filepath.Join(home, ".config", "opencode", "skills")
	if err := os.MkdirAll(filepath.Join(globalRoot, "bravo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(globalRoot, "bravo", "omac.yaml"), []byte("name: bravo\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	reg := &registry.Registry{Registered: []registry.Entry{{Name: "alpha"}}}
	got, err := findUnregisteredSkills(wd, config.DefaultHarness(), reg)
	if err != nil {
		t.Fatalf("findUnregisteredSkills: %v", err)
	}
	want := []string{"bravo"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("findUnregisteredSkills = %v, want %v", got, want)
	}
}

// TestFindUnregisteredSkills_WorkdirHidesUserGlobalDup proves that
// when the same skill name exists in both layers, the workdir version
// shadows the user-global one. Dedup happens inside skillsource.
func TestFindUnregisteredSkills_WorkdirHidesUserGlobalDup(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))

	// Workdir-local 'shared' is registered; user-global 'shared' should
	// be hidden by workdir precedence and must NOT appear as
	// unregistered (it's the same logical skill).
	wd := stageWorkdir(t, "shared")
	globalRoot := filepath.Join(home, ".config", "opencode", "skills", "shared")
	if err := os.MkdirAll(globalRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(globalRoot, "omac.yaml"), []byte("name: shared\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	reg := &registry.Registry{Registered: []registry.Entry{{Name: "shared"}}}
	got, err := findUnregisteredSkills(wd, config.DefaultHarness(), reg)
	if err != nil {
		t.Fatalf("findUnregisteredSkills: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("user-global dup should be hidden by workdir; got %v", got)
	}
}
