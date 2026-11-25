package sandboxrun

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// dockerDesktopLayout mirrors Docker Desktop on macOS: the socket lives in
// <home>/.docker/run, and <varRun>/docker.sock is a symlink to it.
type dockerDesktopLayout struct {
	dotDocker, runDir, sock, varRun, link string
}

func newDockerDesktopLayout(t *testing.T) dockerDesktopLayout {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	l := dockerDesktopLayout{
		dotDocker: filepath.Join(root, "home", ".docker"),
		varRun:    filepath.Join(root, "var-run"),
	}
	l.runDir = filepath.Join(l.dotDocker, "run")
	l.sock = filepath.Join(l.runDir, "docker.sock")
	l.link = filepath.Join(l.varRun, "docker.sock")
	for _, d := range []string{l.runDir, l.varRun} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// Listen on a relative path: the absolute one can exceed the
	// 104-byte limit for socket paths.
	t.Chdir(l.runDir)
	listener, err := net.Listen("unix", "docker.sock")
	if err != nil {
		t.Skipf("cannot create a Unix socket here: %v", err)
	}
	t.Cleanup(func() { listener.Close() })
	if err := os.Symlink(l.sock, l.link); err != nil {
		t.Fatal(err)
	}
	return l
}

func TestSocketsClosedByProtection(t *testing.T) {
	l := newDockerDesktopLayout(t)
	for _, tc := range []struct {
		name      string
		grants    Grants
		want      []closedSocket
		wantNoted []string
	}{
		{
			name:   "socket in a granted folder is a link into a protected folder",
			grants: Grants{UnixSocketDirs: []string{l.varRun}, ProtectedPaths: []string{l.dotDocker}},
			want:   []closedSocket{{grant: l.varRun, path: l.link, protectedBy: []string{l.dotDocker}}},
		},
		{
			name:   "protected link points at a socket in a granted folder",
			grants: Grants{UnixSocketDirs: []string{l.runDir}, ProtectedPaths: []string{l.link}},
			want:   []closedSocket{{grant: l.runDir, path: l.sock, protectedBy: []string{l.link}}},
		},
		{
			name:   "every protected path that covers the socket is named",
			grants: Grants{UnixSocketDirs: []string{l.varRun}, ProtectedPaths: []string{l.link, l.dotDocker}},
			want:   []closedSocket{{grant: l.varRun, path: l.link, protectedBy: []string{l.link, l.dotDocker}}},
		},
		{
			name:   "granted folder lies inside a protected folder",
			grants: Grants{UnixSocketDirs: []string{l.runDir}, ProtectedPaths: []string{l.dotDocker}},
			want:   []closedSocket{{grant: l.runDir, path: l.runDir, protectedBy: []string{l.dotDocker}}},
		},
		{
			name:   "granted socket file lies inside a protected folder",
			grants: Grants{UnixSockets: []string{l.sock}, ProtectedPaths: []string{l.dotDocker}},
			want:   []closedSocket{{grant: l.sock, path: l.sock, protectedBy: []string{l.dotDocker}}},
		},
		{
			name:   "protection lifted from the folder and the link",
			grants: Grants{UnixSocketDirs: []string{l.varRun, l.runDir}, UnixSockets: []string{l.sock}, ProtectedPaths: []string{filepath.Join(filepath.Dir(l.dotDocker), ".ssh")}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := socketsClosedByProtection(&tc.grants)
			if !slices.EqualFunc(got, tc.want, func(a, b closedSocket) bool {
				return a.grant == b.grant && a.path == b.path && slices.Equal(a.protectedBy, b.protectedBy)
			}) {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}

			var stderr bytes.Buffer
			warnSocketsClosedByProtection(&stderr, &tc.grants)
			if len(tc.want) == 0 && stderr.Len() > 0 {
				t.Errorf("unexpected warning: %s", stderr.String())
			}
			for _, c := range tc.want {
				for _, part := range append([]string{c.grant, c.path, "filesystem.override_deny"}, c.protectedBy...) {
					if !strings.Contains(stderr.String(), part) {
						t.Errorf("warning lacks %q: %s", part, stderr.String())
					}
				}
			}
		})
	}
}
