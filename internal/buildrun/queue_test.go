package buildrun

import (
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAcquire_NoContention(t *testing.T) {
	dir := t.TempDir()
	l, err := Acquire(dir, time.Second)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer l.Release()
	if l.LockPath() != filepath.Join(dir, BuildLockName) {
		t.Errorf("LockPath = %q", l.LockPath())
	}
}

func TestAcquire_SerializesContended(t *testing.T) {
	dir := t.TempDir()
	var order []int32
	var mu sync.Mutex
	record := func(n int32) {
		mu.Lock()
		order = append(order, n)
		mu.Unlock()
	}

	var inFlight int32
	var maxConcurrent int32
	var wg sync.WaitGroup
	for n := int32(0); n < 3; n++ {
		wg.Add(1)
		go func(n int32) {
			defer wg.Done()
			l, err := Acquire(dir, 10*time.Second)
			if err != nil {
				t.Errorf("Acquire %d: %v", n, err)
				return
			}
			defer l.Release()
			cur := atomic.AddInt32(&inFlight, 1)
			if cur > atomic.LoadInt32(&maxConcurrent) {
				atomic.StoreInt32(&maxConcurrent, cur)
			}
			record(n)
			time.Sleep(50 * time.Millisecond)
			atomic.AddInt32(&inFlight, -1)
		}(n)
	}
	wg.Wait()

	if atomic.LoadInt32(&maxConcurrent) != 1 {
		t.Errorf("max concurrent builds = %d, want 1 (queue must serialize)", maxConcurrent)
	}
	if len(order) != 3 {
		t.Errorf("recorded %d runs, want 3", len(order))
	}
}

func TestAcquire_TimeoutDenies(t *testing.T) {
	dir := t.TempDir()
	holder, err := Acquire(dir, time.Second)
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	defer holder.Release()

	// A short timeout must deny while the holder keeps the lock.
	start := time.Now()
	_, err = Acquire(dir, 200*time.Millisecond)
	d := time.Since(start)
	if err == nil {
		t.Fatal("expected busy denial, got lock")
	}
	if !errors.Is(err, ErrLockBusy) {
		t.Errorf("err = %v, want ErrLockBusy", err)
	}
	if d < 150*time.Millisecond {
		t.Errorf("denied after %v, want to wait at least the 200ms timeout", d)
	}
	if d > 2*time.Second {
		t.Errorf("denied after %v, want to wait no longer than ~timeout", d)
	}
}

func TestAcquire_DeadlockNotStale(t *testing.T) {
	// Release without deleting: the next Acquire must still work (the
	// lockfile persists; the kernel released the flock on close).
	dir := t.TempDir()
	l1, err := Acquire(dir, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	l1.Release()
	l2, err := Acquire(dir, time.Second)
	if err != nil {
		t.Fatalf("second Acquire after Release: %v", err)
	}
	l2.Release()
}

func TestRelease_NilSafe(t *testing.T) {
	var l *BuildLock
	l.Release() // must not panic
}

// TestAcquireCtx_CancelledWhileWaiting asserts that a contended waiter
// is individually cancellable (S2: spec.md:136): two goroutines contend,
// the holder keeps the lock, and the waiter is cancelled while waiting.
// The waiter must return ErrLockCancelled promptly — NOT wait the full
// 30s timeout, and NOT get the errLockBusy denial. This is the
// "cancelled-while-waiting -> ExitCancelled (4)" outcome, distinct from
// the "timed-out-waiting -> ExitServiceFailure (10)" busy path.
func TestAcquireCtx_CancelledWhileWaiting(t *testing.T) {
	dir := t.TempDir()
	holder, err := Acquire(dir, time.Second)
	if err != nil {
		t.Fatalf("holder Acquire: %v", err)
	}
	defer holder.Release()

	cancel := make(chan struct{})
	start := time.Now()
	// Long timeout: a non-cancellable Acquire would wait the full 30s.
	// The cancelled waiter must return well before that. Run the acquire
	// in a goroutine and cancel it after a beat so the holder keeps the
	// lock the whole time (the waiter is contended, then cancelled).
	type res struct {
		err error
		d   time.Duration
	}
	done := make(chan res, 1)
	go func() {
		s := time.Now()
		_, err := AcquireCtx(dir, 30*time.Second, cancel)
		done <- res{err: err, d: time.Since(s)}
	}()
	time.Sleep(200 * time.Millisecond) // let the waiter block on the contended lock
	close(cancel)
	r := <-done
	if r.err == nil {
		t.Fatal("expected cancellation error, got the lock")
	}
	if !errors.Is(r.err, ErrLockCancelled) {
		t.Errorf("err = %v, want ErrLockCancelled (not the 30s timeout denial)", r.err)
	}
	// Must return promptly (within ~1s of the cancel), not after 30s.
	if r.d > 2*time.Second {
		t.Errorf("cancelled waiter took %v; must return promptly after cancel, not the full timeout", r.d)
	}
	_ = start
}

// TestAcquireCtx_CancelAfterHolderReleases asserts that if the holder
// releases before the cancel fires, the waiter acquires normally (the
// cancel channel is only consulted while contended).
func TestAcquireCtx_CancelAfterHolderReleases(t *testing.T) {
	dir := t.TempDir()
	holder, err := Acquire(dir, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	// Holder releases almost immediately; the waiter's cancel never fires.
	go func() {
		time.Sleep(100 * time.Millisecond)
		holder.Release()
	}()
	cancel := make(chan struct{}) // never closed
	l, err := AcquireCtx(dir, 5*time.Second, cancel)
	if err != nil {
		t.Fatalf("AcquireCtx: %v", err)
	}
	defer l.Release()
}
