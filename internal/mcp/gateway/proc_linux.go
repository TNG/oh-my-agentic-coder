//go:build linux

package gateway

import "syscall"

// dieWithParent makes the kernel kill the child when the gateway dies, even
// by SIGKILL, so no child outlives its gateway holding secrets.
func dieWithParent(attr *syscall.SysProcAttr) { attr.Pdeathsig = syscall.SIGKILL }
