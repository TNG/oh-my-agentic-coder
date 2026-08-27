package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stageDoctorNpmrc stages a HOME + workdir + default profile and writes the
// given npmrc body, then runs doctor and returns its output. profileJSON is
// the sandbox profile the launcher's default template points at.
func stageDoctorNpmrc(t *testing.T, npmrc, profileJSON string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	workdir := t.TempDir()

	writeWorkdirConfig(t, workdir, "builtin", []string{
		"{{self}}", "sandbox", "run",
		"--profile", "default",
		"--", "{{inner_cmd}}", "{{inner_args}}",
	})
	stageProfile(t, home, profileJSON)

	if npmrc != "" {
		if err := os.WriteFile(filepath.Join(home, ".npmrc"), []byte(npmrc), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	env, outBuf, _, drain := newPipeEnv(t, "")
	env.Workdir = workdir
	if code := runDoctor([]string{}, env); code != ExitOK {
		t.Errorf("doctor exit = %d, want ExitOK (advisory)", code)
	}
	drain()
	return outBuf.String()
}

// TestDoctorRegistryConfigWarnsOnInvisibleMapping is the #241 shape: a scope
// mapped to a private registry that the sandbox cannot read.
func TestDoctorRegistryConfigWarnsOnInvisibleMapping(t *testing.T) {
	out := stageDoctorNpmrc(t,
		"@acme:registry=https://npm.acme.test\n",
		`{"meta": {"name": "default"}, "environment": {"allow_vars": ["HOME"]}}`)

	if !strings.Contains(out, "registry config") || !strings.Contains(out, "npm.acme.test") {
		t.Errorf("doctor did not report the invisible mapping; got:\n%s", out)
	}
	if !strings.Contains(out, "404") {
		t.Errorf("doctor did not explain the 404 symptom; got:\n%s", out)
	}
	if !strings.Contains(out, `registry_config: ["npm"]`) {
		t.Errorf("doctor did not name the fix; got:\n%s", out)
	}
}

// TestDoctorRegistryConfigQuietWhenEnabled asserts the check confirms rather
// than nags once the profile opts in.
func TestDoctorRegistryConfigQuietWhenEnabled(t *testing.T) {
	out := stageDoctorNpmrc(t,
		"@acme:registry=https://npm.acme.test\n",
		`{"meta": {"name": "default"}, "filesystem": {"registry_config": ["npm"]}, "environment": {"allow_vars": ["HOME"]}}`)

	if !strings.Contains(out, "[ok] registry config") {
		t.Errorf("doctor did not confirm the projection; got:\n%s", out)
	}
	if strings.Contains(out, "404") {
		t.Errorf("doctor still warned with registry_config enabled; got:\n%s", out)
	}
}

// TestDoctorRegistryConfigFlagsOverrideDenyExposure asserts the blunt remedy
// is called out as exposing the token the projection would have dropped.
func TestDoctorRegistryConfigFlagsOverrideDenyExposure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	workdir := t.TempDir()
	writeWorkdirConfig(t, workdir, "builtin", []string{
		"{{self}}", "sandbox", "run",
		"--profile", "default",
		"--", "{{inner_cmd}}", "{{inner_args}}",
	})
	// override_deny is matched on the expanded path, so "~/.npmrc" resolves
	// to the staged HOME.
	stageProfile(t, home, `{
	  "meta": {"name": "default"},
	  "filesystem": {"override_deny": ["~/.npmrc"]},
	  "environment": {"allow_vars": ["HOME"]}
	}`)
	body := "@acme:registry=https://npm.acme.test\n//npm.acme.test/:_authToken=SECRET\n"
	if err := os.WriteFile(filepath.Join(home, ".npmrc"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	env, outBuf, _, drain := newPipeEnv(t, "")
	env.Workdir = workdir
	runDoctor([]string{}, env)
	drain()
	out := outBuf.String()

	if !strings.Contains(out, "override_deny") {
		t.Errorf("doctor did not mention the override_deny exposure; got:\n%s", out)
	}
	if !strings.Contains(out, "auth token") {
		t.Errorf("doctor did not warn that the token is exposed; got:\n%s", out)
	}
	if strings.Contains(out, "SECRET") {
		t.Fatalf("doctor echoed secret material; got:\n%s", out)
	}
}

// TestDoctorRegistryConfigSilentWithoutPrivateMapping keeps the check from
// becoming noise: no npmrc, or one that only points at npm's own registry,
// must produce no registry-config line at all.
func TestDoctorRegistryConfigSilentWithoutPrivateMapping(t *testing.T) {
	profile := `{"meta": {"name": "default"}, "environment": {"allow_vars": ["HOME"]}}`

	t.Run("no npmrc", func(t *testing.T) {
		out := stageDoctorNpmrc(t, "", profile)
		if strings.Contains(out, "registry config") {
			t.Errorf("reported a registry-config finding with no npmrc; got:\n%s", out)
		}
	})

	t.Run("default registry only", func(t *testing.T) {
		out := stageDoctorNpmrc(t, "registry=https://registry.npmjs.org\n", profile)
		if strings.Contains(out, "registry config") {
			t.Errorf("reported a finding for the default registry; got:\n%s", out)
		}
	})

	t.Run("credentials only", func(t *testing.T) {
		out := stageDoctorNpmrc(t, "//registry.npmjs.org/:_authToken=SECRET\n", profile)
		if strings.Contains(out, "registry config") {
			t.Errorf("reported a finding with no mapping to project; got:\n%s", out)
		}
	})
}

// TestDoctorRegistryConfigReportsBothProjectionAndOverride is the review
// finding: with registry_config AND override_deny set, doctor printed only
// the reassuring "[ok] … projected" line and never mentioned that the real
// token-bearing file is still readable by the sandbox.
func TestDoctorRegistryConfigReportsBothProjectionAndOverride(t *testing.T) {
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
	  "filesystem": {"registry_config": ["npm"], "override_deny": ["~/.npmrc"]},
	  "environment": {"allow_vars": ["HOME"]}
	}`)
	body := "@acme:registry=https://npm.acme.test\n//npm.acme.test/:_authToken=SECRET\n"
	if err := os.WriteFile(filepath.Join(home, ".npmrc"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	env, outBuf, _, drain := newPipeEnv(t, "")
	env.Workdir = workdir
	runDoctor([]string{}, env)
	drain()
	out := outBuf.String()

	if !strings.Contains(out, "[ok] registry config") {
		t.Errorf("did not confirm the projection; got:\n%s", out)
	}
	if !strings.Contains(out, "override_deny") || !strings.Contains(out, "redundant") {
		t.Errorf("did not flag the redundant, token-exposing override; got:\n%s", out)
	}
	if strings.Contains(out, "SECRET") {
		t.Fatalf("doctor echoed secret material; got:\n%s", out)
	}
}

// TestDoctorRegistryConfigReportsUnusableMapping keeps doctor from staying
// silent when the npmrc has private-registry config omac cannot project.
func TestDoctorRegistryConfigReportsUnusableMapping(t *testing.T) {
	out := stageDoctorNpmrc(t,
		"@acme:registry=https://npm.acme.test/api/?apiKey=SECRET\n",
		`{"meta": {"name": "default"}, "environment": {"allow_vars": ["HOME"]}}`)

	if !strings.Contains(out, "cannot be projected") {
		t.Errorf("did not report the unusable mapping; got:\n%s", out)
	}
	if strings.Contains(out, "SECRET") {
		t.Fatalf("doctor echoed the secret; got:\n%s", out)
	}
}

// TestDoctorRegistryConfigReportsUnreadableConfig covers the review finding
// that the launch path warns about an unreadable ~/.npmrc while doctor — the
// one place that can warn *before* a run — printed nothing.
func TestDoctorRegistryConfigReportsUnreadableConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	workdir := t.TempDir()
	writeWorkdirConfig(t, workdir, "builtin", []string{
		"{{self}}", "sandbox", "run",
		"--profile", "default",
		"--", "{{inner_cmd}}", "{{inner_args}}",
	})
	stageProfile(t, home, `{"meta": {"name": "default"}, "environment": {"allow_vars": ["HOME"]}}`)
	// A directory where the file belongs makes the read fail with something
	// other than IsNotExist.
	if err := os.Mkdir(filepath.Join(home, ".npmrc"), 0o755); err != nil {
		t.Fatal(err)
	}

	env, outBuf, _, drain := newPipeEnv(t, "")
	env.Workdir = workdir
	if code := runDoctor([]string{}, env); code != ExitOK {
		t.Errorf("doctor exit = %d, want ExitOK (advisory)", code)
	}
	drain()
	out := outBuf.String()

	if !strings.Contains(out, "cannot inspect") {
		t.Errorf("doctor stayed silent on an unreadable npmrc; got:\n%s", out)
	}
	if !strings.Contains(out, "404") {
		t.Errorf("doctor did not explain the consequence; got:\n%s", out)
	}
}
