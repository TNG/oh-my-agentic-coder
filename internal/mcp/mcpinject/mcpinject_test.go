package mcpinject

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
)

func TestResolvePolicy(t *testing.T) {
	tests := []struct {
		name       string
		cfg        config.MCPConfig
		flag       string
		registered int
		want       Policy
		wantErr    bool
	}{
		{"nothing registered, nothing set", config.MCPConfig{}, "", 0, Policy{ModeRefuse, CredRefuse, false}, false},
		{"registered activates defaults", config.MCPConfig{}, "", 1, Policy{ModeRefuse, CredRefuse, true}, false},
		{"config unregistered activates", config.MCPConfig{Unregistered: "warn"}, "", 0, Policy{ModeWarn, CredRefuse, true}, false},
		{"config credentials activates", config.MCPConfig{LiteralCredentials: "allow"}, "", 0, Policy{ModeRefuse, CredAllow, true}, false},
		{"flag overrides config", config.MCPConfig{Unregistered: "warn"}, "ignore", 0, Policy{ModeIgnore, CredRefuse, true}, false},
		{"flag alone activates", config.MCPConfig{}, "allow", 0, Policy{ModeAllow, CredRefuse, true}, false},
		{"invalid config", config.MCPConfig{Unregistered: "nope"}, "", 1, Policy{}, true},
		{"invalid flag", config.MCPConfig{}, "nope", 1, Policy{}, true},
		{"invalid credentials", config.MCPConfig{LiteralCredentials: "ignore"}, "", 1, Policy{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolvePolicy(tt.cfg, tt.flag, tt.registered)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func findingFields(f []Finding, kind string) []string {
	var out []string
	for _, x := range f {
		if x.Kind == kind {
			out = append(out, x.Server+":"+x.Field)
		}
	}
	return out
}

func names(ex []Existing) []string {
	var out []string
	for _, e := range ex {
		out = append(out, e.Name)
	}
	return out
}

func TestScanOpenCode(t *testing.T) {
	home, workdir := t.TempDir(), t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", "")
	write(t, filepath.Join(home, ".config", "opencode", "opencode.jsonc"), `{
  // global servers
  "mcp": {
    "remote": {"type": "remote", "url": "https://h.example/mcp?token=abc&x=1",
      "headers": {"Authorization": "Bearer abc", "X-Ref": "Bearer {env:TOKEN}"}},
    "local": {"type": "local", "command": ["x"], "environment": {"A": "literal", "B": "{env:B}", "C": "${C}", "D": "$D"},},
  },
}`)
	write(t, filepath.Join(workdir, "opencode.json"), `{"mcp": {"proj": {"type":"remote","url":"https://u:p@h.example/x"}}}`)

	ex, f, err := Scan(config.MCPInjectOpenCodeEnv, workdir, home)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := names(ex), []string{"local", "remote", "proj"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("names = %v, want %v", got, want)
	}
	want := []string{"local:environment.A", "remote:headers.Authorization", "remote:url.query.token", "proj:url.userinfo"}
	if got := findingFields(f, FindingLiteralCredential); !reflect.DeepEqual(got, want) {
		t.Fatalf("findings = %v, want %v", got, want)
	}
}

func TestScanOpenCodeHonorsXDG(t *testing.T) {
	home, workdir, xdg := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	write(t, filepath.Join(xdg, "opencode", "opencode.json"), `{"mcp": {"x": {"type":"local","command":["x"]}}}`)
	ex, _, err := Scan(config.MCPInjectOpenCodeEnv, workdir, home)
	if err != nil || len(ex) != 1 || ex[0].Name != "x" || ex[0].Harness != "opencode" {
		t.Fatalf("ex = %+v, err = %v", ex, err)
	}
}

func TestScanClaude(t *testing.T) {
	home, workdir := t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	write(t, filepath.Join(home, ".claude.json"), `{
  "mcpServers": {"user": {"command": "x", "env": {"K": "literal"}}},
  "projects": {
    "`+workdir+`": {"mcpServers": {"scoped": {"type": "http", "url": "https://h/mcp", "headers": {"Authorization": "${TOKEN}"}}}},
    "/other": {"mcpServers": {"foreign": {"command": "y"}}}
  }
}`)
	write(t, filepath.Join(workdir, ".mcp.json"), `{"mcpServers": {"shared": {"type":"sse","url":"https://h/?api_key=zzz&name=ok"}}}`)
	write(t, filepath.Join(workdir, ".claude", "settings.json"), `{"mcpServers": {"fromsettings": {"command": "z"}}}`)

	ex, f, err := Scan(config.MCPInjectClaudeFlag, workdir, home)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := names(ex), []string{"user", "scoped", "shared", "fromsettings"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("names = %v, want %v", got, want)
	}
	want := []string{"user:env.K", "shared:url.query.api_key"}
	if got := findingFields(f, FindingLiteralCredential); !reflect.DeepEqual(got, want) {
		t.Fatalf("findings = %v, want %v", got, want)
	}
}

func TestScanReportsUnparseableFile(t *testing.T) {
	home, workdir := t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	write(t, filepath.Join(home, ".claude.json"), `{ not json`)
	_, f, err := Scan(config.MCPInjectClaudeFlag, workdir, home)
	if err != nil {
		t.Fatal(err)
	}
	if len(f) != 1 || f[0].Kind != FindingParseError || f[0].File != filepath.Join(home, ".claude.json") {
		t.Fatalf("findings = %+v", f)
	}
}

func TestScanMissingFilesAreEmpty(t *testing.T) {
	ex, f, err := Scan(config.MCPInjectClaudeFlag, t.TempDir(), t.TempDir())
	if err != nil || len(ex) != 0 || len(f) != 0 {
		t.Fatalf("ex=%v f=%v err=%v", ex, f, err)
	}
}

func TestStripJSONCKeepsStrings(t *testing.T) {
	var v map[string]string
	if err := parseJSONC([]byte(`{"a": "http://x//y /* z */", /* c */ "b": "q\"//",}`), &v); err != nil {
		t.Fatal(err)
	}
	if v["a"] != "http://x//y /* z */" || v["b"] != `q"//` {
		t.Fatalf("got %v", v)
	}
}

func TestEvaluate(t *testing.T) {
	ex := []Existing{{Harness: "opencode", File: "/f", Name: "foo"}, {Harness: "opencode", File: "/f", Name: "mine"}}
	cred := []Finding{{File: "/f", Server: "foo", Field: "headers.X", Kind: FindingLiteralCredential}}
	parse := []Finding{{File: "/p", Kind: FindingParseError, Field: "bad"}}
	reg := map[string]bool{"mine": true}

	tests := []struct {
		name     string
		p        Policy
		f        []Finding
		wantWarn int
		wantRef  []string
	}{
		{"inactive", Policy{ModeRefuse, CredRefuse, false}, cred, 0, nil},
		{"allow", Policy{ModeAllow, CredAllow, true}, cred, 0, nil},
		{"allow with refused credentials", Policy{ModeAllow, CredRefuse, true}, cred, 0, []string{"headers.X"}},
		{"warn", Policy{ModeWarn, CredAllow, true}, nil, 1, nil},
		{"warn plus credential refuse", Policy{ModeWarn, CredRefuse, true}, cred, 1, []string{"headers.X"}},
		{"warn credentials", Policy{ModeAllow, CredWarn, true}, cred, 1, nil},
		{"ignore", Policy{ModeIgnore, CredAllow, true}, nil, 0, nil},
		{"ignore with credential refuse", Policy{ModeIgnore, CredRefuse, true}, cred, 0, []string{"headers.X"}},
		{"refuse names entry and file", Policy{ModeRefuse, CredAllow, true}, nil, 0, []string{`"foo"`, "/f"}},
		{"refuse does not name shadowed", Policy{ModeRefuse, CredAllow, true}, nil, 0, []string{`"foo"`}},
		{"parse error refuse", Policy{ModeRefuse, CredAllow, true}, parse, 0, []string{"/p"}},
		{"parse error ignore only warns", Policy{ModeIgnore, CredAllow, true}, parse, 1, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			warn, err := Evaluate(tt.p, ex, tt.f, reg)
			if len(warn) != tt.wantWarn {
				t.Fatalf("warnings = %v, want %d", warn, tt.wantWarn)
			}
			if tt.wantRef == nil {
				if err != nil {
					t.Fatalf("unexpected refusal: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected refusal")
			}
			for _, s := range tt.wantRef {
				if !strings.Contains(err.Error(), s) {
					t.Fatalf("refusal %q lacks %q", err, s)
				}
			}
			if strings.Contains(err.Error(), `"mine"`) {
				t.Fatalf("shadowed entry named: %v", err)
			}
		})
	}
}

var testServers = []Server{{
	Name: "gh",
	Argv: []string{"/bin/omac", "mcp", "bridge", "gh"},
	Env:  map[string]string{"OMAC_BASE": "http://h", "OMAC_SOCKET": "/s", "OMAC_FACADE_TOKEN": "t"},
}}

var testExisting = []Existing{{Name: "old"}, {Name: "gh"}}

func TestBuildOpenCode(t *testing.T) {
	const goldenEnabled = `{"mcp":{"gh":{"command":["/bin/omac","mcp","bridge","gh"],"enabled":true,"environment":{"OMAC_BASE":"http://h","OMAC_FACADE_TOKEN":"t","OMAC_SOCKET":"/s"},"type":"local"}}}`
	const goldenIgnore = `{"mcp":{"gh":{"command":["/bin/omac","mcp","bridge","gh"],"enabled":true,"environment":{"OMAC_BASE":"http://h","OMAC_FACADE_TOKEN":"t","OMAC_SOCKET":"/s"},"type":"local"},"old":{"enabled":false}}}`
	for _, tt := range []struct {
		mode Mode
		want string
	}{{ModeAllow, goldenEnabled}, {ModeWarn, goldenEnabled}, {ModeIgnore, goldenIgnore}, {ModeRefuse, goldenIgnore}} {
		inj, err := Build(config.MCPInjectOpenCodeEnv, testServers, testExisting, Policy{tt.mode, CredRefuse, true}, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if got := inj.Env["OPENCODE_CONFIG_CONTENT"]; got != tt.want {
			t.Fatalf("%s: got %s\nwant %s", tt.mode, got, tt.want)
		}
		if !reflect.DeepEqual(inj.AllowEnv, []string{"OPENCODE_CONFIG_CONTENT"}) || inj.InnerArgs != nil {
			t.Fatalf("%s: %+v", tt.mode, inj)
		}
	}
}

func TestBuildClaude(t *testing.T) {
	const golden = `{"mcpServers":{"gh":{"args":["mcp","bridge","gh"],"command":"/bin/omac","env":{"OMAC_BASE":"http://h","OMAC_FACADE_TOKEN":"t","OMAC_SOCKET":"/s"}}}}`
	for _, mode := range []Mode{ModeAllow, ModeWarn, ModeIgnore, ModeRefuse} {
		dir := t.TempDir()
		inj, err := Build(config.MCPInjectClaudeFlag, testServers, testExisting, Policy{mode, CredRefuse, true}, dir)
		if err != nil {
			t.Fatal(err)
		}
		file := filepath.Join(dir, "omac-mcp.json")
		raw, err := os.ReadFile(file)
		if err != nil || string(raw) != golden {
			t.Fatalf("%s: file %s err %v", mode, raw, err)
		}
		if info, _ := os.Stat(file); info.Mode().Perm() != 0o600 {
			t.Fatalf("mode = %v", info.Mode().Perm())
		}
		wantArgs := []string{"--mcp-config=" + file}
		if mode == ModeIgnore || mode == ModeRefuse {
			wantArgs = append(wantArgs, "--strict-mcp-config")
			if inj.Env["ENABLE_CLAUDEAI_MCP_SERVERS"] != "false" || !reflect.DeepEqual(inj.AllowEnv, []string{"ENABLE_CLAUDEAI_MCP_SERVERS"}) {
				t.Fatalf("%s: %+v", mode, inj)
			}
		} else if inj.Env != nil || inj.AllowEnv != nil {
			t.Fatalf("%s: unexpected env %+v", mode, inj)
		}
		if !reflect.DeepEqual(inj.InnerArgs, wantArgs) {
			t.Fatalf("%s: args = %v, want %v", mode, inj.InnerArgs, wantArgs)
		}
	}
}

func TestBuildInactiveInjectsNothing(t *testing.T) {
	inj, err := Build(config.MCPInjectClaudeFlag, testServers, nil, Policy{}, t.TempDir())
	if err != nil || !reflect.DeepEqual(inj, Injection{}) {
		t.Fatalf("inj = %+v, err = %v", inj, err)
	}
}
