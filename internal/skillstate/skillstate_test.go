package skillstate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tngtech/oh-my-agentic-coder/internal/config"
	"github.com/tngtech/oh-my-agentic-coder/internal/keychain"
	"github.com/tngtech/oh-my-agentic-coder/internal/registry"
	"github.com/tngtech/oh-my-agentic-coder/internal/secrets"
	"github.com/tngtech/oh-my-agentic-coder/internal/skillconfig"
)

// ---- helpers ----

// requiredFalse is the address-of-false needed by the specs' *bool Required.
func requiredFalse() *bool { b := false; return &b }

// skillDir writes a minimal skill directory and returns its path. The content
// is irrelevant to resolution (the meta is passed in-memory) but a real
// directory is needed for bundle hashing.
func skillDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, config.MetaFileName), []byte("name: probe\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// meta builds a Meta with a sidecar block carrying the given specs.
func meta(passthrough []string, secretSpecs []config.SecretSpec, cfgSpecs []config.ConfigSpec) *config.Meta {
	return &config.Meta{
		Name: "probe",
		Sidecar: &config.SidecarMeta{
			Command:        []string{"true"},
			EnvPassthrough: passthrough,
			Secrets:        secretSpecs,
			Config:         cfgSpecs,
		},
	}
}

// env returns an Options.Env backed by a map, so no test mutates the process.
func env(kv map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := kv[k]; return v, ok }
}

// keychainWith returns an Options.Keychain serving the given (skill/name ->
// value) map and reporting ErrNotFound for everything else.
func keychainWith(kv map[string]string) func(string, string, string) (secrets.Secret, error) {
	return func(scope, skill, name string) (secrets.Secret, error) {
		if v, ok := kv[skill+"/"+name]; ok {
			return secrets.NewSecretString(v), nil
		}
		return secrets.Secret{}, keychain.ErrNotFound
	}
}

// deadBusError is the text keyring surfaces on a host with no Secret Service.
const deadBusError = "dbus: couldn't determine address of session bus"

// deadKeychain mimics keychain.GetScoped on such a host: ErrNotFound (so every
// fallback still runs) AND ErrUnavailable (so an exhausted caller can diagnose
// the environment), with the raw cause WRAPPED — matching GetScoped's three-%w
// chain exactly, so keychain.BackendCause can recover it. Using %v here would
// truncate the chain and silently stop exercising the Detail text.
func deadKeychain() func(string, string, string) (secrets.Secret, error) {
	return func(scope, skill, name string) (secrets.Secret, error) {
		return secrets.Secret{}, fmt.Errorf("%w: %w: %w",
			keychain.ErrNotFound, keychain.ErrUnavailable, errors.New(deadBusError))
	}
}

// entry returns a registry entry whose recorded bundle hash matches dir, so
// tests that aren't about drift don't accidentally trip it.
func entry(t *testing.T, name, dir string) registry.Entry {
	t.Helper()
	h, err := config.BundleHash(dir)
	if err != nil {
		t.Fatal(err)
	}
	return registry.Entry{Name: name, SkillDir: dir, BundleHash: h}
}

func kinds(problems []Problem) []ProblemKind {
	out := make([]ProblemKind, 0, len(problems))
	for _, p := range problems {
		out = append(out, p.Kind)
	}
	return out
}

func wantKinds(t *testing.T, problems []Problem, want ...ProblemKind) {
	t.Helper()
	got := kinds(problems)
	if len(got) != len(want) {
		t.Fatalf("problems = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("problems = %v, want %v", got, want)
		}
	}
}

// ---- the acceptance criteria ----

// TestDefaultFromEnvSatisfiesRequiredField is issue #174's Failure 1: a skill
// whose only required config field comes from $default_from_env. It started
// fine under start and serve but live reload skipped it forever, because
// reload's copy of the rule omitted the DefaultFromEnv rung entirely. With one
// implementation there is no "reload's copy" to omit it.
func TestDefaultFromEnvSatisfiesRequiredField(t *testing.T) {
	dir := skillDir(t)
	m := meta(nil, nil, []config.ConfigSpec{
		{Name: "API_BASE", DefaultFromEnv: "MY_API_BASE"},
	})
	r := New(Options{Env: env(map[string]string{"MY_API_BASE": "https://api.example"})})

	armed, problems := r.Resolve(m, entry(t, "probe", dir), dir, nil)
	defer armed.Zero()

	wantKinds(t, problems)
	if got := armed.Config["API_BASE"]; got != "https://api.example" {
		t.Errorf("API_BASE = %q, want the env value", got)
	}
	if got := armed.ConfigSources["API_BASE"]; got != SourceDefaultFromEnv("MY_API_BASE") {
		t.Errorf("source = %q, want default_from_env:MY_API_BASE", got)
	}
}

// TestEnvPassthroughSatisfiesRequiredSecret is Failure 2: a secret listed in
// both secrets: and env_passthrough: is the documented fallback for
// keychain-less environments, and the supervisor injects it at spawn — so a
// preflight that calls it missing refuses a skill that would have worked.
func TestEnvPassthroughSatisfiesRequiredSecret(t *testing.T) {
	dir := skillDir(t)
	m := meta([]string{"ECHO_API_KEY"}, []config.SecretSpec{{Name: "ECHO_API_KEY"}}, nil)
	r := New(Options{
		Env:      env(map[string]string{"ECHO_API_KEY": "from-the-shell"}),
		Keychain: keychainWith(nil),
	})

	armed, problems := r.Resolve(m, entry(t, "probe", dir), dir, nil)
	defer armed.Zero()

	wantKinds(t, problems)
	if got := armed.Secrets["ECHO_API_KEY"].ExposeString(); got != "from-the-shell" {
		t.Errorf("secret = %q, want the env value", got)
	}
	if got := armed.SecretSources["ECHO_API_KEY"]; got != SourceEnvPassthrough {
		t.Errorf("source = %q, want env_passthrough", got)
	}
}

// TestEnvPassthroughWinsOverDeadBackend is the single most load-bearing
// ordering rule in this package. Now that an unavailable backend is
// distinguishable from an absent secret, it would be easy to classify the
// error first and report keychain-unavailable — which would break every
// headless CI runner that resolves its credentials from the shell today.
// The env_passthrough fallback must run BEFORE the classification.
func TestEnvPassthroughWinsOverDeadBackend(t *testing.T) {
	dir := skillDir(t)
	m := meta([]string{"ECHO_API_KEY"}, []config.SecretSpec{{Name: "ECHO_API_KEY"}}, nil)
	r := New(Options{
		Env:      env(map[string]string{"ECHO_API_KEY": "from-the-shell"}),
		Keychain: deadKeychain(),
	})

	armed, problems := r.Resolve(m, entry(t, "probe", dir), dir, nil)
	defer armed.Zero()

	wantKinds(t, problems)
	if got := armed.Secrets["ECHO_API_KEY"].ExposeString(); got != "from-the-shell" {
		t.Errorf("secret = %q, want the env value even though the keychain is down", got)
	}
}

// TestDeadBackendReportsUnavailableNotMissing is Failure 4: keychain.GetScoped
// maps an unavailable backend to ErrNotFound, so the launch path used to tell
// a headless user "required secret missing — run omac secrets set", advice that
// cannot work without a keychain to set it in.
func TestDeadBackendReportsUnavailableNotMissing(t *testing.T) {
	dir := skillDir(t)
	m := meta(nil, []config.SecretSpec{{Name: "TOKEN"}}, nil)
	r := New(Options{Keychain: deadKeychain()})

	armed, problems := r.Resolve(m, entry(t, "probe", dir), dir, nil)
	defer armed.Zero()

	wantKinds(t, problems, KeychainUnavailable)
	if strings.Contains(problems[0].Fix, "omac secrets set") {
		t.Errorf("Fix = %q, must not tell the user to set a secret in a keychain that isn't running", problems[0].Fix)
	}
	if !strings.Contains(problems[0].Fix, "Secret Service") {
		t.Errorf("Fix = %q, want the OS-specific backend hint", problems[0].Fix)
	}
	// Detail must be the backend's own words — which socket failed — not the
	// self-contradicting "secret not found: backend unavailable" chain. This
	// pins keychain.BackendCause's traversal from this side too.
	if problems[0].Detail != deadBusError {
		t.Errorf("Detail = %q, want the raw cause %q", problems[0].Detail, deadBusError)
	}
	if problems[0].Cause == nil || !errors.Is(problems[0].Cause, keychain.ErrUnavailable) {
		t.Errorf("Cause = %v, want the classified error for callers that switch on it", problems[0].Cause)
	}
}

// TestAbsentSecretOnHealthyBackendIsMissing is the other half: with a working
// keychain, an unset required secret must still be MissingSecret with the
// `omac secrets set` remedy.
func TestAbsentSecretOnHealthyBackendIsMissing(t *testing.T) {
	dir := skillDir(t)
	m := meta(nil, []config.SecretSpec{{Name: "TOKEN"}}, nil)
	r := New(Options{Keychain: keychainWith(nil)})

	armed, problems := r.Resolve(m, entry(t, "probe", dir), dir, nil)
	defer armed.Zero()

	wantKinds(t, problems, MissingSecret)
	if want := "omac secrets set probe TOKEN"; problems[0].Fix != want {
		t.Errorf("Fix = %q, want %q", problems[0].Fix, want)
	}
	if got := armed.SecretSources["TOKEN"]; got != SourceMissingReq {
		t.Errorf("source = %q, want missing-required", got)
	}
}

// TestScopedAndUnscopedSecretsBothResolve is Failure 3's root cause: doctor
// probed the keychain unscoped while start probed it workdir-scoped with an
// unscoped fallback, so the two disagreed about the same secret. Every caller
// now goes through this one lookup, whose scope is an Option.
func TestScopedAndUnscopedSecretsBothResolve(t *testing.T) {
	dir := skillDir(t)
	m := meta(nil, []config.SecretSpec{{Name: "TOKEN"}}, nil)
	e := entry(t, "probe", dir)

	for _, tc := range []struct{ name, scope, storedUnder string }{
		{"workdir-scoped hit", "workdir-id", "workdir-id"},
		{"unscoped fallback", "workdir-id", ""},
		{"global skill, unscoped", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := New(Options{
				Scope: tc.scope,
				// Mimic keychain.GetWithFallback: try the scope, fall back to
				// the unscoped key.
				Keychain: func(scope, skill, name string) (secrets.Secret, error) {
					if scope == tc.storedUnder {
						return secrets.NewSecretString("v"), nil
					}
					if tc.storedUnder == "" {
						return secrets.NewSecretString("v"), nil // unscoped fallback finds it
					}
					return secrets.Secret{}, keychain.ErrNotFound
				},
			})
			armed, problems := r.Resolve(m, e, dir, nil)
			defer armed.Zero()
			wantKinds(t, problems)
			if got := armed.SecretSources["TOKEN"]; got != SourceKeychain {
				t.Errorf("source = %q, want keychain", got)
			}
		})
	}
}

// ---- precedence ----

// TestConfigPrecedence locks stored > default > $default_from_env > missing,
// including the rule that an exported-but-empty variable does not satisfy a
// required field (an empty base URL is not a base URL).
func TestConfigPrecedence(t *testing.T) {
	dir := skillDir(t)
	e := entry(t, "probe", dir)
	spec := config.ConfigSpec{Name: "F", Default: "", DefaultFromEnv: "F_ENV"}

	store := &skillconfig.Store{Version: skillconfig.SchemaVersion}
	store.Set("probe", "F", "stored-value")

	cases := []struct {
		name       string
		store      *skillconfig.Store
		def        string
		envValue   map[string]string
		wantValue  string
		wantSource Source
		wantProbs  []ProblemKind
	}{
		{"stored wins over everything", store, "spec-default",
			map[string]string{"F_ENV": "env-value"}, "stored-value", SourceStored, nil},
		{"default beats default_from_env", nil, "spec-default",
			map[string]string{"F_ENV": "env-value"}, "spec-default", SourceDefault, nil},
		{"default_from_env is the last rung", nil, "",
			map[string]string{"F_ENV": "env-value"}, "env-value", SourceDefaultFromEnv("F_ENV"), nil},
		{"empty env value does not satisfy", nil, "",
			map[string]string{"F_ENV": ""}, "", SourceMissingReq, []ProblemKind{MissingField}},
		{"unset env value does not satisfy", nil, "",
			nil, "", SourceMissingReq, []ProblemKind{MissingField}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := spec
			s.Default = c.def
			m := meta(nil, nil, []config.ConfigSpec{s})
			r := New(Options{Env: env(c.envValue)})

			armed, problems := r.Resolve(m, e, dir, c.store)
			defer armed.Zero()

			wantKinds(t, problems, c.wantProbs...)
			if got := armed.Config["F"]; got != c.wantValue {
				t.Errorf("value = %q, want %q", got, c.wantValue)
			}
			if got := armed.ConfigSources["F"]; got != c.wantSource {
				t.Errorf("source = %q, want %q", got, c.wantSource)
			}
		})
	}
}

// TestEnvPassthroughGate covers exactly when the host environment may stand in
// for a keychain-absent secret. This is the fallback the skainet skills'
// omac.yaml documents ("keychain or env_passthrough"); without it `omac start`
// refuses to launch even though the supervisor would inject the var at runtime.
func TestEnvPassthroughGate(t *testing.T) {
	dir := skillDir(t)
	e := entry(t, "probe", dir)
	const name = "SKAINET_TOKEN"

	cases := []struct {
		name        string
		passthrough []string
		env         map[string]string
		wantProbs   []ProblemKind
	}{
		{"listed and env set", []string{name},
			map[string]string{name: "tngai_abcdefgh"}, nil},
		{"listed but env unset", []string{name},
			nil, []ProblemKind{MissingSecret}},
		{"listed but env empty — an empty value is no value", []string{name},
			map[string]string{name: ""}, []ProblemKind{MissingSecret}},
		{"env set but not listed — the skill never opted into passing it through", nil,
			map[string]string{name: "tngai_abcdefgh"}, []ProblemKind{MissingSecret}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := meta(c.passthrough, []config.SecretSpec{{Name: name}}, nil)
			r := New(Options{Env: env(c.env), Keychain: keychainWith(nil)})

			armed, problems := r.Resolve(m, e, dir, nil)
			defer armed.Zero()
			wantKinds(t, problems, c.wantProbs...)
		})
	}
}

// TestKeychainBeatsEnvPassthrough: a stored secret is authoritative, so a
// stale shell export cannot silently shadow the value the user registered.
func TestKeychainBeatsEnvPassthrough(t *testing.T) {
	dir := skillDir(t)
	m := meta([]string{"TOKEN"}, []config.SecretSpec{{Name: "TOKEN"}}, nil)
	r := New(Options{
		Env:      env(map[string]string{"TOKEN": "from-shell"}),
		Keychain: keychainWith(map[string]string{"probe/TOKEN": "from-keychain"}),
	})

	armed, problems := r.Resolve(m, entry(t, "probe", dir), dir, nil)
	defer armed.Zero()

	wantKinds(t, problems)
	if got := armed.Secrets["TOKEN"].ExposeString(); got != "from-keychain" {
		t.Errorf("secret = %q, want the keychain value to win", got)
	}
}

// TestSecretDefaultFromEnvIsNotARuntimeRung documents a deliberate
// non-behaviour: register.go honours a secret's default_from_env when
// prompting, but the supervisor only injects variables named in
// env_passthrough. Accepting it here would arm a skill whose sidecar then
// starts without the value.
func TestSecretDefaultFromEnvIsNotARuntimeRung(t *testing.T) {
	dir := skillDir(t)
	m := meta(nil, []config.SecretSpec{{Name: "TOKEN", DefaultFromEnv: "OTHER_VAR"}}, nil)
	r := New(Options{
		Env:      env(map[string]string{"OTHER_VAR": "value-the-sidecar-would-never-see"}),
		Keychain: keychainWith(nil),
	})

	armed, problems := r.Resolve(m, entry(t, "probe", dir), dir, nil)
	defer armed.Zero()

	wantKinds(t, problems, MissingSecret)
}

// ---- pattern re-validation ----

// TestEnvSuppliedSecretIsPatternChecked: a keychain value was vetted at
// register time, but an env_passthrough value reaches the sidecar unvalidated
// unless it is checked here.
func TestEnvSuppliedSecretIsPatternChecked(t *testing.T) {
	dir := skillDir(t)
	m := meta([]string{"TOKEN"}, []config.SecretSpec{
		{Name: "TOKEN", Pattern: `^tngai_[A-Za-z0-9_-]{8,}$`},
	}, nil)
	opts := Options{
		Env:      env(map[string]string{"TOKEN": "not-a-token"}),
		Keychain: keychainWith(nil),
	}

	armed, problems := New(opts).Resolve(m, entry(t, "probe", dir), dir, nil)
	defer armed.Zero()
	wantKinds(t, problems, InvalidSecret)
	// The value is still resolved: the pattern may simply be stale, and
	// --skip-secret-pattern must be able to pass it through.
	if got := armed.Secrets["TOKEN"].ExposeString(); got != "not-a-token" {
		t.Errorf("secret = %q, want the value carried even while flagged", got)
	}

	opts.SkipSecretPattern = true
	armed2, problems2 := New(opts).Resolve(m, entry(t, "probe", dir), dir, nil)
	defer armed2.Zero()
	wantKinds(t, problems2)
}

// TestKeychainSecretIsNotPatternChecked: re-validating a stored secret would
// make a stale pattern in a skill's omac.yaml break an already-working
// registration.
func TestKeychainSecretIsNotPatternChecked(t *testing.T) {
	dir := skillDir(t)
	m := meta(nil, []config.SecretSpec{{Name: "TOKEN", Pattern: `^tngai_`}}, nil)
	r := New(Options{Keychain: keychainWith(map[string]string{"probe/TOKEN": "legacy-value"})})

	armed, problems := r.Resolve(m, entry(t, "probe", dir), dir, nil)
	defer armed.Zero()

	wantKinds(t, problems)
}

// ---- optional values ----

// TestOptionalValuesNeverBlock: required:false secrets and fields are absent
// without being problems, and are marked missing-optional so `config show`
// can distinguish them.
func TestOptionalValuesNeverBlock(t *testing.T) {
	dir := skillDir(t)
	m := meta(nil,
		[]config.SecretSpec{{Name: "TOKEN", Required: requiredFalse()}},
		[]config.ConfigSpec{{Name: "F", Required: requiredFalse()}})
	r := New(Options{Keychain: keychainWith(nil)})

	armed, problems := r.Resolve(m, entry(t, "probe", dir), dir, nil)
	defer armed.Zero()

	wantKinds(t, problems)
	if got := armed.SecretSources["TOKEN"]; got != SourceMissingOpt {
		t.Errorf("secret source = %q, want missing-optional", got)
	}
	if got := armed.ConfigSources["F"]; got != SourceMissingOpt {
		t.Errorf("config source = %q, want missing-optional", got)
	}
}

// TestOptionalSecretSurvivesDeadBackend: an optional secret must not turn a
// keychain-less host into a wall of keychain-unavailable problems.
func TestOptionalSecretSurvivesDeadBackend(t *testing.T) {
	dir := skillDir(t)
	m := meta(nil, []config.SecretSpec{{Name: "TOKEN", Required: requiredFalse()}}, nil)
	r := New(Options{Keychain: deadKeychain()})

	armed, problems := r.Resolve(m, entry(t, "probe", dir), dir, nil)
	defer armed.Zero()

	wantKinds(t, problems)
}

// ---- bundle ----

// TestBundleDrift reports drift against the recorded hash, and
// AcceptBundleDrift suppresses it.
func TestBundleDrift(t *testing.T) {
	dir := skillDir(t)
	e := entry(t, "probe", dir)
	e.BundleHash = "sha256:stale"
	m := meta(nil, nil, nil)

	armed, problems := New(Options{}).Resolve(m, e, dir, nil)
	defer armed.Zero()
	wantKinds(t, problems, BundleDrift)
	if want := "omac register --force probe"; problems[0].Fix != want {
		t.Errorf("Fix = %q, want %q", problems[0].Fix, want)
	}
	if armed.Bundle == "" {
		t.Error("Bundle should be populated for the caller's approval gate even on drift")
	}

	armed2, problems2 := New(Options{AcceptBundleDrift: true}).Resolve(m, e, dir, nil)
	defer armed2.Zero()
	wantKinds(t, problems2)
	if armed2.Bundle == "" {
		t.Error("Bundle should still be computed under AcceptBundleDrift: the approval gate needs it")
	}
}

// TestBundleHashErrorIsNotDrift keeps an unreadable skill directory out of the
// drift bucket. start aborts the launch on it (it cannot produce useful
// per-skill diagnostics), while serve leaves the hash empty for the approval
// gate to re-derive — neither wants it reported as "you changed the bundle".
func TestBundleHashErrorIsNotDrift(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	e := registry.Entry{Name: "probe", SkillDir: missing, BundleHash: "sha256:whatever"}

	armed, problems := New(Options{}).Resolve(meta(nil, nil, nil), e, missing, nil)
	defer armed.Zero()

	wantKinds(t, problems)
	if armed.BundleErr == nil {
		t.Error("BundleErr should carry the hashing failure")
	}
	if armed.Bundle != "" {
		t.Errorf("Bundle = %q, want empty after a hashing failure", armed.Bundle)
	}
}

// TestSkipBundleHashDoesNoIO: doctor and `config show` only report on values,
// and auto-register eligibility runs before any hash is recorded, so none of
// them should pay a tree walk or see phantom drift.
func TestSkipBundleHashDoesNoIO(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	e := registry.Entry{Name: "probe", SkillDir: missing, BundleHash: "sha256:whatever"}

	armed, problems := New(Options{SkipBundleHash: true}).Resolve(meta(nil, nil, nil), e, missing, nil)
	defer armed.Zero()

	wantKinds(t, problems)
	if armed.BundleErr != nil || armed.Bundle != "" {
		t.Errorf("bundle = %q / err = %v, want both empty", armed.Bundle, armed.BundleErr)
	}
}

// ---- meta ----

// TestLoadReportsBrokenMeta covers both meta failure modes through Load, the
// entry point every launch path uses.
func TestLoadReportsBrokenMeta(t *testing.T) {
	t.Run("missing file", func(t *testing.T) {
		dir := t.TempDir()
		armed, problems := New(Options{}).Load(registry.Entry{Name: "probe"}, dir, nil)
		defer armed.Zero()
		wantKinds(t, problems, MetaBroken)
	})

	t.Run("no sidecar block", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, config.MetaFileName),
			[]byte("name: probe\ntype: skill\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		armed, problems := New(Options{}).Load(registry.Entry{Name: "probe"}, dir, nil)
		defer armed.Zero()
		wantKinds(t, problems, MetaBroken)
		if !strings.Contains(problems[0].Detail, "sidecar") {
			t.Errorf("Detail = %q, want it to name the missing sidecar block", problems[0].Detail)
		}
	})

	t.Run("mount defaults to the skill name", func(t *testing.T) {
		dir := skillDir(t)
		m := meta(nil, nil, nil)
		armed, _ := New(Options{}).Resolve(m, entry(t, "probe", dir), dir, nil)
		defer armed.Zero()
		if armed.Mount != "probe" {
			t.Errorf("Mount = %q, want the skill name", armed.Mount)
		}
	})
}

// ---- pass-level behaviour ----

// TestDeadBackendIsStickyAcrossThePass: start used to abort the launch on the
// first keychain error. Collecting every skill's problems instead must not mean
// re-dialing a bus that is provably not there once per skill — on macOS that is
// one blocking authorization prompt each.
func TestDeadBackendIsStickyAcrossThePass(t *testing.T) {
	dir := skillDir(t)
	m := meta(nil, []config.SecretSpec{{Name: "TOKEN"}}, nil)

	calls := 0
	dead := deadKeychain()
	r := New(Options{Keychain: func(scope, skill, name string) (secrets.Secret, error) {
		calls++
		return dead(scope, skill, name)
	}})

	for _, skill := range []string{"a", "b", "c"} {
		armed, problems := r.Resolve(m, entry(t, skill, dir), dir, nil)
		wantKinds(t, problems, KeychainUnavailable)
		armed.Zero()
	}
	if calls != 1 {
		t.Errorf("keychain called %d times, want 1: a provably dead backend must not be re-probed per skill", calls)
	}
}

// TestOpaqueFailureIsNotSticky is the counterpart, and the reason the sticky
// flag is scoped to ErrUnavailable. A corrupt entry or a single denied item says
// nothing about the next secret's readability, so short-circuiting on it would
// report one skill's error against skills whose secrets are perfectly fine —
// `omac doctor` would print "[fail] b: authorization denied" for a b it never
// asked about.
func TestOpaqueFailureIsNotSticky(t *testing.T) {
	dir := skillDir(t)
	m := meta(nil, []config.SecretSpec{{Name: "TOKEN"}}, nil)

	calls := 0
	r := New(Options{Keychain: func(scope, skill, name string) (secrets.Secret, error) {
		calls++
		if skill == "a" {
			return secrets.Secret{}, errors.New("keychain get omac/a/TOKEN: authorization denied")
		}
		return secrets.NewSecretString("fine"), nil
	}})

	_, aProblems := r.Resolve(m, entry(t, "a", dir), dir, nil)
	wantKinds(t, aProblems, KeychainUnavailable)

	bArmed, bProblems := r.Resolve(m, entry(t, "b", dir), dir, nil)
	defer bArmed.Zero()
	wantKinds(t, bProblems)
	if got := bArmed.Secrets["TOKEN"].ExposeString(); got != "fine" {
		t.Errorf("b's secret = %q, want it resolved: a's per-item failure must not condemn b", got)
	}
	if calls != 2 {
		t.Errorf("keychain called %d times, want 2", calls)
	}
}

// TestOpaqueKeychainFailureGetsNoMisleadingHint: an authorization denial on
// macOS is not fixed by installing gnome-keyring, so it must not carry the
// Secret Service hint.
func TestOpaqueKeychainFailureGetsNoMisleadingHint(t *testing.T) {
	dir := skillDir(t)
	m := meta(nil, []config.SecretSpec{{Name: "TOKEN"}}, nil)
	r := New(Options{Keychain: func(scope, skill, name string) (secrets.Secret, error) {
		return secrets.Secret{}, errors.New("keychain get omac/probe/TOKEN: authorization denied")
	}})

	armed, problems := r.Resolve(m, entry(t, "probe", dir), dir, nil)
	defer armed.Zero()

	wantKinds(t, problems, KeychainUnavailable)
	if problems[0].Fix != "" {
		t.Errorf("Fix = %q, want no remedy: we don't know one for an opaque failure", problems[0].Fix)
	}
	if !strings.Contains(problems[0].Detail, "authorization denied") {
		t.Errorf("Detail = %q, want the raw cause", problems[0].Detail)
	}
}

// TestAbsentSecretIsNotStickiness: an ordinary unset secret says nothing about
// the backend's health, so it must not suppress later skills' lookups.
func TestAbsentSecretIsNotStickiness(t *testing.T) {
	dir := skillDir(t)
	m := meta(nil, []config.SecretSpec{{Name: "TOKEN"}}, nil)

	calls := 0
	r := New(Options{Keychain: func(scope, skill, name string) (secrets.Secret, error) {
		calls++
		if skill == "b" {
			return secrets.NewSecretString("v"), nil
		}
		return secrets.Secret{}, keychain.ErrNotFound
	}})

	for _, skill := range []string{"a", "b"} {
		armed, _ := r.Resolve(m, entry(t, skill, dir), dir, nil)
		armed.Zero()
	}
	if calls != 2 {
		t.Errorf("keychain called %d times, want 2", calls)
	}
}

// TestZeroWipesSecretPlaintext: every caller obtains secret material, including
// the ones that only report on it (doctor, `config show`), so Zero must be
// unconditionally safe and actually wipe.
func TestZeroWipesSecretPlaintext(t *testing.T) {
	dir := skillDir(t)
	m := meta(nil, []config.SecretSpec{{Name: "TOKEN"}}, nil)
	r := New(Options{Keychain: keychainWith(map[string]string{"probe/TOKEN": "sensitive"})})

	armed, _ := r.Resolve(m, entry(t, "probe", dir), dir, nil)
	armed.Zero()
	if got := armed.Secrets["TOKEN"].ExposeString(); got != "" {
		t.Errorf("secret after Zero = %q, want wiped", got)
	}
	armed.Zero() // idempotent, so `defer armed.Zero()` is always safe
	var empty Armed
	empty.Zero() // safe on the zero value, e.g. after a MetaBroken return
}

// ---- multi-problem reporting ----

// TestAllProblemsAreCollected: the reason start accumulates rather than
// returning early is that fixing skill A used to reveal skill B, then C — N
// invocations for N problems. A skill hitting several classes must report all
// of them.
func TestAllProblemsAreCollected(t *testing.T) {
	dir := skillDir(t)
	e := entry(t, "probe", dir)
	e.BundleHash = "sha256:stale"
	m := meta([]string{"BAD"},
		[]config.SecretSpec{
			{Name: "MISSING"},
			{Name: "BAD", Pattern: `^ok-`},
		},
		[]config.ConfigSpec{{Name: "F"}})
	r := New(Options{
		Env:      env(map[string]string{"BAD": "wrong-shape"}),
		Keychain: keychainWith(nil),
	})

	armed, problems := r.Resolve(m, e, dir, nil)
	defer armed.Zero()

	wantKinds(t, problems, BundleDrift, MissingSecret, InvalidSecret, MissingField)
}

// TestMissingFieldsListsSecretsAndFields covers the helper serve and reload use
// to fill a pending-credentials route's "missing" list.
func TestMissingFieldsListsSecretsAndFields(t *testing.T) {
	problems := []Problem{
		{Kind: MissingField, Field: "ZED"},
		{Kind: BundleDrift},
		{Kind: MissingSecret, Field: "ALPHA"},
		{Kind: InvalidSecret, Field: "not-missing-just-malformed"},
	}
	got := MissingFields(problems)
	if len(got) != 2 || got[0] != "ALPHA" || got[1] != "ZED" {
		t.Errorf("MissingFields = %v, want [ALPHA ZED] sorted", got)
	}
}

func TestHasAndFirst(t *testing.T) {
	problems := []Problem{{Kind: MissingField, Field: "F"}, {Kind: BundleDrift}}
	if !Has(problems, BundleDrift, MetaBroken) {
		t.Error("Has should match any of the given kinds")
	}
	if Has(problems, KeychainUnavailable) {
		t.Error("Has matched a kind that isn't present")
	}
	if got := First(problems, BundleDrift); got == nil || got.Kind != BundleDrift {
		t.Errorf("First = %v, want the BundleDrift problem", got)
	}
	if got := First(problems, MetaBroken); got != nil {
		t.Errorf("First = %v, want nil", got)
	}
}

// ---- MergeConfig ----

// TestMergeConfigWorkdirOverridesGlobalPerField locks the first rung of the
// config ladder: "stored" means "stored in this merge", field-by-field, so a
// workdir override of one field doesn't drop the global value of another.
func TestMergeConfigWorkdirOverridesGlobalPerField(t *testing.T) {
	global := &skillconfig.Store{Version: skillconfig.SchemaVersion}
	global.Set("probe", "A", "global-a")
	global.Set("probe", "B", "global-b")
	workdir := &skillconfig.Store{Version: skillconfig.SchemaVersion}
	workdir.Set("probe", "B", "workdir-b")
	workdir.Set("other", "C", "workdir-c")

	merged := MergeConfig(global, workdir)

	for _, c := range []struct{ skill, field, want string }{
		{"probe", "A", "global-a"},  // global survives
		{"probe", "B", "workdir-b"}, // workdir wins
		{"other", "C", "workdir-c"}, // workdir-only skill
	} {
		if got, _ := merged.Get(c.skill, c.field); got != c.want {
			t.Errorf("Get(%q,%q) = %q, want %q", c.skill, c.field, got, c.want)
		}
	}
	// Inputs are not mutated.
	if got, _ := global.Get("probe", "B"); got != "global-b" {
		t.Errorf("global layer was mutated: %q", got)
	}
}

// TestInspectReadsNoSecrets is the property serve and live reload depend on:
// the spawn-approval gate is a security decision and must be settled before any
// keychain read, so a skill about to be refused costs neither an authorization
// prompt nor materialized plaintext.
func TestInspectReadsNoSecrets(t *testing.T) {
	dir := skillDir(t)
	if err := os.WriteFile(filepath.Join(dir, config.MetaFileName),
		[]byte("name: probe\nsidecar:\n  command: [\"true\"]\n  secrets:\n    - name: TOKEN\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := entry(t, "probe", dir)

	calls := 0
	r := New(Options{Keychain: func(scope, skill, name string) (secrets.Secret, error) {
		calls++
		return secrets.NewSecretString("v"), nil
	}})

	armed, problems := r.Inspect(e, dir)
	defer armed.Zero()
	wantKinds(t, problems)
	if calls != 0 {
		t.Errorf("Inspect made %d keychain calls, want 0", calls)
	}
	if armed.Bundle == "" {
		t.Error("Inspect should still produce the bundle hash the approval gate needs")
	}
	if len(armed.Secrets) != 0 {
		t.Errorf("Inspect resolved %d secrets, want none", len(armed.Secrets))
	}

	// Fill is the second half, and only then is the keychain touched.
	if probs := r.Fill(&armed, nil); len(probs) != 0 {
		t.Errorf("Fill problems = %v, want none", probs)
	}
	if calls != 1 {
		t.Errorf("Fill made %d keychain calls, want 1", calls)
	}
	if got := armed.Secrets["TOKEN"].ExposeString(); got != "v" {
		t.Errorf("secret = %q, want it resolved by Fill", got)
	}
}

// TestFillOnBrokenMetaIsNoOp: a caller that ignores Inspect's problems and calls
// Fill anyway must not panic on the nil meta.
func TestFillOnBrokenMetaIsNoOp(t *testing.T) {
	armed, problems := New(Options{}).Inspect(registry.Entry{Name: "probe"}, t.TempDir())
	wantKinds(t, problems, MetaBroken)
	if got := New(Options{}).Fill(&armed, nil); got != nil {
		t.Errorf("Fill = %v, want nil on a broken meta", got)
	}
}

// TestMergeConfigTreatsNilLayerAsEmpty: skillconfig.Load returns (nil, err) for
// an unparseable store and callers discard the error, so a nil layer must not
// panic. <workdir>/.opencode/skill-config.yaml is agent-writable and feeds the
// live-reload handler.
func TestMergeConfigTreatsNilLayerAsEmpty(t *testing.T) {
	populated := &skillconfig.Store{Version: skillconfig.SchemaVersion}
	populated.Set("probe", "F", "v")

	for _, c := range []struct {
		name            string
		global, workdir *skillconfig.Store
		wantValue       string
	}{
		{"nil workdir", populated, nil, "v"},
		{"nil global", nil, populated, "v"},
		{"both nil", nil, nil, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			merged := MergeConfig(c.global, c.workdir) // must not panic
			if got, _ := merged.Get("probe", "F"); got != c.wantValue {
				t.Errorf("Get = %q, want %q", got, c.wantValue)
			}
		})
	}
}

// TestStallForSplitsOnRecoverability is the rule serve and live reload share.
// CI caught the earlier version of it: on a headless runner (no Secret Service)
// every skill with a required secret was routed `broken` instead of
// `pending-credentials`, so the e2e suite's 409 became a 502 and the route was
// no longer promotable — on the primary `omac serve` deployment target.
func TestStallForSplitsOnRecoverability(t *testing.T) {
	cases := []struct {
		name         string
		problems     []Problem
		wantNil      bool
		wantTerminal bool
		wantMissing  []string
	}{
		{name: "ready", wantNil: true},

		// Recoverable: a value can still clear it, so the route stays promotable.
		{
			name:        "missing secret",
			problems:    []Problem{{Kind: MissingSecret, Field: "TOKEN"}},
			wantMissing: []string{"TOKEN"},
		},
		{
			name:        "missing field",
			problems:    []Problem{{Kind: MissingField, Field: "BASE"}},
			wantMissing: []string{"BASE"},
		},
		{
			name:        "dead keychain — start a keyring or export the var",
			problems:    []Problem{{Kind: KeychainUnavailable, Field: "TOKEN", Detail: "no bus", Fix: "install gnome-keyring"}},
			wantMissing: []string{"TOKEN"},
		},
		{
			name:        "malformed env value — fix the export or store a valid one",
			problems:    []Problem{{Kind: InvalidSecret, Field: "TOKEN", Detail: "bad shape", Fix: "fix it"}},
			wantMissing: []string{"TOKEN"},
		},

		// Terminal: needs a re-register, so no credential list is offered.
		{
			name:         "broken meta",
			problems:     []Problem{{Kind: MetaBroken, Detail: "bad yaml", Fix: "omac register --force x"}},
			wantTerminal: true,
		},
		{
			name:         "bundle drift",
			problems:     []Problem{{Kind: BundleDrift, Detail: "changed", Fix: "omac register --force x"}},
			wantTerminal: true,
		},
		{
			name: "terminal outranks recoverable",
			problems: []Problem{
				{Kind: MissingSecret, Field: "TOKEN"},
				{Kind: BundleDrift, Detail: "changed", Fix: "omac register --force x"},
			},
			wantTerminal: true,
		},

		// Every unresolved value is listed once, sorted, whatever its cause.
		{
			name: "mixed recoverable causes are merged and deduped",
			problems: []Problem{
				{Kind: MissingField, Field: "BASE"},
				{Kind: KeychainUnavailable, Field: "TOKEN", Detail: "no bus", Fix: "hint"},
				{Kind: InvalidSecret, Field: "TOKEN", Detail: "bad", Fix: "fix"},
			},
			wantMissing: []string{"BASE", "TOKEN"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := StallFor(c.problems)
			if c.wantNil {
				if got != nil {
					t.Fatalf("StallFor = %+v, want nil", got)
				}
				return
			}
			if got == nil {
				t.Fatal("StallFor = nil, want a stall")
			}
			if got.Terminal != c.wantTerminal {
				t.Errorf("Terminal = %v, want %v", got.Terminal, c.wantTerminal)
			}
			if got.Detail == "" {
				t.Error("a stall must carry a diagnostic")
			}
			if len(got.Missing) != len(c.wantMissing) {
				t.Fatalf("Missing = %v, want %v", got.Missing, c.wantMissing)
			}
			for i := range c.wantMissing {
				if got.Missing[i] != c.wantMissing[i] {
					t.Errorf("Missing = %v, want %v", got.Missing, c.wantMissing)
				}
			}
		})
	}
}

// TestStallForCarriesTheRealRemedy: a cause with its own remedy must set the
// detail, so an agent staring at a pending route is not left with only "run omac
// secrets set" when the keychain is what's broken.
func TestStallForCarriesTheRealRemedy(t *testing.T) {
	st := StallFor([]Problem{
		{Kind: MissingField, Field: "BASE"},
		{Kind: KeychainUnavailable, Field: "TOKEN", Detail: "dbus: no session bus", Fix: "no Secret Service provider found"},
	})
	if !strings.Contains(st.Detail, "dbus: no session bus") || !strings.Contains(st.Detail, "Secret Service") {
		t.Errorf("Detail = %q, want the cause and its remedy", st.Detail)
	}

	// With only plain missing values, the generic list is the detail.
	st = StallFor([]Problem{{Kind: MissingSecret, Field: "TOKEN"}})
	if !strings.Contains(st.Detail, "missing required values") {
		t.Errorf("Detail = %q, want the generic missing-values summary", st.Detail)
	}
}
