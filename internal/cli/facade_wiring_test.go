package cli

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tngtech/oh-my-agentic-coder/internal/config"
	"github.com/tngtech/oh-my-agentic-coder/internal/facade"
)

// wireForTest runs the real launch wiring for the named launcher profile
// ("" = the config default) and returns the facade plus any warnings.
func wireForTest(t *testing.T, launcherProfile string, noSandbox bool) (*facade.Facade, []string) {
	t.Helper()
	plan, err := resolveSandboxPlan(config.DefaultLauncherConfig(), launcherProfile)
	if err != nil {
		t.Fatalf("resolveSandboxPlan(%q): %v", launcherProfile, err)
	}
	f := facade.New("", "", nil, 0, 0, "", "test")
	var warnings []string
	wireFacadeSandbox(f, noSandbox, plan, func(format string, args ...any) {
		warnings = append(warnings, fmt.Sprintf(format, args...))
	})
	return f, warnings
}

// TestWireFacadeSandboxDefaultProfileWiresChecker is the regression guard
// for #173: the default launch resolves the LAUNCHER profile "builtin",
// whose POLICY ref is "default". Feeding the launcher name to the policy
// resolver (what wireFacadeSandbox used to do) always failed, leaving
// ProtectedPathChecker nil and GET /sandbox/denied answering 404 on every
// default `omac start` / `omac serve` — the endpoint's whole purpose is to
// distinguish a sandbox denial from a missing file.
func TestWireFacadeSandboxDefaultProfileWiresChecker(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	f, warnings := wireForTest(t, "", false)

	if len(warnings) != 0 {
		t.Errorf("default profile must wire cleanly; got warnings: %v", warnings)
	}
	if f.ProtectedPathChecker == nil {
		t.Fatal("ProtectedPathChecker is nil; GET /sandbox/denied would 404 on a default launch")
	}
	// A baseline-protected path must be recognized, so the agent gets a
	// real answer rather than a 404.
	creds := filepath.Join(home, ".aws", "credentials")
	rule, ok := f.ProtectedPathChecker.IsProtected(creds)
	if !ok {
		t.Errorf("IsProtected(%q) = false; baseline credentials path must be protected", creds)
	}
	if rule != "baseline" {
		t.Errorf("rule = %q, want baseline", rule)
	}
	if _, ok := f.ProtectedPathChecker.IsProtected(filepath.Join(home, "code", "main.go")); ok {
		t.Error("an ordinary path must not be reported as protected")
	}
	if f.IntentRegistry == nil {
		t.Error("IntentRegistry is nil; POST /sandbox/intent would 503")
	}
}

// TestWireFacadeSandboxAppliesDenialFacadeNote: denial.facade_note from the
// resolved POLICY profile must reach f.DenialNote — it never did while the
// resolution failed.
func TestWireFacadeSandboxAppliesDenialFacadeNote(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	const note = "custom note from the profile"
	stageProfile(t, home, `{"meta": {"name": "default"}, "denial": {"facade_note": "`+note+`"}}`)

	f, warnings := wireForTest(t, "", false)

	if len(warnings) != 0 {
		t.Errorf("unexpected warnings: %v", warnings)
	}
	if f.DenialNote != note {
		t.Errorf("DenialNote = %q, want %q", f.DenialNote, note)
	}
}

// TestWireFacadeSandboxOpaqueLauncherDisablesEndpoint: an external launcher
// (nono) has no omac policy to read, so the endpoint stays off — but the
// warning now says why, instead of reporting a bogus resolve failure for a
// policy file that was never meant to exist.
func TestWireFacadeSandboxOpaqueLauncherDisablesEndpoint(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	f, warnings := wireForTest(t, "nono", false)

	if f.ProtectedPathChecker != nil {
		t.Error("an opaque launcher must not get a protected-path checker")
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "nono") {
		t.Errorf("expected one warning naming the launcher profile; got %v", warnings)
	}
	if f.IntentRegistry == nil {
		t.Error("IntentRegistry must be wired regardless of the sandbox backend")
	}
}

// TestWireFacadeSandboxNoSandboxIsSilent: with --no-sandbox there is no
// sandbox to describe, so the checker stays nil and nothing is warned.
func TestWireFacadeSandboxNoSandboxIsSilent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	f, warnings := wireForTest(t, "", true)

	if f.ProtectedPathChecker != nil {
		t.Error("--no-sandbox must not wire a protected-path checker")
	}
	if len(warnings) != 0 {
		t.Errorf("--no-sandbox must not warn; got %v", warnings)
	}
	if f.IntentRegistry == nil {
		t.Error("IntentRegistry must be wired even without a sandbox")
	}
}
