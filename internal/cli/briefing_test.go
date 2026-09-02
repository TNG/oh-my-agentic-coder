package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
	"github.com/TNG/oh-my-agentic-coder/internal/sandboxbrief"
	"github.com/TNG/oh-my-agentic-coder/internal/toolcache"
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

func TestGitExcludeBriefingAppendsAndIsIdempotent(t *testing.T) {
	wd := t.TempDir()
	if err := os.MkdirAll(filepath.Join(wd, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	rel := filepath.Join(".codewhale", "rules", "omac-sandbox-briefing.md")

	gitExcludeBriefing(wd, rel)
	gitExcludeBriefing(wd, rel) // second call must not duplicate

	excludePath := filepath.Join(wd, ".git", "info", "exclude")
	data, err := os.ReadFile(excludePath)
	if err != nil {
		t.Fatalf("exclude not written: %v", err)
	}
	want := filepath.ToSlash(rel)
	n := strings.Count(string(data), want)
	if n != 1 {
		t.Errorf("exclude contains %d copies of %q, want exactly 1:\n%s", n, want, data)
	}
}

func TestGitExcludeBriefingNoOpWithoutGitDir(t *testing.T) {
	wd := t.TempDir() // no .git
	gitExcludeBriefing(wd, ".codewhale/rules/omac-sandbox-briefing.md")
	if _, err := os.Stat(filepath.Join(wd, ".git")); !os.IsNotExist(err) {
		t.Errorf("gitExcludeBriefing must not create .git when absent (err=%v)", err)
	}
}

// TestGitExcludeBriefingPreservesExistingEntries ensures a user's own
// .git/info/exclude content is not clobbered.
func TestGitExcludeBriefingPreservesExistingEntries(t *testing.T) {
	wd := t.TempDir()
	infoDir := filepath.Join(wd, ".git", "info")
	if err := os.MkdirAll(infoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(infoDir, "exclude"), []byte("*.tmp\nbuild/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitExcludeBriefing(wd, ".codewhale/rules/omac-sandbox-briefing.md")
	data, err := os.ReadFile(filepath.Join(infoDir, "exclude"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	for _, want := range []string{"*.tmp", "build/", ".codewhale/rules/omac-sandbox-briefing.md"} {
		if !strings.Contains(s, want) {
			t.Errorf("exclude missing %q:\n%s", want, s)
		}
	}
}

func TestRemoveBriefingFilePrunesEmptyDirsOnly(t *testing.T) {
	wd := t.TempDir()
	rulesDir := filepath.Join(wd, ".codewhale", "rules")
	if err := os.MkdirAll(rulesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(rulesDir, "omac-sandbox-briefing.md")
	if err := os.WriteFile(path, []byte("BRIEF"), 0o644); err != nil {
		t.Fatal(err)
	}
	removeBriefingFile(path)
	// File and the now-empty .codewhale/rules + .codewhale dirs are gone.
	if _, err := os.Stat(filepath.Join(wd, ".codewhale")); !os.IsNotExist(err) {
		t.Errorf(".codewhale should be pruned when empty (err=%v)", err)
	}

	// A non-empty .codewhale (user content) must survive.
	if err := os.MkdirAll(rulesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("BRIEF"), 0o644); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(wd, ".codewhale", "constitution.json")
	if err := os.WriteFile(keep, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	removeBriefingFile(path)
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("user file under .codewhale must survive: %v", err)
	}
}
