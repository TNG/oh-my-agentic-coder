package cli

// Issue #514: `omac doctor` lists the registered skills whose sidecar the
// spawn gate would refuse, with the command that fixes each, so a user who
// just upgraded can see the one-time rebuild without launching a session.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/registry"
	"github.com/TNG/oh-my-agentic-coder/internal/skilltrust"
)

// writeSidecarManifest replaces stageWorkdir's bare omac.yaml with one that
// declares a sidecar, so doctor's per-skill check does not fail the skill
// before the spawn-gate check is reached.
func writeSidecarManifest(t *testing.T, dir, name string) {
	t.Helper()
	manifest := "name: " + name + "\ntype: skill\nsidecar:\n  command: [\"python3\", \"sidecar.py\"]\n  mount: " + name + "\n"
	if err := os.WriteFile(filepath.Join(dir, "omac.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDoctorReportsSpawnGateRefusals(t *testing.T) {
	isolateHome(t)
	workdir := stageWorkdir(t, "fine", "stale", "fresh")
	dir := func(name string) string { return filepath.Join(workdir, ".opencode", "skills", name) }
	for _, name := range []string{"fine", "stale", "fresh"} {
		writeSidecarManifest(t, dir(name), name)
	}
	h := bundleHashOf(t, dir("fine"))
	if err := skilltrust.Approve("fine", h, dir("fine")); err != nil {
		t.Fatal(err)
	}
	stageLegacySnapshot(t, "stale", dir("stale"))
	reg := &registry.Registry{Version: registry.SchemaVersion}
	for _, name := range []string{"fine", "stale", "fresh"} {
		reg.Registered = append(reg.Registered, registry.Entry{
			Name: name, Harness: "opencode", SkillDir: filepath.Join(".opencode", "skills", name)})
	}
	if err := registry.Save(workdir, reg); err != nil {
		t.Fatal(err)
	}

	env, outBuf, _, drain := newPipeEnv(t, "")
	env.Workdir = workdir
	code := runDoctor([]string{}, env)
	drain()
	out := outBuf.String()

	if code != ExitOK {
		t.Errorf("doctor exit = %d, want ExitOK (refusals are warnings)", code)
	}
	for _, want := range []string{
		"[warn] skill snapshots: 1 skill(s) registered before " + skilltrust.SnapshotPolicySince + ", snapshot must be rebuilt once for the new store format; run each command from a host terminal (see docs/troubleshooting.md#upgrading):",
		"  [warn] stale                omac register stale --harness opencode",
		"[warn] skill registrations: 1 skill(s) not registered from a host terminal yet; run each command from a host terminal:",
		"  [warn] fresh                omac register fresh --harness opencode",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "omac register fine") {
		t.Errorf("an approved skill with a current snapshot must not be listed:\n%s", out)
	}
}

func TestDoctorSpawnGateAllClear(t *testing.T) {
	isolateHome(t)
	workdir := stageWorkdir(t, "fine")
	d := filepath.Join(workdir, ".opencode", "skills", "fine")
	writeSidecarManifest(t, d, "fine")
	if err := skilltrust.Approve("fine", bundleHashOf(t, d), d); err != nil {
		t.Fatal(err)
	}
	reg := &registry.Registry{Version: registry.SchemaVersion, Registered: []registry.Entry{
		{Name: "fine", Harness: "opencode", SkillDir: filepath.Join(".opencode", "skills", "fine")}}}
	if err := registry.Save(workdir, reg); err != nil {
		t.Fatal(err)
	}
	env, outBuf, _, drain := newPipeEnv(t, "")
	env.Workdir = workdir
	_ = runDoctor([]string{}, env)
	drain()
	if out := outBuf.String(); !strings.Contains(out, "[ok] skill approvals: 1 skill(s) approved with a current snapshot") {
		t.Errorf("doctor output missing the all-clear line:\n%s", out)
	}
}
