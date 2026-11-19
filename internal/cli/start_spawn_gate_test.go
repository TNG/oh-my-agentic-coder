package cli

// Issue #514: the launch's spawn gate collects its refusals into one grouped
// report and hands the reloader what it already reported, so a reload right
// after launch does not print every refusal a second time.

import (
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
