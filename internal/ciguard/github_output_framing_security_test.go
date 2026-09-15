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
// no character-set restriction at all. probe-model.sh's emit() uses the
// `name<<DELIM` heredoc framing required by GitHub Actions for multi-line
// values; a newline in the model value is contained inside the delimited
// block and cannot forge an extra key.
// isTopLevelOutputKey reports whether name appears as a top-level key in a
// $GITHUB_OUTPUT file — either as "name=..." or as "name<<DELIM" at the start
// of a line that is NOT itself inside an active heredoc block. Text that
// appears inside a <<DELIM...DELIM pair is a value, not a key.
func isTopLevelOutputKey(content, name string) bool {
	inBlock := false
	delimiter := ""
	for _, line := range strings.Split(content, "\n") {
		if inBlock {
			if line == delimiter {
				inBlock = false
			}
			continue
		}
		// Detect the start of a name<<DELIM block.
		if idx := strings.Index(line, "<<"); idx > 0 {
			inBlock = true
			delimiter = strings.TrimSpace(line[idx+2:])
			// If the key itself is the fabricated name, that's a hit.
			if strings.TrimSpace(line[:idx]) == name {
				return true
			}
			continue
		}
		// Plain "name=value" form.
		if idx := strings.Index(line, "="); idx > 0 {
			if strings.TrimSpace(line[:idx]) == name {
				return true
			}
		}
	}
	return false
}

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

	// Control: the model key header is present (name<<DELIM framing), so
	// the fixture reaches emit() and isn't failing before it.
	if !strings.Contains(written, "model<<OMAC_DELIM") {
		t.Fatalf("control: no model<<OMAC_DELIM header was written at all: the fixture is broken, not the security property\n%s", written)
	}

	// The fabricated key must not appear as a top-level $GITHUB_OUTPUT key
	// (a line of the form "name=value" or "name<<DELIM" outside a heredoc
	// block). Text inside a <<DELIM...DELIM block is a value, not a key.
	if isTopLevelOutputKey(written, "fabricated_key") {
		t.Errorf("a model value containing an embedded newline forged an extra $GITHUB_OUTPUT key the workflow never declared:\n%s", written)
	}
}
