package containerproxy

import (
	"fmt"
	"hash/fnv"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Stable port range: [StablePortMin, StablePortMax).
//
// 30000–39999 sits above the common dev-port range (3xxx–9xxx, 8080, 9090,
// …) and below the macOS/Linux ephemeral range (49152–65535), so collisions
// with arbitrary dev tools or with the kernel's own ephemeral allocations
// are rare. The window is 10000 ports wide, which gives the per-worktree
// hash plenty of room while keeping the fallback scan window small
// (portScanWindow) in the rare collision case.
const (
	StablePortMin  = 30000
	StablePortMax  = 40000
	portScanWindow = 50
	portFileName   = "containerproxy-port"
	portFileDir    = ".omac-control"
)

// stablePortFor returns a deterministic port in [StablePortMin, StablePortMax)
// derived from the canonical (symlink-resolved) worktree path. The same
// worktree always maps to the same port so the warm Gradle daemon's cached
// DOCKER_HOST stays valid across runs (the bug being fixed: the proxy used
// to bind a random ephemeral port each run, and the warm daemon kept
// pointing at the dead old port). The hash is FNV-1a over the canonical
// path, truncated to the range width.
func stablePortFor(worktreePath string) int {
	canonical := worktreePath
	if c, err := filepath.EvalSymlinks(worktreePath); err == nil && c != "" {
		canonical = c
	}
	h := fnv.New32a()
	_, _ = io.WriteString(h, canonical)
	// FNV-1a 32-bit; map into [StablePortMin, StablePortMax).
	span := uint32(StablePortMax - StablePortMin)
	return StablePortMin + int(h.Sum32()%span)
}

// portIsFree reports whether a loopback TCP port can be bound right now.
// A true return means a listener opened and was closed immediately. Used
// by the port-selection helpers and by the control-file reuse check.
func portIsFree(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

// selectPort chooses a bindable port given a preferred port. It tries the
// preferred port, then scans portScanWindow successive ports in the stable
// range (wrapping at StablePortMax back to StablePortMin), and finally
// falls back to fallbackRandom (which must return a free port — production
// wires a 127.0.0.1:0 kernel-assigned port). isFree is injectable so tests
// can simulate a fully-occupied window without binding 50 real sockets.
//
// Returns the selected port. If the preferred port and the whole window
// are occupied, fallbackRandom is called and its result returned (even if
// 0, which the caller treats as "use a random ephemeral port"). The
// caller is responsible for logging the fallback.
func selectPort(preferred int, isFree func(int) bool, fallbackRandom func() int) int {
	if preferred > 0 && isFree(preferred) {
		return preferred
	}
	for i := 1; i <= portScanWindow; i++ {
		cand := preferred + i
		if cand >= StablePortMax {
			cand = StablePortMin + (cand - StablePortMax)
		}
		if cand < StablePortMin || cand >= StablePortMax {
			continue
		}
		if isFree(cand) {
			return cand
		}
	}
	return fallbackRandom()
}

// randomFreePort asks the kernel for a free ephemeral loopback port and
// returns it after releasing the listener. Used as the fallbackRandom
// callback for selectPort when the whole stable window is occupied. A
// returned 0 means the kernel could not allocate one (caller logs a
// warning and Start returns an error — correctness over determinism).
func randomFreePort() int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

// --- control-state port file --------------------------------------------
//
// The assigned port is recorded at <leaf>/.omac-control/containerproxy-port
// so the next run can prefer it (the listener is torn down between runs by
// defer stopContainerProxy(), but the file survives and keeps the port
// stable). The file is written by the SUPERVISOR (unsandboxed) — same
// pattern as gradle.properties and the init scripts — and is read back by
// the supervisor on the next start. It does NOT need to be in the
// executor's read-grant set (the executor never reads it); the executor
// only sees DOCKER_HOST. The control dir is already WriteDenyPaths'd for
// the executor (see buildrun/control.go controlFiles / controlDirs), so
// build code cannot tamper with it.

// portFilePath returns the absolute path to the control-state port file
// for the given OMAC cache leaf (GRADLE_USER_HOME leaf).
func portFilePath(leaf string) string {
	return filepath.Join(leaf, portFileDir, portFileName)
}

// readPreferredPort reads the previously-assigned port from the
// control-state file, if any. Returns 0 when the file is absent,
// unreadable, or contains an out-of-range port (the caller then computes
// a fresh stable port from the worktree path).
func readPreferredPort(leaf string) int {
	b, err := os.ReadFile(portFilePath(leaf))
	if err != nil {
		return 0
	}
	port, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || port < StablePortMin || port >= StablePortMax {
		return 0
	}
	return port
}

// writePreferredPort persists the assigned port to the control-state file
// so the next run can prefer it. Best-effort: a write failure is logged by
// the caller but does not fail the build (the port is still valid for this
// run; only cross-run stability is degraded). The control dir is created
// if absent (PrepareControlState normally creates it, but the container
// proxy may start before PrepareControlState runs in some wiring orders,
// and the port file lives under the same dir).
func writePreferredPort(leaf string, port int) error {
	dir := filepath.Join(leaf, portFileDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create control dir for port file: %w", err)
	}
	return os.WriteFile(portFilePath(leaf), []byte(strconv.Itoa(port)), 0o644)
}
