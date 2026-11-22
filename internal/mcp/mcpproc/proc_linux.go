//go:build linux

// Package mcpproc starts and kills the stdio MCP server processes that the
// gateway and the tool probe run on the host.
package mcpproc

import "syscall"

// dieWithParent makes the kernel kill the child when its parent (the gateway or the probe) dies, even
// by SIGKILL, so no child outlives its gateway holding secrets.
func dieWithParent(attr *syscall.SysProcAttr) { attr.Pdeathsig = syscall.SIGKILL }
