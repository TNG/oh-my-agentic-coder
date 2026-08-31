package cli

import (
	"os"
	"path/filepath"
	"strings"
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
	if !plan.Known || !plan.Native {
		t.Errorf("Known = %v, Native = %v; want both true", plan.Known, plan.Native)
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

// Opaque launchers (external sandbox binaries, the no-sandbox debug shell)
// have no inspectable omac policy: Native is false and no policy is resolved.
func TestResolveSandboxPlanOpaqueLaunchersHaveNoPolicy(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, name := range []string{"no-sandbox-debug"} {
		t.Run(name, func(t *testing.T) {
			plan, err := resolveSandboxPlan(config.DefaultLauncherConfig(), name)
			if err != nil {
				t.Fatalf("resolveSandboxPlan(%q): %v", name, err)
			}
			if !plan.Known {
				t.Errorf("%q is a configured launcher profile; Known should be true", name)
			}
			if plan.Native {
				t.Errorf("%q must not be classified as omac's native sandbox", name)
			}
			if plan.Policy != nil || plan.PolicyRef != "" || plan.PolicyErr != nil {
				t.Errorf("%q must resolve no policy; got ref=%q policy=%v err=%v",
					name, plan.PolicyRef, plan.Policy, plan.PolicyErr)
			}
		})
	}
}

func TestResolveSandboxPlanUnknownProfileErrors(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	plan, err := resolveSandboxPlan(config.DefaultLauncherConfig(), "nosuch")
	if err == nil {
		t.Fatal("expected an error for an unknown launcher profile")
	}
	if !strings.Contains(err.Error(), "nosuch") {
		t.Errorf("error should name the profile: %v", err)
	}
	// The name is still reported so callers can mention it, but nothing
	// downstream may treat the profile as usable.
	if plan.Name != "nosuch" || plan.Known || plan.Native {
		t.Errorf("plan = %+v; want Name=nosuch, Known=false, Native=false", plan)
	}
}

// A launcher profile that spells its policy ref differently (inline form,
// or a non-default name) must be followed, not assumed.
func TestResolveSandboxPlanFollowsPolicyRefInTemplate(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".config", "omac", "sandbox-profiles")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "strict.json"),
		[]byte(`{"meta": {"name": "strict"}, "workdir": {"access": "none"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	lc := config.LauncherConfig{Sandbox: config.SandboxConfig{
		DefaultProfile: "inline",
		Profiles: map[string]config.SandboxProfile{"inline": {
			Command: []string{"{{self}}", "sandbox", "run", "--profile=strict", "--", "{{inner_cmd}}"},
		}},
	}}

	plan, err := resolveSandboxPlan(lc, "")
	if err != nil {
		t.Fatalf("resolveSandboxPlan: %v", err)
	}
	if plan.PolicyRef != "strict" {
		t.Errorf("PolicyRef = %q, want strict", plan.PolicyRef)
	}
	if plan.Policy == nil || plan.Policy.Meta.Name != "strict" {
		t.Fatalf("expected the strict policy to be loaded; got %+v", plan.Policy)
	}
}

// A native launcher pointing at a policy that does not exist is NOT fatal:
// the launch proceeds (the `omac sandbox run` child resolves the policy
// itself and reports), but the error is recorded so policy-derived facade
// features can be disabled with an accurate message.
func TestResolveSandboxPlanMissingPolicyIsRecordedNotFatal(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	lc := config.LauncherConfig{Sandbox: config.SandboxConfig{
		DefaultProfile: "builtin",
		Profiles: map[string]config.SandboxProfile{"builtin": {
			Command: []string{"{{self}}", "sandbox", "run", "--profile", "nosuch", "--", "{{inner_cmd}}"},
		}},
	}}

	plan, err := resolveSandboxPlan(lc, "")
	if err != nil {
		t.Fatalf("a missing policy must not fail the plan: %v", err)
	}
	if plan.PolicyErr == nil {
		t.Error("PolicyErr should record the failed policy resolution")
	}
	if plan.Policy != nil {
		t.Error("Policy must be nil when resolution failed")
	}
	if !plan.Native {
		t.Error("the launcher is still omac's native sandbox")
	}
}

func TestDefaultPolicyRef(t *testing.T) {
	if got := defaultPolicyRef(config.DefaultLauncherConfig()); got != "default" {
		t.Errorf("defaultPolicyRef(default config) = %q, want default", got)
	}

	custom := config.LauncherConfig{Sandbox: config.SandboxConfig{
		DefaultProfile: "builtin",
		Profiles: map[string]config.SandboxProfile{
			"builtin":  {Command: []string{"{{self}}", "sandbox", "run", "--profile", "strict", "--", "{{inner_cmd}}"}},
			"external": {Command: []string{"external-sbx", "--", "{{inner_cmd}}"}},
		},
	}}
	if got := defaultPolicyRef(custom); got != "strict" {
		t.Errorf("defaultPolicyRef = %q; must follow the launcher template, want strict", got)
	}

	// Opaque or unconfigured default launcher: "" means the default policy.
	custom.Sandbox.DefaultProfile = "external"
	if got := defaultPolicyRef(custom); got != "" {
		t.Errorf("opaque launcher: defaultPolicyRef = %q, want empty", got)
	}
	custom.Sandbox.DefaultProfile = "nosuch"
	if got := defaultPolicyRef(custom); got != "" {
		t.Errorf("unknown launcher: defaultPolicyRef = %q, want empty", got)
	}
}
