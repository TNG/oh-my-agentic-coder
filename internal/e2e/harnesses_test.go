//go:build e2e

package e2e

import (
	"testing"
)

func TestAllHarnessesReturnsFour(t *testing.T) {
	hs := allHarnesses()
	if len(hs) != 4 {
		t.Fatalf("expected 4 harnesses, got %d", len(hs))
	}
}

func TestHarnessByName(t *testing.T) {
	for _, name := range []string{"opencode", "claude-code", "codex", "copilot"} {
		h, ok := harnessByName(name)
		if !ok {
			t.Fatalf("harnessByName(%q) not found", name)
		}
		if h.Name != name {
			t.Fatalf("harnessByName(%q) returned %q", name, h.Name)
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
