//go:build e2e || e2e_fast

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// buildOmac compiles the omac binary into a temp dir and returns its path.
//
// Lives in the e2e_fast build set (not just the full e2e build) because the
// model-free regression tests — serve_isolation_test.go in particular —
// drive the real compiled binary as a subprocess and must be runnable at
// PR time without a live LLM harness.
func buildOmac(t *testing.T) string {
	t.Helper()
	// OMAC_BIN lets a caller supply a prebuilt binary instead of compiling from
	// source — used by the "E2E: drift" omac@release matrix leg to exercise the
	// latest RELEASED omac against the latest harnesses. Unset ⇒ build main.
	if bin := os.Getenv("OMAC_BIN"); bin != "" {
		if _, err := os.Stat(bin); err != nil {
			t.Fatalf("OMAC_BIN=%q not usable: %v", bin, err)
		}
		t.Logf("using prebuilt omac binary from OMAC_BIN=%s", bin)
		return bin
	}
	binPath := filepath.Join(t.TempDir(), "omac")
	// Build from repo root (test CWD is internal/e2e/).
	repoRoot := filepath.Join("..", "..")
	cmd := exec.Command("go", "build", "-buildvcs=false", "-o", binPath, "./cmd/omac")
	cmd.Dir = repoRoot
	cmd.Env = os.Environ()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build omac: %v\n%s", err, out)
	}
	return binPath
}

// nestedInOmacSandbox reports whether the test process is itself running
// inside an omac sandbox. Two signals, both intentional:
//
//   - E2E_NESTED=1, set by scripts/e2e-local.sh when it detects OMAC_SOCKET.
//   - OMAC_SOCKET set directly in the env (the belt-and-suspenders path).
//
// The OMAC_SOCKET fallback is deliberate, not a leak: the whole point of
// the accommodations is to let an agent running inside an omac sandbox
// run the e2e suite. That agent inherits OMAC_SOCKET via allowance.go's
// EnvAllowVars (OMAC_* is passed through to the inner process), so when it
// shells out to `go test ./internal/e2e/` — whether via the wrapper or
// directly — the accommodations must apply or the tests fail (nested
// Seatbelt denied, SUN_LEN exceeded, postinstall blocked). The fallback
// ensures direct `go test` invocations work without the wrapper.
//
// The "leak" concern (a spawned agent running e2e unintentionally) is
// moot: if you're running e2e inside an omac sandbox, you want the
// accommodations; if you're not, OMAC_SOCKET is unset and nothing fires.
// CI never sets OMAC_SOCKET, so the accommodations never fire there.
//
// When true, the test accommodations are:
//   - `omac start`/`omac serve` run with --no-sandbox (macOS denies nested
//     Seatbelt profile application; Linux bwrap userns inside userns is
//     untested).
//   - TMPDIR is shortened so the facade's bridge.sock path stays under
//     macOS's 104-byte SUN_LEN limit.
//   - installHarness recovers from blocked postinstall scripts.
//
// Lives in the e2e_fast build set so serve_isolation_test.go (which runs
// under e2e_fast) can use it, not just the full e2e tests.
func nestedInOmacSandbox() bool {
	return os.Getenv("E2E_NESTED") == "1" || os.Getenv("OMAC_SOCKET") != ""
}

// shortTmpDirForNested returns a short TMPDIR path (/tmp/omac-e2e-<pid>)
// suitable for nested-sandbox runs, creating it if needed. Returns "" when
// not nested. The short path keeps the facade's bridge.sock under macOS's
// 104-byte SUN_LEN limit (the sandbox TMPDIR is a deep /var/folders/...
// path that exceeds it).
func shortTmpDirForNested(t *testing.T) string {
	t.Helper()
	if !nestedInOmacSandbox() {
		return ""
	}
	shortTmp := fmt.Sprintf("/tmp/omac-e2e-%d", os.Getpid())
	if err := os.MkdirAll(shortTmp, 0o755); err != nil {
		t.Fatalf("mkdir short TMPDIR %s: %v", shortTmp, err)
	}
	return shortTmp
}

// forceNoSandbox reports whether `omac start` should run with --no-sandbox
// for the given harness. True when the test is nested inside an omac
// sandbox — the inner sandbox (Seatbelt on macOS, bwrap on Linux) cannot
// be applied from inside an existing sandbox, so attempting it fails with
// EPERM/sandbox_apply. --no-sandbox is the only way to launch the inner
// command in that environment.
//
// Used by both runAgent/runAuditAgent (deciding the --no-sandbox flag on
// the omac start argv) AND by runSecurityAudit (deciding which assertion
// branch to take). Both must agree on this value: launching with
// --no-sandbox but asserting against sandbox-active behavior would fail
// every negative assertion (the agent ran unsandboxed, so secrets/env/fs
// are not denied). Keeping the decision in one function prevents the two
// call sites from drifting — the bug this replaced (sandboxActive :=
// !h.Sandbox.NoSandbox in runSecurityAudit, ignoring forceNoSandbox) did
// exactly that.
//
// Lives in fixtures.go (e2e || e2e_fast) so any e2e_fast file that later
// needs it compiles; the e2e-only smoke_test.go also uses it.
func forceNoSandbox(h harnessConfig) bool {
	if nestedInOmacSandbox() {
		return true
	}
	return h.Sandbox.NoSandbox
}

// withEnv returns env with the given key=value set, replacing any existing
// entry for key (rather than appending a duplicate). Used to override
// TMPDIR for nested-sandbox runs across all sites (buildAgentEnv,
// smoke_test.go's launch probe, serve_isolation_test.go's serve subprocess)
// — duplicate TMPDIR entries lead to undefined behavior across tools.
//
// Lives in the e2e_fast build set so serve_isolation_test.go can use it.
func withEnv(env []string, key, value string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env)+1)
	set := false
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			out = append(out, prefix+value)
			set = true
		} else {
			out = append(out, kv)
		}
	}
	if !set {
		out = append(out, prefix+value)
	}
	return out
}
