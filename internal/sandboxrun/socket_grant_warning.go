package sandboxrun

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// closedSocket is a granted Unix socket, or a granted folder for sockets,
// that the agent still cannot connect to on macOS: protected paths are
// denied after every allow (see GenerateSBPL).
type closedSocket struct {
	grant, path string
	protectedBy []string
}

// socketsClosedByProtection checks each socket grant, and each socket
// directly inside a granted socket folder, against the protected paths.
// Symlinks count on both sides, because Seatbelt matches the resolved
// path: Docker Desktop's /var/run/docker.sock is a link into ~/.docker.
// Sockets deeper inside a granted folder, or created after launch, are
// not found.
func socketsClosedByProtection(g *Grants) []closedSocket {
	protected := make(map[string][]string, len(g.ProtectedPaths))
	for _, p := range g.ProtectedPaths {
		protected[p] = pathForms(p)
	}
	protectedBy := func(path string) []string {
		var by []string
		for _, p := range g.ProtectedPaths {
			if anyUnderAny(pathForms(path), protected[p]) {
				by = append(by, p)
			}
		}
		return by
	}

	var closed []closedSocket
	for _, sock := range g.UnixSockets {
		if by := protectedBy(sock); len(by) > 0 {
			closed = append(closed, closedSocket{grant: sock, path: sock, protectedBy: by})
		}
	}
	for _, dir := range g.UnixSocketDirs {
		if by := protectedBy(dir); len(by) > 0 {
			closed = append(closed, closedSocket{grant: dir, path: dir, protectedBy: by})
			continue
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			path := filepath.Join(dir, e.Name())
			if fi, err := os.Stat(path); err != nil || fi.Mode()&os.ModeSocket == 0 {
				continue
			}
			if by := protectedBy(path); len(by) > 0 {
				closed = append(closed, closedSocket{grant: dir, path: path, protectedBy: by})
			}
		}
	}
	return closed
}

func anyUnderAny(paths, roots []string) bool {
	for _, p := range paths {
		for _, r := range roots {
			if isUnder(p, r) {
				return true
			}
		}
	}
	return false
}

func warnSocketsClosedByProtection(stderr io.Writer, g *Grants) {
	for _, c := range socketsClosedByProtection(g) {
		what := "the Unix socket " + c.path + " although " + c.grant + " is granted"
		if c.path == c.grant {
			what = "Unix sockets in " + c.path + " although it is granted"
		}
		fmt.Fprintf(stderr, "omac sandbox: warning: the agent cannot connect to %s, because it lies in a protected path (%s). To allow the connect, add each of these paths to filesystem.override_deny.\n",
			what, strings.Join(c.protectedBy, ", "))
	}
}
