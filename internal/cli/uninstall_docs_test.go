package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/audit"
	"github.com/TNG/oh-my-agentic-coder/internal/builtinskills"
	"github.com/TNG/oh-my-agentic-coder/internal/config"
	"github.com/TNG/oh-my-agentic-coder/internal/keychain"
	"github.com/TNG/oh-my-agentic-coder/internal/plugin"
	"github.com/TNG/oh-my-agentic-coder/internal/prefs"
	"github.com/TNG/oh-my-agentic-coder/internal/registry"
	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
	"github.com/TNG/oh-my-agentic-coder/internal/sandboxrun"
	"github.com/TNG/oh-my-agentic-coder/internal/skillconfig"
	"github.com/TNG/oh-my-agentic-coder/internal/toolcache"
)

// uninstallDoc lists every place omac writes to, so users can remove it.
const uninstallDoc = "../../docs/getting-started/uninstall.md"

// TestUninstallDocListsEveryLocation catches drift between the uninstall page
// and the code: it asks the code where omac writes and fails when the page
// does not name that place. For example, a new harness or a moved cache
// folder fails here until the page is updated.
//
// It only knows the writers listed below. A new kind of file outside these
// folders needs a new entry here as well as on the page.
func TestUninstallDocListsEveryLocation(t *testing.T) {
	raw, err := os.ReadFile(uninstallDoc)
	if err != nil {
		t.Fatalf("read %s: %v", uninstallDoc, err)
	}
	doc := string(raw)

	// The page writes home as "~" and documents the default locations, so
	// resolve them with a known home and no XDG or harness-home overrides.
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, v := range []string{"XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME"} {
		t.Setenv(v, "")
	}
	for _, h := range config.AllHarnesses() {
		if h.HomeEnv != "" {
			t.Setenv(h.HomeEnv, "")
		}
	}
	tilde := func(p string) string {
		if rel, err := filepath.Rel(home, p); err == nil && !strings.HasPrefix(rel, "..") {
			return filepath.ToSlash(filepath.Join("~", rel))
		}
		return filepath.ToSlash(p)
	}
	must := func(p string, err error) string {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return p
	}

	// want maps a location to the code that writes there, for the failure message.
	want := map[string]string{}
	add := func(path, writer string) { want[tilde(path)] = writer }

	// omac's own folders. The page names the folder; every file inside it
	// must stay under one of them.
	add(registry.GlobalDir(), "registry.GlobalDir")
	add(skillconfig.GlobalDir(), "skillconfig.GlobalDir")
	add(prefs.GlobalDir(), "prefs.GlobalDir")
	add(filepath.Dir(must(sandboxprofile.ProfileDir())), "sandboxprofile.ProfileDir")
	cache, err := toolcache.DescribeShared()
	if err != nil {
		t.Fatal(err)
	}
	add(filepath.Dir(cache.Dir), "toolcache.DescribeShared")
	auditDir, _ := audit.DefaultDir()
	add(filepath.Dir(auditDir), "audit.DefaultDir")
	add(filepath.Dir(must(sandboxrun.DiagLogPath())), "sandboxrun.DiagLogPath")

	// Session folders live under an OS-managed temp root. Setting the root
	// variable to its own name yields the spelling the page uses, e.g.
	// "$XDG_RUNTIME_DIR/omac" on Linux and "$TMPDIR/omac" on macOS.
	t.Setenv("XDG_RUNTIME_DIR", "$XDG_RUNTIME_DIR")
	t.Setenv("TMPDIR", "$TMPDIR")
	add(must(runtimeDirBase()), "runtimeDirBase")

	// Files omac adds to the harnesses' own folders.
	for _, h := range config.AllHarnesses() {
		for _, name := range builtinskills.Names() {
			add(filepath.Join(h.GlobalSkillsDir(), name), "builtinskills.Materialize ("+h.Name+")")
		}
		if h.NeedsPluginBootstrap {
			add(plugin.MultiDirPathIn(h.GlobalBridgeDir()), "ensureOpenCodePlugin ("+h.Name+")")
			add(plugin.MultiDirPath("", h.BridgeDir), "omac plugin install ("+h.Name+")")
		}
	}

	// Per-project files.
	add(registry.Path(""), "registry.Path")
	add(skillconfig.Path(""), "skillconfig.Path")

	// Keychain entries share a service-name prefix.
	add(strings.TrimSuffix(keychain.Service("x"), "x"), "keychain.Service")

	for loc, writer := range want {
		if !strings.Contains(doc, loc) {
			t.Errorf("%s does not mention %q (written by %s); update the page", uninstallDoc, loc, writer)
		}
	}
}
