package skillstate

import (
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
	"github.com/TNG/oh-my-agentic-coder/internal/skillconfig"
)

// TestSecurityWorkdirEnvControlVarRefusedOnLegacySkill asserts that a value
// sourced from the agent-writable workdir config layer for a code-execution
// environment variable is refused even on a legacy (unanchored) skill that
// declares no secrets. The workdir layer is agent-writable, so such a value
// must never reach armed.Config — and thus never the sidecar environment —
// regardless of the skill's secret surface.
func TestSecurityWorkdirEnvControlVarRefusedOnLegacySkill(t *testing.T) {
	dir := skillDir(t)
	e := entry(t, "probe", dir)
	spec := config.ConfigSpec{Name: "PYTHONPATH", Type: config.ConfigFieldString}
	m := meta(nil, nil, []config.ConfigSpec{spec})

	global := &skillconfig.Store{Version: skillconfig.SchemaVersion}
	global.Set("probe", "PYTHONPATH", "/usr/local/lib/site-packages")
	workdir := &skillconfig.Store{Version: skillconfig.SchemaVersion}
	workdir.Set("probe", "PYTHONPATH", "/tmp/extra-paths")

	merged := MergeConfig(global, workdir)
	if _, anchored := merged.ApprovedFor("", "probe"); anchored {
		t.Fatal("fixture broken: expected a legacy unanchored skill")
	}

	r := New(Options{Env: env(nil)})
	armed, problems := r.Resolve(m, e, dir, merged)
	defer armed.Zero()

	if got := armed.Config["PYTHONPATH"]; got == "/tmp/extra-paths" {
		t.Errorf("an agent-writable workdir value %q reached armed.Config", got)
	}
	if !Has(problems, InvalidConfig) {
		t.Errorf("no InvalidConfig problem reported for a workdir-sourced control variable: %v", problems)
	}
}

// TestSecurityEnvControlDenylistAppliesToAnchoredSkill pins that a workdir
// override of a code-execution environment variable is refused for an anchored
// skill when it differs from the host-approved anchor, while a workdir value
// equal to the anchor is still accepted. The equal case is the anchor-match
// exception that lets a legitimate re-registration re-supply the same
// host-approved value.
func TestSecurityEnvControlDenylistAppliesToAnchoredSkill(t *testing.T) {
	dir := skillDir(t)
	e := entry(t, "probe", dir)
	spec := config.ConfigSpec{Name: "NODE_OPTIONS", Type: config.ConfigFieldString}
	m := meta(nil, nil, []config.ConfigSpec{spec})

	const approved = "--max-old-space-size=256"
	const override = "--max-old-space-size=512"

	global := &skillconfig.Store{Version: skillconfig.SchemaVersion}
	global.Set("probe", "NODE_OPTIONS", approved)
	global.RecordApproved("wd", "probe", map[string]string{"NODE_OPTIONS": approved})

	r := New(Options{Scope: "wd"})

	// A workdir-sourced override that differs from the anchor is refused.
	diff := &skillconfig.Store{Version: skillconfig.SchemaVersion}
	diff.Set("probe", "NODE_OPTIONS", override)
	armed, problems := r.Resolve(m, e, dir, MergeConfig(global, diff))
	defer armed.Zero()
	if got := armed.Config["NODE_OPTIONS"]; got == override {
		t.Errorf("a workdir override %q differing from the anchor reached armed.Config", got)
	}
	if !Has(problems, InvalidConfig) {
		t.Errorf("no InvalidConfig problem for a workdir override differing from the anchor: %v", problems)
	}

	// A workdir value equal to the anchor is accepted (anchor-match exception).
	match := &skillconfig.Store{Version: skillconfig.SchemaVersion}
	match.Set("probe", "NODE_OPTIONS", approved)
	armed2, problems2 := r.Resolve(m, e, dir, MergeConfig(global, match))
	defer armed2.Zero()
	if got := armed2.Config["NODE_OPTIONS"]; got != approved {
		t.Errorf("a workdir value equal to the anchor was refused (value=%q, problems=%v): the anchor-match exception must preserve a re-supplied host-approved value", got, problems2)
	}
}
