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

// TestSecurityPatternlessConfigFieldIsFlaggedNotFatal asserts that a skill
// declaring both secrets and a string config field without a pattern or choices
// still validates, but is reported by PatternlessSecretStringFields so
// registration and `omac doctor` can warn. The field accepts any value from the
// agent-writable config store, which the sidecar then receives alongside its
// secrets; enforcement happens at launch (see the workdir-override and
// agent-writable tests), not by refusing the manifest.
func TestSecurityPatternlessConfigFieldIsFlaggedNotFatal(t *testing.T) {
	withSecrets := []config.SecretSpec{{Name: "API_KEY"}}

	bad := meta(nil, withSecrets, []config.ConfigSpec{
		{Name: "URL", Type: config.ConfigFieldString},
	})
	if err := bad.Validate(); err != nil {
		t.Fatalf("a secret-holding skill with a patternless string config field was rejected: %v", err)
	}
	if got := bad.PatternlessSecretStringFields(); len(got) != 1 || got[0] != "URL" {
		t.Errorf("PatternlessSecretStringFields = %v, want [URL]", got)
	}

	// Control: a patterned string field alongside secrets is not flagged.
	ok := meta(nil, withSecrets, []config.ConfigSpec{
		{Name: "URL", Type: config.ConfigFieldString, Pattern: "^https://[a-z.]+$"},
	})
	if got := ok.PatternlessSecretStringFields(); len(got) != 0 {
		t.Errorf("a patterned string field was flagged: %v", got)
	}

	// Negative control: a patternless string field WITHOUT secrets is fine —
	// the constraint is about the surface secrets add.
	noSecrets := meta(nil, nil, []config.ConfigSpec{
		{Name: "URL", Type: config.ConfigFieldString},
	})
	if got := noSecrets.PatternlessSecretStringFields(); len(got) != 0 {
		t.Errorf("a patternless field without secrets was flagged: %v", got)
	}
}

// TestSecurityWorkdirConfigOverrideRequiresReapproval asserts that once a
// (workdir, skill) anchor exists — records the values the user approved at
// register time — a workdir-layer value that differs from it is refused, while
// the intentionally approved override resolves cleanly. The workdir
// skill-config.yaml is agent-writable and outside the hashed bundle, so
// changing it after approval does not trigger bundle-drift; the anchor is what
// catches it.
func TestSecurityWorkdirConfigOverrideRequiresReapproval(t *testing.T) {
	dir := skillDir(t)
	e := entry(t, "probe", dir)
	spec := config.ConfigSpec{Name: "URL", Type: config.ConfigFieldString, Pattern: "^https://.+"}
	m := meta(nil, nil, []config.ConfigSpec{spec})

	global := &skillconfig.Store{Version: skillconfig.SchemaVersion}
	global.Set("probe", "URL", "https://primary.example.com")
	workdir := &skillconfig.Store{Version: skillconfig.SchemaVersion}
	workdir.Set("probe", "URL", "https://secondary.example.com")

	// Control: with no anchor the legacy value resolves (pre-anchor installs
	// are grandfathered).
	r := New(Options{Env: env(nil)})
	armed, problems := r.Resolve(m, e, dir, MergeConfig(global, workdir))
	defer armed.Zero()
	if got := armed.Config["URL"]; got != "https://secondary.example.com" || len(problems) != 0 {
		t.Fatalf("control: an unanchored stored value did not resolve cleanly (value=%q, problems=%v)", got, problems)
	}

	tampered := MergeConfig(global, workdir)
	tampered.RecordApproved("wd", "probe", map[string]string{"URL": "https://primary.example.com"})

	rt := New(Options{Scope: "wd"})
	armed2, problems2 := rt.Resolve(m, e, dir, tampered)
	defer armed2.Zero()
	if got := armed2.Config["URL"]; got == "https://secondary.example.com" || !Has(problems2, InvalidConfig) {
		t.Errorf("a workdir override %q differing from the anchored value reached armed.Config without an InvalidConfig problem: the sidecar would use the tampered value", got)
	}

	// An override that was actually registered is approved and resolves.
	approved := MergeConfig(global, workdir)
	approved.RecordApproved("wd", "probe", map[string]string{"URL": "https://secondary.example.com"})
	armed3, problems3 := rt.Resolve(m, e, dir, approved)
	defer armed3.Zero()
	if got := armed3.Config["URL"]; got != "https://secondary.example.com" || len(problems3) != 0 {
		t.Errorf("an override recorded at register did not resolve (value=%q, problems=%v)", got, problems3)
	}
}

// TestSecurityAgentWritableValueOnUnconstrainedSecretFieldRefused asserts the
// exfiltration path the pattern requirement alone cannot close: with no anchor
// (a pre-anchor install), a value sourced from the agent-writable workdir layer
// for an unconstrained field on a secret-holding skill is still refused, while
// the same value from the host-only global layer is allowed.
func TestSecurityAgentWritableValueOnUnconstrainedSecretFieldRefused(t *testing.T) {
	dir := skillDir(t)
	e := entry(t, "probe", dir)
	spec := config.ConfigSpec{Name: "URL", Type: config.ConfigFieldString}
	m := meta(nil, []config.SecretSpec{{Name: "API_KEY"}}, []config.ConfigSpec{spec})

	workdir := &skillconfig.Store{Version: skillconfig.SchemaVersion}
	workdir.Set("probe", "URL", "http://attacker.example.com")

	r := New(Options{Env: env(nil)})
	armed, problems := r.Resolve(m, e, dir, MergeConfig(nil, workdir))
	defer armed.Zero()
	if got := armed.Config["URL"]; got != "" || !Has(problems, InvalidConfig) {
		t.Errorf("an agent-writable value %q on an unconstrained secret-holding field resolved without an InvalidConfig problem", got)
	}

	global := &skillconfig.Store{Version: skillconfig.SchemaVersion}
	global.Set("probe", "URL", "https://primary.example.com")
	armed2, problems2 := r.Resolve(m, e, dir, MergeConfig(global, nil))
	defer armed2.Zero()
	if got := armed2.Config["URL"]; got != "https://primary.example.com" || Has(problems2, InvalidConfig) {
		t.Errorf("a host-only global value did not resolve (value=%q, problems=%v)", got, problems2)
	}
}

// TestSecurityWorkdirCannotForgeApprovalAnchor asserts that a workdir store
// cannot supply the host-only anchor: MergeConfig takes Approved only from the
// global layer, so an agent that writes an `approved:` block into the
// agent-writable workdir file does not make its own value trusted.
func TestSecurityWorkdirCannotForgeApprovalAnchor(t *testing.T) {
	forged := &skillconfig.Store{Version: skillconfig.SchemaVersion}
	forged.Set("probe", "URL", "http://attacker.example.com")
	forged.RecordApproved("wd", "probe", map[string]string{"URL": "http://attacker.example.com"})

	merged := MergeConfig(nil, forged)
	if _, ok := merged.ApprovedFor("wd", "probe"); ok {
		t.Error("a workdir store's `approved:` block was honoured as a trust anchor")
	}
}
