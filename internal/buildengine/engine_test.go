package buildengine

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/tngtech/oh-my-agentic-coder/internal/audit"
	"github.com/tngtech/oh-my-agentic-coder/internal/buildmanifest"
	"github.com/tngtech/oh-my-agentic-coder/internal/buildrun"
)

// engineTestEnv builds the on-disk fixtures the engine needs for a
// direct-host-style Run: a worktree with an executable gradlew, a
// resolved cache dir, and a closeScope func. It returns the worktree,
// cache dir, and closeScope. The wrapper is a stub shell the test
// supplies.
func engineTestEnv(t *testing.T, wrapper string) (worktree, cacheDir string, closeScope func()) {
	t.Helper()
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	wt := t.TempDir()
	if err := os.WriteFile(filepath.Join(wt, "gradlew"), []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	// Prepare the cache scope the same way the CLI does so the engine
	// resolves the same leaf. We use a temp cache home and the shared
	// scope (the default).
	cd, cs, err := prepareTestCacheScope(wt)
	if err != nil {
		t.Fatalf("prepare cache scope: %v", err)
	}
	leaf := filepath.Join(cd, "gradle")
	if err := os.MkdirAll(leaf, 0o700); err != nil {
		t.Fatal(err)
	}
	// Restore init.d writability for t.TempDir's RemoveAll.
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(leaf, "init.d"), 0o755) })
	return wt, cd, cs
}

// prepareTestCacheScope mirrors cli.prepareBuildCache without importing
// internal/cli (which would be a cycle). It uses the shared scope under
// the isolated HOME so the engine's buildrun.GrantsFor resolves the
// same leaf layout.
func prepareTestCacheScope(workdir string) (string, func(), error) {
	// Reuse the same toolcache.PrepareShared path the CLI's default
	// global scope uses, rooted at the isolated HOME.
	return prepareSharedCacheScope(workdir)
}

// fakeSnapshotProvider returns a zero PolicySnapshot (no manifest) so
// the engine skips the gate and proceeds with an empty capability set.
// This is the normal case for a standard Gradle project with no
// .omac/build.yaml.
func fakeSnapshotProvider(worktree, leaf string, req buildrun.Request) (PolicySnapshot, error) {
	return PolicySnapshot{HostPolicy: buildmanifest.HostPolicy{MaxHeap: "2g"}}, nil
}

// fakeProxyStarter returns three disabled handles — no proxies started
// (the engine tests don't exercise the proxy path; they assert
// classification, not proxy wiring).
func fakeProxyStarter(env *ProxyEnv) (filtered ProxyHandle, credential CredentialProxyHandle, container ContainerProxyHandle, err error) {
	return ProxyHandle{}, CredentialProxyHandle{}, ContainerProxyHandle{}, nil
}

// TestRun_ClassifiesRawWrapperExitsAsBuildFailure is the engine-level
// test the spec calls out (§Verification Strategy / Build engine): raw
// wrapper exits 3, 4, and 10 are classified as build_failure, NOT as
// OMAC outcomes (policy_denial / cancelled / service_failure). The
// class carries the disambiguation reserved numeric codes cannot.
//
// The engine uses buildrun.NoSandboxLauncher so the test runs without
// applying a kernel sandbox (nested sandboxes cannot apply a profile).
// A stub wrapper exits with the reserved code; the engine must assign
// ClassBuildFailure and pass the raw code through in Result.Exit.
func TestRun_ClassifiesRawWrapperExitsAsBuildFailure(t *testing.T) {
	for _, exit := range []int{3, 4, 10} {
		name := "exit_" + strconv.Itoa(exit)
		t.Run(name, func(t *testing.T) {
			wrapper := "#!/bin/sh\nexit " + strconv.Itoa(exit) + "\n"
			wt, cacheDir, closeScope := engineTestEnv(t, wrapper)
			var stderr bytes.Buffer
			res := Run(Options{
				Workdir:    wt,
				RawArgs:    []string{"--root", ".", "--", "gradle", ":help"},
				Stdout:     io.Discard,
				Stderr:     &stderr,
				CacheDir:   cacheDir,
				CloseScope: closeScope,
				Auditor:    audit.Nop(),
				Snapshot:   fakeSnapshotProvider,
				Proxies:    fakeProxyStarter,
				Launcher:   buildrun.NoSandboxLauncher,
			})
			if res.Class != ClassBuildFailure {
				t.Errorf("exit %d: class = %q, want %q (raw wrapper exit must NOT be inferred as an OMAC outcome)", exit, res.Class, ClassBuildFailure)
			}
			if res.Exit != exit {
				t.Errorf("exit %d: Result.Exit = %d, want %d (raw wrapper exit passed through)", exit, res.Exit, exit)
			}
			if res.ExitCode() != exit {
				t.Errorf("exit %d: ExitCode() = %d, want %d", exit, res.ExitCode(), exit)
			}
			// The cancellation marker must NOT appear for a raw wrapper
			// exit 4 (only the engine's cancelled path prints it).
			if exit == 4 && strings.Contains(stderr.String(), buildrun.CancelledMarker) {
				t.Errorf("exit 4: stderr must NOT carry the cancellation marker (raw wrapper exit, not an OMAC cancellation):\n%s", stderr.String())
			}
		})
	}
}

// TestRun_SuccessClassifiesAsSuccess asserts a wrapper exit 0 yields
// ClassSuccess with Exit 0 and ExitCode 0.
func TestRun_SuccessClassifiesAsSuccess(t *testing.T) {
	wrapper := "#!/bin/sh\necho hi\nexit 0\n"
	wt, cacheDir, closeScope := engineTestEnv(t, wrapper)
	var stdout bytes.Buffer
	res := Run(Options{
		Workdir:    wt,
		RawArgs:    []string{"--root", ".", "--", "gradle", ":help"},
		Stdout:     &stdout,
		Stderr:     io.Discard,
		CacheDir:   cacheDir,
		CloseScope: closeScope,
		Auditor:    audit.Nop(),
		Snapshot:   fakeSnapshotProvider,
		Proxies:    fakeProxyStarter,
		Launcher:   buildrun.NoSandboxLauncher,
	})
	if res.Class != ClassSuccess {
		t.Errorf("class = %q, want %q", res.Class, ClassSuccess)
	}
	if res.Exit != 0 || res.ExitCode() != 0 {
		t.Errorf("Exit = %d, ExitCode = %d, want 0/0", res.Exit, res.ExitCode())
	}
	if !strings.Contains(stdout.String(), "hi") {
		t.Errorf("stdout = %q, want it to contain the wrapper's output", stdout.String())
	}
}

// TestRun_PolicyDenialClassifiesAsPolicyDenial asserts a grammar error
// (missing separator) yields ClassPolicyDenial with Exit 3.
func TestRun_PolicyDenialClassifiesAsPolicyDenial(t *testing.T) {
	wt, cacheDir, closeScope := engineTestEnv(t, "#!/bin/sh\nexit 0\n")
	res := Run(Options{
		Workdir:    wt,
		RawArgs:    []string{"--root", "."}, // missing `-- gradle ...`
		Stdout:     io.Discard,
		Stderr:     io.Discard,
		CacheDir:   cacheDir,
		CloseScope: closeScope,
		Auditor:    audit.Nop(),
		Snapshot:   fakeSnapshotProvider,
		Proxies:    fakeProxyStarter,
		Launcher:   buildrun.NoSandboxLauncher,
	})
	if res.Class != ClassPolicyDenial {
		t.Errorf("class = %q, want %q", res.Class, ClassPolicyDenial)
	}
	if res.Exit != 3 || res.ExitCode() != 3 {
		t.Errorf("Exit = %d, ExitCode = %d, want 3/3", res.Exit, res.ExitCode())
	}
}

// TestRun_GateErrorClassifiesAsPolicyDenial asserts a *GateError from
// the snapshot provider yields ClassPolicyDenial (NOT service failure)
// and prints the consolidated diff + restart instruction to stderr.
func TestRun_GateErrorClassifiesAsPolicyDenial(t *testing.T) {
	wt, cacheDir, closeScope := engineTestEnv(t, "#!/bin/sh\nexit 0\n")
	// Write a manifest so HasManifest is true and the gate runs.
	if err := os.MkdirAll(filepath.Join(wt, ".omac"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".omac", "build.yaml"), []byte("version: 1\nbuilds:\n  - root: .\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	res := Run(Options{
		Workdir:    wt,
		RawArgs:    []string{"--root", ".", "--", "gradle", ":help"},
		Stdout:     io.Discard,
		Stderr:     &stderr,
		CacheDir:   cacheDir,
		CloseScope: closeScope,
		Auditor:    audit.Nop(),
		// DirectSnapshotProvider will load the manifest and run the
		// gate, which fails with a *GateError (no prior approval).
		Snapshot: DirectSnapshotProvider,
		Proxies:  fakeProxyStarter,
		Launcher: buildrun.NoSandboxLauncher,
	})
	if res.Class != ClassPolicyDenial {
		t.Errorf("class = %q, want %q (GateError is a policy denial)\nstderr:\n%s", res.Class, ClassPolicyDenial, stderr.String())
	}
	if res.Exit != 3 || res.ExitCode() != 3 {
		t.Errorf("Exit = %d, ExitCode = %d, want 3/3", res.Exit, res.ExitCode())
	}
	if !strings.Contains(stderr.String(), "manifest approval required") {
		t.Errorf("stderr must carry the approval-required diagnostic:\n%s", stderr.String())
	}
}

// TestRun_ManifestErrorClassifiesAsPolicyDenial asserts a
// *ManifestError (e.g. secret field in the manifest) from the snapshot
// provider yields ClassPolicyDenial.
func TestRun_ManifestErrorClassifiesAsPolicyDenial(t *testing.T) {
	wt, cacheDir, closeScope := engineTestEnv(t, "#!/bin/sh\nexit 0\n")
	if err := os.MkdirAll(filepath.Join(wt, ".omac"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".omac", "build.yaml"), []byte("version: 1\nbuilds:\n  - root: .\nregistries:\n  - alias: internal\n    upstream: https://maven.internal/repo\n    password: hunter2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	res := Run(Options{
		Workdir:    wt,
		RawArgs:    []string{"--root", ".", "--", "gradle", ":help"},
		Stdout:     io.Discard,
		Stderr:     &stderr,
		CacheDir:   cacheDir,
		CloseScope: closeScope,
		Auditor:    audit.Nop(),
		Snapshot:   DirectSnapshotProvider,
		Proxies:    fakeProxyStarter,
		Launcher:   buildrun.NoSandboxLauncher,
	})
	if res.Class != ClassPolicyDenial {
		t.Errorf("class = %q, want %q (ManifestError is a policy denial)\nstderr:\n%s", res.Class, ClassPolicyDenial, stderr.String())
	}
	if res.Exit != 3 || res.ExitCode() != 3 {
		t.Errorf("Exit = %d, ExitCode = %d, want 3/3", res.Exit, res.ExitCode())
	}
}

// TestRun_ParentOwnedSnapshot_DigestMismatchIsPolicyDenial simulates the
// broker path's snapshot adapter (ticket 04's second adapter): the
// parent owns an in-memory snapshot frozen at activation, and a build
// request compares the reloaded manifest's digest against it. A
// mismatch (the worktree manifest changed after the snapshot was
// frozen) is a policy denial — the engine cannot advance or replace
// the snapshot. The provider NEVER writes approvals (unlike the direct
// adapter, which calls buildmanifest.Gate that records approval on
// first use).
//
// This exercises the second adapter the spec calls for ("A narrow
// snapshot-provider seam has two real adapters") at the engine level,
// proving the engine consumes a parent-owned snapshot correctly and
// treats a digest mismatch as a policy denial without writing.
func TestRun_ParentOwnedSnapshot_DigestMismatchIsPolicyDenial(t *testing.T) {
	wt, cacheDir, closeScope := engineTestEnv(t, "#!/bin/sh\nexit 0\n")
	if err := os.MkdirAll(filepath.Join(wt, ".omac"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".omac", "build.yaml"), []byte("version: 1\nbuilds:\n  - root: .\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A parent-owned snapshot with a digest that does NOT match the
	// worktree manifest (simulating a frozen-then-changed manifest, or
	// a stale snapshot the broker refuses to advance).
	staleSnapshot := func(worktree, leaf string, req buildrun.Request) (PolicySnapshot, error) {
		return PolicySnapshot{
			Digest:       "0000000000000000000000000000000000000000000000000000000000000000",
			Capabilities: buildmanifest.CapabilitySet{},
			HostPolicy:   buildmanifest.HostPolicy{MaxHeap: "2g"},
		}, nil
	}
	var stderr bytes.Buffer
	res := Run(Options{
		Workdir:    wt,
		RawArgs:    []string{"--root", ".", "--", "gradle", ":help"},
		Stdout:     io.Discard,
		Stderr:     &stderr,
		CacheDir:   cacheDir,
		CloseScope: closeScope,
		Auditor:    audit.Nop(),
		Snapshot:   staleSnapshot,
		Proxies:    fakeProxyStarter,
		Launcher:   buildrun.NoSandboxLauncher,
	})
	if res.Class != ClassPolicyDenial {
		t.Errorf("class = %q, want %q (parent-owned snapshot digest mismatch is a policy denial)\nstderr:\n%s", res.Class, ClassPolicyDenial, stderr.String())
	}
	if res.ExitCode() != 3 {
		t.Errorf("ExitCode = %d, want 3", res.ExitCode())
	}
}

// TestResultExitCodeMapping pins the Result.ExitCode translation for
// every class, so the CLI exit-code translator and the broker result
// frame can rely on it without re-deriving.
func TestResultExitCodeMapping(t *testing.T) {
	cases := []struct {
		class ResultClass
		exit  int
		want  int
	}{
		{ClassSuccess, 0, 0},
		{ClassBuildFailure, 1, 1},
		{ClassBuildFailure, 3, 3},
		{ClassBuildFailure, 4, 4},
		{ClassBuildFailure, 10, 10},
		{ClassBuildFailure, 130, 130},
		{ClassPolicyDenial, 3, 3},
		{ClassCancelled, 4, 4},
		{ClassServiceFailure, 10, 10},
	}
	for _, c := range cases {
		r := Result{Class: c.class, Exit: c.exit}
		if got := r.ExitCode(); got != c.want {
			t.Errorf("%q exit=%d: ExitCode() = %d, want %d", c.class, c.exit, got, c.want)
		}
	}
}

// TestStop_MissingWrapperDenied asserts Stop yields ClassPolicyDenial
// when no repo wrapper exists.
func TestStop_MissingWrapperDenied(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	wt := t.TempDir()
	cd, cs, err := prepareTestCacheScope(wt)
	if err != nil {
		t.Fatalf("prepare cache scope: %v", err)
	}
	leaf := filepath.Join(cd, "gradle")
	if err := os.MkdirAll(leaf, 0o700); err != nil {
		t.Fatal(err)
	}
	res := Stop(StopOptions{
		Workdir:    wt,
		RawArgs:    nil,
		Stdout:     io.Discard,
		Stderr:     io.Discard,
		CacheDir:   cd,
		CloseScope: cs,
		Auditor:    audit.Nop(),
	})
	if res.Class != ClassPolicyDenial {
		t.Errorf("class = %q, want %q", res.Class, ClassPolicyDenial)
	}
	if res.ExitCode() != 3 {
		t.Errorf("ExitCode = %d, want 3", res.ExitCode())
	}
}

// TestStop_UnknownFlagDenied asserts an unrecognized flag yields
// ClassPolicyDenial.
func TestStop_UnknownFlagDenied(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	wt := t.TempDir()
	cd, cs, err := prepareTestCacheScope(wt)
	if err != nil {
		t.Fatalf("prepare cache scope: %v", err)
	}
	res := Stop(StopOptions{
		Workdir:    wt,
		RawArgs:    []string{"--bogus"},
		Stdout:     io.Discard,
		Stderr:     io.Discard,
		CacheDir:   cd,
		CloseScope: cs,
		Auditor:    audit.Nop(),
	})
	if res.Class != ClassPolicyDenial {
		t.Errorf("class = %q, want %q", res.Class, ClassPolicyDenial)
	}
	if res.ExitCode() != 3 {
		t.Errorf("ExitCode = %d, want 3", res.ExitCode())
	}
}

// TestStop_StopWrapperExitPassesThroughAsBuildFailure asserts a non-zero
// `gradlew --stop` exit passes through as ClassBuildFailure with the
// wrapper's exit code (NOT a service failure).
func TestStop_StopWrapperExitPassesThroughAsBuildFailure(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	wt := t.TempDir()
	if err := os.WriteFile(filepath.Join(wt, "gradlew"), []byte("#!/bin/sh\nexit 7\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cd, cs, err := prepareTestCacheScope(wt)
	if err != nil {
		t.Fatalf("prepare cache scope: %v", err)
	}
	leaf := filepath.Join(cd, "gradle")
	if err := os.MkdirAll(leaf, 0o700); err != nil {
		t.Fatal(err)
	}
	// GrantsFor creates init.d read-only (0o500); restore writability
	// so t.TempDir's RemoveAll can unlink the always-written control
	// scripts inside it.
	chmodInitDForCleanup(t, leaf)
	res := Stop(StopOptions{
		Workdir:    wt,
		RawArgs:    nil,
		Stdout:     io.Discard,
		Stderr:     io.Discard,
		CacheDir:   cd,
		CloseScope: cs,
		Auditor:    audit.Nop(),
	})
	if res.Class != ClassBuildFailure {
		t.Errorf("class = %q, want %q (raw --stop exit passes through)", res.Class, ClassBuildFailure)
	}
	if res.Exit != 7 || res.ExitCode() != 7 {
		t.Errorf("Exit = %d, ExitCode = %d, want 7/7", res.Exit, res.ExitCode())
	}
}

// chmodInitDForCleanup restores init.d writability under the resolved
// build cache leaf so t.TempDir's RemoveAll can unlink the always-written
// control scripts (GrantsFor creates init.d read-only 0o500 to keep
// build code from planting an init script). Mirrors the cli test helper
// of the same name.
func chmodInitDForCleanup(t *testing.T, leaf string) {
	t.Helper()
	t.Cleanup(func() {
		_ = os.Chmod(filepath.Join(leaf, "init.d"), 0o755)
	})
}
