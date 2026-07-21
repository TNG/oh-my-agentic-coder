package cli

// Tests for the --auto-register-skills path in start.go:
//   - startAutoRegisterWorkdirSkills: silently registers discovered
//     workdir-local skills whose omac.yaml declares no required
//     secrets/fields, leaves skills with required values for the
//     findUnregisteredSkills gate to surface.
//   - skillEligibleForAutoRegister: the eligibility predicate.

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/tngtech/oh-my-agentic-coder/internal/config"
	"github.com/tngtech/oh-my-agentic-coder/internal/registry"
)

// stageSkillWithSidecar creates .opencode/skills/<name>/omac.yaml with
// the given omac.yaml body (a sidecar block). Returns the skill's
// absolute directory.
func stageSkillWithSidecar(t *testing.T, workdir, name, body string) string {
	t.Helper()
	skillDir := filepath.Join(workdir, ".opencode", "skills", name)
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("MkdirAll %s: %v", skillDir, err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "omac.yaml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write omac.yaml: %v", err)
	}
	return skillDir
}

const sidecarNoRequired = `name: no-required
type: skill
sidecar:
  command: ["python3", "x.py"]
  mount: no-required
  secrets:
    - name: OPT_SECRET
      description: "optional"
      required: false
  config:
    - name: OPT_FIELD
      type: string
      description: "optional"
      required: false
`

const sidecarWithRequiredSecret = `name: needs-secret
type: skill
sidecar:
  command: ["python3", "x.py"]
  mount: needs-secret
  secrets:
    - name: MUST_SECRET
      description: "required"
      required: true
`

const sidecarWithRequiredField = `name: needs-field
type: skill
sidecar:
  command: ["python3", "x.py"]
  mount: needs-field
  config:
    - name: MUST_FIELD
      type: string
      description: "required"
      required: true
`

// TestSkillEligibleForAutoRegister_NoSidecarBlock: a directory with a
// bare omac.yaml (no sidecar) is NOT eligible — it would be a skill
// omac can't activate, so we don't auto-register it. The
// findUnregisteredSkills gate surfaces it instead.
func TestSkillEligibleForAutoRegister_NoSidecarBlock(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "omac.yaml"), []byte("name: bare\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := skillEligibleForAutoRegister(dir)
	if err != nil {
		t.Fatalf("skillEligibleForAutoRegister: %v", err)
	}
	if got {
		t.Error("a skill with no sidecar block must not be auto-registered")
	}
}

// TestSkillEligibleForAutoRegister_NoRequiredValues: a skill whose
// secrets and config fields are all optional IS eligible.
func TestSkillEligibleForAutoRegister_NoRequiredValues(t *testing.T) {
	wd := t.TempDir()
	stageSkillWithSidecar(t, wd, "no-required", sidecarNoRequired)
	got, err := skillEligibleForAutoRegister(filepath.Join(wd, ".opencode", "skills", "no-required"))
	if err != nil {
		t.Fatalf("skillEligibleForAutoRegister: %v", err)
	}
	if !got {
		t.Error("a skill with only optional secrets/fields should be eligible")
	}
}

// TestSkillEligibleForAutoRegister_RequiredSecret: a skill with a
// required secret is NOT eligible.
func TestSkillEligibleForAutoRegister_RequiredSecret(t *testing.T) {
	wd := t.TempDir()
	stageSkillWithSidecar(t, wd, "needs-secret", sidecarWithRequiredSecret)
	got, err := skillEligibleForAutoRegister(filepath.Join(wd, ".opencode", "skills", "needs-secret"))
	if err != nil {
		t.Fatalf("skillEligibleForAutoRegister: %v", err)
	}
	if got {
		t.Error("a skill with a required secret must not be auto-registered")
	}
}

// TestSkillEligibleForAutoRegister_RequiredField: a skill with a
// required config field is NOT eligible.
func TestSkillEligibleForAutoRegister_RequiredField(t *testing.T) {
	wd := t.TempDir()
	stageSkillWithSidecar(t, wd, "needs-field", sidecarWithRequiredField)
	got, err := skillEligibleForAutoRegister(filepath.Join(wd, ".opencode", "skills", "needs-field"))
	if err != nil {
		t.Fatalf("skillEligibleForAutoRegister: %v", err)
	}
	if got {
		t.Error("a skill with a required config field must not be auto-registered")
	}
}

// TestStartAutoRegisterWorkdirSkills_OnlyEligible: with the flag set,
// an eligible skill is registered and a skill with a required value is
// left for the gate to surface.
func TestStartAutoRegisterWorkdirSkills_OnlyEligible(t *testing.T) {
	isolateHome(t)
	wd := t.TempDir()
	stageSkillWithSidecar(t, wd, "no-required", sidecarNoRequired)
	stageSkillWithSidecar(t, wd, "needs-secret", sidecarWithRequiredSecret)

	env := makeEnv(wd)
	reg := &registry.Registry{} // empty: nothing registered yet

	done, errs := startAutoRegisterWorkdirSkills(env, config.DefaultHarness(), reg)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	want := []string{"no-required"}
	if !reflect.DeepEqual(done, want) {
		t.Errorf("auto-registered = %v, want %v", done, want)
	}

	// The eligible skill is now in the workdir registry.
	loaded, err := registry.Load(wd)
	if err != nil {
		t.Fatalf("registry.Load: %v", err)
	}
	if e, _ := loaded.Find("no-required"); e == nil {
		t.Error("no-required was not persisted to the registry")
	}
	// The required-secret skill was NOT registered.
	if e, _ := loaded.Find("needs-secret"); e != nil {
		t.Error("needs-secret must not be auto-registered (it has a required secret)")
	}

	// The required-secret skill must still be surfaced by
	// findUnregisteredSkills so the user gets the prompt.
	unreg, err := findUnregisteredSkills(wd, config.DefaultHarness(), loaded)
	if err != nil {
		t.Fatalf("findUnregisteredSkills: %v", err)
	}
	if want := []string{"needs-secret"}; !reflect.DeepEqual(unreg, want) {
		t.Errorf("unregistered after auto-register = %v, want %v", unreg, want)
	}
}

// TestStartAutoRegisterWorkdirSkills_SkipsAlreadyRegistered: an
// already-registered skill is not re-registered.
func TestStartAutoRegisterWorkdirSkills_SkipsAlreadyRegistered(t *testing.T) {
	isolateHome(t)
	wd := t.TempDir()
	skillDir := stageSkillWithSidecar(t, wd, "no-required", sidecarNoRequired)

	bundle := mustBundleHash(t, skillDir)
	reg := &registry.Registry{Registered: []registry.Entry{{
		Name:       "no-required",
		SkillDir:   filepath.Join(".opencode", "skills", "no-required"),
		BundleHash: bundle,
	}}}
	env := makeEnv(wd)

	done, errs := startAutoRegisterWorkdirSkills(env, config.DefaultHarness(), reg)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(done) != 0 {
		t.Errorf("already-registered skill was re-registered: %v", done)
	}
}

// TestStartAutoRegisterWorkdirSkills_SkipsUserGlobal: user-global
// skills are NOT auto-registered by the start path (they're registered
// once, globally, via `omac register --global`); only workdir-local
// skills are in scope.
func TestStartAutoRegisterWorkdirSkills_SkipsUserGlobal(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))

	wd := t.TempDir()
	// Stage a user-global skill with an eligible sidecar.
	globalDir := filepath.Join(home, ".config", "opencode", "skills", "global-eligible")
	if err := os.MkdirAll(globalDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(globalDir, "omac.yaml"), []byte(sidecarNoRequired), 0o644); err != nil {
		t.Fatal(err)
	}

	env := makeEnv(wd)
	done, errs := startAutoRegisterWorkdirSkills(env, config.DefaultHarness(), &registry.Registry{})
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(done) != 0 {
		t.Errorf("user-global skill must not be auto-registered by `omac start`: %v", done)
	}
}

func mustBundleHash(t *testing.T, dir string) string {
	t.Helper()
	h, err := config.BundleHash(dir)
	if err != nil {
		t.Fatalf("BundleHash: %v", err)
	}
	return h
}
