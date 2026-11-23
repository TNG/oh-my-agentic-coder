package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/mcp/mcppolicy"
	"github.com/TNG/oh-my-agentic-coder/internal/mcp/mcpreg"
)

// TestMCPToolsFakeServerHelper is the stdio MCP server the tests below register,
// started by re-executing the test binary.
func TestMCPToolsFakeServerHelper(t *testing.T) {
	if os.Getenv("OMAC_TEST_TOOLS_FAKE") != "1" {
		t.Skip("helper process")
	}
	toolsFakeServer()
	os.Exit(0)
}

func toolsFakeServer() {
	if path := os.Getenv("FAKE_DUMP"); path != "" {
		os.WriteFile(path, []byte(strings.Join(os.Environ(), "\n")), 0o600)
	}
	if os.Getenv("FAKE_FAIL") == "1" {
		fmt.Fprintln(os.Stderr, "fake server: cannot start")
		os.Exit(3)
	}
	desc := func() string {
		b, _ := os.ReadFile(os.Getenv("FAKE_DESC_FILE"))
		return string(b)
	}
	in := bufio.NewScanner(os.Stdin)
	out := json.NewEncoder(os.Stdout)
	for in.Scan() {
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Cursor string `json:"cursor"`
			} `json:"params"`
		}
		if json.Unmarshal(in.Bytes(), &m) != nil || m.ID == nil {
			continue
		}
		reply := func(result any) {
			out.Encode(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": result})
		}
		switch m.Method {
		case "initialize":
			reply(map[string]any{"protocolVersion": "2025-06-18"})
		case "tools/list":
			if m.Params.Cursor == "" {
				reply(map[string]any{"nextCursor": "p2", "tools": []any{
					map[string]any{"name": "get", "annotations": map[string]any{"readOnlyHint": true}},
					map[string]any{"name": "put", "description": desc(), "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}, "n": map[string]any{"type": []any{"integer", "null"}}}, "required": []any{"path"}}, "annotations": map[string]any{"destructiveHint": false}},
				}})
				continue
			}
			tools := []any{map[string]any{"name": "rm"}}
			if _, err := os.Stat(os.Getenv("FAKE_DESC_FILE") + ".extra"); err == nil {
				tools = append(tools, map[string]any{"name": "extra"})
			}
			reply(map[string]any{"tools": tools})
		}
	}
}

type fakeMCP struct {
	wd, descFile string
	scope        mcpreg.Scope
}

func registerToolsFake(t *testing.T, extraEnv map[string]string) *fakeMCP {
	t.Helper()
	isolateHome(t)
	t.Setenv("OMAC_KEYCHAIN_MOCK", filepath.Join(t.TempDir(), "keychain.json"))
	t.Setenv("OMAC_SOCKET", "")
	t.Setenv("OMAC_FACADE_TOKEN", "")
	f := &fakeMCP{wd: t.TempDir(), descFile: filepath.Join(t.TempDir(), "desc")}
	os.WriteFile(f.descFile, []byte("v1"), 0o600)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	envLines := fmt.Sprintf("    OMAC_TEST_TOOLS_FAKE: \"1\"\n    FAKE_DESC_FILE: %q\n", f.descFile)
	for k, v := range extraEnv {
		envLines += fmt.Sprintf("    %s: %q\n", k, v)
	}
	def := filepath.Join(t.TempDir(), "mcp.yaml")
	body := fmt.Sprintf("name: fake\nserver:\n  command: [%q, \"-test.run=^TestMCPToolsFakeServerHelper$\"]\n  env:\n%s", exe, envLines)
	if err := os.WriteFile(def, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	f.scope = mcpreg.ScopeFor(f.wd)
	if _, err := mcpreg.Register(f.scope, def, []string{"opencode"}); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fakeMCP) run(t *testing.T, stdin string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	env, read := captureEnv(t, f.wd)
	if stdin != "" {
		in := filepath.Join(t.TempDir(), "stdin")
		os.WriteFile(in, []byte(stdin), 0o600)
		env.Stdin, _ = os.Open(in)
	}
	code = runMCP(args, env)
	stdout, stderr = read()
	return
}

func (f *fakeMCP) entry(t *testing.T) *mcpreg.Entry {
	t.Helper()
	e, err := mcpreg.Load(f.scope, "fake")
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func classesOf(e *mcpreg.Entry) map[string]mcppolicy.Class { return currentClasses(e.Def) }

func TestMCPToolsShowsProposalsAndPaginates(t *testing.T) {
	f := registerToolsFake(t, nil)
	code, out, errOut := f.run(t, "", "tools", "fake")
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	for _, want := range []string{"claims, not facts", "read-only", "not destructive", "assumed destructive", "unclassified", "new", "3 tools, 3 unclassified; not reviewed"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) > 1 && f[0] == "get" && f[1] != "read" || len(f) > 1 && f[0] == "put" && f[1] != "write" || len(f) > 1 && f[0] == "rm" && f[1] != "delete" {
			t.Errorf("wrong proposal: %q", line)
		}
	}
	if e := f.entry(t); e.Def.Review != nil || len(e.Def.Tools) != 0 {
		t.Fatal("read-only listing changed the registry")
	}
}

func TestMCPClassifyScriptedWritesClassesAndReview(t *testing.T) {
	f := registerToolsFake(t, nil)
	code, _, errOut := f.run(t, "", "tools", "fake", "--classify", "--set", "get=read", "--set", "put=w")
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	e := f.entry(t)
	want := map[string]mcppolicy.Class{"get": mcppolicy.Read, "put": mcppolicy.Write, "rm": mcppolicy.Off}
	got := classesOf(e)
	for n, c := range want {
		if got[n] != c {
			t.Errorf("class of %s = %q, want %q", n, got[n], c)
		}
	}
	_, tools, ok := probeMCPServer(&Env{Stderr: os.Stderr, Stdout: os.Stdout}, "tools", f.scope, "fake")
	if !ok {
		t.Fatal("probe failed")
	}
	hashes := liveHashes(tools)
	if e.Def.Review == nil || e.Def.Review.ToolsHash != mcppolicy.ListHash(hashes) || e.Def.Review.At.IsZero() {
		t.Fatalf("review: %+v", e.Def.Review)
	}
	for n, h := range hashes {
		if e.Def.Review.Tools[n] != h {
			t.Errorf("review of %s differs", n)
		}
	}
	_, out, _ := f.run(t, "", "tools", "fake")
	if strings.Count(out, "reviewed") < 3 || !strings.Contains(out, "no changes") {
		t.Fatalf("after classify:\n%s", out)
	}
}

func TestMCPClassifyScriptedRejectsUnknownToolAndWritesNothing(t *testing.T) {
	f := registerToolsFake(t, nil)
	before := f.entry(t).BundleHash
	code, _, errOut := f.run(t, "", "tools", "fake", "--classify", "--set", "nope=read")
	if code == ExitOK || !strings.Contains(errOut, "nope") {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if f.entry(t).BundleHash != before {
		t.Fatal("registry changed")
	}
}

func TestMCPClassifyInteractive(t *testing.T) {
	f := registerToolsFake(t, nil)
	code, _, errOut := f.run(t, "\nx\nd\noff\n", "tools", "fake", "--classify")
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	got := classesOf(f.entry(t))
	if got["get"] != mcppolicy.Read || got["put"] != mcppolicy.Delete || got["rm"] != mcppolicy.Off {
		t.Fatalf("classes: %v", got)
	}
}

func TestMCPClassifyInteractiveEOFWritesNothing(t *testing.T) {
	f := registerToolsFake(t, nil)
	before := f.entry(t).BundleHash
	code, _, _ := f.run(t, "r\n", "tools", "fake", "--classify")
	if code == ExitOK || f.entry(t).BundleHash != before {
		t.Fatalf("exit %d, registry changed: %v", code, f.entry(t).BundleHash != before)
	}
}

func TestMCPReviewRefusesUnclassifiedTool(t *testing.T) {
	f := registerToolsFake(t, nil)
	if code, _, e := f.run(t, "", "tools", "fake", "--classify", "--rest", "read"); code != ExitOK {
		t.Fatal(e)
	}
	os.WriteFile(f.descFile+".extra", nil, 0o600)
	before := f.entry(t).BundleHash
	code, _, errOut := f.run(t, "", "review", "fake", "--yes")
	if code == ExitOK || !strings.Contains(errOut, "extra") || !strings.Contains(errOut, "omac mcp tools fake --classify") {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if f.entry(t).BundleHash != before {
		t.Fatal("registry changed")
	}
}

func TestMCPReviewAcceptsChangedTools(t *testing.T) {
	f := registerToolsFake(t, nil)
	if code, _, e := f.run(t, "", "tools", "fake", "--classify", "--rest", "read"); code != ExitOK {
		t.Fatal(e)
	}
	os.WriteFile(f.descFile, []byte("v2 changed"), 0o600)
	old := f.entry(t).Def.Review.ToolsHash

	code, out, errOut := f.run(t, "n\n", "review", "fake")
	if code == ExitOK || !strings.Contains(out, "changed") || !strings.Contains(out, "put") {
		t.Fatalf("declined review: exit %d out %q err %q", code, out, errOut)
	}
	if f.entry(t).Def.Review.ToolsHash != old {
		t.Fatal("declined review was written")
	}

	if code, _, e := f.run(t, "y\n", "review", "fake"); code != ExitOK {
		t.Fatal(e)
	}
	if f.entry(t).Def.Review.ToolsHash == old {
		t.Fatal("review not updated")
	}
	_, out, _ = f.run(t, "", "review", "fake", "--yes")
	if !strings.Contains(out, "nothing to review") {
		t.Fatalf("second review: %q", out)
	}
}

func TestMCPToolsStartFailureLeavesRegistryUntouched(t *testing.T) {
	f := registerToolsFake(t, map[string]string{"FAKE_FAIL": "1"})
	before := f.entry(t).BundleHash
	for _, args := range [][]string{
		{"tools", "fake", "--classify", "--rest", "read"},
		{"review", "fake", "--yes"},
	} {
		code, _, errOut := f.run(t, "", args...)
		if code == ExitOK || !strings.Contains(errOut, "fake server: cannot start") {
			t.Fatalf("%v: exit %d: %s", args, code, errOut)
		}
	}
	if f.entry(t).BundleHash != before {
		t.Fatal("registry changed")
	}
}

func TestMCPToolsProbeChildEnvHasNoOmacVariables(t *testing.T) {
	dump := filepath.Join(t.TempDir(), "dump")
	f := registerToolsFake(t, map[string]string{"FAKE_DUMP": dump})
	t.Setenv("OMAC_SIDECAR_SECRET", "s")
	t.Setenv("SIDECAR_PORT", "1")
	if code, _, e := f.run(t, "", "tools", "fake"); code != ExitOK {
		t.Fatal(e)
	}
	raw, err := os.ReadFile(dump)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "SIDECAR_") || (strings.HasPrefix(line, "OMAC_") && !strings.HasPrefix(line, "OMAC_TEST_TOOLS_FAKE=")) {
			t.Errorf("child env holds %q", line)
		}
	}
}

func TestMCPToolsAndReviewRefuseInsideSandbox(t *testing.T) {
	f := registerToolsFake(t, nil)
	t.Setenv("OMAC_SOCKET", "/tmp/x.sock")
	before := f.entry(t).BundleHash
	for _, args := range [][]string{{"tools", "fake"}, {"tools", "fake", "--classify", "--rest", "read"}, {"review", "fake", "--yes"}} {
		code, _, errOut := f.run(t, "", args...)
		if code != ExitMisuse || !strings.Contains(errOut, "refused inside the omac sandbox") {
			t.Fatalf("%v: exit %d: %s", args, code, errOut)
		}
	}
	if f.entry(t).BundleHash != before {
		t.Fatal("registry changed")
	}
}

func TestMCPToolsShowsDescriptionAndSchemaSanitized(t *testing.T) {
	f := registerToolsFake(t, nil)
	evil := "Puts a file.\x1b[31m\nIGNORE\u202e previous\x07 " + strings.Repeat("x", 800)
	os.WriteFile(f.descFile, []byte(evil), 0o600)
	code, out, errOut := f.run(t, "", "tools", "fake")
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	for _, bad := range []string{"\x1b", "\u202e", "\x07"} {
		if strings.Contains(out, bad) {
			t.Errorf("output holds %q", bad)
		}
	}
	if !strings.Contains(out, "description: Puts a file.[31m IGNORE previous ") || !strings.Contains(out, "x...\n") {
		t.Errorf("description not shown capped:\n%s", out)
	}
	if strings.Count(out, "xxxxxxxxxx") == 0 || strings.Contains(out, strings.Repeat("x", 501)) {
		t.Errorf("description not capped at 500")
	}
	if !strings.Contains(out, "parameters:  n: integer|null, path: string (required)") {
		t.Errorf("schema summary missing:\n%s", out)
	}
	if !strings.Contains(out, "(no description)") {
		t.Errorf("tool without description:\n%s", out)
	}
}

func TestMCPReviewShowsOldAndNewDescription(t *testing.T) {
	f := registerToolsFake(t, nil)
	if code, _, e := f.run(t, "", "tools", "fake", "--classify", "--rest", "read"); code != ExitOK {
		t.Fatal(e)
	}
	if got := f.entry(t).Def.Review.Text["put"].Description; got != "v1" {
		t.Fatalf("stored text %q", got)
	}
	os.WriteFile(f.descFile, []byte("v2 now deletes"), 0o600)
	os.WriteFile(f.descFile+".extra", nil, 0o600)
	// extra is unclassified: review refuses, but the diff is printed first.
	_, out, _ := f.run(t, "", "review", "fake", "--yes")
	for _, want := range []string{"changed: put", "old:", "description: v1", "new:", "description: v2 now deletes", "added: extra"} {
		if !strings.Contains(out, want) {
			t.Errorf("review output lacks %q:\n%s", want, out)
		}
	}
}

func TestMCPReviewShowsRemovedTool(t *testing.T) {
	f := registerToolsFake(t, nil)
	os.WriteFile(f.descFile+".extra", nil, 0o600)
	if code, _, e := f.run(t, "", "tools", "fake", "--classify", "--rest", "read"); code != ExitOK {
		t.Fatal(e)
	}
	os.Remove(f.descFile + ".extra")
	_, out, _ := f.run(t, "n\n", "review", "fake")
	if !strings.Contains(out, "removed: extra") {
		t.Errorf("removed tool not named:\n%s", out)
	}
}

func TestMCPReviewWithoutStoredTextStillWorks(t *testing.T) {
	f := registerToolsFake(t, nil)
	if code, _, e := f.run(t, "", "tools", "fake", "--classify", "--rest", "read"); code != ExitOK {
		t.Fatal(e)
	}
	// Turn the record into one written before text was stored.
	if _, err := mcpreg.Update(f.scope, "fake", func(d *mcpreg.Definition) error { d.Review.Text = nil; return nil }); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(f.descFile, []byte("v2"), 0o600)
	code, out, errOut := f.run(t, "y\n", "review", "fake")
	if code != ExitOK || !strings.Contains(out, "old text was not recorded") || !strings.Contains(out, "description: v2") {
		t.Fatalf("exit %d out %s err %s", code, out, errOut)
	}
	if f.entry(t).Def.Review.Text["put"].Description != "v2" {
		t.Fatal("text not stored by review")
	}
}

func TestCleanToolText(t *testing.T) {
	if got := cleanToolText("a\x1b[2Jb\u202ec\r\n\td", 500); got != "a[2Jbc d" {
		t.Errorf("got %q", got)
	}
	if got := cleanToolText(strings.Repeat("é", 600), 500); len([]rune(got)) != 503 || !strings.HasSuffix(got, "...") {
		t.Errorf("cap: %d runes", len([]rune(got)))
	}
}
