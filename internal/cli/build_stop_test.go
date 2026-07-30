package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunBuildStop_InvokesWrapperStopAndReleasesLock asserts `omac build stop`
// runs the repo wrapper with --stop under the leaf's GRADLE_USER_HOME and
// removes the per-worktree queue lockfile. Uses a stub wrapper that
// records its args + GRADLE_USER_HOME so the test runs without a real
// Gradle or kernel sandbox.
func TestRunBuildStop_InvokesWrapperStopAndReleasesLock(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	wt := t.TempDir()

	// Stub wrapper: records its argv and GRADLE_USER_HOME to files the
	// test reads back.
	marker := filepath.Join(wt, "stop-marker")
	wrapper := "#!/bin/sh\n" +
		"echo \"args=$*\" >> " + marker + "\n" +
		"echo \"GUH=$GRADLE_USER_HOME\" >> " + marker + "\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(wt, "gradlew"), []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}

	env := &Env{
		Version: "test",
		Workdir: wt,
		Stdout:  newDevNull(t),
		Stderr:  newCapture(t),
	}

	// Pre-create a lingering lockfile (as a crashed build would leave).
	cacheDir, closeScope, err := prepareBuildCache(wt, "")
	if err != nil {
		t.Fatalf("prepareBuildCache: %v", err)
	}
	leaf := filepath.Join(cacheDir, "gradle")
	if err := os.MkdirAll(leaf, 0o700); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(leaf, ".omac-build.lock")
	if err := os.WriteFile(lockPath, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	closeScope()

	code := runBuildStop(nil, env)
	if code != ExitOK {
		t.Fatalf("runBuildStop = %d, want 0", code)
	}

	// The wrapper was invoked with --stop.
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("wrapper not invoked (marker missing): %v", err)
	}
	if !strings.Contains(string(data), "args=--stop") {
		t.Errorf("wrapper args = %q, want --stop", string(data))
	}
	// GRADLE_USER_HOME pointed at the leaf (not host ~/.gradle).
	if !strings.Contains(string(data), "GUH="+leaf) {
		t.Errorf("wrapper GRADLE_USER_HOME = %q, want leaf %q", string(data), leaf)
	}
	// The lingering lockfile was removed.
	if _, err := os.Stat(lockPath); err == nil {
		t.Errorf("lockfile %s must be removed by build stop", lockPath)
	}
}

// TestRunBuildStop_MissingWrapperDenied asserts `omac build stop` denies
// (exit 3) when no repo wrapper exists, without touching Gradle.
func TestRunBuildStop_MissingWrapperDenied(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	wt := t.TempDir()
	env := &Env{
		Version: "test",
		Workdir: wt,
		Stdout:  newDevNull(t),
		Stderr:  newCapture(t),
	}
	code := runBuildStop(nil, env)
	if code != ExitBuildPolicyDenied {
		t.Errorf("runBuildStop without wrapper = %d, want %d", code, ExitBuildPolicyDenied)
	}
}

// TestRunBuildStop_HonorsRootFlag asserts `omac build stop --root backend`
// resolves the wrapper at <worktree>/backend/gradlew, NOT at the worktree
// root. This is the ticket-04 host bug: the hardcoded "." resolved the
// wrapper at the worktree root, failing with "no repository-owned gradlew
// at <worktree>/gradlew" when the wrapper lived under backend/.
func TestRunBuildStop_HonorsRootFlag(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	wt := t.TempDir()

	// Wrapper under backend/, NOT at the worktree root. A marker records
	// which wrapper actually ran.
	backend := filepath.Join(wt, "backend")
	if err := os.MkdirAll(backend, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(wt, "stop-marker")
	wrapper := "#!/bin/sh\n" +
		"echo \"ran=backend-wrapper args=$*\" >> " + marker + "\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(backend, "gradlew"), []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	// A decoy wrapper at the worktree root that MUST NOT run — if the
	// old hardcoded "." bug is present, this runs instead and the marker
	// records "root-wrapper".
	decoy := "#!/bin/sh\necho \"ran=root-wrapper args=$*\" >> " + marker + "\nexit 0\n"
	if err := os.WriteFile(filepath.Join(wt, "gradlew"), []byte(decoy), 0o755); err != nil {
		t.Fatal(err)
	}

	env := &Env{
		Version: "test",
		Workdir: wt,
		Stdout:  newDevNull(t),
		Stderr:  newCapture(t),
	}

	// Pre-create the leaf so the lockfile removal path doesn't error.
	cacheDir, closeScope, err := prepareBuildCache(wt, "")
	if err != nil {
		t.Fatalf("prepareBuildCache: %v", err)
	}
	leaf := filepath.Join(cacheDir, "gradle")
	if err := os.MkdirAll(leaf, 0o700); err != nil {
		t.Fatal(err)
	}
	closeScope()

	code := runBuildStop([]string{"--root", "backend"}, env)
	if code != ExitOK {
		t.Fatalf("runBuildStop --root backend = %d, want 0", code)
	}
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("wrapper not invoked (marker missing): %v", err)
	}
	s := string(data)
	if !strings.Contains(s, "ran=backend-wrapper") {
		t.Errorf("expected the backend/ wrapper to run; marker=%q", s)
	}
	if strings.Contains(s, "ran=root-wrapper") {
		t.Errorf("the worktree-root decoy wrapper ran (--root backend ignored): marker=%q", s)
	}
	if !strings.Contains(s, "args=--stop") {
		t.Errorf("wrapper args = %q, want --stop", s)
	}
}

// TestRunBuildStop_RootEqualsForm accepts --root=<rel> as well as the
// space-separated form.
func TestRunBuildStop_RootEqualsForm(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	wt := t.TempDir()
	backend := filepath.Join(wt, "backend")
	if err := os.MkdirAll(backend, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(wt, "stop-marker")
	wrapper := "#!/bin/sh\necho \"ran=ok args=$*\" >> " + marker + "\nexit 0\n"
	if err := os.WriteFile(filepath.Join(backend, "gradlew"), []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	cacheDir, closeScope, err := prepareBuildCache(wt, "")
	if err != nil {
		t.Fatalf("prepareBuildCache: %v", err)
	}
	leaf := filepath.Join(cacheDir, "gradle")
	if err := os.MkdirAll(leaf, 0o700); err != nil {
		t.Fatal(err)
	}
	closeScope()

	env := &Env{Version: "test", Workdir: wt, Stdout: newDevNull(t), Stderr: newCapture(t)}
	code := runBuildStop([]string{"--root=backend"}, env)
	if code != ExitOK {
		t.Fatalf("runBuildStop --root=backend = %d, want 0", code)
	}
	data, _ := os.ReadFile(marker)
	if !strings.Contains(string(data), "ran=ok") {
		t.Errorf("backend wrapper did not run via --root=backend: %q", string(data))
	}
}

// TestRunBuildStop_UnknownFlagDenied: an unrecognized flag is a policy
// denial (exit 3), mirroring `omac build`.
func TestRunBuildStop_UnknownFlagDenied(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	wt := t.TempDir()
	env := &Env{Version: "test", Workdir: wt, Stdout: newDevNull(t), Stderr: newCapture(t)}
	code := runBuildStop([]string{"--bogus"}, env)
	if code != ExitBuildPolicyDenied {
		t.Errorf("runBuildStop --bogus = %d, want %d (policy denial)", code, ExitBuildPolicyDenied)
	}
}

// TestRunBuildStop_RootMissingWrapperDenied: --root pointing at a dir
// without a gradlew is a policy denial (the Resolve step fails), NOT a
// fall-through to the worktree root.
func TestRunBuildStop_RootMissingWrapperDenied(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	wt := t.TempDir()
	backend := filepath.Join(wt, "backend")
	if err := os.MkdirAll(backend, 0o755); err != nil {
		t.Fatal(err)
	}
	// No gradlew under backend/. A decoy at the root must NOT be used.
	if err := os.WriteFile(filepath.Join(wt, "gradlew"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	env := &Env{Version: "test", Workdir: wt, Stdout: newDevNull(t), Stderr: newCapture(t)}
	code := runBuildStop([]string{"--root", "backend"}, env)
	if code != ExitBuildPolicyDenied {
		t.Errorf("runBuildStop --root backend (no wrapper) = %d, want %d", code, ExitBuildPolicyDenied)
	}
}

// TestRunBuildStop_HelpExitsZero asserts the --help path prints usage.
func TestRunBuildStop_HelpExitsZero(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	wt := t.TempDir()
	cap := newCapture(t)
	env := &Env{Version: "test", Workdir: wt, Stdout: newDevNull(t), Stderr: cap}
	code := runBuildStop([]string{"--help"}, env)
	if code != ExitOK {
		t.Errorf("runBuildStop --help = %d, want 0", code)
	}
	_ = cap.Sync()
	out, _ := os.ReadFile(cap.Name())
	if !strings.Contains(string(out), "omac build stop") {
		t.Errorf("help text missing 'omac build stop': %q", out)
	}
}
