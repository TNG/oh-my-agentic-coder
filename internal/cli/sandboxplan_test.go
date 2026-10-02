package cli

import (
	"bytes"
	"io"
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
	sel, err := activeProfileSelection(workdir, local, false, io.Discard)
	if err != nil {
		t.Fatalf("activeProfileSelection: %v", err)
	}
	if want, werr := filepath.EvalSymlinks(local); werr != nil || sel.Path != want {
		t.Errorf("selection = %+v; want the resolved form of %q", sel, local)
	}
	if sel.Layer != "workdir" {
		t.Errorf("selection layer = %q; want workdir", sel.Layer)
	}
	if _, err := activeProfileSelection(workdir, filepath.Join(t.TempDir(), "evil.json"), false, io.Discard); err == nil {
		t.Error("a --profile-path outside the trusted dirs must be rejected")
	}
}

// A project-local config is used only after one approval: a first use (content
// that could have been planted in the repo) aborts until
// --accept-project-config, an approved pin covers the loaded files per file,
// and a between-session change aborts with the file that changed named.
func TestActiveProfileSelectionDistrustsChangedProjectConfig(t *testing.T) {
	isolateHome(t)
	workdir := t.TempDir()
	local := filepath.Join(config.LocalConfigDir(workdir), "strict.json")
	if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(local, []byte(`{"meta":{"name":"strict"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.ProjectLauncherConfigPath(workdir), []byte("sandbox:\n  profile_name: strict\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// First use aborts until approved: the content could have been planted.
	var warn bytes.Buffer
	_, err := activeProfileSelection(workdir, "", false, &warn)
	if err == nil {
		t.Fatal("an unapproved project config must abort the launch")
	}
	if !strings.Contains(err.Error(), "not yet approved") ||
		!strings.Contains(err.Error(), local) ||
		!strings.Contains(err.Error(), config.ProjectLauncherConfigPath(workdir)) ||
		!strings.Contains(err.Error(), "--accept-project-config") {
		t.Errorf("abort error should name the files and the approval flag, got: %v", err)
	}

	// The approval pins the loaded files and the local selection runs.
	sel, err := activeProfileSelection(workdir, "", true, io.Discard)
	if err != nil {
		t.Fatalf("approved first use: %v", err)
	}
	if sel.Layer != "workdir" {
		t.Fatalf("approval should select the local layer, got %+v", sel)
	}

	// Simulate the agent replacing the profile between sessions.
	if err := os.WriteFile(local, []byte(`{"meta":{"name":"evil"},"network":{"mode":"open"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = activeProfileSelection(workdir, "", false, &warn)
	if err == nil {
		t.Fatal("a changed project config must abort the launch, not fall back")
	}
	if !strings.Contains(err.Error(), "changed since it was approved") ||
		!strings.Contains(err.Error(), local) ||
		strings.Contains(err.Error(), config.ProjectLauncherConfigPath(workdir)) ||
		!strings.Contains(err.Error(), "--accept-project-config") {
		t.Errorf("abort error should name the changed file only and the re-approval flag, got: %v", err)
	}

	// Explicit re-approval trusts the new content again.
	sel, err = activeProfileSelection(workdir, "", true, io.Discard)
	if err != nil {
		t.Fatalf("re-approve: %v", err)
	}
	if sel.Layer != "workdir" || sel.Path != local {
		t.Errorf("re-approval should restore the local selection, got %+v", sel)
	}
	sel, err = activeProfileSelection(workdir, "", false, io.Discard)
	if err != nil || sel.Layer != "workdir" {
		t.Errorf("after re-approval the local layer must be trusted: %+v (%v)", sel, err)
	}
}

// --profile-path into <workdir>/.omac goes through the same trust store, but
// the typed path is its own approval: the first use pins the profile, a
// between-session change aborts, and --accept-project-config re-approves. The
// bypassed config.yaml neither gates an explicit launch nor inherits the
// approval. A global-layer path is exempt — ~/.config/omac is the
// non-overridable host dir re-read on every launch.
func TestActiveProfileSelectionExplicitPathSharesThePin(t *testing.T) {
	isolateHome(t)
	workdir := t.TempDir()
	local := filepath.Join(config.LocalConfigDir(workdir), "strict.json")
	if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(local, []byte(`{"meta":{"name":"strict"}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	sel, err := activeProfileSelection(workdir, local, false, io.Discard)
	if err != nil {
		t.Fatalf("explicit first use: %v", err)
	}
	if want, werr := filepath.EvalSymlinks(local); werr != nil || sel.Path != want {
		t.Fatalf("selection = %+v; want the resolved form of %q", sel, local)
	}

	sel, err = activeProfileSelection(workdir, local, false, io.Discard)
	if err != nil {
		t.Fatalf("unchanged explicit path must still run: %+v (%v)", sel, err)
	}

	// The agent replaces the profile between the two explicit launches.
	if err := os.WriteFile(local, []byte(`{"meta":{"name":"evil"},"network":{"mode":"open"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = activeProfileSelection(workdir, local, false, io.Discard)
	if err == nil {
		t.Fatal("a replaced workdir-layer profile must abort the explicit-path launch")
	}
	if !strings.Contains(err.Error(), local) || !strings.Contains(err.Error(), "changed since it was approved") {
		t.Errorf("abort error should name the profile, got: %v", err)
	}
	sel, err = activeProfileSelection(workdir, local, true, io.Discard)
	if err != nil || sel.Path != local {
		t.Fatalf("re-approval must accept the explicit path again: %+v (%v)", sel, err)
	}

	// A config planted after the explicit approvals does not gate an explicit
	// launch (only loaded files are pinned), ...
	if err := os.WriteFile(config.ProjectLauncherConfigPath(workdir), []byte("sandbox:\n  profile_name: strict\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sel, err = activeProfileSelection(workdir, local, false, io.Discard)
	if err != nil || sel.Path != local {
		t.Fatalf("an untouched explicit launch must not be gated by the bypassed config.yaml: %+v (%v)", sel, err)
	}

	// ... while a global-layer path is exempt outright.
	global := filepath.Join(os.Getenv("HOME"), ".config", "omac", "sandbox-profiles", "global-strict.json")
	if err := os.MkdirAll(filepath.Dir(global), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(global, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := activeProfileSelection(workdir, global, false, io.Discard); err != nil {
		t.Fatalf("global-layer --profile-path must not be trust-gated: %v", err)
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

// An uncreatable .omac (e.g. the workdir path is a file) warns and continues:
// whatever omac cannot create the agent cannot create either, so nothing
// plantable is missing.
func TestEnsureOmacLocalDirWarnsOnUncreatable(t *testing.T) {
	blocked := filepath.Join(t.TempDir(), "blocked")
	if err := os.WriteFile(blocked, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	var buf strings.Builder
	if err := ensureOmacLocalDir(&buf, blocked); err != nil {
		t.Fatalf("ensureOmacLocalDir must warn and continue, got error: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "proceeding without project-local sandbox configuration") {
		t.Errorf("expected the warn-and-continue notice, got %q", out)
	}
}

// A pinning failure under --accept-project-config aborts: the user believes
// they approved, but without the record the later tampering check is gone —
// failing open is not an option.
func TestApprovedProjectSandboxAbortsWhenPinUnrecordable(t *testing.T) {
	isolateHome(t)
	home := os.Getenv("HOME")
	workdir := t.TempDir()
	profPath := filepath.Join(workdir, ".omac", "strict.json")
	if err := os.MkdirAll(filepath.Dir(profPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(profPath, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workdir, ".omac", "config.yaml"), []byte("sandbox:\n  profile_name: strict\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sel, err := config.ResolveSandboxProfile(workdir)
	if err != nil {
		t.Fatalf("ResolveSandboxProfile: %v", err)
	}
	// The pin store directory is readable but not writable: the trust check
	// succeeds, the record fails.
	if os.Getuid() == 0 {
		t.Skip("running as root: chmod does not deny writes")
	}
	if err := os.MkdirAll(filepath.Join(home, ".config", "omac"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFileT(t, projectPinsPathFor(t), "{}\n")
	if err := os.Chmod(filepath.Join(home, ".config", "omac"), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(home, ".config", "omac"), 0o700) })
	_, verr := approvedProjectSandbox(workdir, sel, true, true)
	if verr == nil {
		t.Fatal("an approval that cannot be recorded must abort the launch")
	}
	if !strings.Contains(verr.Error(), "cannot record the approved project sandbox configuration") {
		t.Errorf("refusal must say the approval could not be recorded, got: %v", verr)
	}
}

// projectPinsPathFor exposes the host pin store path under the test's HOME.
func projectPinsPathFor(t *testing.T) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(os.Getenv("HOME"), ".config", "omac"), 0o700); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(os.Getenv("HOME"), ".config", "omac", "project-sandbox.json")
}

func writeFileT(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
