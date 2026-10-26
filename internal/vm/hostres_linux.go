//go:build linux

package vm

import (
	"bufio"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"
)

// DetectHost reads MemTotal from /proc/meminfo; an unreadable file leaves
// memory unknown (0).
func DetectHost() HostResources {
	h := HostResources{CPUs: runtime.NumCPU()}
	if f, err := os.Open("/proc/meminfo"); err == nil {
		defer f.Close()
		h.MemoryBytes = parseMemInfo(f)
	}
	return h
}

// parseMemInfo returns MemTotal in bytes from a line like
// "MemTotal:       16384 kB", or 0 when no such line parses.
func parseMemInfo(r io.Reader) uint64 {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 || fields[0] != "MemTotal:" {
			continue
		}
		kb, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return 0
		}
		return kb * 1024
	}
	return 0
}
