//go:build e2e || e2e_fast

package e2e

import "testing"

// TestPinnedPackageOverride covers the precedence between the hardcoded
// harnessVersions map, a per-harness E2E_VERSION_* override (wired from the
// e2e workflow's workflow_dispatch *_version inputs), and E2E_USE_LATEST.
// No live agent involved — fast unit test.
func TestPinnedPackageOverride(t *testing.T) {
	t.Setenv("E2E_USE_LATEST", "")
	t.Setenv("E2E_VERSION_OPENCODE", "")

	if got, want := pinnedPackage("opencode"), harnessVersions["opencode"]; got != want {
		t.Errorf("with no override set: pinnedPackage(opencode) = %q, want %q (pinned map)", got, want)
	}

	t.Setenv("E2E_VERSION_OPENCODE", "opencode-ai@9.9.9")
	if got, want := pinnedPackage("opencode"), "opencode-ai@9.9.9"; got != want {
		t.Errorf("with override set: pinnedPackage(opencode) = %q, want %q", got, want)
	}

	t.Setenv("E2E_USE_LATEST", "1")
	if got, want := pinnedPackage("opencode"), "opencode-ai"; got != want {
		t.Errorf("use_latest should win over the override and strip the version: pinnedPackage(opencode) = %q, want %q", got, want)
	}
}

// TestModelIDOverride covers the precedence between the pinned modelIDs map,
// the cross-harness E2E_MODEL override (the workflows' single `model` input),
// and a per-harness E2E_MODEL_<HARNESS> override.
func TestModelIDOverride(t *testing.T) {
	t.Setenv("E2E_MODEL", "")
	t.Setenv("E2E_MODEL_OPENCODE", "")
	t.Setenv("E2E_MODEL_CLAUDE_CODE", "")

	if got, want := modelID("opencode"), modelIDs["opencode"]; got != want {
		t.Errorf("with no override: modelID(opencode) = %q, want %q (pinned map)", got, want)
	}

	// The cross-harness override reaches every harness — including claude-code,
	// which is exactly why claude_code_model exists as a separate input.
	t.Setenv("E2E_MODEL", "vendor/candidate-1")
	if got, want := modelID("opencode"), "vendor/candidate-1"; got != want {
		t.Errorf("cross-harness override: modelID(opencode) = %q, want %q", got, want)
	}
	if got, want := modelID("claude-code"), "vendor/candidate-1"; got != want {
		t.Errorf("cross-harness override: modelID(claude-code) = %q, want %q", got, want)
	}

	t.Setenv("E2E_MODEL_CLAUDE_CODE", "claude-haiku-4-5-20251001")
	if got, want := modelID("claude-code"), "claude-haiku-4-5-20251001"; got != want {
		t.Errorf("per-harness override should win: modelID(claude-code) = %q, want %q", got, want)
	}
	if got, want := modelID("opencode"), "vendor/candidate-1"; got != want {
		t.Errorf("per-harness override must not leak: modelID(opencode) = %q, want %q", got, want)
	}
}

// TestValidateModel locks the one harness-model constraint that exists: the
// claude-code CLI launches only sonnet/haiku, so a cross-harness override
// landing on it must be named as such rather than surfacing as an opaque
// harness startup failure.
func TestValidateModel(t *testing.T) {
	for _, model := range []string{"claude-sonnet-5", "claude-haiku-4-5-20251001", "SONNET"} {
		if err := validateModel("claude-code", model); err != nil {
			t.Errorf("validateModel(claude-code, %q) = %v, want nil", model, err)
		}
	}
	for _, model := range []string{"zai-org/GLM-5.2", "claude-opus-5", ""} {
		if err := validateModel("claude-code", model); err == nil {
			t.Errorf("validateModel(claude-code, %q) = nil, want an error", model)
		}
	}
	// No other harness constrains its model — they all talk to the same
	// openai-compatible gateway.
	for _, h := range allHarnesses() {
		if h.Name == "claude-code" {
			continue
		}
		if err := validateModel(h.Name, "zai-org/GLM-5.2"); err != nil {
			t.Errorf("validateModel(%s, GLM) = %v, want nil", h.Name, err)
		}
	}
}

// TestPinnedModelsAreLaunchable guards the committed pins against the same
// constraint, so a modelIDs edit can't ship a claude-code model the CLI
// refuses to start with.
func TestPinnedModelsAreLaunchable(t *testing.T) {
	for harness, model := range modelIDs {
		if err := validateModel(harness, model); err != nil {
			t.Errorf("pinned modelIDs[%q] = %q is not launchable: %v", harness, model, err)
		}
	}
}

// TestContextLimitOverride covers the declared-window defaults and their
// overrides. A garbage value must fall back rather than declare a zero window.
func TestContextLimitOverride(t *testing.T) {
	t.Setenv("E2E_CONTEXT_LIMIT", "")
	t.Setenv("E2E_OUTPUT_LIMIT", "")
	if got, want := contextLimit(), defaultContextLimit; got != want {
		t.Errorf("contextLimit() = %d, want %d", got, want)
	}
	if got, want := outputLimit(), defaultOutputLimit; got != want {
		t.Errorf("outputLimit() = %d, want %d", got, want)
	}

	t.Setenv("E2E_CONTEXT_LIMIT", "202752")
	t.Setenv("E2E_OUTPUT_LIMIT", "64000")
	if got, want := contextLimit(), 202752; got != want {
		t.Errorf("overridden contextLimit() = %d, want %d", got, want)
	}
	if got, want := outputLimit(), 64000; got != want {
		t.Errorf("overridden outputLimit() = %d, want %d", got, want)
	}

	for _, bad := range []string{"not-a-number", "0", "-5"} {
		t.Setenv("E2E_CONTEXT_LIMIT", bad)
		if got, want := contextLimit(), defaultContextLimit; got != want {
			t.Errorf("E2E_CONTEXT_LIMIT=%q: contextLimit() = %d, want the %d default", bad, got, want)
		}
	}
}

// TestModelEnvVarCompleteness is the model-side counterpart of
// TestVersionEnvVarCompleteness: a harness registered without a modelIDs pin
// or a per-harness override var can't be pointed at a different model.
func TestModelEnvVarCompleteness(t *testing.T) {
	for _, h := range allHarnesses() {
		if _, ok := modelIDs[h.Name]; !ok {
			t.Errorf("harness %q has no entry in modelIDs", h.Name)
		}
		if _, ok := modelEnvVar[h.Name]; !ok {
			t.Errorf("harness %q has no entry in modelEnvVar", h.Name)
		}
	}
}

// TestVersionEnvVarCompleteness guards against a harness being registered
// (allHarnesses(), harnessVersions) without also wiring its workflow_dispatch
// override — the class of bug fixed for pi, which shipped with a pinned
// version but no E2E_VERSION_PI entry, so its version couldn't be overridden
// for a manual run the way every other harness's could.
func TestVersionEnvVarCompleteness(t *testing.T) {
	for _, h := range allHarnesses() {
		if _, ok := harnessVersions[h.Name]; !ok {
			t.Errorf("harness %q has no entry in harnessVersions", h.Name)
		}
		if _, ok := versionEnvVar[h.Name]; !ok {
			t.Errorf("harness %q has no entry in versionEnvVar (add its *_version workflow_dispatch input in .github/workflows/e2e.yml too)", h.Name)
		}
	}
}
