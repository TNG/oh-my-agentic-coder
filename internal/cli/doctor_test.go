package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeWorkdirConfig writes an oh-my-agentic-coder.yaml into the workdir
// with a single sandbox profile whose command is the given argv template.
func writeWorkdirConfig(t *testing.T, workdir string, profileName string, command []string) {
	t.Helper()
	dir := filepath.Join(workdir, ".opencode")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Build YAML by hand to avoid pulling in yaml.v3 from the test.
	var sb strings.Builder
	sb.WriteString("sandbox:\n  default_profile: " + profileName + "\n  profiles:\n")
	sb.WriteString("    " + profileName + ":\n      command:\n")
	for _, c := range command {
		sb.WriteString("        - " + yamlScalar(c) + "\n")
	}
	if err := os.WriteFile(filepath.Join(dir, "oh-my-agentic-coder.yaml"), []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

// yamlScalar renders s as a double-quoted YAML scalar, escaping
// backslashes and double-quotes. Used for command tokens that contain
// braces ({{self}}) which YAML would otherwise parse as flow mappings.
func yamlScalar(s string) string {
	r := strings.NewReplacer("\\", "\\\\", "\"", "\\\"")
	return "\"" + r.Replace(s) + "\""
}

// stageHomeWithCargoSentinels creates a fake HOME with mode-000 cargo
// config/credential sentinel files. Returns the home path.
func stageHomeWithCargoSentinels(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	cargo := filepath.Join(home, ".cargo")
	if err := os.MkdirAll(cargo, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"config", "config.toml", "credentials", "credentials.toml"} {
		p := filepath.Join(cargo, name)
		if err := os.WriteFile(p, []byte("SENTINEL_CONTENT_"+name+"_DO_NOT_PRINT"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(p, 0o600) })
	}
	return home
}

func TestDoctorCacheRootsWarning(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	workdir := t.TempDir()

	// A profile that grants Allow on ~/.cache and ~/Library/Caches,
	// plus Read on ~/.cache (still a warning) but NOT on ~/go (no warning
	// for ~/go Read here — that's covered separately).
	writeWorkdirConfig(t, workdir, "builtin", []string{
		"{{self}}", "sandbox", "run",
		"--profile", "default",
		"--allow-file", "{{socket}}",
		"--read", "{{socket_dir}}",
		"--", "{{inner_cmd}}", "{{inner_args}}",
	})

	// Stage a sandbox profile that grants cache roots.
	stageProfile(t, home, `{
	  "meta": {"name": "default"},
	  "filesystem": {
	    "allow": ["~/.cache"],
	    "read":  ["~/Library/Caches"]
	  }
	}`)

	env, outBuf, _, drain := newPipeEnv(t, "")
	env.Workdir = workdir
	code := runDoctor([]string{}, env)
	drain()
	output := outBuf.String()

	if code != ExitOK {
		t.Errorf("doctor exit = %d, want ExitOK (warnings are advisory)", code)
	}
	if !strings.Contains(output, "~/.cache") {
		t.Errorf("doctor output missing ~/.cache warning; got:\n%s", output)
	}
	if !strings.Contains(output, "~/Library/Caches") {
		t.Errorf("doctor output missing ~/Library/Caches warning; got:\n%s", output)
	}
	if !strings.Contains(strings.ToLower(output), "impact") {
		t.Errorf("doctor output missing impact field; got:\n%s", output)
	}
	if !strings.Contains(strings.ToLower(output), "remediation") {
		t.Errorf("doctor output missing remediation field; got:\n%s", output)
	}
}

func TestDoctorCargoToolchainWarnings(t *testing.T) {
	home := stageHomeWithCargoSentinels(t)
	workdir := t.TempDir()

	writeWorkdirConfig(t, workdir, "builtin", []string{
		"{{self}}", "sandbox", "run",
		"--profile", "default",
		"--allow-file", "{{socket}}",
		"--read", "{{socket_dir}}",
		"--", "{{inner_cmd}}", "{{inner_args}}",
	})

	// Profile granting Allow on ~/.cargo, ~/.rustup, ~/go (whole tool homes).
	stageProfile(t, home, `{
	  "meta": {"name": "default"},
	  "filesystem": {
	    "allow": ["~/.cargo", "~/.rustup", "~/go"]
	  }
	}`)

	env, outBuf, _, drain := newPipeEnv(t, "")
	env.Workdir = workdir
	code := runDoctor([]string{}, env)
	drain()
	output := outBuf.String()

	if code != ExitOK {
		t.Errorf("doctor exit = %d, want ExitOK (advisory only)", code)
	}
	for _, want := range []string{"~/.cargo", "~/.rustup", "~/go"} {
		if !strings.Contains(output, want) {
			t.Errorf("doctor output missing %q warning; got:\n%s", want, output)
		}
	}
	// Cargo sentinel files must exist but their contents must never be printed.
	if strings.Contains(output, "SENTINEL_CONTENT") {
		t.Errorf("doctor leaked sentinel file contents:\n%s", output)
	}
	// Cargo remediation should explain the correct token template and how it
	// reaches the sandboxed harness without sidecar configuration.
	if !strings.Contains(output, ".cargo/config.toml") {
		t.Errorf("doctor output missing project .cargo/config.toml guidance; got:\n%s", output)
	}
	for _, want := range []string{
		"CARGO_REGISTRIES_<NAME>_TOKEN",
		"NAME is the registry key uppercased with '-' replaced by '_'",
		"environment.allow_vars",
		"sandboxed harness",
		"sidecar.env_passthrough",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("doctor output missing %q guidance; got:\n%s", want, output)
		}
	}
}

func TestDoctorCargoReadWarned(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	workdir := t.TempDir()

	writeWorkdirConfig(t, workdir, "builtin", []string{
		"{{self}}", "sandbox", "run",
		"--profile", "default",
		"--", "{{inner_cmd}}", "{{inner_args}}",
	})

	// A Read grant for the whole Cargo home exposes host configuration and
	// credentials inside the sandbox.
	stageProfile(t, home, `{
	  "meta": {"name": "default"},
	  "filesystem": {
	    "read": ["~/.cargo"]
	  }
	}`)

	env, outBuf, _, drain := newPipeEnv(t, "")
	env.Workdir = workdir
	code := runDoctor([]string{}, env)
	drain()
	output := outBuf.String()

	if code != ExitOK {
		t.Errorf("doctor exit = %d, want ExitOK", code)
	}
	if !strings.Contains(output, "Read ~/.cargo") {
		t.Errorf("doctor did not warn on read-only Cargo-home grant; got:\n%s", output)
	}
	if !strings.Contains(output, "host Cargo configuration and credentials") {
		t.Errorf("doctor output missing Cargo Read impact; got:\n%s", output)
	}
}

func TestDoctorCargoBinReadNotWarned(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	workdir := t.TempDir()

	writeWorkdirConfig(t, workdir, "builtin", []string{
		"{{self}}", "sandbox", "run",
		"--profile", "default",
		"--", "{{inner_cmd}}", "{{inner_args}}",
	})

	// The narrow runtime grant does not expose the whole Cargo home.
	stageProfile(t, home, `{
	  "meta": {"name": "default"},
	  "filesystem": {
	    "read": ["~/.cargo/bin"]
	  }
	}`)

	env, outBuf, _, drain := newPipeEnv(t, "")
	env.Workdir = workdir
	code := runDoctor([]string{}, env)
	drain()
	output := outBuf.String()

	if code != ExitOK {
		t.Errorf("doctor exit = %d, want ExitOK", code)
	}
	if strings.Contains(output, "host Cargo configuration and credentials") {
		t.Errorf("doctor warned on narrow Cargo runtime grant; got:\n%s", output)
	}
}

func TestDoctorCargoSentinelsWarnForIsolatedCARGOHome(t *testing.T) {
	home := stageHomeWithCargoSentinels(t)
	workdir := t.TempDir()

	writeWorkdirConfig(t, workdir, "builtin", []string{
		"{{self}}", "sandbox", "run",
		"--profile", "default",
		"--", "{{inner_cmd}}", "{{inner_args}}",
	})

	stageProfile(t, home, `{
	  "meta": {"name": "default"},
	  "filesystem": {
	    "read": ["~/.cargo/bin"]
	  }
	}`)

	env, outBuf, _, drain := newPipeEnv(t, "")
	env.Workdir = workdir
	code := runDoctor([]string{}, env)
	drain()
	output := outBuf.String()

	if code != ExitOK {
		t.Errorf("doctor exit = %d, want ExitOK", code)
	}
	for _, path := range []string{
		"~/.cargo/config", "~/.cargo/config.toml",
		"~/.cargo/credentials", "~/.cargo/credentials.toml",
	} {
		if !strings.Contains(output, path) {
			t.Errorf("missing Cargo sentinel warning for %s: %s", path, output)
		}
	}
	if strings.Contains(output, "SENTINEL_CONTENT") {
		t.Fatal("doctor read Cargo sentinel content")
	}
}

func TestDoctorWholeHomeReadWarned(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	workdir := t.TempDir()

	writeWorkdirConfig(t, workdir, "builtin", []string{
		"{{self}}", "sandbox", "run",
		"--profile=default",
		"--", "{{inner_cmd}}", "{{inner_args}}",
	})

	// A whole-home Read grant covers ~/.cargo/config etc transitively.
	stageProfile(t, home, `{
	  "meta": {"name": "default"},
	  "filesystem": {
	    "read": ["~"]
	  }
	}`)

	env, outBuf, _, drain := newPipeEnv(t, "")
	env.Workdir = workdir
	code := runDoctor([]string{}, env)
	drain()
	output := outBuf.String()

	if code != ExitOK {
		t.Errorf("doctor exit = %d, want ExitOK", code)
	}
	if !strings.Contains(output, "Read access exposes host Cargo configuration and credentials") {
		t.Errorf("doctor should warn that whole-home Read covers ~/.cargo; got:\n%s", output)
	}
}

func TestDoctorParentPathHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	workdir := t.TempDir()

	writeWorkdirConfig(t, workdir, "builtin", []string{
		"{{self}}", "sandbox", "run",
		"--profile", "default",
		"--", "{{inner_cmd}}", "{{inner_args}}",
	})

	// Grant Allow on $HOME (the parent) which transitively covers
	// ~/.cargo etc.
	stageProfile(t, home, `{
	  "meta": {"name": "default"},
	  "filesystem": {
	    "allow": ["$HOME"]
	  }
	}`)

	env, outBuf, _, drain := newPipeEnv(t, "")
	env.Workdir = workdir
	code := runDoctor([]string{}, env)
	drain()
	output := outBuf.String()

	if code != ExitOK {
		t.Errorf("doctor exit = %d, want ExitOK", code)
	}
	if !strings.Contains(output, "~/.cargo") && !strings.Contains(output, "$HOME") {
		t.Errorf("doctor should warn that $HOME Allow covers tool homes; got:\n%s", output)
	}
}

func TestDoctorNoFalseMatchForCargo2(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	workdir := t.TempDir()

	writeWorkdirConfig(t, workdir, "builtin", []string{
		"{{self}}", "sandbox", "run",
		"--profile", "default",
		"--", "{{inner_cmd}}", "{{inner_args}}",
	})

	// ~/.cargo2 is NOT ~/.cargo — must not trigger the cargo warning.
	stageProfile(t, home, `{
	  "meta": {"name": "default"},
	  "filesystem": {
	    "allow": ["~/.cargo2"]
	  }
	}`)

	env, outBuf, _, drain := newPipeEnv(t, "")
	env.Workdir = workdir
	code := runDoctor([]string{}, env)
	drain()
	output := outBuf.String()

	if code != ExitOK {
		t.Errorf("doctor exit = %d, want ExitOK", code)
	}
	if strings.Contains(output, "~/.cargo2") && strings.Contains(strings.ToLower(output), "tool home") {
		t.Errorf("doctor falsely matched ~/.cargo2 as cargo; got:\n%s", output)
	}
}

func TestDoctorOpaqueExternalCommandSkipped(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	workdir := t.TempDir()

	// A non-{{self}} sandbox run command is opaque — doctor can't
	// inspect it, so no warnings should be produced (and no crash).
	writeWorkdirConfig(t, workdir, "nono", []string{
		"nono", "run",
		"--profile", "tng-sandbox",
		"--allow-file", "{{socket}}",
		"--read", "{{socket_dir}}",
		"--", "{{inner_cmd}}", "{{inner_args}}",
	})

	// Even with a default profile that has broad grants, doctor must
	// not warn because it can't see into the nono profile's tng-sandbox.
	stageProfile(t, home, `{
	  "meta": {"name": "default"},
	  "filesystem": {
	    "allow": ["~/.cargo", "~/.rustup", "~/go"]
	  }
	}`)

	env, outBuf, _, drain := newPipeEnv(t, "")
	env.Workdir = workdir
	code := runDoctor([]string{}, env)
	drain()
	output := outBuf.String()

	if code != ExitOK {
		t.Errorf("doctor exit = %d, want ExitOK", code)
	}
	if strings.Contains(output, "~/.cargo") && strings.Contains(strings.ToLower(output), "tool home") {
		t.Errorf("doctor warned for opaque external command; got:\n%s", output)
	}
}

func TestDoctorNonRunBuiltinCommandSkipped(t *testing.T) {
	home := stageHomeWithCargoSentinels(t)
	workdir := t.TempDir()

	writeWorkdirConfig(t, workdir, "builtin", []string{
		"{{self}}", "sandbox", "stage2",
	})

	stageProfile(t, home, `{
	  "meta": {"name": "default"},
	  "filesystem": {
	    "allow": ["~/.cargo"]
	  }
	}`)

	env, outBuf, _, drain := newPipeEnv(t, "")
	env.Workdir = workdir
	code := runDoctor([]string{}, env)
	drain()
	output := outBuf.String()

	if code != ExitOK {
		t.Errorf("doctor exit = %d, want ExitOK", code)
	}
	if strings.Contains(output, `[warn] sandbox profile "builtin"`) {
		t.Errorf("doctor warned for non-run built-in command; got:\n%s", output)
	}
}

func TestDoctorOmittedProfileInspectsDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	workdir := t.TempDir()

	// Omitted --profile resolves to "default".
	writeWorkdirConfig(t, workdir, "builtin", []string{
		"{{self}}", "sandbox", "run",
		"--allow-file", "{{socket}}",
		"--read", "{{socket_dir}}",
		"--", "{{inner_cmd}}", "{{inner_args}}",
	})

	stageProfile(t, home, `{
	  "meta": {"name": "default"},
	  "filesystem": {
	    "allow": ["~/.cargo"]
	  }
	}`)

	env, outBuf, _, drain := newPipeEnv(t, "")
	env.Workdir = workdir
	code := runDoctor([]string{}, env)
	drain()
	output := outBuf.String()

	if code != ExitOK {
		t.Errorf("doctor exit = %d, want ExitOK", code)
	}
	if !strings.Contains(output, "~/.cargo") {
		t.Errorf("doctor should inspect omitted --profile as default; got:\n%s", output)
	}
}

// TestDoctorEmptyAllowVarsWarned asserts doctor flags a native sandbox
// profile whose environment.allow_vars is empty: at launch the selected
// harness's auth vars are injected as --allow-env, flipping the empty
// (inherit-all) list into a restrictive allowlist that strips HOME/PATH and
// provider tokens. Advisory only — exit stays ExitOK.
func TestDoctorEmptyAllowVarsWarned(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	workdir := t.TempDir()

	writeWorkdirConfig(t, workdir, "builtin", []string{
		"{{self}}", "sandbox", "run",
		"--profile", "default",
		"--", "{{inner_cmd}}", "{{inner_args}}",
	})

	// No "environment" block → empty allow_vars (the legacy inherit-all shape).
	stageProfile(t, home, `{
	  "meta": {"name": "default"}
	}`)

	env, outBuf, _, drain := newPipeEnv(t, "")
	env.Workdir = workdir
	code := runDoctor([]string{}, env)
	drain()
	output := outBuf.String()

	if code != ExitOK {
		t.Errorf("doctor exit = %d, want ExitOK (advisory)", code)
	}
	if !strings.Contains(output, "allow_vars") {
		t.Errorf("doctor output missing empty allow_vars warning; got:\n%s", output)
	}
	if !strings.Contains(strings.ToLower(output), "impact") ||
		!strings.Contains(strings.ToLower(output), "remediation") {
		t.Errorf("doctor output missing impact/remediation; got:\n%s", output)
	}
}

// TestDoctorNonEmptyAllowVarsNotWarned asserts the empty-allow_vars warning
// does NOT fire when the profile ships an explicit allowlist.
func TestDoctorNonEmptyAllowVarsNotWarned(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	workdir := t.TempDir()

	writeWorkdirConfig(t, workdir, "builtin", []string{
		"{{self}}", "sandbox", "run",
		"--profile", "default",
		"--", "{{inner_cmd}}", "{{inner_args}}",
	})

	stageProfile(t, home, `{
	  "meta": {"name": "default"},
	  "environment": {"allow_vars": ["HOME", "PATH"]}
	}`)

	env, outBuf, _, drain := newPipeEnv(t, "")
	env.Workdir = workdir
	_ = runDoctor([]string{}, env)
	drain()
	output := outBuf.String()

	if strings.Contains(output, "empty environment.allow_vars") {
		t.Errorf("doctor warned on a profile with an explicit allowlist; got:\n%s", output)
	}
}

func TestDoctorExplicitProfilePathInspected(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	workdir := t.TempDir()

	profilePath := filepath.Join(workdir, "my-profile.json")
	if err := os.WriteFile(profilePath, []byte(`{
	  "meta": {"name": "custom"},
	  "filesystem": {"allow": ["~/go"]}
	}`), 0o644); err != nil {
		t.Fatal(err)
	}

	writeWorkdirConfig(t, workdir, "builtin", []string{
		"{{self}}", "sandbox", "run",
		"--profile", profilePath,
		"--", "{{inner_cmd}}", "{{inner_args}}",
	})

	env, outBuf, _, drain := newPipeEnv(t, "")
	env.Workdir = workdir
	code := runDoctor([]string{}, env)
	drain()
	output := outBuf.String()

	if code != ExitOK {
		t.Errorf("doctor exit = %d, want ExitOK", code)
	}
	if !strings.Contains(output, "~/go") {
		t.Errorf("doctor should inspect explicit profile path; got:\n%s", output)
	}
}

func TestDoctorExistingProfileUnchanged(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	workdir := t.TempDir()

	writeWorkdirConfig(t, workdir, "builtin", []string{
		"{{self}}", "sandbox", "run",
		"--profile", "default",
		"--", "{{inner_cmd}}", "{{inner_args}}",
	})

	profileJSON := `{
	  "meta": {"name": "default"},
	  "filesystem": {"allow": ["~/.cargo"]}
	}`
	stageProfile(t, home, profileJSON)

	env, _, _, drain := newPipeEnv(t, "")
	env.Workdir = workdir
	_ = runDoctor([]string{}, env)
	drain()

	// The on-disk profile must be byte-identical after doctor runs.
	defaultPath := filepath.Join(home, ".config", "omac", "sandbox-profiles", "default.json")
	got, err := os.ReadFile(defaultPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != profileJSON {
		t.Errorf("doctor mutated the existing default profile:\nwant:\n%s\ngot:\n%s", profileJSON, got)
	}
	// And no default.json was scaffolded in a fresh home without one.
	freshHome := t.TempDir()
	t.Setenv("HOME", freshHome)
	env2, _, _, drain2 := newPipeEnv(t, "")
	env2.Workdir = workdir
	_ = runDoctor([]string{}, env2)
	drain2()
	if _, err := os.Stat(filepath.Join(freshHome, ".config", "omac", "sandbox-profiles", "default.json")); !os.IsNotExist(err) {
		t.Errorf("doctor scaffolded default.json in a fresh home; must not mutate")
	}
}

func TestDoctorWarningsExitCodeNeutral(t *testing.T) {
	home := stageHomeWithCargoSentinels(t)
	workdir := t.TempDir()

	writeWorkdirConfig(t, workdir, "builtin", []string{
		"{{self}}", "sandbox", "run",
		"--profile", "default",
		"--", "{{inner_cmd}}", "{{inner_args}}",
	})

	stageProfile(t, home, `{
	  "meta": {"name": "default"},
	  "filesystem": {
	    "allow": ["~/.cache", "~/.cargo", "~/.rustup", "~/go"]
	  }
	}`)

	env, _, _, drain := newPipeEnv(t, "")
	env.Workdir = workdir
	code := runDoctor([]string{}, env)
	drain()

	if code != ExitOK {
		t.Errorf("doctor exit = %d with warnings; must remain ExitOK (advisory)", code)
	}
}

// stageProfile writes a sandbox profile to
// ~/.config/omac/sandbox-profiles/<name>.json in the current HOME.
func stageProfile(t *testing.T, home, jsonContent string) {
	t.Helper()
	dir := filepath.Join(home, ".config", "omac", "sandbox-profiles")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "default.json"), []byte(jsonContent), 0o644); err != nil {
		t.Fatal(err)
	}
}
