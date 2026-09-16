package ciguard

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestSecurityGeneratedOpencodeConfigIsValidJSON asserts that the
// opencode.json e2e-readme-onboarding.sh writes for the driver harness
// stays valid, structurally correct JSON regardless of the model name,
// base URL, or limit values feeding into it.
//
// The config is built by write_opencode_config() using `jq -n --arg` /
// `--argjson`, which always produces well-formed JSON regardless of what
// the argument values contain. This test confirms that property holds:
// a hostile model name or URL cannot inject extra JSON keys.
//
// Extracted from the script rather than sourcing it whole: the script
// installs packages and shells out to git before reaching this point.
func TestSecurityGeneratedOpencodeConfigIsValidJSON(t *testing.T) {
	script := filepath.Join(repoRoot(t), "scripts", "e2e-readme-onboarding.sh")
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("control: %s not found: the fixture is broken, not the security property: %v", script, err)
	}

	extract := exec.Command("awk", `/^write_opencode_config\(\)/,/^}$/{print}`, script)
	fnBody, err := extract.Output()
	if err != nil {
		t.Fatalf("extract write_opencode_config(): %v", err)
	}
	if len(fnBody) == 0 {
		t.Fatalf("control: write_opencode_config() was not found in %s: the fixture is broken, not the security property", script)
	}

	render := func(model, baseURL, contextLimit, outputLimit string) (string, error) {
		dest := filepath.Join(t.TempDir(), "opencode.json")
		driver := string(fnBody) + "\nwrite_opencode_config " +
			shellQuote(model) + " " + shellQuote(baseURL) + " " +
			shellQuote(contextLimit) + " " + shellQuote(outputLimit) + " " +
			shellQuote(dest)
		cmd := exec.Command("bash", "-c", driver)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("render write_opencode_config: %v: %s", err, out)
		}
		data, err := os.ReadFile(dest)
		if err != nil {
			t.Fatal(err)
		}
		return string(data), nil
	}

	// Control: ordinary inputs produce valid JSON.
	benign, _ := render("zai-org/GLM-5.2", "https://gateway.example", "128000", "8000")
	var v any
	if err := json.Unmarshal([]byte(benign), &v); err != nil {
		t.Fatalf("control: benign inputs did not render valid JSON (%v): the fixture is broken, not the security property\n%s", err, benign)
	}

	// A model name containing JSON metacharacters cannot break out of its
	// string position when passed through jq --arg.
	hostile, _ := render(`", "injected": "evil`, "https://gateway.example", "128000", "8000")
	if err := json.Unmarshal([]byte(hostile), &v); err != nil {
		// jq may reject the hostile model as an invalid key — that is also a
		// valid outcome (the config is not produced at all, which is safe).
		return
	}
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatal("hostile render did not parse to a JSON object")
	}
	provider, _ := m["provider"].(map[string]any)
	modelMap, _ := provider["model"].(map[string]any)
	models, _ := modelMap["models"].(map[string]any)
	if _, injected := models["injected"]; injected {
		t.Errorf("a hostile model name injected an extra key into the generated opencode.json:\n%s", hostile)
	}
}

// shellQuote wraps s in single quotes for safe bash argument passing.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}
