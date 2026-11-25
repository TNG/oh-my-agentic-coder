package sandboxrun

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// closedSocket is a granted Unix socket, or a granted folder for sockets,
// that the agent still cannot connect to on macOS: protected paths are
// denied after every allow (see GenerateSBPL).
type closedSocket struct {
	grant         string
	grantIsFolder bool
	// path is what the agent cannot connect to: the grant itself, or one
	// socket in the granted folder.
	path        string
	protectedBy []string
	// ungrantedDir is the folder the socket really lives in when no socket
	// grant covers it. Seatbelt matches the resolved path, so lifting the
	// protection alone would not open such a socket.
	ungrantedDir string
}

// folderSocket is a socket directly inside a granted folder, or a link
// there that may become one: Docker Desktop's /var/run/docker.sock points
// at nothing while Docker is stopped.
type folderSocket struct {
	path     string
	forms    []string
	realPath string
}

// socketsClosedByProtection checks each socket grant, and each socket
// directly inside a granted socket folder, against the protected paths.
// Sockets deeper inside a granted folder, or created after launch, are
// not found.
func socketsClosedByProtection(g *Grants) []closedSocket {
	protectedForms := make(map[string][]string, len(g.ProtectedPaths))
	for _, p := range g.ProtectedPaths {
		protectedForms[p] = pathForms(p)
	}
	protectedBy := func(forms []string) []string {
		var by []string
		for _, p := range g.ProtectedPaths {
			if anyUnderAny(forms, protectedForms[p]) {
				by = append(by, p)
			}
		}
		return by
	}

	var closed []closedSocket
	for _, sock := range g.UnixSockets {
		if by := protectedBy(pathForms(sock)); len(by) > 0 {
			closed = append(closed, closedSocket{grant: sock, path: sock, protectedBy: by})
		}
	}
	for _, dir := range g.UnixSocketDirs {
		sockets := socketsDirectlyIn(dir)
		if len(protectedBy(pathForms(dir))) > 0 {
			forms := pathForms(dir)
			for _, s := range sockets {
				forms = append(forms, s.forms...)
			}
			closed = append(closed, closedSocket{grant: dir, grantIsFolder: true, path: dir, protectedBy: protectedBy(forms)})
			continue
		}
		for _, s := range sockets {
			if by := protectedBy(s.forms); len(by) > 0 {
				closed = append(closed, closedSocket{
					grant: dir, grantIsFolder: true, path: s.path, protectedBy: by,
					ungrantedDir: g.ungrantedSocketDir(s.realPath),
				})
			}
		}
	}
	return closed
}

func socketsDirectlyIn(dir string) []folderSocket {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var sockets []folderSocket
	for _, e := range entries {
		path := filepath.Join(dir, e.Name())
		target := path
		if e.Type()&os.ModeSymlink != 0 {
			if target, err = os.Readlink(path); err != nil {
				continue
			}
			if !filepath.IsAbs(target) {
				target = filepath.Join(dir, target)
			}
		}
		fi, err := os.Stat(target)
		isDanglingLink := err != nil && target != path
		if !isDanglingLink && (err != nil || fi.Mode()&os.ModeSocket == 0) {
			continue
		}
		s := folderSocket{path: path, forms: pathForms(path), realPath: target}
		if target != path {
			s.forms = append(s.forms, pathForms(target)...)
		}
		if resolved := canonicalPath(target); resolved != "" {
			s.realPath = resolved
		}
		sockets = append(sockets, s)
	}
	return sockets
}

func (g *Grants) ungrantedSocketDir(realPath string) string {
	for _, dir := range g.UnixSocketDirs {
		if anyUnderAny([]string{realPath}, pathForms(dir)) {
			return ""
		}
	}
	for _, sock := range g.UnixSockets {
		if slices.Contains(pathForms(sock), realPath) {
			return ""
		}
	}
	return filepath.Dir(realPath)
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

// warnSocketsClosedByProtection tells the user how to open each closed
// socket. Only the built-in protected paths in overridable can be lifted
// with filesystem.override_deny; any other one is a deny rule of the
// session.
func warnSocketsClosedByProtection(stderr io.Writer, g *Grants, overridable []string) {
	for _, c := range socketsClosedByProtection(g) {
		var builtIn, denyRules []string
		for _, p := range c.protectedBy {
			if slices.Contains(overridable, p) {
				builtIn = append(builtIn, p)
			} else {
				denyRules = append(denyRules, p)
			}
		}
		var steps []string
		if len(builtIn) > 0 {
			steps = append(steps, "list "+strings.Join(builtIn, " and ")+" in filesystem.override_deny")
		}
		if c.ungrantedDir != "" {
			steps = append(steps, "add "+c.ungrantedDir+", where the socket really lives, to filesystem.allow_unix_dir")
		}
		if len(denyRules) > 0 {
			steps = append(steps, "remove or narrow the deny rule for "+strings.Join(denyRules, " and ")+", which override_deny cannot lift")
		}
		sideEffect := ""
		if len(builtIn) > 0 {
			sideEffect = " override_deny lifts the protection from the whole path, not only from the socket: other grants that cover it then apply to everything in it."
		}
		fmt.Fprintf(stderr, "omac sandbox: warning: the agent cannot connect to %s, because it lies in a protected path (%s). To allow the connect, %s.%s\n",
			c.describe(), strings.Join(c.protectedBy, ", "), strings.Join(steps, ", and "), sideEffect)
	}
}

func (c closedSocket) describe() string {
	switch {
	case !c.grantIsFolder:
		return "the granted Unix socket " + c.path
	case c.path == c.grant:
		return "Unix sockets in the granted folder " + c.path
	default:
		return "the Unix socket " + c.path + " in the granted folder " + c.grant
	}
}
