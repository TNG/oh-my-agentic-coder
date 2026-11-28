//go:build linux

package gitservice

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestWorkerKernelBoundary(t *testing.T) {
	dir := gitFixture(t)
	outside := filepath.Join(t.TempDir(), "host-secret")
	writeFile(t, outside, "host-only")
	writeFile(t, filepath.Join(dir, ".env"), "project-secret")
	writeFile(t, filepath.Join(dir, "deployment", "local", ".env"), "nested-secret")
	s := newTestService(t, dir, filepath.Join(dir, ".env"), filepath.Join(dir, "deployment", "local", ".env"))
	writeFile(t, filepath.Join(dir, "source"), "editable")
	if err := os.Symlink(outside, filepath.Join(dir, "alias")); err != nil {
		t.Fatal(err)
	}
	ns, err := os.Readlink("/proc/self/ns/net")
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, writable := range []bool{false, true} {
		argv, err := s.workerArgv(true, writable, nil)
		if err != nil {
			t.Fatal(err)
		}
		// Replace only the test worker's fixed Git executable with this probe.
		argv = argv[:slices.Index(argv, "--")]
		argv = append(argv, "--ro-bind", self, "/probe", "--setenv", "OMAC_GIT_TEST_PROBE", "1")
		if writable {
			argv = append(argv, "--setenv", "OMAC_GIT_TEST_WRITE", "1")
		}
		argv = append(argv, "--setenv", "OMAC_GIT_TEST_OUTSIDE", outside,
			"--setenv", "OMAC_GIT_TEST_NETNS", ns,
			"--setenv", "OMAC_GIT_TEST_ADDRESS", listener.Addr().String(),
			"--", "/probe", "-test.run=^TestWorkerConfinementProbe$", "-test.v")
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		cmd := exec.CommandContext(ctx, "/usr/bin/bwrap", argv...)
		cmd.Env = []string{"PATH=/usr/bin:/bin", "AWS_SECRET_ACCESS_KEY=must-not-inherit", "OMAC_FACADE_TOKEN=must-not-inherit", "GIT_CONFIG_COUNT=1"}
		out, err := cmd.CombinedOutput()
		cancel()
		if err != nil || !strings.Contains(string(out), "PASS: TestWorkerConfinementProbe") {
			t.Fatalf("confinement (project write %t): %v\n%s", writable, err, out)
		}
	}
	if data, err := os.ReadFile(outside); err != nil || string(data) != "host-only" {
		t.Fatalf("host canary changed: %q %v", data, err)
	}
}

func TestWorkerConfinementProbe(t *testing.T) {
	if os.Getenv("OMAC_GIT_TEST_PROBE") != "1" {
		t.Skip("runs only as a child of TestWorkerKernelBoundary")
	}
	for _, name := range []string{"AWS_SECRET_ACCESS_KEY", "OMAC_FACADE_TOKEN", "GIT_CONFIG_COUNT", "SSH_AUTH_SOCK"} {
		if _, ok := os.LookupEnv(name); ok {
			t.Errorf("inherited %s", name)
		}
	}
	for _, path := range []string{os.Getenv("OMAC_GIT_TEST_OUTSIDE"), "/repo/alias"} {
		if data, err := os.ReadFile(path); err == nil {
			t.Errorf("read host canary %s: %q", path, data)
		}
	}
	for _, path := range []string{"/repo/.env", "/repo/deployment/local/.env"} {
		if data, err := os.ReadFile(path); (err != nil && !os.IsPermission(err)) || len(data) != 0 {
			t.Errorf("project deny did not mask secret: %q %v", data, err)
		}
	}
	for _, path := range []string{"/repo/deployment", "/repo/deployment/local"} {
		if err := os.Rename(path, "/repo/moved"); err == nil {
			t.Errorf("worker relocated a protected parent: %s", path)
		}
	}
	if err := os.WriteFile("/repo/.git/worker-control", []byte("metadata writable"), 0600); err != nil {
		t.Fatalf("positive metadata write control: %v", err)
	}
	for _, path := range []string{"/repo/.git/config", "/omac/config", "/usr/bin/worker-canary", "/repo/.env"} {
		if err := os.WriteFile(path, []byte("poison"), 0600); err == nil {
			t.Errorf("wrote read-only %s", path)
		}
	}
	err := os.WriteFile("/repo/source", []byte("updated"), 0600)
	if (err == nil) != (os.Getenv("OMAC_GIT_TEST_WRITE") == "1") {
		t.Errorf("project write permission: %v", err)
	}
	if err := os.WriteFile("/tmp/private", []byte("temporary"), 0600); err != nil {
		t.Errorf("private temp unusable: %v", err)
	}
	ns, err := os.Readlink("/proc/self/ns/net")
	if err != nil || ns == os.Getenv("OMAC_GIT_TEST_NETNS") {
		t.Fatalf("network namespace not isolated: %q %v", ns, err)
	}
	conn, err := net.DialTimeout("tcp", os.Getenv("OMAC_GIT_TEST_ADDRESS"), time.Second)
	if err == nil {
		conn.Close()
		t.Fatal("worker reached host listener")
	}
}
