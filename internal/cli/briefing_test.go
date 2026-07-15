package cli

import (
	"os"
	"strings"
	"testing"

	"github.com/tngtech/oh-my-agentic-coder/internal/config"
	"github.com/tngtech/oh-my-agentic-coder/internal/sandboxbrief"
	"github.com/tngtech/oh-my-agentic-coder/internal/toolcache"
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
	text, ok := briefingInjection(false, []string{"claude"}, h, "OVERRIDE", nil)
	if !ok {
		t.Fatal("expected injection to be active for the harness's own binary")
	}
	if !strings.HasPrefix(text, "OVERRIDE") {
		t.Errorf("text = %q; want the override as prefix", text)
	}
	if !strings.Contains(text, "OMAC_CACHE_DIR") {
		t.Errorf("text should append cache guidance; got:\n%s", text)
	}
}

func TestBriefingInjectionAcceptsCacheScope(t *testing.T) {
	h := claudeHarness(t)
	scope := &toolcache.Scope{Mode: toolcache.ModeEphemeral, Dir: "/sandbox/cache"}
	text, ok := briefingInjection(false, []string{"claude"}, h, "OVERRIDE", scope)
	if !ok {
		t.Fatal("expected injection to be active for the harness's own binary")
	}
	if !strings.HasPrefix(text, "OVERRIDE") {
		t.Errorf("text = %q; want the override as prefix", text)
	}
	if !strings.Contains(text, "/sandbox/cache") {
		t.Errorf("text should name the actual cache path; got:\n%s", text)
	}
	if !strings.Contains(text, "ephemeral") {
		t.Errorf("text should name the actual cache mode; got:\n%s", text)
	}
}

func TestBriefingInjectionAppendsCacheGuidanceAfterDefault(t *testing.T) {
	h := claudeHarness(t)
	text, ok := briefingInjection(false, []string{"claude"}, h, "", nil)
	if !ok {
		t.Fatal("expected injection to be active for the harness's own binary")
	}
	if !strings.HasPrefix(text, sandboxbrief.Default()) {
		t.Errorf("text should start with the default briefing; got prefix:\n%q", text[:min(len(text), len(sandboxbrief.Default()))])
	}
	if !strings.Contains(text, "OMAC_CACHE_DIR") {
		t.Errorf("default briefing should be followed by cache guidance; got:\n%s", text)
	}
}

func TestBriefingInjectionAppendsCacheGuidanceAfterCustom(t *testing.T) {
	h := claudeHarness(t)
	const custom = "CUSTOM BRIEFING TEXT"
	text, ok := briefingInjection(false, []string{"claude"}, h, custom, nil)
	if !ok {
		t.Fatal("expected injection to be active")
	}
	if !strings.HasPrefix(text, custom) {
		t.Errorf("text should start with custom briefing; got:\n%s", text)
	}
	if !strings.Contains(text, "OMAC_CACHE_DIR") {
		t.Errorf("custom briefing should be followed by cache guidance; got:\n%s", text)
	}
}

func TestBriefingInjectionSkippedWhenNoSandbox(t *testing.T) {
	h := claudeHarness(t)
	if _, ok := briefingInjection(true, []string{"claude"}, h, "", nil); ok {
		t.Error("expected injection skipped when noSandbox is true")
	}
}

func TestBriefingInjectionSkippedForNonHarnessBinary(t *testing.T) {
	h := claudeHarness(t)
	if _, ok := briefingInjection(false, []string{"bash"}, h, "", nil); ok {
		t.Error("expected injection skipped when inner command is not the harness binary")
	}
}

func TestBriefingInjectionSkippedForEmptyInner(t *testing.T) {
	h := claudeHarness(t)
	if _, ok := briefingInjection(false, nil, h, "", nil); ok {
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
