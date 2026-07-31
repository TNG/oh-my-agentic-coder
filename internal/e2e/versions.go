//go:build e2e || e2e_fast

package e2e

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Versions and model configuration for e2e tests.
//
// Bump these when testing a new harness release or model.
// Set E2E_USE_LATEST=1 in CI or locally to skip pinning
// and install the latest version instead (for fast testing).

// Harness versions (last-known-good as of 2026-07-01).
var harnessVersions = map[string]string{
	"opencode":    "opencode-ai@1.17.12",
	"claude-code": "@anthropic-ai/claude-code@2.1.197",
	"codex":       "@openai/codex@0.142.5",
	"copilot":     "@github/copilot@1.0.68",
	"pi":          "@earendil-works/pi-coding-agent@0.80.6",
}

// versionEnvVar maps a harness name to the env var that can override its
// pinned package spec for a single run, without editing this file. Wired
// from the e2e workflow's workflow_dispatch *_version inputs
// (.github/workflows/e2e.yml); unset in the scheduled run, so that run
// always uses the harnessVersions map above.
var versionEnvVar = map[string]string{
	"opencode":    "E2E_VERSION_OPENCODE",
	"claude-code": "E2E_VERSION_CLAUDE_CODE",
	"codex":       "E2E_VERSION_CODEX",
	"copilot":     "E2E_VERSION_COPILOT",
	"pi":          "E2E_VERSION_PI",
}

// Model identifiers per harness. Read through modelID(), never directly, so
// a run's workflow_dispatch override is honoured.
//
// The gateway serves one name variant at a time and flips between the plain
// name and a "-TEE" suffixed one without notice — on 2026-07-29 it stopped
// accepting plain "zai-org/GLM-5.2" and every non-opencode llm stage went red
// (see issue #184). Pin whichever variant is currently served;
// scripts/probe-model.sh flips to the other one automatically, so a future flip
// in either direction self-heals rather than reddening the matrix.
var modelIDs = map[string]string{
	"opencode":    "zai-org/GLM-5.2-TEE",
	"claude-code": "claude-sonnet-5",
	"codex":       "zai-org/GLM-5.2-TEE",
	"copilot":     "zai-org/GLM-5.2-TEE",
	"pi":          "zai-org/GLM-5.2-TEE",
}

// fallbackModels are tried, in order, when neither name variant of the
// resolved model is served by the gateway. A different family, deliberately:
// the point is to keep a run interpretable when the primary model disappears
// entirely, so it has to be something the gateway is likely to still serve.
//
// Not a workflow input — e2e.yml is already at GitHub's 10-input cap for
// workflow_dispatch — but overridable for a one-off via E2E_MODEL_FALLBACK
// (space- or comma-separated for several).
//
// Substituting one of these is NOT silent: unlike a variant flip (same model,
// gateway naming quirk), running a different family makes a green matrix mean
// something weaker, so probe-model.sh annotates it and the reported model shows
// what actually ran.
var fallbackModels = []string{"moonshotai/Kimi-K3"}

// fallbackModelEnvVar overrides fallbackModels for a single run. Kept in sync
// with scripts/probe-model.sh.
const fallbackModelEnvVar = "E2E_MODEL_FALLBACK"

// teeSuffix is the gateway's confidential-computing name variant. Appending or
// stripping it is the whole of the "variant flip" — see modelCandidates.
const teeSuffix = "-TEE"

// modelCandidates returns the model names to try for a harness, in order: the
// resolved model, its variant flip, then each fallback and its flip. The flip
// is bidirectional so pinning either variant survives the gateway changing its
// mind. Duplicates are dropped, so a fallback that equals the primary does not
// double the probe cost.
//
// claude-code gets exactly one candidate: it talks to ANTHROPIC_BASE_URL, not
// the gateway, so neither the variant flip nor a gateway fallback applies.
func modelCandidates(harness string) []string {
	primary := modelID(harness)
	if harness == "claude-code" {
		return []string{primary}
	}
	var out []string
	seen := map[string]bool{}
	add := func(m string) {
		if m == "" || seen[m] {
			return
		}
		seen[m] = true
		out = append(out, m)
	}
	for _, m := range append([]string{primary}, configuredFallbacks()...) {
		add(m)
		add(flipTEE(m))
	}
	return out
}

// flipTEE returns the other name variant of a model: the plain name gains
// "-TEE", a "-TEE" name loses it.
func flipTEE(model string) string {
	if model == "" {
		return ""
	}
	if strings.HasSuffix(model, teeSuffix) {
		return strings.TrimSuffix(model, teeSuffix)
	}
	return model + teeSuffix
}

// configuredFallbacks returns the fallback chain, honouring
// E2E_MODEL_FALLBACK (comma- or space-separated).
func configuredFallbacks() []string {
	raw := os.Getenv(fallbackModelEnvVar)
	if strings.TrimSpace(raw) == "" {
		return fallbackModels
	}
	var out []string
	for _, f := range strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n'
	}) {
		out = append(out, f)
	}
	return out
}

// isFallbackModel reports whether model is a cross-family substitution rather
// than the primary or its variant flip — the case that must be reported
// loudly. Used by the workflows via scripts/probe-model.sh.
func isFallbackModel(harness, model string) bool {
	primary := modelID(harness)
	return model != primary && model != flipTEE(primary)
}

// modelEnvVar maps a harness name to the env var that overrides its model id
// for a single run. Wired from the e2e workflow's workflow_dispatch
// claude_code_model input (.github/workflows/e2e.yml); unset in the scheduled
// run, so that run always uses the modelIDs map above.
//
// The cross-harness E2E_MODEL override applies to every harness at once and is
// what the workflows' single `model` input sets; these per-harness vars exist
// for claude-code, whose model comes from a different provider than the rest.
var modelEnvVar = map[string]string{
	"opencode":    "E2E_MODEL_OPENCODE",
	"claude-code": "E2E_MODEL_CLAUDE_CODE",
	"codex":       "E2E_MODEL_CODEX",
	"copilot":     "E2E_MODEL_COPILOT",
	"pi":          "E2E_MODEL_PI",
}

// crossHarnessModelEnvVar overrides the model for every harness at once. Kept
// in sync with scripts/resolve-model.sh, which implements the same precedence
// for the workflows' shell steps.
const crossHarnessModelEnvVar = "E2E_MODEL"

// modelID returns the model id a harness runs against. A per-harness
// E2E_MODEL_<HARNESS> override wins, then the cross-harness E2E_MODEL, then
// the pinned modelIDs map — so a single run can test a different model
// without editing this file.
func modelID(harness string) string {
	if ev, ok := modelEnvVar[harness]; ok {
		if v := os.Getenv(ev); v != "" {
			return v
		}
	}
	if v := os.Getenv(crossHarnessModelEnvVar); v != "" {
		return v
	}
	return modelIDs[harness]
}

// Declared context/output window for the openai-compatible provider configs
// the harnesses are handed (opencode's models entry; scripts/doc-drift.sh and
// scripts/e2e-readme-onboarding.sh write the same block for their own agent).
//
// The default is deliberately conservative and model-independent: a declared
// window LARGER than the model's real one overflows mid-run and 422s, while a
// smaller one only makes the harness compact earlier. 100k sits under every
// model in modelIDs, so a model override needs no matching limit override to
// work — E2E_CONTEXT_LIMIT / E2E_OUTPUT_LIMIT are there to raise it for a
// model whose window is worth exercising in full.
const (
	defaultContextLimit = 100_000
	defaultOutputLimit  = 32_000
)

// contextLimit returns the context window to declare, honouring
// E2E_CONTEXT_LIMIT. A non-numeric or non-positive value is ignored.
func contextLimit() int { return envInt("E2E_CONTEXT_LIMIT", defaultContextLimit) }

// outputLimit returns the output window to declare, honouring
// E2E_OUTPUT_LIMIT. A non-numeric or non-positive value is ignored.
func outputLimit() int { return envInt("E2E_OUTPUT_LIMIT", defaultOutputLimit) }

func envInt(name string, fallback int) int {
	v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name)))
	if err != nil || v <= 0 {
		return fallback
	}
	return v
}

// claudeCodeModelFamilies are the model families the claude-code CLI accepts
// for --model. Anything else is rejected by claude-code itself with an opaque
// startup error, so validateModel names the real cause instead.
var claudeCodeModelFamilies = []string{"sonnet", "haiku"}

// validateModel reports whether a resolved model can actually run on a
// harness. Only claude-code constrains this: it is the one harness bound to a
// single provider, and only that provider's sonnet/haiku tiers are launchable.
// Kept in sync with the same check in scripts/resolve-model.sh, which fails a
// workflow's model input before the run spends time installing anything.
func validateModel(harness, model string) error {
	if harness != "claude-code" {
		return nil
	}
	for _, family := range claudeCodeModelFamilies {
		if strings.Contains(strings.ToLower(model), family) {
			return nil
		}
	}
	return fmt.Errorf("claude-code cannot run model %q: it accepts only %s models "+
		"(override it separately via E2E_MODEL_CLAUDE_CODE / the claude_code_model workflow input)",
		model, strings.Join(claudeCodeModelFamilies, " or "))
}

// pinnedPackage returns the package spec for a harness.
// When E2E_USE_LATEST=1, returns the bare package name (latest), ignoring
// any per-harness version override. Otherwise, a non-empty versionEnvVar
// override takes precedence over the harnessVersions map, so a single run
// can test a candidate version without editing this file.
func pinnedPackage(harness string) string {
	if useLatest() {
		// Strip @version from "pkg@1.2.3" → "pkg".
		pkg := harnessVersions[harness]
		if i := lastIndexByte(pkg, '@'); i > 0 {
			return pkg[:i]
		}
		return pkg
	}
	if ev, ok := versionEnvVar[harness]; ok {
		if v := os.Getenv(ev); v != "" {
			return v
		}
	}
	return harnessVersions[harness]
}

// useLatest returns true when E2E_USE_LATEST=1 is set, which
// skips version pinning and installs the latest harness release.
func useLatest() bool {
	return os.Getenv("E2E_USE_LATEST") != ""
}

// lastIndexByte returns the index of the last occurrence of b in s, or -1.
func lastIndexByte(s string, b byte) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == b {
			return i
		}
	}
	return -1
}
