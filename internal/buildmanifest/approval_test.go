package buildmanifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStoreLoadApproval_RoundTrip(t *testing.T) {
	leaf := t.TempDir()
	rec := ApprovalRecord{
		Digest:       "abc123",
		Capabilities: CapabilitySet{BuildRoots: []string{"backend"}, Images: []string{"pgvector/pgvector:pg16"}},
	}
	rec.ApprovedAt = time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	if err := StoreApproval(leaf, rec); err != nil {
		t.Fatalf("StoreApproval: %v", err)
	}
	got, err := LoadApproval(leaf)
	if err != nil {
		t.Fatalf("LoadApproval: %v", err)
	}
	if got.Digest != rec.Digest {
		t.Errorf("Digest = %q, want %q", got.Digest, rec.Digest)
	}
	if len(got.Capabilities.BuildRoots) != 1 || got.Capabilities.BuildRoots[0] != "backend" {
		t.Errorf("BuildRoots = %v", got.Capabilities.BuildRoots)
	}
	// File lives under .omac-control/, not the leaf root.
	path := filepath.Join(leaf, ControlDir, ApprovalFilename)
	if _, err := os.Stat(path); err != nil {
		t.Errorf("approval file not at %s: %v", path, err)
	}
}

func TestLoadApproval_MissingFileIsZero(t *testing.T) {
	leaf := t.TempDir()
	got, err := LoadApproval(leaf)
	if err != nil {
		t.Fatalf("missing approval should not error: %v", err)
	}
	if got.Digest != "" {
		t.Errorf("missing approval should be zero, got %+v", got)
	}
}

func TestStoreLoadActive_RoundTrip(t *testing.T) {
	leaf := t.TempDir()
	rec := ActiveRecord{
		Digest:       "deadbeef",
		Capabilities: CapabilitySet{Registries: []string{"internal"}},
	}
	if err := StoreActive(leaf, rec); err != nil {
		t.Fatalf("StoreActive: %v", err)
	}
	got, err := LoadActive(leaf)
	if err != nil {
		t.Fatalf("LoadActive: %v", err)
	}
	if got.Digest != rec.Digest {
		t.Errorf("Digest = %q, want %q", got.Digest, rec.Digest)
	}
	if len(got.Capabilities.Registries) != 1 || got.Capabilities.Registries[0] != "internal" {
		t.Errorf("Registries = %v", got.Capabilities.Registries)
	}
}

func TestDiff_AddedRemovedImages(t *testing.T) {
	prev := CapabilitySet{Images: []string{"a", "b"}}
	cur := CapabilitySet{Images: []string{"b", "c"}}
	d := Diff(prev, cur)
	if len(d.AddedImages) != 1 || d.AddedImages[0] != "c" {
		t.Errorf("AddedImages = %v, want [c]", d.AddedImages)
	}
	if len(d.RemovedImages) != 1 || d.RemovedImages[0] != "a" {
		t.Errorf("RemovedImages = %v, want [a]", d.RemovedImages)
	}
	if d.IsEmpty() {
		t.Error("diff with added+removed images should be non-empty")
	}
}

func TestDiff_Empty(t *testing.T) {
	cs := CapabilitySet{Images: []string{"a"}}
	d := Diff(cs, cs)
	if !d.IsEmpty() {
		t.Error("identical sets should yield empty diff")
	}
}

func TestDiffRender(t *testing.T) {
	d := CapabilityDiff{
		AddedImages:   []string{"postgres:17"},
		RemovedImages: []string{"postgres:16"},
	}
	out := d.Render()
	for _, want := range []string{"added images", "postgres:17", "removed images", "postgres:16", "Restart OMAC"} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q:\n%s", want, out)
		}
	}
}
