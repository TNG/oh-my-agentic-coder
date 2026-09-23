package ephemeraldocker

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
)

func flockExclusive(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

type limaCall struct {
	home string
	args []string
}

type fakeLima struct {
	mu    sync.Mutex
	calls []limaCall
	err   error
}

func (f *fakeLima) run(home string, args ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, limaCall{home: home, args: append([]string(nil), args...)})
	return f.err
}

func (f *fakeLima) recorded() []limaCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]limaCall(nil), f.calls...)
}

type fakeReaper struct {
	mu    sync.Mutex
	names []string
	err   error
}

func (f *fakeReaper) reap(vm string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.names = append(f.names, vm)
	return f.err
}

// mkUnit creates one fake session unit under root. hold keeps its liveness
// lock held for the test's duration (an active session).
func mkUnit(t *testing.T, root, sess string, marker *SessionMarker, hold bool) {
	t.Helper()
	dir := filepath.Join(root, sess)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if marker != nil {
		raw, err := json.Marshal(marker)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, markerName), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	lock, err := os.OpenFile(filepath.Join(dir, lockName), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if hold {
		if err := flockExclusive(lock); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = lock.Close() }) // closing drops the lock
		return
	}
	_ = lock.Close()
}

func unitSymlink(sess string) string {
	return filepath.Join(symlinkDir, symlinkPrefix+sess)
}

func TestSweepReapsOrphan(t *testing.T) {
	root := t.TempDir()
	sess := "0123456789ab"
	sym := unitSymlink(sess)
	if err := os.Symlink(filepath.Join(root, sess), sym); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(sym) })
	mkUnit(t, root, sess, &SessionMarker{VMName: "omac-eph-aaaa1111", HostPort: 39999, Symlink: sym}, false)

	fl := &fakeLima{}
	rp := &fakeReaper{}
	res, errs := Sweep(root, fl.run, rp.reap)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(res.Reaped) != 1 || res.Reaped[0] != "omac-eph-aaaa1111" {
		t.Errorf("Reaped = %v, want [omac-eph-aaaa1111]", res.Reaped)
	}
	calls := fl.recorded()
	if len(calls) != 1 {
		t.Fatalf("limactl calls = %d, want 1", len(calls))
	}
	if calls[0].home != sym || calls[0].args[0] != "delete" || calls[0].args[1] != "-f" || calls[0].args[2] != "omac-eph-aaaa1111" {
		t.Errorf("unexpected delete call: %+v", calls[0])
	}
	if _, err := os.Stat(filepath.Join(root, sess)); !errors.Is(err, os.ErrNotExist) {
		t.Error("orphan session dir must be removed")
	}
	if _, err := os.Lstat(sym); !errors.Is(err, os.ErrNotExist) {
		t.Error("orphan LIMA_HOME alias must be removed")
	}
	if len(rp.names) != 0 {
		t.Errorf("QEMU reap must not run on a clean delete, got %v", rp.names)
	}
}

func TestSweepSkipsActiveSession(t *testing.T) {
	root := t.TempDir()
	sess := "456789abcdef"
	mkUnit(t, root, sess, &SessionMarker{VMName: "omac-eph-bbbb2222", HostPort: 30001, Symlink: unitSymlink(sess)}, true)

	fl := &fakeLima{}
	res, errs := Sweep(root, fl.run, func(string) error { return nil })
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(res.Active) != 1 || res.Active[0] != sess {
		t.Errorf("Active = %v, want [%s]", res.Active, sess)
	}
	if len(res.Reaped) != 0 {
		t.Errorf("active session must never be reaped: %v", res.Reaped)
	}
	if len(fl.recorded()) != 0 {
		t.Error("no limactl call may touch a live session")
	}
	if _, err := os.Stat(filepath.Join(root, sess)); err != nil {
		t.Errorf("active session dir must stay: %v", err)
	}
}

func TestSweepIgnoresForeignEntries(t *testing.T) {
	root := t.TempDir()
	// Foreign: a foreign VM's state, the shared image, junk names.
	if err := os.MkdirAll(filepath.Join(root, "colima"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "alpine-cloud.qcow2"), []byte("img"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "deadbeef"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".DS_Store-dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	res, errs := Sweep(root, func(string, ...string) error { return nil }, func(string) error { return nil })
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(res.Reaped) != 0 || len(res.Active) != 0 {
		t.Errorf("foreign entries must be untouched: %+v", res)
	}
	for _, name := range []string{"colima", "deadbeef", ".DS_Store-dir", "alpine-cloud.qcow2"} {
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			t.Errorf("foreign entry %s must stay: %v", name, err)
		}
	}
}

func TestSweepCleansUnmarkedGarbage(t *testing.T) {
	root := t.TempDir()
	sess := "abc123def456"
	sym := unitSymlink(sess)
	if err := os.Symlink(filepath.Join(root, sess), sym); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(sym) })
	// Lock file exists, lock free, but no marker: layout crashed midway
	// — no VM was ever started, so no limactl call may happen.
	mkUnit(t, root, sess, nil, false)

	fl := &fakeLima{}
	res, errs := Sweep(root, fl.run, func(string) error { return nil })
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(res.Reaped) != 0 {
		t.Errorf("garbage has no VM name to report: %v", res.Reaped)
	}
	if len(fl.recorded()) != 0 {
		t.Error("unmarked garbage must not trigger a limactl delete")
	}
	if _, err := os.Stat(filepath.Join(root, sess)); !errors.Is(err, os.ErrNotExist) {
		t.Error("garbage session dir must be removed")
	}
	if _, err := os.Lstat(sym); !errors.Is(err, os.ErrNotExist) {
		t.Error("garbage LIMA_HOME alias must be removed")
	}
}

func TestSweepTreatsCorruptMarkerAsGarbage(t *testing.T) {
	root := t.TempDir()
	bad := "badbadbad001"
	if err := os.MkdirAll(filepath.Join(root, bad), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, bad, markerName), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	good := "badbadbad002"
	mkUnit(t, root, good, &SessionMarker{VMName: "omac-eph-cccc3333", HostPort: 30002, Symlink: unitSymlink(good)}, false)

	fl := &fakeLima{}
	res, errs := Sweep(root, fl.run, func(string) error { return nil })
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if _, err := os.Stat(filepath.Join(root, bad)); !errors.Is(err, os.ErrNotExist) {
		t.Error("corrupt-marker dir must be removed as garbage")
	}
	if len(res.Reaped) != 1 || res.Reaped[0] != "omac-eph-cccc3333" {
		t.Errorf("the healthy unit must still be reaped: %v", res.Reaped)
	}
	if _, err := os.Stat(filepath.Join(root, good)); !errors.Is(err, os.ErrNotExist) {
		t.Error("the orphan unit must be removed")
	}
}

func TestSweepQEMUReapOnDeleteFailure(t *testing.T) {
	root := t.TempDir()
	sess := "112233445566"
	mkUnit(t, root, sess, &SessionMarker{VMName: "omac-eph-dddd4444", HostPort: 30003, Symlink: unitSymlink(sess)}, false)

	fl := &fakeLima{err: errors.New("limactl delete failed")}
	rp := &fakeReaper{}
	_, errs := Sweep(root, fl.run, rp.reap)
	if len(errs) == 0 {
		t.Fatal("a failed delete must be reported")
	}
	joined := ""
	for _, e := range errs {
		joined += e.Error()
	}
	if !strings.Contains(joined, "omac-eph-dddd4444") {
		t.Errorf("the error must name the VM so the user can clean up manually: %v", errs)
	}
	if len(rp.names) != 1 || rp.names[0] != "omac-eph-dddd4444" {
		t.Errorf("QEMU reap must run after a failed delete: %v", rp.names)
	}
	if _, err := os.Stat(filepath.Join(root, sess)); !errors.Is(err, os.ErrNotExist) {
		t.Error("unit dir must still be removed (error reports the name)")
	}
}

func TestSweepRemovesDanglingAliases(t *testing.T) {
	root := t.TempDir()
	// A dangling alias whose unit is already gone.
	dangling := unitSymlink("998877665544")
	if err := os.Symlink(filepath.Join(root, "gone"), dangling); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(dangling) })
	// An alias whose target exists (an active unit) must survive.
	live := unitSymlink("998877665545")
	if err := os.Symlink(root, live); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(live) })

	_, errs := Sweep(root, func(string, ...string) error { return nil }, func(string) error { return nil })
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if _, err := os.Lstat(dangling); !errors.Is(err, os.ErrNotExist) {
		t.Error("dangling alias must be removed")
	}
	if _, err := os.Lstat(live); err != nil {
		t.Error("alias with existing target must survive")
	}
}

func TestSweepMissingRootIsNoop(t *testing.T) {
	res, errs := Sweep(filepath.Join(t.TempDir(), "nope"), func(string, ...string) error { return nil }, func(string) error { return nil })
	if len(errs) != 0 || len(res.Reaped) != 0 || len(res.Active) != 0 {
		t.Errorf("missing root must be a no-op: %+v %v", res, errs)
	}
}
