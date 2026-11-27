//go:build darwin

package sandboxrun

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

const connectUnixSocketScript = `IO::Socket::UNIX->new(Type => SOCK_STREAM(), Peer => $ARGV[0]) or do { print "$!"; exit 3 }`

// TestIntegrationSharedOpenCodePTYDirStaysClosed checks that a folder in
// the protected paths refuses a socket connect whichever way a profile
// grants it. The baseline entry for the shared OpenCode helper folder goes
// through the same code path, so the test protects its own folder instead
// of touching a real helper. The connect must fail with EPERM, and a
// connect to a granted but unprotected folder must succeed.
func TestIntegrationSharedOpenCodePTYDirStaysClosed(t *testing.T) {
	protected := newSocketFolder(t, "omac-ptytest-*")
	open := newSocketFolder(t, "omac-pty-*")
	protectedSock := filepath.Join(protected, "p.sock")
	openSock := filepath.Join(open, "o.sock")
	serveUnixSocket(t, protectedSock, "helper-reached")
	serveUnixSocket(t, openSock, "helper-reached")

	for _, tc := range []struct {
		name, grant, sock string
		protect           bool
	}{
		{"exact folder", protected, protectedSock, true},
		{"private tmp spelling", "/private" + protected, protectedSock, true},
		{"tmp", "/tmp", protectedSock, true},
		{"unprotected folder", open, openSock, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wd, err := os.MkdirTemp("/tmp", "omac-wd-*")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(wd)
			p := &sandboxprofile.Profile{
				Workdir:    sandboxprofile.Workdir{Access: sandboxprofile.AccessReadWrite},
				Filesystem: sandboxprofile.Filesystem{AllowUnixDir: []string{tc.grant}},
				Network:    sandboxprofile.Network{Mode: sandboxprofile.ModeFiltered},
			}
			g, err := ResolveGrants(p, wd, nil)
			if err != nil {
				t.Fatal(err)
			}
			g.ProtectedPaths = append(g.ProtectedPaths, protected)

			out, code := runSandboxed(t, g, "/usr/bin/perl", "-MIO::Socket::UNIX", "-e", connectUnixSocketScript, tc.sock)
			switch {
			case tc.protect && (code != 3 || !strings.Contains(out, "Operation not permitted")):
				t.Errorf("connect to %s under allow_unix_dir %s: want EPERM, got exit=%d out=%q", tc.sock, tc.grant, code, out)
			case !tc.protect && code != 0:
				t.Errorf("connect to %s under allow_unix_dir %s: want success, got exit=%d out=%q", tc.sock, tc.grant, code, out)
			}
		})
	}
}

// TestIntegrationPTYDirListenAndConnect covers what OpenCode's shell helper
// does in a session: with the grants of a real launch (an allow_unix_dir
// folder and one open port), one sandboxed process listens on
// <folder>/opencode-pty-<uid>/<32 hex>.sock and another connects to it.
func TestIntegrationPTYDirListenAndConnect(t *testing.T) {
	folder := newSocketFolder(t, "omac-pty-*")
	token := make([]byte, 16)
	if _, err := rand.Read(token); err != nil {
		t.Fatal(err)
	}
	sockDir := filepath.Join(folder, "opencode-pty-"+strconv.Itoa(os.Getuid()))
	sock := filepath.Join(sockDir, hex.EncodeToString(token)+".sock")

	wd := t.TempDir()
	p := &sandboxprofile.Profile{
		Workdir:    sandboxprofile.Workdir{Access: sandboxprofile.AccessReadWrite},
		Filesystem: sandboxprofile.Filesystem{AllowUnixDir: []string{folder}},
		Network:    sandboxprofile.Network{Mode: sandboxprofile.ModeFiltered, OpenPort: []int{usabilityFreePort(t)}},
	}
	g, err := ResolveGrants(p, wd, nil)
	if err != nil {
		t.Fatal(err)
	}

	listenScript := `mkdir $ARGV[0]; my $s = IO::Socket::UNIX->new(Type => SOCK_STREAM(), Local => $ARGV[1], Listen => 1) or do { print "$!"; exit 3 }; sleep 30`
	argv, err := BuildChildArgv(g, []string{"/usr/bin/perl", "-MIO::Socket::UNIX", "-e", listenScript, sockDir, sock})
	if err != nil {
		t.Fatal(err)
	}
	listener := exec.Command(argv[0], argv[1:]...)
	var listenerOutput bytes.Buffer
	listener.Stdout, listener.Stderr = &listenerOutput, &listenerOutput
	if err := listener.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		listener.Process.Kill()
		listener.Wait()
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the sandboxed process did not create %s: %q", sock, listenerOutput.String())
		}
		time.Sleep(50 * time.Millisecond)
	}

	out, code := runSandboxed(t, g, "/usr/bin/perl", "-MIO::Socket::UNIX", "-e", connectUnixSocketScript, sock)
	if code != 0 {
		t.Errorf("sandboxed connect to %s: exit=%d out=%q", sock, code, out)
	}
}

func newSocketFolder(t *testing.T, pattern string) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", pattern)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}
