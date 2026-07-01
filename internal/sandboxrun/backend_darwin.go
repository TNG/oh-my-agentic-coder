//go:build darwin

package sandboxrun

import (
	"fmt"
	"os/exec"
)

// sandboxExecPath is the Seatbelt CLI shipped with every macOS.
const sandboxExecPath = "/usr/bin/sandbox-exec"

// CheckPlatform verifies the macOS sandbox prerequisites.
func CheckPlatform() error {
	if _, err := exec.LookPath(sandboxExecPath); err != nil {
		return fmt.Errorf("sandbox-exec not found at %s: %w", sandboxExecPath, err)
	}
	return nil
}

// BuildChildArgv wraps the inner command in sandbox-exec with the
// generated SBPL profile. The supervisor's filtered env is applied by
// the caller on the exec.Cmd.
//
// Unlike Linux's bwrap (which builds a mount namespace), Seatbelt uses
// path-based deny/allow rules over the real filesystem. So the inner
// harness binary (e.g. claude, codex) must have its directory granted
// as readable — (deny default) blocks exec otherwise. We resolve the
// binary on the host PATH and add its dir (and symlink-resolved dir)
// to ReadPaths before generating the SBPL, mirroring backend_linux.go.
func BuildChildArgv(g *Grants, innerArgv []string) ([]string, error) {
	if err := CheckPlatform(); err != nil {
		return nil, err
	}
	gz := *g
	gz.ReadPaths = append(append([]string{}, g.ReadPaths...), resolveInnerBinaryDirs(innerArgv)...)
	profile := GenerateSBPL(&gz)
	argv := []string{sandboxExecPath, "-p", profile, "--"}
	argv = append(argv, innerArgv...)
	return argv, nil
}
