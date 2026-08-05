package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tngtech/oh-my-agentic-coder/internal/buildmanifest"
	"github.com/tngtech/oh-my-agentic-coder/internal/buildrun"
)

// writeApproveManifest writes `.omac/build.yaml` under wt with the given
// content, mirroring buildmanifest's test helper.
func writeApproveManifest(t *testing.T, wt, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(wt, ".omac"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".omac", "build.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// nonTTYStdin returns a *os.File backed by a regular temp file (NOT a
// character device), so isInteractive returns false. This is the
// security-critical gate: an agent or script with piped/redirected
// stdin cannot auto-confirm.
func nonTTYStdin(t *testing.T) *os.File {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "approve-stdin-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

// TestRunBuildApprove_RefusedInManagedSession asserts `omac build approve`
// exits 3 (ExitBuildPolicyDenied) when OMAC_BUILD_BROKER_REQUIRED=1 +
// the broker tuple is present. An agent cannot approve its own
// capability set (ticket 06).
func TestRunBuildApprove_RefusedInManagedSession(t *testing.T) {
	clearBrokerEnv(t)
	t.Setenv(envBuildBrokerRequired, "1")
	t.Setenv(envControlBase, "http://127.0.0.1:12345")
	t.Setenv(envBuildToken, "abc")
	t.Setenv("HOME", t.TempDir())
	wt := t.TempDir()
	env := &Env{
		Version: "test",
		Workdir: wt,
		Stdout:  newDevNull(t),
		Stderr:  newCapture(t),
		Stdin:   nonTTYStdin(t),
	}
	code := runBuildApprove(nil, env)
	if code != ExitBuildPolicyDenied {
		t.Fatalf("runBuildApprove in managed session = %d, want %d", code, ExitBuildPolicyDenied)
	}
}

// TestRunBuildApprove_RefusedWhenPartialOMACEnvPresent asserts approve
// refuses (exit 3) when any partial OMAC session env is present even
// without the required marker (defense in depth — an agent inside an
// omac session has OMAC_SOCKET/OMAC_BASE set).
func TestRunBuildApprove_RefusedWhenPartialOMACEnvPresent(t *testing.T) {
	cases := map[string]string{
		envOmacSocket:  "/tmp/omac.sock",
		envOmacBase:    "http://127.0.0.1:9999",
		envControlBase: "http://127.0.0.1:9999",
		envBuildToken:  "abc",
	}
	for name, val := range cases {
		t.Run(name, func(t *testing.T) {
			clearBrokerEnv(t)
			t.Setenv(name, val)
			t.Setenv("HOME", t.TempDir())
			wt := t.TempDir()
			env := &Env{
				Version: "test",
				Workdir: wt,
				Stdout:  newDevNull(t),
				Stderr:  newCapture(t),
				Stdin:   nonTTYStdin(t),
			}
			code := runBuildApprove(nil, env)
			if code != ExitBuildPolicyDenied {
				t.Errorf("partial env %q: code = %d, want %d", name, code, ExitBuildPolicyDenied)
			}
		})
	}
}

// TestRunBuildApprove_RefusedWithoutTTY asserts approve refuses (exit 3)
// when stdin is not a TTY. This is the security-critical gate: an agent
// or script with piped/redirected stdin cannot auto-confirm. A real
// interactive host terminal has ModeCharDevice set; a temp file does
// not.
func TestRunBuildApprove_RefusedWithoutTTY(t *testing.T) {
	clearBrokerEnv(t)
	t.Setenv("HOME", t.TempDir())
	wt := t.TempDir()
	cap := newCapture(t)
	env := &Env{
		Version: "test",
		Workdir: wt,
		Stdout:  newDevNull(t),
		Stderr:  cap,
		Stdin:   nonTTYStdin(t),
	}
	code := runBuildApprove(nil, env)
	if code != ExitBuildPolicyDenied {
		t.Fatalf("runBuildApprove without TTY = %d, want %d", code, ExitBuildPolicyDenied)
	}
	_ = cap.Sync()
	out, _ := os.ReadFile(cap.Name())
	if !strings.Contains(string(out), "not a TTY") {
		t.Errorf("stderr missing 'not a TTY' diagnostic: %q", out)
	}
}

// TestRunBuildApprove_NoManifestIsNoOp asserts approve succeeds (exit 0)
// with a "nothing to approve" message when the worktree has no
// .omac/build.yaml — the normal standard-Gradle-project case.
//
// NOTE: this test reaches the manifest-load step, which requires
// passing both the managed-session and TTY gates. The TTY gate is
// relaxed here by pointing Stdin at /dev/null ON A REAL TTY ONLY — but
// in a sandbox/CI /dev/null is a character device on macOS/Linux, so
// isInteractive returns true for it. On platforms where /dev/null is
// not a character device this test would hit the TTY refusal instead;
// that is acceptable (the gate is working). The non-TTY refusal is
// covered by TestRunBuildApprove_RefusedWithoutTTY above.
func TestRunBuildApprove_NoManifestIsNoOp(t *testing.T) {
	clearBrokerEnv(t)
	t.Setenv("HOME", t.TempDir())
	wt := t.TempDir()
	stdin, err := os.Open(os.DevNull)
	if err != nil {
		t.Skipf("cannot open %s: %v", os.DevNull, err)
	}
	t.Cleanup(func() { stdin.Close() })
	if !isInteractive(stdin) {
		t.Skipf("%s is not a character device on this platform; the TTY gate refuses (correct behavior, but this test exercises the past-TTY path)", os.DevNull)
	}
	outCap := newCapture(t)
	env := &Env{
		Version: "test",
		Workdir: wt,
		Stdout:  outCap,
		Stderr:  newCapture(t),
		Stdin:   stdin,
	}
	code := runBuildApprove(nil, env)
	if code != ExitOK {
		t.Fatalf("runBuildApprove with no manifest = %d, want %d", code, ExitOK)
	}
	_ = outCap.Sync()
	out, _ := os.ReadFile(outCap.Name())
	if !strings.Contains(string(out), "nothing to approve") {
		t.Errorf("stdout missing 'nothing to approve': %q", out)
	}
}

// TestRunBuildApprove_RendersDiffAndNeverExecutes asserts that with a
// manifest present, approve renders the capability diff to stdout and
// does NOT execute any build code (no gradlew invocation). The test
// plants a manifest with container images, then runs approve with
// /dev/null as stdin; /dev/null is a character device on macOS/Linux
// (so isInteractive returns true, exercising the past-TTY path), and
// reading from it yields EOF → empty confirm input → abort without
// writing a durable approval and without executing the wrapper.
//
// See TestRunBuildApprove_NoManifestIsNoOp for the TTY-gate note:
// /dev/null is a character device on macOS/Linux, so this exercises
// the past-TTY path on those platforms. On platforms where /dev/null
// is not a character device the test skips (the non-TTY refusal is
// covered by TestRunBuildApprove_RefusedWithoutTTY).
func TestRunBuildApprove_RendersDiffAndNeverExecutes(t *testing.T) {
	clearBrokerEnv(t)
	t.Setenv("HOME", t.TempDir())
	wt := t.TempDir()
	writeApproveManifest(t, wt, `version: 1
builds:
  - root: backend
    tool: gradle
    containers:
      images:
        - pgvector/pgvector:pg16
`)

	// Plant a gradlew that, if executed, would write a marker file.
	// Approve must NOT invoke it.
	marker := filepath.Join(wt, "approve-executed")
	if err := os.WriteFile(filepath.Join(wt, "gradlew"), []byte("#!/bin/sh\necho ran > "+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// stdin = /dev/null: a character device on macOS/Linux (so
	// isInteractive returns true, exercising the past-TTY path), and
	// reading from it yields EOF → empty confirm input → abort.
	stdin, err := os.Open(os.DevNull)
	if err != nil {
		t.Skipf("cannot open %s: %v", os.DevNull, err)
	}
	t.Cleanup(func() { stdin.Close() })
	if !isInteractive(stdin) {
		t.Skipf("%s is not a character device on this platform; the TTY gate refuses (correct behavior). The non-TTY refusal is covered by TestRunBuildApprove_RefusedWithoutTTY.", os.DevNull)
	}

	outCap := newCapture(t)
	env := &Env{
		Version: "test",
		Workdir: wt,
		Stdout:  outCap,
		Stderr:  newCapture(t),
		Stdin:   stdin,
	}
	code := runBuildApprove(nil, env)
	if code != ExitOK {
		t.Fatalf("runBuildApprove (abort path) = %d, want %d", code, ExitOK)
	}
	_ = outCap.Sync()
	out, _ := os.ReadFile(outCap.Name())
	// The diff render must mention the container image.
	if !strings.Contains(string(out), "pgvector/pgvector:pg16") {
		t.Errorf("stdout missing capability diff with image name: %q", out)
	}
	// Must have aborted without writing approval (EOF on /dev/null →
	// empty input → not y/yes/confirm → abort).
	if !strings.Contains(string(out), "aborted") {
		t.Errorf("stdout missing 'aborted' confirmation: %q", out)
	}
	// Must NOT have executed the wrapper.
	if _, err := os.Stat(marker); err == nil {
		t.Errorf("approve executed the wrapper (marker %s exists) — approve must never execute build code", marker)
	}
	// Must NOT have written a durable approval.
	cacheDir, closeScope, err := prepareBuildCache(wt, "")
	if err != nil {
		t.Fatalf("prepareBuildCache: %v", err)
	}
	canon, _ := canonicalWorktree(wt)
	loc := buildControlApprovalLocation(cacheDir, canon)
	leaf := buildrun.GradleLeaf(cacheDir)
	rec, _ := buildmanifest.LoadApprovalAt(leaf, loc)
	if rec.Digest != "" {
		t.Errorf("approve wrote a durable approval on abort — it must only write after explicit confirm: %+v", rec)
	}
	closeScope()
	chmodBuildLeafInitDForCleanup(t, cacheDir)
}

// TestRunBuildApprove_ParseArgs asserts the --root flag is parsed
// (mirroring `omac build stop`'s grammar) and unknown flags are
// denied. The TTY/managed gates fire first, so these tests use the
// /dev/null TTY trick to reach the arg-parsing step.
func TestRunBuildApprove_ParseArgs(t *testing.T) {
	t.Run("root space form", func(t *testing.T) {
		root, err := parseApproveArgs([]string{"--root", "backend"})
		if err != nil || root != "backend" {
			t.Errorf("--root backend: root=%q err=%v", root, err)
		}
	})
	t.Run("root equals form", func(t *testing.T) {
		root, err := parseApproveArgs([]string{"--root=backend"})
		if err != nil || root != "backend" {
			t.Errorf("--root=backend: root=%q err=%v", root, err)
		}
	})
	t.Run("default root is dot", func(t *testing.T) {
		root, err := parseApproveArgs(nil)
		if err != nil || root != "." {
			t.Errorf("default root=%q err=%v want %q", root, err, ".")
		}
	})
	t.Run("root requires value", func(t *testing.T) {
		if _, err := parseApproveArgs([]string{"--root"}); err == nil {
			t.Error("--root without value must error")
		}
	})
	t.Run("root must not be empty", func(t *testing.T) {
		if _, err := parseApproveArgs([]string{"--root="}); err == nil {
			t.Error("--root= empty must error")
		}
	})
	t.Run("unknown flag denied", func(t *testing.T) {
		if _, err := parseApproveArgs([]string{"--bogus"}); err == nil {
			t.Error("--bogus must error")
		}
	})
}

// TestIsInteractive_NonTTYReturnsFalse asserts the TTY check returns
// false for a regular file (the security gate: piped stdin cannot
// auto-confirm).
func TestIsInteractive_NonTTYReturnsFalse(t *testing.T) {
	f := nonTTYStdin(t)
	if isInteractive(f) {
		t.Error("regular temp file reported as interactive")
	}
	if isInteractive(nil) {
		t.Error("nil file reported as interactive")
	}
}
