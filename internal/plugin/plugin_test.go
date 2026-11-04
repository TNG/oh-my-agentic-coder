package plugin

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

const bridge = ".opencode/plugins"

func TestEmbeddedSourceNonEmpty(t *testing.T) {
	if len(MultiDirSource()) == 0 {
		t.Fatal("embedded plugin source is empty")
	}
}

// TestEmbeddedSourceMatchesCanonical guards the invariant that the
// embedded copy under assets/ stays byte-for-byte identical to the
// canonical plugin OpenCode auto-loads from .opencode/plugins/. If this
// fails, copy .opencode/plugins/omac-multidir.ts over
// internal/plugin/assets/omac-multidir.ts (or vice versa).
func TestEmbeddedSourceMatchesCanonical(t *testing.T) {
	// This test file lives at <repo>/internal/plugin/; the canonical
	// plugin is at <repo>/.opencode/plugins/omac-multidir.ts.
	canonical := filepath.Join("..", "..", ".opencode", "plugins", MultiDirFileName)
	want, err := os.ReadFile(canonical)
	if err != nil {
		t.Skipf("canonical plugin not found (%v); skipping drift check", err)
	}
	if string(want) != string(MultiDirSource()) {
		t.Fatalf("embedded %s drifted from %s; re-sync the two files",
			MultiDirFileName, canonical)
	}
}

func TestInstallIntoEmptyWorkdir(t *testing.T) {
	wd := t.TempDir()
	res, err := InstallMultiDir(wd, bridge, false)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if res.Unchanged || res.Overwrote {
		t.Errorf("first install should be a fresh write, got %+v", res)
	}
	data, err := os.ReadFile(res.Path)
	if err != nil {
		t.Fatalf("read installed: %v", err)
	}
	if string(data) != string(MultiDirSource()) {
		t.Error("installed content differs from embedded source")
	}
	ok, err := IsMultiDirInstalled(wd, bridge)
	if err != nil || !ok {
		t.Errorf("IsMultiDirInstalled = %v, %v; want true, nil", ok, err)
	}
}

func TestInstallIdempotentUnchanged(t *testing.T) {
	wd := t.TempDir()
	if _, err := InstallMultiDir(wd, bridge, false); err != nil {
		t.Fatalf("first install: %v", err)
	}
	res, err := InstallMultiDir(wd, bridge, false)
	if err != nil {
		t.Fatalf("second install: %v", err)
	}
	if !res.Unchanged {
		t.Errorf("re-install of identical content should report Unchanged, got %+v", res)
	}
}

func TestInstallRefusesToClobberWithoutForce(t *testing.T) {
	wd := t.TempDir()
	dest := MultiDirPath(wd, bridge)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, []byte("// local edits\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := InstallMultiDir(wd, bridge, false)
	if err == nil {
		t.Fatal("expected refusal to overwrite a differing file without --force")
	}
	var conflict *ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected *ConflictError, got %T: %v", err, err)
	}
	if conflict.Path != dest {
		t.Errorf("ConflictError.Path = %q, want %q", conflict.Path, dest)
	}
	// With force it overwrites.
	res, err := InstallMultiDir(wd, bridge, true)
	if err != nil {
		t.Fatalf("forced install: %v", err)
	}
	if !res.Overwrote {
		t.Errorf("forced install should report Overwrote, got %+v", res)
	}
}

func TestIsMultiDirInstalledFalseWhenAbsent(t *testing.T) {
	wd := t.TempDir()
	ok, err := IsMultiDirInstalled(wd, bridge)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Error("should report not installed in an empty workdir")
	}
}

func TestInstallMultiDirIn_AbsoluteDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "opencode", "plugins")
	res, err := InstallMultiDirIn(dir, false)
	if err != nil {
		t.Fatalf("install in abs dir: %v", err)
	}
	if res.Path != filepath.Join(dir, MultiDirFileName) {
		t.Errorf("path = %q, want under %q", res.Path, dir)
	}
	ok, err := IsMultiDirInstalledIn(dir)
	if err != nil || !ok {
		t.Errorf("IsMultiDirInstalledIn = %v, %v; want true, nil", ok, err)
	}
}

// A plugin change must record its old hash, or users holding the old copy
// keep it forever after an upgrade (issue #360).
func TestMultiDirHashesUpToDate(t *testing.T) {
	sum := sha256.Sum256(multiDirSource)
	if got := hex.EncodeToString(sum[:]); got != multiDirSHA256 {
		t.Fatalf("assets/omac-multidir.ts changed: add the old multiDirSHA256 (%s) to previousMultiDirSHA256, then set multiDirSHA256 to %s", multiDirSHA256, got)
	}
	if previousMultiDirSHA256[multiDirSHA256] {
		t.Fatal("the current plugin hash must not be listed as a previous version")
	}
}

func TestInstallUpgradesPreviousVersionWithoutForce(t *testing.T) {
	old := []byte("// plugin from an older omac\n")
	sum := sha256.Sum256(old)
	previousMultiDirSHA256[hex.EncodeToString(sum[:])] = true
	t.Cleanup(func() { delete(previousMultiDirSHA256, hex.EncodeToString(sum[:])) })

	dir := t.TempDir()
	dest := MultiDirPathIn(dir)
	if err := os.WriteFile(dest, old, 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := InstallMultiDirIn(dir, false)
	if err != nil {
		t.Fatalf("install over a previous version: %v", err)
	}
	if !res.Upgraded || !res.Overwrote {
		t.Errorf("want Upgraded and Overwrote, got %+v", res)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, multiDirSource) {
		t.Error("the previous version was not replaced with the embedded plugin")
	}
}
