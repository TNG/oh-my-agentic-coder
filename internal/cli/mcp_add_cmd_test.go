package cli

import (
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/mcp/mcppolicy"
	"github.com/TNG/oh-my-agentic-coder/internal/mcp/mcpreg"
)

func runAdd(t *testing.T, wd string, args ...string) (int, string, string) {
	t.Helper()
	env, read := captureEnv(t, wd)
	code := runMCP(args, env)
	out, errOut := read()
	return code, out, errOut
}

func TestSplitAtDoubleDash(t *testing.T) {
	before, after, found := splitAtDoubleDash([]string{"a", "--", "b", "--", "-e"})
	if !found || !reflect.DeepEqual(before, []string{"a"}) || !reflect.DeepEqual(after, []string{"b", "--", "-e"}) {
		t.Fatalf("%v %v %v", before, after, found)
	}
	if _, _, found := splitAtDoubleDash([]string{"a"}); found {
		t.Fatal("found")
	}
}

func TestMCPAddMixedFlagsAndPositional(t *testing.T) {
	wd, _ := mcpTestSetup(t)
	t.Setenv("OMAC_SECRET_TOK", "s3cret")
	code, _, errOut := runAdd(t, wd, "add", "demo", "-e", "LEVEL=debug", "--harness", "opencode", "--secret", "TOK",
		"--", "/usr/bin/env", "-e", "x", "--harness")
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	e, err := mcpreg.Load(mcpreg.ScopeFor(wd), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"/usr/bin/env", "-e", "x", "--harness"}; !reflect.DeepEqual(e.Def.Server.Command, want) {
		t.Fatalf("command = %v", e.Def.Server.Command)
	}
	if !reflect.DeepEqual(e.Def.Server.Env, map[string]string{"LEVEL": "debug"}) || !reflect.DeepEqual(e.Def.Secrets, []string{"TOK"}) {
		t.Fatalf("def = %+v", e.Def)
	}
	if !reflect.DeepEqual(e.Harnesses, []string{"opencode"}) {
		t.Fatalf("harnesses = %v", e.Harnesses)
	}
}

func TestMCPAddRefusals(t *testing.T) {
	wd, _ := mcpTestSetup(t)
	t.Setenv("OMAC_SECRET_TOK", "s3cret")
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"no separator", []string{"add", "demo", "--harness", "opencode", "/bin/true"}, "Usage"},
		{"no command", []string{"add", "demo", "--harness", "opencode", "--"}, "Usage"},
		{"empty value", []string{"add", "demo", "--harness", "opencode", "-e", "A=", "--", "/bin/true"}, "declare --secret A"},
		{"variable reference", []string{"add", "demo", "--harness", "opencode", "-e", "A=${HOME}", "--", "/bin/true"}, "declare --secret A"},
		{"env and secret", []string{"add", "demo", "--harness", "opencode", "-e", "TOK=x", "--secret", "TOK", "--", "/bin/true"}, "-e and --secret"},
		{"command in workdir", []string{"add", "demo", "--harness", "opencode", "--", filepath.Join(wd, "srv")}, "inside the workdir"},
		{"relative command", []string{"add", "demo", "--harness", "opencode", "--", "./srv"}, "relative path"},
		{"missing harness", []string{"add", "demo", "--", "/bin/true"}, "--harness is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, _, errOut := runAdd(t, wd, tt.args...)
			if code != ExitMisuse || !strings.Contains(errOut, tt.want) {
				t.Fatalf("exit %d, stderr %q, want %q", code, errOut, tt.want)
			}
			if entries, _ := mcpreg.List(mcpreg.ScopeFor(wd)); len(entries) != 0 {
				t.Fatal("registered despite refusal")
			}
		})
	}
}

func TestMCPAddReplaceKeepsPolicy(t *testing.T) {
	wd, _ := mcpTestSetup(t)
	scope := mcpreg.ScopeFor(wd)
	if code, _, e := runAdd(t, wd, "add", "demo", "--harness", "opencode", "--", "/bin/true"); code != ExitOK {
		t.Fatal(e)
	}
	if code, _, e := runAdd(t, wd, "add", "demo", "--harness", "opencode", "--", "/bin/false"); code != ExitMisuse || !strings.Contains(e, "--replace") {
		t.Fatalf("exit %d: %s", code, e)
	}
	pin := &mcppolicy.Pin{Tools: map[string]string{"get": "h1"}, PinnedAt: time.Now().UTC()}
	pin.ToolsHash = mcppolicy.ListHash(pin.Tools)
	if _, err := mcpreg.Update(scope, "demo", func(d *mcpreg.Definition) error {
		d.Access = mcppolicy.ReadWrite
		d.Tools = []mcppolicy.Tool{{Name: "get", Class: mcppolicy.Read}}
		d.Approvals = &mcppolicy.Approvals{Write: "none"}
		d.Pin = pin
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if code, out, e := runAdd(t, wd, "add", "demo", "--replace", "-e", "A=1", "--", "/bin/false", "x"); code != ExitOK || !strings.Contains(out, "replaced demo") {
		t.Fatalf("exit %d: %s %s", code, out, e)
	}
	got, err := mcpreg.Load(scope, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Def.Server.Command, []string{"/bin/false", "x"}) || got.Def.Server.Env["A"] != "1" {
		t.Fatalf("server = %+v", got.Def.Server)
	}
	if got.Def.Access != mcppolicy.ReadWrite || len(got.Def.Tools) != 1 || got.Def.Tools[0].Class != mcppolicy.Read ||
		got.Def.Approvals == nil || got.Def.Approvals.Write != "none" || got.Def.Pin == nil || got.Def.Pin.ToolsHash != pin.ToolsHash {
		t.Fatalf("policy lost: %+v", got.Def)
	}
	if code, _, e := runAdd(t, wd, "add", "demo", "--replace", "--harness", "claude-code", "--", "/bin/true"); code != ExitMisuse || !strings.Contains(e, "deregister") {
		t.Fatalf("exit %d: %s", code, e)
	}
}

func TestMCPAddJSONRemoteAndUnknownKeysRefused(t *testing.T) {
	wd, _ := mcpTestSetup(t)
	tests := []struct{ json, want string }{
		{`{"url":"https://h/mcp"}`, "remote MCP servers are not supported yet"},
		{`{"type":"http","command":"/bin/true"}`, "remote MCP servers are not supported yet"},
		{`{"type":"sse","command":"/bin/true"}`, "remote MCP servers are not supported yet"},
		{`{"mcpServers":{"demo":{"type":"http","url":"https://h"}}}`, "remote MCP servers are not supported yet"},
		{`{"command":"/bin/true","cwd":"/"}`, "unknown field"},
		{`{"command":"/bin/true","env":{"A":""}}`, "declare --secret A"},
		{`{"command":"/bin/true","env":{"A":"${A}"}}`, "declare --secret A"},
		{`{"command":"/bin/true"}`, ""},
	}
	for _, tt := range tests {
		code, _, errOut := runAdd(t, wd, "add-json", "--harness", "opencode", "demo", tt.json)
		if tt.want == "" {
			if code != ExitOK {
				t.Fatalf("%s: exit %d %s", tt.json, code, errOut)
			}
			continue
		}
		if code != ExitMisuse || !strings.Contains(errOut, tt.want) {
			t.Fatalf("%s: exit %d, stderr %q", tt.json, code, errOut)
		}
	}
}

func TestMCPAddJSONSecretValueStaysOutOfSnapshot(t *testing.T) {
	wd, _ := mcpTestSetup(t)
	const secret = "ghp_verysecretvalue123"
	t.Setenv("OMAC_SECRET_PROMPTED", "from-env-var-456")
	json := `{"mcpServers":{"demo":{"type":"stdio","command":"/bin/true","args":["--x"],"env":{"GITHUB_TOKEN":"` + secret + `","PROMPTED":"","LEVEL":"info"}}}}`
	code, out, errOut := runAdd(t, wd, "add-json", "--harness", "claude-code", "--secret", "GITHUB_TOKEN", "--secret", "PROMPTED", "demo", json)
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if strings.Contains(out+errOut, secret) {
		t.Fatal("secret printed")
	}
	scope := mcpreg.ScopeFor(wd)
	e, err := mcpreg.Load(scope, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(e.Def.Secrets, []string{"GITHUB_TOKEN", "PROMPTED"}) || e.Def.Server.Env["LEVEL"] != "info" {
		t.Fatalf("def = %+v", e.Def)
	}
	for key, want := range map[string]string{"GITHUB_TOKEN": secret, "PROMPTED": "from-env-var-456"} {
		v, err := mcpreg.GetSecret(scope, "demo", key)
		if err != nil || v.ExposeString() != want {
			t.Fatalf("keychain %s: %v", key, err)
		}
	}
	serverDir := filepath.Dir(filepath.Dir(e.SnapshotDir))
	files := 0
	err = filepath.WalkDir(serverDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		files++
		b, _ := os.ReadFile(p)
		if strings.Contains(string(b), secret) || strings.Contains(string(b), "from-env-var-456") {
			t.Errorf("%s holds a secret value", p)
		}
		return nil
	})
	if err != nil || files < 2 {
		t.Fatalf("walked %d files: %v", files, err)
	}
}

func TestMCPAddJSONFile(t *testing.T) {
	wd, _ := mcpTestSetup(t)
	file := filepath.Join(t.TempDir(), "servers.json")
	body := `{"mcpServers":{"a":{"command":"/bin/true"},"b":{"command":"/bin/false","env":{"TOK":"x1"}},"c":{"command":"/bin/true"}}}`
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, e := runAdd(t, wd, "add-json", "--harness", "opencode", "--file", file, "a", "missing"); code != ExitMisuse || !strings.Contains(e, `"missing"`) {
		t.Fatalf("exit %d: %s", code, e)
	}
	if entries, _ := mcpreg.List(mcpreg.ScopeFor(wd)); len(entries) != 0 {
		t.Fatal("partial registration")
	}
	if code, _, e := runAdd(t, wd, "add-json", "--harness", "opencode", "--secret", "TOK", "--file", file, "b", "a"); code != ExitOK {
		t.Fatal(e)
	}
	entries, _ := mcpreg.List(mcpreg.ScopeFor(wd))
	if len(entries) != 2 || entries[0].Name != "a" || entries[1].Name != "b" || !reflect.DeepEqual(entries[1].Def.Secrets, []string{"TOK"}) {
		t.Fatalf("entries = %+v", entries)
	}
}

func TestMCPAddRefusedInsideSandbox(t *testing.T) {
	for _, v := range []string{"OMAC_SOCKET", "OMAC_FACADE_TOKEN"} {
		t.Run(v, func(t *testing.T) {
			wd, _ := mcpTestSetup(t)
			t.Setenv(v, "x")
			for _, args := range [][]string{
				{"add", "demo", "--harness", "opencode", "--", "/bin/true"},
				{"add-json", "--harness", "opencode", "demo", `{"command":"/bin/true"}`},
			} {
				code, _, e := runAdd(t, wd, args...)
				if code != ExitMisuse || !strings.Contains(e, "inside the omac sandbox") {
					t.Fatalf("%v: exit %d, %q", args, code, e)
				}
			}
			if entries, _ := mcpreg.List(mcpreg.ScopeFor(wd)); len(entries) != 0 {
				t.Fatal("registered from inside the sandbox")
			}
		})
	}
}
