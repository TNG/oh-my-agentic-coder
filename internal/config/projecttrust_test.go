package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestProjectSandboxTrustPinsFirstUseAndDetectsChange(t *testing.T) {
	isolateHome(t)
	workdir := t.TempDir()
	prof := filepath.Join(LocalConfigDir(workdir), "strict.json")
	writeFile(t, prof, `{"meta":{"name":"strict"}}`)
	writeFile(t, ProjectLauncherConfigPath(workdir), "sandbox:\n  profile_name: strict\n")

	sel, err := ResolveSandboxProfile(workdir)
	if err != nil {
		t.Fatalf("ResolveSandboxProfile: %v", err)
	}
	if sel.Layer != "workdir" {
		t.Fatalf("expected the workdir layer, got %+v", sel)
	}

	trusted, firstUse, reason := ProjectSandboxTrust(workdir, sel)
	if !trusted || !firstUse || reason != "" {
		t.Fatalf("first use = (trusted %v, firstUse %v, %q); want trusted first use", trusted, firstUse, reason)
	}
	if err := PinProjectSandbox(workdir, sel); err != nil {
		t.Fatalf("PinProjectSandbox: %v", err)
	}

	if trusted, firstUse, _ := ProjectSandboxTrust(workdir, sel); !trusted || firstUse {
		t.Errorf("after pinning, unchanged content must be trusted without a new pin (trusted %v, firstUse %v)", trusted, firstUse)
	}

	// An agent replacing the profile between sessions must be detected.
	writeFile(t, prof, `{"meta":{"name":"evil"},"network":{"mode":"open"}}`)
	sel2, err := ResolveSandboxProfile(workdir)
	if err != nil {
		t.Fatalf("ResolveSandboxProfile: %v", err)
	}
	trusted, firstUse, reason = ProjectSandboxTrust(workdir, sel2)
	if trusted || firstUse {
		t.Errorf("changed profile must be distrusted (trusted %v, firstUse %v)", trusted, firstUse)
	}
	if !strings.Contains(reason, "changed since it was approved") {
		t.Errorf("reason should explain the change, got %q", reason)
	}

	// Re-approval accepts the new content.
	if err := PinProjectSandbox(workdir, sel2); err != nil {
		t.Fatalf("re-approve: %v", err)
	}
	if trusted, firstUse, _ := ProjectSandboxTrust(workdir, sel2); !trusted || firstUse {
		t.Errorf("re-approved content must be trusted (trusted %v, firstUse %v)", trusted, firstUse)
	}
}

// A local config that currently selects no profile is still pinned, so adding
// a profile_name later (the replacement path) is detected.
func TestProjectSandboxTrustDetectsLateProfileName(t *testing.T) {
	isolateHome(t)
	workdir := t.TempDir()
	writeFile(t, ProjectLauncherConfigPath(workdir), "cache:\n  scope: workdir\n")

	sel, err := ResolveSandboxProfile(workdir)
	if err != nil {
		t.Fatalf("ResolveSandboxProfile: %v", err)
	}
	if sel.Layer == "workdir" {
		t.Fatalf("a config without profile_name must not select the workdir layer, got %+v", sel)
	}
	if err := PinProjectSandbox(workdir, sel); err != nil {
		t.Fatalf("PinProjectSandbox: %v", err)
	}

	writeFile(t, ProjectLauncherConfigPath(workdir), "sandbox:\n  profile_name: evil\n")
	writeFile(t, filepath.Join(LocalConfigDir(workdir), "evil.json"), `{"meta":{"name":"evil"}}`)
	sel2, err := ResolveSandboxProfile(workdir)
	if err != nil {
		t.Fatalf("ResolveSandboxProfile: %v", err)
	}
	if sel2.Layer != "workdir" {
		t.Fatalf("fixture: expected the workdir layer after adding profile_name, got %+v", sel2)
	}
	if trusted, _, _ := ProjectSandboxTrust(workdir, sel2); trusted {
		t.Error("adding a profile_name to the project config after approval must be detected")
	}
}

func TestProjectSandboxTrustNoLocalContent(t *testing.T) {
	isolateHome(t)
	workdir := t.TempDir()
	sel, err := ResolveSandboxProfile(workdir)
	if err != nil {
		t.Fatalf("ResolveSandboxProfile: %v", err)
	}
	trusted, firstUse, _ := ProjectSandboxTrust(workdir, sel)
	if !trusted || firstUse {
		t.Errorf("a project with no local content must be trusted and need no pin (trusted %v, firstUse %v)", trusted, firstUse)
	}
	if err := PinProjectSandbox(workdir, sel); err != nil {
		t.Errorf("pinning nothing must be a no-op, got %v", err)
	}
}
