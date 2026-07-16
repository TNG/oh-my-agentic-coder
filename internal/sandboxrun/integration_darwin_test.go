//go:build darwin

package sandboxrun

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tngtech/oh-my-agentic-coder/internal/sandboxprofile"
)

// runSandboxed executes argv under sandbox-exec with the grants and
// returns combined output + exit code.
func runSandboxed(t *testing.T, g *Grants, argv ...string) (string, int) {
	t.Helper()
	full, err := BuildChildArgv(g, argv)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(full[0], full[1:]...)
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			t.Fatalf("exec: %v (%s)", err, out)
		}
	}
	return string(out), code
}

// testGrants builds a minimal grant set around a temp workdir.
func testGrants(t *testing.T) *Grants {
	t.Helper()
	wd := t.TempDir()
	p := &sandboxprofile.Profile{
		Workdir: sandboxprofile.Workdir{Access: sandboxprofile.AccessReadWrite},
		Network: sandboxprofile.Network{Mode: sandboxprofile.ModeBlocked},
	}
	g, err := ResolveGrants(p, wd, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestIntegrationWorkdirReadWrite(t *testing.T) {
	g := testGrants(t)
	f := filepath.Join(g.Workdir, "out.txt")
	_, code := runSandboxed(t, g, "/bin/sh", "-c", fmt.Sprintf("echo hi > %s && cat %s", f, f))
	if code != 0 {
		t.Errorf("workdir rw failed, exit %d", code)
	}
}

func TestIntegrationUngrantedPathDenied(t *testing.T) {
	g := testGrants(t)
	secret := filepath.Join(t.TempDir(), "secret.txt") // sibling tempdir, NOT granted... but
	// t.TempDir() parents may fall under the baseline $TMPDIR write grant on macOS.
	// Use the user's home dir directly instead: ~/.ssh is protected.
	home, _ := os.UserHomeDir()
	sshDir := filepath.Join(home, ".ssh")
	if _, err := os.Stat(sshDir); err != nil {
		t.Skip("no ~/.ssh on this machine")
	}
	_ = secret
	out, code := runSandboxed(t, g, "/bin/ls", sshDir)
	if code == 0 {
		t.Errorf("~/.ssh listing should fail, got: %s", out)
	}
}

func TestIntegrationReadOnlyGrant(t *testing.T) {
	roDir := t.TempDir()
	roFile := filepath.Join(roDir, "cfg.txt")
	if err := os.WriteFile(roFile, []byte("cfg"), 0o644); err != nil {
		t.Fatal(err)
	}
	wd := t.TempDir()
	p := &sandboxprofile.Profile{
		Workdir:    sandboxprofile.Workdir{Access: sandboxprofile.AccessReadWrite},
		Filesystem: sandboxprofile.Filesystem{Read: []string{roDir}},
		Network:    sandboxprofile.Network{Mode: sandboxprofile.ModeBlocked},
	}
	g, err := ResolveGrants(p, wd, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Note: macOS $TMPDIR baseline covers /var/folders/... so reads may
	// be allowed via the baseline; assert the *write* fails and read works.
	if _, code := runSandboxed(t, g, "/bin/cat", roFile); code != 0 {
		t.Error("read of read-granted file failed")
	}
	// Write must fail... unless the tempdir sits under the baseline
	// $TMPDIR write grant. Verify the premise first.
	underTmp := false
	if tmp := os.Getenv("TMPDIR"); tmp != "" {
		if rel, err := filepath.Rel(tmp, roDir); err == nil && !strings.HasPrefix(rel, "..") {
			underTmp = true
		}
	}
	if underTmp {
		t.Skip("test tempdir is under baseline $TMPDIR write grant; cannot assert read-only here")
	}
	if _, code := runSandboxed(t, g, "/bin/sh", "-c", "echo x >> "+roFile); code == 0 {
		t.Error("write to read-only grant should fail")
	}
}

func TestIntegrationProtectedPathDeniedUnderBroadGrant(t *testing.T) {
	// Grant the whole home dir read; ~/.ssh must still be denied.
	home, _ := os.UserHomeDir()
	sshDir := filepath.Join(home, ".ssh")
	if _, err := os.Stat(sshDir); err != nil {
		t.Skip("no ~/.ssh on this machine")
	}
	wd := t.TempDir()
	p := &sandboxprofile.Profile{
		Workdir:    sandboxprofile.Workdir{Access: sandboxprofile.AccessReadWrite},
		Filesystem: sandboxprofile.Filesystem{Read: []string{home}},
		Network:    sandboxprofile.Network{Mode: sandboxprofile.ModeBlocked},
	}
	g, err := ResolveGrants(p, wd, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out, code := runSandboxed(t, g, "/bin/ls", sshDir); code == 0 {
		t.Errorf("~/.ssh must be denied under broad home grant, got: %s", out)
	}
	// The broad grant itself works.
	if _, code := runSandboxed(t, g, "/bin/ls", home); code != 0 {
		t.Error("granted home dir listing failed")
	}
}

// TestIntegrationOverrideDenyGrantsAccess proves the documented
// filesystem.override_deny escape hatch (README's Docker-socket / cloud
// creds recipe) actually works through a real Seatbelt sandbox, not
// just at the Grants-struct level (TestResolveGrantsOverrideDeny in
// grants_test.go only checks ProtectedPaths no longer contains the
// entry).
func TestIntegrationOverrideDenyGrantsAccess(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home")
	}
	sshDir := filepath.Join(home, ".ssh")
	if _, err := os.Stat(sshDir); err != nil {
		t.Skip("no ~/.ssh on this machine")
	}
	wd := t.TempDir()
	p := &sandboxprofile.Profile{
		Workdir: sandboxprofile.Workdir{Access: sandboxprofile.AccessReadWrite},
		Filesystem: sandboxprofile.Filesystem{
			Read:         []string{home},
			OverrideDeny: []string{sshDir},
		},
		Network: sandboxprofile.Network{Mode: sandboxprofile.ModeBlocked},
	}
	g, err := ResolveGrants(p, wd, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out, code := runSandboxed(t, g, "/bin/ls", sshDir); code != 0 {
		t.Errorf("override_deny should punch a hole granting ~/.ssh access, got exit %d: %s", code, out)
	}
}

// TestIntegrationAllowUnixDirGrantsSocketAccess proves
// filesystem.allow_unix_dir — the field the README's Docker/Agent View
// daemon-socket recipes actually use, distinct from the single-socket
// filesystem.allow already covered by TestIntegrationUnixSocketUnderNetworkDeny
// above — grants real AF_UNIX connect access through a real Seatbelt
// sandbox. Only tested structurally today (grants_test.go's
// TestResolveGrantsUnixSocketDir just checks it resolves into
// UnixSocketDirs); this is the missing end-to-end proof that the
// generated SBPL rule actually works, which matters here specifically
// because macOS classifies AF_UNIX connect() as a network operation
// rather than file I/O (sbpl.go).
func TestIntegrationAllowUnixDirGrantsSocketAccess(t *testing.T) {
	sockDir, err := os.MkdirTemp("/tmp", "omac-unixdir-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(sockDir)
	sock := filepath.Join(sockDir, "daemon.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go http.Serve(l, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "unixdir-ok")
	}))

	wd := t.TempDir()
	p := &sandboxprofile.Profile{
		Workdir: sandboxprofile.Workdir{Access: sandboxprofile.AccessReadWrite},
		Filesystem: sandboxprofile.Filesystem{
			AllowUnixDir: []string{sockDir},
		},
		Network: sandboxprofile.Network{Mode: sandboxprofile.ModeFiltered},
	}
	g, err := ResolveGrants(p, wd, nil)
	if err != nil {
		t.Fatal(err)
	}
	out, code := runSandboxed(t, g, "/usr/bin/curl", "-sS", "--max-time", "3",
		"--unix-socket", sock, "http://x/")
	if code != 0 || !strings.Contains(out, "unixdir-ok") {
		t.Errorf("allow_unix_dir should grant AF_UNIX connect access to a socket in the dir: exit=%d out=%s", code, out)
	}
}

func TestIntegrationKeychainDenied(t *testing.T) {
	// The hard guarantee is layered: (a) the keychain DB files under
	// ~/Library/Keychains are protected paths (unreadable even under a
	// broad home grant), and (b) the keychain daemons' mach services
	// are denied. The deterministic, prompt-free check is (a): the
	// legacy in-process keychain API cannot read what it cannot open.
	home, _ := os.UserHomeDir()
	kcDir := filepath.Join(home, "Library", "Keychains")
	if _, err := os.Stat(kcDir); err != nil {
		t.Skip("no ~/Library/Keychains")
	}
	wd := t.TempDir()
	p := &sandboxprofile.Profile{
		Workdir:    sandboxprofile.Workdir{Access: sandboxprofile.AccessReadWrite},
		Filesystem: sandboxprofile.Filesystem{Read: []string{home}}, // broad grant
		Network:    sandboxprofile.Network{Mode: sandboxprofile.ModeBlocked},
	}
	g, err := ResolveGrants(p, wd, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out, code := runSandboxed(t, g, "/bin/ls", kcDir); code == 0 {
		t.Errorf("~/Library/Keychains must be denied even under broad home grant, got: %s", out)
	}
}

func TestIntegrationNetworkDenied(t *testing.T) {
	// Spin up a local HTTP server; blocked-mode sandbox must not reach it.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "reachable")
	}))
	defer srv.Close()
	g := testGrants(t)
	out, code := runSandboxed(t, g, "/usr/bin/curl", "-sS", "--max-time", "3", srv.URL)
	if code == 0 {
		t.Errorf("network must be blocked, curl got: %s", out)
	}
}

func TestIntegrationOpenPortReachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "bridge-ok")
	}))
	defer srv.Close()
	port := srv.Listener.Addr().(*net.TCPAddr).Port

	wd := t.TempDir()
	p := &sandboxprofile.Profile{
		Workdir: sandboxprofile.Workdir{Access: sandboxprofile.AccessReadWrite},
		Network: sandboxprofile.Network{
			Mode:     sandboxprofile.ModeFiltered,
			OpenPort: []int{port},
		},
	}
	g, err := ResolveGrants(p, wd, nil)
	if err != nil {
		t.Fatal(err)
	}
	out, code := runSandboxed(t, g, "/usr/bin/curl", "-sS", "--max-time", "3",
		fmt.Sprintf("http://127.0.0.1:%d/", port))
	if code != 0 || !strings.Contains(out, "bridge-ok") {
		t.Errorf("open_port loopback failed: exit=%d out=%s", code, out)
	}
	// A different local port must stay blocked.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv2.Close()
	port2 := srv2.Listener.Addr().(*net.TCPAddr).Port
	if _, code := runSandboxed(t, g, "/usr/bin/curl", "-sS", "--max-time", "3",
		fmt.Sprintf("http://127.0.0.1:%d/", port2)); code == 0 {
		t.Error("non-granted loopback port must stay blocked")
	}
}

func TestIntegrationUnixSocketUnderNetworkDeny(t *testing.T) {
	sockDir, err := os.MkdirTemp("/tmp", "omac-sock-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(sockDir)
	sock := filepath.Join(sockDir, "bridge.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go http.Serve(l, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "unix-ok")
	}))

	wd := t.TempDir()
	p := &sandboxprofile.Profile{
		Workdir: sandboxprofile.Workdir{Access: sandboxprofile.AccessReadWrite},
		Filesystem: sandboxprofile.Filesystem{
			Allow: []string{sock},
			Read:  []string{sockDir},
		},
		Network: sandboxprofile.Network{Mode: sandboxprofile.ModeFiltered},
	}
	g, err := ResolveGrants(p, wd, nil)
	if err != nil {
		t.Fatal(err)
	}
	out, code := runSandboxed(t, g, "/usr/bin/curl", "-sS", "--max-time", "3",
		"--unix-socket", sock, "http://x/")
	if code != 0 || !strings.Contains(out, "unix-ok") {
		t.Errorf("unix socket under network deny failed: exit=%d out=%s", code, out)
	}
}

func TestIntegrationTmpCanonicalization(t *testing.T) {
	// Grant via /tmp/...; access via /private/tmp/... must work.
	dir, err := os.MkdirTemp("/tmp", "omac-canon-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	file := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	wd := t.TempDir()
	p := &sandboxprofile.Profile{
		Workdir:    sandboxprofile.Workdir{Access: sandboxprofile.AccessReadWrite},
		Filesystem: sandboxprofile.Filesystem{Read: []string{dir}},
		Network:    sandboxprofile.Network{Mode: sandboxprofile.ModeBlocked},
	}
	g, err := ResolveGrants(p, wd, nil)
	if err != nil {
		t.Fatal(err)
	}
	private := "/private" + file
	if _, code := runSandboxed(t, g, "/bin/cat", private); code != 0 {
		t.Errorf("canonicalized path %s not readable", private)
	}
}

func TestIntegrationTMPDIRWritableByDefault(t *testing.T) {
	g := testGrants(t)
	_, code := runSandboxed(t, g, "/bin/sh", "-c", `f="${TMPDIR:-/tmp}/omac-default-write-$$"; echo x > "$f" && rm "$f"`)
	if code != 0 {
		t.Error("default temp dir must be writable")
	}
}

func TestIntegrationExitCodePropagation(t *testing.T) {
	g := testGrants(t)
	_, code := runSandboxed(t, g, "/bin/sh", "-c", "exit 3")
	if code != 3 {
		t.Errorf("exit code = %d, want 3", code)
	}
}

func TestIntegrationGrantsInheritedByChildren(t *testing.T) {
	home, _ := os.UserHomeDir()
	sshDir := filepath.Join(home, ".ssh")
	if _, err := os.Stat(sshDir); err != nil {
		t.Skip("no ~/.ssh")
	}
	g := testGrants(t)
	// Spawn a subshell that spawns ls: restrictions must hold transitively.
	_, code := runSandboxed(t, g, "/bin/sh", "-c", "/bin/sh -c '/bin/ls "+sshDir+"'")
	if code == 0 {
		t.Error("restrictions must be inherited by grandchildren")
	}
}

func TestIntegrationDNSTimeboxSanity(t *testing.T) {
	// Guard: blocked mode shouldn't hang forever on name resolution.
	g := testGrants(t)
	start := time.Now()
	runSandboxed(t, g, "/usr/bin/curl", "-sS", "--max-time", "5", "https://example.com/")
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Errorf("blocked-network curl took %v", elapsed)
	}
}
