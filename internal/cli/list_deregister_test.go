package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tngtech/oh-my-agentic-coder/internal/registry"
)

// captureEnv returns an Env whose Stdout/Stderr are temp files plus a
// reader for their contents.
func captureEnv(t *testing.T, workdir string) (*Env, func() (string, string)) {
	t.Helper()
	out, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	errf, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	null, _ := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	env := &Env{Workdir: workdir, Stdout: out, Stderr: errf, Stdin: null, Version: "test"}
	read := func() (string, string) {
		ob, _ := os.ReadFile(out.Name())
		eb, _ := os.ReadFile(errf.Name())
		return string(ob), string(eb)
	}
	return env, read
}

// stageRegisteredSkill creates a workdir skill dir with omac.yaml and a
// registry entry for it.
func stageRegisteredSkill(t *testing.T, workdir, name string) {
	t.Helper()
	skillDir := filepath.Join(workdir, ".opencode", "skills", name)
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := "name: " + name + "\ntype: skill\nversion: 0.1.0\ndescription: d\n" +
		"sidecar:\n  command: [\"python3\", \"s.py\"]\n  mount: " + name + "\n  health: {path: /status}\n  protocols: [\"http\"]\n"
	if err := os.WriteFile(filepath.Join(skillDir, "omac.yaml"), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}
	rel := filepath.Join(".opencode", "skills", name)
	reg, err := registry.Load(workdir)
	if err != nil {
		t.Fatal(err)
	}
	reg.Upsert(registry.Entry{Name: name, SkillDir: rel, BundleHash: "x"})
	if err := registry.Save(workdir, reg); err != nil {
		t.Fatal(err)
	}
}

func TestListHidesStaleSkill(t *testing.T) {
	isolateHome(t)
	wd := t.TempDir()
	stageRegisteredSkill(t, wd, "demo")

	// Sanity: it lists while present.
	env, read := captureEnv(t, wd)
	if code := runList(nil, env); code != ExitOK {
		t.Fatalf("list code = %d", code)
	}
	if out, _ := read(); !strings.Contains(out, "demo") {
		t.Fatalf("live skill not listed: %q", out)
	}

	// Delete the skill directory; the registry entry survives.
	if err := os.RemoveAll(filepath.Join(wd, ".opencode", "skills", "demo")); err != nil {
		t.Fatal(err)
	}

	env, read = captureEnv(t, wd)
	if code := runList(nil, env); code != ExitOK {
		t.Fatalf("list code = %d", code)
	}
	out, errOut := read()
	// The deleted skill must NOT appear as a live row...
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "demo") {
			t.Errorf("stale skill listed as live: %q", line)
		}
	}
	// ...but the user is told it's a stale registration and how to remove it.
	if !strings.Contains(errOut, "stale registration") || !strings.Contains(errOut, "omac deregister demo") {
		t.Errorf("stale notice missing: %q", errOut)
	}
}

func TestListAllShowsStale(t *testing.T) {
	isolateHome(t)
	wd := t.TempDir()
	stageRegisteredSkill(t, wd, "demo")
	if err := os.RemoveAll(filepath.Join(wd, ".opencode", "skills", "demo")); err != nil {
		t.Fatal(err)
	}
	env, read := captureEnv(t, wd)
	if code := runList(nil, env); code != ExitOK {
		t.Fatalf("list code = %d", code)
	}
	out, _ := read()
	if !strings.Contains(out, "demo") || !strings.Contains(out, "missing") {
		t.Errorf("stale row should show with STATUS=missing: %q", out)
	}
}

func TestDeregisterPruneRemovesStale(t *testing.T) {
	isolateHome(t)
	wd := t.TempDir()
	stageRegisteredSkill(t, wd, "live")
	stageRegisteredSkill(t, wd, "dead")
	if err := os.RemoveAll(filepath.Join(wd, ".opencode", "skills", "dead")); err != nil {
		t.Fatal(err)
	}

	env, read := captureEnv(t, wd)
	if code := runDeregister([]string{"--prune"}, env); code != ExitOK {
		t.Fatalf("prune code = %d", code)
	}
	out, _ := read()
	if !strings.Contains(out, "pruned stale registration dead") {
		t.Errorf("prune output = %q", out)
	}

	reg, err := registry.Load(wd)
	if err != nil {
		t.Fatal(err)
	}
	if _, idx := reg.Find("dead"); idx != -1 {
		t.Error("dead entry survived prune")
	}
	if _, idx := reg.Find("live"); idx == -1 {
		t.Error("live entry was wrongly pruned")
	}
}

// stageUnregisteredSkill creates a workdir skill dir with omac.yaml but
// NO registry entry (the discovered-but-unregistered case).
func stageUnregisteredSkill(t *testing.T, workdir, name string) string {
	t.Helper()
	skillDir := filepath.Join(workdir, ".opencode", "skills", name)
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := "name: " + name + "\ntype: skill\nversion: 0.1.0\ndescription: d\n" +
		"sidecar:\n  command: [\"python3\", \"s.py\"]\n  mount: " + name + "\n  health: {path: /status}\n  protocols: [\"http\"]\n"
	if err := os.WriteFile(filepath.Join(skillDir, "omac.yaml"), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}
	return skillDir
}

func TestDeregisterDeletesUnregisteredOnDiskSkill(t *testing.T) {
	isolateHome(t)
	wd := t.TempDir()
	dir := stageUnregisteredSkill(t, wd, "apple-calendar")

	// --yes so the deletion runs without an interactive prompt.
	env, read := captureEnv(t, wd)
	if code := runDeregister([]string{"--yes", "apple-calendar"}, env); code != ExitOK {
		t.Fatalf("deregister code = %d", code)
	}
	out, _ := read()
	if !strings.Contains(out, "deleted unregistered skill apple-calendar") {
		t.Errorf("output = %q", out)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("source dir should be gone, stat err = %v", err)
	}
}

func TestDeregisterKeepsRegisteredSkillFiles(t *testing.T) {
	isolateHome(t)
	wd := t.TempDir()
	stageRegisteredSkill(t, wd, "keepme")
	dir := filepath.Join(wd, ".opencode", "skills", "keepme")

	env, read := captureEnv(t, wd)
	if code := runDeregister([]string{"keepme"}, env); code != ExitOK {
		t.Fatalf("deregister code = %d", code)
	}
	out, _ := read()
	if !strings.Contains(out, "deregistered keepme") {
		t.Errorf("output = %q", out)
	}
	// A registered skill is removed from the registry only; its files
	// must survive (deleting source is reserved for unregistered ones).
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("registered skill's files must be kept, stat err = %v", err)
	}
}

func TestDeregisterUnknownSkillNoop(t *testing.T) {
	isolateHome(t)
	wd := t.TempDir()
	env, read := captureEnv(t, wd)
	if code := runDeregister([]string{"--yes", "ghost"}, env); code != ExitOK {
		t.Fatalf("deregister code = %d", code)
	}
	out, _ := read()
	if !strings.Contains(out, "was not registered and no skill of that name was found") {
		t.Errorf("output = %q", out)
	}
}

func TestDeregisterGlobalRemovesStaleGlobalEntry(t *testing.T) {
	isolateHome(t)
	wd := t.TempDir()

	// Stage a stale GLOBAL entry pointing at a nonexistent absolute dir.
	if err := registry.WithGlobalLock(func() error {
		g := &registry.Registry{Version: registry.SchemaVersion}
		g.Upsert(registry.Entry{Name: "ghost", SkillDir: filepath.Join(t.TempDir(), "gone"), BundleHash: "x"})
		return registry.SaveGlobal(g)
	}); err != nil {
		t.Fatal(err)
	}

	// It must not appear as a live skill, but must be flagged stale.
	env, read := captureEnv(t, wd)
	runList(nil, env)
	_, errOut := read()
	if !strings.Contains(errOut, "ghost") || !strings.Contains(errOut, "omac deregister --global ghost") {
		t.Errorf("stale global notice missing: %q", errOut)
	}

	// --global removes it.
	env, read = captureEnv(t, wd)
	if code := runDeregister([]string{"--global", "ghost"}, env); code != ExitOK {
		t.Fatalf("deregister --global code = %d", code)
	}
	if out, _ := read(); !strings.Contains(out, "deregistered ghost") {
		t.Errorf("deregister output = %q", out)
	}
	g, err := registry.LoadGlobal()
	if err != nil {
		t.Fatal(err)
	}
	if _, idx := g.Find("ghost"); idx != -1 {
		t.Error("ghost survived deregister --global")
	}
}
