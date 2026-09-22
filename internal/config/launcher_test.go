package config

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestSandboxBriefingParsesFromYAML(t *testing.T) {
	const in = `
sandbox:
  briefing: |
    custom sandbox note
`
	var lc LauncherConfig
	if err := yaml.Unmarshal([]byte(in), &lc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := "custom sandbox note\n"
	if lc.Sandbox.Briefing != want {
		t.Errorf("Sandbox.Briefing = %q; want %q", lc.Sandbox.Briefing, want)
	}
}

func TestSandboxBriefingEmptyWhenAbsent(t *testing.T) {
	var lc LauncherConfig
	if err := yaml.Unmarshal([]byte("sandbox: {}\n"), &lc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if lc.Sandbox.Briefing != "" {
		t.Errorf("Sandbox.Briefing = %q; want empty", lc.Sandbox.Briefing)
	}
}

func strPtr(s string) *string { return &s }

func TestValidateSandboxAcceptsAbsentLegacyFields(t *testing.T) {
	for _, sb := range []SandboxConfig{
		{},
		{ProfileName: "strict", Briefing: "note"},
	} {
		if err := validateSandbox(sb, "cfg", "", false); err != nil {
			t.Errorf("validateSandbox(%+v) = %v; want nil", sb, err)
		}
	}
}

func TestValidateSandboxRejectsDefaultProfile(t *testing.T) {
	for _, p := range []string{"", "builtin", "nono", "nono-netprofile", "custom", "no-sandbox-debug"} {
		err := validateSandbox(SandboxConfig{DefaultProfile: strPtr(p)}, "cfg", "", false)
		if err == nil {
			t.Errorf("default_profile %q should be rejected", p)
			continue
		}
		for _, want := range []string{"default_profile", `"` + p + `"`, "profile_name", "sandbox-profiles/<name>.json", "docs/configuration.md"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error for %q should contain %q: %v", p, want, err)
			}
		}
		if strings.Contains(err.Error(), ".omac/<name>.json") {
			t.Errorf("a global error must not point at the project location: %v", err)
		}
		if strings.Contains(err.Error(), "Profiles defined for this layer") {
			t.Errorf("an empty profile dir must not list profiles: %v", err)
		}
	}

	// no-sandbox-debug additionally points at the unsandboxed-shell escape.
	err := validateSandbox(SandboxConfig{DefaultProfile: strPtr("no-sandbox-debug")}, "cfg", "", false)
	if err == nil || !strings.Contains(err.Error(), "--no-sandbox --inner bash") {
		t.Errorf("no-sandbox-debug error should point at --no-sandbox --inner bash: %v", err)
	}
}

func TestValidateSandboxProjectLayerHint(t *testing.T) {
	err := validateSandbox(SandboxConfig{DefaultProfile: strPtr("builtin")}, "cfg", "", true)
	if err == nil {
		t.Fatal("a present default_profile must be rejected")
	}
	if !strings.Contains(err.Error(), "<workdir>/.omac/<name>.json") {
		t.Errorf("a project error should point at the project location: %v", err)
	}
	if strings.Contains(err.Error(), "sandbox-profiles/<name>.json") {
		t.Errorf("a project error must not point at the global location: %v", err)
	}
}

// TestValidateSandboxSuggestsRenameWhenProfileExists: the referenced profile
// exists in the declaring layer's directory, so the hint offers the rename
// plus the layer's alternatives instead of just "remove the line".
func TestValidateSandboxSuggestsRenameWhenProfileExists(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"tng-default", "work"} {
		writeFile(t, filepath.Join(dir, name+".json"), "{}")
	}
	writeFile(t, filepath.Join(dir, "tng-default.pages.json"), "{}") // pages sibling, unlisted
	writeFile(t, filepath.Join(dir, "default.json"), "{}")           // implicit, unlisted
	if err := os.Symlink(filepath.Join(dir, "work.json"), filepath.Join(dir, "broken.json")); err == nil {
		// symlinked profiles are unusable and must stay unlisted (best effort)
		defer os.Remove(filepath.Join(dir, "broken.json"))
	}

	err := validateSandbox(SandboxConfig{DefaultProfile: strPtr("tng-default")}, "cfg", dir, false)
	if err == nil {
		t.Fatal("a present default_profile must be rejected")
	}
	out := err.Error()
	for _, want := range []string{
		`"tng-default"`,
		"Profiles defined for this layer: tng-default, work",
		"- Keep using your previous one by renaming the field:",
		`sandbox:`,
		`profile_name: "tng-default"`,
		"- Or use one of the other profiles defined for this layer: work",
		"- Or remove the line to fall back to this layer's default.json.",
		"docs/configuration.md",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rename hint should contain %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "pages") || strings.Contains(out, `broken"`) {
		t.Errorf("the profile list must exclude pages files and symlinks:\n%s", out)
	}
}

// TestValidateSandboxListsAlternativesWithoutRename: the referenced name is
// not among the layer's profiles, so no "keep using" bullet may appear.
func TestValidateSandboxListsAlternativesWithoutRename(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "strict.json"), "{}")

	err := validateSandbox(SandboxConfig{DefaultProfile: strPtr("tng-default")}, "cfg", dir, false)
	if err == nil {
		t.Fatal("a present default_profile must be rejected")
	}
	out := err.Error()
	for _, want := range []string{
		"Profiles defined for this layer: strict",
		"- Or use one of the profiles defined for this layer: strict",
		"- Or remove the line to fall back to this layer's default.json.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("alternatives hint should contain %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Keep using your previous one") {
		t.Errorf("without a matching profile the rename bullet must not appear:\n%s", out)
	}
}

func TestListProfileNames(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "work.json"), "{}")
	writeFile(t, filepath.Join(dir, "a.json"), "{}")
	writeFile(t, filepath.Join(dir, "default.json"), "{}")
	writeFile(t, filepath.Join(dir, "a.pages.json"), "{}")
	writeFile(t, filepath.Join(dir, "notjson.yaml"), "{}")

	if got, want := listProfileNames(dir), []string{"a", "work"}; !slices.Equal(got, want) {
		t.Errorf("listProfileNames = %v; want %v", got, want)
	}
	if got := listProfileNames(""); got != nil {
		t.Errorf("listProfileNames(\"\") = %v; want nil", got)
	}
	if got := listProfileNames(t.TempDir()); got != nil {
		t.Errorf("listProfileNames(missing dir) = %v; want nil", got)
	}
}

func TestJoinProfileNamesCapsDisplay(t *testing.T) {
	var names []string
	for i := 0; i < 8; i++ {
		names = append(names, fmt.Sprintf("p%d", i))
	}
	got := joinProfileNames(names)
	if !strings.Contains(got, "p4") || strings.Contains(got, "p5") || !strings.Contains(got, "and 3 more") {
		t.Errorf("joinProfileNames should show 5 names and the remainder; got %q", got)
	}
}

func TestValidateSandboxRejectsProfilesPresent(t *testing.T) {
	for name, sb := range map[string]SandboxConfig{
		"empty":     {Profiles: map[string]any{}},
		"non-empty": {Profiles: map[string]any{"x": nil}},
	} {
		err := validateSandbox(sb, "cfg", "", false)
		if err == nil {
			t.Errorf("%s profiles block should be rejected", name)
			continue
		}
		for _, want := range []string{"profiles", "profile_name", "sandbox-profiles/<name>.json", "docs/configuration.md"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: error should contain %q: %v", name, want, err)
			}
		}
	}
}

// A launcher config that still carries a legacy field fails both layers'
// loaders with the layer-appropriate hint, including the (soon) renamed-hint
// shape.
func TestResolveSandboxProfileLegacyDefaultProfileError(t *testing.T) {
	isolateHome(t)
	globalDir := filepath.Join(os.Getenv("HOME"), ".config", "omac", "sandbox-profiles")
	writeFile(t, filepath.Join(globalDir, "tng-default.json"), "{}")
	writeFile(t, GlobalLauncherConfigPath(), "sandbox:\n  default_profile: tng-default\n")

	_, err := ResolveSandboxProfile(t.TempDir())
	if err == nil {
		t.Fatal("a global default_profile referencing an existing profile must fail the selection too")
	}
	for _, want := range []string{"Profiles defined for this layer", "profile_name: \"tng-default\""} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("selection error should contain %q: %v", want, err)
		}
	}
}

func TestSandboxDefaultProfileParsesAsPointerPresent(t *testing.T) {
	var lc LauncherConfig
	if err := yaml.Unmarshal([]byte("sandbox:\n  default_profile: builtin\n"), &lc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if lc.Sandbox.DefaultProfile == nil {
		t.Fatal("a present default_profile must parse as a non-nil pointer")
	}
	if *lc.Sandbox.DefaultProfile != "builtin" {
		t.Errorf("DefaultProfile = %q; want builtin", *lc.Sandbox.DefaultProfile)
	}

	var absent LauncherConfig
	if err := yaml.Unmarshal([]byte("sandbox: {}\n"), &absent); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if absent.Sandbox.DefaultProfile != nil {
		t.Error("an absent default_profile must parse as nil")
	}
}

func TestSandboxProfileNameParsesFromYAML(t *testing.T) {
	const in = "sandbox:\n  profile_name: strict\n"
	var lc LauncherConfig
	if err := yaml.Unmarshal([]byte(in), &lc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if lc.Sandbox.ProfileName != "strict" {
		t.Errorf("Sandbox.ProfileName = %q; want %q", lc.Sandbox.ProfileName, "strict")
	}
}

// isolateHome points HOME (and thus the global config tree) at a temp dir.
func isolateHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestResolveSandboxProfileLocalNameWins(t *testing.T) {
	isolateHome(t)
	workdir := t.TempDir()
	writeFile(t, filepath.Join(workdir, ".omac", "config.yaml"), "sandbox:\n  profile_name: strict\n")
	local := filepath.Join(workdir, ".omac", "strict.json")
	writeFile(t, local, "{}")
	// A global profile of the same name must NOT be picked.
	globalDir := filepath.Join(os.Getenv("HOME"), ".config", "omac", "sandbox-profiles")
	writeFile(t, filepath.Join(globalDir, "strict.json"), "{}")

	sel, err := ResolveSandboxProfile(workdir)
	if err != nil {
		t.Fatalf("ResolveSandboxProfile: %v", err)
	}
	if sel.Path != local || sel.Layer != "workdir" || sel.Name != "strict" {
		t.Errorf("selection = %+v; want path %q layer local name strict", sel, local)
	}
}

func TestResolveSandboxProfileLocalDefaultBeatsGlobalName(t *testing.T) {
	isolateHome(t)
	workdir := t.TempDir()
	local := filepath.Join(workdir, ".omac", "default.json")
	writeFile(t, local, "{}")
	globalDir := filepath.Join(os.Getenv("HOME"), ".config", "omac", "sandbox-profiles")
	writeFile(t, filepath.Join(globalDir, "strict.json"), "{}")
	writeFile(t, filepath.Join(os.Getenv("HOME"), ".config", "omac", "config.yaml"), "sandbox:\n  profile_name: strict\n")

	sel, err := ResolveSandboxProfile(workdir)
	if err != nil {
		t.Fatalf("ResolveSandboxProfile: %v", err)
	}
	if sel.Path != local || sel.Layer != "workdir" {
		t.Errorf("selection = %+v; want the local default %q", sel, local)
	}
}

func TestResolveSandboxProfileGlobalFallback(t *testing.T) {
	isolateHome(t)
	workdir := t.TempDir()
	globalDir := filepath.Join(os.Getenv("HOME"), ".config", "omac", "sandbox-profiles")
	global := filepath.Join(globalDir, "strict.json")
	writeFile(t, global, "{}")
	writeFile(t, filepath.Join(os.Getenv("HOME"), ".config", "omac", "config.yaml"), "sandbox:\n  profile_name: strict\n")

	sel, err := ResolveSandboxProfile(workdir)
	if err != nil {
		t.Fatalf("ResolveSandboxProfile: %v", err)
	}
	if sel.Path != global || sel.Layer != "global" {
		t.Errorf("selection = %+v; want global %q", sel, global)
	}
}

func TestResolveSandboxProfileBuiltinDefault(t *testing.T) {
	isolateHome(t)
	sel, err := ResolveSandboxProfile(t.TempDir())
	if err != nil {
		t.Fatalf("ResolveSandboxProfile: %v", err)
	}
	if sel.Path != "" || sel.Layer != "builtin" {
		t.Errorf("selection = %+v; want builtin with empty path", sel)
	}
}

func TestResolveSandboxProfileMissingNameIsError(t *testing.T) {
	isolateHome(t)
	workdir := t.TempDir()
	writeFile(t, filepath.Join(workdir, ".omac", "config.yaml"), "sandbox:\n  profile_name: nope\n")
	if _, err := ResolveSandboxProfile(workdir); err == nil {
		t.Fatal("a profile_name that does not exist should be an error, not a silent fallback")
	}
}

func TestResolveSandboxProfileRejectsNameTraversal(t *testing.T) {
	isolateHome(t)
	workdir := t.TempDir()
	writeFile(t, filepath.Join(workdir, ".omac", "config.yaml"), "sandbox:\n  profile_name: ../evil\n")
	if _, err := ResolveSandboxProfile(workdir); err == nil {
		t.Fatal("a profile_name with a path separator must be rejected")
	}
}

func TestExplicitProfileSelectionContainment(t *testing.T) {
	isolateHome(t)
	workdir := t.TempDir()

	globalDir := filepath.Join(os.Getenv("HOME"), ".config", "omac", "sandbox-profiles")
	global := filepath.Join(globalDir, "g.json")
	writeFile(t, global, "{}")
	local := filepath.Join(workdir, ".omac", "l.json")
	writeFile(t, local, "{}")

	for _, tc := range []struct {
		name, path, layer string
	}{
		{"global", global, "global"},
		{"workdir", local, "workdir"},
	} {
		sel, err := ExplicitProfileSelection(workdir, tc.path)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if sel.Path != tc.path || sel.Layer != tc.layer {
			t.Errorf("%s: sel = %+v; want path %q layer %q", tc.name, sel, tc.path, tc.layer)
		}
	}

	outside := filepath.Join(t.TempDir(), "evil.json")
	writeFile(t, outside, "{}")
	if _, err := ExplicitProfileSelection(workdir, outside); err == nil {
		t.Error("a --profile-path outside the trusted dirs must be rejected")
	}
}

func TestResolveSandboxProfileRejectsSymlinkedDefault(t *testing.T) {
	isolateHome(t)
	workdir := t.TempDir()
	real := filepath.Join(t.TempDir(), "real.json")
	writeFile(t, real, "{}")
	link := filepath.Join(workdir, ".omac", "default.json")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := ResolveSandboxProfile(workdir); err == nil {
		t.Fatal("a symlinked .omac/default.json must be rejected, not auto-selected")
	}
}

func TestExplicitProfileSelectionRelativeAnchorsWorkdir(t *testing.T) {
	isolateHome(t)
	workdir := t.TempDir()
	local := filepath.Join(workdir, ".omac", "strict.json")
	writeFile(t, local, "{}")

	sel, err := ExplicitProfileSelection(workdir, filepath.Join(".omac", "strict.json"))
	if err != nil {
		t.Fatalf("ExplicitProfileSelection: %v", err)
	}
	if sel.Path != local || sel.Layer != "workdir" {
		t.Errorf("selection = %+v; want path %q layer workdir", sel, local)
	}

	// A relative path needs a workdir to anchor on.
	if _, err := ExplicitProfileSelection("", "strict.json"); err == nil {
		t.Error("a relative --profile-path without a workdir must error")
	}
}

func TestExplicitProfileSelectionSymlinkIsError(t *testing.T) {
	isolateHome(t)
	workdir := t.TempDir()
	real := filepath.Join(workdir, ".omac", "real.json")
	writeFile(t, real, "{}")
	link := filepath.Join(workdir, ".omac", "link.json")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := ExplicitProfileSelection(workdir, link); err == nil {
		t.Fatal("a symlinked --profile-path must be rejected")
	}
}

func TestProjectLauncherConfigPathIsOmacDir(t *testing.T) {
	if got, want := ProjectLauncherConfigPath("/w"), filepath.Join("/w", ".omac", "config.yaml"); got != want {
		t.Errorf("ProjectLauncherConfigPath = %q; want %q", got, want)
	}
}
