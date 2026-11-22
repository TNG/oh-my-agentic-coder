package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
	"github.com/TNG/oh-my-agentic-coder/internal/mcp/mcppolicy"
	"github.com/TNG/oh-my-agentic-coder/internal/mcp/mcpreg"
	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
	"github.com/TNG/oh-my-agentic-coder/internal/secrets"
)

// The test binary doubles as the omac binary (the gateway and the bridge are
// started as `<test binary> mcp ...`) and as the fake stdio MCP server.
func init() {
	if os.Getenv("OMAC_TEST_FAKE_MCP") == "1" {
		fakeMCPServer()
		os.Exit(0)
	}
	if len(os.Args) > 1 && os.Args[1] == "mcp" {
		os.Exit(Run(os.Args[1:], "test"))
	}
}

func fakeMCPServer() {
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 1<<20), 1<<20)
	out := json.NewEncoder(os.Stdout)
	for in.Scan() {
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.Unmarshal(in.Bytes(), &m) != nil || len(m.ID) == 0 {
			continue
		}
		var result any
		switch m.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"tools": map[string]any{}}}
		case "tools/list":
			result = map[string]any{"tools": fakeMCPTools(os.Getenv("FAKE_TOKEN"))}
		case "tools/call":
			var call struct {
				Params struct {
					Name string `json:"name"`
				} `json:"params"`
			}
			_ = json.Unmarshal(in.Bytes(), &call)
			if call.Params.Name == "hang" {
				select {}
			}
			result = map[string]any{"content": []any{}}
		default:
			result = map[string]any{}
		}
		_ = out.Encode(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": result})
	}
}

// fakeMCPTools is the tool list of the fake server; whoami's description is
// the secret so a test can see that it arrived.
func fakeMCPTools(token string) []any {
	schema := map[string]any{"type": "object"}
	return []any{
		map[string]any{"name": "whoami", "description": token, "inputSchema": schema},
		map[string]any{"name": "hang", "description": "never answers", "inputSchema": schema},
	}
}

// fakeMCPDefinition is a definition of the fake server that classifies and reviews
// its tools, so the gateway lets them through.
func fakeMCPDefinition(name, exe string) string {
	perTool := map[string]string{}
	for _, tool := range fakeMCPTools("tok-from-keychain") {
		raw, _ := json.Marshal(tool)
		hash, err := mcppolicy.ToolHash(raw)
		if err != nil {
			panic(err)
		}
		perTool[tool.(map[string]any)["name"].(string)] = hash
	}
	return fmt.Sprintf(`name: %s
server:
  command: [%q]
  env:
    OMAC_TEST_FAKE_MCP: "1"
secrets: [FAKE_TOKEN]
tools:
  - {name: whoami, class: read}
  - {name: hang, class: read}
reviewed:
  tools_hash: %s
  tools: {hang: %s, whoami: %s}
  at: 2026-01-01T00:00:00Z
`, name, exe, mcppolicy.ListHash(perTool), perTool["hang"], perTool["whoami"])
}

type mcpRig struct {
	t        *testing.T
	wd       string
	shortTmp string
	argsPath string
	envPath  string
	capture  string
}

func newMCPRig(t *testing.T) *mcpRig {
	t.Helper()
	isolateHome(t)
	t.Setenv("OMAC_KEYCHAIN_MOCK", filepath.Join(t.TempDir(), "keychain.json"))
	t.Setenv("OMAC_SOCKET", "")
	t.Setenv("OMAC_FACADE_TOKEN", "")
	t.Setenv("OPENCODE_CONFIG_CONTENT", "")
	shortTmp, err := os.MkdirTemp("/tmp", "omac-mcp-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(shortTmp) })
	t.Setenv("TMPDIR", shortTmp)
	t.Setenv("XDG_RUNTIME_DIR", shortTmp)

	r := &mcpRig{t: t, wd: t.TempDir(), shortTmp: shortTmp}
	r.argsPath = filepath.Join(t.TempDir(), "args")
	r.envPath = filepath.Join(t.TempDir(), "env")
	t.Setenv("OMAC_TEST_ARGS", r.argsPath)
	t.Setenv("OMAC_TEST_ENV", r.envPath)
	r.capture = r.writeInner("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$OMAC_TEST_ARGS\"\nenv > \"$OMAC_TEST_ENV\"\nfor a in \"$@\"; do case \"$a\" in --mcp-config=*) cp \"${a#--mcp-config=}\" \"$OMAC_TEST_ARGS.cfg\";; esac; done\n")
	return r
}

func (r *mcpRig) writeInner(script string) string {
	r.t.Helper()
	path := filepath.Join(r.t.TempDir(), "inner")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		r.t.Fatal(err)
	}
	return path
}

func (r *mcpRig) writeFile(rel, content string) {
	r.t.Helper()
	path := filepath.Join(r.wd, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		r.t.Fatal(err)
	}
}

func (r *mcpRig) writeLauncherConfig(content string) {
	r.t.Helper()
	path := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "omac", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		r.t.Fatal(err)
	}
}

// register registers a fake MCP server; withSecret stores its FAKE_TOKEN.
func (r *mcpRig) register(name string, withSecret bool, harnesses ...string) {
	r.t.Helper()
	exe, err := executablePath()
	if err != nil {
		r.t.Fatal(err)
	}
	def := fakeMCPDefinition(name, exe)
	defPath := filepath.Join(r.t.TempDir(), "mcp.yaml")
	if err := os.WriteFile(defPath, []byte(def), 0o600); err != nil {
		r.t.Fatal(err)
	}
	scope := mcpreg.ScopeFor(r.wd)
	if _, err := mcpreg.Register(scope, defPath, harnesses); err != nil {
		r.t.Fatal(err)
	}
	if withSecret {
		if err := mcpreg.SetSecret(scope, name, "FAKE_TOKEN", secrets.NewSecretString("tok-from-keychain")); err != nil {
			r.t.Fatal(err)
		}
	}
}

func (r *mcpRig) launch(harnessName, inner string, mod func(*launchOpts)) (int, string) {
	r.t.Helper()
	harness, ok := config.LookupHarness(harnessName)
	if !ok {
		r.t.Fatalf("harness %q missing", harnessName)
	}
	env, stderr := launchTestEnv(r.t, r.wd)
	opts := launchOpts{label: "start", harness: harness, innerCmdOverride: inner, noSandbox: true}
	if mod != nil {
		mod(&opts)
	}
	code := runLaunch(env, opts)
	return code, stderr()
}

func (r *mcpRig) captured() (args []string, env map[string]string, ran bool) {
	argBytes, err := os.ReadFile(r.argsPath)
	if err != nil {
		return nil, nil, false
	}
	envBytes, err := os.ReadFile(r.envPath)
	if err != nil {
		r.t.Fatal(err)
	}
	return strings.Fields(string(argBytes)), parseEnvironment(string(envBytes)), true
}

func (r *mcpRig) gatewayLogs() []string {
	var logs []string
	_ = filepath.WalkDir(r.shortTmp, func(path string, d fs.DirEntry, err error) error {
		if err == nil && strings.HasPrefix(d.Name(), "mcp-") && strings.HasSuffix(d.Name(), ".log") {
			logs = append(logs, path)
		}
		return nil
	})
	return logs
}

// claudeConfig returns the --mcp-config file the inner command received; the
// capture script copies it because omac removes the sandbox temp dir on exit.
func (r *mcpRig) claudeConfig() map[string]any {
	r.t.Helper()
	raw, err := os.ReadFile(r.argsPath + ".cfg")
	if err != nil {
		r.t.Fatalf("inner command got no --mcp-config: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		r.t.Fatal(err)
	}
	return doc
}

const unregisteredClaudeEntry = `{"mcpServers":{"other":{"command":"x"}}}`

func TestMCPRefusalHappensBeforeAnySpawn(t *testing.T) {
	r := newMCPRig(t)
	r.register("demo", true, "claude-code")
	r.writeFile(".mcp.json", unregisteredClaudeEntry)

	code, stderr := r.launch("claude", r.capture, func(o *launchOpts) { o.mcpUnregistered = "refuse" })
	if code != ExitConfigInvalid {
		t.Fatalf("exit %d, want %d\n%s", code, ExitConfigInvalid, stderr)
	}
	for _, want := range []string{`"other"`, ".mcp.json", "omac mcp register", "--mcp-unregistered=allow", "rotate"} {
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
}

func TestMCPServerRegisteredForAnotherHarnessStillActivatesThePolicy(t *testing.T) {
	r := newMCPRig(t)
	r.register("demo", true, "opencode")
	r.writeFile(".mcp.json", unregisteredClaudeEntry)

	code, stderr := r.launch("claude", r.capture, func(o *launchOpts) { o.mcpUnregistered = "refuse" })
	if code != ExitConfigInvalid {
		t.Fatalf("exit %d, want %d: a harness's own MCP entry must be refused once the project uses the registry\n%s", code, ExitConfigInvalid, stderr)
	}
	if !strings.Contains(stderr, `"other"`) {
		t.Errorf("refusal does not name the entry:\n%s", stderr)
	}
	if _, _, ran := r.captured(); ran {
		t.Error("the inner command ran")
	}
}

func TestMCPUnregisteredFlagOverridesRefusal(t *testing.T) {
	r := newMCPRig(t)
	r.register("demo", true, "claude-code")
	r.writeFile(".mcp.json", unregisteredClaudeEntry)

	code, stderr := r.launch("claude", r.capture, func(o *launchOpts) { o.mcpUnregistered = "warn" })
	if code != ExitOK {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	if !strings.Contains(stderr, `unregistered MCP server "other"`) {
		t.Errorf("no warning:\n%s", stderr)
	}
	if _, _, ran := r.captured(); !ran {
		t.Error("warn mode did not start the inner command")
	}
}

func TestMCPAllowChangesNothing(t *testing.T) {
	r := newMCPRig(t)
	r.register("demo", true, "claude-code")
	r.writeFile(".mcp.json", unregisteredClaudeEntry)

	code, stderr := r.launch("claude", r.capture, func(o *launchOpts) { o.mcpUnregistered = "allow" })
	if code != ExitOK {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	if strings.Contains(stderr, "unregistered") {
		t.Errorf("allow mode printed a warning:\n%s", stderr)
	}
}

const sameNameClaudeEntry = `{"mcpServers":{"demo":{"command":"x"}}}`

func TestMCPSameNameEntryIsRefusedBeforeAnySpawn(t *testing.T) {
	r := newMCPRig(t)
	r.register("demo", true, "claude-code")
	r.writeFile(".mcp.json", sameNameClaudeEntry)

	code, stderr := r.launch("claude", r.capture, func(o *launchOpts) { o.mcpUnregistered = "refuse" })
	if code != ExitConfigInvalid {
		t.Fatalf("exit %d, want %d\n%s", code, ExitConfigInvalid, stderr)
	}
	for _, want := range []string{`"demo"`, ".mcp.json", "also registered with omac"} {
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
}

func TestMCPSameNameEntryWarnsAndAllowIgnores(t *testing.T) {
	for mode, wantWarning := range map[string]bool{"warn": true, "allow": false} {
		t.Run(mode, func(t *testing.T) {
			r := newMCPRig(t)
			r.register("demo", true, "claude-code")
			r.writeFile(".mcp.json", sameNameClaudeEntry)

			code, stderr := r.launch("claude", r.capture, func(o *launchOpts) { o.mcpUnregistered = mode })
			if code != ExitOK {
				t.Fatalf("exit %d\n%s", code, stderr)
			}
			if got := strings.Contains(stderr, "also registered with omac"); got != wantWarning {
				t.Errorf("warning present = %v, want %v:\n%s", got, wantWarning, stderr)
			}
		})
	}
}

func TestMCPUnparseableConfigRefuses(t *testing.T) {
	r := newMCPRig(t)
	r.register("demo", true, "claude-code")
	r.writeFile(".mcp.json", `{ not json`)

	code, stderr := r.launch("claude", r.capture, func(o *launchOpts) { o.mcpUnregistered = "refuse" })
	if code != ExitConfigInvalid || !strings.Contains(stderr, "cannot parse MCP config") {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
}

func TestMCPInactiveWithNothingRegisteredChangesNothingForOpenCode(t *testing.T) {
	r := newMCPRig(t)
	r.writeFile("opencode.json", `{"mcp":{"other":{"type":"local","command":["x"]}}}`)

	code, stderr := r.launch("opencode", r.capture, nil)
	if code != ExitOK {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	if strings.Contains(stderr, "MCP") {
		t.Errorf("inactive MCP handling printed something:\n%s", stderr)
	}
	args, env, _ := r.captured()
	for _, a := range args {
		if strings.Contains(a, "mcp") {
			t.Errorf("unexpected argument %q in %v", a, args)
		}
	}
	if v := env["OPENCODE_CONFIG_CONTENT"]; v != "" {
		t.Errorf("OPENCODE_CONFIG_CONTENT = %q, want unset", v)
	}
	if logs := r.gatewayLogs(); len(logs) != 0 {
		t.Errorf("a gateway started: %v", logs)
	}
}

func TestMCPClaudeStartsStrictWhenNothingIsRegistered(t *testing.T) {
	r := newMCPRig(t)

	code, stderr := r.launch("claude", r.capture, nil)
	if code != ExitOK {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	if strings.Contains(stderr, "strict MCP mode") {
		t.Errorf("notice printed although nothing is configured:\n%s", stderr)
	}
	args, _, _ := r.captured()
	if !reflect.DeepEqual(args, []string{"--strict-mcp-config", "--no-chrome"}) {
		t.Errorf("args = %v, want strict mode and --no-chrome", args)
	}
	if logs := r.gatewayLogs(); len(logs) != 0 {
		t.Errorf("a gateway started: %v", logs)
	}
}

func TestMCPClaudeStrictModeNoticeNamesEntriesNotValues(t *testing.T) {
	r := newMCPRig(t)
	r.writeFile(".mcp.json", `{"mcpServers":{"other":{"command":"x","args":["--token","s3cret-arg"],"env":{"K":"s3cret-env"}}}}`)

	code, stderr := r.launch("claude", r.capture, nil)
	if code != ExitOK {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	const want = `omac start: Claude Code runs in strict MCP mode, so these MCP servers from its own configuration are ignored and will not load inside omac: other. Register them with "omac mcp add" or run with --mcp-unregistered=allow to keep them.`
	if strings.Count(stderr, want) != 1 {
		t.Errorf("want the notice once:\n%s", stderr)
	}
	for _, secret := range []string{"s3cret-arg", "s3cret-env", `"x"`} {
		if strings.Contains(stderr, secret) {
			t.Errorf("notice leaks %s:\n%s", secret, stderr)
		}
	}
}

func TestMCPClaudeStrictModeNoticeLimitsNames(t *testing.T) {
	r := newMCPRig(t)
	var entries []string
	for i := 0; i < 12; i++ {
		entries = append(entries, fmt.Sprintf(`"srv%02d":{"command":"x"}`, i))
	}
	r.writeFile(".mcp.json", `{"mcpServers":{`+strings.Join(entries, ",")+`}}`)

	code, stderr := r.launch("claude", r.capture, nil)
	if code != ExitOK {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "srv09 and 2 more.") || strings.Contains(stderr, "srv10") {
		t.Errorf("names not limited to 10:\n%s", stderr)
	}
}

func TestMCPClaudeStrictModeNoticeReportsUnparseableConfig(t *testing.T) {
	r := newMCPRig(t)
	r.writeFile(".mcp.json", `{not json`)

	code, stderr := r.launch("claude", r.capture, nil)
	if code != ExitOK {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "strict MCP mode") || !strings.Contains(stderr, ".mcp.json") {
		t.Errorf("no notice for the unparseable file:\n%s", stderr)
	}
}

func TestMCPClaudeDoesNotDuplicateUserStrictFlag(t *testing.T) {
	r := newMCPRig(t)

	code, stderr := r.launch("claude", r.capture, func(o *launchOpts) { o.innerArgs = []string{"--strict-mcp-config"} })
	if code != ExitOK {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	args, _, _ := r.captured()
	if !reflect.DeepEqual(args, []string{"--no-chrome", "--strict-mcp-config"}) {
		t.Errorf("args = %v, want the user's flag once", args)
	}
}

func TestMCPClaudeUserMCPConfigStillLoadsWhenNothingIsRegistered(t *testing.T) {
	r := newMCPRig(t)
	r.writeFile("own.json", `{"mcpServers":{}}`)
	own := "--mcp-config=" + filepath.Join(r.wd, "own.json")

	code, stderr := r.launch("claude", r.capture, func(o *launchOpts) { o.innerArgs = []string{own} })
	if code != ExitOK {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	args, _, _ := r.captured()
	if want := []string{"--strict-mcp-config", "--no-chrome", own}; !reflect.DeepEqual(args, want) {
		t.Errorf("args = %v, want %v", args, want)
	}
}

func TestMCPClaudeWarnAndAllowPassNoMCPFlags(t *testing.T) {
	for _, mode := range []string{"warn", "allow"} {
		t.Run(mode, func(t *testing.T) {
			r := newMCPRig(t)
			code, stderr := r.launch("claude", r.capture, func(o *launchOpts) { o.mcpUnregistered = mode })
			if code != ExitOK {
				t.Fatalf("exit %d\n%s", code, stderr)
			}
			if args, _, _ := r.captured(); len(args) != 0 {
				t.Errorf("args = %v, want none", args)
			}
		})
	}
}

func TestMCPNonInjectableHarnessGetsNoStrictFlag(t *testing.T) {
	r := newMCPRig(t)
	code, stderr := r.launch("codex", r.capture, nil)
	if code != ExitOK {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	if args, _, _ := r.captured(); containsString(args, "--strict-mcp-config") {
		t.Errorf("args = %v", args)
	}
}

func TestMCPClaudeInjectionReachesInnerArgsAndEnv(t *testing.T) {
	r := newMCPRig(t)
	r.register("demo", true, "claude-code")

	code, stderr := r.launch("claude", r.capture, func(o *launchOpts) { o.innerArgs = []string{"--user-arg"} })
	if code != ExitOK {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	args, env, _ := r.captured()

	cfg := r.claudeConfig()
	servers, _ := cfg["mcpServers"].(map[string]any)
	demo, _ := servers["demo"].(map[string]any)
	if demo == nil {
		t.Fatalf("demo missing from %v", cfg)
	}
	bridge := testBridgeArgv(t, "demo")
	if demo["command"] != bridge[0] {
		t.Errorf("command = %v, want %s", demo["command"], bridge[0])
	}
	entryEnv, _ := demo["env"].(map[string]any)
	if entryEnv["OMAC_FACADE_TOKEN"] != env["OMAC_FACADE_TOKEN"] || entryEnv["OMAC_FACADE_TOKEN"] == "" {
		t.Errorf("entry token %v does not match the launch token %q", entryEnv["OMAC_FACADE_TOKEN"], env["OMAC_FACADE_TOKEN"])
	}
	if entryEnv["OMAC_BASE"] != env["OMAC_BASE"] || entryEnv["OMAC_SOCKET"] != env["OMAC_SOCKET"] {
		t.Errorf("entry env %v does not match the launch env", entryEnv)
	}

	mcpIdx, userIdx := -1, -1
	for i, a := range args {
		if strings.HasPrefix(a, "--mcp-config=") {
			mcpIdx = i
		}
		if a == "--user-arg" {
			userIdx = i
		}
	}
	if mcpIdx < 0 || userIdx < 0 || mcpIdx > userIdx {
		t.Errorf("--mcp-config must precede user args: %v", args)
	}
	if args[0] != "--strict-mcp-config" {
		t.Errorf("registered servers must come with strict mode: %v", args)
	}
	if _, ok := env["ENABLE_CLAUDEAI_MCP_SERVERS"]; ok && os.Getenv("ENABLE_CLAUDEAI_MCP_SERVERS") == "" {
		t.Error("connectors variable must not be set")
	}
	for k, v := range env {
		if strings.Contains(v, "tok-from-keychain") {
			t.Errorf("secret leaked into inner env %s", k)
		}
	}
}

func TestMCPOpenCodeInjectionReachesEnv(t *testing.T) {
	r := newMCPRig(t)
	r.register("demo", true, "opencode")

	code, stderr := r.launch("opencode", r.capture, nil)
	if code != ExitOK {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	_, env, _ := r.captured()
	var cfg struct {
		MCP map[string]struct {
			Command []string `json:"command"`
			Enabled bool     `json:"enabled"`
		} `json:"mcp"`
	}
	if err := json.Unmarshal([]byte(env["OPENCODE_CONFIG_CONTENT"]), &cfg); err != nil {
		t.Fatalf("OPENCODE_CONFIG_CONTENT: %v (%q)", err, env["OPENCODE_CONFIG_CONTENT"])
	}
	demo := cfg.MCP["demo"]
	if !demo.Enabled || strings.Join(demo.Command, " ") != strings.Join(testBridgeArgv(t, "demo"), " ") {
		t.Errorf("demo entry = %+v", demo)
	}
	if strings.Contains(env["OPENCODE_CONFIG_CONTENT"], "tok-from-keychain") {
		t.Error("secret in the injected config")
	}
}

func TestMCPMissingSecretSkipsServerWithWarning(t *testing.T) {
	r := newMCPRig(t)
	r.register("demo", false, "claude-code")

	code, stderr := r.launch("claude", r.capture, nil)
	if code != ExitOK {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	if !strings.Contains(stderr, `MCP server "demo" skipped`) || !strings.Contains(stderr, "FAKE_TOKEN") {
		t.Errorf("no skip warning:\n%s", stderr)
	}
	if logs := r.gatewayLogs(); len(logs) != 0 {
		t.Errorf("a gateway started for a server without its secret: %v", logs)
	}
	if _, err := os.Stat(r.argsPath + ".cfg"); err == nil {
		t.Error("a skipped server left an --mcp-config injection")
	}
}

func TestMCPAllowEnvOnlyWhenInjecting(t *testing.T) {
	native := sandboxPlan{Native: true}
	argv := []string{"omac", "sandbox", "run", "--", "inner"}

	var none *mcpPlan
	if got := none.allowEnv(argv, native); strings.Contains(strings.Join(got, " "), "--allow-env") {
		t.Errorf("nil plan added allow-env: %v", got)
	}
	empty := &mcpPlan{}
	if got := empty.allowEnv(argv, native); strings.Contains(strings.Join(got, " "), "--allow-env") {
		t.Errorf("empty injection added allow-env: %v", got)
	}

	injecting := &mcpPlan{}
	injecting.injection.AllowEnv = []string{"OPENCODE_CONFIG_CONTENT"}
	got := strings.Join(injecting.allowEnv(argv, native), " ")
	if !strings.Contains(got, "--allow-env OPENCODE_CONFIG_CONTENT") {
		t.Errorf("injection not allowed: %s", got)
	}
	if got := strings.Join(injecting.allowEnv(argv, sandboxPlan{}), " "); strings.Contains(got, "--allow-env") {
		t.Errorf("non-native backend got allow-env: %s", got)
	}
}

func TestMCPUserExportedConfigIsNotForwardedWithoutInjection(t *testing.T) {
	for _, name := range []string{"opencode", "claude"} {
		harness, _ := config.LookupHarness(name)
		for _, v := range append(harness.ForwardedEnvVars(), harnessEnvAllowVars(harness, sandboxPlan{})...) {
			if v == "OPENCODE_CONFIG_CONTENT" {
				t.Errorf("harness %s forwards %s on its own", name, v)
			}
		}
	}
}

func TestMCPDenyVarsConflictFailsAndNonNativeOnlyWarns(t *testing.T) {
	p := &mcpPlan{}
	p.injection.AllowEnv = []string{"OPENCODE_CONFIG_CONTENT"}
	denying := sandboxPlan{Native: true, Name: "builtin", Policy: &sandboxprofile.Profile{
		Environment: sandboxprofile.Environment{DenyVars: []string{"OPENCODE_*"}},
	}}
	env, stderr := launchTestEnv(t, t.TempDir())

	err := p.checkSandboxEnv(env, "omac start", denying, false)
	if err == nil || !strings.Contains(err.Error(), "OPENCODE_CONFIG_CONTENT") || !strings.Contains(err.Error(), "deny_vars") {
		t.Fatalf("err = %v", err)
	}
	if err := p.checkSandboxEnv(env, "omac start", sandboxPlan{Native: true, Policy: &sandboxprofile.Profile{}}, false); err != nil {
		t.Errorf("no conflict, got %v", err)
	}
	if err := p.checkSandboxEnv(env, "omac start", denying, true); err != nil {
		t.Errorf("--no-sandbox must not check, got %v", err)
	}
	if err := p.checkSandboxEnv(env, "omac start", sandboxPlan{Name: "nono"}, false); err != nil {
		t.Errorf("non-native backend must only warn, got %v", err)
	}
	if !strings.Contains(stderr(), "cannot verify") {
		t.Errorf("no warning for the non-native backend:\n%s", stderr())
	}
}

func TestMCPUnregisteredFlagValidation(t *testing.T) {
	env, stderr := launchTestEnv(t, t.TempDir())
	if _, code := parseLaunchArgs("start", []string{"--mcp-unregistered=bogus"}, env); code != ExitMisuse {
		t.Errorf("invalid value: exit %d, want %d (%s)", code, ExitMisuse, stderr())
	}
	for _, cmd := range []string{"start", "continue", "resume"} {
		for _, mode := range []string{"allow", "warn", "ignore", "refuse"} {
			opts, code := parseLaunchArgs(cmd, []string{"--mcp-unregistered=" + mode}, env)
			if code != ExitOK || opts.mcpUnregistered != mode {
				t.Errorf("%s: %s: code=%d value=%q", cmd, mode, code, opts.mcpUnregistered)
			}
		}
	}
}

func TestMCPServeNotice(t *testing.T) {
	r := newMCPRig(t)
	harness, _ := config.LookupHarness("opencode")
	env, stderr := launchTestEnv(t, r.wd)
	mcpServeNotice(env, harness)
	if stderr() != "" {
		t.Fatalf("notice without registrations: %q", stderr())
	}
	r.register("demo", true, "opencode")
	mcpServeNotice(env, harness)
	if !strings.Contains(stderr(), "omac serve") || !strings.Contains(stderr(), "MCP") {
		t.Errorf("notice = %q", stderr())
	}
}

// TestMCPEndToEndBridgeRoundTrip starts the real gateway for a registered fake
// stdio server, then lets the inner command talk to it through the real bridge
// over the real facade. It does not use a real harness: the inner command
// spawns the bridge itself instead of the harness reading the injected entry.
func TestMCPEndToEndBridgeRoundTrip(t *testing.T) {
	r := newMCPRig(t)
	r.register("demo", true, "claude-code")
	out := filepath.Join(t.TempDir(), "bridge-out")
	bridge := testBridgeArgv(t, "demo")
	script := fmt.Sprintf(`#!/bin/sh
env > "$OMAC_TEST_ENV"
printf '%%s\n' \
  '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}' \
  '{"jsonrpc":"2.0","method":"notifications/initialized"}' \
  '{"jsonrpc":"2.0","id":2,"method":"tools/list"}' \
  | '%s' %s > '%s'
`, bridge[0], strings.Join(bridge[1:], " "), out)
	inner := r.writeInner(script)

	code, stderr := r.launch("claude", inner, nil)
	if code != ExitOK {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("no bridge output: %v\n%s", err, stderr)
	}
	got := string(raw)
	if !strings.Contains(got, `"protocolVersion":"2025-06-18"`) {
		t.Errorf("no initialize result:\n%s", got)
	}
	if !strings.Contains(got, `"name":"whoami"`) || !strings.Contains(got, "tok-from-keychain") {
		t.Errorf("tools/list did not reach the server with its keychain secret:\n%s", got)
	}
	_, env, _ := r.captured()
	for k, v := range env {
		if strings.Contains(v, "tok-from-keychain") {
			t.Errorf("secret leaked into inner env %s", k)
		}
	}
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func TestMCPClaudeDefaultIgnoresHarnessEntriesAndStartsStrict(t *testing.T) {
	r := newMCPRig(t)
	r.register("demo", true, "claude-code")
	r.writeFile(".mcp.json", `{"mcpServers":{"other":{"command":"x"},"far":{"type":"http","url":"https://example.invalid/mcp"}}}`)

	code, stderr := r.launch("claude", r.capture, nil)
	if code != ExitOK {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	for _, want := range []string{"notice:", `"other"`, `"far"`, "ignored by strict mode", "remote MCP servers are not supported yet"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("notice lacks %q:\n%s", want, stderr)
		}
	}
	for _, bad := range []string{"warning:", "rotate"} {
		if strings.Contains(stderr, bad) {
			t.Errorf("ignore notice contains %q:\n%s", bad, stderr)
		}
	}
	args, _, ran := r.captured()
	if !ran || len(args) == 0 || args[0] != "--strict-mcp-config" {
		t.Errorf("args = %v, want --strict-mcp-config first", args)
	}
	r.claudeConfig()
}

func TestMCPIgnoreReportsUserMCPConfigAsLoaded(t *testing.T) {
	r := newMCPRig(t)
	r.register("demo", true, "claude-code")
	r.writeFile("extra.json", `{"mcpServers":{"mine":{"command":"x"}}}`)

	code, stderr := r.launch("claude", r.capture, func(o *launchOpts) { o.innerArgs = []string{"--mcp-config", filepath.Join(r.wd, "extra.json")} })
	if code != ExitOK {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "warning: unregistered MCP server \"mine\"") || strings.Contains(stderr, "notice:") {
		t.Errorf("an entry the user passes must be reported as loaded, not ignored:\n%s", stderr)
	}
}

func TestMCPIgnoreFallsBackToRefuseForOpenCode(t *testing.T) {
	r := newMCPRig(t)
	r.register("demo", true, "opencode")
	r.writeFile("opencode.json", `{"mcp":{"other":{"type":"remote","url":"https://example.invalid/mcp"}}}`)

	code, stderr := r.launch("opencode", r.capture, func(o *launchOpts) { o.mcpUnregistered = "ignore" })
	if code != ExitConfigInvalid {
		t.Fatalf("exit %d, want %d\n%s", code, ExitConfigInvalid, stderr)
	}
	for _, want := range []string{"ignore is not available for opencode", "using refuse", `"other"`, "Remote MCP servers are not supported yet"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("output lacks %q:\n%s", want, stderr)
		}
	}
	if strings.Contains(stderr, "omac mcp register") {
		t.Errorf("a remote entry must not be told to register:\n%s", stderr)
	}
}

func TestMCPOpenCodeDefaultIsRefuse(t *testing.T) {
	r := newMCPRig(t)
	r.register("demo", true, "opencode")
	r.writeFile("opencode.json", `{"mcp":{"other":{"type":"local","command":["x"]}}}`)

	code, stderr := r.launch("opencode", r.capture, nil)
	if code != ExitConfigInvalid {
		t.Fatalf("exit %d, want %d\n%s", code, ExitConfigInvalid, stderr)
	}
	if strings.Contains(stderr, "not available") {
		t.Errorf("the fallback message belongs to an explicit ignore only:\n%s", stderr)
	}
}

func TestMCPClaudeKeepsUserChromeChoice(t *testing.T) {
	for _, flag := range []string{"--chrome", "--no-chrome"} {
		r := newMCPRig(t)
		code, stderr := r.launch("claude", r.capture, func(o *launchOpts) { o.innerArgs = []string{flag} })
		if code != ExitOK {
			t.Fatalf("%s: exit %d\n%s", flag, code, stderr)
		}
		args, _, _ := r.captured()
		n := 0
		for _, a := range args {
			if a == "--chrome" || a == "--no-chrome" {
				n++
			}
		}
		if n != 1 {
			t.Errorf("%s: args = %v, want the user's chrome flag once and none added", flag, args)
		}
	}
}

// The audit redactor is built before the gateways start, so the plan loads
// the MCP secrets first and hands their values over; a missing secret is
// reported once.
func TestMCPSecretsReachTheAuditRedactorAndWarnOnce(t *testing.T) {
	r := newMCPRig(t)
	r.register("demo", true, "claude-code")
	r.register("nosecret", false, "claude-code")
	entries, _, err := mcpreg.Effective(r.wd, "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	p := &mcpPlan{entries: entries}
	env, stderr := launchTestEnv(t, r.wd)
	p.loadAllSecrets(env, "omac start")
	if got := p.secretValues(); len(got) != 1 || got[0] != "tok-from-keychain" {
		t.Fatalf("secret values %v", got)
	}
	if _, ok := p.secrets["nosecret"]; ok {
		t.Error("an entry without its secret must be skipped")
	}
	if n := strings.Count(stderr(), `MCP server "nosecret" skipped`); n != 1 {
		t.Errorf("skip warning printed %d times", n)
	}
	var nilPlan *mcpPlan
	nilPlan.loadAllSecrets(env, "omac start")
	if nilPlan.secretValues() != nil {
		t.Error("a nil plan has no secrets")
	}
}

func TestMCPMissingSecretWarnsOnceInALaunch(t *testing.T) {
	r := newMCPRig(t)
	r.register("demo", false, "claude-code")
	if code, stderr := r.launch("claude", r.capture, nil); code != ExitOK || strings.Count(stderr, `MCP server "demo" skipped`) != 1 {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
}

func testBridgeArgv(t *testing.T, name string) []string {
	t.Helper()
	exe, err := executablePath()
	if err != nil {
		t.Fatal(err)
	}
	return bridgeArgv(exe, name)
}
