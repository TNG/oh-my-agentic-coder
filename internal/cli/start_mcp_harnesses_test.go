package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The tests in this file start the harness with a stand-in inner command. They
// check what omac passes; none of them runs the real harness, so they say
// nothing about whether the harness accepts it.

func clearHarnessMCPEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{"CODEX_HOME", "COPILOT_HOME", "PI_CODING_AGENT_DIR", "CODEWHALE_HOME", "CODEWHALE_MCP_CONFIG", "DEEPSEEK_MCP_CONFIG"} {
		t.Setenv(name, "")
	}
}

// captureWithFile records the arguments, the environment, and a copy of the
// file that the environment variable $1 or the argument following flag names;
// omac removes the sandbox temp dir on exit.
func (r *mcpRig) captureCopying(envVar, flag string) string {
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$OMAC_TEST_ARGS\"\nenv > \"$OMAC_TEST_ENV\"\n"
	if envVar != "" {
		script += "[ -n \"$" + envVar + "\" ] && cp \"$" + envVar + "\" \"$OMAC_TEST_ARGS.cfg\"\n"
	}
	if flag != "" {
		script += "prev=; for a in \"$@\"; do [ \"$prev\" = '" + flag + "' ] && f=\"${a#@}\" && cp \"$f\" \"$OMAC_TEST_ARGS.cfg\"; prev=\"$a\"; done\n"
	}
	return r.writeInner(script)
}

func (r *mcpRig) rawArgs() []string {
	r.t.Helper()
	raw, err := os.ReadFile(r.argsPath)
	if err != nil {
		r.t.Fatalf("inner command did not run: %v", err)
	}
	return strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
}

func (r *mcpRig) copiedConfig() map[string]any {
	r.t.Helper()
	return r.claudeConfig()
}

func TestMCPCodeWhaleReceivesTheRegisteredServer(t *testing.T) {
	clearHarnessMCPEnv(t)
	r := newMCPRig(t)
	r.register("demo", true, "codewhale")

	code, stderr := r.launch("codewhale", r.captureCopying("CODEWHALE_MCP_CONFIG", ""), nil)
	if code != ExitOK {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	_, env, ran := r.captured()
	if !ran {
		t.Fatal("the inner command did not run")
	}
	if env["CODEWHALE_MCP_CONFIG"] == "" {
		t.Fatal("CODEWHALE_MCP_CONFIG not set")
	}
	servers, _ := r.copiedConfig()["mcpServers"].(map[string]any)
	entry, _ := servers["demo"].(map[string]any)
	if entry == nil {
		t.Fatalf("no demo entry: %v", servers)
	}
	cfgEnv, _ := entry["env"].(map[string]any)
	if cfgEnv["OMAC_FACADE_TOKEN"] == "" || cfgEnv["OMAC_BASE"] == "" {
		t.Errorf("bridge env missing: %v", cfgEnv)
	}
	if strings.Contains(string(mustJSON(t, entry)), "tok-from-keychain") {
		t.Error("a registered secret reached the harness config")
	}
	if logs := r.gatewayLogs(); len(logs) == 0 {
		t.Error("no gateway started")
	}
}

func TestMCPCodeWhaleDefaultsToRefuseAndIgnoreFallsBack(t *testing.T) {
	clearHarnessMCPEnv(t)
	for _, mode := range []string{"", "ignore"} {
		r := newMCPRig(t)
		r.register("demo", true, "codewhale")
		r.writeFile(".codewhale/mcp.json", `{"mcpServers":{"native":{"command":"x"}}}`)

		code, stderr := r.launch("codewhale", r.capture, func(o *launchOpts) { o.mcpUnregistered = mode })
		if code != ExitConfigInvalid {
			t.Fatalf("mode %q: exit %d\n%s", mode, code, stderr)
		}
		if !strings.Contains(stderr, `unregistered MCP server "native"`) || !strings.Contains(stderr, "omac mcp add --scope project --harness codewhale") {
			t.Errorf("mode %q: refusal lacks the entry or the add hint:\n%s", mode, stderr)
		}
		if mode == "ignore" && !strings.Contains(stderr, "ignore is not available for codewhale") {
			t.Errorf("no fallback message:\n%s", stderr)
		}
		if _, _, ran := r.captured(); ran {
			t.Error("the inner command ran")
		}
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestMCPCopilotReceivesTheRegisteredServer(t *testing.T) {
	clearHarnessMCPEnv(t)
	r := newMCPRig(t)
	r.register("demo", true, "copilot")

	code, stderr := r.launch("copilot", r.captureCopying("", "--additional-mcp-config"), nil)
	if code != ExitOK {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	args := r.rawArgs()
	flagAt := -1
	for i, a := range args {
		if a == "--additional-mcp-config" {
			flagAt = i
		}
	}
	if flagAt < 0 || flagAt+1 >= len(args) || !strings.HasPrefix(args[flagAt+1], "@") {
		t.Fatalf("args = %q", args)
	}
	servers, _ := r.copiedConfig()["mcpServers"].(map[string]any)
	entry, _ := servers["demo"].(map[string]any)
	if entry == nil || entry["type"] != "local" {
		t.Fatalf("entry = %v", servers)
	}
	if strings.Contains(strings.Join(args, " "), "tok-from-keychain") || strings.Contains(string(mustJSON(t, entry)), "tok-from-keychain") {
		t.Error("a registered secret reached the harness config")
	}
}

func TestMCPCopilotTurnsOffItsOwnEntriesByNameAndWarns(t *testing.T) {
	clearHarnessMCPEnv(t)
	for _, mode := range []string{"", "ignore"} {
		r := newMCPRig(t)
		r.register("demo", true, "copilot")
		r.writeFile(".mcp.json", `{"mcpServers":{"native":{"command":"x"}}}`)

		code, stderr := r.launch("copilot", r.capture, func(o *launchOpts) { o.mcpUnregistered = mode })
		if code != ExitOK {
			t.Fatalf("mode %q: exit %d\n%s", mode, code, stderr)
		}
		if !strings.Contains(stderr, `warning: MCP server "native"`) || !strings.Contains(stderr, "turned off for this run") {
			t.Errorf("mode %q: no warning:\n%s", mode, stderr)
		}
		args := strings.Join(r.rawArgs(), " ")
		if !strings.Contains(args, "--disable-mcp-server native") || !strings.Contains(args, "--additional-mcp-config") {
			t.Errorf("mode %q: args = %q", mode, args)
		}
	}
}

func TestMCPCopilotRefusesWhatItCannotTurnOff(t *testing.T) {
	clearHarnessMCPEnv(t)
	files := map[string]string{
		"same name as a registered server": `{"mcpServers":{"demo":{"command":"x"}}}`,
		"name that needs quoting":          `{"mcpServers":{"a b":{"command":"x"}}}`,
		"unparseable file":                 `{"mcpServers":`,
	}
	for name, content := range files {
		r := newMCPRig(t)
		r.register("demo", true, "copilot")
		r.writeFile(".mcp.json", content)

		code, stderr := r.launch("copilot", r.capture, nil)
		if code != ExitConfigInvalid || !strings.Contains(stderr, "MCP policy refuses this start") {
			t.Fatalf("%s: exit %d\n%s", name, code, stderr)
		}
		if _, _, ran := r.captured(); ran {
			t.Errorf("%s: the inner command ran", name)
		}
	}
}

func TestMCPCopilotUserAdditionalConfigIsNotCombined(t *testing.T) {
	clearHarnessMCPEnv(t)
	r := newMCPRig(t)
	r.register("demo", true, "copilot")

	code, stderr := r.launch("copilot", r.capture, func(o *launchOpts) {
		o.mcpUnregistered = "warn"
		o.innerArgs = []string{"--additional-mcp-config", `{"mcpServers":{}}`}
	})
	if code == ExitOK || !strings.Contains(stderr, "--additional-mcp-config") {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	if _, _, ran := r.captured(); ran {
		t.Error("the inner command ran")
	}
}

func TestMCPCodexReceivesTheRegisteredServer(t *testing.T) {
	clearHarnessMCPEnv(t)
	r := newMCPRig(t)
	r.register("demo", true, "codex")

	code, stderr := r.launch("codex", r.capture, nil)
	if code != ExitOK {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	args := r.rawArgs()
	if len(args) < 2 || args[0] != "-c" || !strings.HasPrefix(args[1], "mcp_servers.demo={command = ") {
		t.Fatalf("args = %q", args)
	}
	if !strings.Contains(args[1], `env_vars = ["OMAC_BASE", "OMAC_FACADE_TOKEN", "OMAC_SOCKET"]`) {
		t.Errorf("env_vars missing: %s", args[1])
	}
	_, env, _ := r.captured()
	if env["OMAC_FACADE_TOKEN"] == "" || strings.Contains(strings.Join(args, " "), env["OMAC_FACADE_TOKEN"]) {
		t.Errorf("the facade token is missing from the environment or present in the arguments")
	}
	if strings.Contains(strings.Join(args, " "), "tok-from-keychain") {
		t.Error("a registered secret is on the command line")
	}
	if logs := r.gatewayLogs(); len(logs) == 0 {
		t.Error("no gateway started")
	}
}

func TestMCPCodexTurnsOffItsOwnUserEntriesByNameAndWarns(t *testing.T) {
	clearHarnessMCPEnv(t)
	for _, mode := range []string{"", "ignore"} {
		home := t.TempDir()
		t.Setenv("CODEX_HOME", home)
		if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("[mcp_servers.native]\ncommand = \"x\"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		r := newMCPRig(t)
		r.register("demo", true, "codex")

		code, stderr := r.launch("codex", r.capture, func(o *launchOpts) { o.mcpUnregistered = mode })
		if code != ExitOK {
			t.Fatalf("mode %q: exit %d\n%s", mode, code, stderr)
		}
		if !strings.Contains(stderr, `warning: MCP server "native"`) {
			t.Errorf("mode %q: no warning:\n%s", mode, stderr)
		}
		args := strings.Join(r.rawArgs(), "\n")
		if !strings.Contains(args, "mcp_servers.native.enabled=false") || !strings.Contains(args, "mcp_servers.demo={") {
			t.Errorf("mode %q: args = %q", mode, args)
		}
	}
}

func TestMCPCodexRefusesWhatItCannotTurnOff(t *testing.T) {
	clearHarnessMCPEnv(t)
	cases := map[string]struct{ user, project string }{
		"project entry (not known to be loaded)": {project: "[mcp_servers.native]\ncommand = \"x\"\n"},
		"same name as a registered server":       {user: "[mcp_servers.demo]\ncommand = \"x\"\n"},
		"unparseable file":                       {user: "[mcp_servers.native\n"},
	}
	for name, c := range cases {
		home := t.TempDir()
		t.Setenv("CODEX_HOME", home)
		if c.user != "" {
			if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(c.user), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		r := newMCPRig(t)
		r.register("demo", true, "codex")
		if c.project != "" {
			r.writeFile(".codex/config.toml", c.project)
		}

		code, stderr := r.launch("codex", r.capture, nil)
		if code != ExitConfigInvalid || !strings.Contains(stderr, "MCP policy refuses this start") {
			t.Fatalf("%s: exit %d\n%s", name, code, stderr)
		}
		if _, _, ran := r.captured(); ran {
			t.Errorf("%s: the inner command ran", name)
		}
	}
}

func TestMCPPiReceivesTheRegisteredServer(t *testing.T) {
	clearHarnessMCPEnv(t)
	r := newMCPRig(t)
	r.register("demo", true, "pi")

	code, stderr := r.launch("pi", r.captureCopying("", "-e"), nil)
	if code != ExitOK {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	args := r.rawArgs()
	if len(args) < 2 || args[0] != "-e" || !strings.HasSuffix(args[1], "omac-mcp-pi.js") {
		t.Fatalf("args = %q", args)
	}
	code2, err := os.ReadFile(r.argsPath + ".cfg")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(code2), `pi.registerMcpServer(name, config)`) || !strings.Contains(string(code2), `[["demo",`) {
		t.Errorf("the extension does not register the server:\n%s", code2)
	}
	_, env, _ := r.captured()
	if env["OMAC_FACADE_TOKEN"] == "" || strings.Contains(string(code2), env["OMAC_FACADE_TOKEN"]) {
		t.Error("the facade token is missing from the environment or present in the extension")
	}
	if strings.Contains(string(code2)+strings.Join(args, " "), "tok-from-keychain") {
		t.Error("a registered secret reached the extension or the command line")
	}
	if logs := r.gatewayLogs(); len(logs) == 0 {
		t.Error("no gateway started")
	}
}

func TestMCPPiDefaultsToRefuseAndIgnoreFallsBack(t *testing.T) {
	clearHarnessMCPEnv(t)
	for _, mode := range []string{"", "ignore"} {
		r := newMCPRig(t)
		r.register("demo", true, "pi")
		r.writeFile(".pi/mcp.json", `{"mcpServers":{"native":{"command":"x"}}}`)

		code, stderr := r.launch("pi", r.capture, func(o *launchOpts) { o.mcpUnregistered = mode })
		if code != ExitConfigInvalid || !strings.Contains(stderr, `unregistered MCP server "native"`) {
			t.Fatalf("mode %q: exit %d\n%s", mode, code, stderr)
		}
		if mode == "ignore" && !strings.Contains(stderr, "ignore is not available for pi") {
			t.Errorf("no fallback message:\n%s", stderr)
		}
		if _, _, ran := r.captured(); ran {
			t.Error("the inner command ran")
		}
	}
}

func TestExperimentalHarnessesWarnAtStart(t *testing.T) {
	clearHarnessMCPEnv(t)
	for name, want := range map[string]bool{"pi": true, "codewhale": true, "codex": false, "copilot": false, "claude": false, "opencode": false} {
		r := newMCPRig(t)
		code, stderr := r.launch(name, r.capture, nil)
		if code != ExitOK {
			t.Fatalf("%s: exit %d\n%s", name, code, stderr)
		}
		if got := strings.Contains(stderr, "experimental harness in omac"); got != want {
			t.Errorf("%s: warning printed = %v, want %v\n%s", name, got, want, stderr)
		}
	}
}

func TestOpenCodeV1WarnsAsLegacyAndV2DoesNot(t *testing.T) {
	clearHarnessMCPEnv(t)
	for version, want := range map[string]bool{"1.4.2": true, "2.1.0": false} {
		r := newMCPRig(t)
		bin := filepath.Join(t.TempDir(), "opencode")
		script := "#!/bin/sh\n[ \"$1\" = --version ] && echo " + version + " && exit 0\nprintf '%s\\n' \"$@\" > \"$OMAC_TEST_ARGS\"\nenv > \"$OMAC_TEST_ENV\"\n"
		if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		code, stderr := r.launch("opencode", bin, nil)
		if code != ExitOK {
			t.Fatalf("%s: exit %d\n%s", version, code, stderr)
		}
		if got := strings.Contains(stderr, "OpenCode v1 is legacy"); got != want {
			t.Errorf("%s: legacy warning printed = %v, want %v\n%s", version, got, want, stderr)
		}
	}
}
