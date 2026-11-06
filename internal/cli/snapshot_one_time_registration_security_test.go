package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
	"github.com/TNG/oh-my-agentic-coder/internal/registry"
	"github.com/TNG/oh-my-agentic-coder/internal/skilltrust"
)

// Registering a skill once must be enough for every later launch. The frozen
// snapshot is reused, even when the interpreter writes version-tagged bytecode
// caches into it, and excluded files appearing in the workdir must not ask the
// user to register again. Only a change to hashed source may do that.
//
// This pins the user-facing promise of the snapshot-policy change: the
// one-time re-registration after upgrading happens once, not per launch and
// not per upgrade.
func TestSecuritySnapshotRegistrationIsOneTime(t *testing.T) {
	isolateHome(t)
	const name = "one-time"
	workdir := stageWorkdir(t, name)
	src := filepath.Join(workdir, ".opencode", "skills", name)
	manifest := "name: one-time\ntype: skill\nsidecar:\n  command: [\"python3\", \"sidecar.py\"]\n  mount: one-time\n"
	for rel, body := range map[string]string{
		config.MetaFileName: manifest,
		"sidecar.py":        "import helper\nprint(helper.hello())\n",
		"helper.py":         "def hello():\n    return 'hi'\n",
	} {
		if err := os.WriteFile(filepath.Join(src, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	env := makeEnv(workdir)
	defer env.Stdout.Close()
	if code := runRegister([]string{name, "--no-secrets", "--no-fields"}, env); code != ExitOK {
		t.Fatalf("register exit %d", code)
	}

	h := bundleHashOf(t, src)
	snap, ok := skilltrust.SnapshotPath(name, h)
	if !ok {
		t.Fatal("no current-policy snapshot after the single registration")
	}
	storePath := filepath.Join(registry.GlobalDir(), "approvals.json")
	storeBefore, err := os.ReadFile(storePath)
	if err != nil {
		t.Fatal(err)
	}
	// Mark the snapshot so a silent rebuild is detectable: replacing the
	// directory would lose this file, even though the path stays the same.
	marker := filepath.Join(snap, ".one-time-marker")
	if err := os.WriteFile(marker, []byte("original snapshot"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Repeated launches must keep resolving to the same snapshot without any
	// further registration.
	for i := 1; i <= 3; i++ {
		got, err := approvedSpawnDir(name, src, "")
		if err != nil {
			t.Fatalf("launch %d refused an approved skill: %v", i, err)
		}
		if got != snap {
			t.Fatalf("launch %d used %q, want %q", i, got, snap)
		}
	}

	// The sidecar runs with the snapshot as cwd, so its interpreter writes
	// __pycache__ there. Those files are version-tagged and excluded from the
	// bundle hash; they must not invalidate the approval or force a rebuild.
	for _, rel := range []string{
		filepath.Join("__pycache__", "helper.cpython-311.pyc"),
		filepath.Join("__pycache__", "helper.cpython-312.pyc"),
	} {
		p := filepath.Join(snap, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("bytecode cache"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// The same interpreter may also leave excluded files in the workdir.
	if err := os.WriteFile(filepath.Join(src, "module.pyc"), []byte("stale bytecode"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := bundleHashOf(t, src); got != h {
		t.Fatalf("excluded workdir file changed the approved hash: %s", got)
	}
	got, err := approvedSpawnDir(name, src, "")
	if err != nil {
		t.Fatalf("launch after cache churn refused: %v", err)
	}
	if got != snap {
		t.Fatalf("launch after cache churn used %q, want %q", got, snap)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("a launch rebuilt the snapshot instead of reusing it: %v", err)
	}
	if after, err := os.ReadFile(storePath); err != nil || !bytes.Equal(storeBefore, after) {
		t.Errorf("repeated launches rewrote the approval store: %v", err)
	}

	// Positive control: the promise is one-time for unchanged source. A
	// changed hashed file still requires host registration.
	if err := os.WriteFile(filepath.Join(src, "sidecar.py"), []byte("import helper\nprint('changed')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := approvedSpawnDir(name, src, ""); err == nil || got != "" {
		t.Fatalf("changed source spawned without re-registration: %q, %v", got, err)
	}
}
