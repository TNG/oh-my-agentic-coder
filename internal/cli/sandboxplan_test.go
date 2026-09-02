package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/tngtech/oh-my-agentic-coder/internal/config"
)

func TestResolveSandboxPlanDefaultProfileResolvesPolicy(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	plan, err := resolveSandboxPlan(config.DefaultLauncherConfig(), "")
	if err != nil {
		t.Fatalf("resolveSandboxPlan: %v", err)
	}
	// The two namespaces: launcher name "builtin", policy ref "default".
	if plan.Name != "builtin" {
		t.Errorf("Name = %q, want builtin (the launcher profile)", plan.Name)
	}
	if plan.PolicyRef != "default" {
		t.Errorf("PolicyRef = %q, want default (the policy profile)", plan.PolicyRef)
	}
	if !plan.Known {
		t.Errorf("Known = %v; want true", plan.Known)
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

	plan, err := resolveSandboxPlan(config.DefaultLauncherConfig(), "")
	if err != nil {
		t.Fatalf("resolveSandboxPlan: %v", err)
	}
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
	plan, err := resolveSandboxPlan(config.DefaultLauncherConfig(), custom)
	if err != nil {
		t.Fatalf("resolveSandboxPlan: %v", err)
	}
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

// A broken default policy file is NOT fatal: the launch proceeds (the
// `omac sandbox run` child resolves the policy itself and reports), but the
// error is recorded so policy-derived facade features can be disabled with
// an accurate message.
func TestResolveSandboxPlanBrokenPolicyIsRecordedNotFatal(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stageProfile(t, home, `{ not valid json`)

	plan, err := resolveSandboxPlan(config.DefaultLauncherConfig(), "")
	if err != nil {
		t.Fatalf("a broken policy must not fail the plan: %v", err)
	}
	if plan.PolicyErr == nil {
		t.Error("PolicyErr should record the failed policy resolution")
	}
	if plan.Policy != nil {
		t.Error("Policy must be nil when resolution failed")
	}
}
