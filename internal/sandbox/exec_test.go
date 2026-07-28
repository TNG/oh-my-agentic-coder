package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestNewChildCmdSetsWorkdir verifies the child command is configured to run
// in the requested workdir (and inherits the caller cwd when empty). This is
// the seam that fixes the macOS/Seatbelt gap where the child otherwise ran in
// omac's launch directory instead of --workdir (Linux gets it via bwrap
// --chdir; Seatbelt has no equivalent, so it must be set on the exec).
func TestNewChildCmdSetsWorkdir(t *testing.T) {
	cmd := newChildCmd([]string{"/bin/true"}, []string{"A=1"}, "/some/work")
	if cmd.Dir != "/some/work" {
		t.Errorf("cmd.Dir = %q, want /some/work", cmd.Dir)
	}
	if empty := newChildCmd([]string{"/bin/true"}, nil, ""); empty.Dir != "" {
		t.Errorf("empty workdir must leave cmd.Dir unset, got %q", empty.Dir)
	}
}

// TestExecWithEnvRunsInWorkdir runs a real child and asserts it executes in
// the configured workdir rather than the test process cwd.
func TestExecWithEnvRunsInWorkdir(t *testing.T) {
	wd := t.TempDir()
	out := filepath.Join(wd, "cwd.txt")
	code, err := ExecWithEnv([]string{"/bin/sh", "-c", "pwd -P > " + out}, os.Environ(), wd, nil)
	if err != nil {
		t.Fatalf("ExecWithEnv error: %v", err)
	}
	if code != 0 {
		t.Fatalf("child exit code = %d, want 0", code)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read cwd file: %v", err)
	}
	want, _ := filepath.EvalSymlinks(wd)
	if strings.TrimSpace(string(got)) != want {
		t.Errorf("child cwd = %q, want %q", strings.TrimSpace(string(got)), want)
	}
}

// TestExecWithReadyCallbackAndExitCode verifies that ExecWithReady invokes
// the onReady hook after the child starts and propagates the child's exit
// code. Uses a trivial shell command (no network, no tty interaction in the
// test harness).
func TestExecWithReadyCallbackAndExitCode(t *testing.T) {
	var called int32
	// `true` exits 0 quickly; give onReady time to fire by sleeping a hair.
	code, err := ExecWithReady([]string{"/bin/sh", "-c", "sleep 0.1; exit 7"}, nil, func() {
		atomic.StoreInt32(&called, 1)
	})
	if err != nil {
		t.Fatalf("ExecWithReady error: %v", err)
	}
	if code != 7 {
		t.Errorf("exit code = %d, want 7", code)
	}
	// onReady runs on a goroutine just before Wait; it should have fired.
	deadline := time.Now().Add(time.Second)
	for atomic.LoadInt32(&called) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if atomic.LoadInt32(&called) == 0 {
		t.Error("onReady was not called")
	}
}

func TestExecEmptyArgv(t *testing.T) {
	if _, err := ExecWithReady(nil, nil, nil); err == nil {
		t.Error("expected error for empty argv")
	}
}
