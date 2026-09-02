package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
	"github.com/TNG/oh-my-agentic-coder/internal/plugin"
	"github.com/TNG/oh-my-agentic-coder/internal/prefs"
)

func TestPluginInstallCommand(t *testing.T) {
	isolateHome(t)
	env := makeEnv(t.TempDir())

	if code := runPlugin([]string{"install", "opencode-desktop"}, env); code != ExitOK {
		t.Fatalf("install exit=%d, want %d", code, ExitOK)
	}
	dest := filepath.Join(env.Workdir, ".opencode", "plugins", plugin.MultiDirFileName)
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("plugin not written: %v", err)
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
