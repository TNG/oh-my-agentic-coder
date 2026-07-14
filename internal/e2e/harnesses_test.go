//go:build e2e

package e2e

import (
	"runtime"
	"testing"
)

// expectedHarnessNames is allHarnesses' expected content for the current
// GOOS: codex is excluded on darwin (see allHarnesses; its Rust HTTP client
// is incompatible with the macOS Seatbelt sandbox — issue #48).
func expectedHarnessNames() []string {
	names := []string{"opencode", "claude-code", "codex", "copilot", "pi"}
	if runtime.GOOS != "darwin" {
		return names
	}
	out := names[:0:0]
	for _, n := range names {
		if n != "codex" {
			out = append(out, n)
		}
	}
	return out
}

func TestAllHarnessesReturnsFour(t *testing.T) {
	hs := allHarnesses()
	want := len(expectedHarnessNames())
	if len(hs) != want {
		t.Fatalf("expected %d harnesses, got %d", want, len(hs))
	}
}

func TestHarnessByName(t *testing.T) {
	for _, name := range expectedHarnessNames() {
		h, ok := harnessByName(name)
		if !ok {
			t.Fatalf("harnessByName(%q) not found", name)
		}
		if h.Name != name {
			t.Fatalf("harnessByName(%q) returned %q", name, h.Name)
		}
	}
	if runtime.GOOS == "darwin" {
		if _, ok := harnessByName("codex"); ok {
			t.Fatal("harnessByName(\"codex\") should return false on darwin")
		}
	}
	if _, ok := harnessByName("nonexistent"); ok {
		t.Fatal("harnessByName should return false for unknown harness")
	}
}

func TestRunArgsNonEmpty(t *testing.T) {
	for _, h := range allHarnesses() {
		args := h.RunArgs("test prompt")
		if len(args) == 0 {
			t.Fatalf("%s: RunArgs returned empty", h.Name)
		}
	}
}
