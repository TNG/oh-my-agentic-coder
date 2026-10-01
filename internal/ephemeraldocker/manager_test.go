package ephemeraldocker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func baseOpts(t *testing.T, cache string, fl *fakeLima) SessionOpts {
	t.Helper()
	return SessionOpts{
		CacheDir:    cache,
		WorkDir:     "/Users/me/work/proj",
		Arch:        "aarch64",
		BootTimeout: 2 * time.Second,
		Run:         fl.run,
		Reap:        func(string) error { return nil },
		Probe:       func(context.Context, int) bool { return true },
		EnsureImage: func(string, string, string) (ImageSpec, error) {
			return ImageSpec{Path: "/cache/alpine-cloud.qcow2", Digest: "sha512:" + strings.Repeat("a", 128)}, nil
		},
		LookPath: func(string) (string, error) { return "/opt/homebrew/bin/limactl", nil },
		PortFree: func(int) bool { return true },
	}
}

func TestStartSessionHappyPath(t *testing.T) {
	fl := &fakeLima{}
	cache := t.TempDir()
	s, err := StartSession(context.Background(), baseOpts(t, cache, fl))
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	defer s.Teardown()

	if s.HostPort < 30000 || s.HostPort >= 40000 {
		t.Errorf("HostPort %d outside the stable window", s.HostPort)
	}
	if want := fmt.Sprintf("tcp://127.0.0.1:%d", s.HostPort); s.DockerHost() != want {
		t.Errorf("DockerHost() = %q, want %q", s.DockerHost(), want)
	}

	calls := fl.recorded()
	if len(calls) != 3 {
		t.Fatalf("limactl calls = %d (%+v), want create+start+shell", len(calls), calls)
	}
	vm := s.VMName
	if calls[0].home != s.Lay.LimaHome || calls[0].args[0] != "create" || calls[0].args[1] != "--name" || calls[0].args[2] != vm {
		t.Errorf("create call wrong: %+v", calls[0])
	}
	if calls[1].args[0] != "start" || calls[1].args[1] != "--tty=false" || calls[1].args[2] != vm {
		t.Errorf("start call wrong: %+v", calls[1])
	}
	if calls[2].args[0] != "shell" || calls[2].args[1] != vm {
		t.Errorf("firewall verification call wrong: %+v", calls[2])
	}

	// The generated YAML is on disk and carries the session identity.
	raw, err := os.ReadFile(s.Lay.LimaYAML)
	if err != nil {
		t.Fatalf("lima.yaml missing: %v", err)
	}
	if !strings.Contains(string(raw), "vmType: qemu") || !strings.Contains(string(raw), vm) {
		t.Errorf("lima.yaml content unexpected:\n%s", raw)
	}
	if _, err := os.Stat(s.Lay.MarkerPath); err != nil {
		t.Errorf("marker missing: %v", err)
	}

	// Teardown: delete -f, LIMA_HOME gone, dir gone, lock released.
	if err := s.Teardown(); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	calls = fl.recorded()
	var deletes []limaCall
	for _, c := range calls {
		if len(c.args) >= 2 && c.args[0] == "delete" {
			deletes = append(deletes, c)
		}
	}
	if len(deletes) != 1 || deletes[0].args[1] != "-f" || deletes[0].args[2] != vm || deletes[0].home != s.Lay.LimaHome {
		t.Errorf("teardown delete call wrong: %+v", deletes)
	}
	if _, err := os.Stat(s.Lay.Dir); !errors.Is(err, os.ErrNotExist) {
		t.Error("session dir must be gone after teardown")
	}
	if _, err := os.Lstat(s.Lay.LimaHome); !errors.Is(err, os.ErrNotExist) {
		t.Error("LIMA_HOME must be gone after teardown")
	}
	if acquired, cleanup := tryLock(s.Lay.LockPath); !acquired {
		t.Error("liveness lock must be released after teardown")
	} else {
		cleanup()
	}
}

func TestStartSessionOnlyEndpointIsAuthoritative(t *testing.T) {
	// create and start both report failure, but the endpoint answers:
	// the session starts anyway (nur Endpoint zählt).
	for _, sub := range []string{"create", "start"} {
		t.Run(sub, func(t *testing.T) {
			fl := &fakeLima{}
			fl.setFail(func(args []string) error {
				if args[0] == sub {
					return errors.New(sub + " reported failure")
				}
				return nil
			})
			s, err := StartSession(context.Background(), baseOpts(t, t.TempDir(), fl))
			if err != nil {
				t.Fatalf("a failing %s exit code must not block: %v", sub, err)
			}
			defer s.Teardown()
		})
	}
}

func TestStartSessionEndpointTimeoutTearsDown(t *testing.T) {
	fl := &fakeLima{}
	opts := baseOpts(t, t.TempDir(), fl)
	opts.Probe = func(context.Context, int) bool { return false }
	opts.BootTimeout = 30 * time.Millisecond
	s, err := StartSession(context.Background(), opts)
	if err == nil {
		s.Teardown()
		t.Fatal("endpoint timeout must fail the session")
	}
	if s != nil {
		t.Fatal("no session may be returned on timeout")
	}
	var sawDelete bool
	for _, c := range fl.recorded() {
		if len(c.args) >= 1 && c.args[0] == "delete" {
			sawDelete = true
		}
		if c.args[0] == "shell" {
			t.Error("firewall verification must never run when the endpoint never answered")
		}
	}
	if !sawDelete {
		t.Error("timeout must trigger the full teardown (limactl delete -f)")
	}
	// The agent would start from the caller's perspective only on nil
	// error; the unit state must already be gone here.
	if got := fl.recorded(); len(got) < 3 {
		t.Errorf("expected create+start+delete at least, got %+v", got)
	}
}

func TestStartSessionFirewallFailureTearsDown(t *testing.T) {
	fl := &fakeLima{}
	fl.setFail(func(args []string) error {
		if args[0] == "shell" {
			return errors.New("nft table missing")
		}
		return nil
	})
	opts := baseOpts(t, t.TempDir(), fl)
	s, err := StartSession(context.Background(), opts)
	if err == nil {
		s.Teardown()
		t.Fatal("a failed firewall verification must fail the session")
	}
	if s != nil {
		t.Fatal("no session may be returned without the vmguard table")
	}
	var sawDelete bool
	for _, c := range fl.recorded() {
		if len(c.args) >= 1 && c.args[0] == "delete" {
			sawDelete = true
		}
	}
	if !sawDelete {
		t.Error("firewall failure must trigger the full teardown")
	}
	// The failure-path teardown removed the whole unit: no session dirs
	// may remain under the scope root.
	entries, readErr := os.ReadDir(filepath.Join(opts.CacheDir, scopeDirName))
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		t.Fatalf("read scope root: %v", readErr)
	}
	for _, e := range entries {
		if e.IsDir() && isSessionID(e.Name()) {
			t.Errorf("session dir %s must be gone after the failure-path teardown", e.Name())
		}
	}
}

func TestStartSessionMissingLimactlFailsClosed(t *testing.T) {
	fl := &fakeLima{}
	opts := baseOpts(t, t.TempDir(), fl)
	opts.LookPath = func(string) (string, error) { return "", os.ErrNotExist }
	s, err := StartSession(context.Background(), opts)
	if !errors.Is(err, ErrLimaMissing) {
		t.Fatalf("want ErrLimaMissing, got %v", err)
	}
	if s != nil {
		t.Fatal("no session without limactl")
	}
	if len(fl.recorded()) != 0 {
		t.Errorf("no limactl call may happen without the binary: %+v", fl.recorded())
	}
}

func TestStartSessionSweepsOrphansFirst(t *testing.T) {
	fl := &fakeLima{}
	cache := t.TempDir()
	root := filepath.Join(cache, scopeDirName)
	orphan := "999999999999"
	mkUnit(t, root, orphan, &SessionMarker{VMName: "omac-eph-eeee5555", HostPort: 30004}, false)

	s, err := StartSession(context.Background(), baseOpts(t, cache, fl))
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	defer s.Teardown()
	if _, err := os.Stat(filepath.Join(root, orphan)); !errors.Is(err, os.ErrNotExist) {
		t.Error("sweep must have reaped the orphan before booting")
	}
	if _, err := os.Stat(filepath.Join(root, filepath.Base(s.Lay.Dir))); err != nil {
		t.Errorf("the new session's dir must exist: %v", err)
	}
}

func TestStartSessionPortExhaustionFailsCleanly(t *testing.T) {
	fl := &fakeLima{}
	opts := baseOpts(t, t.TempDir(), fl)
	opts.PortFree = func(int) bool { return false }
	s, err := StartSession(context.Background(), opts)
	if !errors.Is(err, ErrNoFreePort) {
		t.Fatalf("want ErrNoFreePort, got %v", err)
	}
	if s != nil {
		t.Fatal("no session without a port")
	}
}

func TestTeardownReportsNameOnDeleteFailure(t *testing.T) {
	fl := &fakeLima{}
	var reaped []string
	opts := baseOpts(t, t.TempDir(), fl)
	opts.Reap = func(vm string) error { reaped = append(reaped, vm); return nil }
	s, err := StartSession(context.Background(), opts)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	fl.setFail(func(args []string) error {
		if args[0] == "delete" {
			return errors.New("delete failed")
		}
		return nil
	})
	err = s.Teardown()
	if err == nil {
		t.Fatal("a failed delete must be reported")
	}
	if !strings.Contains(err.Error(), s.VMName) {
		t.Errorf("the teardown error must name the VM for the next sweep/manual cleanup: %v", err)
	}
	if len(reaped) == 0 || reaped[0] != s.VMName {
		t.Errorf("QEMU reap must run after a failed delete: %v", reaped)
	}
	if _, serr := os.Stat(s.Lay.Dir); !errors.Is(serr, os.ErrNotExist) {
		t.Error("the unit dir must still be removed after a failed delete")
	}
}

func TestTeardownDetectsLeftoverListener(t *testing.T) {
	fl := &fakeLima{}
	opts := baseOpts(t, t.TempDir(), fl)
	portFreeFlip := &flipBool{val: true}
	opts.PortFree = portFreeFlip.isFree
	s, err := StartSession(context.Background(), opts)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	portFreeFlip.val = false // something still listens on the host port
	err = s.Teardown()
	if err == nil {
		t.Fatal("a leftover listener must be reported")
	}
	if !strings.Contains(err.Error(), fmt.Sprint(s.HostPort)) || !strings.Contains(err.Error(), s.VMName) {
		t.Errorf("the error must name the port and VM: %v", err)
	}
}

func TestTeardownIdempotent(t *testing.T) {
	fl := &fakeLima{}
	s, err := StartSession(context.Background(), baseOpts(t, t.TempDir(), fl))
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if err := s.Teardown(); err != nil {
		t.Fatalf("first Teardown: %v", err)
	}
	if err := s.Teardown(); err != nil {
		t.Fatalf("second Teardown must be a no-op: %v", err)
	}
	deletes := 0
	for _, c := range fl.recorded() {
		if len(c.args) >= 1 && c.args[0] == "delete" {
			deletes++
		}
	}
	if deletes != 1 {
		t.Errorf("limactl delete runs = %d, want exactly 1", deletes)
	}
}

type flipBool struct {
	mu  sync.Mutex
	val bool
}

func (f *flipBool) isFree(int) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.val
}

// TestIntegrationRealBoot boots a real VM with real limactl. Opt-in via
// OMAC_EPHEMERAL_DOCKER_E2E=1: the boot needs a working lima guestagent
// (AF_UNIX), which agent sandboxes block — run it host-side, not from an
// omac session. Skips cleanly on non-darwin or without limactl.
func TestIntegrationRealBoot(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("ephemeral-docker is macOS-only (lima/qemu backend)")
	}
	if _, err := FindLimactl(exec.LookPath); err != nil {
		t.Skipf("limactl missing: %v", err)
	}
	if os.Getenv("OMAC_EPHEMERAL_DOCKER_E2E") != "1" {
		t.Skip("real-boot E2E is opt-in: set OMAC_EPHEMERAL_DOCKER_E2E=1 and run host-side (the lima guestagent needs AF_UNIX, which agent sandboxes block)")
	}

	opts := SessionOpts{
		CacheDir:    t.TempDir(),
		WorkDir:     t.TempDir(),
		Arch:        "aarch64",
		BootTimeout: DefaultBootTimeout,
	}
	s, err := StartSession(context.Background(), opts)
	if err != nil {
		t.Fatalf("real boot: %v", err)
	}
	if !DefaultEndpointProbe(context.Background(), s.HostPort) {
		t.Fatalf("endpoint not answering on port %d", s.HostPort)
	}
	if err := s.Teardown(); err != nil {
		t.Fatalf("real teardown: %v", err)
	}
	if portIsFree(s.HostPort) != true {
		t.Fatalf("host port %d still busy after teardown", s.HostPort)
	}
}
