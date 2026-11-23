//go:build unix

package mcpproc

import (
	"os/exec"
	"syscall"
)

// SetProcessGroup starts cmd in its own process group, so Kill reaches
// everything it spawns, and on Linux has the kernel kill it when this
// process dies.
func SetProcessGroup(cmd *exec.Cmd) {
	attr := &syscall.SysProcAttr{Setpgid: true}
	dieWithParent(attr)
	cmd.SysProcAttr = attr
}

// Kill sends SIGKILL to the process group of cmd, or to cmd alone when
// that fails.
func Kill(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	if syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) != nil {
		cmd.Process.Kill()
	}
}
