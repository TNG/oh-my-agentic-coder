package cli

import (
	"os"
	"path/filepath"
	"runtime"
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

// uninstallHarnessSection names the uninstall page section for a harness.
// A harness missing here belongs under "Other harnesses".
var uninstallHarnessSection = map[string]string{
	"opencode":    "OpenCode",
	"claude-code": "Claude Code",
}

// TestUninstallDocListsEveryLocation catches drift between the uninstall page
// and the code: it asks the code where omac writes and fails when the page
// does not name that place in the right section. For example, a new harness
// fails until "Other harnesses" lists its skills folder, and the macOS CI job
// fails if the macOS audit log path moves.
//
// It only knows the writers listed below. A new kind of file outside these
// folders needs a new entry here as well as on the page.
func TestUninstallDocListsEveryLocation(t *testing.T) {
	raw, err := os.ReadFile(uninstallDoc)
	if err != nil {
		t.Fatalf("read %s: %v", uninstallDoc, err)
	}
	doc := string(raw)

	// The page writes home as "~" and a redirected folder as "$VAR/...".
	// redirect points VAR at a known folder, so show can spell it that way.
	home := t.TempDir()
	t.Setenv("HOME", home)
	redirectRoot := t.TempDir()
	redirected := map[string]bool{}
	redirect := func(v string) {
		t.Setenv(v, filepath.Join(redirectRoot, v))
		redirected[v] = true
	}
	unset := func(vars ...string) {
		for _, v := range vars {
			t.Setenv(v, "")
			delete(redirected, v)
		}
	}
	show := func(p string) string {
		for v := range redirected {
			if rel, err := filepath.Rel(filepath.Join(redirectRoot, v), p); err == nil && !strings.HasPrefix(rel, "..") {
				return filepath.ToSlash(filepath.Join("$"+v, rel))
			}
		}
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
	check := func(section, path, writer string) {
		t.Helper()
		body, ok := markdownSection(doc, section)
		if !ok {
			t.Errorf("%s has no section %q", uninstallDoc, section)
			return
		}
		if loc := show(path); !strings.Contains(body, loc) {
			t.Errorf("%s: section %q does not mention %q (written by %s); update the page", uninstallDoc, section, loc, writer)
		}
	}

	harnessHomes := []string{"XDG_CONFIG_HOME"}
	for _, h := range config.AllHarnesses() {
		if h.HomeEnv != "" {
			harnessHomes = append(harnessHomes, h.HomeEnv)
		}
	}
	unset(append(harnessHomes, "XDG_STATE_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_RUNTIME_DIR")...)

	// omac's own folders, at their defaults and with XDG_CONFIG_HOME set.
	// The page names the folder; every file inside it must stay under one
	// of them.
	const general = "On every system"
	cache, err := toolcache.DescribeShared()
	if err != nil {
		t.Fatal(err)
	}
	check(general, filepath.Dir(cache.Dir), "toolcache.DescribeShared")
	check(general, filepath.Dir(must(sandboxrun.DiagLogPath())), "sandboxrun.DiagLogPath")
	check(general, filepath.Dir(must(sandboxprofile.ProfileDir())), "sandboxprofile.ProfileDir")
	for _, xdg := range []bool{false, true} {
		if xdg {
			redirect("XDG_CONFIG_HOME")
		}
		check(general, registry.GlobalDir(), "registry.GlobalDir")
		check(general, skillconfig.GlobalDir(), "skillconfig.GlobalDir")
		check(general, prefs.GlobalDir(), "prefs.GlobalDir")
	}
	unset("XDG_CONFIG_HOME")
	check(general, registry.Path(""), "registry.Path")
	check(general, skillconfig.Path(""), "skillconfig.Path")
	check(general, strings.TrimSuffix(keychain.Service("x"), "x"), "keychain.Service")

	// OS-specific places: checked on the OS the test runs on, in that OS's
	// section, with and without the variable that moves them.
	osSection := "Linux"
	if runtime.GOOS == "darwin" {
		osSection = "macOS"
		redirect("TMPDIR")
		check(osSection, must(runtimeDirBase()), "runtimeDirBase")
	}
	for _, xdg := range []bool{false, true} {
		if xdg {
			redirect("XDG_STATE_HOME")
			redirect("XDG_RUNTIME_DIR")
		}
		auditDir, _ := audit.DefaultDir()
		check(osSection, auditDir, "audit.DefaultDir")
		if runtime.GOOS != "darwin" {
			check(osSection, must(runtimeDirBase()), "runtimeDirBase")
		}
	}
	unset("XDG_STATE_HOME", "XDG_RUNTIME_DIR")

	// Files omac adds to a harness's own folders, in that harness's section,
	// with and without its folder variable.
	for _, h := range config.AllHarnesses() {
		section, ok := uninstallHarnessSection[h.Name]
		if !ok {
			section = "Other harnesses"
		}
		for _, moved := range []bool{false, true} {
			if moved {
				for _, v := range harnessHomes {
					redirect(v)
				}
			}
			for _, name := range builtinskills.Names() {
				check(section, filepath.Join(h.GlobalSkillsDir(), name), "builtinskills.Materialize ("+h.Name+")")
			}
			if h.NeedsPluginBootstrap {
				check(section, plugin.MultiDirPathIn(h.GlobalBridgeDir()), "ensureOpenCodePlugin ("+h.Name+")")
			}
		}
		unset(harnessHomes...)
		if h.NeedsPluginBootstrap {
			check(section, plugin.MultiDirPath("", h.BridgeDir), "omac plugin install ("+h.Name+")")
		}
	}
}

// markdownSection returns the body under the first heading whose text is
// heading, up to the next heading of the same or a higher level.
func markdownSection(doc, heading string) (string, bool) {
	lines := strings.Split(doc, "\n")
	inFence := false
	start, level := -1, 0
	for i, line := range lines {
		if strings.HasPrefix(line, "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		hashes := len(line) - len(strings.TrimLeft(line, "#"))
		if hashes == 0 || !strings.HasPrefix(line[hashes:], " ") {
			continue
		}
		if start >= 0 && hashes <= level {
			return strings.Join(lines[start:i], "\n"), true
		}
		if start < 0 && strings.TrimSpace(line[hashes:]) == heading {
			start, level = i+1, hashes
		}
	}
	if start < 0 {
		return "", false
	}
	return strings.Join(lines[start:], "\n"), true
}
