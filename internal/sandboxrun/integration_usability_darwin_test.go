//go:build darwin

package sandboxrun

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

// TestIntegrationUsabilityMatrix runs what a session typically does under
// every network mode and enforcement the profile generator supports, and
// prints one line per probe:
//
//	MATRIX filtered+proxy/kernel/unix-connect-unixdir allowed
//
// Comparing these lines between two commits shows every access a change
// opens or closes. The test fails only for results that hold in every mode:
// protected files stay closed, and the basic workdir, grant, system and
// exec accesses stay open.
func TestIntegrationUsabilityMatrix(t *testing.T) {
	fx := newUsabilityFixture(t)
	for _, sc := range usabilityScenarios() {
		t.Run(sc.name+"/"+sc.enforcement, func(t *testing.T) {
			g := fx.grants(t, sc)
			for _, pr := range fx.probes() {
				allowed := usabilityRun(t, g, pr.script, pr.args...)
				// A bind probe that got through leaves its socket behind.
				for _, a := range pr.args {
					if filepath.Base(a) == usabilityBindName {
						os.Remove(a)
					}
				}
				result := usabilityDenied
				if allowed {
					result = usabilityAllowed
				}
				fmt.Printf("MATRIX %s/%s/%s %s\n", sc.name, sc.enforcement, pr.name, result)
				if pr.want != usabilityAny && result != pr.want {
					t.Errorf("%s: got %s, want %s in every mode", pr.name, result, pr.want)
				}
			}
		})
	}
}

type usabilityScenario struct {
	name, mode, enforcement string
	proxy, openPort, listen bool
}

func usabilityScenarios() []usabilityScenario {
	k, env := sandboxprofile.EnforceKernel, sandboxprofile.EnforceEnvOnly
	return []usabilityScenario{
		{name: "open", mode: sandboxprofile.ModeOpen, enforcement: k},
		{name: "filtered", mode: sandboxprofile.ModeFiltered, enforcement: k},
		{name: "filtered+proxy", mode: sandboxprofile.ModeFiltered, enforcement: k, proxy: true},
		{name: "filtered+open-port", mode: sandboxprofile.ModeFiltered, enforcement: k, openPort: true},
		{name: "filtered+listen-port", mode: sandboxprofile.ModeFiltered, enforcement: k, listen: true},
		{name: "filtered+proxy+ports", mode: sandboxprofile.ModeFiltered, enforcement: k, proxy: true, openPort: true, listen: true},
		{name: "filtered", mode: sandboxprofile.ModeFiltered, enforcement: env},
		{name: "blocked", mode: sandboxprofile.ModeBlocked, enforcement: k},
	}
}

const (
	usabilityAllowed = "allowed"
	usabilityDenied  = "denied"
	usabilityAny     = "" // depends on the mode: reported, not asserted

	usabilityBindName = "b.sock"
)

type usabilityProbe struct {
	name   string
	want   string
	script string
	args   []string
}

// usabilityFixture holds the folders, sockets and servers the probes use.
// The folders lie under /tmp, which the baseline makes readable but not
// writable, so a write succeeds only through a grant; /tmp also keeps
// socket paths below the 104-byte limit.
type usabilityFixture struct {
	workdir, rw, ro, unixDir, sockGrant, prot, tmp, home string
	proxyPort, openPort, otherPort, listenPort           int
}

func newUsabilityFixture(t *testing.T) *usabilityFixture {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "omac-um-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	tmp, err := os.MkdirTemp("", "omac-um-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(tmp) })
	fx := &usabilityFixture{
		workdir:   filepath.Join(root, "wd"),
		rw:        filepath.Join(root, "rw"),
		ro:        filepath.Join(root, "ro"),
		unixDir:   filepath.Join(root, "ud"),
		sockGrant: filepath.Join(root, "sg"),
		prot:      filepath.Join(root, "prot"),
		tmp:       tmp,
		home:      stagedHome(t),
	}
	for _, d := range []string{fx.workdir, fx.rw, fx.ro, fx.unixDir, fx.sockGrant, fx.prot, filepath.Join(fx.home, ".ssh")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{
		filepath.Join(fx.workdir, "seed.txt"),
		filepath.Join(fx.rw, "seed.txt"),
		filepath.Join(fx.ro, "seed.txt"),
		filepath.Join(fx.prot, "secret.txt"),
		filepath.Join(fx.home, ".ssh", "id_test"),
		filepath.Join(fx.workdir, "signing.keychain-db"),
	} {
		if err := os.WriteFile(f, []byte("seed\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(fx.prot, "run.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, sock := range []string{fx.unixSock(), fx.grantedSock(), fx.protSock()} {
		usabilityServeUnix(t, sock)
	}
	if err := os.Symlink(fx.protSock(), filepath.Join(fx.workdir, "p.sock")); err != nil {
		t.Fatal(err)
	}
	fx.proxyPort = usabilityServeTCP(t)
	fx.openPort = usabilityServeTCP(t)
	fx.otherPort = usabilityServeTCP(t)
	fx.listenPort = usabilityFreePort(t)
	return fx
}

func (fx *usabilityFixture) unixSock() string    { return filepath.Join(fx.unixDir, "d.sock") }
func (fx *usabilityFixture) grantedSock() string { return filepath.Join(fx.sockGrant, "g.sock") }
func (fx *usabilityFixture) protSock() string    { return filepath.Join(fx.prot, "p.sock") }

// grants builds the scenario's grant set. The protected folder is also an
// allow_unix_dir, so only the protection stands between a probe and it.
func (fx *usabilityFixture) grants(t *testing.T, sc usabilityScenario) *Grants {
	t.Helper()
	p := &sandboxprofile.Profile{
		Workdir: sandboxprofile.Workdir{Access: sandboxprofile.AccessReadWrite},
		Filesystem: sandboxprofile.Filesystem{
			Allow:        []string{fx.rw, fx.grantedSock()},
			Read:         []string{fx.ro},
			AllowUnixDir: []string{fx.unixDir, fx.prot},
			Deny:         []string{fx.prot},
		},
		Network: sandboxprofile.Network{Mode: sc.mode, Enforcement: sc.enforcement},
	}
	if sc.openPort {
		p.Network.OpenPort = []int{fx.openPort}
	}
	if sc.listen {
		p.Network.ListenPort = []int{fx.listenPort}
	}
	g, err := ResolveGrants(p, fx.workdir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sc.proxy {
		g.ProxyPort = fx.proxyPort
	}
	return g
}

func (fx *usabilityFixture) probes() []usabilityProbe {
	curlTCP := `/usr/bin/curl -sS --max-time 3 "http://127.0.0.1:$1/" | /usr/bin/grep -q usability-ok`
	curlUnix := `/usr/bin/curl -sS --max-time 3 --unix-socket "$1" http://x/ | /usr/bin/grep -q usability-ok`
	bindUnix := `/usr/bin/perl -MIO::Socket::UNIX -e 'IO::Socket::UNIX->new(Type => SOCK_STREAM(), Local => $ARGV[0], Listen => 1) or exit 3' "$1"`
	listenTCP := `/usr/bin/perl -MIO::Socket::INET -e 'IO::Socket::INET->new(LocalAddr => "127.0.0.1", LocalPort => $ARGV[0], Listen => 1, ReuseAddr => 1) or exit 3' "$1"`
	port := strconv.Itoa
	A, D, X := usabilityAllowed, usabilityDenied, usabilityAny
	return []usabilityProbe{
		{"workdir-read", A, `/bin/cat "$1" >/dev/null`, []string{filepath.Join(fx.workdir, "seed.txt")}},
		{"workdir-write", A, `echo x > "$1"`, []string{filepath.Join(fx.workdir, "new.txt")}},
		{"grant-rw-read", A, `/bin/cat "$1" >/dev/null`, []string{filepath.Join(fx.rw, "seed.txt")}},
		{"grant-rw-write", A, `echo x > "$1"`, []string{filepath.Join(fx.rw, "new.txt")}},
		{"grant-ro-read", A, `/bin/cat "$1" >/dev/null`, []string{filepath.Join(fx.ro, "seed.txt")}},
		{"grant-ro-write", X, `echo x >> "$1"`, []string{filepath.Join(fx.ro, "seed.txt")}},
		{"tmpdir-write", A, `echo x > "$1/f" && /bin/cat "$1/f" >/dev/null`, []string{fx.tmp}},
		{"system-read", A, `/bin/cat /System/Library/CoreServices/SystemVersion.plist >/dev/null && /bin/ls /usr/lib >/dev/null`, nil},
		{"exec-usr-bin", A, `/usr/bin/env /usr/bin/true && /usr/bin/uname >/dev/null`, nil},
		{"dev-null-urandom", A, `echo x > /dev/null && /usr/bin/head -c1 /dev/urandom >/dev/null`, nil},
		{"pty", X, `/usr/bin/script -q /dev/null /bin/sh -c 'test -t 0 && test -t 1'`, nil},
		{"tcp-connect-proxy-port", X, curlTCP, []string{port(fx.proxyPort)}},
		{"tcp-connect-open-port", X, curlTCP, []string{port(fx.openPort)}},
		{"tcp-connect-other-port", X, curlTCP, []string{port(fx.otherPort)}},
		{"tcp-listen", X, listenTCP, []string{port(fx.listenPort)}},
		{"unix-connect-unixdir", X, curlUnix, []string{fx.unixSock()}},
		{"unix-connect-socket-grant", X, curlUnix, []string{fx.grantedSock()}},
		{"unix-bind-unixdir", X, bindUnix, []string{filepath.Join(fx.unixDir, usabilityBindName)}},
		{"unix-bind-workdir", X, bindUnix, []string{filepath.Join(fx.workdir, usabilityBindName)}},
		{"unix-bind-tmpdir", X, bindUnix, []string{filepath.Join(fx.tmp, usabilityBindName)}},
		{"protected-read", D, `/bin/cat "$1" >/dev/null`, []string{filepath.Join(fx.prot, "secret.txt")}},
		{"protected-list", D, `/bin/ls "$1" >/dev/null`, []string{fx.prot}},
		{"protected-write", D, `echo x > "$1"`, []string{filepath.Join(fx.prot, "new.txt")}},
		{"protected-exec", X, `"$1"`, []string{filepath.Join(fx.prot, "run.sh")}},
		{"protected-unix-connect", X, curlUnix, []string{fx.protSock()}},
		{"protected-unix-connect-symlink", X, curlUnix, []string{filepath.Join(fx.workdir, "p.sock")}},
		{"protected-unix-bind", X, bindUnix, []string{filepath.Join(fx.prot, usabilityBindName)}},
		{"protected-ssh-read", D, `/bin/cat "$1" >/dev/null`, []string{filepath.Join(fx.home, ".ssh", "id_test")}},
		{"keychain-name-read", D, `/bin/cat "$1" >/dev/null`, []string{filepath.Join(fx.workdir, "signing.keychain-db")}},
	}
}

// usabilityRun reports whether the shell script exits 0 in the sandbox.
// The script gets args as $1, $2, ...
func usabilityRun(t *testing.T, g *Grants, script string, args ...string) bool {
	t.Helper()
	argv, err := BuildChildArgv(g, append([]string{"/bin/sh", "-c", script, "sh"}, args...))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = g.Workdir
	cmd.WaitDelay = time.Second
	return cmd.Run() == nil
}

func usabilityServeUnix(t *testing.T, sock string) {
	t.Helper()
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go http.Serve(l, usabilityHandler())
}

func usabilityServeTCP(t *testing.T) int {
	t.Helper()
	srv := httptest.NewServer(usabilityHandler())
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().(*net.TCPAddr).Port
}

func usabilityHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "usability-ok") })
}

func usabilityFreePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}
