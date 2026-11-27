package cli

import (
	"os"
	"strings"
	"testing"
)

func TestDenyScanFlagsHidden(t *testing.T) {
	outF, _ := os.CreateTemp(t.TempDir(), "out")
	errF, _ := os.CreateTemp(t.TempDir(), "err")
	env := &Env{Stdout: outF, Stderr: errF}

	printSandboxUsage(env)
	runSandboxRun([]string{"--bogus"}, env)

	text, err := os.ReadFile(errF.Name())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(text), "usage: omac sandbox run") {
		t.Fatalf("usage line missing from output: %q", text)
	}
	if strings.Contains(string(text), "deny-scan") {
		t.Errorf("internal flags leaked into usage text:\n%s", text)
	}
}
