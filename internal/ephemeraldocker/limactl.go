package ephemeraldocker

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
)

// ErrLimaMissing reports that limactl is not installed. The CLI maps it
// to the installable diagnosis and ExitPrerequisiteMissing; the feature
// never silently degrades to another executor.
var ErrLimaMissing = errors.New("limactl not found on $PATH")

// FindLimactl resolves the limactl binary. lookPath is injectable for
// tests; production passes exec.LookPath.
func FindLimactl(lookPath func(string) (string, error)) (string, error) {
	path, err := lookPath("limactl")
	if err != nil {
		return "", fmt.Errorf("ephemeral-docker: %w (install Lima, for example with `brew install lima`)", ErrLimaMissing)
	}
	return path, nil
}

// DefaultLimaCtl runs limactl with LIMA_HOME set to limaHome (see the
// LimaCtl type for why every call must name its unit). Lima's progress
// output goes to stderr for diagnosability.
func DefaultLimaCtl(limaHome string, args ...string) error {
	cmd := exec.Command("limactl", args...)
	cmd.Env = append(os.Environ(), "LIMA_HOME="+limaHome)
	cmd.Stdout = io.Discard
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// DefaultReaper kills stray QEMU processes of one VM unit (see Reaper).
// pkill exits 1 when nothing matched, which means nothing was left to
// reap and is not an error.
func DefaultReaper(vmName string) error {
	cmd := exec.Command("pkill", "-f", vmName)
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 1 {
			return nil
		}
		return fmt.Errorf("ephemeral-docker: reap qemu processes of %s: %w", vmName, err)
	}
	return nil
}
