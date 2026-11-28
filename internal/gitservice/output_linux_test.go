//go:build linux

package gitservice

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

func TestOutputLimitThroughCopy(t *testing.T) {
	for _, size := range []int{maxOutput - 1, maxOutput, maxOutput + 1} {
		var output boundedBuffer
		reader := struct{ io.Reader }{bytes.NewReader(bytes.Repeat([]byte("x"), size))}
		_, err := io.Copy(&output, reader)
		if (err != nil) != (size > maxOutput) {
			t.Fatalf("size %d: %v", size, err)
		}
	}
}

func TestWorkerOutputLimitThroughPipes(t *testing.T) {
	dir := gitFixture(t)
	s := newTestService(t, dir)
	writeFile(t, filepath.Join(dir, "large"), strings.Repeat("x", maxOutput+1))
	for _, redirect := range []string{"", " >&2"} {
		// Test-only alias makes Git emit oversized stdout or stderr inside the worker.
		_, err := s.run(context.Background(), false, false, nil, "-c", "alias.oversize=!cat /repo/large"+redirect, "oversize")
		if err == nil || !strings.Contains(err.Error(), "Git output exceeds limit") {
			t.Fatalf("redirect %q: %v", redirect, err)
		}
	}
}
