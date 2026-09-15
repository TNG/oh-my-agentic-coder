//go:build vuln

package cli

import (
	"os"
	"os/exec"
	"testing"
)

// TestSecurityControlInfoRequiresLiveOwnedPID asserts that readControlInfo
// does not treat a record naming a dead process as evidence a live `omac
// serve` is listening at its control_base.
//
// readControlInfo's only check is ControlBase != "" (plus the loopback
// check the sibling test adds); the PID field it also reads is never
// verified alive or owned by the current user. A stale or planted record
// naming a dead pid is indistinguishable from a genuine one.
func TestSecurityControlInfoRequiresLiveOwnedPID(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())

	// Control: a record naming our own live pid is accepted, so the
	// mechanism itself works and a blanket rejection wouldn't make this
	// test pass for the wrong reason.
	writeRawControlInfo(t, controlInfo{ControlBase: "http://127.0.0.1:45671", PID: os.Getpid()})
	if _, ok := readControlInfo(); !ok {
		t.Fatalf("control: a record naming our own live pid was rejected: the fixture is broken, not the security property")
	}

	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Skipf("no /usr/bin/true available to obtain a guaranteed-dead pid: %v", err)
	}
	deadPID := cmd.Process.Pid

	writeRawControlInfo(t, controlInfo{ControlBase: "http://127.0.0.1:45671", PID: deadPID})
	ci, ok := readControlInfo()
	if ok && ci.PID == deadPID {
		t.Errorf("readControlInfo accepted a control-info record naming dead pid %d as a live serve process: "+
			"host-side omac commands will send reload requests to whatever control_base a stale or planted record names", deadPID)
	}
}
