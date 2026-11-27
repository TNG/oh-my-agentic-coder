package cli

import (
	"os"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
	"github.com/TNG/oh-my-agentic-coder/internal/mcp/mcpreg"
)

// withoutMCPInjection makes the named harness one that cannot receive MCP
// servers until the test ends; every built-in harness can receive them now.
func withoutMCPInjection(t *testing.T, name string) {
	t.Helper()
	restore := config.OverrideHarnessesForTest(func(reg []config.Harness) []config.Harness {
		for i := range reg {
			if reg[i].Name == name {
				reg[i].MCPInjection = config.MCPInjectNone
			}
		}
		return reg
	})
	t.Cleanup(restore)
}

func unsupportedRig(t *testing.T) *mcpRig {
	t.Helper()
	withoutMCPInjection(t, "pi")
	r := newMCPRig(t)
	for _, name := range []string{"CODEX_HOME", "COPILOT_HOME", "PI_CODING_AGENT_DIR", "CODEWHALE_HOME", "CODEWHALE_MCP_CONFIG", "DEEPSEEK_MCP_CONFIG"} {
		t.Setenv(name, "")
	}
	return r
}

func (r *mcpRig) registerGlobal(name string, harnesses ...string) {
	r.t.Helper()
	exe, err := executablePath()
	if err != nil {
		r.t.Fatal(err)
	}
	defPath := r.t.TempDir() + "/mcp.yaml"
	if err := os.WriteFile(defPath, []byte(fakeMCPDefinition(name, exe)), 0o600); err != nil {
		r.t.Fatal(err)
	}
	if _, err := mcpreg.Register(mcpreg.GlobalScope(), defPath, harnesses); err != nil {
		r.t.Fatal(err)
	}
}

func TestMCPUnsupportedHarnessRefusesRegisteredServers(t *testing.T) {
	tests := []struct {
		name     string
		harness  string
		register func(r *mcpRig)
	}{
		{"project scope", "pi", func(r *mcpRig) { r.register("demo", false, "opencode") }},
		{"global scope", "pi", func(r *mcpRig) { r.registerGlobal("demo", "claude-code") }},
		{"registered for another harness", "pi", func(r *mcpRig) { r.register("demo", false, "opencode", "claude-code") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := unsupportedRig(t)
			tt.register(r)

			code, stderr := r.launch(tt.harness, r.capture, nil)
			if code != ExitConfigInvalid {
				t.Fatalf("exit %d, want %d\n%s", code, ExitConfigInvalid, stderr)
			}
			for _, want := range []string{
				"omac start: MCP policy refuses this start",
				"omac cannot provide MCP servers to " + tt.harness + " yet (supported: " + supportedMCPHarnesses() + "). Registered servers that will not be available: demo.",
				"Use a supported harness, or run once with --mcp-unregistered=warn",
				"--mcp-unregistered=allow",
			} {
				if !strings.Contains(stderr, want) {
					t.Errorf("refusal lacks %q:\n%s", want, stderr)
				}
			}
			if _, _, ran := r.captured(); ran {
				t.Error("the inner command ran")
			}
			if logs := r.gatewayLogs(); len(logs) != 0 {
				t.Errorf("a gateway started: %v", logs)
			}
		})
	}
}

func TestMCPUnsupportedHarnessWarnAndAllowStartWithoutMCP(t *testing.T) {
	tests := []struct {
		mode        string
		wantWarning bool
	}{{"warn", true}, {"allow", false}}
	for _, tt := range tests {
		t.Run(tt.mode, func(t *testing.T) {
			r := unsupportedRig(t)
			r.register("demo", false, "opencode")

			code, stderr := r.launch("pi", r.capture, func(o *launchOpts) { o.mcpUnregistered = tt.mode })
			if code != ExitOK {
				t.Fatalf("exit %d\n%s", code, stderr)
			}
			warned := strings.Contains(stderr, "warning: "+"omac cannot provide MCP servers to pi yet (supported: "+supportedMCPHarnesses()+"). Registered servers that will not be available: demo.")
			if warned != tt.wantWarning {
				t.Errorf("warning present = %v, want %v:\n%s", warned, tt.wantWarning, stderr)
			}
			if !tt.wantWarning && strings.Contains(withoutExperimentalNotice(stderr), "MCP") {
				t.Errorf("allow printed something:\n%s", stderr)
			}
			args, env, ran := r.captured()
			if !ran {
				t.Fatal("the inner command did not run")
			}
			for _, a := range args {
				if strings.Contains(a, "mcp") {
					t.Errorf("unexpected argument %q", a)
				}
			}
			if env["OPENCODE_CONFIG_CONTENT"] != "" {
				t.Error("MCP configuration was injected")
			}
			if logs := r.gatewayLogs(); len(logs) != 0 {
				t.Errorf("a gateway started: %v", logs)
			}
		})
	}
}

func TestMCPUnsupportedHarnessWithNothingRegisteredChangesNothing(t *testing.T) {
	r := unsupportedRig(t)
	r.writeFile(".pi/mcp.json", `{"mcpServers":{"native":{"command":"x"}}}`)

	code, stderr := r.launch("pi", r.capture, nil)
	if code != ExitOK || strings.Contains(withoutExperimentalNotice(stderr), "MCP") {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	if _, _, ran := r.captured(); !ran {
		t.Fatal("the inner command did not run")
	}
}

func TestMCPUnsupportedHarnessExplicitModeScansNativeConfigWithoutRegistrations(t *testing.T) {
	r := unsupportedRig(t)
	r.writeFile(".pi/mcp.json", `{"mcpServers":{"native":{"command":"x","args":["a"],"env":{"TOKEN":"value-must-not-print"}}}}`)

	code, stderr := r.launch("pi", r.capture, func(o *launchOpts) { o.mcpUnregistered = "refuse" })
	if code != ExitConfigInvalid {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	if !strings.Contains(stderr, `unregistered MCP server "native"`) || !strings.Contains(stderr, ".pi/mcp.json") {
		t.Errorf("refusal lacks the entry:\n%s", stderr)
	}
	if strings.Contains(stderr, "cannot provide") || strings.Contains(stderr, "omac mcp add") || strings.Contains(stderr, "value-must-not-print") {
		t.Errorf("unexpected text in:\n%s", stderr)
	}

	code, stderr = r.launch("pi", r.capture, func(o *launchOpts) { o.mcpUnregistered = "warn" })
	if code != ExitOK || !strings.Contains(stderr, "warning: ") || !strings.Contains(stderr, `"native"`) {
		t.Fatalf("warn: exit %d\n%s", code, stderr)
	}
}

func TestMCPUnsupportedHarnessNativeEntriesAreRefusedWithRegistrations(t *testing.T) {
	r := unsupportedRig(t)
	r.register("demo", false, "opencode")
	r.writeFile(".pi/mcp.json", `{"mcpServers":{"native":{"command":"x"}}}`)

	code, stderr := r.launch("pi", r.capture, nil)
	if code != ExitConfigInvalid {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	for _, want := range []string{"Registered servers that will not be available: demo.", `unregistered MCP server "native"`, "rotate"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("refusal lacks %q:\n%s", want, stderr)
		}
	}
	if strings.Contains(stderr, "omac mcp add") {
		t.Errorf("a hint for an unsupported harness was printed:\n%s", stderr)
	}
}

func TestMCPUnsupportedHarnessSharesThePathOfContinueAndResume(t *testing.T) {
	r := unsupportedRig(t)
	r.register("demo", false, "opencode")
	harness, _ := config.LookupHarness("pi")
	for _, label := range []string{"continue", "resume"} {
		env, stderr := launchTestEnv(t, r.wd)
		plan, code := prepareMCP(env, harness, config.LauncherConfig{}, launchOpts{label: label})
		if plan != nil || code != ExitConfigInvalid || !strings.Contains(stderr(), "omac "+label+": MCP policy refuses this start") {
			t.Errorf("%s: plan=%v code=%d stderr=%q", label, plan, code, stderr())
		}
	}
}

func TestMCPRegistrationRejectsHarnessWithoutMCPSupport(t *testing.T) {
	withoutMCPInjection(t, "pi")
	wd, def := mcpTestSetup(t)
	t.Setenv("OMAC_SECRET_API_TOKEN", "tok")
	want := "omac cannot provide MCP servers to %s yet (supported: " + supportedMCPHarnesses() + ")"
	for _, harness := range []string{"pi", "opencode,pi"} {
		t.Run(harness, func(t *testing.T) {
			resolved, _ := config.LookupHarness(strings.TrimPrefix(harness, "opencode,"))
			for _, args := range [][]string{
				{"register", def, "--harness", harness},
				{"add", "demo", "--harness", harness, "--", "/usr/bin/env"},
				{"add-json", "demo", `{"command":"env"}`, "--harness", harness},
			} {
				code, _, errOut := runAdd(t, wd, args...)
				if code != ExitMisuse || !strings.Contains(errOut, strings.Replace(want, "%s", resolved.Name, 1)) {
					t.Errorf("%s: exit %d, stderr %q", args[0], code, errOut)
				}
			}
			if _, err := mcpreg.Load(mcpreg.ScopeFor(wd), "demo"); err == nil {
				t.Error("the server was registered")
			}
		})
	}
}

func TestMCPRegistrationKeepsUnknownHarnessError(t *testing.T) {
	wd, def := mcpTestSetup(t)
	code, _, errOut := runAdd(t, wd, "register", def, "--harness", "nope")
	if code != ExitMisuse || !strings.Contains(errOut, `unknown harness "nope"`) {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
}

func TestMCPDiagnoseScansHarnessesWithoutInjectionAndFlagsUnsupportedRegistrations(t *testing.T) {
	withoutMCPInjection(t, "pi")
	f := newDiagFixture(t)
	for _, name := range []string{"CODEX_HOME", "COPILOT_HOME", "PI_CODING_AGENT_DIR", "CODEWHALE_HOME", "CODEWHALE_MCP_CONFIG", "DEEPSEEK_MCP_CONFIG"} {
		t.Setenv(name, "")
	}
	if err := os.MkdirAll(f.wd+"/.pi", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.wd+"/.pi/mcp.json", []byte(`{"mcpServers":{"native":{"command":"npx"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.wd+"/.mcp.json", []byte(`{"mcpServers":{"viaclaude":{"command":"x"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := mcpreg.RegisterDefinition(f.scope, reviewedDef("old", "sh", nil), []string{"opencode", "pi"}); err != nil {
		t.Fatal(err)
	}

	code, out, _ := runDiagnoseCmd(t, f.wd)
	if code != ExitGeneric {
		t.Fatalf("exit %d\n%s", code, out)
	}
	for _, want := range []string{
		`pi: "native" in ` + f.wd + "/.pi/mcp.json is not managed by omac",
		"omac cannot provide registered MCP servers to pi",
		`claude-code: "viaclaude"`,
		"omac cannot provide MCP servers to pi yet (supported: " + supportedMCPHarnesses() + "); this server will not be available there",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("diagnose lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "register it: omac mcp add --scope project --harness pi") {
		t.Errorf("diagnose offers a rejected command:\n%s", out)
	}
}

// supportedMCPHarnesses is the list the unsupported-harness message names.
func supportedMCPHarnesses() string { return strings.Join(config.MCPCapableHarnessNames(), ", ") }

// withoutExperimentalNotice drops the start notice for experimental harnesses, which names MCP
// but is not about the MCP handling these tests check.
func withoutExperimentalNotice(stderr string) string {
	var kept []string
	for _, line := range strings.Split(stderr, "\n") {
		if !strings.Contains(line, "experimental harness in omac") {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}
