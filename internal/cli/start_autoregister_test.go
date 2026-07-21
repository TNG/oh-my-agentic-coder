package cli

// Tests for the --auto-register-skills path in start.go:
//   - startAutoRegisterWorkdirSkills: silently registers discovered
//     workdir-local skills whose required values resolve at launch time,
//     leaves skills with missing required values for the
//     findUnregisteredSkills gate to surface.
//   - skillEligibleForAutoRegister: the eligibility predicate.

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/tngtech/oh-my-agentic-coder/internal/config"
	"github.com/tngtech/oh-my-agentic-coder/internal/keychain"
	"github.com/tngtech/oh-my-agentic-coder/internal/registry"
	"github.com/tngtech/oh-my-agentic-coder/internal/secrets"
	"github.com/tngtech/oh-my-agentic-coder/internal/skillconfig"
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

func autoRegisterConfig(t *testing.T, workdir string) *skillconfig.Store {
	t.Helper()
	workdirStore, err := skillconfig.Load(workdir)
	if err != nil {
		t.Fatalf("skillconfig.Load: %v", err)
	}
	globalStore, err := skillconfig.LoadGlobal()
	if err != nil {
		t.Fatalf("skillconfig.LoadGlobal: %v", err)
	}
	return mergeConfig(globalStore, workdirStore)
}

func autoRegisterEligible(t *testing.T, workdir, skillName, skillDir string, skipSecretPattern bool) (bool, error) {
	t.Helper()
	meta, err := config.LoadMeta(filepath.Join(skillDir, config.MetaFileName))
	if err != nil {
		return false, err
	}
	return skillEligibleForAutoRegister(workdir, skillName, meta, autoRegisterConfig(t, workdir), skipSecretPattern)
}

func runAutoRegisterWorkdirSkills(t *testing.T, env *Env, harness config.Harness, reg *registry.Registry, skipSecretPattern bool) ([]string, []string) {
	t.Helper()
	done, diagnostics, err := startAutoRegisterWorkdirSkills(env, harness, reg, autoRegisterConfig(t, env.Workdir), skipSecretPattern)
	if err != nil {
		t.Fatalf("startAutoRegisterWorkdirSkills: %v", err)
	}
	return done, diagnostics
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

// sidecarWithRequiredFieldButDefault mirrors the echo-rest shape: a
// config field with NO explicit `required:` (so IsRequired()==true) but
// a non-empty `default:`. At start time the default fills the value
// without user input, so this skill IS eligible for auto-registration.
const sidecarWithRequiredFieldButDefault = `name: has-default
type: skill
sidecar:
  command: ["python3", "x.py"]
  mount: has-default
  config:
    - name: FIELD_WITH_DEFAULT
      type: string
      description: "no explicit required, but has a default"
      default: "hello"
`

const sidecarWithRequiredFieldDefaultFromEnv = `name: has-default-from-env
type: skill
sidecar:
  command: ["python3", "x.py"]
  mount: has-default-from-env
  config:
    - name: FIELD_WITH_DEFAULT_FROM_ENV
      type: string
      description: "no explicit required, but has a default_from_env"
      default_from_env: "SOME_ENV_VAR"
`

// sidecarWithSecretInEnvPassthrough: a required secret that is also
// listed in env_passthrough is satisfiable at runtime from the host
// env (start.go resolves env_passthrough secrets without keychain),
// so the skill IS eligible for auto-registration.
const sidecarWithSecretInEnvPassthrough = `name: passthrough-secret
type: skill
sidecar:
  command: ["python3", "x.py"]
  mount: passthrough-secret
  env_passthrough:
    - MUST_SECRET
  secrets:
    - name: MUST_SECRET
      description: "required but supplied via env_passthrough"
      required: true
`

const sidecarWithPatternedSecretInEnvPassthrough = `name: patterned-passthrough-secret
type: skill
sidecar:
  command: ["python3", "x.py"]
  mount: patterned-passthrough-secret
  env_passthrough:
    - MUST_SECRET
  secrets:
    - name: MUST_SECRET
      description: "required token supplied via env_passthrough"
      pattern: "^valid-[a-z]+$"
      required: true
`

func TestParseLaunchArgs_AutoRegisterSkills(t *testing.T) {
	opts, ok := parseLaunchArgs("start", []string{"--auto-register-skills"}, devnullEnv(t))
	if !ok || !opts.autoRegisterSkills {
		t.Fatal("--auto-register-skills was not parsed")
	}
}

func TestRunStart_AutoRegistersEligibleWorkdirSkill(t *testing.T) {
	isolateHome(t)
	wd := t.TempDir()
	stageSkillWithSidecar(t, wd, "eligible", `name: eligible
type: skill
sidecar:
  command: ["true"]
  mount: eligible
`)

	env, _ := launchTestEnv(t, wd)
	runStart([]string{"--auto-register-skills", "--no-sandbox", "--inner", "/bin/true"}, env)

	loaded, err := registry.Load(wd)
	if err != nil {
		t.Fatalf("registry.Load: %v", err)
	}
	if entry, _ := loaded.Find("eligible"); entry == nil {
		t.Error("eligible was not persisted to the registry")
	}
}

func TestRunStart_AutoRegisterCorruptWorkdirConfigFailsIO(t *testing.T) {
	isolateHome(t)
	wd := t.TempDir()
	stageSkillWithSidecar(t, wd, "eligible", `name: eligible
type: skill
sidecar:
  command: ["true"]
  mount: eligible
`)
	if err := os.WriteFile(skillconfig.Path(wd), []byte("skills: ["), 0o600); err != nil {
		t.Fatalf("write corrupt skill config: %v", err)
	}

	env, stderr := launchTestEnv(t, wd)
	code := runStart([]string{"--auto-register-skills", "--no-sandbox", "--inner", "/bin/true"}, env)
	if code != ExitIOError {
		t.Fatalf("runStart() = %d, want ExitIOError (%d)\nstderr:\n%s", code, ExitIOError, stderr())
	}
	if output := stderr(); strings.Contains(output, "unregistered skills") || strings.Contains(output, "register with:") {
		t.Errorf("runStart() reported registration guidance after config load failure:\n%s", output)
	}
}

func TestRunStart_AutoRegistersEligibleBeforeRequiredSkillGate(t *testing.T) {
	isolateHome(t)
	wd := t.TempDir()
	stageSkillWithSidecar(t, wd, "no-required", sidecarNoRequired)
	stageSkillWithSidecar(t, wd, "needs-secret", sidecarWithRequiredSecret)

	env, _ := launchTestEnv(t, wd)
	if code := runStart([]string{"--auto-register-skills", "--no-sandbox", "--inner", "/bin/true"}, env); code != ExitPrerequisiteMissing {
		t.Fatalf("runStart() = %d, want ExitPrerequisiteMissing (%d)", code, ExitPrerequisiteMissing)
	}

	loaded, err := registry.Load(wd)
	if err != nil {
		t.Fatalf("registry.Load: %v", err)
	}
	if entry, _ := loaded.Find("no-required"); entry == nil {
		t.Error("no-required was not persisted to the registry")
	}
	if entry, _ := loaded.Find("needs-secret"); entry != nil {
		t.Error("needs-secret must remain unregistered")
	}
}

// TestSkillEligibleForAutoRegister_NoSidecarBlock: a directory with a
// bare omac.yaml (no sidecar) is NOT eligible — it would be a skill
// omac can't activate, so we don't auto-register it. The
// findUnregisteredSkills gate surfaces it instead.
func TestSkillEligibleForAutoRegister_NoSidecarBlock(t *testing.T) {
	isolateHome(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "omac.yaml"), []byte("name: bare\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := autoRegisterEligible(t, dir, "bare", dir, false)
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
	isolateHome(t)
	wd := t.TempDir()
	stageSkillWithSidecar(t, wd, "no-required", sidecarNoRequired)
	got, err := autoRegisterEligible(t, wd, "no-required", filepath.Join(wd, ".opencode", "skills", "no-required"), false)
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
	isolateHome(t)
	wd := t.TempDir()
	stageSkillWithSidecar(t, wd, "needs-secret", sidecarWithRequiredSecret)
	got, err := autoRegisterEligible(t, wd, "needs-secret", filepath.Join(wd, ".opencode", "skills", "needs-secret"), false)
	if err != nil {
		t.Fatalf("skillEligibleForAutoRegister: %v", err)
	}
	if got {
		t.Error("a skill with a required secret must not be auto-registered")
	}
}

// TestSkillEligibleForAutoRegister_RequiredField: a skill with a
// required config field (no default) is NOT eligible.
func TestSkillEligibleForAutoRegister_RequiredField(t *testing.T) {
	isolateHome(t)
	wd := t.TempDir()
	stageSkillWithSidecar(t, wd, "needs-field", sidecarWithRequiredField)
	got, err := autoRegisterEligible(t, wd, "needs-field", filepath.Join(wd, ".opencode", "skills", "needs-field"), false)
	if err != nil {
		t.Fatalf("skillEligibleForAutoRegister: %v", err)
	}
	if got {
		t.Error("a skill with a required config field (no default) must not be auto-registered")
	}
}

// TestSkillEligibleForAutoRegister_RequiredFieldWithDefault: a skill
// whose config field is implicitly required (no `required:` key) but
// has a non-empty `default:` IS eligible. A `default_from_env:` is
// eligible only when its named environment variable is non-empty.
func TestSkillEligibleForAutoRegister_RequiredFieldWithDefault(t *testing.T) {
	isolateHome(t)
	wd := t.TempDir()
	stageSkillWithSidecar(t, wd, "has-default", sidecarWithRequiredFieldButDefault)
	got, err := autoRegisterEligible(t, wd, "has-default", filepath.Join(wd, ".opencode", "skills", "has-default"), false)
	if err != nil {
		t.Fatalf("skillEligibleForAutoRegister: %v", err)
	}
	if !got {
		t.Error("a skill whose required fields all have defaults should be eligible (echo-rest shape)")
	}
}

// TestSkillEligibleForAutoRegister_RequiredFieldWithDefaultFromEnvUnset
// verifies that default_from_env only satisfies a required field when the
// named environment variable is non-empty.
func TestSkillEligibleForAutoRegister_RequiredFieldWithDefaultFromEnvUnset(t *testing.T) {
	isolateHome(t)
	t.Setenv("SOME_ENV_VAR", "")
	wd := t.TempDir()
	stageSkillWithSidecar(t, wd, "has-default-from-env", sidecarWithRequiredFieldDefaultFromEnv)
	got, err := autoRegisterEligible(t, wd, "has-default-from-env", filepath.Join(wd, ".opencode", "skills", "has-default-from-env"), false)
	if err != nil {
		t.Fatalf("skillEligibleForAutoRegister: %v", err)
	}
	if got {
		t.Error("a skill with an unset required default_from_env must not be auto-registered")
	}
}

func TestSkillEligibleForAutoRegister_RequiredFieldWithDefaultFromEnv(t *testing.T) {
	isolateHome(t)
	t.Setenv("SOME_ENV_VAR", "configured")
	wd := t.TempDir()
	stageSkillWithSidecar(t, wd, "has-default-from-env", sidecarWithRequiredFieldDefaultFromEnv)
	got, err := autoRegisterEligible(t, wd, "has-default-from-env", filepath.Join(wd, ".opencode", "skills", "has-default-from-env"), false)
	if err != nil {
		t.Fatalf("skillEligibleForAutoRegister: %v", err)
	}
	if !got {
		t.Error("a skill with a configured required default_from_env should be eligible")
	}
}

// TestSkillEligibleForAutoRegister_SecretInEnvPassthrough: a required
// secret that is in env_passthrough is satisfiable from the host env
// at runtime, so the skill IS eligible.
func TestSkillEligibleForAutoRegister_SecretInEnvPassthrough(t *testing.T) {
	isolateHome(t)
	t.Setenv("MUST_SECRET", "non-empty-test-secret")
	wd := t.TempDir()
	stageSkillWithSidecar(t, wd, "passthrough-secret", sidecarWithSecretInEnvPassthrough)
	got, err := autoRegisterEligible(t, wd, "passthrough-secret", filepath.Join(wd, ".opencode", "skills", "passthrough-secret"), false)
	if err != nil {
		t.Fatalf("skillEligibleForAutoRegister: %v", err)
	}
	if !got {
		t.Error("a skill whose required secret is in env_passthrough should be eligible")
	}
}

// TestStartAutoRegisterWorkdirSkills_RequiresNonEmptyPassthroughSecret
// verifies that a passthrough secret only makes a skill eligible when the
// environment supplies a non-empty value.
func TestStartAutoRegisterWorkdirSkills_RequiresNonEmptyPassthroughSecret(t *testing.T) {
	isolateHome(t)
	t.Setenv("MUST_SECRET", "")
	wd := t.TempDir()
	stageSkillWithSidecar(t, wd, "passthrough-secret", sidecarWithSecretInEnvPassthrough)

	env := makeEnv(wd)
	done, errs := runAutoRegisterWorkdirSkills(t, env, config.DefaultHarness(), &registry.Registry{}, false)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(done) != 0 {
		t.Errorf("auto-registered = %v, want []", done)
	}

	loaded, err := registry.Load(wd)
	if err != nil {
		t.Fatalf("registry.Load: %v", err)
	}
	if e, _ := loaded.Find("passthrough-secret"); e != nil {
		t.Error("passthrough-secret must not be persisted without MUST_SECRET")
	}

	unreg, err := findUnregisteredSkills(wd, config.DefaultHarness(), loaded)
	if err != nil {
		t.Fatalf("findUnregisteredSkills: %v", err)
	}
	if want := []string{"passthrough-secret"}; !reflect.DeepEqual(unreg, want) {
		t.Errorf("unregistered after auto-register = %v, want %v", unreg, want)
	}
}

// TestStartAutoRegisterWorkdirSkills_RejectsInvalidPassthroughSecretPattern
// verifies auto-registration uses the same pattern check as launch before it
// accepts a required env_passthrough secret as satisfying the skill.
func TestStartAutoRegisterWorkdirSkills_RejectsInvalidPassthroughSecretPattern(t *testing.T) {
	isolateHome(t)
	t.Setenv("MUST_SECRET", "invalid-token")
	wd := t.TempDir()
	skillDir := stageSkillWithSidecar(t, wd, "patterned-passthrough-secret", sidecarWithPatternedSecretInEnvPassthrough)

	eligible, err := autoRegisterEligible(t, wd, "patterned-passthrough-secret", skillDir, false)
	if err != nil {
		t.Fatalf("skillEligibleForAutoRegister: %v", err)
	}
	if eligible {
		t.Error("a required passthrough secret that violates its pattern must not be eligible")
	}

	done, errs := runAutoRegisterWorkdirSkills(t, makeEnv(wd), config.DefaultHarness(), &registry.Registry{}, false)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(done) != 0 {
		t.Errorf("auto-registered = %v, want []", done)
	}

	loaded, err := registry.Load(wd)
	if err != nil {
		t.Fatalf("registry.Load: %v", err)
	}
	if entry, _ := loaded.Find("patterned-passthrough-secret"); entry != nil {
		t.Error("an invalid passthrough secret must not be persisted to the registry")
	}

	unregistered, err := findUnregisteredSkills(wd, config.DefaultHarness(), loaded)
	if err != nil {
		t.Fatalf("findUnregisteredSkills: %v", err)
	}
	if want := []string{"patterned-passthrough-secret"}; !reflect.DeepEqual(unregistered, want) {
		t.Errorf("unregistered after auto-register = %v, want %v", unregistered, want)
	}
}

func TestStartAutoRegisterWorkdirSkills_KeychainSecretBeatsInvalidPassthroughValue(t *testing.T) {
	isolateHome(t)
	t.Setenv("MUST_SECRET", "invalid-token")
	wd := t.TempDir()
	stageSkillWithSidecar(t, wd, "patterned-passthrough-secret", sidecarWithPatternedSecretInEnvPassthrough)

	scope := keychain.WorkdirID(wd)
	value := secrets.NewSecretString("valid-keychain")
	defer value.Zero()
	if err := keychain.SetScoped(scope, "patterned-passthrough-secret", "MUST_SECRET", value); err != nil {
		t.Fatalf("keychain.SetScoped: %v", err)
	}
	t.Cleanup(func() {
		if err := keychain.DeleteScoped(scope, "patterned-passthrough-secret", "MUST_SECRET"); err != nil {
			t.Errorf("keychain.DeleteScoped: %v", err)
		}
	})

	done, errs := runAutoRegisterWorkdirSkills(t, makeEnv(wd), config.DefaultHarness(), &registry.Registry{}, false)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if want := []string{"patterned-passthrough-secret"}; !reflect.DeepEqual(done, want) {
		t.Errorf("auto-registered = %v, want %v", done, want)
	}

	loaded, err := registry.Load(wd)
	if err != nil {
		t.Fatalf("registry.Load: %v", err)
	}
	if entry, _ := loaded.Find("patterned-passthrough-secret"); entry == nil {
		t.Error("patterned-passthrough-secret was not persisted from its keychain secret")
	}
}

func TestStartAutoRegisterWorkdirSkills_UnscopedKeychainSecretBeatsInvalidPassthroughValue(t *testing.T) {
	isolateHome(t)
	t.Setenv("MUST_SECRET", "invalid-token")
	wd := t.TempDir()
	stageSkillWithSidecar(t, wd, "patterned-passthrough-secret", sidecarWithPatternedSecretInEnvPassthrough)

	value := secrets.NewSecretString("valid-keychain")
	defer value.Zero()
	if err := keychain.Set("patterned-passthrough-secret", "MUST_SECRET", value); err != nil {
		t.Fatalf("keychain.Set: %v", err)
	}
	t.Cleanup(func() {
		if err := keychain.Delete("patterned-passthrough-secret", "MUST_SECRET"); err != nil {
			t.Errorf("keychain.Delete: %v", err)
		}
	})

	done, errs := runAutoRegisterWorkdirSkills(t, makeEnv(wd), config.DefaultHarness(), &registry.Registry{}, false)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if want := []string{"patterned-passthrough-secret"}; !reflect.DeepEqual(done, want) {
		t.Errorf("auto-registered = %v, want %v", done, want)
	}
}

func TestStartAutoRegisterWorkdirSkills_StoredConfigSatisfiesRequiredField(t *testing.T) {
	for _, tc := range []struct {
		name string
		save func(*testing.T, string)
	}{
		{
			name: "workdir",
			save: func(t *testing.T, workdir string) {
				t.Helper()
				store, err := skillconfig.Load(workdir)
				if err != nil {
					t.Fatalf("skillconfig.Load: %v", err)
				}
				store.Set("needs-field", "MUST_FIELD", "from-workdir")
				if err := skillconfig.Save(workdir, store); err != nil {
					t.Fatalf("skillconfig.Save: %v", err)
				}
			},
		},
		{
			name: "global",
			save: func(t *testing.T, _ string) {
				t.Helper()
				store, err := skillconfig.LoadGlobal()
				if err != nil {
					t.Fatalf("skillconfig.LoadGlobal: %v", err)
				}
				store.Set("needs-field", "MUST_FIELD", "from-global")
				if err := skillconfig.SaveGlobal(store); err != nil {
					t.Fatalf("skillconfig.SaveGlobal: %v", err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateHome(t)
			wd := t.TempDir()
			stageSkillWithSidecar(t, wd, "needs-field", sidecarWithRequiredField)
			tc.save(t, wd)

			done, errs := runAutoRegisterWorkdirSkills(t, makeEnv(wd), config.DefaultHarness(), &registry.Registry{}, false)
			if len(errs) != 0 {
				t.Fatalf("unexpected errors: %v", errs)
			}
			if want := []string{"needs-field"}; !reflect.DeepEqual(done, want) {
				t.Errorf("auto-registered = %v, want %v", done, want)
			}

			loaded, err := registry.Load(wd)
			if err != nil {
				t.Fatalf("registry.Load: %v", err)
			}
			if entry, _ := loaded.Find("needs-field"); entry == nil {
				t.Error("needs-field was not persisted from its stored config value")
			}
		})
	}
}

func TestStartAutoRegisterWorkdirSkills_InvalidPassthroughSecretRequiresSkipPattern(t *testing.T) {
	isolateHome(t)
	t.Setenv("MUST_SECRET", "invalid-token")
	wd := t.TempDir()
	stageSkillWithSidecar(t, wd, "patterned-passthrough-secret", sidecarWithPatternedSecretInEnvPassthrough)

	env := makeEnv(wd)
	withoutSkip, errs := runAutoRegisterWorkdirSkills(t, env, config.DefaultHarness(), &registry.Registry{}, false)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors without --skip-secret-pattern: %v", errs)
	}
	if len(withoutSkip) != 0 {
		t.Errorf("auto-registered without --skip-secret-pattern = %v, want []", withoutSkip)
	}

	loaded, err := registry.Load(wd)
	if err != nil {
		t.Fatalf("registry.Load: %v", err)
	}
	if entry, _ := loaded.Find("patterned-passthrough-secret"); entry != nil {
		t.Error("patterned-passthrough-secret must not persist without --skip-secret-pattern")
	}

	withSkip, errs := runAutoRegisterWorkdirSkills(t, env, config.DefaultHarness(), &registry.Registry{}, true)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors with --skip-secret-pattern: %v", errs)
	}
	if want := []string{"patterned-passthrough-secret"}; !reflect.DeepEqual(withSkip, want) {
		t.Errorf("auto-registered with --skip-secret-pattern = %v, want %v", withSkip, want)
	}

	loaded, err = registry.Load(wd)
	if err != nil {
		t.Fatalf("registry.Load: %v", err)
	}
	if entry, _ := loaded.Find("patterned-passthrough-secret"); entry == nil {
		t.Error("patterned-passthrough-secret was not persisted with --skip-secret-pattern")
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

	done, errs := runAutoRegisterWorkdirSkills(t, env, config.DefaultHarness(), reg, false)
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

	done, errs := runAutoRegisterWorkdirSkills(t, env, config.DefaultHarness(), reg, false)
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
	done, errs := runAutoRegisterWorkdirSkills(t, env, config.DefaultHarness(), &registry.Registry{}, false)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(done) != 0 {
		t.Errorf("user-global skill must not be auto-registered by `omac start`: %v", done)
	}
}

// TestStartAutoRegisterWorkdirSkills_RequiredFieldWithDefault: the
// echo-rest shape — a config field with no explicit `required:` (so
// IsRequired()==true) but a `default:` — must be auto-registered,
// not surfaced by the gate. This is the regression that bit the user
// (echo-rest was refused even though all its fields had defaults).
func TestStartAutoRegisterWorkdirSkills_RequiredFieldWithDefault(t *testing.T) {
	isolateHome(t)
	wd := t.TempDir()
	stageSkillWithSidecar(t, wd, "has-default", sidecarWithRequiredFieldButDefault)

	env := makeEnv(wd)
	reg := &registry.Registry{}

	done, errs := runAutoRegisterWorkdirSkills(t, env, config.DefaultHarness(), reg, false)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	want := []string{"has-default"}
	if !reflect.DeepEqual(done, want) {
		t.Errorf("auto-registered = %v, want %v", done, want)
	}

	loaded, err := registry.Load(wd)
	if err != nil {
		t.Fatalf("registry.Load: %v", err)
	}
	if e, _ := loaded.Find("has-default"); e == nil {
		t.Error("has-default was not persisted to the registry")
	}

	// Nothing should be left for the gate to surface.
	unreg, err := findUnregisteredSkills(wd, config.DefaultHarness(), loaded)
	if err != nil {
		t.Fatalf("findUnregisteredSkills: %v", err)
	}
	if len(unreg) != 0 {
		t.Errorf("unregistered after auto-register = %v, want []", unreg)
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
