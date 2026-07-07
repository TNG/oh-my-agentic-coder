//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// sessionLogDir is the directory where e2e test output artifacts are
// written. Set via E2E_LOG_DIR env var (CI sets it to a known path
// for artifact upload). When unset, no files are written — the test
// still logs via t.Logf.
//
// The directory is created on first use. Each subtest writes:
//
//	<logdir>/<harness>-<os>-<testname>/
//	  agent-stdout.txt   — raw agent stdout
//	  agent-stderr.txt   — raw agent stderr (includes omac sandbox diag)
//	  sidecar-*.log      — sidecar process logs
//	  omac.log           — opencode's own log (if present)
//	  meta.txt           — test metadata (harness, os, prompt, env vars)
//	  sandbox-profile.json — the sandbox profile used
var sessionLogDir = os.Getenv("E2E_LOG_DIR")

// writeSessionArtifacts writes captured test output to the session log
// directory. Called after the agent run completes (success or failure).
// When E2E_LOG_DIR is unset, this is a no-op.
func writeSessionArtifacts(t *testing.T, h harnessConfig, testType string,
	home, workdir string, prompt string, stdout, stderr string,
	env []string, profilePath string) {
	t.Helper()
	if sessionLogDir == "" {
		return
	}
	dirName := fmt.Sprintf("%s-%s-%s", h.Name, runtime.GOOS, testType)
	dir := filepath.Join(sessionLogDir, dirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Logf("warning: cannot create session log dir %s: %v", dir, err)
		return
	}

	mustWrite := func(name, content string) {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Logf("warning: cannot write %s: %v", path, err)
		}
	}

	mustWrite("agent-stdout.txt", stdout)
	mustWrite("agent-stderr.txt", stderr)

	// Metadata: harness, OS, prompt, env var names (not values for
	// secrets).
	var meta strings.Builder
	fmt.Fprintf(&meta, "harness: %s\n", h.Name)
	fmt.Fprintf(&meta, "binary: %s\n", h.BinaryName)
	fmt.Fprintf(&meta, "os: %s/%s\n", runtime.GOOS, runtime.GOARCH)
	fmt.Fprintf(&meta, "test_type: %s\n", testType)
	fmt.Fprintf(&meta, "no_sandbox: %v\n", h.Sandbox.NoSandbox)
	fmt.Fprintf(&meta, "prompt: %s\n", prompt)
	fmt.Fprintf(&meta, "\n## env vars (names only)\n")
	for _, kv := range env {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			continue
		}
		name := kv[:eq]
		if strings.Contains(strings.ToUpper(name), "SECRET") ||
			strings.Contains(strings.ToUpper(name), "TOKEN") ||
			strings.Contains(strings.ToUpper(name), "KEY") ||
			strings.Contains(strings.ToUpper(name), "PASSWORD") {
			fmt.Fprintf(&meta, "  %s=<redacted>\n", name)
		} else {
			fmt.Fprintf(&meta, "  %s\n", kv)
		}
	}
	mustWrite("meta.txt", meta.String())

	// Sandbox profile (copy if exists).
	if profilePath != "" {
		if data, err := os.ReadFile(profilePath); err == nil {
			mustWrite("sandbox-profile.json", string(data))
		}
	}

	// Sidecar logs.
	pattern := filepath.Join(os.TempDir(), "omac-*", "logs", "*.log")
	matches, _ := filepath.Glob(pattern)
	for _, m := range matches {
		data, err := os.ReadFile(m)
		if err != nil {
			continue
		}
		name := filepath.Base(m)
		mustWrite("sidecar-"+name, string(data))
	}

	// opencode's own log.
	ocLog := filepath.Join(home, ".local", "share", "opencode", "log", "opencode.log")
	if data, err := os.ReadFile(ocLog); err == nil {
		mustWrite("omac.log", string(data))
	}

	// Audit output file (security audit test): the raw probe output
	// written by audit.sh. Captures probe results independent of how
	// the harness rendered tool output.
	if data, err := os.ReadFile(filepath.Join(workdir, "audit-output.txt")); err == nil {
		mustWrite("audit-output.txt", string(data))
	}

	t.Logf("session artifacts written to %s", dir)
}
