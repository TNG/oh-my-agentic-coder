//go:build !unix

package mcpproc

import "os/exec"

// SetProcessGroup starts cmd in its own process group, so Kill reaches
// everything it spawns, and on Linux has the kernel kill it when this
// process dies.
func SetProcessGroup(*exec.Cmd) {}

// Kill sends SIGKILL to the process group of cmd, or to cmd alone when
// that fails.
func Kill(cmd *exec.Cmd) {
	if cmd.Process != nil {
		cmd.Process.Kill()
	}
}
