package intent

import (
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestRegistryExplainMore(t *testing.T) {
	r := New(time.Minute)
	t.Cleanup(r.Close)

	if r.ConsumeExplainMore("example.com") {
		t.Error("unset host should return false")
	}

	// Case-insensitive; consumed on read (one-shot).
	r.MarkExplainMore("Example.COM")
	if !r.ConsumeExplainMore("example.com") {
		t.Error("marked host should return true (case-insensitive)")
	}
	if r.ConsumeExplainMore("example.com") {
		t.Error("second consume should return false (one-shot)")
	}

	// URL and host:port forms normalize to the bare host, matching the popup lookup.
	r.MarkExplainMore("https://api.example.com/v2")
	if !r.ConsumeExplainMore("api.example.com") {
		t.Error("URL-form mark should match bare-host consume")
	}
	r.MarkExplainMore("db.example.com:5432")
	if !r.ConsumeExplainMore("db.example.com") {
		t.Error("host:port mark should match bare-host consume")
	}

	// nil-safe.
	var nilReg *Registry
	nilReg.MarkExplainMore("x")
	if nilReg.ConsumeExplainMore("x") {
		t.Error("nil registry should return false")
	}
}

func TestRegistryClearExplainMore(t *testing.T) {
	r := New(time.Minute)
	t.Cleanup(r.Close)

	// Clear retires the flag so a later Consume sees nothing; case-insensitive
	// and normalized the same way as Mark/Consume.
	r.MarkExplainMore("Example.COM")
	r.ClearExplainMore("https://example.com/x")
	if r.ConsumeExplainMore("example.com") {
		t.Error("cleared host should no longer be marked")
	}

	// Idempotent and nil-safe: clearing an unset host or a nil registry is a no-op.
	r.ClearExplainMore("never.marked")
	var nilReg *Registry
	nilReg.ClearExplainMore("x")
}

// TestRegistryRecordAndClearExplainMore guards the serialized behavior of the
// composite operation: it records the intent and retires the explain-more
// flag in one step, so a later GET fallback sees the declared hint, not the
// explain-more re-ask. Also covers the edge the spec called out: an empty
// reason still retires a stale flag (mirroring ClearExplainMore), so a no-op
// POST cannot leave the "re-declare and retry" hint live.
func TestRegistryRecordAndClearExplainMore(t *testing.T) {
	r := New(time.Minute)
	t.Cleanup(r.Close)

	// Mark the host, then retire the flag by posting a fuller intent.
	r.MarkExplainMore("api.example.com")
	r.RecordAndClearExplainMore("api.example.com", "fetch signed release notes")

	// The flag is retired: a later consume sees nothing.
	if r.ConsumeExplainMore("api.example.com") {
		t.Error("composite op should retire the explain-more flag")
	}
	// The intent itself was recorded.
	if e, ok := r.Lookup("api.example.com"); !ok || e.Reason != "fetch signed release notes" {
		t.Errorf("intent not recorded: %+v ok=%v", e, ok)
	}

	// Empty reason does not record an entry but still retires a stale flag.
	r.MarkExplainMore("empty.example")
	r.RecordAndClearExplainMore("empty.example", "   ")
	if r.ConsumeExplainMore("empty.example") {
		t.Error("empty-reason composite op should still retire the explain-more flag")
	}
	if _, ok := r.Lookup("empty.example"); ok {
		t.Error("empty reason should not record an intent entry")
	}

	// Nil-safe.
	var nilReg *Registry
	nilReg.RecordAndClearExplainMore("x", "y")
}

// TestRegistryRecordAndClearExplainMoreIsAtomic proves the composite operation
// holds the registry mutex across both the record and the flag deletion, so a
// concurrent GET (ConsumeExplainMore) can never observe the half-applied state
// where the new intent is recorded but the explain-more flag is still live.
//
// The race this guards: with separate Record + ClearExplainMore calls, a
// concurrent ConsumeExplainMore running in the gap between them would consume
// the still-live flag and return true — reviving the "re-declare and retry"
// hint after the replacement intent was already posted. Under the composite
// operation the concurrent consume must block until both mutations are
// committed, and then see the flag as already gone.
//
// The interleaving is forced with a gate: RecordAndClearExplainMore runs in a
// worker goroutine and signals via recordAndClearEnterHook (fired while it
// holds the mutex, before it deletes the flag). The main goroutine waits for
// that signal, then issues ConsumeExplainMore, which must block on the held
// mutex. It then releases the worker via the released channel and asserts the
// consume returned false. Run under `go test -race`.
func TestRegistryRecordAndClearExplainMoreIsAtomic(t *testing.T) {
	r := New(time.Minute)
	t.Cleanup(r.Close)
	// Restore the hook after the test so it can't leak into other tests.
	t.Cleanup(func() { recordAndClearEnterHook = nil })

	// A live flag for the target the composite op will retire.
	r.MarkExplainMore("api.example.com")

	// Gates used to force the exact interleaving the race lives in.
	entered := make(chan struct{})
	released := make(chan struct{})

	// Wire the in-critical-section hook so the worker signals it has the mutex
	// and is about to delete the flag. The hook fires under the held lock, so
	// the main goroutine's ConsumeExplainMore cannot proceed until the worker
	// returns from the hook and the lock is released.
	recordAndClearEnterHook = func() {
		entered <- struct{}{}
		<-released
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		r.RecordAndClearExplainMore("api.example.com", "fetch signed release notes")
	}()

	// Wait until the worker is inside the critical section, holding the mutex.
	<-entered

	// Now attempt to consume the flag concurrently. With separate Record +
	// ClearExplainMore calls this would race and could return true; under the
	// single-lock composite it must block until the worker commits and the
	// flag is already deleted, then return false.
	consumeDone := make(chan bool, 1)
	go func() {
		consumeDone <- r.ConsumeExplainMore("api.example.com")
	}()

	// Give the consumer a window to (incorrectly) observe the flag. If the
	// composite op were non-atomic, ConsumeExplainMore could complete here
	// with true. Under the held mutex it stays blocked.
	select {
	case got := <-consumeDone:
		t.Fatalf("ConsumeExplainMore must block while RecordAndClearExplainMore holds the mutex; got %v", got)
	case <-time.After(50 * time.Millisecond):
	}

	// Release the worker so it deletes the flag and drops the mutex.
	close(released)

	// The consumer now runs and must see the flag as already retired.
	if got := <-consumeDone; got {
		t.Error("ConsumeExplainMore must return false after the composite op retired the flag, but observed it as live")
	}

	wg.Wait()

	// And the intent itself was recorded.
	if e, ok := r.Lookup("api.example.com"); !ok || e.Reason != "fetch signed release notes" {
		t.Errorf("intent not recorded: %+v ok=%v", e, ok)
	}
}

func TestRegistryExplainMoreTTLExpiry(t *testing.T) {
	r := New(20 * time.Millisecond)
	t.Cleanup(r.Close)
	r.MarkExplainMore("host.example")
	time.Sleep(60 * time.Millisecond)
	if r.ConsumeExplainMore("host.example") {
		t.Error("expired explain-more flag should return false")
	}
}

func TestRegistryRecordLookup(t *testing.T) {
	r := New(time.Minute)
	r.Record("example.com", "fetch release notes")
	e, ok := r.Lookup("example.com")
	if !ok {
		t.Fatal("entry not found")
	}
	if e.Reason != "fetch release notes" {
		t.Errorf("reason = %q", e.Reason)
	}
}

func TestRegistryHostLowercase(t *testing.T) {
	r := New(time.Minute)
	r.Record("EXAMPLE.com", "x")
	if _, ok := r.Lookup("example.com"); !ok {
		t.Error("lowercase lookup failed")
	}
	if _, ok := r.LookupHost("EXAMPLE.COM"); !ok {
		t.Error("LookupHost should lowercase")
	}
}

func TestRegistryPathNormalization(t *testing.T) {
	r := New(time.Minute)
	abs, _ := filepath.Abs("/tmp/./foo")
	r.Record("/tmp/./foo", "read config")
	e, ok := r.Lookup(abs)
	if !ok {
		t.Fatalf("normalized lookup failed; want %q", abs)
	}
	if e.Reason != "read config" {
		t.Errorf("reason = %q", e.Reason)
	}
}

func TestRegistryTTLExpiry(t *testing.T) {
	r := New(20 * time.Millisecond)
	r.Record("ephemeral.example", "short-lived")
	if _, ok := r.Lookup("ephemeral.example"); !ok {
		t.Fatal("entry missing before TTL")
	}
	time.Sleep(40 * time.Millisecond)
	if _, ok := r.Lookup("ephemeral.example"); ok {
		t.Error("entry survived past TTL")
	}
}

func TestRegistryOverwrite(t *testing.T) {
	r := New(time.Minute)
	r.Record("dup.example", "first")
	r.Record("dup.example", "second")
	e, ok := r.Lookup("dup.example")
	if !ok || e.Reason != "second" {
		t.Errorf("overwrite failed: %+v", e)
	}
}

func TestRegistryNilSafe(t *testing.T) {
	var r *Registry
	r.Record("anything", "no-op")
	if _, ok := r.Lookup("anything"); ok {
		t.Error("nil registry should return false")
	}
	if _, ok := r.LookupHost("anything"); ok {
		t.Error("nil registry LookupHost should return false")
	}
}

func TestRegistryEmptyReasonIgnored(t *testing.T) {
	r := New(time.Minute)
	r.Record("empty.example", "")
	if _, ok := r.Lookup("empty.example"); ok {
		t.Error("empty reason should not record")
	}
}

func TestRegistryCloseStopsSweeper(t *testing.T) {
	r := New(time.Minute)
	r.Close()
	// No panic; sweeper stopped. Record after close is still safe
	// (map stays usable; sweeper just no longer runs).
	r.Record("after.example", "x")
	if _, ok := r.Lookup("after.example"); !ok {
		t.Error("record after close should still work")
	}
}

func TestRegistryNormalizeNetworkForms(t *testing.T) {
	// All three forms must resolve to the same bare-host key so a popup
	// lookup by host finds the agent's declaration however it was phrased.
	for _, target := range []string{
		"https://API.example.com/v1/releases",
		"API.example.com:443",
		"api.example.com",
	} {
		r := New(time.Minute)
		r.Record(target, "why")
		if _, ok := r.LookupHost("api.example.com"); !ok {
			t.Errorf("declared %q; host lookup api.example.com missed", target)
		}
	}
}

func TestRegistryLookupSubtree(t *testing.T) {
	r := New(time.Minute)
	root, _ := filepath.Abs("/tmp/proj")
	child := filepath.Join(root, "fixtures", "big.json")
	r.Record(child, "load fixture data")

	// Candidate offered at learn-review is the reduced ancestor dir.
	got := r.LookupSubtree(root)
	if len(got) != 1 || got[0].Reason != "load fixture data" {
		t.Fatalf("subtree lookup of ancestor = %+v; want the child's intent", got)
	}
	// A host entry must never surface in a path subtree lookup.
	r.Record("example.com", "net reason")
	if got := r.LookupSubtree(root); len(got) != 1 {
		t.Errorf("host intent leaked into subtree lookup: %+v", got)
	}
	// Unrelated directory yields nothing.
	other, _ := filepath.Abs("/tmp/other")
	if got := r.LookupSubtree(other); len(got) != 0 {
		t.Errorf("unrelated subtree lookup = %+v; want none", got)
	}
}

func TestRegistryReasonTruncated(t *testing.T) {
	r := New(time.Minute)
	long := ""
	for i := 0; i < maxReasonLen*2; i++ {
		long += "x"
	}
	r.Record("example.com", long)
	e, ok := r.Lookup("example.com")
	if !ok {
		t.Fatal("not found")
	}
	if len(e.Reason) != maxReasonLen {
		t.Errorf("reason length = %d; want %d", len(e.Reason), maxReasonLen)
	}
}

func TestRegistryMaxEntriesEvictsOldest(t *testing.T) {
	r := New(time.Minute)
	// Fill past the cap; the map must never exceed maxEntries.
	for i := 0; i < maxEntries+50; i++ {
		r.Record("host"+string(rune('a'+i%26))+string(rune('0'+i/26))+".example", "r")
	}
	r.mu.Lock()
	n := len(r.entries)
	r.mu.Unlock()
	if n > maxEntries {
		t.Errorf("registry holds %d entries; cap is %d", n, maxEntries)
	}
}
