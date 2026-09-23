package ephemeraldocker

import (
	"errors"
	"fmt"
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
