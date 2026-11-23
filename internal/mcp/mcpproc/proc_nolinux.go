//go:build unix && !linux

package mcpproc

import "syscall"

// dieWithParent does nothing: only Linux has Pdeathsig. Elsewhere a child
// ends when its stdin closes, which the OS does when its parent dies.
func dieWithParent(*syscall.SysProcAttr) {}
