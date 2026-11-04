package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
	"github.com/TNG/oh-my-agentic-coder/internal/plugin"
	"github.com/TNG/oh-my-agentic-coder/internal/prefs"
)

func TestPluginInstallCommand(t *testing.T) {
	isolateHome(t)
	env := makeEnv(t.TempDir())
	warnings, err := os.CreateTemp(t.TempDir(), "warnings")
	if err != nil {
		t.Fatal(err)
	}
	defer warnings.Close()
	env.Stderr = warnings

	if code := runPlugin([]string{"install", "opencode-desktop"}, env); code != ExitOK {
		t.Fatalf("install exit=%d, want %d", code, ExitOK)
	}
	dest := filepath.Join(env.Workdir, ".opencode", "plugins", plugin.MultiDirFileName)
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("plugin not written: %v", err)
	}
	output, err := os.ReadFile(warnings.Name())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(output), plugin.MultiDirCompatibility) {
		t.Fatal("install must warn about the minimum OpenCode version")
	}
}

// ensureOpenCodePlugin must stay silent on the common launch path where the
// global plugin is already current. Printing a static requirement on every
// launch was the bug this locks out.
func TestOpenCodePluginBootstrap_UnchangedIsSilent(t *testing.T) {
	isolateHome(t)
	env := makeEnv(t.TempDir())
	h, _ := config.LookupHarness("opencode")
	// First call provisions the plugin; its output is not under test here.
	ensureOpenCodePlugin(env, h)

	warnings, err := os.CreateTemp(t.TempDir(), "warnings")
	if err != nil {
		t.Fatal(err)
	}
	defer warnings.Close()
	env.Stderr = warnings
	ensureOpenCodePlugin(env, h)
	output, err := os.ReadFile(warnings.Name())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(output), plugin.MultiDirCompatibility) {
		t.Fatalf("an unchanged install must not print the compatibility warning, got: %s", output)
	}
	if strings.TrimSpace(string(output)) != "" {
		t.Fatalf("an unchanged install must print nothing, got: %s", output)
	}
}

// A first provision changes state, so it must print the compatibility
// requirement.
func TestOpenCodePluginBootstrap_ChangedWarnsAboutCompatibility(t *testing.T) {
	isolateHome(t)
	env := makeEnv(t.TempDir())
	warnings, err := os.CreateTemp(t.TempDir(), "warnings")
	if err != nil {
		t.Fatal(err)
	}
	defer warnings.Close()
	env.Stderr = warnings
	h, _ := config.LookupHarness("opencode")
	ensureOpenCodePlugin(env, h)
	output, err := os.ReadFile(warnings.Name())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(output), plugin.MultiDirCompatibility) {
		t.Fatalf("a changed install must warn about compatibility, got: %s", output)
	}
	if !strings.Contains(string(output), "provisioned the omac OpenCode plugin") {
		t.Fatalf("expected a provisioned line, got: %s", output)
	}
}

func TestOpenCodePluginBootstrap_ChecksProjectLocalCopy(t *testing.T) {
	for _, tc := range []struct {
		name             string
		global           string
		local            string
		wantLocalWarning bool
	}{
		{"current global, stale local", "current", "stale", true},
		{"missing global, stale local", "missing", "stale", true},
		{"conflicting global, stale local", "stale", "stale", true},
		{"current global, current local", "current", "current", false},
		{"current global, missing local", "current", "missing", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateHome(t)
			env := makeEnv(t.TempDir())
			h, _ := config.LookupHarness("opencode")
			local := plugin.MultiDirPath(env.Workdir, h.BridgeDir)
			for _, file := range []struct {
				path  string
				state string
			}{
				{plugin.MultiDirPathIn(h.GlobalBridgeDir()), tc.global},
				{local, tc.local},
			} {
				if file.state == "missing" {
					continue
				}
				data := []byte("older plugin\n")
				if file.state == "current" {
					data = plugin.MultiDirSource()
				}
				if err := os.MkdirAll(filepath.Dir(file.path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(file.path, data, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			warnings, err := os.CreateTemp(t.TempDir(), "warnings")
			if err != nil {
				t.Fatal(err)
			}
			defer warnings.Close()
			env.Stderr = warnings
			ensureOpenCodePlugin(env, h)
			output, err := os.ReadFile(warnings.Name())
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Contains(string(output), local); got != tc.wantLocalWarning {
				t.Fatalf("local warning = %v, want %v; output: %s", got, tc.wantLocalWarning, output)
			}
			if tc.wantLocalWarning {
				command := "omac --workdir " + env.Workdir + " plugin install opencode-desktop --force"
				if !strings.Contains(string(output), command) {
					t.Fatalf("missing project-local replacement command; output: %s", output)
				}
				data, err := os.ReadFile(local)
				if err != nil {
					t.Fatal(err)
				}
				if string(data) != "older plugin\n" {
					t.Fatal("bootstrap overwrote the differing project-local plugin")
				}
			}
		})
	}
}

// A conflicting local edit changes what the user must do, so it must print the
// compatibility requirement and tell the user to restart OpenCode.
func TestOpenCodePluginBootstrap_ConflictWarnsAndSaysRestart(t *testing.T) {
	isolateHome(t)
	env := makeEnv(t.TempDir())
	h, _ := config.LookupHarness("opencode")
	gdir := h.GlobalBridgeDir()
	if gdir == "" {
		t.Fatal("GlobalBridgeDir empty under isolated HOME/XDG")
	}
	if err := os.MkdirAll(gdir, 0o755); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(gdir, plugin.MultiDirFileName)
	if err := os.WriteFile(dest, []byte("// edited locally\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	warnings, err := os.CreateTemp(t.TempDir(), "warnings")
	if err != nil {
		t.Fatal(err)
	}
	defer warnings.Close()
	env.Stderr = warnings
	ensureOpenCodePlugin(env, h)
	output, err := os.ReadFile(warnings.Name())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(output), plugin.MultiDirCompatibility) {
		t.Fatalf("a conflict must warn about compatibility, got: %s", output)
	}
	if !strings.Contains(string(output), "Overwrite it") {
		t.Fatalf("expected an overwrite instruction, got: %s", output)
	}
	if !strings.Contains(string(output), "restart OpenCode") {
		t.Fatalf("the overwrite instruction must say to restart OpenCode, got: %s", output)
	}
}

// A stale project-local copy must be flagged when installing, so a user who
// only ever runs the global install still learns their local copy is old.
func TestPluginInstall_WarnsAboutStaleProjectLocalCopy(t *testing.T) {
	isolateHome(t)
	env := makeEnv(t.TempDir())
	h, _ := config.LookupHarness("opencode")
	local := plugin.MultiDirPath(env.Workdir, h.BridgeDir)
	if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(local, []byte("// stale local copy\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	warnings, err := os.CreateTemp(t.TempDir(), "warnings")
	if err != nil {
		t.Fatal(err)
	}
	defer warnings.Close()
	env.Stderr = warnings

	if code := runPlugin([]string{"install", "opencode-desktop", "--global"}, env); code != ExitOK {
		t.Fatalf("install exit=%d, want %d", code, ExitOK)
	}
	output, err := os.ReadFile(warnings.Name())
	if err != nil {
		t.Fatal(err)
	}
	msg := string(output)
	if !strings.Contains(msg, local) {
		t.Fatalf("warning must name the stale local path %s, got: %s", local, msg)
	}
	if !strings.Contains(msg, "omac --workdir "+env.Workdir+" plugin install opencode-desktop --force") {
		t.Fatalf("warning must give the path-specific replacement command, got: %s", msg)
	}
}

// A project-local copy identical to the bundled plugin needs no warning.
func TestPluginInstall_MatchingProjectLocalCopyIsSilent(t *testing.T) {
	isolateHome(t)
	env := makeEnv(t.TempDir())
	h, _ := config.LookupHarness("opencode")
	local := plugin.MultiDirPath(env.Workdir, h.BridgeDir)
	if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(local, plugin.MultiDirSource(), 0o644); err != nil {
		t.Fatal(err)
	}
	warnings, err := os.CreateTemp(t.TempDir(), "warnings")
	if err != nil {
		t.Fatal(err)
	}
	defer warnings.Close()
	env.Stderr = warnings

	if code := runPlugin([]string{"install", "opencode-desktop", "--global"}, env); code != ExitOK {
		t.Fatalf("install exit=%d, want %d", code, ExitOK)
	}
	output, err := os.ReadFile(warnings.Name())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(output), "also exists at "+local) {
		t.Fatalf("a matching local copy must not warn, got: %s", output)
	}
}

func TestPluginInstallUnknownTarget(t *testing.T) {
	isolateHome(t)
	env := makeEnv(t.TempDir())
	if code := runPlugin([]string{"install", "nope"}, env); code != ExitMisuse {
		t.Errorf("exit=%d, want %d for unknown target", code, ExitMisuse)
	}
}

func TestPluginInstallAlias(t *testing.T) {
	isolateHome(t)
	env := makeEnv(t.TempDir())
	if code := runPlugin([]string{"install", "multidir"}, env); code != ExitOK {
		t.Errorf("alias install exit=%d, want %d", code, ExitOK)
	}
}

func TestPluginInstallGlobal(t *testing.T) {
	isolateHome(t)
	env := makeEnv(t.TempDir())

	if code := runPlugin([]string{"install", "opencode-desktop", "--global"}, env); code != ExitOK {
		t.Fatalf("global install exit=%d, want %d", code, ExitOK)
	}
	// Must land in the global plugins dir, NOT the workdir.
	h, _ := config.LookupHarness("opencode")
	gdir := h.GlobalBridgeDir()
	if gdir == "" {
		t.Fatal("GlobalBridgeDir empty under isolated HOME/XDG")
	}
	if _, err := os.Stat(filepath.Join(gdir, plugin.MultiDirFileName)); err != nil {
		t.Fatalf("plugin not written to global dir %s: %v", gdir, err)
	}
	// And it must NOT have been written into the workdir.
	if _, err := os.Stat(filepath.Join(env.Workdir, ".opencode", "plugins", plugin.MultiDirFileName)); err == nil {
		t.Error("global install unexpectedly wrote into the workdir too")
	}
}

// warnPluginMissing must return true (proceed) without prompting when the
// plugin is already installed.
func TestWarnPluginMissing_InstalledProceeds(t *testing.T) {
	isolateHome(t)
	env := makeEnv(t.TempDir())
	h, _ := config.LookupHarness("opencode")
	if _, err := plugin.InstallMultiDir(env.Workdir, h.BridgeDir, false); err != nil {
		t.Fatalf("install: %v", err)
	}
	if !warnPluginMissing(env, h) {
		t.Error("should proceed when plugin is installed")
	}
}

// A global install must also satisfy the serve check (OpenCode loads
// global plugins too), so warnPluginMissing should proceed without warning
// even when the workdir has no local copy.
func TestWarnPluginMissing_GlobalInstallProceeds(t *testing.T) {
	isolateHome(t)
	env := makeEnv(t.TempDir())
	h, _ := config.LookupHarness("opencode")
	gdir := h.GlobalBridgeDir()
	if gdir == "" {
		t.Fatal("GlobalBridgeDir empty under isolated HOME/XDG")
	}
	if _, err := plugin.InstallMultiDirIn(gdir, false); err != nil {
		t.Fatalf("global install: %v", err)
	}
	// Workdir is deliberately empty of any local plugin.
	if !warnPluginMissing(env, h) {
		t.Error("should proceed when plugin is installed globally")
	}
}

// When suppressed via prefs, warnPluginMissing proceeds silently even with
// the plugin absent.
func TestWarnPluginMissing_SuppressedProceeds(t *testing.T) {
	isolateHome(t)
	env := makeEnv(t.TempDir())
	if err := prefs.Save(&prefs.Store{SuppressPluginWarning: true}); err != nil {
		t.Fatalf("save prefs: %v", err)
	}
	h, _ := config.LookupHarness("opencode")
	if !warnPluginMissing(env, h) {
		t.Error("should proceed when warning is suppressed")
	}
}

// With a non-TTY stdin (/dev/null in makeEnv) and the plugin missing,
// warnPluginMissing warns once and proceeds rather than blocking.
func TestWarnPluginMissing_NonInteractiveProceeds(t *testing.T) {
	isolateHome(t)
	env := makeEnv(t.TempDir())
	h, _ := config.LookupHarness("opencode")
	if !warnPluginMissing(env, h) {
		t.Error("non-interactive run should proceed after warning")
	}
}
