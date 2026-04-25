package cli

// Tests for the pure pieces of omac config: resolveFieldView (the
// precedence ladder) and secretFingerprint (the on-the-wire format).
// The end-to-end runConfig path is exercised in the smoke test in the
// commit message; trying to test it here would require building a
// working keychain mock and is more value than this trivial command
// merits.

import (
	"os"
	"strings"
	"testing"

	"github.com/tngtech/oh-my-agentic-coder/internal/config"
	"github.com/tngtech/oh-my-agentic-coder/internal/skillconfig"
)

func TestResolveFieldView_StoredWins(t *testing.T) {
	store := &skillconfig.Store{}
	store.Set("s", "F", "from-store")

	v := resolveFieldView(
		config.ConfigSpec{
			Name: "F", Default: "from-default", DefaultFromEnv: "DOES_NOT_MATTER",
		},
		store, "s",
	)
	if v.Value != "from-store" || v.Source != "stored" {
		t.Errorf("got value=%q source=%q; want from-store/stored", v.Value, v.Source)
	}
}

func TestResolveFieldView_DefaultBeatsEnv(t *testing.T) {
	t.Setenv("OMAC_TEST_VAR", "from-env")
	v := resolveFieldView(
		config.ConfigSpec{
			Name: "F", Default: "from-default", DefaultFromEnv: "OMAC_TEST_VAR",
		},
		&skillconfig.Store{}, "s",
	)
	// Spec.Default is checked BEFORE default_from_env, even when both
	// are set. Authors who want env-only behavior should leave Default
	// empty.
	if v.Value != "from-default" || v.Source != "default" {
		t.Errorf("got value=%q source=%q; want from-default/default", v.Value, v.Source)
	}
}

func TestResolveFieldView_DefaultFromEnv(t *testing.T) {
	t.Setenv("OMAC_TEST_VAR", "from-env")
	v := resolveFieldView(
		config.ConfigSpec{Name: "F", DefaultFromEnv: "OMAC_TEST_VAR"},
		&skillconfig.Store{}, "s",
	)
	if v.Value != "from-env" || v.Source != "default_from_env:OMAC_TEST_VAR" {
		t.Errorf("got value=%q source=%q; want from-env/default_from_env:OMAC_TEST_VAR", v.Value, v.Source)
	}
}

func TestResolveFieldView_DefaultFromEnv_Unset(t *testing.T) {
	// DefaultFromEnv set, but the env var doesn't exist in this process.
	// Required field with no other source falls through to missing-required.
	v := resolveFieldView(
		config.ConfigSpec{Name: "F", DefaultFromEnv: "OMAC_DEFINITELY_NOT_SET_42"},
		&skillconfig.Store{}, "s",
	)
	if v.Value != "<missing-required>" || v.Source != "missing-required" {
		t.Errorf("got value=%q source=%q; want <missing-required>/missing-required", v.Value, v.Source)
	}
}

func TestResolveFieldView_OptionalMissing(t *testing.T) {
	notRequired := false
	v := resolveFieldView(
		config.ConfigSpec{Name: "F", Required: &notRequired},
		&skillconfig.Store{}, "s",
	)
	if v.Value != "<missing-optional>" || v.Source != "missing-optional" {
		t.Errorf("got value=%q source=%q; want <missing-optional>/missing-optional", v.Value, v.Source)
	}
	if v.Required {
		t.Error("Required should be false")
	}
}

func TestResolveFieldView_TypeProjected(t *testing.T) {
	// fieldView.Type carries the EffectiveType, so an unspecified type
	// surfaces as "string" (the default), not the empty string.
	v := resolveFieldView(config.ConfigSpec{Name: "F"}, &skillconfig.Store{}, "s")
	if v.Type != "string" {
		t.Errorf("default Type should project as 'string', got %q", v.Type)
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
