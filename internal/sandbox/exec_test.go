package sandbox

import (
	"sync/atomic"
	"testing"
	"time"
)

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
