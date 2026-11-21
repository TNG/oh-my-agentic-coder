//go:build !linux

package updater

import (
	"fmt"
	"os"
	"runtime"
)

// openVerifiedDescriptor fails closed on platforms where the verified bytes
// cannot be handed to an installer as an inherited descriptor.
func openVerifiedDescriptor([]byte) (*os.File, error) {
	return nil, fmt.Errorf("verified descriptor inheritance is not supported on %s", runtime.GOOS)
}

func verifiedArtifactPath() string {
	return ""
}
