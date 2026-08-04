//go:build e2e

// Wire-level guard for the sandbox briefing's system-prompt shape.
//
// omac injects an always-on sandbox briefing into every agent's system
// prompt. For OpenCode the delivery path is an env var plus a plugin:
// `omac start` sets OMAC_SANDBOX_BRIEFING (cli/start.go:944) and the omac
// plugin's experimental.chat.system.transform hook folds it into
// OpenCode's system prompt (plugin/assets/omac-multidir.ts).
//
// The hook receives `output.system []string`, and OpenCode maps EVERY
// entry of that slice to its own {role:"system"} message. So a hook that
// pushes a new entry turns OpenCode's single system message into two —
// and strict OpenAI-compatible servers reject a system message at index
// > 0 ("system message must come first"; Qwen chat templates in
// particular). The hook must therefore merge into an existing block, and
// this test enforces that end to end.
//
// Why a stub model server: the assembled system prompt is never
// persisted anywhere, so there is nothing to read back. OpenCode's own
// session model has no system role at all —
//
//	type Message = UserMessage | AssistantMessage
//
// (@opencode-ai/sdk types.gen.d.ts) — so neither GET /session/{id}/message
// nor any other serve route can show it, and opencode.log does not record
// request bodies. The prompt is built per-request inside the provider
// call and immediately serialized to HTTP. The outbound request body is
// therefore the only place {role:"system"} messages exist as such, which
// makes it the only place the question "how many are there?" has a
// literal answer.
//
// Nothing about OpenCode or omac is stubbed. The sandbox, the env filter,
// the plugin provisioning, OpenCode's plugin discovery, its prompt
// assembly and its system→messages mapping are all real; only the model
// at the far end is ours. Faking it is forced anyway: a real model costs
// money, is nondeterministic, and would never report what it received.
//
// No model credentials required — the stub replaces the provider.
//
// Run:  go test -tags=e2e -run TestOpenCodeSingleSystemMessage -v ./internal/e2e/
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tngtech/oh-my-agentic-coder/internal/plugin"
	"github.com/tngtech/oh-my-agentic-coder/internal/sandboxbrief"
)

// briefingRunTimeout bounds the whole agent run. Generous because
// OpenCode installs its plugin dependency (@opencode-ai/plugin) on first
// launch when any plugin is present, and blocks prompt assembly until
// that finishes.
const briefingRunTimeout = 3 * time.Minute

// stubModelID is the model id declared in the fixture opencode.json and
// selected via `-m model/<id>`. The provider key is "model" (see
// writeStubProviderConfig), matching opencodeConfig().RunArgs.
const stubModelID = "stub"

// briefingAnchor is a phrase from internal/sandboxbrief/brief.md used to
// confirm the merged block really is the briefing rather than some other
// system text. Deliberately short and lowercase-insensitive-free: the
// test asserts the briefing ARRIVED, while the system-message count is
// the actual invariant.
const briefingAnchor = "omac sandbox"

// manifestAnchor is the skills-manifest heading the plugin renders
// (renderManifest in the plugin source). The manifest is appended AFTER the
// briefing, so when both are present their relative order is an invariant.
// Only checked when the manifest is actually present: it requires at least
// one activated skill in the workdir, which this fixture does not
// guarantee.
const manifestAnchor = "## omac skills available in this workspace"

// TestOpenCodeSingleSystemMessage runs the real OpenCode under the real
// omac sandbox with the real omac plugin, pointed at a stub model
// endpoint, and asserts every outbound model request carries exactly one
// {role:"system"} message — and that the briefing is in it.
//
// Pre-fix (a hook that pushed a new system block) this reports 2:
// OpenCode's own collapse only triggers above 2 entries, so a single
// push is never normalized away.
func TestOpenCodeSingleSystemMessage(t *testing.T) {
	h := opencodeConfig()

	// The briefing is injected only when a real sandbox wraps the inner
	// command (cli/briefing.go:22). A nested run forces --no-sandbox
	// (fixtures.go:114), which would suppress the briefing and make this
	// test assert nothing.
	if forceNoSandbox(h) {
		t.Skip("sandbox briefing requires a real sandbox (cli/briefing.go:22); " +
			"nested runs force --no-sandbox (fixtures.go:114)")
	}
	skipIfSandboxUnavailable(t)

	home := t.TempDir()
	workdir := t.TempDir()
	// Runtime dirs OpenCode expects to exist; the sandbox skips
	// nonexistent allow paths, so create them before launch (mirrors
	// smoke_test.go).
	for _, dir := range []string{
		".cache", ".cache/opencode",
		".local/share/opencode", ".local/state/opencode/locks",
	} {
		if err := os.MkdirAll(filepath.Join(home, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// --- stub model endpoint -------------------------------------------
	// Records every chat/completions request body. OpenCode forks
	// concurrent title- and summary-generation calls alongside the main
	// turn, so more than one request is expected and the handler must be
	// concurrency-safe.
	var mu sync.Mutex
	var bodies [][]byte
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/chat/completions") {
			body, err := io.ReadAll(r.Body)
			if err == nil {
				mu.Lock()
				bodies = append(bodies, body)
				mu.Unlock()
			}
		}
		writeStubCompletion(w)
	}))
	t.Cleanup(stub.Close)

	addr, ok := stub.Listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("stub listener is not TCP: %T", stub.Listener.Addr())
	}
	t.Logf("stub model endpoint at %s (loopback port %d)", stub.URL, addr.Port)

	// --- real omac + real opencode -------------------------------------
	omacBin := buildOmac(t)
	installHarness(t, h, home)
	version := harnessVersion(t, h, home)
	writeStubProviderConfig(t, home, stub.URL)

	// Provision the plugin the way `omac start` does (cli/setup.go:142).
	// omac would do this itself at launch; doing it here first makes the
	// dependency explicit and surfaces a provisioning failure as a clear
	// error rather than a missing briefing 3 minutes later.
	pluginDir := filepath.Join(home, ".config", "opencode", "plugins")
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := plugin.InstallMultiDirIn(pluginDir, false)
	if err != nil {
		t.Fatalf("provision omac plugin into %s: %v", pluginDir, err)
	}
	t.Logf("omac plugin provisioned at %s (unchanged=%v)", res.Path, res.Unchanged)

	// Filtered network (the production default) plus the single loopback
	// grant the stub needs. The child dials the stub directly because
	// netproxy injects NO_PROXY for loopback and refuses loopback
	// destinations itself; open_port is what permits the connect.
	writeSandboxProfile(t, home, h, &AllowanceSpec{OpenPorts: []int{addr.Port}})

	// --- run, sandboxed ------------------------------------------------
	// No --no-sandbox: that would suppress the briefing entirely.
	const prompt = "Reply with the single word: ok"
	args := []string{"start", h.Name, "--",
		"run", "--print-logs", "-m", "model/" + stubModelID, prompt}

	ctx, cancel := context.WithTimeout(context.Background(), briefingRunTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, omacBin, args...)
	cmd.Dir = workdir
	env := append(withHome(os.Environ(), home), "PWD="+workdir)
	if shortTmp := shortTmpDirForNested(t); shortTmp != "" {
		env = withEnv(env, "TMPDIR", shortTmp)
	}
	cmd.Env = env
	cmd.Stdin = strings.NewReader("")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	t.Logf("running: omac %s", strings.Join(args, " "))
	// The exit code is deliberately not asserted. The stub is not a real
	// model, so OpenCode may well error after the request — but the
	// request has already been captured by then, and that is the subject
	// of this test.
	runErr := cmd.Run()

	profilePath := filepath.Join(home, ".config", "omac", "sandbox-profiles", "default.json")
	writeSessionArtifacts(t, h, "plugin-briefing", home, workdir, prompt,
		stdout.String(), stderr.String(), cmd.Env, profilePath)

	mu.Lock()
	captured := append([][]byte{}, bodies...)
	mu.Unlock()

	result := "PASS"
	if len(captured) == 0 {
		result = "FAIL"
	}
	emitCompat(compatLine(h.Name, version, runtime.GOOS, "plugin-system", result))

	if len(captured) == 0 {
		t.Fatalf("stub received no chat/completions request (omac exit: %v)\n"+
			"STDOUT:\n%s\nSTDERR:\n%s",
			runErr, tailLines(stdout.String(), 40), tailLines(stderr.String(), 80))
	}
	t.Logf("captured %d chat/completions request(s)", len(captured))

	// --- assertions ----------------------------------------------------
	// Two independent invariants, checked separately so each failure means
	// exactly what it says: the briefing must ARRIVE, and it must arrive
	// merged into a single system block. Deliberately not short-circuited
	// on the count — the pre-fix bug delivers the briefing correctly in a
	// second block, and reporting that as "briefing missing" would send a
	// debugger down the wrong path.
	//
	// Every request is checked, not merely the first: OpenCode's forked
	// title/summary calls go through the same prompt assembly, and "first"
	// would be a race between concurrent requests.
	sawBriefing := false
	for i, body := range captured {
		systems, err := systemMessages(body)
		if err != nil {
			t.Errorf("request %d: %v", i, err)
			continue
		}
		for _, s := range systems {
			if !strings.Contains(s, briefingAnchor) {
				continue
			}
			sawBriefing = true
			assertBriefingIntact(t, i, s)
		}
		if len(systems) != 1 {
			t.Errorf("request %d: got %d system messages, want exactly 1.\n"+
				"The omac plugin must MERGE the briefing into the last existing "+
				"system block, not push a new one: OpenCode maps every entry of "+
				"output.system to its own {role:\"system\"} message, and strict "+
				"OpenAI-compatible servers (Qwen chat templates) reject a system "+
				"message at index > 0 with \"system message must come first\".\n"+
				"system blocks:\n%s",
				i, len(systems), describeBlocks(systems))
		}
	}

	if !sawBriefing {
		t.Errorf("no captured request carried the sandbox briefing (anchor %q).\n"+
			"Either the plugin hook did not run (not discovered from %s) or "+
			"OMAC_SANDBOX_BRIEFING did not survive the sandbox env filter.\n"+
			"STDERR:\n%s",
			briefingAnchor, pluginDir, tailLines(stderr.String(), 60))
	}
}

// assertBriefingIntact checks that the system block carrying the briefing
// carries ALL of it, verbatim, and that the skills manifest — when present —
// follows it rather than being spliced into the middle. Counting system
// messages proves omac did not add a block; this proves what it merged into
// the surviving block is the whole briefing and not a truncated,
// re-ordered, or interleaved version.
//
// The expectation is sandboxbrief.Default() itself rather than a list of
// anchor phrases: briefingInjection appends the cache guidance to the
// resolved briefing and the plugin concatenates the result unmodified
// (cli/briefing.go:28-34), so the default text is an exact substring of
// what lands on the wire. Comparing against the source of truth keeps this
// from drifting when brief.md is edited, and rejects text injected between
// its sections — which per-section anchors would allow. It does assume the
// fixture leaves config.yaml's sandbox.briefing override unset.
func assertBriefingIntact(t *testing.T, reqIdx int, block string) {
	t.Helper()

	if want := sandboxbrief.Default(); !strings.Contains(block, want) {
		t.Errorf("request %d: system block carries the briefing anchor %q but "+
			"not the briefing verbatim — it arrived truncated, reordered, or "+
			"with text spliced into it. Want (%d bytes) the exact contents of "+
			"internal/sandboxbrief/brief.md.\nBlock:\n%s",
			reqIdx, briefingAnchor, len(want), describeBlocks([]string{block}))
		return
	}

	// The plugin appends [briefing, manifest] in that order, so a manifest
	// ahead of the briefing means the two were spliced, not concatenated.
	if at := strings.Index(block, manifestAnchor); at >= 0 {
		if brief := strings.Index(block, briefingAnchor); at < brief {
			t.Errorf("request %d: skills manifest at offset %d precedes the "+
				"briefing at offset %d; the plugin appends the briefing first. "+
				"Block:\n%s",
				reqIdx, at, brief, describeBlocks([]string{block}))
		}
	}
}

// systemMessages returns the content of every {role:"system"} message in
// an OpenAI-compatible chat/completions request body.
func systemMessages(body []byte) ([]string, error) {
	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("parse request body: %w (body: %s)", err, truncate(string(body), 200))
	}
	if len(req.Messages) == 0 {
		return nil, fmt.Errorf("request has no messages (body: %s)", truncate(string(body), 200))
	}
	var out []string
	for _, m := range req.Messages {
		if m.Role != "system" {
			continue
		}
		// Content is a string for system messages, but tolerate the
		// array-of-parts form rather than failing the whole assertion.
		var s string
		if err := json.Unmarshal(m.Content, &s); err != nil {
			s = string(m.Content)
		}
		out = append(out, s)
	}
	return out, nil
}

// describeBlocks renders system blocks for a failure message: index,
// length and a short head, so a count mismatch shows WHICH block is the
// unexpected one without dumping a full system prompt.
func describeBlocks(blocks []string) string {
	var b strings.Builder
	for i, s := range blocks {
		fmt.Fprintf(&b, "  [%d] %d bytes: %s\n", i, len(s), truncate(oneLine(s), 160))
	}
	return b.String()
}

// oneLine collapses newlines so a block preview stays on one line.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// writeStubCompletion emits a minimal OpenAI-compatible streaming
// response: one content chunk, one terminal chunk with finish_reason and
// usage, then [DONE]. Enough for the client to complete a turn; the test
// does not depend on OpenCode being satisfied by it.
func writeStubCompletion(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	chunks := []string{
		`{"id":"omac-e2e","object":"chat.completion.chunk","created":0,` +
			`"model":"` + stubModelID + `","choices":[{"index":0,` +
			`"delta":{"role":"assistant","content":"ok"},"finish_reason":null}]}`,
		`{"id":"omac-e2e","object":"chat.completion.chunk","created":0,` +
			`"model":"` + stubModelID + `","choices":[{"index":0,` +
			`"delta":{},"finish_reason":"stop"}],` +
			`"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
		"[DONE]",
	}
	for _, c := range chunks {
		fmt.Fprintf(w, "data: %s\n\n", c)
		if flusher != nil {
			flusher.Flush()
		}
	}
}

// writeStubProviderConfig writes the OpenCode provider fixture pointing
// at the stub endpoint: auth.json for the (unused but required) API key
// and opencode.json declaring an @ai-sdk/openai-compatible provider
// keyed "model" with a single model id.
//
// Mirrors opencodeConfig().ProviderSetup (harnesses.go) but takes the
// base URL as an argument instead of reading SKAINET_INTERNAL, which is
// what makes this test credential-free.
func writeStubProviderConfig(t *testing.T, home, baseURL string) {
	t.Helper()

	authDir := filepath.Join(home, ".local", "share", "opencode")
	if err := os.MkdirAll(authDir, 0o755); err != nil {
		t.Fatal(err)
	}
	auth := map[string]map[string]string{
		"model": {"type": "api", "key": "omac-e2e-stub-key"},
	}
	authBytes, err := json.Marshal(auth)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(authDir, "auth.json"), authBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	cfgDir := filepath.Join(home, ".config", "opencode")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	opencodeJSON := map[string]any{
		"share": "disabled",
		"provider": map[string]any{
			"model": map[string]any{
				"name":    "omac e2e stub",
				"npm":     "@ai-sdk/openai-compatible",
				"options": map[string]any{"baseURL": baseURL},
				"models": map[string]any{
					stubModelID: map[string]any{
						"name":  "omac e2e stub",
						"limit": map[string]any{"context": 32768, "output": 4096},
					},
				},
			},
		},
	}
	cfgBytes, err := json.Marshal(opencodeJSON)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "opencode.json"), cfgBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("stub provider config written to %s (baseURL=%s)", cfgDir, baseURL)
}
