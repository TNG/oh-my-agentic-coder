//go:build e2e

// Smoke tier: model-free harness-version compatibility checks.
//
// These sit between the model-free/harness-free PR gate (e2e_fast) and the
// full agent-driven suite (TestE2EEchoRest / TestE2ESecurityAudit). They
// install the REAL harness binary — pinned or, under E2E_USE_LATEST, the
// latest release — and verify the assumptions omac makes about it WITHOUT a
// model round-trip:
//
//   - TestHarnessCLIContract asserts every CLI flag/subcommand omac derives
//     from its registry still exists in the harness's own --help, so a release
//     that renames or moves a flag fails fast with a precise signal instead of
//     surfacing a week later as an inscrutable agent failure.
//   - TestHarnessLaunchProbe drives `omac start <harness> -- --version` through
//     the real sandbox, exercising omac's launch/PATH/config-home/sandbox-
//     admission wiring end-to-end (everything except the model call).
//   - TestHarnessServeProbe launches `omac serve <harness>` with the REAL inner
//     server daemon under the sandbox and drives the host-side control plane
//     over HTTP (activate a directory, read its manifest, list dirs,
//     deactivate) — the Desktop-mode analog of the launch probe. It runs only
//     for harnesses with a server-launch convention (opencode, for OpenCode
//     Desktop); the rest run their inner command as-is under serve and emit a
//     SKIP row.
//
// All three are selectable without secrets (no SKAINET/Anthropic token needed):
//
//	go test -tags=e2e -run 'TestHarnessCLIContract|TestHarnessLaunchProbe|TestHarnessServeProbe' ./internal/e2e/
//
// The weekly "E2E: drift" workflow runs them with E2E_USE_LATEST=1 across every
// harness and OS (alongside the llm stage), never as a separate per-PR job.
//
// Each test prints one machine-readable OMAC_COMPAT line per harness/stage
// (see compatLine); the workflow parses those into the persisted compatibility
// matrix.

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// smokeCmdTimeout bounds a single harness --version/--help invocation.
const smokeCmdTimeout = 60 * time.Second

// launchProbeTimeout bounds the `omac start ... -- --version` launch probe.
// Generous vs. a bare --version because it spins up the sandbox first.
const launchProbeTimeout = 2 * time.Minute

// serveProbeTimeout bounds the whole `omac serve` probe: sandbox spin-up, inner
// daemon launch, and the control-plane activate/dirs/deactivate round-trip.
const serveProbeTimeout = 3 * time.Minute

// serveReadyTimeout bounds how long the probe waits for the control plane to
// announce itself on stdout after `omac serve` starts.
const serveReadyTimeout = 90 * time.Second

// serveControlBaseRe extracts the control-plane base URL from the line
// `omac serve` prints on startup: "control plane on http://127.0.0.1:PORT; ...".
var serveControlBaseRe = regexp.MustCompile(`control plane on (http://[0-9.]+:[0-9]+)`)

// smokeHarnesses returns the harnesses to exercise, honoring E2E_HARNESS
// (single-harness CI matrix leg) exactly like TestE2EEchoRest.
func smokeHarnesses(t *testing.T) []harnessConfig {
	t.Helper()
	if name := os.Getenv("E2E_HARNESS"); name != "" {
		cfg, ok := harnessByName(name)
		if !ok {
			t.Fatalf("E2E_HARNESS=%q not a known harness", name)
		}
		return []harnessConfig{cfg}
	}
	return allHarnesses()
}

// TestHarnessCLIContract installs each harness and verifies omac's CLI
// assumptions against the installed version's --help. Model-free.
func TestHarnessCLIContract(t *testing.T) {
	for _, h := range smokeHarnesses(t) {
		t.Run(h.Name, func(t *testing.T) {
			home := t.TempDir()
			installHarness(t, h, home)

			version := harnessVersion(t, h, home)
			t.Logf("%s resolved version: %q", h.Name, version)

			contract, ok := deriveContract(h.Name)
			if !ok {
				t.Fatalf("deriveContract(%q) returned ok=false", h.Name)
			}

			corpus := helpCorpus(t, h, home, contract.Subcommands)
			missing := missingTokens(corpus, contract.Tokens)

			result := "PASS"
			if len(missing) > 0 {
				result = "FAIL"
			}
			// Emit the compat line before asserting so the matrix records the
			// outcome even when the assertion fails the test.
			emitCompat(compatLine(h.Name, version, runtime.GOOS, "contract", result))

			if len(missing) > 0 {
				var b strings.Builder
				b.WriteString("CLI contract drift — the installed ")
				b.WriteString(h.Name)
				b.WriteString(" version no longer exposes flags/subcommands omac depends on:\n")
				for _, m := range missing {
					b.WriteString("  - ")
					b.WriteString(m.Token)
					b.WriteString("  (omac uses it in: ")
					b.WriteString(m.Source)
					b.WriteString(")\n")
				}
				b.WriteString("Fix the harness descriptor (internal/config/harness.go) or the e2e config " +
					"(internal/e2e/harnesses.go) to match the new CLI, then re-run.")
				ghaContractAnnotation(t, h.Name, missing)
				t.Fatalf("%s", b.String())
			}
			t.Logf("%s: all %d contract tokens present", h.Name, len(contract.Tokens))
		})
	}
}

// TestHarnessLaunchProbe verifies omac can launch each harness through the real
// sandbox and reach the actual binary, without a model call, by running the
// harness's own `--version` as the inner command.
func TestHarnessLaunchProbe(t *testing.T) {
	for _, h := range smokeHarnesses(t) {
		t.Run(h.Name, func(t *testing.T) {
			home := t.TempDir()
			workdir := t.TempDir()

			// Runtime dirs some harnesses expect; the sandbox skips nonexistent
			// allow paths, so create them before launch (mirrors runE2E).
			for _, dir := range []string{".cache", ".cache/opencode", ".local/share/opencode", ".local/state/opencode/locks"} {
				if err := os.MkdirAll(filepath.Join(home, dir), 0o755); err != nil {
					t.Fatal(err)
				}
			}

			omacBin := buildOmac(t)
			installHarness(t, h, home)
			version := harnessVersion(t, h, home)
			writeSandboxProfile(t, home, h, nil)

			ctx, cancel := context.WithTimeout(context.Background(), launchProbeTimeout)
			defer cancel()

			args := []string{"start", h.Name}
			if h.Sandbox.NoSandbox {
				args = append(args, "--no-sandbox")
			}
			args = append(args, "--", "--version")

			cmd := exec.CommandContext(ctx, omacBin, args...)
			cmd.Dir = workdir
			// A `--version` probe needs no provider creds, so use a bare HOME
			// env — NOT buildAgentEnv, whose per-harness EnvVars t.Fatal when
			// SKAINET_TOKEN is unset (claude-code, copilot). This keeps the
			// launch probe genuinely model-free and secret-free.
			cmd.Env = append(withHome(os.Environ(), home), "PWD="+workdir)
			cmd.Stdin = strings.NewReader("")
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr

			t.Logf("launch probe: omac %s", strings.Join(args, " "))
			err := cmd.Run()
			combined := stdout.String() + "\n" + stderr.String()

			result := "PASS"
			if err != nil || ctx.Err() == context.DeadlineExceeded {
				result = "FAIL"
			}
			emitCompat(compatLine(h.Name, version, runtime.GOOS, "launch", result))

			if ctx.Err() == context.DeadlineExceeded {
				t.Fatalf("launch probe timed out after %v\nSTDOUT:\n%s\nSTDERR:\n%s",
					launchProbeTimeout, tailLines(stdout.String(), 100), tailLines(stderr.String(), 100))
			}
			if err != nil {
				t.Fatalf("omac start ... -- --version failed: %v\nSTDOUT:\n%s\nSTDERR:\n%s",
					err, stdout.String(), stderr.String())
			}
			if strings.TrimSpace(combined) == "" {
				t.Fatalf("launch probe produced no output — inner binary may not have run")
			}
			t.Logf("%s launch probe ok (%d bytes of --version output)", h.Name, len(strings.TrimSpace(combined)))
		})
	}
}

// TestHarnessServeProbe verifies omac's Desktop/serve mode end-to-end without a
// model call: it launches `omac serve <harness>` with the REAL inner server
// daemon under the sandbox, then drives the host-side control plane over HTTP
// (activate a directory → read its manifest → list dirs → deactivate) and
// confirms the inner daemon was admitted through the sandbox and stays alive.
//
// Only harnesses with a server-launch convention are exercised (opencode, whose
// `opencode serve` daemon backs OpenCode Desktop). The rest run their inner
// command as-is under serve — there is no daemon to probe — so they emit a SKIP
// row and are skipped. Model-free and secret-free: `opencode serve` boots an
// HTTP server without provider auth, and the probe never issues a model call.
func TestHarnessServeProbe(t *testing.T) {
	for _, h := range smokeHarnesses(t) {
		t.Run(h.Name, func(t *testing.T) {
			if !harnessHasServerMode(h.Name) {
				emitCompat(compatLine(h.Name, "n/a", runtime.GOOS, "serve", "SKIP"))
				t.Skipf("%s has no server-launch convention; serve/Desktop mode not applicable", h.Name)
			}

			home := t.TempDir()
			workdir := t.TempDir()
			cwd := t.TempDir()

			// Runtime dirs the harness expects; the sandbox skips nonexistent
			// allow paths, so create them before launch (mirrors the launch probe).
			for _, dir := range []string{".cache", ".cache/opencode", ".local/share/opencode", ".local/state/opencode/locks"} {
				if err := os.MkdirAll(filepath.Join(home, dir), 0o755); err != nil {
					t.Fatal(err)
				}
			}

			omacBin := buildOmac(t)
			installHarness(t, h, home)
			version := harnessVersion(t, h, home)
			writeSandboxProfile(t, home, h, nil)

			err := runServeProbe(t, h, omacBin, home, workdir, cwd)

			result := "PASS"
			if err != nil {
				result = "FAIL"
			}
			// Emit before asserting so the matrix records the outcome even when
			// the assertion fails (same discipline as the other probes).
			emitCompat(compatLine(h.Name, version, runtime.GOOS, "serve", result))

			if err != nil {
				t.Fatalf("serve probe failed: %v", err)
			}
			t.Logf("%s serve probe ok (control plane + inner daemon + activate/deactivate)", h.Name)
		})
	}
}

// runServeProbe launches `omac serve <harness>` as a subprocess with the real
// inner daemon under the sandbox, waits for the control plane, drives one
// activate → dirs → deactivate cycle, and verifies the daemon was admitted and
// the process is still alive. It returns a descriptive error on the first
// failure (with captured output), or nil on success. The subprocess is always
// torn down via t.Cleanup.
func runServeProbe(t *testing.T, h harnessConfig, omacBin, home, workdir, cwd string) error {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), serveProbeTimeout)
	defer cancel()

	args := []string{"serve", h.Name, "--control-addr", "127.0.0.1:0", "--verbose"}
	if h.Sandbox.NoSandbox {
		args = append(args, "--no-sandbox")
	}

	cmd := exec.CommandContext(ctx, omacBin, args...)
	cmd.Dir = cwd
	// A serve launch probe needs no provider creds (opencode serve boots an
	// HTTP server without auth), so use a bare HOME env — NOT buildAgentEnv,
	// whose per-harness EnvVars t.Fatal when SKAINET_TOKEN is unset. This keeps
	// the probe genuinely model-free and secret-free.
	cmd.Env = append(withHome(os.Environ(), home), "PWD="+cwd)
	cmd.Stdin = strings.NewReader("")

	// Capture stdout/stderr into goroutine-safe buffers rather than an
	// os/exec pipe: the probe polls stdout for the control-plane line while a
	// background goroutine calls cmd.Wait(), and reading a StdoutPipe
	// concurrently with Wait is a documented footgun. Buffers we own sidestep
	// it entirely.
	var stdout, stderr syncBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	t.Logf("serve probe: omac %s", strings.Join(args, " "))
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start omac serve: %w", err)
	}

	// Wait for the subprocess in the background so we can distinguish "still
	// running" (healthy daemon) from "exited early" (inner launch failed).
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-waitCh:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			<-waitCh
		}
	})

	// Poll stdout for the control-plane URL the server prints on startup,
	// bailing early if the process exits before announcing it.
	controlBase, err := waitForControlBase(&stdout, waitCh, serveReadyTimeout)
	if err != nil {
		return fmt.Errorf("%w\nSTDERR:\n%s", err, tailLines(stderr.String(), 60))
	}

	// The inner daemon must be admitted through the sandbox. The control plane
	// is announced BEFORE the child launches, so wait for the onReady marker
	// (`--verbose`) rather than checking once — otherwise a slow runner could
	// finish the API round-trip before the child has even spawned.
	if err := waitForStderr(&stderr, "inner command started", waitCh, serveReadyTimeout); err != nil {
		return fmt.Errorf("inner daemon never started under the sandbox: %w\nSTDERR:\n%s",
			err, tailLines(stderr.String(), 60))
	}

	client := &http.Client{Timeout: 15 * time.Second}

	// Activate the workdir and assert the manifest carries a namespacing token.
	manifest, err := serveControlPost(client, controlBase+"/__omac__/activate", workdir)
	if err != nil {
		return fmt.Errorf("activate %s: %w\nSTDERR:\n%s", workdir, err, tailLines(stderr.String(), 60))
	}
	token, _ := manifest["dir_token"].(string)
	if token == "" {
		return fmt.Errorf("activate returned no dir_token: %v", manifest)
	}
	if state, _ := manifest["state"].(string); state == "" {
		return fmt.Errorf("activate returned no state: %v", manifest)
	}

	// The dir must now show up as active over the control plane.
	dirsBody, err := serveControlGet(client, controlBase+"/__omac__/dirs")
	if err != nil {
		return fmt.Errorf("GET /__omac__/dirs: %w", err)
	}
	if !strings.Contains(dirsBody, workdir) {
		return fmt.Errorf("/__omac__/dirs did not list the activated dir %q: %s", workdir, dirsBody)
	}

	// Deactivate cleanly.
	if _, err := serveControlPost(client, controlBase+"/__omac__/deactivate", workdir); err != nil {
		return fmt.Errorf("deactivate %s: %w", workdir, err)
	}

	// The daemon must still be alive after the whole round-trip — an early exit
	// means the sandboxed `<harness> serve` crashed rather than served.
	select {
	case werr := <-waitCh:
		return fmt.Errorf("omac serve exited while the probe was driving it (inner daemon likely crashed): %v\nSTDERR:\n%s",
			werr, tailLines(stderr.String(), 60))
	default:
	}
	return nil
}

// syncBuffer is a goroutine-safe bytes.Buffer. The serve probe reads captured
// stderr for diagnostics while the subprocess may still be writing to it (the
// daemon runs until teardown), so plain bytes.Buffer would race.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// waitForControlBase polls the captured stdout for the control-plane URL
// `omac serve` prints on startup, returning it once seen. It fails fast if the
// process exits first (waitCh fires) or the timeout elapses.
func waitForControlBase(stdout *syncBuffer, waitCh <-chan error, timeout time.Duration) (string, error) {
	deadline := time.After(timeout)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		if m := serveControlBaseRe.FindStringSubmatch(stdout.String()); m != nil {
			return m[1], nil
		}
		select {
		case werr := <-waitCh:
			// Drain-check one last time: the line may have been flushed just
			// before exit.
			if m := serveControlBaseRe.FindStringSubmatch(stdout.String()); m != nil {
				return m[1], nil
			}
			return "", fmt.Errorf("omac serve exited before announcing the control plane: %v", werr)
		case <-deadline:
			return "", fmt.Errorf("omac serve did not announce a control plane within %v", timeout)
		case <-tick.C:
		}
	}
}

// waitForStderr polls the captured stderr until it contains substr, returning
// nil once seen. It fails fast if the process exits first or the timeout
// elapses (the final drain-check covers a marker flushed just before exit).
func waitForStderr(stderr *syncBuffer, substr string, waitCh <-chan error, timeout time.Duration) error {
	deadline := time.After(timeout)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		if strings.Contains(stderr.String(), substr) {
			return nil
		}
		select {
		case werr := <-waitCh:
			if strings.Contains(stderr.String(), substr) {
				return nil
			}
			return fmt.Errorf("process exited before %q appeared: %v", substr, werr)
		case <-deadline:
			return fmt.Errorf("%q did not appear within %v", substr, timeout)
		case <-tick.C:
		}
	}
}

// serveControlPost POSTs {"dir": dir} to a control-plane endpoint and returns
// the decoded JSON response, erroring on any non-200 status.
func serveControlPost(client *http.Client, url, dir string) (map[string]any, error) {
	body, _ := json.Marshal(map[string]string{"dir": dir})
	resp, err := client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var m map[string]any
	if derr := json.NewDecoder(resp.Body).Decode(&m); derr != nil {
		return nil, fmt.Errorf("decode response: %w", derr)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d body=%v", resp.StatusCode, m)
	}
	return m, nil
}

// serveControlGet GETs a control-plane endpoint and returns the raw body.
func serveControlGet(client *http.Client, url string) (string, error) {
	resp, err := client.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status %d body=%s", resp.StatusCode, b)
	}
	return string(b), nil
}

// harnessVersion runs `<bin> --version` in the temp HOME and returns the
// sanitized version string. Fails the test if the binary can't report a
// version (a strong signal the install itself drifted).
func harnessVersion(t *testing.T, h harnessConfig, home string) string {
	t.Helper()
	out, err := runInHome(t, home, h.BinaryName+" --version", smokeCmdTimeout)
	if err != nil {
		t.Fatalf("%s --version failed: %v\n%s", h.BinaryName, err, out)
	}
	if strings.TrimSpace(out) == "" {
		t.Fatalf("%s --version produced no output", h.BinaryName)
	}
	return parseVersion(out)
}

// helpCorpus concatenates `<bin> --help` and `<bin> <sub> --help` for every
// subcommand omac invokes, so a flag documented only under a subcommand still
// resolves. Help output frequently goes to stderr and/or exits non-zero, so
// this captures combined output and ignores exit status.
func helpCorpus(t *testing.T, h harnessConfig, home string, subs []string) string {
	t.Helper()
	var b strings.Builder
	out, _ := runInHome(t, home, h.BinaryName+" --help", smokeCmdTimeout)
	b.WriteString(out)
	b.WriteString("\n")
	for _, sub := range subs {
		subOut, _ := runInHome(t, home, h.BinaryName+" "+sub+" --help", smokeCmdTimeout)
		b.WriteString(subOut)
		b.WriteString("\n")
	}
	return b.String()
}

// runInHome runs a simple command line via `sh -c` with the temp-HOME
// environment (so the harness binary on the temp PATH resolves), capturing
// combined stdout+stderr. cmdline must contain only trusted, shell-safe tokens
// (binary name + fixed flags) — never interpolated user input.
func runInHome(t *testing.T, home, cmdline string, timeout time.Duration) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", cmdline)
	cmd.Env = withHome(os.Environ(), home)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// emitCompat prints a machine-readable compatibility line to stdout for the
// workflow to parse (and echoes it to the test log via nothing extra — it is
// already visible under `go test -v`).
func emitCompat(line string) {
	// A bare Println (not t.Logf) keeps the line unindented and greppable on
	// stdout regardless of the test's verbosity framing.
	fmt.Println(line)
}

// ghaContractAnnotation surfaces contract drift as a GitHub Actions error
// annotation so the run summary shows which flags disappeared without opening
// the log. No-op outside GHA.
func ghaContractAnnotation(t *testing.T, harness string, missing []contractToken) {
	t.Helper()
	if os.Getenv("GITHUB_ACTIONS") != "true" {
		return
	}
	toks := make([]string, 0, len(missing))
	for _, m := range missing {
		toks = append(toks, m.Token)
	}
	fmt.Println("::error title=CLI contract drift (" + harness + ")::" +
		harness + " no longer exposes: " + strings.Join(toks, ", "))
}
