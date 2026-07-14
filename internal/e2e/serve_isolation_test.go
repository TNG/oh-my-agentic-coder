//go:build e2e

package e2e

import (
	"bufio"
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
	"strings"
	"testing"
	"time"
)

// stageServeSkill writes a minimal workdir-local skill whose omac.yaml
// declares a required secret that is never supplied. `serve` classifies it
// pending-credentials on activation: a real facade route is installed (so
// there's something to isolate) but no sidecar process is spawned, keeping
// this test fast and independent of any live harness install.
func stageServeSkill(t *testing.T, workdir, name string) {
	t.Helper()
	skillDir := filepath.Join(workdir, ".opencode", "skills", name)
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := "name: " + name + "\n" +
		"sidecar:\n" +
		"  command: [\"true\"]\n" +
		"  secrets:\n" +
		"    - name: API_TOKEN\n" +
		"      required: true\n"
	if err := os.WriteFile(filepath.Join(skillDir, "omac.yaml"), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}
}

var (
	controlBaseRe = regexp.MustCompile(`OMAC_CONTROL_BASE=(\S+)`)
	facadePortRe  = regexp.MustCompile(`facade on 127\.0\.0\.1:(\d+)`)
)

// TestE2EServeDirTokenIsolation is a live regression test for the dir_token
// leak fixed in 3ea0336 (#74, tracked for e2e coverage in #66): the
// control-plane port is whitelisted into the sandbox (see injectOpenPort in
// internal/cli/serve.go), so anything /__omac__/dirs returns is available
// to a fully-confined sandboxed agent process. Before the fix, that
// endpoint included every active directory's dir_token — the facade's
// namespace key — letting one directory's agent harvest another's token
// and reach its skills through the facade, which has no notion of which
// sandbox is asking.
//
// Unlike the in-process handler tests in internal/cli/serve_test.go
// (TestDirsEndpointDoesNotLeakTokens, TestTwoDirsDistinctTokensAndRoutes),
// this drives the real compiled `omac serve` binary as a subprocess and
// talks to its control-plane and facade ports over real loopback sockets —
// the same topology a sandboxed inner process actually uses — catching
// regressions in the real wire path, not just the Go-level handler. It
// runs with --no-inner (control plane only, no inner harness), so it needs
// no live LLM harness install and no OS sandbox, and runs fast.
func TestE2EServeDirTokenIsolation(t *testing.T) {
	omacBin := buildOmac(t)
	home := t.TempDir()
	cwd := t.TempDir()
	wdA := t.TempDir()
	wdB := t.TempDir()
	stageServeSkill(t, wdA, "slack")
	stageServeSkill(t, wdB, "slack")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, omacBin, "serve", "opencode", "--no-inner", "--control-addr", "127.0.0.1:0")
	cmd.Dir = cwd
	cmd.Env = withHome(os.Environ(), home)
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf
	if err := cmd.Start(); err != nil {
		t.Fatalf("start omac serve: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	controlBaseCh := make(chan string, 1)
	facadePortCh := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stdoutPipe)
		for scanner.Scan() {
			line := scanner.Text()
			if m := controlBaseRe.FindStringSubmatch(line); m != nil {
				select {
				case controlBaseCh <- m[1]:
				default:
				}
			}
			if m := facadePortRe.FindStringSubmatch(line); m != nil {
				select {
				case facadePortCh <- m[1]:
				default:
				}
			}
		}
	}()

	var controlBase, facadePort string
	select {
	case controlBase = <-controlBaseCh:
	case <-time.After(15 * time.Second):
		t.Fatalf("omac serve did not print OMAC_CONTROL_BASE within 15s; stderr:\n%s", stderrBuf.String())
	}
	select {
	case facadePort = <-facadePortCh:
	case <-time.After(15 * time.Second):
		t.Fatalf("omac serve did not print facade port within 15s; stderr:\n%s", stderrBuf.String())
	}

	client := &http.Client{Timeout: 10 * time.Second}

	activate := func(dir string) map[string]any {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"dir": dir})
		resp, err := client.Post(controlBase+"/__omac__/activate", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("activate %s: %v", dir, err)
		}
		defer resp.Body.Close()
		var m map[string]any
		if derr := json.NewDecoder(resp.Body).Decode(&m); derr != nil {
			t.Fatalf("decode activate response for %s: %v", dir, derr)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("activate %s: status %d body=%v", dir, resp.StatusCode, m)
		}
		return m
	}

	mA := activate(wdA)
	mB := activate(wdB)
	tokA, _ := mA["dir_token"].(string)
	tokB, _ := mB["dir_token"].(string)
	if tokA == "" || tokB == "" {
		t.Fatalf("expected non-empty dir_tokens, got A=%q B=%q", tokA, tokB)
	}
	if tokA == tokB {
		t.Fatalf("dir A and dir B were minted the same token: %q", tokA)
	}

	// --- Core regression: /__omac__/dirs must not leak either token. ---
	resp, err := client.Get(controlBase + "/__omac__/dirs")
	if err != nil {
		t.Fatalf("GET /__omac__/dirs: %v", err)
	}
	dirsBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(dirsBody), tokA) || strings.Contains(string(dirsBody), tokB) {
		t.Errorf("/__omac__/dirs leaked a dir_token over the wire (issue #74 regression): %s", dirsBody)
	}

	// --- Positive control: each dir's own token still resolves its own
	// route through the real facade TCP listener — proves namespacing is
	// live, not just that every request happens to fail. ---
	facadeGet := func(token string) int {
		t.Helper()
		u := fmt.Sprintf("http://127.0.0.1:%s/%s/slack/status", facadePort, token)
		r, err := client.Get(u)
		if err != nil {
			t.Fatalf("GET %s: %v", u, err)
		}
		defer r.Body.Close()
		return r.StatusCode
	}
	if got := facadeGet(tokA); got != http.StatusConflict {
		t.Errorf("dir A's own token against its own route: status=%d, want 409 (pending-credentials)", got)
	}
	if got := facadeGet(tokB); got != http.StatusConflict {
		t.Errorf("dir B's own token against its own route: status=%d, want 409 (pending-credentials)", got)
	}

	// --- Negative control: a same-shaped but wrong token must not resolve
	// any route. With two dirs active, the single-dir flat-alias fallback
	// (refreshSingleDirAliases) is torn down, so no other path should
	// resolve the mount. ---
	wrongToken := strings.Repeat("0", len(tokA))
	if wrongToken == tokA || wrongToken == tokB {
		wrongToken = strings.Repeat("f", len(tokA))
	}
	if got := facadeGet(wrongToken); got != http.StatusNotFound {
		t.Errorf("guessed token resolved a route: status=%d, want 404", got)
	}
}
