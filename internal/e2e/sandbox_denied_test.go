//go:build e2e || e2e_fast

package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestE2ESandboxDeniedAnswersOnDefaultLaunch is the live regression test for
// #173: on a DEFAULT launch (no flags) the facade must be able to tell the
// agent whether a path it could not read is protected by the sandbox or
// simply absent.
//
// The bug was a namespace mix-up. omac has two unrelated things called
// "sandbox profile": the LAUNCHER profile (a templated argv, named
// "builtin" by default) and the POLICY profile (the grant JSON, named
// "default"). The facade wiring fed the launcher name to the policy
// resolver, which always failed, so ProtectedPathChecker stayed nil and
// GET /sandbox/denied answered 404 for every path — indistinguishable from
// "endpoint present, path not protected" only by the X-Omac-Reason header
// the agent does not read.
//
// The in-process tests (internal/cli/facade_wiring_test.go,
// internal/facade/sandbox_denied_test.go) inject a plan or a stub checker.
// This one drives the real compiled binary through the real launch path and
// talks to its facade over loopback. It runs with --no-inner (control plane
// + facade only, no inner harness), so it needs no harness install, no
// model credentials, and no OS sandbox — and is part of the model-free
// e2e_fast slice.
func TestE2ESandboxDeniedAnswersOnDefaultLaunch(t *testing.T) {
	omacBin := buildOmac(t)
	home := t.TempDir()
	cwd := t.TempDir()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// No --no-sandbox: this is the default wiring path, the one that was
	// broken. --no-inner keeps it fast and harness-independent.
	cmd := exec.CommandContext(ctx, omacBin, "serve", "opencode", "--no-inner", "--control-addr", "127.0.0.1:0")
	cmd.Dir = cwd
	cmd.Env = withHome(os.Environ(), home)
	if shortTmp := shortTmpDirForNested(t); shortTmp != "" {
		cmd.Env = withEnv(cmd.Env, "TMPDIR", shortTmp)
	}
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

	facadePortCh := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stdoutPipe)
		for scanner.Scan() {
			if m := facadePortRe.FindStringSubmatch(scanner.Text()); m != nil {
				select {
				case facadePortCh <- m[1]:
				default:
				}
			}
		}
	}()
	var facadePort string
	select {
	case facadePort = <-facadePortCh:
	case <-time.After(15 * time.Second):
		t.Fatalf("omac serve did not print facade port within 15s; stderr:\n%s", stderrBuf.String())
	}

	client := &http.Client{Timeout: 10 * time.Second}
	query := func(path string) (int, map[string]any) {
		t.Helper()
		url := fmt.Sprintf("http://127.0.0.1:%s/sandbox/denied?path=%s", facadePort, path)
		resp, err := client.Get(url)
		if err != nil {
			t.Fatalf("GET %s: %v", url, err)
		}
		defer resp.Body.Close()
		var m map[string]any
		// A disabled endpoint answers text/plain, so a decode failure is
		// informative rather than fatal here.
		_ = json.NewDecoder(resp.Body).Decode(&m)
		return resp.StatusCode, m
	}

	// A baseline-protected credentials path: the endpoint must answer
	// "denied by the sandbox", not 404.
	protected := filepath.Join(home, ".aws", "credentials")
	code, body := query(protected)
	if code != http.StatusOK {
		t.Fatalf("GET /sandbox/denied?path=%s → %d (want 200); the protected-path checker is not wired.\n"+
			"body=%v\nserve stderr:\n%s", protected, code, body, stderrBuf.String())
	}
	if denied, _ := body["denied"].(bool); !denied {
		t.Errorf("denied = %v, want true for %s (body=%v)", body["denied"], protected, body)
	}
	if rule, _ := body["rule"].(string); rule != "baseline" {
		t.Errorf("rule = %v, want baseline (body=%v)", body["rule"], body)
	}
	if note, _ := body["note"].(string); note == "" {
		t.Errorf("note must carry the denial guidance; body=%v", body)
	}

	// An ordinary path inside the workdir is not protected: 404 with an
	// explicit denied:false — the other half of the answer, which proves the
	// 200 above is a real verdict and not a blanket response.
	ordinary := filepath.Join(cwd, "main.go")
	code, body = query(ordinary)
	if code != http.StatusNotFound {
		t.Errorf("GET /sandbox/denied?path=%s → %d (want 404); body=%v", ordinary, code, body)
	}
	if denied, ok := body["denied"].(bool); !ok || denied {
		t.Errorf("denied = %v, want false for an unprotected path (body=%v)", body["denied"], body)
	}

	// The bogus resolve warning fired on every default launch before the fix.
	if bytes.Contains(stderrBuf.Bytes(), []byte("could not be resolved")) {
		t.Errorf("default launch warned about an unresolvable sandbox profile:\n%s", stderrBuf.String())
	}
}
