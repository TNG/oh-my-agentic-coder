//go:build unix

package gateway

import (
	"os/exec"
	"syscall"
)

func setProcessGroup(cmd *exec.Cmd) {
	attr := &syscall.SysProcAttr{Setpgid: true}
	dieWithParent(attr)
	cmd.SysProcAttr = attr
}

func killProcess(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	if syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) != nil {
		cmd.Process.Kill()
	}
}
