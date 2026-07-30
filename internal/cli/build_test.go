package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tngtech/oh-my-agentic-coder/internal/buildrun"
)

// TestRunBuildDenials verifies the policy-denial side of `omac build`:
// resolution failures, unsupported adapters and grammar errors exit with
// ExitBuildPolicyDenied and a structured stderr message, without ever
// touching the sandbox. These must run unconditionally (no kernel
// sandbox needed).
func TestRunBuildDenials(t *testing.T) {
	// Isolated HOME so a host-level omac config can't leak in.
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
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
// first return). Best-effort: a missing init.d is silently skipped.
func chmodBuildLeafInitDForCleanup(t *testing.T, cacheDir string) {
	t.Helper()
	leaf := filepath.Join(cacheDir, "gradle")
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(leaf, "init.d"), 0o755) })
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
