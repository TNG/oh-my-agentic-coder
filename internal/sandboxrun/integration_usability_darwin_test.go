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
// opens or closes. A probe with an expected result fails the test when it
// differs; see the want field of each probe in usabilityFixture.probes.
func TestIntegrationUsabilityMatrix(t *testing.T) {
	fx := newUsabilityFixture(t)
	// Every probe must succeed outside the sandbox, so a "denied" row can
	// only come from the profile.
	for _, pr := range fx.probes() {
		out, err := usabilityRunArgv(append([]string{"/bin/sh", "-c", pr.script, "sh"}, pr.args...), fx.workdir)
		if pr.creates != "" {
			os.Remove(pr.creates)
		}
		if err != nil {
			t.Fatalf("probe %s fails outside the sandbox: %v: %s", pr.name, err, out)
		}
	}
	for _, sc := range usabilityScenarios() {
		t.Run(sc.name+"/"+sc.enforcement, func(t *testing.T) {
			g := fx.grants(t, sc)
			for _, pr := range fx.probes() {
				out, err := usabilityRun(t, g, pr.script, pr.args...)
				if pr.creates != "" {
					os.Remove(pr.creates)
				}
				result := usabilityAllowed
				if err != nil {
					result = usabilityDenied
					t.Logf("%s denied: %v: %s", pr.name, err, out)
				}
				fmt.Printf("MATRIX %s/%s/%s %s\n", sc.name, sc.enforcement, pr.name, result)
				if want := pr.expect(sc); want != usabilityAny && result != want {
					t.Errorf("%s: got %s, want %s", pr.name, result, want)
				}
			}
		})
	}
}

type usabilityScenario struct {
	name, mode, enforcement string
	proxy, openPort, listen bool
}

// allowsAllNetwork reports whether the profile emits (allow network*).
func (sc usabilityScenario) allowsAllNetwork() bool {
	return sc.mode == sandboxprofile.ModeOpen || sc.enforcement == sandboxprofile.EnforceEnvOnly
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
	usabilityAny     = "" // reported, not asserted
)

type usabilityProbe struct {
	name string
	// want is the result in every scenario. A network probe without one
	// must be allowed where the profile allows all network.
	want    string
	network bool
	script  string
	args    []string
	// creates is removed after the probe, so each scenario starts clean.
	creates string
}

func (pr usabilityProbe) expect(sc usabilityScenario) string {
	if pr.want == usabilityAny && pr.network && sc.allowsAllNetwork() {
		return usabilityAllowed
	}
	return pr.want
}

// usabilityFixture holds the folders, sockets and servers the probes use.
// Most folders lie under /tmp, which the baseline makes readable but not
// writable, so a write there succeeds only through a grant; /tmp also keeps
// socket paths below the 104-byte limit. tmp and home lie under $TMPDIR,
// like a session's temp folder.
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
	trueBin, err := os.ReadFile("/usr/bin/true")
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{fx.workdir, fx.rw, fx.tmp, fx.prot} {
		if err := os.WriteFile(filepath.Join(d, "true"), trueBin, 0o700); err != nil {
			t.Fatal(err)
		}
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
	// --noproxy: a proxy set in the caller's environment must not answer
	// for the sandbox.
	curlTCP := `/usr/bin/curl -sS --noproxy '*' --max-time 3 "http://127.0.0.1:$1/" | /usr/bin/grep -q usability-ok`
	curlUnix := `/usr/bin/curl -sS --noproxy '*' --max-time 3 --unix-socket "$1" http://x/ | /usr/bin/grep -q usability-ok`
	bindUnix := `/usr/bin/perl -MIO::Socket::UNIX -e 'IO::Socket::UNIX->new(Type => SOCK_STREAM(), Local => $ARGV[0], Listen => 1) or exit 3' "$1"`
	listenTCP := `/usr/bin/perl -MIO::Socket::INET -e 'IO::Socket::INET->new(LocalAddr => "127.0.0.1", LocalPort => $ARGV[0], Listen => 1, ReuseAddr => 1) or exit 3' "$1"`
	read := `/bin/cat "$1" >/dev/null`
	write := `echo x > "$1"`
	run := `"$1"`
	port := strconv.Itoa
	join := filepath.Join
	A, D, X := usabilityAllowed, usabilityDenied, usabilityAny
	file := func(name, want, script, path string) usabilityProbe {
		return usabilityProbe{name: name, want: want, script: script, args: []string{path}}
	}
	created := func(name, want, script, path string) usabilityProbe {
		return usabilityProbe{name: name, want: want, script: script, args: []string{path}, creates: path}
	}
	netProbe := func(name, script, arg string) usabilityProbe {
		return usabilityProbe{name: name, network: true, script: script, args: []string{arg}}
	}
	bind := func(name, want, dir string) usabilityProbe {
		path := join(dir, "b.sock")
		return usabilityProbe{name: name, want: want, network: true, script: bindUnix, args: []string{path}, creates: path}
	}
	return []usabilityProbe{
		file("workdir-read", A, read, join(fx.workdir, "seed.txt")),
		created("workdir-write", A, write, join(fx.workdir, "new.txt")),
		file("workdir-exec", A, run, join(fx.workdir, "true")),
		file("grant-rw-read", A, read, join(fx.rw, "seed.txt")),
		created("grant-rw-write", A, write, join(fx.rw, "new.txt")),
		file("grant-rw-exec", A, run, join(fx.rw, "true")),
		file("grant-ro-read", A, read, join(fx.ro, "seed.txt")),
		file("grant-ro-write", D, `echo x >> "$1"`, join(fx.ro, "seed.txt")),
		created("tmpdir-write", A, write+` && `+read, join(fx.tmp, "new.txt")),
		file("tmpdir-exec", A, run, join(fx.tmp, "true")),
		{name: "system-read", want: A, script: `/bin/cat /System/Library/CoreServices/SystemVersion.plist >/dev/null && /bin/ls /usr/lib >/dev/null`},
		{name: "exec-usr-bin", want: A, script: `/usr/bin/env /usr/bin/true && /usr/bin/uname >/dev/null`},
		{name: "dev-null-urandom", want: A, script: `echo x > /dev/null && /usr/bin/head -c1 /dev/urandom >/dev/null`},
		{name: "pty", want: A, script: `/usr/bin/script -q /dev/null /bin/sh -c 'test -t 0 && test -t 1'`},
		netProbe("tcp-connect-proxy-port", curlTCP, port(fx.proxyPort)),
		netProbe("tcp-connect-open-port", curlTCP, port(fx.openPort)),
		netProbe("tcp-connect-other-port", curlTCP, port(fx.otherPort)),
		netProbe("tcp-listen", listenTCP, port(fx.listenPort)),
		file("unix-connect-unixdir", A, curlUnix, fx.unixSock()),
		file("unix-connect-socket-grant", A, curlUnix, fx.grantedSock()),
		bind("unix-bind-unixdir", X, fx.unixDir),
		bind("unix-bind-workdir", X, fx.workdir),
		bind("unix-bind-tmpdir", X, fx.tmp),
		file("protected-read", D, read, join(fx.prot, "secret.txt")),
		file("protected-list", D, `/bin/ls "$1" >/dev/null`, fx.prot),
		created("protected-write", D, write, join(fx.prot, "new.txt")),
		file("protected-exec", D, run, join(fx.prot, "true")),
		file("protected-unix-connect", D, curlUnix, fx.protSock()),
		file("protected-unix-connect-symlink", D, curlUnix, join(fx.workdir, "p.sock")),
		bind("protected-unix-bind", D, fx.prot),
		file("protected-ssh-read", D, read, join(fx.home, ".ssh", "id_test")),
		file("keychain-name-read", D, read, join(fx.workdir, "signing.keychain-db")),
	}
}

// usabilityRun runs the shell script in the sandbox with args as $1, $2, ...
// and returns its output and exit error.
func usabilityRun(t *testing.T, g *Grants, script string, args ...string) (string, error) {
	t.Helper()
	argv, err := BuildChildArgv(g, append([]string{"/bin/sh", "-c", script, "sh"}, args...))
	if err != nil {
		t.Fatal(err)
	}
	return usabilityRunArgv(argv, g.Workdir)
}

func usabilityRunArgv(argv []string, dir string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	return string(out), err
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
