package buildrun

import (
	"bytes"
	"errors"
	"fmt"
	"io"
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
	g, err := GrantsFor(wt, cacheDir, BuildConfig{})
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
	cancel, force, second, release := SignalContext()
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
	select {
	case <-force:
	case <-time.After(2 * time.Second):
		t.Fatal("force must close on the second signal")
	}
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

// TestRunBuildForcedCancelCollapsesKillAfter asserts that a closed
// ForceCancel collapses the graceful window: a child that ignores SIGTERM
// (would normally wait the full KillAfter) is SIGKILLed ~immediately when
// the force channel closes.
func TestRunBuildForcedCancelCollapsesKillAfter(t *testing.T) {
	g := testRunGrants(t)
	res := Resolved{
		Worktree:   g.Workdir,
		ProjectDir: g.Workdir,
		Wrapper:    "/bin/sh",
		// Ignores SIGTERM so only the forced SIGKILL ends it.
		Args: []string{"-c", "trap '' TERM INT; sleep 30"},
	}
	cancel := make(chan struct{})
	force := make(chan struct{})
	start := time.Now()
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
			ForceCancel: force,
			// Long graceful window: without force the child would wait the
			// full 5s; with force it must die in ~forcedKillAfter.
			KillAfter: 5 * time.Second,
		})
		close(done)
	}()
	// Trigger graceful cancel, then force after a beat so the SIGKILL
	// collapses the 5s window.
	time.Sleep(200 * time.Millisecond)
	close(cancel)
	time.Sleep(200 * time.Millisecond)
	close(force)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("RunBuild did not return after forced cancel")
	}
	if runErr != nil {
		t.Fatalf("err = %v", runErr)
	}
	if exit != ExitCancelled {
		t.Errorf("exit = %d, want ExitCancelled (%d)", exit, ExitCancelled)
	}
	d := time.Since(start)
	if d > 2*time.Second {
		t.Errorf("forced cancel took %v; ForceCancel must collapse KillAfter to ~%v", d, forcedKillAfter)
	}
}

// TestRunBuildMaxDurationCancel asserts the build-duration ceiling cancels
// a long-running build as if the caller signalled.
func TestRunBuildMaxDurationCancel(t *testing.T) {
	g := testRunGrants(t)
	res := Resolved{
		Worktree:   g.Workdir,
		ProjectDir: g.Workdir,
		Wrapper:    "/bin/sh",
		Args:       []string{"-c", "sleep 30"},
	}
	cancel := make(chan struct{}) // not closed; MaxDuration drives the cancel
	exit, err := RunBuild(RunOptions{
		Resolved: res, Grants: g,
		Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{},
		Launcher:    NoSandboxLauncher,
		Auditor:     audit.Nop(),
		Cancel:      cancel,
		KillAfter:   200 * time.Millisecond,
		MaxDuration: 300 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if exit != ExitCancelled {
		t.Errorf("exit = %d, want ExitCancelled (%d)", exit, ExitCancelled)
	}
}

// TestRunBuildForcedCancelRecyclesDaemon asserts S3 (spec.md:144): a
// FORCED cancel (ForceCancel fires) recycles the (potentially corrupt)
// Gradle daemon by invoking OnForcedCancel after the gradlew group is
// SIGKILLed. A GRACEFUL cancel (first signal only, no force) must NOT
// invoke OnForcedCancel — the warm daemon is preserved per spec.
func TestRunBuildForcedCancelRecyclesDaemon(t *testing.T) {
	g := testRunGrants(t)
	res := Resolved{
		Worktree:   g.Workdir,
		ProjectDir: g.Workdir,
		Wrapper:    "/bin/sh",
		// Ignores SIGTERM so only the forced SIGKILL ends it.
		Args: []string{"-c", "trap '' TERM INT; sleep 30"},
	}
	stops := make(chan struct{}, 4)
	onForce := func(stderr io.Writer) error {
		stops <- struct{}{}
		return nil
	}
	cancel := make(chan struct{})
	force := make(chan struct{})
	done := make(chan struct{})
	var exit int
	go func() {
		exit, _ = RunBuild(RunOptions{
			Resolved:       res,
			Grants:         g,
			Stdout:         &bytes.Buffer{},
			Stderr:         &bytes.Buffer{},
			Launcher:       NoSandboxLauncher,
			Auditor:        audit.Nop(),
			Cancel:         cancel,
			ForceCancel:    force,
			KillAfter:      5 * time.Second,
			OnForcedCancel: onForce,
		})
		close(done)
	}()
	time.Sleep(200 * time.Millisecond)
	close(cancel) // graceful cancel
	time.Sleep(200 * time.Millisecond)
	close(force) // forced cancel
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("RunBuild did not return after forced cancel")
	}
	if exit != ExitCancelled {
		t.Errorf("exit = %d, want ExitCancelled (%d)", exit, ExitCancelled)
	}
	select {
	case <-stops:
		// good: forced cancel recycled the daemon
	case <-time.After(time.Second):
		t.Error("OnForcedCancel was not invoked after a forced cancel (S3: daemon must be recycled)")
	}
}

// TestRunBuildGracefulCancelPreservesDaemon asserts that a GRACEFUL
// cancel (first signal only, no force) does NOT invoke OnForcedCancel —
// the warm Gradle daemon is preserved per spec §144.
func TestRunBuildGracefulCancelPreservesDaemon(t *testing.T) {
	g := testRunGrants(t)
	res := Resolved{
		Worktree:   g.Workdir,
		ProjectDir: g.Workdir,
		Wrapper:    "/bin/sh",
		// Dies on SIGTERM (default disposition) within the window.
		Args: []string{"-c", "sleep 30"},
	}
	stops := make(chan struct{}, 4)
	onForce := func(stderr io.Writer) error {
		stops <- struct{}{}
		return nil
	}
	cancel := make(chan struct{})
	force := make(chan struct{}) // never closed
	done := make(chan struct{})
	var exit int
	go func() {
		exit, _ = RunBuild(RunOptions{
			Resolved:       res,
			Grants:         g,
			Stdout:         &bytes.Buffer{},
			Stderr:         &bytes.Buffer{},
			Launcher:       NoSandboxLauncher,
			Auditor:        audit.Nop(),
			Cancel:         cancel,
			ForceCancel:    force,
			KillAfter:      2 * time.Second,
			OnForcedCancel: onForce,
		})
		close(done)
	}()
	time.Sleep(200 * time.Millisecond)
	close(cancel) // graceful cancel only (force stays open)
	<-done
	if exit != ExitCancelled {
		t.Errorf("exit = %d, want ExitCancelled (%d)", exit, ExitCancelled)
	}
	select {
	case <-stops:
		t.Error("OnForcedCancel must NOT fire on graceful cancel (daemon preserved)")
	case <-time.After(200 * time.Millisecond):
		// good: no recycle
	}
}

// TestParseArgs_MaxDurationOverBudgetDeniesBeforeStart asserts that a
// non-positive --max-duration is rejected at parse time (P4: spec.md:150
// — an excessive/invalid request fails before executor startup). The
// build never starts; the request is a policy denial.
func TestParseArgs_MaxDurationOverBudgetDeniesBeforeStart(t *testing.T) {
	for _, args := range [][]string{
		{"--max-duration", "0", "--", "gradle"},
		{"--max-duration", "-5m", "--", "gradle"},
		{"--max-duration", "notaduration", "--", "gradle"},
	} {
		_, err := ParseArgs(args)
		if err == nil {
			t.Fatalf("ParseArgs(%v) expected an error, got none", args)
		}
		var reqErr *RequestError
		if !errors.As(err, &reqErr) {
			t.Errorf("ParseArgs(%v) err = %T, want *RequestError", args, err)
		}
	}
}

// recordingAuditor captures every emitted Event for assertions. It
// satisfies audit.Auditor without writing anywhere.
type recordingAuditor struct {
	events []audit.Event
}

func (r *recordingAuditor) Emit(ev audit.Event) { r.events = append(r.events, ev) }
func (r *recordingAuditor) Close() error        { return nil }
func (r *recordingAuditor) RunID() string       { return "test" }
func (r *recordingAuditor) NextSeq() uint64     { return 0 }

// eventText renders an audit.Event the way a JSON sink would, so a leak
// assertion can grep the serialized form for the token. It covers the
// fields the build path populates (Argv, SandboxProfile, ExitCode, ...).
func eventText(ev audit.Event) string {
	return fmt.Sprintf("%+v", ev)
}

// TestRunBuildProxyTokenDoesNotLeak asserts the proxy token — now carried
// in GRADLE_OPTS (https.proxyPassword=<token>) so the wrapper download
// authenticates against the omac proxy — does NOT leak through any omac-
// side output: neither stderr (omac's own log/error lines), nor the audit
// trail. The token is safe in GRADLE_OPTS because the JVM does not print
// that env var (unlike JAVA_TOOL_OPTIONS, which the JVM prints on every
// launch — spec.md:180). Only the proxy host:port may appear in omac's
// output; the userinfo must stay in the child env.
func TestRunBuildProxyTokenDoesNotLeak(t *testing.T) {
	const token = "leakcanary-deadbeef"
	wt, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cacheDir := filepath.Join(t.TempDir(), "cache")
	g, err := GrantsFor(wt, cacheDir, BuildConfig{
		ProxyURL:  "http://omac:" + token + "@127.0.0.1:9999",
		ProxyPort: 9999,
	})
	if err != nil {
		t.Fatalf("GrantsFor: %v", err)
	}
	t.Cleanup(g.CleanupTmp)

	// Sanity: the token IS in the child env (GRADLE_OPTS), so a leak
	// assertion is meaningful — if it were absent there'd be nothing to
	// leak. This is the intended, safe channel (JVM does not print it).
	env := ChildEnv(g)
	gradleOpts := ""
	for _, kv := range env {
		if strings.HasPrefix(kv, "GRADLE_OPTS=") {
			gradleOpts = strings.TrimPrefix(kv, "GRADLE_OPTS=")
		}
	}
	if !strings.Contains(gradleOpts, token) {
		t.Fatalf("setup invariant: GRADLE_OPTS must carry the token to authenticate; got %q", gradleOpts)
	}
	if strings.Contains(gradleOpts, "JAVA_TOOL_OPTIONS") {
		t.Fatalf("GRADLE_OPTS must never reference JAVA_TOOL_OPTIONS: %q", gradleOpts)
	}

	// Force omac's service-failure stderr path: a launcher that errors
	// makes RunBuild emit "build executor launch: ..." to stderr WITHOUT
	// ever spawning the child, so any token in that line would be an omac
	// leak (not the child printing its own env).
	boomLauncher := func(*BuildGrants, []string) ([]string, error) {
		return nil, errors.New("simulated launch failure")
	}
	res := Resolved{
		Worktree:   g.Workdir,
		ProjectDir: g.Workdir,
		Wrapper:    "/bin/true",
		Args:       []string{":help"},
	}
	var stderr bytes.Buffer
	rec := &recordingAuditor{}
	_, runErr := RunBuild(RunOptions{
		Resolved: res, Grants: g,
		Stdout:   &bytes.Buffer{},
		Stderr:   &stderr,
		Launcher: boomLauncher,
		Auditor:  rec,
	})
	if runErr == nil {
		t.Fatal("expected a launch error from boomLauncher")
	}

	// 1. omac's own stderr must not contain the token. The proxy
	//    host:port (127.0.0.1:9999) is fine to log; the userinfo is not.
	//    netproxy logs only destination hosts (server.go:264), and RunBuild
	//    logs only the error wrapper, never the env/gradleOpts.
	if strings.Contains(stderr.String(), token) {
		t.Errorf("proxy token leaked into omac stderr:\n%s", stderr.String())
	}

	// 2. The audit trail must not carry the token. InnerExec logs only
	//    argv (gradlew + args), never env; ProcessExit carries only codes.
	//    A future change that adds env to an event would leak here.
	for _, ev := range rec.events {
		if strings.Contains(eventText(ev), token) {
			t.Errorf("proxy token leaked into audit event: %+v", ev)
		}
	}

	// 3. Confirm the intended channel is the only one: JAVA_TOOL_OPTIONS
	//    must be absent from the child env entirely (the JVM prints it).
	for _, kv := range env {
		if strings.HasPrefix(kv, "JAVA_TOOL_OPTIONS=") {
			t.Errorf("JAVA_TOOL_OPTIONS must NEVER be set (JVM prints it, leaking tokens): %q", kv)
		}
	}
}
