package buildmanifest

import (
	"testing"
)

func TestDigest_Deterministic(t *testing.T) {
	// Same content, different MAP-KEY order / whitespace → same digest.
	// (Image LIST order is content, not formatting, so it must affect the
	// digest; only map-key order and whitespace are canonicalized away.)
	a, _ := Parse([]byte(`version: 1
builds:
  - root: backend
    tool: gradle
    containers:
      images:
        - pgvector/pgvector:pg16
        - minio/minio:latest
`))
	b, _ := Parse([]byte(`version: 1
builds:
  - root: backend
    containers:
      images:
        - pgvector/pgvector:pg16
        - minio/minio:latest
    tool: gradle
`))
	da, db := Digest(a), Digest(b)
	if da != db {
		t.Errorf("digest not deterministic across map-key order:\n  a=%s\n  b=%s", da, db)
	}
	if da == "" {
		t.Error("digest should not be empty for a non-empty manifest")
	}
}

func TestDigest_ImageOrderIsContent(t *testing.T) {
	// Image list order IS content: reordering images changes the digest
	// (the manifest declares an ordered approved-image list).
	a, _ := Parse([]byte(`version: 1
builds:
  - root: backend
    containers:
      images: [pgvector/pgvector:pg16, minio/minio:latest]
`))
	b, _ := Parse([]byte(`version: 1
builds:
  - root: backend
    containers:
      images: [minio/minio:latest, pgvector/pgvector:pg16]
`))
	if Digest(a) == Digest(b) {
		t.Error("image list order should be content (affect the digest)")
	}
}

func TestDigest_DifferentContentDifferentDigest(t *testing.T) {
	a, _ := Parse([]byte(`version: 1
builds:
  - root: backend
    containers:
      images: [postgres:17]
`))
	b, _ := Parse([]byte(`version: 1
builds:
  - root: backend
    containers:
      images: [postgres:16]
`))
	if Digest(a) == Digest(b) {
		t.Error("different manifests must have different digests")
	}
}

func TestDigest_ZeroManifestStable(t *testing.T) {
	z1 := &Manifest{}
	z2 := &Manifest{}
	if Digest(z1) != Digest(z2) {
		t.Error("zero manifests should have equal digests")
	}
	// And distinct from a non-empty manifest.
	nonEmpty, _ := Parse([]byte(`version: 1
builds:
  - root: backend
`))
	if Digest(z1) == Digest(nonEmpty) {
		t.Error("zero manifest digest must differ from non-empty")
	}
}

func TestCapabilitySet_PostCeiling(t *testing.T) {
	m, _ := Parse([]byte(`version: 1
builds:
  - root: backend
    containers:
      images: [pgvector/pgvector:pg16]
registries:
  - alias: internal
    upstream: ghcr.io/tng
resources:
  maxHeap: 3g
`))
	host := HostPolicy{MaxHeap: "4g"}
	cs := m.CapabilitySet(host)
	if !cs.HasBuildRoot("backend") {
		t.Error("missing build root backend")
	}
	if !cs.HasImage("pgvector/pgvector:pg16") {
		t.Error("missing image")
	}
	if !cs.HasRegistry("internal") {
		t.Error("missing registry internal")
	}
	if cs.Resources.MaxHeap != "3g" {
		t.Errorf("Resources.MaxHeap = %q, want 3g", cs.Resources.MaxHeap)
	}
	if cs.HostPolicy != host {
		t.Error("HostPolicy snapshot not stored")
	}
}

func TestCapabilitySet_HostPolicyIncludedInEquality(t *testing.T) {
	m, _ := Parse([]byte(`version: 1
resources:
  maxHeap: 2g
`))
	cs1 := m.CapabilitySet(HostPolicy{MaxHeap: "4g"})
	cs2 := m.CapabilitySet(HostPolicy{MaxHeap: "2g"})
	// Same manifest, different host ceiling → different capability sets.
	if cs1.Equal(cs2) {
		t.Error("capability sets with different host ceilings should not be equal")
	}
}

func TestSliceMinus(t *testing.T) {
	if got := sliceMinus([]string{"a", "b", "c"}, []string{"b"}); !equalStr(got, []string{"a", "c"}) {
		t.Errorf("sliceMinus = %v, want [a c]", got)
	}
}

func equalStr(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestDigest_IsHexSHA256(t *testing.T) {
	m, _ := Parse([]byte(`version: 1
builds:
  - root: backend
`))
	d := Digest(m)
	if len(d) != 64 || !isHex(d) {
		t.Errorf("digest should be 64-char hex SHA-256, got %q", d)
	}
}

func isHex(s string) bool {
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}
