package cli

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/mcp/mcppolicy"
	"github.com/TNG/oh-my-agentic-coder/internal/mcp/mcpreg"
	"github.com/TNG/oh-my-agentic-coder/internal/secrets"
)

type diagFixture struct {
	wd    string
	scope mcpreg.Scope
}

func newDiagFixture(t *testing.T) diagFixture {
	t.Helper()
	wd, _ := mcpTestSetup(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	return diagFixture{wd: wd, scope: mcpreg.ScopeFor(wd)}
}

func (f diagFixture) register(t *testing.T, def *mcpreg.Definition) *mcpreg.Entry {
	t.Helper()
	e, err := mcpreg.RegisterDefinition(f.scope, def, []string{"opencode"})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func reviewedDef(name string, command string, classes map[string]mcppolicy.Class, reviewed ...string) *mcpreg.Definition {
	def := &mcpreg.Definition{Name: name, Server: mcpreg.ServerSpec{Command: []string{command}}}
	for tool, class := range classes {
		def.Tools = append(def.Tools, mcppolicy.Tool{Name: tool, Class: class})
	}
	hashes := map[string]string{}
	for _, tool := range reviewed {
		hashes[tool] = "hash-" + tool
	}
	def.Review = &mcppolicy.Review{ToolsHash: mcppolicy.ListHash(hashes), Tools: hashes, At: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
	return def
}

func runDiagnoseCmd(t *testing.T, wd string, args ...string) (int, string, string) {
	t.Helper()
	env, read := captureEnv(t, wd)
	code := runMCP(append([]string{"diagnose"}, args...), env)
	out, errOut := read()
	return code, out, errOut
}

func TestMCPDiagnoseCorruptAndHealthy(t *testing.T) {
	f := newDiagFixture(t)
	f.register(t, reviewedDef("good", "sh", map[string]mcppolicy.Class{"echo": mcppolicy.Read}, "echo"))
	bad := f.register(t, reviewedDef("bad", "sh", nil))
	if err := os.WriteFile(bad.DefPath, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, _ := runDiagnoseCmd(t, f.wd)
	if code != ExitGeneric {
		t.Fatalf("exit %d, out %q", code, out)
	}
	if !strings.Contains(out, "bad (project") || !strings.Contains(out, "fails to load") || !strings.Contains(out, "good (project") || !strings.Contains(out, "): ok\n") {
		t.Fatalf("output: %q", out)
	}
	if !strings.Contains(out, "1 problem(s) found") {
		t.Fatalf("summary: %q", out)
	}
}

func TestMCPDiagnoseCleanExitsZero(t *testing.T) {
	f := newDiagFixture(t)
	f.register(t, reviewedDef("good", "sh", map[string]mcppolicy.Class{"echo": mcppolicy.Read}, "echo"))
	code, out, _ := runDiagnoseCmd(t, f.wd)
	if code != ExitOK || !strings.Contains(out, "no problems found") {
		t.Fatalf("exit %d, out %q", code, out)
	}
}

func TestMCPDiagnoseMissingSecretByNameOnly(t *testing.T) {
	f := newDiagFixture(t)
	def := reviewedDef("srv", "sh", map[string]mcppolicy.Class{"echo": mcppolicy.Read}, "echo")
	def.Secrets = []string{"API_TOKEN", "OTHER_TOKEN"}
	def.Server.Env = map[string]string{"VISIBLE": "env-value-xyz"}
	f.register(t, def)
	if err := mcpreg.SetSecret(f.scope, "srv", "OTHER_TOKEN", secrets.NewSecretString("secret-value-abc")); err != nil {
		t.Fatal(err)
	}
	code, out, _ := runDiagnoseCmd(t, f.wd)
	if code != ExitGeneric || !strings.Contains(out, "secrets missing from the keychain: API_TOKEN") {
		t.Fatalf("exit %d, out %q", code, out)
	}
	if strings.Contains(out, "OTHER_TOKEN") || strings.Contains(out, "secret-value-abc") || strings.Contains(out, "env-value-xyz") {
		t.Fatalf("leak or false report: %q", out)
	}
}

func TestMCPDiagnoseUnclassifiedAndUnreviewed(t *testing.T) {
	f := newDiagFixture(t)
	f.register(t, reviewedDef("half", "sh", map[string]mcppolicy.Class{"echo": mcppolicy.Read}, "echo", "wipe"))
	f.register(t, reviewedDef("noreview", "sh", nil))
	_, err := mcpreg.Update(f.scope, "noreview", func(d *mcpreg.Definition) error { d.Review = nil; return nil })
	if err != nil {
		t.Fatal(err)
	}
	code, out, _ := runDiagnoseCmd(t, f.wd)
	if code != ExitGeneric {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out, "tools not classified (refused until classified): wipe") {
		t.Fatalf("unclassified missing: %q", out)
	}
	if !strings.Contains(out, "tools not reviewed yet; run: omac mcp tools noreview --classify") {
		t.Fatalf("not-reviewed message missing: %q", out)
	}
}

func TestMCPDiagnoseCommandProblems(t *testing.T) {
	f := newDiagFixture(t)
	f.register(t, reviewedDef("nocmd", "definitely-not-a-program-xyz", nil))
	_, out, _ := runDiagnoseCmd(t, f.wd)
	if !strings.Contains(out, `command "definitely-not-a-program-xyz" not found`) {
		t.Fatalf("output: %q", out)
	}
}

func TestMCPDiagnoseUnregisteredHarnessEntry(t *testing.T) {
	f := newDiagFixture(t)
	cfg := `{"mcp": {"stray": {"type": "local", "command": ["npx", "stray"], "environment": {"K": "very-secret"}}}}`
	if err := os.WriteFile(f.wd+"/opencode.json", []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, _ := runDiagnoseCmd(t, f.wd)
	if code != ExitGeneric || !strings.Contains(out, `"stray"`) || !strings.Contains(out, "opencode.json") || !strings.Contains(out, "not managed by omac") {
		t.Fatalf("exit %d, out %q", code, out)
	}
	if strings.Contains(out, "very-secret") {
		t.Fatalf("leak: %q", out)
	}
}

func fakeProbe(t *testing.T, tools []mcppolicy.ToolInfo, tail string, err error) {
	t.Helper()
	orig := mcpProbeListTools
	t.Cleanup(func() { mcpProbeListTools = orig })
	mcpProbeListTools = func(context.Context, *mcpreg.Entry, map[string]secrets.Secret, []string, time.Duration) ([]mcppolicy.ToolInfo, string, error) {
		return tools, tail, err
	}
}

func TestMCPDiagnoseProbeChanged(t *testing.T) {
	f := newDiagFixture(t)
	f.register(t, reviewedDef("srv", "sh", map[string]mcppolicy.Class{"echo": mcppolicy.Read, "gone": mcppolicy.Read}, "echo", "gone"))
	fakeProbe(t, []mcppolicy.ToolInfo{{Name: "echo", Hash: "other"}, {Name: "fresh", Hash: "h"}}, "", nil)
	code, out, _ := runDiagnoseCmd(t, f.wd, "--probe")
	if code != ExitGeneric {
		t.Fatalf("exit %d", code)
	}
	for _, want := range []string{"changed since the last review (added: fresh)", "(removed: gone)", "(changed: echo)"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in %q", want, out)
		}
	}
}

func TestMCPDiagnoseProbeMatchingReviewIsClean(t *testing.T) {
	f := newDiagFixture(t)
	f.register(t, reviewedDef("srv", "sh", map[string]mcppolicy.Class{"echo": mcppolicy.Read}, "echo"))
	fakeProbe(t, []mcppolicy.ToolInfo{{Name: "echo", Hash: "hash-echo"}}, "", nil)
	if code, out, _ := runDiagnoseCmd(t, f.wd, "--probe"); code != ExitOK {
		t.Fatalf("exit %d, out %q", code, out)
	}
}

func TestMCPDiagnoseProbeStartErrorShowsStderrTail(t *testing.T) {
	f := newDiagFixture(t)
	f.register(t, reviewedDef("srv", "sh", map[string]mcppolicy.Class{"echo": mcppolicy.Read}, "echo"))
	fakeProbe(t, nil, "boom: missing module\n", errors.New("exited early"))
	code, out, _ := runDiagnoseCmd(t, f.wd, "--probe")
	if code != ExitGeneric || !strings.Contains(out, "probe failed: exited early") || !strings.Contains(out, "boom: missing module") {
		t.Fatalf("exit %d, out %q", code, out)
	}
}

func TestMCPDiagnoseProbeRefusedInsideSandbox(t *testing.T) {
	f := newDiagFixture(t)
	t.Setenv("OMAC_FACADE_TOKEN", "tok")
	fakeProbe(t, nil, "", errors.New("must not run"))
	code, out, errOut := runDiagnoseCmd(t, f.wd, "--probe")
	if code != ExitMisuse || out != "" || !strings.Contains(errOut, "refused inside the omac sandbox") {
		t.Fatalf("exit %d, out %q, err %q", code, out, errOut)
	}
	if code, _, _ := runDiagnoseCmd(t, f.wd); code != ExitOK {
		t.Fatalf("plain diagnose inside sandbox exit %d", code)
	}
}

func TestMCPShowAndListGolden(t *testing.T) {
	f := newDiagFixture(t)
	def := reviewedDef("srv", "sh", map[string]mcppolicy.Class{"echo": mcppolicy.Read, "rm": mcppolicy.Delete}, "echo", "rm", "extra")
	def.Secrets = []string{"API_TOKEN"}
	def.Server.Env = map[string]string{"VISIBLE": "env-value-xyz"}
	def.Access = mcppolicy.ReadWrite
	f.register(t, def)
	if err := mcpreg.SetSecret(f.scope, "srv", "API_TOKEN", secrets.NewSecretString("secret-value-abc")); err != nil {
		t.Fatal(err)
	}

	env, read := captureEnv(t, f.wd)
	if code := runMCP([]string{"show", "srv"}, env); code != ExitOK {
		t.Fatalf("show exit %d", code)
	}
	out, _ := read()
	for _, want := range []string{
		"access:      read-write\n",
		"approvals:   read=none write=required delete=required\n",
		"reviewed:    2026-01-02  3 tools  fingerprint " + mcppolicy.ListHash(def.Review.Tools)[:12] + "\n",
		"  echo                             read\n",
		"  extra                            unclassified\n",
		"  rm                               delete\n",
		"harnesses:   opencode\n",
		"API_TOKEN (stored)",
		"env:         VISIBLE",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("show lacks %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "secret-value-abc") || strings.Contains(out, "env-value-xyz") {
		t.Fatalf("show leaks a value:\n%s", out)
	}

	env, read = captureEnv(t, f.wd)
	if code := runMCP([]string{"list"}, env); code != ExitOK {
		t.Fatalf("list exit %d", code)
	}
	out, _ = read()
	if !strings.Contains(out, "opencode") || !strings.Contains(out, "  3 tools   2 classified  reviewed") {
		t.Fatalf("list output: %q", out)
	}
	if strings.Contains(out, "secret-value-abc") || strings.Contains(out, "env-value-xyz") {
		t.Fatalf("list leaks a value: %q", out)
	}
}
