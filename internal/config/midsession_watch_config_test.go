package config

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestMidsessionWatchDefaults asserts the compiled-in tuning is used
// when no config exists: the optional full pass is off (the per-directory
// re-check covers the same-tick window) and concurrency is unthrottled
// (0 = the walk's normal concurrency).
func TestMidsessionWatchDefaults(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	workdir := t.TempDir()
	lc, _, err := LoadLauncher(workdir)
	if err != nil {
		t.Fatalf("LoadLauncher: %v", err)
	}
	if got := lc.Sandbox.MidsessionWatch.FullPassTicks; got != 0 {
		t.Errorf("FullPassTicks = %d, want 0 (off)", got)
	}
	if got := lc.Sandbox.MidsessionWatch.FullPassConcurrency; got != 0 {
		t.Errorf("FullPassConcurrency = %d, want 0 (unthrottled)", got)
	}
}

// TestMidsessionWatchExplicitZero asserts an explicit full_pass_ticks: 0
// is accepted and stays off, rather than being treated as invalid.
func TestMidsessionWatchExplicitZero(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeConfig(t, filepath.Join(home, ".config", "omac", "config.yaml"), `
sandbox:
  midsession_watch:
    full_pass_ticks: 0
`)
	lc, _, err := LoadLauncher(t.TempDir())
	if err != nil {
		t.Fatalf("LoadLauncher: %v", err)
	}
	if got := lc.Sandbox.MidsessionWatch.FullPassTicks; got != 0 {
		t.Errorf("FullPassTicks = %d, want 0 (off)", got)
	}
}

// TestMidsessionWatchGlobalConfigOverride asserts the user-global config
// can tune the watch, and that an unset field falls back to its default.
func TestMidsessionWatchGlobalConfigOverride(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeConfig(t, filepath.Join(home, ".config", "omac", "config.yaml"), `
sandbox:
  midsession_watch:
    full_pass_ticks: 30
    full_pass_concurrency: 2
`)
	lc, _, err := LoadLauncher(t.TempDir())
	if err != nil {
		t.Fatalf("LoadLauncher: %v", err)
	}
	if got := lc.Sandbox.MidsessionWatch.FullPassTicks; got != 30 {
		t.Errorf("FullPassTicks = %d, want 30", got)
	}
	if got := lc.Sandbox.MidsessionWatch.FullPassConcurrency; got != 2 {
		t.Errorf("FullPassConcurrency = %d, want 2", got)
	}
}

// TestSecurityWorkdirConfigCannotTuneMidsessionWatch asserts the watch
// tuning is a security field: a weaker full-pass cadence widens the
// window in which a mid-session secret file goes unnoticed, so only the
// user-global config may set it — a project config must not.
func TestSecurityWorkdirConfigCannotTuneMidsessionWatch(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	workdir := t.TempDir()
	writeConfig(t, filepath.Join(workdir, ".opencode", "oh-my-agentic-coder.yaml"), `
sandbox:
  midsession_watch:
    full_pass_ticks: 1000000
    full_pass_concurrency: 1
`)
	lc, _, err := LoadLauncher(workdir)
	if err != nil {
		t.Fatalf("LoadLauncher: %v", err)
	}
	if got := lc.Sandbox.MidsessionWatch.FullPassTicks; got != 0 {
		t.Errorf("a workdir config set full_pass_ticks to %d (want the default 0 = off): a project can weaken its own detection bound", got)
	}
	if got := lc.Sandbox.MidsessionWatch.FullPassConcurrency; got != 0 {
		t.Errorf("a workdir config set full_pass_concurrency to %d (want the default 0 = unthrottled)", got)
	}
}

// TestMidsessionWatchRejectsNegative asserts a negative tuning value is
// a config error instead of a silently clamped no-op.
func TestMidsessionWatchRejectsNegative(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want string
	}{
		{"ticks", "full_pass_ticks: -3", "full_pass_ticks"},
		{"concurrency", "full_pass_concurrency: -1", "full_pass_concurrency"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			writeConfig(t, filepath.Join(home, ".config", "omac", "config.yaml"), `
sandbox:
  midsession_watch:
    `+tc.yaml+`
`)
			_, _, err := LoadLauncher(t.TempDir())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("LoadLauncher err = %v, want a %s validation error", err, tc.want)
			}
		})
	}
}
