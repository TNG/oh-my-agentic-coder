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
// probe-model.sh's emit() uses the `name<<DELIM` heredoc framing required
// by GitHub Actions for multi-line values; a newline in the model value is
// contained inside the delimited block and cannot forge an extra key.
//
// The function is extracted and driven directly rather than through the full
// probe-model.sh invocation: resolve-model.sh's charset filter (added in the
// same fix) would reject a newline-containing value before emit() is ever
// reached, masking the property under test.

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
		if idx := strings.Index(line, "<<"); idx > 0 {
			inBlock = true
			delimiter = strings.TrimSpace(line[idx+2:])
			if strings.TrimSpace(line[:idx]) == name {
				return true
			}
			continue
		}
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

	// Extract emit() from probe-model.sh and drive it directly with a model
	// value containing an embedded newline. Driving the full script would
	// invoke resolve-model.sh's charset filter first, which rejects newlines
	// before emit() is reached — correct behaviour, but it would mask the
	// framing property this test specifically covers.
	extract := exec.Command("awk", `/^emit\(\)/,/^}$/{print}`, script)
	fnBody, err := extract.Output()
	if err != nil || len(fnBody) == 0 {
		t.Fatalf("control: emit() not found in %s: the fixture is broken: %v", script, err)
	}

	outputFile := filepath.Join(t.TempDir(), "github_output")
	if err := os.WriteFile(outputFile, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	// Call emit() with a model value that contains a real embedded newline
	// followed by text that would look like a new key if framing were absent.
	hostile := "sonnet\nfabricated_key=evil-value"
	driver := string(fnBody) + "\ngithub_output=1\nemit " +
		shellQuote(hostile) + " false " + shellQuote(hostile)
	cmd := exec.Command("bash", "-c", driver)
	cmd.Env = append(os.Environ(), "GITHUB_OUTPUT="+outputFile)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("emit() driver failed: %v\n%s", err, out)
	}

	data, err := os.ReadFile(outputFile)
	if err != nil {
		t.Fatalf("read GITHUB_OUTPUT: %v", err)
	}
	written := string(data)

	// Control: the model key header is present (name<<DELIM framing).
	if !strings.Contains(written, "model<<OMAC_DELIM") {
		t.Fatalf("control: no model<<OMAC_DELIM header was written: the fixture is broken\n%s", written)
	}

	// The fabricated key must not appear as a top-level $GITHUB_OUTPUT key.
	if isTopLevelOutputKey(written, "fabricated_key") {
		t.Errorf("a model value containing an embedded newline forged an extra $GITHUB_OUTPUT key:\n%s", written)
	}
}
