//go:build !unix

package gateway

import "os/exec"

func setProcessGroup(*exec.Cmd) {}

func killProcess(cmd *exec.Cmd) {
	if cmd.Process != nil {
		cmd.Process.Kill()
	}
}
