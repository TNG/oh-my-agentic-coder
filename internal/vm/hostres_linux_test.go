//go:build linux

package vm

import (
	"strings"
	"testing"
)

func TestParseMemInfo(t *testing.T) {
	in := "MemFree:         1000 kB\nMemTotal:       16384 kB\nBuffers:           0 kB\n"
	if got, want := parseMemInfo(strings.NewReader(in)), uint64(16384*1024); got != want {
		t.Errorf("parseMemInfo = %d, want %d", got, want)
	}
	if got := parseMemInfo(strings.NewReader("garbage\n")); got != 0 {
		t.Errorf("parseMemInfo(garbage) = %d, want 0", got)
	}
}
