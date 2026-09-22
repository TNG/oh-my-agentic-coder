package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

func TestResolveSandboxPlanDefaultProfileResolvesPolicy(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	plan := resolveSandboxPlan("", config.ProfileSelection{})
	if plan.PolicyRef != "default" {
		t.Errorf("PolicyRef = %q, want default", plan.PolicyRef)
	}
	if plan.PolicyErr != nil {
		t.Errorf("PolicyErr = %v; the default policy must resolve", plan.PolicyErr)
	}
	if plan.Policy == nil {
		t.Fatal("Policy is nil; the default policy must resolve")
	}
	if plan.PolicyPath != "" {
		t.Errorf("PolicyPath = %q; a fresh home has no file, want the compiled-in defaults", plan.PolicyPath)
	}
	// Read-only: resolving must not scaffold the user's profile file.
	defaultPath := filepath.Join(home, ".config", "omac", "sandbox-profiles", "default.json")
	if _, err := os.Stat(defaultPath); !os.IsNotExist(err) {
		t.Errorf("resolving a plan scaffolded %s (err=%v); must be read-only", defaultPath, err)
	}
}

func TestResolveSandboxPlanLoadsPolicyFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stageProfile(t, home, `{"meta": {"name": "default"}, "workdir": {"access": "read"}}`)

	plan := resolveSandboxPlan("", config.ProfileSelection{})
	if plan.Policy == nil || plan.Policy.Workdir.Access != "read" {
		t.Fatalf("staged policy file must win; got %+v", plan.Policy)
	}
	want := filepath.Join(home, ".config", "omac", "sandbox-profiles", "default.json")
	if plan.PolicyPath != want {
		t.Errorf("PolicyPath = %q, want %q", plan.PolicyPath, want)
	}
}

// A non-empty selection path (the resolved sandbox profile) is the policy the
// plan enforces, loaded from that exact path rather than the default.
func TestResolveSandboxPlanUsesProfileRef(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	workdir := t.TempDir()
	custom := filepath.Join(config.LocalConfigDir(workdir), "custom.json")
	if err := os.MkdirAll(filepath.Dir(custom), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(custom, []byte(`{"meta": {"name": "custom"}, "workdir": {"access": "read"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	plan := resolveSandboxPlan(workdir, config.ProfileSelection{Path: custom, Name: "custom", Layer: "workdir"})
	if plan.PolicyRef != custom {
		t.Errorf("PolicyRef = %q, want %q", plan.PolicyRef, custom)
	}
	if plan.PolicyPath != custom {
		t.Errorf("PolicyPath = %q, want %q", plan.PolicyPath, custom)
	}
	if plan.Policy == nil || plan.Policy.Workdir.Access != "read" {
		t.Fatalf("custom policy must load; got %+v", plan.Policy)
	}
	// The parent seeds env forwarding from plan.Policy (forwardHarnessEnv), so
	// it must reflect the custom file — not the default's non-empty allow_vars.
	if len(plan.Policy.Environment.AllowVars) != 0 {
		t.Errorf("AllowVars = %v; the custom profile declares none, so the plan must not show the default's", plan.Policy.Environment.AllowVars)
	}
}

func TestWarnPermissiveProfile(t *testing.T) {
	// A profile that reaches a cloud metadata endpoint is a HIGH finding.
	p := &sandboxprofile.Profile{
		Environment: sandboxprofile.Environment{AllowVars: []string{"PATH"}},
		Network:     sandboxprofile.Network{AllowDomain: []string{"169.254.169.254"}},
	}
	var buf bytes.Buffer
	warnPermissiveProfile(&buf, "/repo/custom.json", p)
	if !strings.Contains(buf.String(), "169.254.169.254") {
		t.Errorf("expected a finding about the metadata endpoint, got:\n%s", buf.String())
	}

	// The default profile (ref == "") is not linted here.
	buf.Reset()
	warnPermissiveProfile(&buf, "", p)
	if buf.Len() != 0 {
		t.Errorf("empty ref should be a no-op, got:\n%s", buf.String())
	}

	// A nil policy is a no-op.
	buf.Reset()
	warnPermissiveProfile(&buf, "/repo/custom.json", nil)
	if buf.Len() != 0 {
		t.Errorf("nil policy should be a no-op, got:\n%s", buf.String())
	}
}

func TestExcludeProfilePagesFile(t *testing.T) {
	workdir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workdir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	// A profile inside the workdir: its .pages.json sibling is git-excluded.
	excludeProfilePagesFile(workdir, filepath.Join(workdir, "sandbox.json"))
	data, err := os.ReadFile(filepath.Join(workdir, ".git", "info", "exclude"))
	if err != nil {
		t.Fatalf("exclude file not written: %v", err)
	}
	if !strings.Contains(string(data), "sandbox.pages.json") {
		t.Errorf("exclude should list the pages file, got: %q", data)
	}

	// A profile outside the workdir is not excluded.
	excludeProfilePagesFile(workdir, filepath.Join(t.TempDir(), "external.json"))
	data, _ = os.ReadFile(filepath.Join(workdir, ".git", "info", "exclude"))
	if strings.Contains(string(data), "external.pages.json") {
		t.Errorf("a profile outside the workdir must not be excluded, got: %q", data)
	}
}

func TestProfileRefFromConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	// An explicit --profile flag value wins.
	got, err := profileRefFromConfig(t.TempDir(), "/x/custom.json")
	if err != nil || got != "/x/custom.json" {
		t.Errorf("flag ref should win, got (%q, %v)", got, err)
	}

	// No config on disk resolves to the built-in default ("").
	if got, err = profileRefFromConfig(t.TempDir(), ""); err != nil || got != "" {
		t.Errorf("no config should resolve to default (empty), got (%q, %v)", got, err)
	}

	// A project config with profile_name resolves to that .omac path.
	workdir := t.TempDir()
	prof := filepath.Join(config.LocalConfigDir(workdir), "strict.json")
	if err := os.MkdirAll(filepath.Dir(prof), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(prof, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.ProjectLauncherConfigPath(workdir),
		[]byte("sandbox:\n  profile_name: strict\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err = profileRefFromConfig(workdir, ""); err != nil || got != prof {
		t.Errorf("profileRefFromConfig = (%q, %v), want (%q, nil)", got, err, prof)
	}

	// A broken profile_name surfaces its error instead of silently
	// falling back to the default.
	if err := os.WriteFile(config.ProjectLauncherConfigPath(workdir),
		[]byte("sandbox:\n  profile_name: missing\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err = profileRefFromConfig(workdir, ""); err == nil {
		t.Error("a missing profile_name should return an error, got nil")
	}
}

func TestActiveProfileSelectionCLIPathWins(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	workdir := t.TempDir()
	local := filepath.Join(config.LocalConfigDir(workdir), "strict.json")
	if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(local, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	sel, err := activeProfileSelection(workdir, local)
	if err != nil {
		t.Fatalf("activeProfileSelection: %v", err)
	}
	if sel.Path != local || sel.Layer != "workdir" {
		t.Errorf("selection = %+v; want path %q layer local", sel, local)
	}
	if _, err := activeProfileSelection(workdir, filepath.Join(t.TempDir(), "evil.json")); err == nil {
		t.Error("a --profile-path outside the trusted dirs must be rejected")
	}
}

func TestRejectLegacySandboxFlag(t *testing.T) {
	env, _, errBuf, drain := newPipeEnv(t, "")
	if !rejectLegacySandboxFlag("start", []string{"--sandbox", "builtin"}, env) {
		t.Fatal("--sandbox must be rejected with a migration error")
	}
	drain()
	if out := errBuf.String(); !strings.Contains(out, "0.10.0") || !strings.Contains(out, "--profile-path") {
		t.Errorf("migration error should name the release and the replacement; got:\n%s", out)
	}
}

// A broken default policy file is NOT fatal: the launch proceeds (the
// `omac sandbox run` child resolves the policy itself and reports), but the
// error is recorded so policy-derived facade features can be disabled with
// an accurate message.
func TestResolveSandboxPlanBrokenPolicyIsRecordedNotFatal(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stageProfile(t, home, `{ not valid json`)

	plan := resolveSandboxPlan("", config.ProfileSelection{})
	if plan.PolicyErr == nil {
		t.Error("PolicyErr should record the failed policy resolution")
	}
	if plan.Policy != nil {
		t.Error("Policy must be nil when resolution failed")
	}
}
