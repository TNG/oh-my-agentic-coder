//go:build linux

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

	"github.com/tngtech/oh-my-agentic-coder/internal/sandboxprofile"
)

// requireBwrap skips when bubblewrap is unavailable (CI containers
// often lack the userns privileges too).
func requireBwrap(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bwrap not installed")
	}
	// Smoke-test: user namespaces may be disabled.
	if err := exec.Command("bwrap", "--ro-bind", "/", "/", "true").Run(); err != nil {
		t.Skipf("bwrap not functional here: %v", err)
	}
}

// runBwrapped builds the argv via BuildBwrapArgv with a trivial inner
// command (no stage2 — Landlock needs the omac binary; fs tests don't).
func runBwrapped(t *testing.T, g *Grants, innerArgv ...string) (string, int) {
	t.Helper()
	argv, err := BuildBwrapArgv(g, innerArgv)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
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

func linuxTestGrants(t *testing.T) *Grants {
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

func TestIntegrationUnboundPathAbsent(t *testing.T) {
	requireBwrap(t)
	g := linuxTestGrants(t)
	out, code := runBwrapped(t, g, "/bin/ls", "/root")
	if code == 0 {
		t.Errorf("/root should be absent, ls got: %s", out)
	}
	// And a probe with test -e must say it doesn't exist (ENOENT, not EACCES).
	out, _ = runBwrapped(t, g, "/bin/sh", "-c", "test -e /root && echo EXISTS || echo ABSENT")
	if !strings.Contains(out, "ABSENT") {
		t.Errorf("unbound path should be ENOENT: %s", out)
	}
}

func TestIntegrationRoBindWriteDenied(t *testing.T) {
	requireBwrap(t)
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
	if _, code := runBwrapped(t, g, "/bin/cat", roFile); code != 0 {
		t.Error("read of ro-bound file failed")
	}
	if _, code := runBwrapped(t, g, "/bin/sh", "-c", "echo x >> "+roFile); code == 0 {
		t.Error("write to ro-bind should fail")
	}
}

func TestIntegrationWorkdirWritable(t *testing.T) {
	requireBwrap(t)
	g := linuxTestGrants(t)
	f := filepath.Join(g.Workdir, "out.txt")
	if _, code := runBwrapped(t, g, "/bin/sh", "-c", fmt.Sprintf("echo hi > %s && cat %s", f, f)); code != 0 {
		t.Error("workdir rw failed")
	}
}

func TestIntegrationProtectedMaskedUnderGrant(t *testing.T) {
	requireBwrap(t)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home")
	}
	sshDir := filepath.Join(home, ".ssh")
	if _, err := os.Stat(sshDir); err != nil {
		t.Skip("no ~/.ssh")
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
	out, _ := runBwrapped(t, g, "/bin/sh", "-c", "ls -A "+sshDir+" | wc -l")
	if strings.TrimSpace(out) != "0" {
		t.Errorf("~/.ssh must appear empty (tmpfs mask), got %q entries", strings.TrimSpace(out))
	}
}

// TestIntegrationOverrideDenyGrantsAccess proves the documented
// filesystem.override_deny escape hatch (README's Docker-socket / cloud
// creds recipe) actually works through a real bwrap sandbox, not just
// at the Grants-struct level (TestResolveGrantsOverrideDeny in
// grants_test.go only checks ProtectedPaths no longer contains the
// entry).
func TestIntegrationOverrideDenyGrantsAccess(t *testing.T) {
	requireBwrap(t)
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
	out, code := runBwrapped(t, g, "/bin/sh", "-c", "ls -A "+sshDir+" | wc -l")
	if code != 0 {
		t.Errorf("override_deny should punch a hole granting ~/.ssh access, got exit %d: %s", code, out)
	}
}

func TestIntegrationStage2LandlockPorts(t *testing.T) {
	requireBwrap(t)
	if !LandlockNetSupported() {
		t.Skipf("Landlock ABI %d < 4", LandlockABI())
	}
	// Spin up two local servers: one on an allowed port, one not.
	allowed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "allowed-ok")
	}))
	defer allowed.Close()
	blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer blocked.Close()
	allowedPort := allowed.Listener.Addr().(*net.TCPAddr).Port
	blockedPort := blocked.Listener.Addr().(*net.TCPAddr).Port

	// Build the omac binary: stage2 must be a real `omac sandbox stage2`
	// invocation (the test binary cannot stand in for it).
	omac := filepath.Join(t.TempDir(), "omac")
	build := exec.Command("go", "build", "-o", omac, "github.com/tngtech/oh-my-agentic-coder/cmd/omac")
	build.Env = os.Environ()
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build omac: %v\n%s", err, out)
	}

	wd := t.TempDir()
	p := &sandboxprofile.Profile{
		Workdir: sandboxprofile.Workdir{Access: sandboxprofile.AccessReadWrite},
		Network: sandboxprofile.Network{
			Mode:     sandboxprofile.ModeFiltered,
			OpenPort: []int{allowedPort},
		},
	}
	g, err := ResolveGrants(p, wd, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Read access to the omac binary dir + curl deps come from baseline.
	g.ReadPaths = append(g.ReadPaths, filepath.Dir(omac))

	stage2 := []string{omac, "sandbox", "stage2"}
	stage2 = append(stage2, Stage2Args(g)...)

	curl, err := exec.LookPath("curl")
	if err != nil {
		t.Skip("curl not installed")
	}
	run := func(port int) int {
		argvTail := append(append([]string{}, stage2...), "--", curl, "-sS", "--max-time", "3",
			fmt.Sprintf("http://127.0.0.1:%d/", port))
		argv, err := BuildBwrapArgv(g, argvTail)
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(argv[0], argv[1:]...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				return ee.ExitCode()
			}
			t.Fatalf("exec: %v (%s)", err, out)
		}
		return 0
	}
	if code := run(allowedPort); code != 0 {
		t.Errorf("allowed port %d unreachable, exit %d", allowedPort, code)
	}
	if code := run(blockedPort); code == 0 {
		t.Errorf("blocked port %d reachable", blockedPort)
	}
}
