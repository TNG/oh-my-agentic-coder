package buildmanifest

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGate_FirstEverFails(t *testing.T) {
	// Criterion 4 / 5: first-ever build (no active record) fails the gate.
	leaf := t.TempDir()
	m, _ := Parse([]byte(`version: 1
builds:
  - root: backend
    containers:
      images: [postgres:17]
`))
	host := HostPolicy{MaxHeap: "4g"}
	caps := m.CapabilitySet(host)
	_, err := Gate(leaf, Digest(m), caps)
	if err == nil {
		t.Fatal("first-ever gate should fail")
	}
	var ge *GateError
	if !errors.As(err, &ge) {
		t.Fatalf("want *GateError, got %T: %v", err, err)
	}
	if !ge.FirstEver {
		t.Error("first-ever should be flagged")
	}
	if !strings.Contains(ge.Error(), "no prior approval") {
		t.Errorf("error should mention no prior approval: %v", ge)
	}
}

func TestGate_UnchangedApprovedStartsUnattended(t *testing.T) {
	// Criterion 5: an unchanged approved manifest starts unattended.
	leaf := t.TempDir()
	m, _ := Parse([]byte(`version: 1
builds:
  - root: backend
`))
	host := HostPolicy{MaxHeap: "4g"}
	caps := m.CapabilitySet(host)
	digest := Digest(m)
	if err := Approve(leaf, digest, caps); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	res, err := Gate(leaf, digest, caps)
	if err != nil {
		t.Fatalf("unchanged approved gate should pass: %v", err)
	}
	if res.Digest != digest {
		t.Errorf("Digest = %q, want %q", res.Digest, digest)
	}
	if !res.Capabilities.HasBuildRoot("backend") {
		t.Error("frozen caps missing build root")
	}
}

func TestGate_ChangedMidSessionFailsWithDiff(t *testing.T) {
	// Criterion 5 (second half): effective policy frozen for the session
	// even if the worktree file changes. A mid-session edit changes the
	// digest → gate fails with the consolidated diff + restart instruction.
	leaf := t.TempDir()
	m1, _ := Parse([]byte(`version: 1
builds:
  - root: backend
    containers:
      images: [postgres:16]
`))
	host := HostPolicy{MaxHeap: "4g"}
	caps1 := m1.CapabilitySet(host)
	digest1 := Digest(m1)
	if err := Approve(leaf, digest1, caps1); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	// Mid-session edit: change the image.
	m2, _ := Parse([]byte(`version: 1
builds:
  - root: backend
    containers:
      images: [postgres:17]
`))
	caps2 := m2.CapabilitySet(host)
	_, err := Gate(leaf, Digest(m2), caps2)
	if err == nil {
		t.Fatal("changed manifest gate should fail")
	}
	var ge *GateError
	if !errors.As(err, &ge) {
		t.Fatalf("want *GateError, got %T", err)
	}
	if ge.FirstEver {
		t.Error("not first-ever")
	}
	rendered := ge.Error()
	if !strings.Contains(rendered, "manifest changed since last approval") {
		t.Errorf("error should mention change: %v", rendered)
	}
	// The diff should show postgres:17 added and postgres:16 removed.
	if len(ge.Diff.AddedImages) != 1 || ge.Diff.AddedImages[0] != "postgres:17" {
		t.Errorf("AddedImages = %v, want [postgres:17]", ge.Diff.AddedImages)
	}
	if len(ge.Diff.RemovedImages) != 1 || ge.Diff.RemovedImages[0] != "postgres:16" {
		t.Errorf("RemovedImages = %v, want [postgres:16]", ge.Diff.RemovedImages)
	}
	if !strings.Contains(rendered, "Restart OMAC") {
		t.Errorf("error should instruct restart: %v", rendered)
	}
}

func TestGate_HostCeilingDroppedFails(t *testing.T) {
	// The approval record stores the effective capability set including a
	// HostPolicy snapshot. If the host ceiling later DROPS below what was
	// approved, the stored set is invalid → re-approval forced (even with
	// matching digest).
	leaf := t.TempDir()
	m, _ := Parse([]byte(`version: 1
resources:
  maxHeap: 3g
`))
	hostHi := HostPolicy{MaxHeap: "4g"}
	caps := m.CapabilitySet(hostHi)
	digest := Digest(m)
	if err := Approve(leaf, digest, caps); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	// Host ceiling drops to 2g: the approved 3g request is now above it.
	hostLo := HostPolicy{MaxHeap: "2g"}
	capsLo := m.CapabilitySet(hostLo)
	_, err := Gate(leaf, digest, capsLo)
	if err == nil {
		t.Fatal("dropped ceiling should fail gate")
	}
	var ge *GateError
	if !errors.As(err, &ge) {
		t.Fatalf("want *GateError, got %T", err)
	}
	if !strings.Contains(ge.Error(), "host policy ceiling dropped") {
		t.Errorf("error should mention ceiling drop: %v", ge)
	}
}

func TestGate_SecondRunAfterFirstEverStartsUnattended(t *testing.T) {
	// Criterion 5: the first use PRESENTS the diff AND records approval;
	// the SECOND run (unchanged digest) starts unattended.
	leaf := t.TempDir()
	m, _ := Parse([]byte(`version: 1
builds:
  - root: backend
`))
	host := HostPolicy{MaxHeap: "4g"}
	caps := m.CapabilitySet(host)
	digest := Digest(m)
	// First run: fails with the diff + restart instruction, AND records
	// approval (so the second run can start unattended).
	_, err := Gate(leaf, digest, caps)
	if err == nil {
		t.Fatal("first run should fail with diff")
	}
	// Second run: same digest → unattended.
	res, err := Gate(leaf, digest, caps)
	if err != nil {
		t.Fatalf("second run should start unattended, got: %v", err)
	}
	if res.Digest != digest {
		t.Errorf("Digest = %q, want %q", res.Digest, digest)
	}
}

func TestApprove_RoundTripEnablesUnattended(t *testing.T) {
	// Approve writes both the approval record and the active record, so
	// the next Gate passes unattended.
	leaf := t.TempDir()
	m, _ := Parse([]byte(`version: 1
builds:
  - root: backend
`))
	caps := m.CapabilitySet(HostPolicy{MaxHeap: "4g"})
	digest := Digest(m)
	if err := Approve(leaf, digest, caps); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	// Both files exist.
	if _, err := loadFile(leaf, ApprovalFilename); err != nil {
		t.Errorf("approval file: %v", err)
	}
	if _, err := loadFile(leaf, ActiveFilename); err != nil {
		t.Errorf("active file: %v", err)
	}
	if _, err := Gate(leaf, digest, caps); err != nil {
		t.Errorf("after Approve, Gate should pass: %v", err)
	}
}

func loadFile(leaf, name string) ([]byte, error) {
	return os.ReadFile(filepath.Join(leaf, ControlDir, name))
}
