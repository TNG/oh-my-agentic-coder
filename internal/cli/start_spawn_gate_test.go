package cli

// Issue #514: the launch's spawn gate collects its refusals into one grouped
// report and hands the reloader what it already reported, so a reload right
// after launch does not print every refusal a second time.

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/facade"
	"github.com/TNG/oh-my-agentic-coder/internal/registry"
	"github.com/TNG/oh-my-agentic-coder/internal/skillstate"
	"github.com/TNG/oh-my-agentic-coder/internal/skilltrust"
)

func TestGateApprovedSkillsSplitsAndReports(t *testing.T) {
	isolateHome(t)
	workdir := stageWorkdir(t, "ok", "no")
	armed := func(name string) skillstate.Armed {
		e := registry.Entry{Name: name, Harness: "opencode", SkillDir: filepath.Join(".opencode", "skills", name)}
		return skillstate.Armed{Entry: e, AbsDir: filepath.Join(workdir, e.SkillDir), Mount: name}
	}
	okDir := filepath.Join(workdir, ".opencode", "skills", "ok")
	h := bundleHashOf(t, okDir)
	if err := skilltrust.Approve("ok", h, okDir); err != nil {
		t.Fatal(err)
	}
	snap, _ := skilltrust.SnapshotPath("ok", h)

	gate := gateApprovedSkills([]skillstate.Armed{armed("ok"), armed("no")}, workdir)

	if len(gate.approved) != 1 || gate.approved[0].Entry.Name != "ok" || gate.approved[0].AbsDir != snap {
		t.Errorf("approved = %+v, want only ok spawned from %s", gate.approved, snap)
	}
	if len(gate.refused) != 1 || gate.refused[0].Skill != "no" || gate.refused[0].State != facade.RouteBroken {
		t.Fatalf("refused routes = %+v, want one broken route for no", gate.refused)
	}
	if d := gate.refused[0].Detail; !strings.Contains(d, "`omac register no --harness opencode`") {
		t.Errorf("route detail lacks the harness-aware command: %s", d)
	}
	if len(gate.problems) != 1 || gate.problems[0].Kind != skillstate.NotApproved || gate.problems[0].Skill != "no" {
		t.Errorf("problems = %+v, want one NotApproved for no", gate.problems)
	}
	// The reloader dedupes on the exact detail string, so the seed must be
	// the same text reload would compute for an unchanged refusal.
	if gate.warned["no"] != gate.refused[0].Detail {
		t.Errorf("warned seed = %q, want the refusal detail %q", gate.warned["no"], gate.refused[0].Detail)
	}
	if _, seeded := gate.warned["ok"]; seeded {
		t.Error("an approved skill must not be seeded as warned")
	}
}

func TestStartReloaderSeededWarningsAreNotRepeated(t *testing.T) {
	r := newStartReloaderForTest(t)
	r.warned = map[string]string{"no": "spawn refused: same reason"}
	if r.warnOnce("no", "spawn refused: same reason") {
		t.Error("a refusal the launch already printed was printed again by reload")
	}
	if !r.warnOnce("no", "spawn refused: changed reason") {
		t.Error("a changed reason must still be reported")
	}
}

// stageRefusedSkill writes a workdir skill with a sidecar manifest and
// registers it for opencode without approving it, so the spawn gate refuses it.
func stageRefusedSkill(t *testing.T, workdir, name, rel string) registry.Entry {
	t.Helper()
	dir := filepath.Join(workdir, rel)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSidecarManifest(t, dir, name)
	// The recorded hash matches the files, so serve's drift check passes
	// and the spawn gate is what refuses the skill.
	e := registry.Entry{Name: name, Harness: "opencode", SkillDir: rel, BundleHash: bundleHashOf(t, dir)}
	if err := registry.WithLock(workdir, func() error {
		reg, err := registry.Load(workdir)
		if err != nil {
			return err
		}
		reg.Upsert(e)
		return registry.Save(workdir, reg)
	}); err != nil {
		t.Fatal(err)
	}
	return e
}

// TestReloadDoesNotRepeatLaunchRefusals (F6): the reloader seeded from the
// launch gate prints nothing for a refusal the launch already reported, for a
// workdir and a user-global skill alike, prints one short line when a reason
// changes, and never lets a control character through. The routes it mounts
// carry the copy-selecting command.
func TestReloadDoesNotRepeatLaunchRefusals(t *testing.T) {
	isolateHome(t)
	workdir := t.TempDir()
	local := stageRefusedSkill(t, workdir, "local", filepath.Join(".opencode", "skills", "local"))
	globDir := stageUserGlobalSkill(t, "glob")
	writeSidecarManifest(t, globDir, "glob")
	glob := registry.Entry{Name: "glob", Harness: "opencode", SkillDir: globDir}
	if err := registry.SaveGlobal(&registry.Registry{Version: registry.SchemaVersion, Registered: []registry.Entry{glob}}); err != nil {
		t.Fatal(err)
	}

	armed := func(e registry.Entry) skillstate.Armed {
		abs := e.SkillDir
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(workdir, abs)
		}
		return skillstate.Armed{Entry: e, AbsDir: abs, Mount: e.Name}
	}
	gate := gateApprovedSkills([]skillstate.Armed{armed(local), armed(glob)}, workdir)
	if len(gate.problems) != 2 {
		t.Fatalf("launch gate problems = %+v, want two refusals", gate.problems)
	}

	r, baseURL := newLiveReloader(t, workdir)
	env, _, errBuf, drain := newPipeEnv(t, "")
	env.Workdir = workdir
	r.env = env
	r.warned = gate.warned

	r.reload()
	drain()
	if out := errBuf.String(); strings.Contains(out, "will not run") || strings.Contains(out, "spawn refused") {
		t.Errorf("reload repeated a refusal the launch already reported:\n%s", out)
	}
	for mount, want := range map[string]string{
		"local": "`omac register local --harness opencode`",
		"glob":  "`omac register glob --harness opencode --global`",
	} {
		resp, body := httpGet(t, baseURL+"/"+mount+"/x")
		if resp.StatusCode != http.StatusBadGateway || !strings.Contains(body, want) {
			t.Errorf("%s route: status %d body %q, want 502 containing %s", mount, resp.StatusCode, body, want)
		}
	}

	// A changed reason is reported once, in the report's words. Approving
	// and then deleting the snapshot moves "local" from not-approved to
	// snapshot-missing; a skill registered mid-session with an escape
	// sequence in its directory name is new, so it prints too, sanitized.
	localDir := filepath.Join(workdir, local.SkillDir)
	h := bundleHashOf(t, localDir)
	if err := skilltrust.Approve("local", h, localDir); err != nil {
		t.Fatal(err)
	}
	snap, _ := skilltrust.SnapshotPath("local", h)
	if err := os.RemoveAll(snap); err != nil {
		t.Fatal(err)
	}
	stageRefusedSkill(t, workdir, "esc", filepath.Join(".opencode", "skills", "esc\x1b[2J"))

	env2, _, errBuf2, drain2 := newPipeEnv(t, "")
	env2.Workdir = workdir
	r.env = env2
	r.reload()
	drain2()
	out := errBuf2.String()
	if want := "omac start: local will not run: snapshot missing from the host store; run from a host terminal: omac register local --harness opencode"; !strings.Contains(out, want) {
		t.Errorf("changed reason not reported; want %q in:\n%s", want, out)
	}
	if !strings.Contains(out, "omac start: esc will not run:") || strings.Contains(out, "\x1b") {
		t.Errorf("new skill line missing or unsanitized:\n%q", out)
	}
	if strings.Contains(out, "glob will not run") {
		t.Errorf("unchanged refusal printed again:\n%s", out)
	}
}
