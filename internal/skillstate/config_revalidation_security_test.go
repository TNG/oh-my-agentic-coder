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

// TestSecurityPatternlessConfigFieldRejectedForSecretSkill asserts that a
// skill declaring both secrets and a string config field without a pattern or
// choices is rejected at validation time. The agent-writable config store can
// set an unconstrained field to any value, and the sidecar uses that value at
// runtime alongside those secrets — so the manifest must refuse the
// combination rather than relying on launch-time revalidation alone.
func TestSecurityPatternlessConfigFieldRejectedForSecretSkill(t *testing.T) {
	withSecrets := []config.SecretSpec{{Name: "API_KEY"}}

	// Control: a patterned string field alongside secrets validates fine, so
	// the fix targets the missing-constraint case, not string fields in general.
	ok := meta(nil, withSecrets, []config.ConfigSpec{
		{Name: "URL", Type: config.ConfigFieldString, Pattern: "^https://[a-z.]+$"},
	})
	if err := ok.Validate(); err != nil {
		t.Fatalf("control: a patterned string field on a secret-holding skill was rejected: %v", err)
	}

	// A patternless, choice-less string field on a secret-holding skill must
	// not pass validation.
	bad := meta(nil, withSecrets, []config.ConfigSpec{
		{Name: "URL", Type: config.ConfigFieldString},
	})
	if err := bad.Validate(); err == nil {
		t.Error("a secret-holding skill with a patternless string config field passed validation: " +
			"the field accepts any value from the agent-writable config store")
	}

	// Negative control: a patternless string field WITHOUT secrets is fine —
	// the constraint is about the surface secrets add.
	noSecrets := meta(nil, nil, []config.ConfigSpec{
		{Name: "URL", Type: config.ConfigFieldString},
	})
	if err := noSecrets.Validate(); err != nil {
		t.Fatalf("negative control: a patternless string field without secrets was rejected: %v", err)
	}
}

// TestSecurityWorkdirConfigOverrideRequiresReapproval asserts that a
// workdir-layer config value differing from the approved global-layer value
// does not silently win the merge and reach the sidecar. The workdir
// skill-config.yaml is agent-writable and outside the hashed bundle, so
// changing it after approval does not trigger bundle-drift — the override
// must be refused or reported as a problem requiring re-registration.
func TestSecurityWorkdirConfigOverrideRequiresReapproval(t *testing.T) {
	dir := skillDir(t)
	e := entry(t, "probe", dir)
	spec := config.ConfigSpec{Name: "URL", Type: config.ConfigFieldString, Pattern: "^https://.+"}
	m := meta(nil, nil, []config.ConfigSpec{spec})

	// Control: a single-layer (global-only) value resolves cleanly, so a
	// fix that rejects all stored values would not pass vacuously.
	globalOnly := &skillconfig.Store{Version: skillconfig.SchemaVersion}
	globalOnly.Set("probe", "URL", "https://primary.example.com")

	r := New(Options{Env: env(nil)})
	armed, problems := r.Resolve(m, e, dir, globalOnly)
	defer armed.Zero()
	if got := armed.Config["URL"]; got != "https://primary.example.com" || len(problems) != 0 {
		t.Fatalf("control: a single-layer stored value was not resolved cleanly (value=%q, problems=%v): "+
			"the fixture is broken, not the security property", got, problems)
	}

	// A workdir-layer value that differs from the global-layer value must
	// not silently replace it in the merged store the sidecar reads.
	global := &skillconfig.Store{Version: skillconfig.SchemaVersion}
	global.Set("probe", "URL", "https://primary.example.com")

	workdir := &skillconfig.Store{Version: skillconfig.SchemaVersion}
	workdir.Set("probe", "URL", "https://secondary.example.com")

	merged := MergeConfig(global, workdir)
	armed2, problems2 := r.Resolve(m, e, dir, merged)
	defer armed2.Zero()

	if got := armed2.Config["URL"]; got == "https://secondary.example.com" && len(problems2) == 0 {
		t.Errorf("a workdir-layer override %q silently replaced the approved value %q "+
			"and reached armed.Config with no problem: the sidecar would use the "+
			"tampered value without re-registration", got, "https://primary.example.com")
	}
}
