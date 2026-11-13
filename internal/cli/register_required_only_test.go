package cli

import (
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
	"github.com/TNG/oh-my-agentic-coder/internal/keychain"
	"github.com/TNG/oh-my-agentic-coder/internal/registry"
	"github.com/TNG/oh-my-agentic-coder/internal/skillconfig"
	"github.com/TNG/oh-my-agentic-coder/internal/skillstate"
)

// `omac register --required-only` asks only for values omac start cannot do
// without. These tests feed stdin with a value that would be stored if a
// prompt read it, so an untouched store proves that nothing was asked.

func TestRegisterFieldRequiredOnly(t *testing.T) {
	no := false
	cases := []struct {
		name      string
		spec      config.ConfigSpec
		env       map[string]string
		want      valueOutcome
		wantValue string // "" = nothing stored
	}{
		{
			name: "optional without default is left out",
			spec: config.ConfigSpec{Name: "F", Required: &no},
			want: valueLeftOut,
		},
		{
			name: "optional with default is left out",
			spec: config.ConfigSpec{Name: "F", Required: &no, Default: "d"},
			want: valueLeftOut,
		},
		{
			name: "required with default is left out, start uses the default",
			spec: config.ConfigSpec{Name: "F", Default: "d"},
			want: valueLeftOut,
		},
		{
			name:      "required with default_from_env stores the env value",
			spec:      config.ConfigSpec{Name: "F", DefaultFromEnv: "OMAC_TEST_RO_F"},
			env:       map[string]string{"OMAC_TEST_RO_F": "from-env"},
			want:      valueStored,
			wantValue: "from-env",
		},
		{
			name: "optional with default_from_env is left out",
			spec: config.ConfigSpec{Name: "F", Required: &no, DefaultFromEnv: "OMAC_TEST_RO_F"},
			env:  map[string]string{"OMAC_TEST_RO_F": "from-env"},
			want: valueLeftOut,
		},
		{
			name:      "required without any default is asked",
			spec:      config.ConfigSpec{Name: "F"},
			want:      valueStored,
			wantValue: "typed",
		},
		{
			name:      "required with unset default_from_env is asked",
			spec:      config.ConfigSpec{Name: "F", DefaultFromEnv: "OMAC_TEST_RO_UNSET"},
			want:      valueStored,
			wantValue: "typed",
		},
		{
			name:      "OMAC_CONFIG_ still supplies an optional value",
			spec:      config.ConfigSpec{Name: "F", Required: &no},
			env:       map[string]string{"OMAC_CONFIG_F": "from-config-env"},
			want:      valueStored,
			wantValue: "from-config-env",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for k, v := range c.env {
				t.Setenv(k, v)
			}
			env := envWithStdin(t, "typed\n")
			store := &skillconfig.Store{}
			got, err := registerField(env, store, nil, "skill", "", c.spec, false, nil, nil, false, nil, true)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Errorf("outcome = %d, want %d", got, c.want)
			}
			v, ok := store.Get("skill", "F")
			if c.wantValue == "" && ok {
				t.Errorf("stored %q, want nothing stored", v)
			}
			if c.wantValue != "" && v != c.wantValue {
				t.Errorf("stored %q (present=%v), want %q", v, ok, c.wantValue)
			}
		})
	}
}

// A value from --fields-from is stored even when the flag would not ask
// for it, and an already stored value stays.
func TestRegisterFieldRequiredOnlyKeepsSuppliedValues(t *testing.T) {
	no := false
	spec := config.ConfigSpec{Name: "F", Required: &no, Default: "d"}

	env := envWithStdin(t, "")
	store := &skillconfig.Store{}
	got, err := registerField(env, store, nil, "skill", "", spec, false, nil, map[string]string{"F": "from-file"}, false, nil, true)
	if err != nil || got != valueStored {
		t.Fatalf("from file: outcome = %d, err = %v", got, err)
	}
	if v, _ := store.Get("skill", "F"); v != "from-file" {
		t.Errorf("from file: stored %q", v)
	}

	store = &skillconfig.Store{}
	store.Set("skill", "F", "kept")
	got, err = registerField(env, store, nil, "skill", "", spec, false, nil, nil, false, nil, true)
	if err != nil || got != valueStored {
		t.Fatalf("already stored: outcome = %d, err = %v", got, err)
	}
	if v, _ := store.Get("skill", "F"); v != "kept" {
		t.Errorf("already stored: value changed to %q", v)
	}
}

// An optional value declined on an earlier run stays declined: the flag
// must not drop it from the registry's skipped list.
func TestRegisterFieldRequiredOnlyKeepsPreviousSkip(t *testing.T) {
	no := false
	env := envWithStdin(t, "typed\n")
	got, err := registerField(env, &skillconfig.Store{}, nil, "skill", "", config.ConfigSpec{Name: "F", Required: &no}, false, map[string]bool{"F": true}, nil, false, nil, true)
	if err != nil || got != valueSkipped {
		t.Errorf("outcome = %d, err = %v; want valueSkipped", got, err)
	}
}

func TestRegisterSecretRequiredOnly(t *testing.T) {
	isolateHome(t)
	no := false
	env := envWithStdin(t, "")
	prompts := fakeSecretInput(t, "optional-value")
	got, err := registerSecret(env, "", "ro-secret", "", config.SecretSpec{Name: "OPT", Required: &no}, false, nil, nil, false, "linux", true)
	if err != nil || got != valueLeftOut {
		t.Fatalf("optional: outcome = %d, err = %v; want valueLeftOut", got, err)
	}
	if prompts.Len() != 0 {
		t.Errorf("optional secret was prompted: %q", prompts.String())
	}

	prompts = fakeSecretInput(t, "required-value")
	got, err = registerSecret(env, "", "ro-secret", "", config.SecretSpec{Name: "REQ"}, false, nil, nil, false, "linux", true)
	if err != nil || got != valueStored {
		t.Fatalf("required: outcome = %d, err = %v; want valueStored", got, err)
	}
	if !strings.Contains(prompts.String(), "REQ") {
		t.Errorf("required secret was not prompted: %q", prompts.String())
	}
}

// TestRunRegisterRequiredOnly runs the whole command on a skill with one
// value of each kind. Only the required secret and the required field
// without a default are asked, nothing is recorded as declined, and the
// resolver that omac start uses accepts the result.
func TestRunRegisterRequiredOnly(t *testing.T) {
	isolateHome(t)
	wd := t.TempDir()
	stageHarnessSkillBody(t, wd, "opencode", "rodemo", `  secrets:
    - name: RO_TOKEN
    - name: RO_EXTRA_TOKEN
      required: false
  config:
    - name: RO_REGION
    - name: RO_MODE
      default: fast
    - name: RO_VERBOSE
      type: bool
      required: false
`)
	env, _, errBuf, drain := newPipeEnv(t, "eu\n")
	env.Workdir = wd
	prompts := fakeSecretInput(t, "token-1", "token-2")

	code := runRegister([]string{"rodemo", "--harness", "opencode", "--required-only"}, env)
	drain()
	out := errBuf.String()
	if code != ExitOK {
		t.Fatalf("register: code = %d\n%s", code, out)
	}
	if got := prompts.String(); strings.Count(got, "?") != 1 || !strings.Contains(got, "RO_TOKEN") {
		t.Errorf("secret prompts = %q, want only RO_TOKEN", got)
	}
	for _, want := range []string{
		"RO_EXTRA_TOKEN (optional, not asked)",
		"RO_MODE uses the default: fast",
		"RO_VERBOSE (optional, not asked)",
		"3 values not asked",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stderr is missing %q:\n%s", want, out)
		}
	}

	store, err := skillconfig.Load(wd)
	if err != nil {
		t.Fatal(err)
	}
	if got := store.Skills["rodemo"]; len(got) != 1 || got["RO_REGION"] != "eu" {
		t.Errorf("stored config = %v, want only RO_REGION=eu", got)
	}
	reg, err := registry.Load(wd)
	if err != nil {
		t.Fatal(err)
	}
	e, _ := reg.Find("rodemo")
	if e == nil {
		t.Fatal("skill not registered")
	}
	if len(e.SkippedSecretNames) != 0 || len(e.SkippedConfigFields) != 0 {
		t.Errorf("skipped lists = %v / %v, want empty so a later register asks again",
			e.SkippedSecretNames, e.SkippedConfigFields)
	}

	view, code := buildSkillView(env, "rodemo")
	if code != ExitOK {
		t.Fatalf("buildSkillView: code = %d", code)
	}
	defer view.zero()
	sources := map[string]string{}
	for _, f := range view.Config {
		sources[f.Name] = f.Source
	}
	for name, want := range map[string]skillstate.Source{
		"RO_REGION":  skillstate.SourceStored,
		"RO_MODE":    skillstate.SourceDefault,
		"RO_VERBOSE": skillstate.SourceMissingOpt,
	} {
		if sources[name] != string(want) {
			t.Errorf("%s source = %q, want %q", name, sources[name], want)
		}
	}
}

func TestRunRegisterRequiredOnlyRejectsReprompt(t *testing.T) {
	isolateHome(t)
	wd := t.TempDir()
	stageHarnessSkillBody(t, wd, "opencode", "rodemo", "")
	for _, flag := range []string{"--reprompt-fields", "--reprompt-secrets"} {
		env, _, errBuf, drain := newPipeEnv(t, "")
		env.Workdir = wd
		code := runRegister([]string{"rodemo", "--required-only", flag}, env)
		drain()
		if code != ExitMisuse {
			t.Errorf("%s: code = %d, want ExitMisuse", flag, code)
		}
		if !strings.Contains(errBuf.String(), "cannot be combined") {
			t.Errorf("%s: stderr = %q", flag, errBuf.String())
		}
	}
}

// A default that differs from the one recorded at the last register is
// asked, not taken: the skill changed it since, and start would refuse it.
func TestRegisterFieldRequiredOnlyAsksChangedDefault(t *testing.T) {
	no := false
	cases := []struct {
		name      string
		spec      config.ConfigSpec
		recorded  map[string]string
		wantValue string // "" = nothing stored
	}{
		{"unchanged default is left out", config.ConfigSpec{Name: "F", Default: "d"}, map[string]string{"F": "d"}, ""},
		{"changed default is asked", config.ConfigSpec{Name: "F", Default: "new"}, map[string]string{"F": "old"}, "typed"},
		{"added default is asked", config.ConfigSpec{Name: "F", Required: &no, Default: "new"}, map[string]string{}, "typed"},
		{"optional with changed default is asked", config.ConfigSpec{Name: "F", Required: &no, Default: "new"}, map[string]string{"F": "old"}, "typed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := envWithStdin(t, "typed\n")
			env.Workdir = t.TempDir()
			defStore := &skillconfig.Store{}
			defStore.RecordApprovedDefaults(keychain.WorkdirID(env.Workdir), "skill", c.recorded)
			store := &skillconfig.Store{}
			if _, err := registerField(env, store, nil, "skill", "", c.spec, false, nil, nil, false, defStore, true); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			v, ok := store.Get("skill", "F")
			if c.wantValue == "" && ok {
				t.Errorf("stored %q, want nothing stored", v)
			}
			if c.wantValue != "" && v != c.wantValue {
				t.Errorf("stored %q (present=%v), want %q", v, ok, c.wantValue)
			}
		})
	}
}

func TestUnstoredDefaults(t *testing.T) {
	specs := []config.ConfigSpec{{Name: "A", Default: "a"}, {Name: "B", Default: "b"}, {Name: "C"}}
	got := unstoredDefaults(specs, map[string]string{"B": "set"})
	if len(got) != 1 || got["A"] != "a" {
		t.Errorf("unstoredDefaults = %v, want map[A:a]", got)
	}
	if got := unstoredDefaults(nil, nil); got == nil || len(got) != 0 {
		t.Errorf("unstoredDefaults(nil) = %#v, want an empty, non-nil map", got)
	}
}
