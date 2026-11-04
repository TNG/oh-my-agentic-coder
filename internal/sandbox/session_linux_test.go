//go:build linux

package sandbox

import (
	"os"
	"os/exec"
	"slices"
	"syscall"
	"testing"
)

// TestChildPIDLookups checks both ways of finding a process's children:
// the /proc children file and the /proc/<n>/stat scan used when the kernel
// lacks CONFIG_PROC_CHILDREN.
func TestChildPIDLookups(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()

	if got := childPIDs(os.Getpid()); !slices.Contains(got, cmd.Process.Pid) {
		t.Errorf("childPIDs = %v, want it to contain %d", got, cmd.Process.Pid)
	}
	if got := scanChildPIDs(os.Getpid()); !slices.Contains(got, cmd.Process.Pid) {
		t.Errorf("scanChildPIDs = %v, want it to contain %d", got, cmd.Process.Pid)
	}
	if ppid, ok := parentPID(os.Getpid()); !ok || ppid != os.Getppid() {
		t.Errorf("parentPID = %d, %v; want %d", ppid, ok, os.Getppid())
	}
}

// TestDetachedSessionGroupsOnlySessionLeaders: a child that called setsid
// is relayed to; one that merely has its own process group is not.
func TestDetachedSessionGroupsOnlySessionLeaders(t *testing.T) {
	leader := exec.Command("sleep", "30")
	leader.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	group := exec.Command("sleep", "30")
	group.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	for _, c := range []*exec.Cmd{leader, group} {
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.Process.Kill(); _ = c.Wait() }()
	}

	got := detachedSessionGroups(os.Getpid())
	if !slices.Contains(got, leader.Process.Pid) {
		t.Errorf("detachedSessionGroups = %v, want the session leader %d", got, leader.Process.Pid)
	}
	if slices.Contains(got, group.Process.Pid) {
		t.Errorf("detachedSessionGroups = %v, must not contain the non-leader %d", got, group.Process.Pid)
	}
}
