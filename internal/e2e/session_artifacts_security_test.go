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

// TestSecuritySessionArtifactsRecordEnvNamesOnly asserts that
// writeSessionArtifacts never writes an env var's VALUE into the uploaded
// meta.txt artifact — only its name, as the file's own doc comment and
// in-line comment both claim ("env vars (names only)").
//
// The redaction is a substring denylist on the var NAME
// (SECRET/TOKEN/KEY/PASSWORD); a var whose value is itself sensitive but
// whose name doesn't match — ANTHROPIC_BASE_URL, SKAINET_INTERNAL, an
// internal proxy or registry URL — is printed as "NAME=value" in full.
func TestSecuritySessionArtifactsRecordEnvNamesOnly(t *testing.T) {
	dir := t.TempDir()
	old := sessionLogDir
	sessionLogDir = dir
	t.Cleanup(func() { sessionLogDir = old })

	h := harnessConfig{Name: "probe", BinaryName: "probe"}
	env := []string{
		"PATH=/usr/bin",
		"SKAINET_INTERNAL=https://internal-model-gateway.example.corp",
	}
	writeSessionArtifacts(t, h, "unittest", "/home", "/work", "prompt", "out", "err", env, "")

	metaPath := filepath.Join(dir, fmt.Sprintf("%s-%s-%s", h.Name, runtime.GOOS, "unittest"), "meta.txt")
	data, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("meta.txt was not written: %v", err)
	}
	meta := string(data)

	// Control: the var NAME does appear (so the fixture reaches the
	// vulnerable code and isn't silently dropping the whole env section).
	if !strings.Contains(meta, "SKAINET_INTERNAL") {
		t.Fatalf("control: the env var name SKAINET_INTERNAL is missing from meta.txt entirely: the fixture is broken, not the security property\n%s", meta)
	}

	if strings.Contains(meta, "internal-model-gateway.example.corp") {
		t.Errorf("meta.txt recorded the VALUE of an env var whose value is itself sensitive (an internal URL), not just its name:\n%s", meta)
	}
}
