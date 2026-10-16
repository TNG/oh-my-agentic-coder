package cli

import (
	"os"
	"reflect"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
)

func boolPtr(b bool) *bool { return &b }

func lookupHarnessForTest(t *testing.T, name string) config.Harness {
	t.Helper()
	h, ok := config.LookupHarness(name)
	if !ok {
		t.Fatalf("LookupHarness(%q) not found", name)
	}
	return h
}

func TestHerdrResumeArgv(t *testing.T) {
	// The default harness (opencode) needs no token: the hint and the
	// resume command read `omac continue -s <id>`.
	opencode := lookupHarnessForTest(t, "opencode")
	want := []string{"omac", "continue", "-s", "ses_123"}
	if got := herdrResumeArgv(opencode, "ses_123"); !reflect.DeepEqual(got, want) {
		t.Fatalf("herdrResumeArgv(opencode) = %q, want %q", got, want)
	}

	// Non-default harnesses embed their alias token.
	claude := lookupHarnessForTest(t, "claude-code")
	want = []string{"omac", "continue", claude.Aliases[0], "-s", "ses_123"}
	if got := herdrResumeArgv(claude, "ses_123"); !reflect.DeepEqual(got, want) {
		t.Fatalf("herdrResumeArgv(claude-code) = %q, want %q", got, want)
	}

	// Ids herdr would reject (or that name non-resumable sessions) never
	// produce a resume command.
	for _, id := range []string{"", "-flag", "it's", "sidechain-abc"} {
		if got := herdrResumeArgv(claude, id); got != nil {
			t.Fatalf("herdrResumeArgv(claude-code, %q) = %q, want nil", id, got)
		}
	}

	// A harness without resume-by-id support has no bridge resume either.
	if got := herdrResumeArgv(config.Harness{Name: "bare"}, "ses_123"); got != nil {
		t.Fatalf("herdrResumeArgv(no-session harness) = %q, want nil", got)
	}
}

// setHerdrEnv simulates running inside a herdr pane.
func setHerdrEnv(t *testing.T) {
	t.Helper()
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_PANE_ID", "pane-test")
	t.Setenv("HERDR_BIN_PATH", "/usr/bin/herdr")
}

func TestMaybeHerdrBridgeDisabledByConfig(t *testing.T) {
	setHerdrEnv(t)
	opencode := lookupHarnessForTest(t, "opencode")
	lc := config.LauncherConfig{}
	lc.Herdr = config.HerdrConfig{Enabled: boolPtr(false)}
	env := &Env{Workdir: t.TempDir(), Stdout: os.Stdout, Stderr: os.Stderr}
	if b := maybeHerdrBridge(lc, opencode, env, nil, "", false); b != nil {
		t.Fatal("bridge built despite herdr.enabled: false")
	}

	// Default (unset) stays enabled.
	lc.Herdr = config.HerdrConfig{}
	t.Setenv("HOME", t.TempDir())
	if b := maybeHerdrBridge(lc, opencode, env, nil, "", false); b == nil {
		t.Fatal("bridge disabled by default, want enabled")
	}
}

func TestMaybeHerdrBridgeOffOutsideHerdr(t *testing.T) {
	t.Setenv("HERDR_ENV", "") // not inside a pane
	opencode := lookupHarnessForTest(t, "opencode")
	lc := config.LauncherConfig{}
	t.Setenv("HOME", t.TempDir())
	env := &Env{Workdir: t.TempDir(), Stdout: os.Stdout, Stderr: os.Stderr}
	if b := maybeHerdrBridge(lc, opencode, env, nil, "", false); b != nil {
		t.Fatal("bridge built outside a herdr pane, want nil")
	}
}

func TestMaybeHerdrBridgeOffForUnsupportedStore(t *testing.T) {
	setHerdrEnv(t)
	// codex supports resume-by-id but has no host-side watchable store
	// in v1, so the bridge must stay off rather than degrade herdr's
	// own screen detection without a resume command in return.
	codex := lookupHarnessForTest(t, "codex")
	if codex.Session == nil || codex.Session.ResumeByIDArgs == nil {
		t.Fatal("codex fixture lost resume support; test needs rework")
	}
	lc := config.LauncherConfig{}
	t.Setenv("HOME", t.TempDir())
	env := &Env{Workdir: t.TempDir(), Stdout: os.Stdout, Stderr: os.Stderr}
	if b := maybeHerdrBridge(lc, codex, env, nil, "", false); b != nil {
		t.Fatal("bridge built for codex, want nil (no watchable store)")
	}
}

func TestMaybeHerdrBridgeOn(t *testing.T) {
	setHerdrEnv(t)
	t.Setenv("HOME", t.TempDir())
	lc := config.LauncherConfig{}
	env := &Env{Workdir: t.TempDir(), Stdout: os.Stdout, Stderr: os.Stderr}

	for _, name := range []string{"opencode", "claude-code"} {
		h := lookupHarnessForTest(t, name)
		if h.Session == nil || h.Session.ResumeByIDArgs == nil {
			t.Fatalf("%s fixture lost resume support; test needs rework", name)
		}
		if b := maybeHerdrBridge(lc, h, env, nil, "ses_123", false); b == nil {
			t.Fatalf("bridge not built for %s inside a herdr pane", name)
		}
	}
}
