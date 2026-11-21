//go:build linux

package updater

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// The first ExtraFiles descriptor in the child is fd 3.
const verifiedArtifactChildFD = 3

// openVerifiedDescriptor copies the already-verified bytes into an anonymous
// sealed memfd so the installer can inherit exactly those bytes by descriptor
// rather than reopen a mutable path.
func openVerifiedDescriptor(data []byte) (*os.File, error) {
	fd, err := unix.MemfdCreate("omac-update-verified", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
	if err != nil {
		return nil, fmt.Errorf("create verified artifact descriptor: %w", err)
	}
	f := os.NewFile(uintptr(fd), "omac-update-verified")
	if _, err := f.Write(data); err != nil {
		f.Close()
		return nil, fmt.Errorf("write verified artifact descriptor: %w", err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		f.Close()
		return nil, fmt.Errorf("rewind verified artifact descriptor: %w", err)
	}
	seals := unix.F_SEAL_SEAL | unix.F_SEAL_SHRINK | unix.F_SEAL_GROW | unix.F_SEAL_WRITE
	if _, err := unix.FcntlInt(f.Fd(), unix.F_ADD_SEALS, seals); err != nil {
		f.Close()
		return nil, fmt.Errorf("seal verified artifact descriptor: %w", err)
	}
	return f, nil
}

func verifiedArtifactPath() string {
	return fmt.Sprintf("/proc/self/fd/%d", verifiedArtifactChildFD)
}
