package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func strictProfileSelection(workdir string) ProfileSelection {
	return ProfileSelection{
		Path:  filepath.Join(LocalConfigDir(workdir), "strict.json"),
		Name:  "strict",
		Layer: "workdir",
	}
}

// The pin covers exactly the files a launch loads, per file: the reason names
// the file that diverged — config.yaml, the selected profile, or both — and
// approval records only the files the approved launch actually saw.
func TestProjectSandboxApprovalCoversLoadedFilesByName(t *testing.T) {
	isolateHome(t)
	workdir := t.TempDir()
	profPath := filepath.Join(LocalConfigDir(workdir), "strict.json")
	writeFile(t, profPath, `{"meta":{"name":"strict"}}`)
	writeFile(t, ProjectLauncherConfigPath(workdir), "sandbox:\n  profile_name: strict\n")

	sel, err := ResolveSandboxProfile(workdir)
	if err != nil {
		t.Fatalf("ResolveSandboxProfile: %v", err)
	}
	if sel.Layer != "workdir" {
		t.Fatalf("expected the workdir layer, got %+v", sel)
	}

	trusted, firstUse, reason := ProjectSandboxTrust(workdir, sel, true)
	if trusted || !firstUse {
		t.Fatalf("first use = (trusted %v, firstUse %v); want unapproved first use", trusted, firstUse)
	}
	if !strings.Contains(reason, "not yet approved") ||
		!strings.Contains(reason, ProjectLauncherConfigPath(workdir)) ||
		!strings.Contains(reason, profPath) {
		t.Errorf("first-use reason should name both loaded files, got %q", reason)
	}
	if err := PinProjectSandbox(workdir, sel, true); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if trusted, firstUse, _ := ProjectSandboxTrust(workdir, sel, true); !trusted || firstUse {
		t.Errorf("approved content must be trusted (trusted %v, firstUse %v)", trusted, firstUse)
	}

	// Only the profile changed: config.yaml must not be blamed.
	writeFile(t, profPath, `{"meta":{"name":"evil"},"network":{"mode":"open"}}`)
	trusted, firstUse, reason = ProjectSandboxTrust(workdir, sel, true)
	if trusted || firstUse {
		t.Fatalf("changed profile must be distrusted (trusted %v, firstUse %v)", trusted, firstUse)
	}
	if !strings.Contains(reason, profPath) || strings.Contains(reason, ProjectLauncherConfigPath(workdir)) {
		t.Errorf("reason should name only the profile, got %q", reason)
	}
	if !strings.Contains(reason, "changed since it was approved") {
		t.Errorf("reason should say changed, got %q", reason)
	}

	// Re-approve, then change the config only (now including a new
	// profile_name): the reason names config.yaml and its newly selected
	// profile, not the previously approved one.
	if err := PinProjectSandbox(workdir, sel, true); err != nil {
		t.Fatalf("re-approve: %v", err)
	}
	writeFile(t, ProjectLauncherConfigPath(workdir), "sandbox:\n  profile_name: other\n")
	other := filepath.Join(LocalConfigDir(workdir), "other.json")
	writeFile(t, other, `{"meta":{"name":"other"}}`)
	sel2, err := ResolveSandboxProfile(workdir)
	if err != nil {
		t.Fatalf("ResolveSandboxProfile after rename: %v", err)
	}
	trusted, firstUse, reason = ProjectSandboxTrust(workdir, sel2, true)
	if trusted || firstUse {
		t.Fatalf("changed config with new selection must be distrusted (trusted %v, firstUse %v)", trusted, firstUse)
	}
	if !strings.Contains(reason, ProjectLauncherConfigPath(workdir)) ||
		!strings.Contains(reason, other) || strings.Contains(reason, profPath) {
		t.Errorf("reason should name config.yaml and the new profile, got %q", reason)
	}
}

// An explicit --profile-path first use pins the profile only; the config.yaml
// it bypassed stays unapproved for later config-driven launches.
func TestProjectSandboxExplicitPathPinsProfileOnly(t *testing.T) {
	isolateHome(t)
	workdir := t.TempDir()
	profPath := filepath.Join(LocalConfigDir(workdir), "strict.json")
	writeFile(t, profPath, `{"meta":{"name":"strict"}}`)
	sel := strictProfileSelection(workdir)

	trusted, firstUse, _ := ProjectSandboxTrust(workdir, sel, false)
	if trusted || !firstUse {
		t.Fatalf("explicit first use = (trusted %v, firstUse %v); want unapproved first use", trusted, firstUse)
	}
	if err := PinProjectSandbox(workdir, sel, false); err != nil {
		t.Fatalf("explicit pin: %v", err)
	}
	if trusted, _, _ := ProjectSandboxTrust(workdir, sel, false); !trusted {
		t.Error("the explicit path must be trusted right after its first use")
	}

	// A config.yaml appearing later does not inherit the profile's approval
	// on a config-driven launch, but the explicit path keeps working: only
	// loaded files are compared.
	writeFile(t, ProjectLauncherConfigPath(workdir), "sandbox:\n  profile_name: strict\n")
	if trusted, _, _ := ProjectSandboxTrust(workdir, sel, true); trusted {
		t.Error("config.yaml planted after an explicit approval must not be trusted on a config-driven launch")
	}
	if trusted, _, _ := ProjectSandboxTrust(workdir, sel, false); !trusted {
		t.Error("the explicit path compares only the profile it loaded")
	}
}

func TestProjectSandboxTrustNoLocalContent(t *testing.T) {
	isolateHome(t)
	workdir := t.TempDir()
	sel, err := ResolveSandboxProfile(workdir)
	if err != nil {
		t.Fatalf("ResolveSandboxProfile: %v", err)
	}
	trusted, firstUse, _ := ProjectSandboxTrust(workdir, sel, true)
	if !trusted || firstUse {
		t.Errorf("a project with no local content must be trusted and need no pin (trusted %v, firstUse %v)", trusted, firstUse)
	}
	if err := PinProjectSandbox(workdir, sel, true); err != nil {
		t.Errorf("pinning nothing must be a no-op, got %v", err)
	}
}
