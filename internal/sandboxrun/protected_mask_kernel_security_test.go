//go:build vuln && linux

package sandboxrun

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestSecurityProtectedPathMaskedUnderRootGrantAtKernelLevel is the
// kernel-enforcement complement to
// TestSecurityProtectedMasksEmittedUnderRootGrant (argv-shape only): it
// actually runs bubblewrap and tries to read a protected file from inside
// a sandbox granted "/".
func TestSecurityProtectedPathMaskedUnderRootGrantAtKernelLevel(t *testing.T) {
	requireWorkingBwrap(t)

	omac := buildOmac(t)
	home := t.TempDir()
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	secretPath := filepath.Join(sshDir, "id_rsa")
	if err := os.WriteFile(secretPath, []byte("TOPSECRET-PRIVATE-KEY\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	g := &Grants{
		Workdir:        home,
		AllowPaths:     []string{"/"},
		ProtectedPaths: []string{sshDir},
		NetworkMode:    "blocked",
	}

	stage2 := append([]string{omac, "sandbox", "stage2"}, Stage2Args(g)...)
	tail := append(append([]string{}, stage2...), "--", "/bin/cat", secretPath)
	argv, err := BuildBwrapArgv(g, tail)
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput()

	// Control: without ProtectedPaths at all, the same secret IS readable
	// through the same root grant — proves the sandbox construction here
	// really does expose the tree, so a refusal above would be about the
	// masking, not an unrelated launch failure.
	unprotected := &Grants{Workdir: home, AllowPaths: []string{"/"}, NetworkMode: "blocked"}
	controlTail := append(append([]string{}, append([]string{omac, "sandbox", "stage2"}, Stage2Args(unprotected)...)...), "--", "/bin/cat", secretPath)
	controlArgv, cerr := BuildBwrapArgv(unprotected, controlTail)
	if cerr != nil {
		t.Fatal(cerr)
	}
	controlOut, _ := exec.Command(controlArgv[0], controlArgv[1:]...).CombinedOutput()
	if !strings.Contains(string(controlOut), "TOPSECRET") {
		t.Fatalf("control: the secret was not readable even without any protected-path declaration (%q): the fixture is broken, not the security property", controlOut)
	}

	if err == nil && strings.Contains(string(out), "TOPSECRET") {
		t.Errorf("a file under a declared protected path (%s) was readable from inside a sandbox granted \"/\": the protection is silently dropped, not just weakened. output: %q", sshDir, out)
	}
}
