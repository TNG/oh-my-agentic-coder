//go:build unix && !linux

package gateway

import "syscall"

// dieWithParent does nothing: only Linux has Pdeathsig. Elsewhere a child
// ends when its stdin closes, which the OS does when the gateway dies.
func dieWithParent(*syscall.SysProcAttr) {}
