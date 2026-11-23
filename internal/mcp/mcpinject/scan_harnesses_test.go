package mcpinject

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
)

type scanCase struct {
	name      string
	files     map[string]string // relative to the root; "{root}" in a value expands to it
	env       map[string]string // "{root}" in a value expands to the root
	args      []string
	workdir   string // relative to the root
	want      []found
	wantPlug  []string
	wantFinds []string // files (relative to root) reported as findings
}

func runScanCases(t *testing.T, kind config.MCPScanKind, managedFor func(root string) managedDirs, cases []scanCase) {
	t.Helper()
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			home := filepath.Join(root, "home")
			if err := os.MkdirAll(home, 0o755); err != nil {
				t.Fatal(err)
			}
			expand := func(s string) string { return strings.ReplaceAll(s, "{root}", root) }
			for rel, content := range tt.files {
				write(t, filepath.Join(root, rel), expand(content))
			}
			env := map[string]string{}
			for k, v := range tt.env {
				env[k] = expand(v)
			}
			var args []string
			for _, a := range tt.args {
				args = append(args, expand(a))
			}
			workdir := filepath.Join(root, tt.workdir)
			if err := os.MkdirAll(workdir, 0o755); err != nil {
				t.Fatal(err)
			}
			managed := managedDirs{}
			if managedFor != nil {
				managed = managedFor(root)
			}
			host := Host{Workdir: workdir, Home: home, Getenv: fakeEnv(env), InnerArgs: args}
			ex, findings := scanFound(t, kind, host, managed)

			var got []found
			var plugins []string
			for _, e := range foundOf(ex) {
				got = append(got, found{e.name, strings.TrimPrefix(e.file, root+"/")})
			}
			for _, e := range ex {
				if e.PluginProvided {
					plugins = append(plugins, e.Name)
				}
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("found\n got %v\nwant %v", got, tt.want)
			}
			if !reflect.DeepEqual(sorted(plugins), sorted(tt.wantPlug)) {
				t.Errorf("plugin-provided = %v, want %v", plugins, tt.wantPlug)
			}
			var gotFinds []string
			for _, f := range findings {
				gotFinds = append(gotFinds, strings.TrimPrefix(f.File, root+"/"))
			}
			if !reflect.DeepEqual(sorted(gotFinds), sorted(tt.wantFinds)) {
				t.Errorf("findings = %v, want %v", gotFinds, tt.wantFinds)
			}
		})
	}
}

func sorted(in []string) []string {
	out := append([]string(nil), in...)
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

const codexEntry = "[mcp_servers.%s]\ncommand = \"npx\"\nargs = [\"-y\", \"srv\"]\n"

func TestScanCodex(t *testing.T) {
	entry := func(name string) string { return fmt.Sprintf(codexEntry, name) }
	runScanCases(t, config.MCPScanCodex, func(root string) managedDirs {
		return managedDirs{codex: []string{filepath.Join(root, "etc-codex")}}
	}, []scanCase{
		{name: "nothing", want: nil},
		{
			name:    "user, project walk up to the git root, system",
			workdir: "repo/pkg/sub",
			files: map[string]string{
				"home/.codex/config.toml":          entry("user"),
				"repo/.git/HEAD":                   "ref",
				"repo/.codex/config.toml":          entry("root"),
				"repo/pkg/.codex/config.toml":      entry("mid"),
				"repo/pkg/sub/.codex/config.toml":  entry("cwd"),
				".codex/config.toml":               entry("above-git-root"),
				"etc-codex/config.toml":            entry("admin"),
				"repo/pkg/sub/.codex/other.toml":   entry("not-read"),
				"home/.codex/p.config.toml":        entry("profile-not-read"),
				"repo/pkg/.codex/config.toml.orig": entry("not-read"),
			},
			want: []found{
				{"admin", "etc-codex/config.toml"},
				{"cwd", "repo/pkg/sub/.codex/config.toml"},
				{"mid", "repo/pkg/.codex/config.toml"},
				{"root", "repo/.codex/config.toml"},
				{"user", "home/.codex/config.toml"},
			},
		},
		{
			name:    "without a git root only the working directory counts",
			workdir: "repo/sub",
			files: map[string]string{
				"repo/.codex/config.toml":     entry("parent"),
				"repo/sub/.codex/config.toml": entry("cwd"),
			},
			want: []found{{"cwd", "repo/sub/.codex/config.toml"}},
		},
		{
			name:  "CODEX_HOME relocates the user config",
			env:   map[string]string{"CODEX_HOME": "{root}/elsewhere"},
			files: map[string]string{"elsewhere/config.toml": entry("moved"), "home/.codex/config.toml": entry("default")},
			want:  []found{{"moved", "elsewhere/config.toml"}},
		},
		{
			name: "plugin tables, inline tables, dotted keys and disabled entries",
			files: map[string]string{"home/.codex/config.toml": `
model = "x"
[plugins."a@b"]
enabled = true
[plugins."a@b".mcp_servers.fromplugin]
command = "p"
[mcp_servers]
inline = { command = "i", args = ["a"], env = { TOKEN = "t" } }
dotted.command = "d"
[mcp_servers.off]
command = "o"
enabled = false
[mcp_servers.remote]
url = "https://h"
`},
			want: []found{
				{"dotted", "home/.codex/config.toml"}, {"fromplugin", "home/.codex/config.toml"},
				{"inline", "home/.codex/config.toml"}, {"off", "home/.codex/config.toml"}, {"remote", "home/.codex/config.toml"},
			},
			wantPlug: []string{"fromplugin"},
		},
		{
			name:  "root level mcp_servers table in one inline value",
			files: map[string]string{"home/.codex/config.toml": "mcp_servers = { a = { command = \"x\" }, b = { url = \"https://h\" } }\n"},
			want:  []found{{"a", "home/.codex/config.toml"}, {"b", "home/.codex/config.toml"}},
		},
		{
			name:      "unparseable TOML is a finding",
			files:     map[string]string{"home/.codex/config.toml": "[mcp_servers.x\ncommand = 1"},
			wantFinds: []string{"home/.codex/config.toml"},
		},
		{
			name:      "a table that is a value is a finding",
			files:     map[string]string{"home/.codex/config.toml": "mcp_servers = 1\n[mcp_servers.x]\n"},
			wantFinds: []string{"home/.codex/config.toml"},
		},
		{
			name: "-c and --config overrides",
			args: []string{"-c", "mcp_servers.cli.command=\"npx\"", "--config", "mcp_servers.cli.args=[\"a\"]", "--config=mcp_servers.other.url=\"https://h\"", "-c", "model=\"x\"", "-c", "mcp_servers.bare.command=npx"},
			want: []found{{"bare", "-c/--config (command line)"}, {"cli", "-c/--config (command line)"}, {"other", "-c/--config (command line)"}},
		},
	})
}

func TestCodexOverridesMergeIntoOneServer(t *testing.T) {
	ex, _ := scanFound(t, config.MCPScanCodex, Host{Workdir: t.TempDir(), Home: t.TempDir(), InnerArgs: []string{
		"-c", `mcp_servers.cli.command="npx"`, "-c", `mcp_servers.cli.args=["-y","srv"]`, "-c", `mcp_servers.cli.env.TOKEN="secret-value"`,
	}}, managedDirs{})
	if len(ex) != 1 || ex[0].Command != "npx" || !reflect.DeepEqual(ex[0].Args, []string{"-y", "srv"}) || !reflect.DeepEqual(ex[0].EnvKeys, []string{"TOKEN"}) {
		t.Fatalf("got %+v", ex)
	}
	if strings.Contains(fmt.Sprintf("%+v", ex), "secret-value") {
		t.Fatal("a value was kept")
	}
}

func TestScanCodexKeepsNamesAndNeverValues(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "home", ".codex", "config.toml"), `
[mcp_servers.gh]
command = "npx"
args = ["-y", "gh-server"]
env = { GITHUB_TOKEN = "ghp-secret-value" }
env_vars = ["PASS_ME", { name = "ALSO_ME", source = "local" }]
[mcp_servers.web]
url = "https://h/mcp"
bearer_token_env_var = "WEB_TOKEN"
http_headers = { X-Key = "header-secret-value" }
env_http_headers = { X-Other = "OTHER_ENV" }
`)
	ex, f := scanFound(t, config.MCPScanCodex, Host{Workdir: root, Home: filepath.Join(root, "home")}, managedDirs{})
	if len(f) != 0 || len(ex) != 2 {
		t.Fatalf("ex %+v f %+v", ex, f)
	}
	gh, web := ex[0], ex[1]
	if gh.Transport != "stdio" || gh.Command != "npx" || !reflect.DeepEqual(gh.EnvKeys, []string{"GITHUB_TOKEN", "PASS_ME", "ALSO_ME"}) {
		t.Errorf("gh = %+v", gh)
	}
	if web.Transport != "http" || !reflect.DeepEqual(web.HeaderKeys, []string{"X-Key", "X-Other"}) || !reflect.DeepEqual(web.EnvKeys, []string{"WEB_TOKEN"}) {
		t.Errorf("web = %+v", web)
	}
	if dump := fmt.Sprintf("%+v", ex); strings.Contains(dump, "secret-value") {
		t.Errorf("a value was kept: %s", dump)
	}
}

func TestParseTOML(t *testing.T) {
	valid := []string{
		"",
		"# only a comment\n",
		"a = 1\nb = 'lit'\nc = \"esc \\\" \\u00e9\"\nd = [1,\n 2, # note\n]\ne = { x = 1 }\n",
		"\"quoted key\".sub = true\n[a . b]\nk = 1979-05-27T07:32:00Z\n",
		"s = \"\"\"\nmulti\n[mcp_servers.fake]\n\"\"\"\nafter = 1\n",
		"s = '''\n[mcp_servers.fake]\n'''\n[[arr]]\nx = 1\n[[arr]]\nx = 2\n",
		"a = 1\r\nb = 2\r\n",
	}
	for _, src := range valid {
		if _, err := parseTOML([]byte(src)); err != nil {
			t.Errorf("%q: %v", src, err)
		}
	}
	doc, err := parseTOML([]byte("s = \"\"\"\nline\n[mcp_servers.fake]\n\"\"\"\n"))
	if err != nil || doc["mcp_servers"] != nil {
		t.Errorf("a header inside a multi-line string was read as a table: %v %v", doc, err)
	}
	invalid := []string{
		"a", "a =", "a = \"open", "[a", "a = [1", "a = { x = 1", "a = 1 b = 2", "a = 'x'\n[a]\n", "a = \"\\q\"", "= 1", "a = \"x\" junk",
		"\xff = 1",
	}
	for _, src := range invalid {
		if _, err := parseTOML([]byte(src)); err == nil {
			t.Errorf("%q parsed", src)
		}
	}
}

func TestScanCopilot(t *testing.T) {
	entry := func(name string) string {
		return `{"mcpServers":{"` + name + `":{"type":"local","command":"npx","args":["x"],"env":{"T":"v"}}}}`
	}
	runScanCases(t, config.MCPScanCopilot, nil, []scanCase{
		{name: "nothing"},
		{
			name:    "user config, project files up to the git root",
			workdir: "repo/a/b",
			files: map[string]string{
				"home/.copilot/mcp-config.json": entry("user"),
				"repo/.git/HEAD":                "",
				"repo/.mcp.json":                entry("root-mcp"),
				"repo/.github/mcp.json":         entry("root-github"),
				"repo/a/.mcp.json":              `{"bare": {"type":"http","url":"https://h","headers":{"A":"b"}}, "note": "text"}`,
				"repo/a/b/.github/mcp.json":     entry("cwd-github"),
				".mcp.json":                     entry("above-git-root"),
				"repo/.vscode/mcp.json":         `{"servers":{"vscode":{"command":"x"}}}`,
			},
			want: []found{
				{"bare", "repo/a/.mcp.json"}, {"cwd-github", "repo/a/b/.github/mcp.json"},
				{"root-github", "repo/.github/mcp.json"}, {"root-mcp", "repo/.mcp.json"}, {"user", "home/.copilot/mcp-config.json"},
			},
		},
		{
			name:     "COPILOT_HOME relocates the user config and the plugins",
			env:      map[string]string{"COPILOT_HOME": "{root}/ch"},
			files:    map[string]string{"ch/mcp-config.json": entry("moved"), "home/.copilot/mcp-config.json": entry("default"), "ch/installed-plugins/m/p/mcp.json": entry("plug")},
			want:     []found{{"moved", "ch/mcp-config.json"}, {"plug", "ch/installed-plugins/m/p/mcp.json"}},
			wantPlug: []string{"plug"},
		},
		{
			name: "plugin mcp.json files, dependencies skipped",
			files: map[string]string{
				"home/.copilot/installed-plugins/m/p/mcp.json":                entry("plug"),
				"home/.copilot/installed-plugins/m/p/node_modules/x/mcp.json": entry("dependency"),
				"home/.copilot/installed-plugins/m/p/other.json":              entry("other"),
			},
			want:     []found{{"plug", "home/.copilot/installed-plugins/m/p/mcp.json"}},
			wantPlug: []string{"plug"},
		},
		{
			name:  "--additional-mcp-config inline and @file",
			args:  []string{"--additional-mcp-config", `{"mcpServers":{"inline":{"command":"x"}}}`, "--additional-mcp-config=@{root}/extra.json", "prompt"},
			files: map[string]string{"extra.json": entry("fromfile")},
			want:  []found{{"fromfile", "extra.json"}, {"inline", "--additional-mcp-config (inline JSON)"}},
		},
		{
			name:      "unparseable and unreadable files are findings",
			files:     map[string]string{"home/.copilot/mcp-config.json": "{not json", "home/.copilot/installed-plugins/p/mcp.json": `{"mcpServers": 3}`},
			wantFinds: []string{"home/.copilot/mcp-config.json", "home/.copilot/installed-plugins/p/mcp.json"},
			wantPlug:  nil,
		},
	})
}

func TestScanCopilotNeverKeepsValues(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "home", ".copilot", "mcp-config.json"),
		`{"mcpServers":{"a":{"type":"http","url":"https://h","headers":{"Authorization":"Bearer secret-value"}},"b":{"type":"local","command":"x","env":{"K":"secret-value"}}}}`)
	ex, _ := scanFound(t, config.MCPScanCopilot, Host{Workdir: root, Home: filepath.Join(root, "home")}, managedDirs{})
	if len(ex) != 2 || ex[0].Transport != "http" || ex[1].Transport != "stdio" || !reflect.DeepEqual(ex[0].HeaderKeys, []string{"Authorization"}) {
		t.Fatalf("got %+v", ex)
	}
	if strings.Contains(fmt.Sprintf("%+v", ex), "secret-value") {
		t.Fatal("a value was kept")
	}
}

func TestScanPi(t *testing.T) {
	entry := func(name string) string { return `{"mcpServers":{"` + name + `":{"command":"npx","enabled":false}}}` }
	runScanCases(t, config.MCPScanPi, nil, []scanCase{
		{name: "nothing"},
		{
			name:    "user and project, parents are not searched",
			workdir: "repo/sub",
			files: map[string]string{
				"home/.pi/agent/mcp.json": entry("user"),
				"repo/sub/.pi/mcp.json":   `{"servers":{"proj":{"url":"https://h"}}}`,
				"repo/.pi/mcp.json":       entry("parent"),
			},
			want: []found{{"proj", "repo/sub/.pi/mcp.json"}, {"user", "home/.pi/agent/mcp.json"}},
		},
		{
			name:  "PI_CODING_AGENT_DIR relocates the user config",
			env:   map[string]string{"PI_CODING_AGENT_DIR": "{root}/agent"},
			files: map[string]string{"agent/mcp.json": entry("moved"), "home/.pi/agent/mcp.json": entry("default")},
			want:  []found{{"moved", "agent/mcp.json"}},
		},
		{name: "unparseable", files: map[string]string{".pi/mcp.json": "[1"}, wantFinds: []string{".pi/mcp.json"}},
	})
}

func TestScanCodeWhale(t *testing.T) {
	servers := func(key, name string) string {
		return `{"` + key + `":{"` + name + `":{"command":"npx","disabled":true}}}`
	}
	runScanCases(t, config.MCPScanCodeWhale, nil, []scanCase{
		{name: "nothing"},
		{
			name: "servers and mcpServers keys, global and workspace",
			files: map[string]string{
				"home/.codewhale/mcp.json":   servers("servers", "global"),
				"work/.codewhale/mcp.json":   servers("mcpServers", "workspace"),
				"home/.deepseek/mcp.json":    servers("servers", "legacy-ignored"),
				"parent/.codewhale/mcp.json": servers("servers", "parent-ignored"),
			},
			workdir: "work",
			want:    []found{{"global", "home/.codewhale/mcp.json"}, {"workspace", "work/.codewhale/mcp.json"}},
		},
		{
			name:  "the legacy home is read only when the codewhale file is absent",
			files: map[string]string{"home/.deepseek/mcp.json": servers("servers", "legacy")},
			want:  []found{{"legacy", "home/.deepseek/mcp.json"}},
		},
		{
			name:  "path overrides in the environment",
			env:   map[string]string{"CODEWHALE_MCP_CONFIG": "{root}/a.json", "DEEPSEEK_MCP_CONFIG": "{root}/b.json"},
			files: map[string]string{"a.json": servers("servers", "a"), "b.json": servers("mcpServers", "b")},
			want:  []found{{"a", "a.json"}, {"b", "b.json"}},
		},
		{
			name:  "CODEWHALE_HOME is read as well",
			env:   map[string]string{"CODEWHALE_HOME": "{root}/cw"},
			files: map[string]string{"cw/mcp.json": servers("servers", "moved"), "home/.codewhale/mcp.json": servers("servers", "default")},
			want:  []found{{"default", "home/.codewhale/mcp.json"}, {"moved", "cw/mcp.json"}},
		},
		{
			name: "mcp_config_path in config.toml",
			files: map[string]string{
				"home/.codewhale/config.toml": "api_key = \"secret\"\nmcp_config_path = \"{root}/custom/mcp.json\"\n",
				"custom/mcp.json":             servers("servers", "custom"),
			},
			want: []found{{"custom", "custom/mcp.json"}},
		},
		{
			name:      "an unparseable config.toml is a finding",
			files:     map[string]string{"home/.codewhale/config.toml": "mcp_config_path = "},
			wantFinds: []string{"home/.codewhale/config.toml"},
		},
		{
			name:      "both keys in one file do not parse",
			files:     map[string]string{"home/.codewhale/mcp.json": `{"servers":{"a":{}},"mcpServers":{"b":{}}}`},
			wantFinds: []string{"home/.codewhale/mcp.json"},
		},
		{
			name:  "remote entry with env headers",
			files: map[string]string{"home/.codewhale/mcp.json": `{"servers":{"r":{"url":"https://h","transport":"sse","env_headers":{"X":"V"},"bearer_token_env_var":"TOK"}}}`},
			want:  []found{{"r", "home/.codewhale/mcp.json"}},
		},
	})
}

func TestScanClaudePluginDirectories(t *testing.T) {
	entry := func(name string) string { return `{"mcpServers":{"` + name + `":{"command":"npx"}}}` }
	runScanCases(t, config.MCPScanClaude, nil, []scanCase{
		{
			name: "synced bundles are scanned, marketplaces are not",
			files: map[string]string{
				"home/.claude/plugins/synced/u1/u2/.mcp.json":                      `{"asana":{"type":"http","url":"https://h"},"github":{"command":"x"}}`,
				"home/.claude/plugins/synced/u1/u3/.mcp.json":                      entry("wrapped"),
				"home/.claude/plugins/synced/u1/u4/.claude-plugin/plugin.json":     `{"name":"p","mcpServers":{"inline":{"command":"x"}}}`,
				"home/.claude/plugins/synced/u1/u5/node_modules/x/.mcp.json":       entry("dependency"),
				"home/.claude/plugins/marketplaces/m/p/.mcp.json":                  entry("catalog"),
				"home/.claude/plugins/marketplaces/m/p/.claude-plugin/plugin.json": `{"mcpServers":{"catalog2":{"command":"x"}}}`,
			},
			want: []found{
				{"asana", "home/.claude/plugins/synced/u1/u2/.mcp.json"}, {"github", "home/.claude/plugins/synced/u1/u2/.mcp.json"},
				{"inline", "home/.claude/plugins/synced/u1/u4/.claude-plugin/plugin.json"},
				{"wrapped", "home/.claude/plugins/synced/u1/u3/.mcp.json"},
			},
			wantPlug: []string{"asana", "github", "inline", "wrapped"},
		},
		{
			name:     "CLAUDE_CONFIG_DIR plugins",
			env:      map[string]string{"CLAUDE_CONFIG_DIR": "{root}/cfg"},
			files:    map[string]string{"cfg/plugins/synced/a/.mcp.json": entry("moved")},
			want:     []found{{"moved", "cfg/plugins/synced/a/.mcp.json"}},
			wantPlug: []string{"moved"},
		},
		{
			name: "installed_plugins.json install paths",
			files: map[string]string{
				"home/.claude/plugins/installed_plugins.json": `{"version":2,"plugins":{"p@m":[{"scope":"user","installPath":"{root}/cache/p","version":"1"}],"q@m":[{"installPath":"{root}/cache/q"}]}}`,
				"cache/p/.mcp.json":                           entry("fromp"),
				"cache/q/.claude-plugin/plugin.json":          `{"mcpServers":{"fromq":{"url":"https://h"}}}`,
				"cache/unlisted/.mcp.json":                    entry("unlisted"),
			},
			want:     []found{{"fromp", "cache/p/.mcp.json"}, {"fromq", "cache/q/.claude-plugin/plugin.json"}},
			wantPlug: []string{"fromp", "fromq"},
		},
		{
			name:  "installed_plugins.json with a missing install directory",
			files: map[string]string{"home/.claude/plugins/installed_plugins.json": `{"plugins":{"p@m":[{"installPath":"{root}/gone"}]}}`},
		},
		{
			name:      "installed_plugins.json that is not JSON",
			files:     map[string]string{"home/.claude/plugins/installed_plugins.json": "{oops"},
			wantFinds: []string{"home/.claude/plugins/installed_plugins.json"},
		},
		{
			name:      "installed_plugins.json of an unknown shape",
			files:     map[string]string{"home/.claude/plugins/installed_plugins.json": `{"plugins":{"p@m":{"dir":"x"}}}`},
			wantFinds: []string{"home/.claude/plugins/installed_plugins.json"},
		},
		{
			name:      "installed_plugins.json that is not an object",
			files:     map[string]string{"home/.claude/plugins/installed_plugins.json": `[1,2]`},
			wantFinds: []string{"home/.claude/plugins/installed_plugins.json"},
		},
		{
			name:  "installed_plugins.json without plugins",
			files: map[string]string{"home/.claude/plugins/installed_plugins.json": `{"plugins":{}}`},
		},
		{
			name:      "a malformed bundle file is a plugin-provided finding",
			files:     map[string]string{"home/.claude/plugins/synced/a/.mcp.json": "{nope"},
			wantFinds: []string{"home/.claude/plugins/synced/a/.mcp.json"},
		},
	})
}

func TestPluginProvidedIsReported(t *testing.T) {
	policy := Policy{Unregistered: ModeRefuse, Active: true}
	_, refusal := Evaluate(policy,
		[]Existing{{Harness: "claude-code", File: "/p/.mcp.json", Name: "asana", PluginProvided: true}},
		[]Finding{{File: "/q/.mcp.json", Kind: FindingParseError, Detail: "bad", PluginProvided: true}})
	if refusal == nil || strings.Count(refusal.Error(), "(provided by a plugin)") != 2 {
		t.Fatalf("refusal = %v", refusal)
	}
}

func TestEvaluateUnsupported(t *testing.T) {
	entries := []Existing{{Harness: "pi", File: "/p/config.toml", Name: "native", Command: "npx"}}
	message := "omac cannot provide MCP servers to pi yet (supported: " + strings.Join(config.MCPCapableHarnessNames(), ", ") + "). Registered servers that will not be available: a, b."
	tests := []struct {
		name        string
		policy      Policy
		registered  []string
		entries     []Existing
		wantWarn    []string
		wantRefusal []string
		refusalNot  []string
	}{
		{name: "inactive", policy: Policy{}, registered: []string{"a"}},
		{name: "allow is silent", policy: Policy{Unregistered: ModeAllow, Active: true}, registered: []string{"a"}, entries: entries},
		{
			name: "refuse", policy: Policy{Unregistered: ModeRefuse, Active: true}, registered: []string{"a", "b"},
			wantRefusal: []string{message, "Use a supported harness, or run once with --mcp-unregistered=warn", "--mcp-unregistered=allow"},
			refusalNot:  []string{"omac mcp register", "omac mcp add"},
		},
		{
			name: "refuse with a native entry", policy: Policy{Unregistered: ModeRefuse, Active: true}, registered: []string{"a", "b"}, entries: entries,
			wantRefusal: []string{message, `unregistered MCP server "native" in /p/config.toml`, "rotate"},
			refusalNot:  []string{"omac mcp add"},
		},
		{name: "warn", policy: Policy{Unregistered: ModeWarn, Active: true}, registered: []string{"a", "b"}, wantWarn: []string{message}},
		{
			name: "explicit mode with nothing registered and nothing found", policy: Policy{Unregistered: ModeRefuse, Active: true},
		},
		{
			name: "explicit mode with nothing registered and an entry", policy: Policy{Unregistered: ModeRefuse, Active: true}, entries: entries,
			wantRefusal: []string{`unregistered MCP server "native"`}, refusalNot: []string{"cannot provide"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			warnings, refusal := EvaluateUnsupported(tt.policy, "pi", tt.registered, tt.entries, nil)
			if len(tt.wantWarn) == 0 && len(warnings) != 0 {
				t.Errorf("warnings = %v", warnings)
			}
			for _, want := range tt.wantWarn {
				if !strings.Contains(strings.Join(warnings, "\n"), want) {
					t.Errorf("warnings lack %q: %v", want, warnings)
				}
			}
			if len(tt.wantRefusal) == 0 {
				if refusal != nil {
					t.Errorf("refusal = %v", refusal)
				}
				return
			}
			if refusal == nil {
				t.Fatal("no refusal")
			}
			for _, want := range tt.wantRefusal {
				if !strings.Contains(refusal.Error(), want) {
					t.Errorf("refusal lacks %q:\n%s", want, refusal)
				}
			}
			for _, not := range tt.refusalNot {
				if strings.Contains(refusal.Error(), not) {
					t.Errorf("refusal contains %q:\n%s", not, refusal)
				}
			}
		})
	}
}

func TestAddCommandIsEmptyForHarnessesWithoutMCPSupport(t *testing.T) {
	if got := AddCommand(Existing{Harness: "pi", Name: "x", Command: "npx"}); got != "" {
		t.Errorf("AddCommand = %q", got)
	}
}

func TestEveryHarnessHasAScanKind(t *testing.T) {
	for _, h := range config.AllHarnesses() {
		if h.MCPScan == config.MCPScanNone {
			t.Errorf("harness %s has no MCP scan kind", h.Name)
		}
		if _, _, err := Scan(h.MCPScan, Host{Workdir: t.TempDir(), Home: t.TempDir()}); err != nil {
			t.Errorf("%s: %v", h.Name, err)
		}
	}
}
