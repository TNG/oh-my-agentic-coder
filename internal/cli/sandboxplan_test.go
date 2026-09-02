package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tngtech/oh-my-agentic-coder/internal/sandboxprofile"
)

func TestResolveSandboxPlanDefaultProfileResolvesPolicy(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	plan := resolveSandboxPlan("")
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

	plan := resolveSandboxPlan("")
	if plan.Policy == nil || plan.Policy.Workdir.Access != "read" {
		t.Fatalf("staged policy file must win; got %+v", plan.Policy)
	}
	want := filepath.Join(home, ".config", "omac", "sandbox-profiles", "default.json")
	if plan.PolicyPath != want {
		t.Errorf("PolicyPath = %q, want %q", plan.PolicyPath, want)
	}
}

// A non-empty profileRef (the resolved sandbox.profile_path) is the policy the
// plan enforces, loaded from that exact path rather than the default.
func TestResolveSandboxPlanUsesProfileRef(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	custom := filepath.Join(t.TempDir(), "custom.json")
	if err := os.WriteFile(custom, []byte(`{"meta": {"name": "custom"}, "workdir": {"access": "read"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	plan := resolveSandboxPlan(custom)
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

// A broken default policy file is NOT fatal: the launch proceeds (the
// `omac sandbox run` child resolves the policy itself and reports), but the
// error is recorded so policy-derived facade features can be disabled with
// an accurate message.
func TestResolveSandboxPlanBrokenPolicyIsRecordedNotFatal(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stageProfile(t, home, `{ not valid json`)

	plan := resolveSandboxPlan("")
	if plan.PolicyErr == nil {
		t.Error("PolicyErr should record the failed policy resolution")
	}
	if plan.Policy != nil {
		t.Error("Policy must be nil when resolution failed")
	}
}
