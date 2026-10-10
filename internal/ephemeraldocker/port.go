package ephemeraldocker

import (
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net"
	"path/filepath"
)

// The host port for the session's docker endpoint forward follows the
// documented stableport scheme (CONTEXT.md, "Stable proxy port"): FNV-1a
// over the canonical path mapped into [30000, 40000), with a 50-port
// neighbor scan on collision. The internal/stableport package itself is
// not on main (it lives on origin/feat/jvm-build-executor), so this is a
// faithful, minimal re-implementation of the documented scheme — not a
// new invention.
const (
	stablePortMin  = 30000
	stablePortMax  = 40000
	portScanWindow = 50
)

// ErrNoFreePort reports an exhausted stable window. Unlike the proxy use
// of the scheme there is no kernel-ephemeral fallback: DOCKER_HOST must
// point at the exact port pinned in the Lima config, and a port that
// cannot be known in advance is useless here.
var ErrNoFreePort = errors.New("no free loopback port in the stable window")

// HostPortFor returns a deterministic loopback port for the canonical
// worktree path. isFree is injectable so tests can simulate occupancy
// without binding real sockets.
func HostPortFor(worktreePath string, isFree func(int) bool) (int, error) {
	if worktreePath == "" {
		return 0, errors.New("ephemeral-docker: worktree path required for port derivation")
	}
	if isFree == nil {
		isFree = portIsFree
	}
	canonical := worktreePath
	if c, err := filepath.EvalSymlinks(worktreePath); err == nil && c != "" {
		canonical = c
	}
	h := fnv.New32a()
	_, _ = io.WriteString(h, canonical)
	span := uint32(stablePortMax - stablePortMin)
	preferred := stablePortMin + int(h.Sum32()%span)
	for i := 0; i < portScanWindow; i++ {
		port := preferred + i
		if port >= stablePortMax {
			port -= stablePortMax - stablePortMin // wrap, like the scheme
		}
		if isFree(port) {
			return port, nil
		}
	}
	return 0, fmt.Errorf("ephemeral-docker: stable port window exhausted for %s: %w", worktreePath, ErrNoFreePort)
}

// portIsFree reports whether a loopback TCP port can be bound right now
// (the stableport IsFree shape).
func portIsFree(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}
