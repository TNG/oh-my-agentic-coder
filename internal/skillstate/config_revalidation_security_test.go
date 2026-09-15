//go:build vuln

package skillstate

import (
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
	"github.com/TNG/oh-my-agentic-coder/internal/skillconfig"
)

// TestSecurityStoredConfigRevalidatedAtLaunch asserts that a config value
// violating the skill's declared pattern or choices does not reach the
// sidecar's environment just because it is already sitting in
// skill-config.yaml.
//
// resolveConfig documents, as an explicit decision, that stored values are
// not re-checked against spec.Pattern/Choices at launch — only at `omac
// register` prompt time. skill-config.yaml lives beside the workdir
// (skillconfig.Path), outside the hash BundleHash covers, so editing it
// after approval is a way to hand the sidecar a value its own manifest
// declares invalid, without tripping bundle-drift detection.
func TestSecurityStoredConfigRevalidatedAtLaunch(t *testing.T) {
	dir := skillDir(t)
	e := entry(t, "probe", dir)
	spec := config.ConfigSpec{Name: "MODE", Choices: []string{"safe", "strict"}}
	m := meta(nil, nil, []config.ConfigSpec{spec})

	store := &skillconfig.Store{Version: skillconfig.SchemaVersion}
	store.Set("probe", "MODE", "safe")

	r := New(Options{Env: env(nil)})

	// Control: a value on the declared choices list resolves normally, so
	// a fix that refuses every stored value wouldn't make this test pass
	// for the wrong reason.
	armed, problems := r.Resolve(m, e, dir, store)
	defer armed.Zero()
	if got := armed.Config["MODE"]; got != "safe" || len(problems) != 0 {
		t.Fatalf("control: a valid stored choice was not resolved cleanly (value=%q, problems=%v): the fixture is broken, not the security property", got, problems)
	}

	hostile := &skillconfig.Store{Version: skillconfig.SchemaVersion}
	hostile.Set("probe", "MODE", "delete-everything")

	armed2, problems2 := r.Resolve(m, e, dir, hostile)
	defer armed2.Zero()

	if got := armed2.Config["MODE"]; got == "delete-everything" && len(problems2) == 0 {
		t.Errorf("a stored config value %q outside the declared choices %v reached armed.Config with no problem reported: "+
			"it will be injected into the sidecar's environment verbatim", got, spec.Choices)
	}
}
