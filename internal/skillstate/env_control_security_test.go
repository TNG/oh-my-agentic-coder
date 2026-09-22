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

// TestSecurityEnvControlDenylistAppliesToAnchoredSkill asserts that the
// denylist coverage reaches anchored skills while the anchor-match exception
// is preserved: a workdir value equal to the host-approved anchor is accepted
// (legitimate re-supply of a host-approved interpreter flag), but a
// workdir-sourced override that differs from the anchor is refused. A
// host-only global value for the same name also resolves (control), so the
// acceptance is proven to track the anchor match, not a missing denylist.
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
	// anchor match redeems the agent-writable source: the value reaching the
	// sidecar is precisely the one the host approved, so legitimate
	// re-supply of a host-approved interpreter flag is not broken.
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
	if got := armed2.Config["NODE_OPTIONS"]; got != approved {
		t.Errorf("a workdir-sourced denylisted value equal to the anchor did not reach armed.Config (got %q): legitimate re-supply of a host-approved value must be accepted", got)
	}
	if Has(problems2, InvalidConfig) {
		t.Errorf("an anchor-matching workdir value was refused: %v", problems2)
	}

	// A workdir-sourced override that differs from the host-approved anchor
	// is refused: the anchor check rejects the mismatch before any denylist
	// consideration, so the sandbox cannot steer the sidecar with a flipped
	// value.
	const hostile = "--require=/tmp/evil.js"
	workdir2 := &skillconfig.Store{Version: skillconfig.SchemaVersion}
	workdir2.Set("probe", "NODE_OPTIONS", hostile)
	global2 := &skillconfig.Store{Version: skillconfig.SchemaVersion}
	global2.RecordApproved("wd", "probe", map[string]string{"NODE_OPTIONS": approved})
	merged2 := MergeConfig(global2, workdir2)
	if _, wdSourced := merged2.FromWorkdir["probe"]["NODE_OPTIONS"]; !wdSourced {
		t.Fatalf("fixture broken: the hostile workdir value was not marked workdir-sourced (got %+v)", merged2.FromWorkdir)
	}

	armed3, problems3 := r.Resolve(m, e, dir, merged2)
	defer armed3.Zero()
	if got, ok := armed3.Config["NODE_OPTIONS"]; ok && got == hostile {
		t.Errorf("a workdir-sourced override %q differing from the anchor reached armed.Config for an anchored skill", got)
	}
	if !Has(problems3, InvalidConfig) {
		t.Errorf("no InvalidConfig problem for a workdir-sourced override differing from the anchor on an anchored skill: %v", problems3)
	}
}
