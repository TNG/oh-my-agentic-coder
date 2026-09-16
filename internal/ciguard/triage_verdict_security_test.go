package ciguard

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestSecurityTriageVerdictCannotSuppressFindings asserts that
// security_scan_triage.py never marks a finding as a tracked duplicate on
// the strength of an LLM response naming an issue number that isn't
// actually among the open issues it was given.
//
// is_dup checks only verdict["duplicate_of"] is not None and
// confidence != "low" — it never verifies duplicate_of against the issues
// list it just supplied to the model. A scanner finding's own title or
// description (untrusted content, since it names user-controlled code
// paths) reaches the LLM prompt as free text; a response fabricating a
// plausible-looking issue number is enough to have a genuinely new finding
// rendered "LIKELY DUPLICATE" and folded out of the public new-finding
// count, with no human ever told the number was fake.
//
// Driven by importing the script as a Python module (its `if __name__ ==
// "__main__"` guard makes this safe) and monkeypatching ask_llm_duplicate
// and fetch_open_issues — no network, no gh CLI, no real LLM call, no
// production code change.
func TestSecurityTriageVerdictCannotSuppressFindings(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Fatalf("python3 is required to exercise this property and was not found on PATH: %v", err)
	}

	scriptsDir := filepath.Join(repoRoot(t), ".github", "scripts")
	if _, err := os.Stat(filepath.Join(scriptsDir, "security_scan_triage.py")); err != nil {
		t.Fatalf("control: security_scan_triage.py not found: the fixture is broken, not the security property: %v", err)
	}

	dir := t.TempDir()
	vulnsPath := filepath.Join(dir, "vulns.json")
	triageOut := filepath.Join(dir, "triage.md")
	summaryOut := filepath.Join(dir, "summary.md")
	if err := os.WriteFile(vulnsPath, []byte(`[{"id":"F1","title":"hostile finding","severity":"high","description":"desc"}]`), 0o644); err != nil {
		t.Fatal(err)
	}

	driver := `
import sys, json
sys.path.insert(0, ` + pyQuote(scriptsDir) + `)
import security_scan_triage as m

# Real open issues do NOT include #999 — the number the fake verdict below
# will claim as a match.
m.fetch_open_issues = lambda repo: [{"number": 1, "title": "unrelated", "body": "unrelated"}]
m.ask_llm_duplicate = lambda finding, issues: {"duplicate_of": 999, "confidence": "high", "reasoning": "fabricated"}

sys.argv = ["security_scan_triage.py",
    "--vulns", ` + pyQuote(vulnsPath) + `,
    "--repo", "owner/repo",
    "--triage-out", ` + pyQuote(triageOut) + `,
    "--summary-out", ` + pyQuote(summaryOut) + `]
m.main()
`
	cmd := exec.Command("python3", "-c", driver)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("driver script failed: %v\n%s", err, out)
	}

	triage, err := os.ReadFile(triageOut)
	if err != nil {
		t.Fatalf("triage-out was not written: %v", err)
	}
	body := string(triage)

	// Control: the finding is actually processed and reaches the triage
	// report at all, so a fix that silently drops findings wouldn't make
	// this test pass for the wrong reason.
	if !strings.Contains(body, "hostile finding") {
		t.Fatalf("control: the finding did not reach the triage report at all: the fixture is broken, not the security property\n%s", body)
	}

	if strings.Contains(body, "LIKELY DUPLICATE of #999") {
		t.Errorf("a triage verdict naming issue #999 — not among the open issues actually supplied — marked a finding as a tracked duplicate with no validation against the real issue list:\n%s", body)
	}
}

// pyQuote renders a Go string as a Python string literal safe to splice into
// an inline script (paths only; no untrusted content).
func pyQuote(s string) string {
	return `"""` + strings.ReplaceAll(s, `"`, `\"`) + `"""`
}
