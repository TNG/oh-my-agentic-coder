//go:build vuln

package ciguard

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestSecurityProbeModelOutputFramingResistsInjection asserts that a
// resolved model value containing an embedded newline cannot forge
// additional $GITHUB_OUTPUT keys beyond the three probe-model.sh declares
// (model/fallback/candidates).
//
// resolve-model.sh's claude-code guard (reject_unlaunchable) only checks
// that the value contains "sonnet" or "haiku" as a substring — it applies
// no character-set restriction at all — and probe-model.sh's emit()
// appends bare `echo "model=$model"` lines to $GITHUB_OUTPUT with no
// `name<<DELIM` heredoc framing. A value of "sonnet\nfabricated_key=evil"
// passes the substring check intact and, once echoed, becomes two physical
// lines: the legitimate "model=sonnet" and an attacker-chosen
// "fabricated_key=evil" the workflow never declared.
func TestSecurityProbeModelOutputFramingResistsInjection(t *testing.T) {
	script := filepath.Join(repoRoot(t), "scripts", "probe-model.sh")
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("control: %s not found: the fixture is broken, not the security property: %v", script, err)
	}

	outputFile := filepath.Join(t.TempDir(), "github_output")
	if err := os.WriteFile(outputFile, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("bash", script, "claude-code", "--github-output")
	cmd.Env = append(os.Environ(),
		"GITHUB_OUTPUT="+outputFile,
		"E2E_MODEL_CLAUDE_CODE=sonnet\nfabricated_key=evil-value",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("probe-model.sh claude-code failed: %v\n%s", err, out)
	}

	data, err := os.ReadFile(outputFile)
	if err != nil {
		t.Fatalf("read GITHUB_OUTPUT: %v", err)
	}
	written := string(data)

	// Control: the legitimate "model=" key is actually present, so the
	// fixture reaches emit() and isn't failing before it.
	if !strings.Contains(written, "model=") {
		t.Fatalf("control: no model= key was written at all: the fixture is broken, not the security property\n%s", written)
	}

	if strings.Contains(written, "fabricated_key=evil-value") {
		t.Errorf("a model value containing an embedded newline forged an extra $GITHUB_OUTPUT key the workflow never declared:\n%s", written)
	}
}
