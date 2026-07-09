package intent

import (
	"path/filepath"
	"testing"
	"time"
)

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
