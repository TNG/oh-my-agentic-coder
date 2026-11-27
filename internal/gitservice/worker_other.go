//go:build !linux

package gitservice

import (
	"context"
	"fmt"
	"os"
)

func supportedPlatform() error {
	return fmt.Errorf("the Git service prototype requires Linux and bubblewrap")
}
func multipleLinks(os.FileInfo) bool          { return true }
func lockRepository(string) (*os.File, error) { return nil, supportedPlatform() }
func (s *Service) run(context.Context, bool, bool, []byte, ...string) ([]byte, error) {
	return nil, supportedPlatform()
}

func completedWorkerFailure(error) bool { return false }
