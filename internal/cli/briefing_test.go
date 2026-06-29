package cli

import (
	"os"
	"testing"

	"github.com/tngtech/oh-my-agentic-coder/internal/config"
)

func claudeHarness(t *testing.T) config.Harness {
	t.Helper()
	h, ok := config.LookupHarness("claude")
	if !ok {
		t.Fatal("claude harness missing")
	}
	return h
}

func TestBriefingInjectionActiveForAgent(t *testing.T) {
	h := claudeHarness(t)
	text, ok := briefingInjection(false, []string{"claude"}, h, "OVERRIDE")
	if !ok {
		t.Fatal("expected injection to be active for the harness's own binary")
	}
	if text != "OVERRIDE" {
		t.Errorf("text = %q; want the override", text)
	}
}

func TestBriefingInjectionSkippedWhenNoSandbox(t *testing.T) {
	h := claudeHarness(t)
	if _, ok := briefingInjection(true, []string{"claude"}, h, ""); ok {
		t.Error("expected injection skipped when noSandbox is true")
	}
}

func TestBriefingInjectionSkippedForNonHarnessBinary(t *testing.T) {
	h := claudeHarness(t)
	if _, ok := briefingInjection(false, []string{"bash"}, h, ""); ok {
		t.Error("expected injection skipped when inner command is not the harness binary")
	}
}

func TestBriefingInjectionSkippedForEmptyInner(t *testing.T) {
	h := claudeHarness(t)
	if _, ok := briefingInjection(false, nil, h, ""); ok {
		t.Error("expected injection skipped for empty inner command")
	}
}

func TestEnsureOpenCodePluginSkipsNonOpenCode(t *testing.T) {
	// Claude has no bridge plugin (GlobalBridgeDir returns ""); the
	// provisioner must be a no-op — no panic, no filesystem write. We use
	// the Claude harness precisely so the test never touches
	// ~/.config/opencode.
	h := claudeHarness(t)
	f, err := os.CreateTemp(t.TempDir(), "stderr-*")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	env := &Env{Stderr: f}
	ensureOpenCodePlugin(env, h) // must simply return for a non-opencode harness
}
