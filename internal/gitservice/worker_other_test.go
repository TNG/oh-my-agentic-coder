//go:build !linux

package gitservice

import (
	"strings"
	"testing"
)

func TestGitServiceRefusesUnsupportedPlatform(t *testing.T) {
	if s, err := New(Options{Workdir: t.TempDir(), Name: "Test", Email: "test@example.invalid"}); err == nil || !strings.Contains(err.Error(), "requires Linux") {
		if s != nil {
			s.Close()
		}
		t.Fatalf("unsupported platform: %v", err)
	}
}
