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

// TestSecurityEnvControlDenylistAppliesToAnchoredSkill asserts that a
// code-execution environment variable sourced from the agent-writable workdir
// layer is refused for an anchored skill too. The anchor records what the host
// approved at register time, but the workdir layer remains agent-writable, so a
// value sourced from it must never reach armed.Config for such a name — even
// when it coincidentally equals the anchor. A host-only global value for the
// same name still resolves (control), so the refusal is proven to track the
// workdir provenance, not the field name alone.
func TestSecurityEnvControlDenylistAppliesToAnchoredSkill(t *testing.T) {
	dir := skillDir(t)
	e := entry(t, "probe", dir)
	spec := config.ConfigSpec{Name: "NODE_OPTIONS", Type: config.ConfigFieldString}
	m := meta(nil, nil, []config.ConfigSpec{spec})

	const approved = "--max-old-space-size=256"
	r := New(Options{Scope: "wd"})

	// Control: a denylisted name whose value comes only from the host-only
	// global layer, matching the host-approved anchor, resolves normally. The
	// denylist guards the agent-writable workdir layer, not host-approved
	// values, so a fix that refused every value for the name would not make
	// this control pass for the wrong reason.
	hostOnly := &skillconfig.Store{Version: skillconfig.SchemaVersion}
	hostOnly.Set("probe", "NODE_OPTIONS", approved)
	hostOnly.RecordApproved("wd", "probe", map[string]string{"NODE_OPTIONS": approved})
	armed, problems := r.Resolve(m, e, dir, hostOnly)
	defer armed.Zero()
	if got := armed.Config["NODE_OPTIONS"]; got != approved || Has(problems, InvalidConfig) {
		t.Fatalf("control: a host-only denylisted value did not resolve (value=%q, problems=%v): the fixture is broken, not the security property", got, problems)
	}

	// The workdir layer supplies the same value the host approved, with no
	// global value shadowing it, so MergeConfig marks it workdir-sourced. The
	// anchor match must NOT redeem an agent-writable source for a
	// code-execution env var: the sidecar would run with a value the sandbox
	// can change at any time.
	workdir := &skillconfig.Store{Version: skillconfig.SchemaVersion}
	workdir.Set("probe", "NODE_OPTIONS", approved)
	global := &skillconfig.Store{Version: skillconfig.SchemaVersion}
	global.RecordApproved("wd", "probe", map[string]string{"NODE_OPTIONS": approved})
	merged := MergeConfig(global, workdir)
	if _, wdSourced := merged.FromWorkdir["probe"]["NODE_OPTIONS"]; !wdSourced {
		t.Fatalf("fixture broken: the workdir value was not marked workdir-sourced (got %+v)", merged.FromWorkdir)
	}

	armed2, problems2 := r.Resolve(m, e, dir, merged)
	defer armed2.Zero()
	if got, ok := armed2.Config["NODE_OPTIONS"]; ok && got == approved {
		t.Errorf("a workdir-sourced denylisted value %q reached armed.Config for an anchored skill: the agent-writable layer must not supply code-execution env vars", got)
	}
	if !Has(problems2, InvalidConfig) {
		t.Errorf("no InvalidConfig problem for a workdir-sourced denylisted value on an anchored skill: %v", problems2)
	}
}
