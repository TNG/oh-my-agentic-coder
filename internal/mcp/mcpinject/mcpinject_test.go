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
		{"nothing registered, nothing set", config.MCPConfig{}, "", 0, Policy{ModeRefuse, false}, false},
		{"registered activates defaults", config.MCPConfig{}, "", 1, Policy{ModeRefuse, true}, false},
		{"config unregistered activates", config.MCPConfig{Unregistered: "warn"}, "", 0, Policy{ModeWarn, true}, false},
		{"flag overrides config", config.MCPConfig{Unregistered: "refuse"}, "warn", 0, Policy{ModeWarn, true}, false},
		{"flag alone activates", config.MCPConfig{}, "allow", 0, Policy{ModeAllow, true}, false},
		{"invalid config", config.MCPConfig{Unregistered: "nope"}, "", 1, Policy{}, true},
		{"invalid flag", config.MCPConfig{}, "nope", 1, Policy{}, true},
		{"ignore is not a mode", config.MCPConfig{}, "ignore", 1, Policy{}, true},
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
    "remote": {"type": "remote", "url": "https://h.example/mcp"},
    "local": {"type": "local", "command": ["x"],},
  },
}`)
	write(t, filepath.Join(workdir, "opencode.json"), `{"mcp": {"proj": {"type":"remote","url":"https://h.example/x"}}}`)

	ex, f, err := Scan(config.MCPScanOpenCode, hostFor(workdir, home))
	if err != nil || len(f) != 0 {
		t.Fatalf("err = %v, findings = %v", err, f)
	}
	if got, want := names(ex), []string{"local", "remote", "proj"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("names = %v, want %v", got, want)
	}
}

func TestScanOpenCodeHonorsXDG(t *testing.T) {
	home, workdir, xdg := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	write(t, filepath.Join(xdg, "opencode", "opencode.json"), `{"mcp": {"x": {"type":"local","command":["x"]}}}`)
	ex, _, err := Scan(config.MCPScanOpenCode, hostFor(workdir, home))
	if err != nil || len(ex) != 1 || ex[0].Name != "x" || ex[0].Harness != "opencode" {
		t.Fatalf("ex = %+v, err = %v", ex, err)
	}
}

func TestScanClaude(t *testing.T) {
	home, workdir := t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	write(t, filepath.Join(home, ".claude.json"), `{
  "mcpServers": {"user": {"command": "x"}},
  "projects": {
    "`+workdir+`": {"mcpServers": {"scoped": {"type": "http", "url": "https://h/mcp"}}},
    "/other": {"mcpServers": {"foreign": {"command": "y"}}}
  }
}`)
	write(t, filepath.Join(workdir, ".mcp.json"), `{"mcpServers": {"shared": {"type":"sse","url":"https://h/"}}}`)
	write(t, filepath.Join(workdir, ".claude", "settings.json"), `{"mcpServers": {"fromsettings": {"command": "z"}}}`)

	ex, f, err := Scan(config.MCPScanClaude, hostFor(workdir, home))
	if err != nil || len(f) != 0 {
		t.Fatalf("err = %v, findings = %v", err, f)
	}
	if got, want := names(ex), []string{"user", "scoped", "shared", "fromsettings"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("names = %v, want %v", got, want)
	}
}

func TestScanReportsUnparseableFile(t *testing.T) {
	home, workdir := t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	write(t, filepath.Join(home, ".claude.json"), `{ not json`)
	_, f, err := Scan(config.MCPScanClaude, hostFor(workdir, home))
	if err != nil {
		t.Fatal(err)
	}
	if len(f) != 1 || f[0].Kind != FindingParseError || f[0].File != filepath.Join(home, ".claude.json") {
		t.Fatalf("findings = %+v", f)
	}
}

func TestScanMissingFilesAreEmpty(t *testing.T) {
	ex, f, err := Scan(config.MCPScanClaude, hostFor(t.TempDir(), t.TempDir()))
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
	ex := []Existing{{Harness: "opencode", File: "/f", Name: "foo"}, {Harness: "opencode", File: "/f", Name: "mine", AlsoRegistered: true}}
	parse := []Finding{{File: "/p", Kind: FindingParseError, Detail: "bad"}}

	tests := []struct {
		name     string
		p        Policy
		f        []Finding
		wantWarn []string
		wantRef  []string
	}{
		{"inactive", Policy{ModeRefuse, false}, parse, nil, nil},
		{"allow", Policy{ModeAllow, true}, parse, nil, nil},
		{"warn", Policy{ModeWarn, true}, parse, []string{`"foo"`, `"mine"`, "also registered", "/f", "/p"}, nil},
		{"refuse names entry and file", Policy{ModeRefuse, true}, nil, nil, []string{`"foo"`, `"mine"`, "also registered with omac", "/f", "rotate", "omac mcp register", "--mcp-unregistered=allow"}},
		{"refuse names unparseable file", Policy{ModeRefuse, true}, parse, nil, []string{"/p", `"foo"`}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			warn, err := Evaluate(tt.p, ex, tt.f)
			joined := strings.Join(warn, "\n")
			if len(tt.wantWarn) == 0 && len(warn) != 0 {
				t.Fatalf("unexpected warnings: %v", warn)
			}
			for _, s := range tt.wantWarn {
				if !strings.Contains(joined, s) {
					t.Fatalf("warnings %q lack %q", joined, s)
				}
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
		})
	}
}

func TestEvaluateSameNameEntryOffends(t *testing.T) {
	ex := []Existing{{File: "/f", Name: "mine", AlsoRegistered: true}}
	if _, err := Evaluate(Policy{ModeRefuse, true}, ex, nil); err == nil || !strings.Contains(err.Error(), "also registered") {
		t.Fatalf("err = %v", err)
	}
	if warn, err := Evaluate(Policy{ModeWarn, true}, ex, nil); err != nil || len(warn) != 1 {
		t.Fatalf("warn = %v err = %v", warn, err)
	}
	if warn, err := Evaluate(Policy{ModeAllow, true}, ex, nil); err != nil || len(warn) != 0 {
		t.Fatalf("warn = %v err = %v", warn, err)
	}
}

var testServers = []Server{{
	Name: "gh",
	Argv: []string{"/bin/omac", "mcp", "bridge", "gh"},
	Env:  map[string]string{"OMAC_BASE": "http://h", "OMAC_SOCKET": "/s", "OMAC_FACADE_TOKEN": "t"},
}}

func TestBuildOpenCodeOnlyAddsServers(t *testing.T) {
	const golden = `{"mcp":{"gh":{"command":["/bin/omac","mcp","bridge","gh"],"enabled":true,"environment":{"OMAC_BASE":"http://h","OMAC_FACADE_TOKEN":"t","OMAC_SOCKET":"/s"},"type":"local"}}}`
	for _, mode := range []Mode{ModeAllow, ModeWarn, ModeRefuse} {
		inj, err := Build(config.MCPInjectOpenCodeEnv, testServers, Policy{mode, true}, t.TempDir(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := inj.Env["OPENCODE_CONFIG_CONTENT"]; got != golden {
			t.Fatalf("%s: got %s\nwant %s", mode, got, golden)
		}
		if !reflect.DeepEqual(inj.AllowEnv, []string{"OPENCODE_CONFIG_CONTENT"}) || inj.InnerArgs != nil {
			t.Fatalf("%s: %+v", mode, inj)
		}
	}
}

func TestBuildClaudeOnlyAddsConfigFlag(t *testing.T) {
	const golden = `{"mcpServers":{"gh":{"args":["mcp","bridge","gh"],"command":"/bin/omac","env":{"OMAC_BASE":"http://h","OMAC_FACADE_TOKEN":"t","OMAC_SOCKET":"/s"}}}}`
	for _, mode := range []Mode{ModeAllow, ModeWarn, ModeRefuse} {
		dir := t.TempDir()
		inj, err := Build(config.MCPInjectClaudeFlag, testServers, Policy{mode, true}, dir, nil)
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
		if mode == ModeRefuse {
			wantArgs = []string{"--strict-mcp-config", "--mcp-config=" + file}
		}
		if inj.Env != nil || inj.AllowEnv != nil || !reflect.DeepEqual(inj.InnerArgs, wantArgs) {
			t.Fatalf("%s: %+v", mode, inj)
		}
	}
}

func TestBuildInactiveInjectsNothing(t *testing.T) {
	inj, err := Build(config.MCPInjectClaudeFlag, testServers, Policy{}, t.TempDir(), nil)
	if err != nil || !reflect.DeepEqual(inj, Injection{}) {
		t.Fatalf("inj = %+v, err = %v", inj, err)
	}
}

func TestBuildClaudeStrictMode(t *testing.T) {
	strict := []string{"--strict-mcp-config"}
	tests := []struct {
		name     string
		kind     config.MCPInjectionKind
		policy   Policy
		userArgs []string
		want     []string
	}{
		{"claude default, nothing registered", config.MCPInjectClaudeFlag, Policy{ModeRefuse, false}, nil, strict},
		{"claude refuse, nothing registered", config.MCPInjectClaudeFlag, Policy{ModeRefuse, true}, nil, strict},
		{"claude warn", config.MCPInjectClaudeFlag, Policy{ModeWarn, true}, nil, nil},
		{"claude allow", config.MCPInjectClaudeFlag, Policy{ModeAllow, true}, nil, nil},
		{"user passed the flag", config.MCPInjectClaudeFlag, Policy{ModeRefuse, false}, strict, nil},
		{"opencode", config.MCPInjectOpenCodeEnv, Policy{ModeRefuse, false}, nil, nil},
		{"harness without injection", config.MCPInjectNone, Policy{ModeRefuse, false}, nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inj, err := Build(tt.kind, nil, tt.policy, t.TempDir(), tt.userArgs)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(inj.InnerArgs, tt.want) || inj.Env != nil || inj.AllowEnv != nil {
				t.Fatalf("inj = %+v, want args %v", inj, tt.want)
			}
		})
	}
}

func TestBuildClaudeDoesNotDuplicateUserStrictFlag(t *testing.T) {
	dir := t.TempDir()
	inj, err := Build(config.MCPInjectClaudeFlag, testServers, Policy{ModeRefuse, true}, dir, []string{"--strict-mcp-config"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"--mcp-config=" + filepath.Join(dir, "omac-mcp.json")}; !reflect.DeepEqual(inj.InnerArgs, want) {
		t.Fatalf("args = %v, want %v", inj.InnerArgs, want)
	}
}
