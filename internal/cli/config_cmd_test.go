package cli

// Tests for omac config's own presentation: the placeholder markers, the type
// projection, the source columns, and secretFingerprint (the on-the-wire
// format). The precedence ladder itself now lives in internal/skillstate and is
// tested there — this command only renders its result, which is the point of
// issue #174: what `config show` DISPLAYS is by construction what start USES.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tngtech/oh-my-agentic-coder/internal/keychain"
	"github.com/tngtech/oh-my-agentic-coder/internal/secrets"
	"github.com/tngtech/oh-my-agentic-coder/internal/skillstate"
)

// stageSkillForConfigShow writes a skill declaring one config field sourced
// from $default_from_env and one required secret that is ALSO listed under
// env_passthrough — the two shapes issue #174 found mishandled — then registers
// it workdir-local.
func stageSkillForConfigShow(t *testing.T, env *Env, name string) {
	t.Helper()
	dir := filepath.Join(env.Workdir, ".opencode", "skills", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := "name: " + name + "\n" +
		"sidecar:\n" +
		"  command: [\"true\"]\n" +
		"  env_passthrough:\n" +
		"    - API_TOKEN\n" +
		"  secrets:\n" +
		"    - name: API_TOKEN\n" +
		"      required: true\n" +
		"  config:\n" +
		"    - name: API_BASE\n" +
		"      default_from_env: OMAC_TEST_API_BASE\n" +
		"    - name: OPTIONAL_F\n" +
		"      required: false\n"
	if err := os.WriteFile(filepath.Join(dir, "omac.yaml"), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := runRegister([]string{name, "--no-secrets", "--no-fields"}, env); code != ExitOK {
		t.Fatalf("register exit=%d", code)
	}
}

// TestConfigShowSourcesFromEnvPassthroughAndDefaultFromEnv covers both
// divergences this command had: a secret satisfied from the host env was shown
// as "<missing>" even though start accepts it and the sidecar receives it, and
// a $default_from_env field's origin is now named in the source column.
func TestConfigShowSourcesFromEnvPassthroughAndDefaultFromEnv(t *testing.T) {
	isolateHome(t)
	env := makeEnv(t.TempDir())
	stageSkillForConfigShow(t, env, "probe")
	t.Setenv("OMAC_TEST_API_BASE", "https://api.example")
	t.Setenv("API_TOKEN", "shell-supplied")

	view, code := buildSkillView(env, "probe")
	if code != ExitOK {
		t.Fatalf("buildSkillView exit=%d", code)
	}
	defer view.zero()

	cfg := map[string]fieldView{}
	for _, f := range view.Config {
		cfg[f.Name] = f
	}
	if got := cfg["API_BASE"]; got.Value != "https://api.example" ||
		got.Source != "default_from_env:OMAC_TEST_API_BASE" {
		t.Errorf("API_BASE = %q/%q, want the env value and its source", got.Value, got.Source)
	}
	// Type projects the EffectiveType, so an unspecified type reads "string".
	if got := cfg["API_BASE"].Type; got != "string" {
		t.Errorf("Type = %q, want string", got)
	}
	if got := cfg["OPTIONAL_F"]; got.Value != "<missing-optional>" || got.Source != "missing-optional" ||
		got.Required {
		t.Errorf("OPTIONAL_F = %+v, want the optional marker", got)
	}

	if len(view.Secrets) != 1 {
		t.Fatalf("secrets = %v, want one", view.Secrets)
	}
	s := view.Secrets[0]
	if s.Source != string(skillstate.SourceEnvPassthrough) {
		t.Errorf("source = %q, want env_passthrough", s.Source)
	}
	if s.Fingerprint != secretFingerprint("shell-supplied") {
		t.Errorf("fingerprint = %q, want the env-supplied value's fingerprint (not <missing>)", s.Fingerprint)
	}
}

// TestConfigShowFindsWorkdirScopedSecret is issue #174's Failure 3 for this
// command: it read secrets UNSCOPED while start reads them workdir-scoped, so a
// secret stored per-workdir showed as missing here while start launched fine.
func TestConfigShowFindsWorkdirScopedSecret(t *testing.T) {
	isolateHome(t)
	env := makeEnv(t.TempDir())
	stageSkillForConfigShow(t, env, "probe")
	t.Setenv("OMAC_TEST_API_BASE", "https://api.example")

	scope := keychain.WorkdirID(env.Workdir)
	if err := keychain.SetScoped(scope, "probe", "API_TOKEN", secrets.NewSecretString("scoped-value")); err != nil {
		t.Fatalf("SetScoped: %v", err)
	}

	view, code := buildSkillView(env, "probe")
	if code != ExitOK {
		t.Fatalf("buildSkillView exit=%d", code)
	}
	defer view.zero()

	s := view.Secrets[0]
	if s.Source != string(skillstate.SourceKeychain) {
		t.Errorf("source = %q, want keychain", s.Source)
	}
	if s.Fingerprint != secretFingerprint("scoped-value") {
		t.Errorf("fingerprint = %q, want the workdir-scoped value's", s.Fingerprint)
	}
}

// TestConfigShowMarksMissingRequired: with neither a keychain entry nor the
// env var, the markers appear and `omac config get` refuses (so a $(...)
// substitution never captures a placeholder).
func TestConfigShowMarksMissingRequired(t *testing.T) {
	isolateHome(t)
	env := makeEnv(t.TempDir())
	stageSkillForConfigShow(t, env, "probe")

	view, code := buildSkillView(env, "probe")
	if code != ExitOK {
		t.Fatalf("buildSkillView exit=%d", code)
	}
	defer view.zero()

	for _, f := range view.Config {
		if f.Name != "API_BASE" {
			continue
		}
		if f.Value != "<missing-required>" || f.Source != "missing-required" {
			t.Errorf("API_BASE = %q/%q, want the required marker", f.Value, f.Source)
		}
	}
	if view.Secrets[0].Fingerprint != "<missing>" {
		t.Errorf("fingerprint = %q, want <missing>", view.Secrets[0].Fingerprint)
	}
	if code := runConfigGet([]string{"probe", "API_BASE"}, env); code != ExitConfigInvalid {
		t.Errorf("config get on a missing field: code=%d, want %d", code, ExitConfigInvalid)
	}
}

func TestSecretFingerprint_Format(t *testing.T) {
	// sha256 of "hello world" is b94d27b9934d3e... -> first 12 hex chars.
	got := secretFingerprint("hello world")
	const want = "sha256:b94d27b9934d"
	if got != want {
		t.Errorf("secretFingerprint(\"hello world\") = %q, want %q", got, want)
	}
}

func TestSecretFingerprint_Empty(t *testing.T) {
	if got := secretFingerprint(""); got != "<absent>" {
		t.Errorf("empty input should yield <absent>, got %q", got)
	}
}

func TestSecretFingerprint_DifferentInputsDiffer(t *testing.T) {
	a := secretFingerprint("alpha")
	b := secretFingerprint("bravo")
	if a == b {
		t.Errorf("fingerprints should differ for different inputs (%q == %q)", a, b)
	}
	if !strings.HasPrefix(a, "sha256:") || !strings.HasPrefix(b, "sha256:") {
		t.Error("fingerprints must carry sha256: prefix")
	}
}

// TestRunConfigShow_UnregisteredSkill is a regression test for a nil-
// pointer panic: buildSkillView returns (nil, code) on the unknown-
// skill branch, and the original implementation deferred view.zero()
// before the error check. Success means the command exits with the
// right code without segfaulting; we don't bother capturing stderr.
func TestRunConfigShow_UnregisteredSkill(t *testing.T) {
	dir := t.TempDir() // no .opencode/sidecar.json => zero registered skills
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open /dev/null: %v", err)
	}
	t.Cleanup(func() { null.Close() })
	env := &Env{Workdir: dir, Stdout: null, Stderr: null, Stdin: null, Version: "test"}

	if code := runConfigShow([]string{"nonexistent"}, env); code != ExitPrerequisiteMissing {
		t.Fatalf("runConfigShow on unknown skill: code=%d, want %d", code, ExitPrerequisiteMissing)
	}
}

func TestRunConfigGet_UnregisteredSkill(t *testing.T) {
	dir := t.TempDir()
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open /dev/null: %v", err)
	}
	t.Cleanup(func() { null.Close() })
	env := &Env{Workdir: dir, Stdout: null, Stderr: null, Stdin: null, Version: "test"}

	if code := runConfigGet([]string{"nonexistent", "FIELD"}, env); code != ExitPrerequisiteMissing {
		t.Fatalf("runConfigGet on unknown skill: code=%d, want %d", code, ExitPrerequisiteMissing)
	}
}

// TestSecretFingerprint_MatchesSidecar pins the byte-for-byte format
// the echo-rest reference sidecar (sidecar.py) uses, so `omac config
// show` and the sidecar's /whoami response print the same string for
// the same secret. If you ever change the algorithm here, change it
// in sidecar.py's fingerprint() too (and bump a major version, since
// users rely on cross-checking these two values).
func TestSecretFingerprint_MatchesSidecar(t *testing.T) {
	// Sidecar formula: "sha256:" + hashlib.sha256(s).hexdigest()[:12]
	// With s = "abc" => sha256 = ba7816bf8f01cfea... => prefix "ba7816bf8f01"
	got := secretFingerprint("abc")
	const want = "sha256:ba7816bf8f01"
	if got != want {
		t.Errorf("fingerprint(%q) = %q, want %q (must match sidecar.py)", "abc", got, want)
	}
}
