package cli

import (
	"context"
	"errors"

	"github.com/TNG/oh-my-agentic-coder/internal/ephemeraldocker"
)

// newEphemeralDockerSession is the boot seam for the launch pipeline:
// production calls ephemeraldocker.StartSession; tests replace it to run
// the wiring without a VM.
var newEphemeralDockerSession = func(ctx context.Context, opts ephemeraldocker.SessionOpts) (ephemeraldocker.Handle, error) {
	return ephemeraldocker.StartSession(ctx, opts)
}

// ephemeralDockerExitCode maps boot failures to the documented exit
// codes: missing prerequisites (limactl, a pinned image for this
// architecture) are installable/known conditions, everything else is a
// runtime failure.
func ephemeralDockerExitCode(err error) int {
	switch {
	case errors.Is(err, ephemeraldocker.ErrLimaMissing),
		errors.Is(err, ephemeraldocker.ErrUnpinnedArch):
		return ExitPrerequisiteMissing
	default:
		return ExitGeneric
	}
}

// ephemeralDockerGuestArch maps the host GOARCH to the guest
// architecture Lima boots. arm64 Macs boot aarch64; Intel Macs map to
// x86_64, which the image table rejects (fail closed: no measured digest
// exists for it).
func ephemeralDockerGuestArch(goarch string) string {
	if goarch == "arm64" {
		return "aarch64"
	}
	return "x86_64"
}
