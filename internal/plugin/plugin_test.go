package plugin

import (
	"os"
	"path/filepath"
	"strings"
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

// TestBriefingMergesIntoExistingSystemBlock guards the invariant that the
// plugin never increases the number of entries in OpenCode's `system`
// array. OpenCode maps every entry to its own {role:"system"} message
// (session/llm/request.ts), and strict OpenAI-compatible servers — Qwen
// chat templates in particular — reject a system message at index > 0
// with "system message must come first". The briefing and the skills
// manifest must therefore be appended to the last existing block.
//
// This is a source-level guard because the repo has no TypeScript test
// runner; if one is ever added, replace it with a real hook test.
func TestBriefingMergesIntoExistingSystemBlock(t *testing.T) {
	src := string(MultiDirSource())
	for _, banned := range []string{
		"output.system.push(brief",
		"output.system.push(renderManifest",
	} {
		if strings.Contains(src, banned) {
			t.Errorf("%s must not %s: pushing a new system block adds a second "+
				"system message and breaks providers that require exactly one "+
				"at index 0", MultiDirFileName, banned)
		}
	}
	if !strings.Contains(src, "output.system[last] =") {
		t.Errorf("%s no longer merges into the last system block; the "+
			"single-system-message invariant is unenforced", MultiDirFileName)
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
	if _, err := InstallMultiDir(wd, bridge, false); err == nil {
		t.Fatal("expected refusal to overwrite a differing file without --force")
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
