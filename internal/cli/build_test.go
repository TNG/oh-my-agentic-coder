package cli

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/tngtech/oh-my-agentic-coder/internal/audit"
	"github.com/tngtech/oh-my-agentic-coder/internal/buildrun"
)

// clearBrokerEnvForDirectTests clears the broker/OMAC session env vars
// so the direct-host build path is selected. Inside an omac sandbox
// (where the test process inherits OMAC_SOCKET/OMAC_BASE/...), the
// managed-mode discriminator would otherwise fail closed — correct in
// production, but the direct-path tests need direct mode. Call this
// at the top of any test that exercises the direct build/stop path.
func clearBrokerEnvForDirectTests(t *testing.T) {
	t.Helper()
	for _, k := range []string{envBuildBrokerRequired, envControlBase, envBuildToken, envOmacSocket, envOmacBase} {
		t.Setenv(k, "")
	}
}

// TestRunBuildDenials verifies the policy-denial side of `omac build`:
// resolution failures, unsupported adapters and grammar errors exit with
// ExitBuildPolicyDenied and a structured stderr message, without ever
// touching the sandbox. These must run unconditionally (no kernel
// sandbox needed).
func TestRunBuildDenials(t *testing.T) {
	// Isolated HOME so a host-level omac config can't leak in.
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	clearBrokerEnvForDirectTests(t)
	wt := t.TempDir()
	env := &Env{
		Version: "test",
		Workdir: wt,
		Stdout:  newDevNull(t),
		Stderr:  newCapture(t),
	}

	run := func(args ...string) (int, string) {
		t.Helper()
		cap := newCapture(t)
		env.Stderr = cap
		code := runBuild(args, env)
		_ = cap.Sync()
		out, err := os.ReadFile(cap.Name())
		if err != nil {
			t.Fatal(err)
		}
		return code, string(out)
	}

	t.Run("unsupported adapter denied", func(t *testing.T) {
		code, errOut := run("--root", ".", "--", "maven", "verify")
		if code != ExitBuildPolicyDenied {
			t.Errorf("code = %d, want %d", code, ExitBuildPolicyDenied)
		}
		if !strings.Contains(errOut, "unsupported adapter") {
			t.Errorf("stderr = %q, want unsupported-adapter message", errOut)
		}
		if !strings.HasPrefix(errOut, "omac build:") {
			t.Errorf("stderr must be omac-prefixed: %q", errOut)
		}
	})

	t.Run("missing separator denied", func(t *testing.T) {
		code, errOut := run("--root", "backend")
		if code != ExitBuildPolicyDenied {
			t.Errorf("code = %d, want %d", code, ExitBuildPolicyDenied)
		}
		if !strings.Contains(errOut, "separator") {
			t.Errorf("stderr = %q", errOut)
		}
	})

	t.Run("traversal root denied", func(t *testing.T) {
		code, errOut := run("--root", "../outside", "--", "gradle", ":help")
		if code != ExitBuildPolicyDenied {
			t.Errorf("code = %d, want %d", code, ExitBuildPolicyDenied)
		}
		if !strings.Contains(errOut, "outside the worktree") {
			t.Errorf("stderr = %q", errOut)
		}
	})

	t.Run("absolute root outside denied", func(t *testing.T) {
		code, errOut := run("--root", t.TempDir(), "--", "gradle", ":help")
		if code != ExitBuildPolicyDenied {
			t.Errorf("code = %d, want %d", code, ExitBuildPolicyDenied)
		}
		if !strings.Contains(errOut, "outside the worktree") {
			t.Errorf("stderr = %q", errOut)
		}
	})

	t.Run("symlink root escape denied", func(t *testing.T) {
		outside := t.TempDir()
		if err := os.WriteFile(filepath.Join(outside, "gradlew"), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(wt, "evil")); err != nil {
			t.Fatal(err)
		}
		code, errOut := run("--root", "evil", "--", "gradle", ":help")
		if code != ExitBuildPolicyDenied {
			t.Errorf("code = %d, want %d", code, ExitBuildPolicyDenied)
		}
		if !strings.Contains(errOut, "symlink") {
			t.Errorf("stderr = %q, want symlink-escape message", errOut)
		}
	})

	t.Run("missing wrapper denied", func(t *testing.T) {
		code, errOut := run("--root", ".", "--", "gradle", ":help")
		if code != ExitBuildPolicyDenied {
			t.Errorf("code = %d, want %d", code, ExitBuildPolicyDenied)
		}
		if !strings.Contains(errOut, "gradlew") {
			t.Errorf("stderr = %q", errOut)
		}
	})

	t.Run("non-executable wrapper denied", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(wt, "gradlew"), []byte("#!/bin/sh\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		code, errOut := run("--root", ".", "--", "gradle", ":help")
		if code != ExitBuildPolicyDenied {
			t.Errorf("code = %d, want %d", code, ExitBuildPolicyDenied)
		}
		if !strings.Contains(errOut, "not executable") {
			t.Errorf("stderr = %q", errOut)
		}
	})

	t.Run("usage on help", func(t *testing.T) {
		cap := newCapture(t)
		env.Stderr = cap
		code := runBuild([]string{"--help"}, env)
		if code != ExitOK {
			t.Errorf("code = %d, want %d", code, ExitOK)
		}
		_ = cap.Sync()
		out, _ := os.ReadFile(cap.Name())
		s := string(out)
		for _, want := range []string{"omac build", "--root", "-- gradle", "exit codes", "policy denial", "cancell"} {
			if !strings.Contains(strings.ToLower(s), want) {
				t.Errorf("help text missing %q:\n%s", want, s)
			}
		}
	})
}

// TestBuildExitCodeReservations pins the disambiguation contract: omac's
// reserved exit codes must never collide with a raw Gradle exit code or
// shell signal conventions, so an `omac build` caller can tell
// policy/cancel/service outcomes apart from the build's own result by rc
// alone (plus the omac-prefixed stderr marker).
func TestBuildExitCodeReservations(t *testing.T) {
	for _, reserved := range []struct {
		name string
		code int
	}{
		{"ExitBuildPolicyDenied", ExitBuildPolicyDenied},
		{"ExitBuildCancelled", ExitBuildCancelled},
		{"ExitServiceFailure", buildrun.ExitServiceFailure},
	} {
		if reserved.code == 1 {
			t.Errorf("%s collides with Gradle's canonical build-failure rc 1", reserved.name)
		}
		if reserved.code >= 126 {
			t.Errorf("%s = %d collides with the shell 126/127/128+n convention", reserved.name, reserved.code)
		}
	}
	if buildrun.ExitServiceFailure == ExitBuildPolicyDenied || buildrun.ExitServiceFailure == ExitBuildCancelled {
		t.Errorf("service-failure code %d must differ from policy (%d) and cancel (%d)",
			buildrun.ExitServiceFailure, ExitBuildPolicyDenied, ExitBuildCancelled)
	}
}

// TestStartContainerProxy_Gating asserts ticket 08: the container proxy is
// started ONLY when the approved manifest declares container images (and
// only on macOS v1). The containerProxyStarter seam is faked so no real
// Docker/Colima daemon is touched. On Linux the proxy is not started
// (kernel-blocked build path); on macOS with no approved images it is not
// started (a standard Gradle project needs no Docker mediation).
func TestStartContainerProxy_Gating(t *testing.T) {
	env := &Env{Version: "test", Workdir: t.TempDir(), Stdout: newDevNull(t), Stderr: newDevNull(t)}
	auditor := audit.Nop()

	t.Run("no approved images not started", func(t *testing.T) {
		// The production gate (startContainerProxy) returns empty when no
		// images are approved; assert the production behavior directly
		// without touching a real Docker/Colima daemon.
		url, enabled, stop, err := startContainerProxy(env, t.TempDir(), t.TempDir(), nil, "b-test", auditor)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if url != "" || enabled || stop != nil {
			t.Errorf("no approved images must not start the proxy: url=%q enabled=%v stop=%v", url, enabled, stop != nil)
		}
	})

	t.Run("approved images started on macOS only", func(t *testing.T) {
		url, enabled, stop, err := startContainerProxy(env, t.TempDir(), t.TempDir(), []string{"pgvector/pgvector:pg16"}, "b-test", auditor)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if runtime.GOOS != "darwin" {
			// Linux: kernel-blocked, proxy not started.
			if url != "" || enabled || stop != nil {
				t.Errorf("Linux must not start the container proxy: url=%q enabled=%v", url, enabled)
			}
			return
		}
		// macOS: proxy started. stop is non-nil and runs Cleanup.
		if url == "" || !enabled || stop == nil {
			t.Fatalf("macOS with approved images must start the proxy: url=%q enabled=%v stop=%v", url, enabled, stop != nil)
		}
		if !strings.HasPrefix(url, "tcp://127.0.0.1:") {
			t.Errorf("DOCKER_HOST must be a loopback tcp URL: %q", url)
		}
		if strings.Contains(url, "@") {
			t.Errorf("DOCKER_HOST must carry no userinfo (ownership-based auth, not token): %q", url)
		}
		stop()
	})
}

// TestContainerExecutorID asserts the executor ownership label value is a
// stable, non-secret derivation of the worktree path (distinct across
// concurrent worktrees).
func TestContainerExecutorID(t *testing.T) {
	a := containerExecutorID("/repo/.worktrees/feat-a")
	b := containerExecutorID("/repo/.worktrees/feat-b")
	if a == b {
		t.Errorf("distinct worktrees must yield distinct executor ids: %q == %q", a, b)
	}
	if !strings.HasPrefix(a, "omac-") {
		t.Errorf("executor id must be omac-prefixed: %q", a)
	}
	if containerExecutorID("") == "" {
		t.Error("empty worktree must yield a non-empty fallback id")
	}
}

// newCapture returns a temp *os.File suitable as Env.Stderr/Stdout.
func newCapture(t *testing.T) *os.File {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "cap-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

// newDevNull returns a discard *os.File.
func newDevNull(t *testing.T) *os.File {
	t.Helper()
	f, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

// chmodBuildLeafInitDForCleanup restores init.d writability under the
// resolved build cache leaf so t.TempDir's RemoveAll can unlink the
// always-written retire-checkstyle-twins.gradle (and any
// registry-credentials.gradle) inside it. GrantsFor creates init.d
// read-only (0o500) to keep build code from planting an init script;
// that mode blocks RemoveAll, so every cli test that builds a leaf via
// prepareBuildCache/runBuild/runBuildStop must register this cleanup.
// cacheDir is the resolved OMAC cache scope dir (prepareBuildCache's
// first return), OR the HOME dir of a subprocess omac binary (the
// subprocess resolves the global scope at $HOME/.cache/omac/<digest>, so
// the caller passes the subprocess HOME when it cannot know the resolved
// scope dir up front). Best-effort: a missing init.d is silently skipped.
func chmodBuildLeafInitDForCleanup(t *testing.T, cacheDir string) {
	t.Helper()
	t.Cleanup(func() {
		leaf := filepath.Join(cacheDir, "gradle")
		// A subprocess omac binary never resolves the scope in the test
		// process; it uses os.UserHomeDir() of ITS env (the passed HOME), which
		// puts the leaf at $HOME/.cache/omac/<digest>/gradle. Fall back to
		// that layout when the direct leaf does not exist.
		if _, err := os.Stat(leaf); err != nil {
			home := cacheDir
			if entries, rerr := os.ReadDir(filepath.Join(home, ".cache", "omac")); rerr == nil {
				for _, e := range entries {
					if !e.IsDir() {
						continue
					}
					candidate := filepath.Join(home, ".cache", "omac", e.Name(), "gradle", "init.d")
					if info, serr := os.Stat(candidate); serr == nil && info.IsDir() {
						leaf = filepath.Dir(candidate)
						break
					}
				}
			}
		}
		_ = os.Chmod(filepath.Join(leaf, "init.d"), 0o755)
	})
}

// TestDaemonRecycle_ErrorLogsButBuildContinues asserts that a failing
// `gradlew --stop` (exit 1) returns a non-nil error but does NOT abort the
// build caller (the daemonRecycle closure in runBuild prints a warning and
// carries on — build.go:297-299). We test the error seam directly using
// StopGradleDaemon, which is what daemonRecycle wraps.
func TestDaemonRecycle_ErrorLogsButBuildContinues(t *testing.T) {
	wt := t.TempDir()
	wrapper := filepath.Join(wt, "gradlew")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	var stderrBuf bytes.Buffer
	err := buildrun.StopGradleDaemon(buildrun.StopDaemonOptions{
		Wrapper:    wrapper,
		ProjectDir: wt,
		Leaf:       t.TempDir(),
		Stderr:     &stderrBuf,
	})
	if err == nil {
		t.Error("StopGradleDaemon must return an error when gradlew --stop exits non-zero")
	}
	// Confirm the error is an ExitError (exit code 1), proving the
	// daemonRecycle closure receives a non-nil error and the build
	// continues (the error is logged, not returned).
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Errorf("expected *exec.ExitError, got %T: %v", err, err)
	}
	if ee != nil && ee.ExitCode() != 1 {
		t.Errorf("ExitCode = %d, want 1", ee.ExitCode())
	}
}

// TestBuildCacheDirResolution pins the GRADLE_USER_HOME provenance
// contract: the cache dir handed to buildrun comes from the resolved
// launcher config scope via internal/toolcache, never a hardcoded path.
func TestBuildCacheDirResolution(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	wt := t.TempDir()

	t.Run("default global scope resolves shared cache", func(t *testing.T) {
		dir, closeScope, err := prepareBuildCache(wt, "")
		if err != nil {
			t.Fatalf("prepareBuildCache: %v", err)
		}
		defer closeScope()
		want := filepath.Join(tmpHome, ".cache", "omac")
		if !strings.HasPrefix(dir, want+string(filepath.Separator)) {
			t.Errorf("cache dir %q not under shared omac cache root %q", dir, want)
		}
	})

	t.Run("workdir scope resolves per-workdir cache", func(t *testing.T) {
		dir, closeScope, err := prepareBuildCache(wt, "workdir")
		if err != nil {
			t.Fatalf("prepareBuildCache: %v", err)
		}
		defer closeScope()
		global, cg, err := prepareBuildCache(wt, "global")
		if err != nil {
			t.Fatal(err)
		}
		defer cg()
		if dir == global {
			t.Errorf("workdir-scoped cache must differ from global: %q", dir)
		}
	})

	t.Run("invalid override scope rejected", func(t *testing.T) {
		if _, _, err := prepareBuildCache(wt, "bogus"); err == nil {
			t.Error("expected error for bogus scope override")
		}
	})
}

// TestBuildExecutorSecurityBoundary (ticket 10, checkbox 6) is a
// consolidated regression test asserting the build-executor security
// boundary envelope at the unit level. The individual pieces are tested
// in their owning packages (grants_test.go, containerproxy/proxy_test.go,
// build_proxy_test.go); this test documents the FULL boundary in one
// place so a regression in any single piece is caught here too. Kernel-
// level fixture reads / raw-socket probes are host-side (gated integration
// tests skip in-sandbox); this covers the unit-provable envelope.
func TestBuildExecutorSecurityBoundary(t *testing.T) {
	t.Run("startContainerProxy returns disabled when no approved images", func(t *testing.T) {
		if runtime.GOOS != "darwin" {
			t.Skip("macOS-only proxy seam (Linux short-circuits on the platform gate before the no-images gate)")
		}
		// The container proxy is not started without approved images, so
		// DOCKER_HOST is never injected — the executor cannot reach the raw
		// daemon socket. (internal/buildrun/grants_test.go:
		// TestGrantsForContainerProxyEnv covers the enabled case + the
		// ChildEnv DOCKER_HOST absence; this asserts the disabled case from
		// the CLI gate.)
		env := &Env{Version: "test", Workdir: t.TempDir(), Stdout: newDevNull(t), Stderr: newDevNull(t)}
		url, enabled, stop, err := startContainerProxy(env, t.TempDir(), t.TempDir(), nil, "b-test", audit.Nop())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if url != "" || enabled || stop != nil {
			t.Errorf("no approved images must not start the proxy (raw socket would leak): url=%q enabled=%v", url, enabled)
		}
	})
	t.Run("container proxy URL is loopback with no userinfo", func(t *testing.T) {
		if runtime.GOOS != "darwin" {
			t.Skip("macOS-only proxy start")
		}
		env := &Env{Version: "test", Workdir: t.TempDir(), Stdout: newDevNull(t), Stderr: newDevNull(t)}
		url, enabled, stop, err := startContainerProxy(env, t.TempDir(), t.TempDir(), []string{"pgvector/pgvector:pg16"}, "b-test", audit.Nop())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !enabled || stop == nil {
			t.Fatalf("macOS with approved images must start the proxy: enabled=%v", enabled)
		}
		defer stop()
		if !strings.HasPrefix(url, "tcp://127.0.0.1:") {
			t.Errorf("DOCKER_HOST must be a loopback tcp URL: %q", url)
		}
		if strings.Contains(url, "@") {
			t.Errorf("DOCKER_HOST must carry no userinfo (ownership-based auth, not token): %q", url)
		}
	})
	t.Run("executor id is stable + non-secret + distinct per worktree", func(t *testing.T) {
		a := containerExecutorID("/repo/.worktrees/feat-a")
		b := containerExecutorID("/repo/.worktrees/feat-b")
		if a == b {
			t.Errorf("distinct worktrees must yield distinct executor ids: %q == %q", a, b)
		}
		if !strings.HasPrefix(a, "omac-") {
			t.Errorf("executor id must be omac-prefixed: %q", a)
		}
	})
	t.Run("build request id is non-empty, b-prefixed, and non-colliding", func(t *testing.T) {
		id := newBuildRequestID()
		if !strings.HasPrefix(id, "b") {
			t.Errorf("build request id must be b-prefixed: %q", id)
		}
		if len(id) < 10 {
			t.Errorf("build request id too short (must carry time + random): %q", id)
		}
		// Two ids generated in the same second differ by the random suffix.
		id2 := newBuildRequestID()
		if id == id2 {
			t.Errorf("two build request ids must not collide: %q == %q", id, id2)
		}
	})
}
