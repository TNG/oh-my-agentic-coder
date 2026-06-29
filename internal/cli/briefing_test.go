package cli

import (
	"testing"

	"github.com/tngtech/oh-my-agentic-coder/internal/config"
)

func claudeHarness(t *testing.T) config.Harness {
	t.Helper()
	h, ok := config.LookupHarness("claude")
	if !ok {
		t.Fatal("claude harness missing")
	}
	return h
}

func TestBriefingInjectionActiveForAgent(t *testing.T) {
	h := claudeHarness(t)
	text, ok := briefingInjection(false, []string{"claude"}, h, "OVERRIDE")
	if !ok {
		t.Fatal("expected injection to be active for the harness's own binary")
	}
	if text != "OVERRIDE" {
		t.Errorf("text = %q; want the override", text)
	}
}

func TestBriefingInjectionSkippedWhenNoSandbox(t *testing.T) {
	h := claudeHarness(t)
	if _, ok := briefingInjection(true, []string{"claude"}, h, ""); ok {
		t.Error("expected injection skipped when noSandbox is true")
	}
}

func TestBriefingInjectionSkippedForNonHarnessBinary(t *testing.T) {
	h := claudeHarness(t)
	if _, ok := briefingInjection(false, []string{"bash"}, h, ""); ok {
		t.Error("expected injection skipped when inner command is not the harness binary")
	}
}

func TestBriefingInjectionSkippedForEmptyInner(t *testing.T) {
	h := claudeHarness(t)
	if _, ok := briefingInjection(false, nil, h, ""); ok {
		t.Error("expected injection skipped for empty inner command")
	}
}
