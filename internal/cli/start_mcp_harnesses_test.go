package cli

import (
	"encoding/json"
	"os"
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
