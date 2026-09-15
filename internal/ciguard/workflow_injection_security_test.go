//go:build vuln

package ciguard

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// repoRoot resolves the repository root from this package's location
// (internal/ciguard -> internal -> root), mirroring the pattern used by
// internal/e2e's tests.
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repoRoot() = %q does not look like the repo root (no go.mod): %v", root, err)
	}
	return root
}

func workflowFiles(t *testing.T) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(repoRoot(t), ".github", "workflows", "*.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) == 0 {
		t.Fatalf("control: no workflow files found: the fixture is broken, not the security property")
	}
	return matches
}

// step mirrors the fields of a GitHub Actions job step relevant here.
type step struct {
	Name string `yaml:"name"`
	Run  string `yaml:"run"`
}

type job struct {
	Steps []step `yaml:"steps"`
}

type workflow struct {
	Jobs map[string]job `yaml:"jobs"`
}

// dangerousExpr matches a ${{ }} expression whose root object is
// attacker-influenced: a workflow_dispatch input, a prior step's output, or
// a job dependency's output. The Actions runner substitutes these into the
// run block's TEXT before the shell ever sees them — indistinguishable from
// the script author having typed the value themselves.
var dangerousExpr = regexp.MustCompile(`\$\{\{\s*(github\.event\.inputs\.|inputs\.|steps\.[a-zA-Z0-9_-]+\.outputs\.|needs\.[a-zA-Z0-9_-]+\.outputs\.)`)

// TestSecurityWorkflowRunBlocksHaveNoExpressionInterpolation asserts that no
// `run:` block in any shipped workflow interpolates an attacker-influenced
// GitHub Actions expression directly into shell text.
//
// The runner performs ${{ }} substitution as plain text BEFORE bash parses
// the script — there is no quoting boundary. A workflow_dispatch input or a
// prior step's output containing `$(...)`  or `; ...` becomes code executed
// with that job's secrets. The safe pattern is to bind the value via `env:`
// and dereference it as `"$VAR"`, which bash quotes normally.
//
// This is the durable form of the check: scripts/check-workflow-shell.py
// deliberately blanks every ${{ }} expression before running `bash -n`
// (documented in its own header), so it cannot see this class at all — this
// test is what closes that gap.
func TestSecurityWorkflowRunBlocksHaveNoExpressionInterpolation(t *testing.T) {
	files := workflowFiles(t)

	// Control: the expression-detection regex itself fires on a known
	// dangerous shape, so a fixture that never triggers isn't hiding a
	// broken regex.
	if !dangerousExpr.MatchString(`echo "${{ steps.probe.outputs.model }}"`) {
		t.Fatalf("control: the detection regex did not match a known dangerous pattern: the fixture is broken, not the security property")
	}
	if dangerousExpr.MatchString(`echo "${{ env.SAFE_VAR }}"`) {
		t.Fatalf("control: the detection regex fired on env.*, which is not one of the flagged roots: the fixture is broken")
	}

	var hits []string
	for _, path := range files {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var wf workflow
		if err := yaml.Unmarshal(raw, &wf); err != nil {
			t.Fatalf("%s: not parseable as YAML: %v", path, err)
		}
		for jobName, j := range wf.Jobs {
			for i, s := range j.Steps {
				if s.Run == "" {
					continue
				}
				if loc := dangerousExpr.FindString(s.Run); loc != "" {
					hits = append(hits, filepath.Base(path)+": job "+jobName+" step "+strconv.Itoa(i)+" ("+s.Name+"): "+loc)
				}
			}
		}
	}

	if len(hits) > 0 {
		t.Errorf("%d run: block(s) interpolate an attacker-influenced expression directly into shell text:\n  %s", len(hits), strings.Join(hits, "\n  "))
	}
}
