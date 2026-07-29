package buildrun

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/tngtech/oh-my-agentic-coder/internal/audit"
)

// testGrants builds a minimal, un-sandboxed grant set around a temp
// worktree for Run tests that use an injected launcher.
func testRunGrants(t *testing.T) *BuildGrants {
	t.Helper()
	wt, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cacheDir := filepath.Join(t.TempDir(), "cache")
	g, err := GrantsFor(wt, cacheDir)
	if err != nil {
		t.Fatalf("GrantsFor: %v", err)
	}
	t.Cleanup(g.CleanupTmp)
	return g
}

// TestNoSandboxLauncher pins the exported test adapter: it must pass the
// inner argv through untouched (same contract the old test-local
// NoSandboxLauncher had).
func TestNoSandboxLauncher(t *testing.T) {
	inner := []string{"/bin/echo", "hi"}
	got, err := NoSandboxLauncher(&BuildGrants{}, inner)
	if err != nil {
		t.Fatalf("NoSandboxLauncher: %v", err)
	}
	if len(got) != len(inner) {
		t.Fatalf("NoSandboxLauncher returned %d args, want %d: %v", len(got), len(inner), got)
	}
	for i := range inner {
		if got[i] != inner[i] {
			t.Errorf("arg %d = %q, want %q", i, got[i], inner[i])
		}
	}
}

func TestRunBuildStreamsOutput(t *testing.T) {
	g := testRunGrants(t)
	res := Resolved{
		Worktree:   g.Workdir,
		ProjectDir: g.Workdir,
		Wrapper:    "/bin/sh",
		Args:       []string{"-c", "echo out; echo err >&2"},
	}
	var stdout, stderr bytes.Buffer
	exit, err := RunBuild(RunOptions{
		Resolved: res,
		Grants:   g,
		Stdout:   &stdout,
		Stderr:   &stderr,
		Launcher: NoSandboxLauncher,
		Auditor:  audit.Nop(),
	})
	if err != nil || exit != 0 {
		t.Fatalf("RunBuild = (%d, %v)", exit, err)
	}
	if got := stdout.String(); got != "out\n" {
		t.Errorf("stdout = %q, want %q", got, "out\n")
	}
	if got := stderr.String(); got != "err\n" {
		t.Errorf("stderr = %q, want %q", got, "err\n")
	}
}

func TestRunBuildPropagatesExitCode(t *testing.T) {
	g := testRunGrants(t)
	res := Resolved{
		Worktree:   g.Workdir,
		ProjectDir: g.Workdir,
		Wrapper:    "/bin/sh",
		Args:       []string{"-c", "exit 42"},
	}
	exit, err := RunBuild(RunOptions{
		Resolved: res, Grants: g,
		Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{},
		Launcher: NoSandboxLauncher, Auditor: audit.Nop(),
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if exit != 42 {
		t.Errorf("exit = %d, want 42", exit)
	}
}

func TestRunBuildSetsGradleUserHomeEnv(t *testing.T) {
	g := testRunGrants(t)
	res := Resolved{
		Worktree:   g.Workdir,
		ProjectDir: g.Workdir,
		Wrapper:    "/bin/sh",
		Args:       []string{"-c", `printf '%s' "$GRADLE_USER_HOME"`},
	}
	var stdout bytes.Buffer
	exit, err := RunBuild(RunOptions{
		Resolved: res, Grants: g,
		Stdout: &stdout, Stderr: &bytes.Buffer{},
		Launcher: NoSandboxLauncher, Auditor: audit.Nop(),
	})
	if err != nil || exit != 0 {
		t.Fatalf("RunBuild = (%d, %v)", exit, err)
	}
	if stdout.String() != g.GradleUserHome() {
		t.Errorf("GRADLE_USER_HOME = %q, want %q", stdout.String(), g.GradleUserHome())
	}
}

func TestRunBuildWorkingDirectoryIsProjectRoot(t *testing.T) {
	g := testRunGrants(t)
	backend := filepath.Join(g.Workdir, "backend")
	if err := os.MkdirAll(backend, 0o755); err != nil {
		t.Fatal(err)
	}
	res := Resolved{
		Worktree:   g.Workdir,
		ProjectDir: backend,
		Wrapper:    "/bin/sh",
		Args:       []string{"-c", "pwd -P"},
	}
	var stdout bytes.Buffer
	exit, err := RunBuild(RunOptions{
		Resolved: res, Grants: g,
		Stdout: &stdout, Stderr: &bytes.Buffer{},
		Launcher: NoSandboxLauncher, Auditor: audit.Nop(),
	})
	if err != nil || exit != 0 {
		t.Fatalf("RunBuild = (%d, %v)", exit, err)
	}
	if got := strings.TrimSpace(stdout.String()); got != backend {
		t.Errorf("pwd = %q, want %q", got, backend)
	}
}

func TestRunBuildCancellationKillsChild(t *testing.T) {
	g := testRunGrants(t)
	res := Resolved{
		Worktree:   g.Workdir,
		ProjectDir: g.Workdir,
		Wrapper:    "/bin/sh",
		// Child ignores SIGTERM: exercises the graceful->SIGKILL staging.
		Args: []string{"-c", "trap '' TERM INT; sleep 30"},
	}
	cancel := make(chan struct{})
	close(cancel) // cancel immediately once the child is running
	start := time.Now()
	exit, err := RunBuild(RunOptions{
		Resolved: res, Grants: g,
		Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{},
		Launcher:  NoSandboxLauncher,
		Auditor:   audit.Nop(),
		Cancel:    cancel,
		KillAfter: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("cancellation took %v; staged kill must bound the wait", d)
	}
	if exit != ExitCancelled {
		t.Errorf("exit = %d, want ExitCancelled (%d)", exit, ExitCancelled)
	}
}

// recordingGroupKill wraps the REAL syscall signal delivery and records
// the SIGKILL attempts only — the test needs the child to actually die
// from the graceful SIGTERM, so the graceful stage must reach the real
// process group; only the hard stage is observed (asserted never
// reached).
type recordingGroupKill struct {
	killed []int
}

func (r *recordingGroupKill) kill(pid int, sig syscall.Signal) error {
	if sig == syscall.SIGKILL {
		r.killed = append(r.killed, pid)
		return nil // observed, not delivered
	}
	return syscall.Kill(pid, sig)
}

func TestRunBuildGracefulChildSkipsSIGKILL(t *testing.T) {
	// P5 race guard: a child that honors the graceful SIGTERM and exits
	// inside the window must NOT trigger the hard-stage group kill — the
	// pid is reaped, and kill(-pid, 9) afterwards could SIGKILL an
	// unrelated process group that recycled the pgid.
	g := testRunGrants(t)
	res := Resolved{
		Worktree:   g.Workdir,
		ProjectDir: g.Workdir,
		Wrapper:    "/bin/sh",
		// Dies on SIGTERM (default disposition) within milliseconds.
		Args: []string{"-c", "sleep 30"},
	}
	cancel := make(chan struct{})
	kill := &recordingGroupKill{}
	done := make(chan struct{})
	var exit int
	var runErr error
	go func() {
		exit, runErr = RunBuild(RunOptions{
			Resolved: res, Grants: g,
			Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{},
			Launcher:    NoSandboxLauncher,
			Auditor:     audit.Nop(),
			Cancel:      cancel,
			KillAfter:   2 * time.Second,
			GroupSignal: kill.kill,
		})
		close(done)
	}()
	// Cancel only after the child had time to start: a pre-start close
	// races the select (case order) and could skip the kill staging
	// entirely.
	time.Sleep(300 * time.Millisecond)
	close(cancel)
	<-done
	if runErr != nil {
		t.Fatalf("err = %v", runErr)
	}
	if exit != ExitCancelled {
		t.Errorf("exit = %d, want ExitCancelled (%d)", exit, ExitCancelled)
	}
	if len(kill.killed) > 0 {
		t.Errorf("SIGKILL sent to group although the child exited gracefully (pgid recycling hazard): pids=%v", kill.killed)
	}
}

func TestSignalContextSecondSignalStillCancels(t *testing.T) {
	// P6: the hard-exit on a second signal must not bypass deferred
	// cleanup (CleanupTmp). The contract: a second signal only forces the
	// cancel channel closed (the graceful window collapses to 0 via
	// options.KillAfter), so runBuild returns through its normal defer
	// chain instead of os.Exit-ing mid-cleanup.
	cancel, second, release := SignalContext()
	defer release()
	select {
	case <-cancel:
		t.Fatal("cancel closed before any signal")
	default:
	}
	second <- syscall.SIGINT
	select {
	case <-cancel:
	case <-time.After(2 * time.Second):
		t.Fatal("cancel must close on the first signal")
	}
	// The second signal forces urgency but never os.Exits: the watcher
	// returns silently, the caller unwinds through its normal defer
	// chain (CleanupTmp + friends) — no observable process exit here.
	second <- syscall.SIGTERM
}

func TestRunBuildCancellationMarker(t *testing.T) {
	// rc==4 must be distinguishable from a raw `gradle exit 4`: the
	// cancellation path prints the omac-prefixed marker to stderr BEFORE
	// the code is returned.
	g := testRunGrants(t)
	res := Resolved{
		Worktree:   g.Workdir,
		ProjectDir: g.Workdir,
		Wrapper:    "/bin/sh",
		Args:       []string{"-c", "sleep 30"},
	}
	var stderr bytes.Buffer
	cancel := make(chan struct{})
	close(cancel)
	exit, err := RunBuild(RunOptions{
		Resolved: res, Grants: g,
		Stdout: &bytes.Buffer{}, Stderr: &stderr,
		Launcher:  NoSandboxLauncher,
		Auditor:   audit.Nop(),
		Cancel:    cancel,
		KillAfter: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if exit != ExitCancelled {
		t.Errorf("exit = %d, want ExitCancelled (%d)", exit, ExitCancelled)
	}
	if !strings.Contains(stderr.String(), CancelledMarker) {
		t.Errorf("stderr = %q, want cancellation marker %q", stderr.String(), CancelledMarker)
	}
}
