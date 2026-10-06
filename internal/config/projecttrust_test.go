package config

import (
	"encoding/json"
	"os"
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

func trustOf(t *testing.T, workdir string, sel ProfileSelection, configDriven bool) (bool, bool, string) {
	t.Helper()
	trusted, firstUse, reason, err := ProjectSandboxTrust(workdir, sel, configDriven)
	if err != nil {
		t.Fatalf("ProjectSandboxTrust: %v", err)
	}
	return trusted, firstUse, reason
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

	trusted, firstUse, reason := trustOf(t, workdir, sel, true)
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
	if trusted, firstUse, _ := trustOf(t, workdir, sel, true); !trusted || firstUse {
		t.Errorf("approved content must be trusted (trusted %v, firstUse %v)", trusted, firstUse)
	}

	// Only the profile changed: config.yaml must not be blamed.
	writeFile(t, profPath, `{"meta":{"name":"evil"},"network":{"mode":"open"}}`)
	trusted, firstUse, reason = trustOf(t, workdir, sel, true)
	if trusted || firstUse {
		t.Fatalf("changed profile must be distrusted (trusted %v, firstUse %v)", trusted, firstUse)
	}
	if !strings.Contains(reason, profPath) || strings.Contains(reason, ProjectLauncherConfigPath(workdir)) {
		t.Errorf("reason should name only the profile, got %q", reason)
	}
	if !strings.Contains(reason, "changed since it was approved") {
		t.Errorf("reason should say changed, got %q", reason)
	}
}

// Pins are keyed per path: approving profile B through an explicit
// --profile-path must not disturb the pin that covered profile A, and the
// next config-driven launch of A proceeds without re-approval.
func TestProjectSandboxPinsAreRecordedPerPath(t *testing.T) {
	isolateHome(t)
	workdir := t.TempDir()
	profA := filepath.Join(LocalConfigDir(workdir), "a.json")
	profB := filepath.Join(LocalConfigDir(workdir), "b.json")
	for path, body := range map[string]string{
		ProjectLauncherConfigPath(workdir): "sandbox:\n  profile_name: a\n",
		profA:                              `{"meta":{"name":"a"}}`,
		profB:                              `{"meta":{"name":"b"}}`,
	} {
		writeFile(t, path, body)
	}

	if err := PinProjectSandbox(workdir, strictProfileSelectionNamed(workdir, profA), true); err != nil {
		t.Fatalf("approve A: %v", err)
	}
	if err := PinProjectSandbox(workdir, strictProfileSelectionNamed(workdir, profB), false); err != nil {
		t.Fatalf("approve B: %v", err)
	}
	if trusted, _, _ := trustOf(t, workdir, strictProfileSelectionNamed(workdir, profA), true); !trusted {
		t.Error("approving B must not invalidate A's pin; the next config-driven launch of A must be trusted")
	}
	if trusted, _, _ := trustOf(t, workdir, strictProfileSelectionNamed(workdir, profB), false); !trusted {
		t.Error("the explicit path must be trusted right after its approval")
	}
}

func strictProfileSelectionNamed(workdir, path string) ProfileSelection {
	return ProfileSelection{Path: path, Name: strings.TrimSuffix(filepath.Base(path), ".json"), Layer: "workdir"}
}

// An explicit --profile-path first use pins the profile only; the config.yaml
// it bypassed stays unapproved for later config-driven launches.
func TestProjectSandboxExplicitPathPinsProfileOnly(t *testing.T) {
	isolateHome(t)
	workdir := t.TempDir()
	profPath := filepath.Join(LocalConfigDir(workdir), "strict.json")
	writeFile(t, profPath, `{"meta":{"name":"strict"}}`)
	sel := strictProfileSelection(workdir)

	trusted, firstUse, _ := trustOf(t, workdir, sel, false)
	if trusted || !firstUse {
		t.Fatalf("explicit first use = (trusted %v, firstUse %v); want unapproved first use", trusted, firstUse)
	}
	if err := PinProjectSandbox(workdir, sel, false); err != nil {
		t.Fatalf("explicit pin: %v", err)
	}
	if trusted, _, _ := trustOf(t, workdir, sel, false); !trusted {
		t.Error("the explicit path must be trusted right after its first use")
	}

	// A config.yaml appearing later does not inherit the profile's approval
	// on a config-driven launch, but the explicit path keeps working: only
	// loaded files are compared.
	writeFile(t, ProjectLauncherConfigPath(workdir), "sandbox:\n  profile_name: strict\n")
	if trusted, _, _ := trustOf(t, workdir, sel, true); trusted {
		t.Error("config.yaml planted after an explicit approval must not be trusted on a config-driven launch")
	}
	if trusted, _, _ := trustOf(t, workdir, sel, false); !trusted {
		t.Error("the explicit path compares only the profile it loaded")
	}
}

// An approved file that disappears since its approval counts as changed, not
// as "never approved": a session that deletes .omac/config.yaml must not get
// the silent fallback to the global layer while a pin still exists, and a
// deleted profile is refused the same way.
func TestProjectSandboxApprovedAbsenceIsRefused(t *testing.T) {
	t.Run("approved config.yaml deleted", func(t *testing.T) {
		isolateHome(t)
		workdir := t.TempDir()
		writeFile(t, ProjectLauncherConfigPath(workdir), "facade:\n  max_body_bytes: 4242\n")
		sel, err := ResolveSandboxProfile(workdir)
		if err != nil {
			t.Fatalf("ResolveSandboxProfile: %v", err)
		}
		if err := PinProjectSandbox(workdir, sel, true); err != nil {
			t.Fatalf("approve: %v", err)
		}
		if err := os.Remove(ProjectLauncherConfigPath(workdir)); err != nil {
			t.Fatal(err)
		}
		trusted, firstUse, reason := trustOf(t, workdir, sel, true)
		if trusted || firstUse {
			t.Fatalf("deleting an approved config must be distrusted (trusted %v, firstUse %v)", trusted, firstUse)
		}
		if !strings.Contains(reason, "disappeared") || !strings.Contains(reason, ProjectLauncherConfigPath(workdir)) {
			t.Errorf("reason should name the missing file, got %q", reason)
		}
	})

	t.Run("approved profile deleted", func(t *testing.T) {
		isolateHome(t)
		workdir := t.TempDir()
		profPath := filepath.Join(LocalConfigDir(workdir), "strict.json")
		writeFile(t, profPath, `{"meta":{"name":"strict"}}`)
		sel := strictProfileSelection(workdir)
		if err := PinProjectSandbox(workdir, sel, false); err != nil {
			t.Fatalf("approve: %v", err)
		}
		if err := os.Remove(profPath); err != nil {
			t.Fatal(err)
		}
		if trusted, _, _ := trustOf(t, workdir, sel, false); trusted {
			t.Error("deleting an approved profile must be distrusted")
		}
	})

	t.Run("re-approving an absence records it", func(t *testing.T) {
		isolateHome(t)
		workdir := t.TempDir()
		writeFile(t, ProjectLauncherConfigPath(workdir), "facade:\n  max_body_bytes: 4242\n")
		sel, err := ResolveSandboxProfile(workdir)
		if err != nil {
			t.Fatalf("ResolveSandboxProfile: %v", err)
		}
		if err := PinProjectSandbox(workdir, sel, true); err != nil {
			t.Fatalf("approve: %v", err)
		}
		if err := os.Remove(ProjectLauncherConfigPath(workdir)); err != nil {
			t.Fatal(err)
		}
		// The user deletes the file themselves and re-runs with
		// --accept-project-config: the absence becomes the approved state.
		if err := PinProjectSandbox(workdir, sel, true); err != nil {
			t.Fatalf("re-approve: %v", err)
		}
		if trusted, _, _ := trustOf(t, workdir, sel, true); !trusted {
			t.Error("a file the user deleted and re-approved must be trusted as absent")
		}
		// ...but the file must not silently reappear approved.
		writeFile(t, ProjectLauncherConfigPath(workdir), "facade:\n  max_body_bytes: 1337\n")
		if trusted, _, _ := trustOf(t, workdir, sel, true); trusted {
			t.Error("config.yaml planted over an approved absence must be refused")
		}
	})
}

// A project with no local content is trusted and needs no pin.
func TestProjectSandboxTrustNoLocalContent(t *testing.T) {
	isolateHome(t)
	workdir := t.TempDir()
	sel, err := ResolveSandboxProfile(workdir)
	if err != nil {
		t.Fatalf("ResolveSandboxProfile: %v", err)
	}
	trusted, firstUse, _ := trustOf(t, workdir, sel, true)
	if !trusted || firstUse {
		t.Errorf("a project with no local content must be trusted and need no pin (trusted %v, firstUse %v)", trusted, firstUse)
	}
	if err := PinProjectSandbox(workdir, sel, true); err != nil {
		t.Errorf("pinning nothing must be a no-op, got %v", err)
	}
}

// Table of degraded pin-store states: every one of them must fail closed
// rather than silently re-approve anything.
//
//   - legacy pin formats (single combined hash; per-file-name slots) carry no
//     per-path entries, so they count as unapproved and ask for one
//     re-approval;
//   - a unreadable store must never be treated as "no pins yet";
//   - a corrupt store must never be treated as "no pins yet".
func TestProjectSandboxDegradedPinStores(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"legacy single combined hash", `{"projects":{"/w":{"hash":"abc"}}}`},
		{"legacy per-file slots", `{"projects":{"/w":{"config_hash":"c","profile_hash":"p"}}}`},
		{"empty store", `{}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateHome(t)
			writeFile(t, projectPinsPath(), tc.raw)
			pins, err := loadProjectPins()
			if err != nil {
				t.Fatalf("loadProjectPins on degraded store: %v", err)
			}
			// The old shapes carry no per-path approvals, so they must decode
			// into an empty files map: everything counts as unapproved and
			// asks for one re-approval.
			if files := pins.Projects["/w"].Files; len(files) != 0 {
				t.Errorf("degraded store must decode with no per-path pins, got %v", files)
			}
			if _, approved := pins.Projects["/w"].Files[ProjectLauncherConfigPath("/w")]; approved {
				t.Errorf("legacy pin must not count as approved for config.yaml: %v", pins.Projects["/w"].Files)
			}
		})
	}

	t.Run("corrupt store fails closed", func(t *testing.T) {
		isolateHome(t)
		workdir := t.TempDir()
		writeFile(t, projectPinsPath(), "{not json")
		profPath := filepath.Join(LocalConfigDir(workdir), "strict.json")
		writeFile(t, profPath, `{"meta":{"name":"strict"}}`)
		trusted, _, reason, err := ProjectSandboxTrust(workdir, strictProfileSelection(workdir), false)
		if err == nil {
			t.Fatalf("a corrupt pin store must surface an error, got trusted=%v firstUse-reported-reason=%q", trusted, reason)
		}
		if !strings.Contains(err.Error(), "read the approved project sandbox store") {
			t.Errorf("error should say the store is unreadable, got %v", err)
		}
	})

	t.Run("unreadable pin store surfaces an error", func(t *testing.T) {
		isolateHome(t)
		writeFile(t, projectPinsPath()+"-x", "")
		// A directory where the store should be makes the read fail.
		if err := os.MkdirAll(projectPinsPath(), 0o700); err != nil {
			t.Fatal(err)
		}
		_, err := loadProjectPins()
		if err == nil {
			t.Fatal("reading a directory as the pin store must error")
		}
	})
}

// A workdir whose only project content is an operational config.yaml (cache
// scope, no profile_name) goes through the same approve-then-refuse flow.
func TestProjectSandboxOperationalOnlyConfig(t *testing.T) {
	isolateHome(t)
	workdir := t.TempDir()
	writeFile(t, filepath.Join(LocalConfigDir(workdir), "config.yaml"), "cache:\n  scope: global\n")

	sel, err := ResolveSandboxProfile(workdir)
	if err != nil {
		t.Fatalf("ResolveSandboxProfile: %v", err)
	}
	if sel.Layer == "workdir" {
		t.Fatalf("no local profile exists; selection must not be workdir: %+v", sel)
	}

	trusted, firstUse, _ := trustOf(t, workdir, sel, true)
	if trusted || !firstUse {
		t.Fatalf("operational-only config must be gated on first use (trusted %v, firstUse %v)", trusted, firstUse)
	}
	if err := PinProjectSandbox(workdir, sel, true); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if trusted, _, _ := trustOf(t, workdir, sel, true); !trusted {
		t.Error("the approved operational-only config must be trusted")
	}
	writeFile(t, ProjectLauncherConfigPath(workdir), "cache:\n  scope: config\n")
	if _, _, reason := trustOf(t, workdir, sel, true); !strings.Contains(reason, "changed since") {
		t.Errorf("a tampered operational-only config must be refused, got %q", reason)
	}
}

// The pin store on disk keeps its on-disk shape honest for debugging: one
// projects map keyed by workdir, one files map keyed by path.
func TestProjectPinsStoreShape(t *testing.T) {
	isolateHome(t)
	workdir := t.TempDir()
	profPath := filepath.Join(LocalConfigDir(workdir), "strict.json")
	writeFile(t, profPath, `{"meta":{"name":"strict"}}`)

	if err := PinProjectSandbox(workdir, strictProfileSelection(workdir), false); err != nil {
		t.Fatalf("approve: %v", err)
	}
	raw, err := os.ReadFile(projectPinsPath())
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]map[string]map[string]map[string]string
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("store is not the documented shape: %v", err)
	}
	files := decoded["projects"][workdir]["files"]
	if !strings.HasSuffix(profPath, ".json") || files[profPath] == "" {
		t.Errorf("store missing per-path digest for %s: %s", profPath, raw)
	}
}
