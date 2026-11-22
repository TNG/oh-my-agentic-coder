package mcpinject

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
)

func hostFor(workdir, home string) Host {
	return Host{Workdir: workdir, Home: home, Getenv: os.Getenv}
}

func fakeEnv(vars map[string]string) func(string) string {
	return func(name string) string { return vars[name] }
}

type found struct{ name, file string }

func scanFound(t *testing.T, kind config.MCPScanKind, host Host, managed managedDirs) ([]Existing, []Finding) {
	t.Helper()
	ex, f, err := scan(kind, host, managed)
	if err != nil {
		t.Fatal(err)
	}
	return ex, f
}

func foundOf(ex []Existing) []found {
	var out []found
	for _, e := range ex {
		out = append(out, found{e.Name, e.File})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

func TestScanOpenCodeLocations(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	repo := filepath.Join(root, "repo")
	workdir := filepath.Join(repo, "sub", "dir")
	custom := filepath.Join(root, "custom.json")
	customDir := filepath.Join(root, "customdir")
	managed := filepath.Join(root, "managed")
	local := `{"mcp":{"%s":{"type":"local","command":["x"]}}}`
	entry := func(name string) string { return strings.Replace(local, "%s", name, 1) }

	write(t, filepath.Join(home, ".config", "opencode", "opencode.json"), entry("global"))
	write(t, filepath.Join(repo, "opencode.jsonc"), entry("parent"))
	write(t, filepath.Join(repo, "sub", ".opencode", "opencode.json"), entry("dotdir"))
	write(t, filepath.Join(workdir, "opencode.json"), `{"mcp":{"v2":{"type":"remote","url":"https://h"}}}`)
	write(t, custom, entry("envfile"))
	write(t, filepath.Join(customDir, "opencode.json"), entry("envdir"))
	write(t, filepath.Join(managed, "opencode.json"), entry("admin"))

	host := Host{Workdir: workdir, Home: home, Getenv: fakeEnv(map[string]string{
		"OPENCODE_CONFIG":         custom,
		"OPENCODE_CONFIG_DIR":     customDir,
		"OPENCODE_CONFIG_CONTENT": `{"mcp":{"servers":{"inline":{"type":"local","command":["i"],"environment":{"K":"secret"}}}}}`,
	})}
	ex, f := scanFound(t, config.MCPScanOpenCode, host, managedDirs{openCode: []string{managed}})
	if len(f) != 0 {
		t.Fatalf("findings = %+v", f)
	}
	want := []found{
		{"admin", filepath.Join(managed, "opencode.json")},
		{"dotdir", filepath.Join(repo, "sub", ".opencode", "opencode.json")},
		{"envdir", filepath.Join(customDir, "opencode.json")},
		{"envfile", custom},
		{"global", filepath.Join(home, ".config", "opencode", "opencode.json")},
		{"inline", openCodeConfigContentLabel},
		{"parent", filepath.Join(repo, "opencode.jsonc")},
		{"v2", filepath.Join(workdir, "opencode.json")},
	}
	if got := foundOf(ex); !reflect.DeepEqual(got, want) {
		t.Fatalf("found = %v\nwant %v", got, want)
	}
	for _, e := range ex {
		if e.Managed != (e.Name == "admin") {
			t.Errorf("%s: Managed = %v", e.Name, e.Managed)
		}
		if e.Name == "inline" && !reflect.DeepEqual(e.EnvKeys, []string{"K"}) {
			t.Errorf("inline env keys = %v", e.EnvKeys)
		}
	}
}

func TestScanOpenCodeShapes(t *testing.T) {
	tests := []struct {
		name string
		json string
		want []string
	}{
		{"v1 entries", `{"mcp":{"a":{"type":"local","command":["x"]},"b":{"type":"remote","url":"https://h"}}}`, []string{"a", "b"}},
		{"v2 servers", `{"mcp":{"servers":{"c":{"type":"local","command":["x"]}},"timeout":{"catalog":5}}}`, []string{"c"}},
		{"v1 server named servers", `{"mcp":{"servers":{"type":"remote","url":"https://h"}}}`, []string{"servers"}},
		{"enabled only is no server", `{"mcp":{"jira":{"enabled":true}}}`, nil},
		{"disabled entry still listed", `{"mcp":{"d":{"type":"local","command":["x"],"enabled":false}}}`, []string{"d"}},
		{"no mcp", `{"model":"x"}`, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workdir := t.TempDir()
			write(t, filepath.Join(workdir, "opencode.json"), tt.json)
			ex, f := scanFound(t, config.MCPScanOpenCode, Host{Workdir: workdir, Home: t.TempDir()}, managedDirs{})
			if len(f) != 0 {
				t.Fatalf("findings = %+v", f)
			}
			var got []string
			for _, e := range foundOf(ex) {
				got = append(got, e.name)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("names = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestScanClaudeLocations(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	repo := filepath.Join(root, "repo")
	workdir := filepath.Join(repo, "pkg")
	configDir := filepath.Join(root, "claudecfg")
	managed := filepath.Join(root, "managed")
	extra := filepath.Join(root, "extra.json")
	addDir := filepath.Join(root, "adddir")
	pluginDir := filepath.Join(root, "plugin")
	srv := func(name string) string { return `{"mcpServers":{"` + name + `":{"command":"x"}}}` }

	write(t, filepath.Join(home, ".claude.json"), `{"projects":{"/other":{"mcpServers":{"foreign":{"command":"y"}}}}}`)
	write(t, filepath.Join(configDir, ".claude.json"), srv("relocated"))
	write(t, filepath.Join(repo, ".mcp.json"), srv("parentproject"))
	write(t, filepath.Join(managed, "managed-mcp.json"), srv("fixed"))
	write(t, filepath.Join(managed, "managed-settings.json"), `{"managedMcpServers":{"provided":{"type":"http","url":"https://h"}}}`)
	write(t, extra, srv("fromfile"))
	write(t, filepath.Join(repo, ".claude", "agents", "sub", "a.md"),
		"---\nname: a\nmcpServers:\n  - playwright:\n      type: stdio\n      command: npx\n      env:\n        TOKEN: secret\n  - github\n---\nbody\n")
	write(t, filepath.Join(configDir, "agents", "u.md"), "---\nname: u\nmcpServers:\n  - useragent:\n      command: z\n---\n")
	write(t, filepath.Join(addDir, ".claude", "agents", "d.md"), "---\nmcpServers:\n  - fromadddir:\n      command: z\n---\n")
	write(t, filepath.Join(pluginDir, ".mcp.json"), srv("pluginroot"))
	write(t, filepath.Join(pluginDir, ".claude-plugin", "plugin.json"),
		`{"mcpServers":{"inlineplugin":{"command":"p"}}}`)

	host := Host{Workdir: workdir, Home: home,
		Getenv: fakeEnv(map[string]string{"CLAUDE_CONFIG_DIR": configDir}),
		InnerArgs: []string{"--model", "x", "--mcp-config", extra, `{"mcpServers":{"inline":{"command":"i"}}}`,
			"--add-dir", addDir, "--agents", `{"rev":{"description":"d","mcpServers":[{"cli":{"command":"c"}}]}}`,
			"--plugin-dir=" + pluginDir, "-p", "prompt"},
	}
	ex, f := scanFound(t, config.MCPScanClaude, host, managedDirs{claude: []string{managed}})
	if len(f) != 0 {
		t.Fatalf("findings = %+v", f)
	}
	got := map[string]Existing{}
	for _, e := range ex {
		got[e.Name] = e
	}
	wantFiles := map[string]string{
		"relocated":     filepath.Join(configDir, ".claude.json"),
		"parentproject": filepath.Join(repo, ".mcp.json"),
		"fixed":         filepath.Join(managed, "managed-mcp.json"),
		"provided":      filepath.Join(managed, "managed-settings.json"),
		"fromfile":      extra,
		"inline":        "--mcp-config (inline JSON)",
		"playwright":    filepath.Join(repo, ".claude", "agents", "sub", "a.md"),
		"useragent":     filepath.Join(configDir, "agents", "u.md"),
		"fromadddir":    filepath.Join(addDir, ".claude", "agents", "d.md"),
		"cli":           "--agents (inline JSON)",
		"pluginroot":    filepath.Join(pluginDir, ".mcp.json"),
		"inlineplugin":  filepath.Join(pluginDir, ".claude-plugin", "plugin.json"),
	}
	for name, file := range wantFiles {
		if e, ok := got[name]; !ok || e.File != file {
			t.Errorf("%s: got %+v, want file %s", name, e, file)
		}
	}
	if len(got) != len(wantFiles) {
		t.Errorf("found %d servers, want %d (foreign project or string reference leaked?): %v", len(got), len(wantFiles), foundOf(ex))
	}
	for name, e := range got {
		wantManaged := name == "fixed" || name == "provided"
		if e.Managed != wantManaged {
			t.Errorf("%s: Managed = %v", name, e.Managed)
		}
	}
	if !reflect.DeepEqual(got["playwright"].EnvKeys, []string{"TOKEN"}) {
		t.Errorf("playwright env keys = %v", got["playwright"].EnvKeys)
	}
}

func TestFlagValues(t *testing.T) {
	args := []string{"--mcp-config", "a.json", "b.json", "--x", "--mcp-config=c.json", "--mcp-config", "-p"}
	if got, want := flagValues(args, "--mcp-config"), []string{"a.json", "b.json", "c.json"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("flagValues = %v, want %v", got, want)
	}
}

func TestScanFindingsForBrokenFiles(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every file")
	}
	root := t.TempDir()
	workdir := filepath.Join(root, "work")
	managed := filepath.Join(root, "managed")
	write(t, filepath.Join(workdir, ".mcp.json"), `{ not json`)
	write(t, filepath.Join(managed, "managed-mcp.json"), `{"mcpServers":{}}`)
	if err := os.Chmod(filepath.Join(managed, "managed-mcp.json"), 0o000); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(workdir, ".claude", "agents", "bad.md"), "---\nmcpServers: [unclosed\n---\n")
	host := Host{Workdir: workdir, Home: filepath.Join(root, "home"), InnerArgs: []string{"--mcp-config", `{"mcpServers": `}}
	_, f := scanFound(t, config.MCPScanClaude, host, managedDirs{claude: []string{managed}})
	kinds := map[string]string{}
	managedFlag := map[string]bool{}
	for _, finding := range f {
		kinds[finding.File] = finding.Kind
		managedFlag[finding.File] = finding.Managed
	}
	want := map[string]string{
		filepath.Join(workdir, ".mcp.json"):                   FindingParseError,
		filepath.Join(managed, "managed-mcp.json"):            FindingUnreadable,
		filepath.Join(workdir, ".claude", "agents", "bad.md"): FindingParseError,
		"--mcp-config (inline JSON)":                          FindingParseError,
	}
	if !reflect.DeepEqual(kinds, want) {
		t.Fatalf("findings = %v, want %v", kinds, want)
	}
	if !managedFlag[filepath.Join(managed, "managed-mcp.json")] {
		t.Error("unreadable managed file is not marked managed")
	}
}

func TestEvaluateExplainsManagedFiles(t *testing.T) {
	ex := []Existing{{Harness: "claude-code", File: "/etc/claude-code/managed-mcp.json", Name: "fixed", Managed: true}}
	_, refusal := Evaluate(Policy{Unregistered: ModeRefuse, Active: true}, ex, nil)
	if refusal == nil || !strings.Contains(refusal.Error(), "administrator-managed") {
		t.Fatalf("refusal = %v", refusal)
	}
}
