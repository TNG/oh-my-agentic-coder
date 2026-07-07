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
