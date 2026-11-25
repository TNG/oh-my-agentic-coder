package sandboxrun

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"testing"
)

// dockerDesktopLayout mirrors Docker Desktop on macOS: the socket lives in
// <home>/.docker/run, and <varRun>/docker.sock is a symlink to it.
type dockerDesktopLayout struct {
	dotDocker, runDir, sock, varRun, link string
	// stoppedVarRun holds the link as it is while Docker is stopped: its
	// target in runDir does not exist.
	stoppedVarRun, danglingLink string
}

func newDockerDesktopLayout(t *testing.T) dockerDesktopLayout {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	l := dockerDesktopLayout{
		dotDocker:     filepath.Join(root, "home", ".docker"),
		varRun:        filepath.Join(root, "var-run"),
		stoppedVarRun: filepath.Join(root, "var-run-stopped"),
	}
	l.runDir = filepath.Join(l.dotDocker, "run")
	l.sock = filepath.Join(l.runDir, "docker.sock")
	l.link = filepath.Join(l.varRun, "docker.sock")
	l.danglingLink = filepath.Join(l.stoppedVarRun, "docker.sock")
	for _, d := range []string{l.runDir, l.varRun, l.stoppedVarRun} {
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
	for link, target := range map[string]string{
		l.link:         l.sock,
		l.danglingLink: filepath.Join(l.runDir, "not-started.sock"),
		// A link to a regular file is not a socket and must be ignored.
		filepath.Join(l.varRun, "config.json"): filepath.Join(l.dotDocker, "config.json"),
	} {
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(l.dotDocker, "config.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	return l
}

func TestWarnSocketsClosedByProtection(t *testing.T) {
	l := newDockerDesktopLayout(t)
	const (
		prefix     = "omac sandbox: warning: the agent cannot connect to "
		sideEffect = " override_deny lifts the protection from the whole path, not only from the socket: other grants that cover it then apply to everything in it."
	)
	builtIn := []string{l.dotDocker, l.link, l.danglingLink}
	for _, tc := range []struct {
		name   string
		grants *Grants
		want   string
	}{
		{
			name:   "socket in a granted folder is a protected link into a protected folder",
			grants: &Grants{UnixSocketDirs: []string{l.varRun}, ProtectedPaths: []string{l.dotDocker, l.link}},
			want: prefix + "the Unix socket " + l.link + " in the granted folder " + l.varRun +
				", because it lies in a protected path (" + l.dotDocker + ", " + l.link + "). To allow the connect, list " +
				l.dotDocker + " and " + l.link + " in filesystem.override_deny, and add " + l.runDir +
				", where the socket really lives, to filesystem.allow_unix_dir." + sideEffect + "\n",
		},
		{
			name:   "link whose target does not exist yet",
			grants: &Grants{UnixSocketDirs: []string{l.stoppedVarRun}, ProtectedPaths: []string{l.dotDocker, l.danglingLink}},
			want: prefix + "the Unix socket " + l.danglingLink + " in the granted folder " + l.stoppedVarRun +
				", because it lies in a protected path (" + l.dotDocker + ", " + l.danglingLink + "). To allow the connect, list " +
				l.dotDocker + " and " + l.danglingLink + " in filesystem.override_deny, and add " + l.runDir +
				", where the socket really lives, to filesystem.allow_unix_dir." + sideEffect + "\n",
		},
		{
			name:   "granted folder in a protected folder names the protected link to its socket too",
			grants: &Grants{UnixSocketDirs: []string{l.runDir}, ProtectedPaths: []string{l.dotDocker, l.link}},
			want: prefix + "Unix sockets in the granted folder " + l.runDir +
				", because it lies in a protected path (" + l.dotDocker + ", " + l.link + "). To allow the connect, list " +
				l.dotDocker + " and " + l.link + " in filesystem.override_deny." + sideEffect + "\n",
		},
		{
			name:   "protected link points at a socket in a granted folder",
			grants: &Grants{UnixSocketDirs: []string{l.runDir}, ProtectedPaths: []string{l.link}},
			want: prefix + "the Unix socket " + l.sock + " in the granted folder " + l.runDir +
				", because it lies in a protected path (" + l.link + "). To allow the connect, list " +
				l.link + " in filesystem.override_deny." + sideEffect + "\n",
		},
		{
			name:   "granted socket file in a protected folder",
			grants: &Grants{UnixSockets: []string{l.sock}, ProtectedPaths: []string{l.dotDocker}},
			want: prefix + "the granted Unix socket " + l.sock +
				", because it lies in a protected path (" + l.dotDocker + "). To allow the connect, list " +
				l.dotDocker + " in filesystem.override_deny." + sideEffect + "\n",
		},
		{
			name:   "deny rule of the session is not offered for override_deny",
			grants: &Grants{UnixSocketDirs: []string{l.runDir}, ProtectedPaths: []string{filepath.Dir(l.dotDocker)}},
			want: prefix + "Unix sockets in the granted folder " + l.runDir +
				", because it lies in a protected path (" + filepath.Dir(l.dotDocker) + "). To allow the connect, remove or narrow the deny rule for " +
				filepath.Dir(l.dotDocker) + ", which override_deny cannot lift.\n",
		},
		{
			name:   "the documented Docker Desktop recipe",
			grants: &Grants{UnixSocketDirs: []string{l.runDir}, ProtectedPaths: []string{filepath.Join(filepath.Dir(l.dotDocker), ".ssh")}},
		},
		{
			name:   "link to a regular file in a protected folder",
			grants: &Grants{UnixSocketDirs: []string{l.varRun}, ProtectedPaths: []string{filepath.Join(l.dotDocker, "config.json")}},
		},
		{
			name: "learn mode lifts the protection",
			grants: (&Grants{
				UnixSocketDirs: []string{l.varRun, l.runDir}, UnixSockets: []string{l.sock},
				ProtectedPaths: []string{l.dotDocker, l.link},
			}).withUnrestrictedFilesystem(),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stderr bytes.Buffer
			warnSocketsClosedByProtection(&stderr, tc.grants, builtIn)
			if got := stderr.String(); got != tc.want {
				t.Errorf("warning\n got: %q\nwant: %q", got, tc.want)
			}
		})
	}
}
