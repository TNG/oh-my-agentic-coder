//go:build unix

package config

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// lockProjectPins takes an exclusive flock on the pin store's sibling lock
// file so concurrent approvals cannot overwrite each other's recorded
// digests. The unlock function is safe to call once.
func lockProjectPins() (unlock func(), err error) {
	path := projectPinsPath()
	if path == "" {
		return func() {}, nil
	}
	lockDir := filepath.Dir(path)
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		return nil, fmt.Errorf("ensure pin store dir: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(lockDir, filepath.Base(path)+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open pin store lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("flock pin store lock: %w", err)
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
}
