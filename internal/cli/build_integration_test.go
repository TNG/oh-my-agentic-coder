package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// buildOmacBinary compiles the omac binary for integration tests.
func buildOmacBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "omac-test-bin")
	out, err := exec.Command("go", "build", "-buildvcs=false", "-o", bin, "../../cmd/omac").CombinedOutput()
	if err != nil {
		t.Fatalf("go build omac: %v\n%s", err, out)
	}
	return bin
}

// TestBuildHarnessIndependence invokes the compiled `omac build` twice
// under two different stripped, harness-flavored minimal environments
// (env -i style) and asserts identical contract behavior: same exit code,
// same result marker. No harness-specific transport may influence the
// build contract.
//
// The kernel-sandboxed launch requires sandbox-exec to accept a profile;
// inside a nested omac sandbox (this dev environment) that is impossible
// (sandbox_apply: Operation not permitted), so the full launch check
// skips when the sandbox-exec self-test fails. The DENIAL contract —
// which is the harness-independence point — runs unconditionally.
//
// TODO(doc): host-side validation of a real repository Gradle wrapper
// (`./gradlew :help` against a pre-seeded <cache scope>/gradle leaf) is
// pending — the fixtures below use stub shells, NOT a real wrapper,
// because a real cold-cache wrapper cannot bootstrap with the executor's
// network blocked (see docs/build-command.md §Cold-cache wrapper
// bootstrap) and this environment cannot stage a nested sandbox run. Do
// not replace the stubs with a fake "real wrapper" assertion here.
func TestBuildHarnessIndependence(t *testing.T) {
	bin := buildOmacBinary(t)

	// Fixture worktree with a wrapper whose output is environment-visible
	// (proves env construction is identical across harness flavors).
	wt := t.TempDir()
	cacheHome := t.TempDir()
	chmodBuildLeafInitDForCleanup(t, cacheHome)
	wrapper := "#!/bin/sh\necho \"GUH-SET=${GRADLE_USER_HOME:+yes}\"\necho \"HOME-AWARE=${HOME:-unset}\"\nexit 0\n"
	if err := os.WriteFile(filepath.Join(wt, "gradlew"), []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}

	envs := map[string][]string{
		"opencode-flavored": {
			"PATH=" + os.Getenv("PATH"),
			"HOME=" + cacheHome,
			"OPENCODE=1",
			"OMAC_SOCKET=/tmp/should-not-leak.sock",
			"OMAC_BASE=http+unix://should/not/leak",
		},
		"claude-flavored": {
			"PATH=" + os.Getenv("PATH"),
			"HOME=" + cacheHome,
			"CLAUDECODE=1",
			"ANTHROPIC_API_KEY=sk-ant-must-not-leak",
		},
	}

	type outcome struct {
		code   int
		stdout string
		stderr string
	}
	results := map[string]outcome{}

	run := func(env []string, args ...string) outcome {
		cmd := exec.Command(bin, args...)
		cmd.Dir = wt
		cmd.Env = env
		var so, se strings.Builder
		cmd.Stdout, cmd.Stderr = &so, &se
		err := cmd.Run()
		code := 0
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				code = ee.ExitCode()
			} else {
				t.Fatalf("run: %v", err)
			}
		}
		return outcome{code: code, stdout: so.String(), stderr: se.String()}
	}

	// Denial contract — unconditional (never touches the sandbox).
	for name, env := range envs {
		o := run(env, "build", "--root", "../escape", "--", "gradle", ":help")
		results[name] = o
		if o.code != ExitBuildPolicyDenied {
			t.Errorf("%s: denial code = %d, want %d (stderr: %s)", name, o.code, ExitBuildPolicyDenied, o.stderr)
		}
		if !strings.Contains(o.stderr, "omac build:") || !strings.Contains(o.stderr, "outside the worktree") {
			t.Errorf("%s: denial stderr = %q", name, o.stderr)
		}
	}
	a, b := results["opencode-flavored"], results["claude-flavored"]
	if a.code != b.code || !strings.Contains(a.stderr, "outside") || !strings.Contains(b.stderr, "outside") {
		t.Errorf("denial contract diverged across harness flavors: %+v vs %+v", a, b)
	}

	// Credential isolation marker: a harness-flavored env credential must
	// not appear in any output even on the denial path.
	for name, o := range results {
		if strings.Contains(o.stdout, "sk-ant") || strings.Contains(o.stderr, "sk-ant") ||
			strings.Contains(o.stdout, "should-not-leak") || strings.Contains(o.stderr, "should-not-leak") {
			t.Errorf("%s: harness env leaked into build output: stdout=%q stderr=%q", name, o.stdout, o.stderr)
		}
	}

	// Full launch — kernel-gated.
	if !kernelSandboxAvailable(t) {
		t.Skip("nested sandbox: sandbox-exec self-test failed; kernel-enforced launch covered on host/CI")
	}
	marks := map[string]string{}
	for name, env := range envs {
		o := run(env, "build", "--root", ".", "--", "gradle")
		if o.code != 0 {
			t.Fatalf("%s: build exit = %d, stderr: %s", name, o.code, o.stderr)
		}
		if !strings.Contains(o.stdout, "GUH-SET=yes") {
			t.Errorf("%s: GRADLE_USER_HOME not injected; stdout = %q", name, o.stdout)
		}
		// HOME is deliberately not forwarded (host gradle control state
		// must stay out of the executor).
		if strings.Contains(o.stdout, "sk-ant") || strings.Contains(o.stdout, "OMAC_SOCKET") {
			t.Errorf("%s: harness env leaked into executor", name)
		}
		marks[name] = o.stdout
	}
	if marks["opencode-flavored"] != marks["claude-flavored"] {
		t.Errorf("build output diverged across harness flavors:\n%q\nvs\n%q",
			marks["opencode-flavored"], marks["claude-flavored"])
	}
}

// kernelSandboxAvailable probes whether the platform kernel sandbox can
// actually be applied from this process. Inside an omac sandbox, macOS
// denies nested sandbox_apply, and the self-test is the documented gate:
// executor tests requiring kernel enforcement skip when it fails.
func kernelSandboxAvailable(t *testing.T) bool {
	t.Helper()
	switch runtime.GOOS {
	case "darwin":
		cmd := exec.Command("/usr/bin/sandbox-exec", "-p", "(allow default)", "/usr/bin/true")
		if err := cmd.Run(); err != nil {
			t.Logf("sandbox-exec self-test failed (nested?): %v", err)
			return false
		}
		return true
	case "linux":
		if _, err := exec.LookPath("bwrap"); err != nil {
			return false
		}
		if err := exec.Command("bwrap", "--ro-bind", "/", "/", "true").Run(); err != nil {
			return false
		}
		return true
	default:
		return false
	}
}

// TestBuildStreaming verifies output streams incrementally rather than
// being buffered to completion: a line printed first must be observable
// while the build is still running.
func TestBuildStreaming(t *testing.T) {
	if !kernelSandboxAvailable(t) {
		t.Skip("kernel sandbox unavailable (nested)")
	}
	bin := buildOmacBinary(t)
	wt := t.TempDir()
	wrapper := "#!/bin/sh\necho first-line\nsleep 2\necho second-line\n"
	if err := os.WriteFile(filepath.Join(wt, "gradlew"), []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	cacheHome := t.TempDir()
	chmodBuildLeafInitDForCleanup(t, cacheHome)
	cmd := exec.Command(bin, "build", "--root", ".", "--", "gradle")
	cmd.Dir = wt
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + cacheHome}
	pr, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	got := make(chan string, 1)
	go func() {
		buf := make([]byte, 64)
		n, _ := pr.Read(buf)
		got <- string(buf[:n])
	}()
	select {
	case s := <-got:
		if !strings.Contains(s, "first-line") {
			t.Errorf("first streamed chunk = %q, want it to contain first-line", s)
		}
	case <-time.After(1500 * time.Millisecond):
		t.Error("no output streamed before build completion stream test window closed")
	}
	_ = cmd.Wait()
}

// TestBuildCancellation verifies SIGINT to the omac process maps to the
// distinct cancellation exit code, preceded on stderr by the
// omac-prefixed cancellation marker (rc==4 alone would be
// indistinguishable from a raw `gradle exit 4`).
func TestBuildCancellation(t *testing.T) {
	if !kernelSandboxAvailable(t) {
		t.Skip("kernel sandbox unavailable (nested)")
	}
	bin := buildOmacBinary(t)
	wt := t.TempDir()
	wrapper := "#!/bin/sh\nsleep 30\n"
	if err := os.WriteFile(filepath.Join(wt, "gradlew"), []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	cacheHome := t.TempDir()
	chmodBuildLeafInitDForCleanup(t, cacheHome)
	cmd := exec.Command(bin, "build", "--root", ".", "--", "gradle")
	cmd.Dir = wt
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + cacheHome}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Give the build a moment to start, then interrupt omac itself.
	time.Sleep(500 * time.Millisecond)
	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	err := cmd.Wait()
	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			t.Fatalf("wait: %v", err)
		}
	}
	if code != ExitBuildCancelled {
		t.Errorf("exit = %d, want %d (cancellation)", code, ExitBuildCancelled)
	}
	if !strings.Contains(stderr.String(), "omac build: cancelled") {
		t.Errorf("stderr = %q, want the omac-prefixed cancellation marker", stderr.String())
	}
}
