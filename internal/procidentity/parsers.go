// Pure parsing helpers shared across platform implementations. No
// build tag — these are unit-tested without real processes on every
// platform. The platform files (procidentity_linux.go,
// procidentity_darwin.go) call these from their identifyNative.

package procidentity

import (
	"fmt"
	"strconv"
	"strings"
)

// parseGradleMainClass extracts the Gradle daemon bootstrap main class
// from a NUL-separated argv blob (Linux /proc/<pid>/cmdline, or the
// macOS `ps -o args=` line split on whitespace). Returns
// GradleDaemonMainClass if the class appears as an exact argv token
// (NOT a substring match — the spec forbids relying on substring alone),
// "" otherwise. Pure function — unit-tested without real processes.
func parseGradleMainClass(cmdline []byte) string {
	// NUL-separated argv with a possible trailing NUL.
	tokens := strings.Split(strings.TrimRight(string(cmdline), "\x00"), "\x00")
	for _, tok := range tokens {
		if tok == GradleDaemonMainClass {
			return GradleDaemonMainClass
		}
		if strings.HasSuffix(tok, "/"+GradleDaemonMainClass) {
			return GradleDaemonMainClass
		}
	}
	return ""
}

// parseStartTime extracts field 22 (`starttime`, clock ticks since boot)
// from a /proc/<pid>/stat contents. The comm field (field 2) is wrapped
// in parens and may contain spaces, so naive whitespace splitting is
// wrong: strip from the first `(` to the last `)` before splitting.
// Pure function — unit-tested without real processes.
func parseStartTime(stat string) (string, error) {
	s := stat
	if i := strings.IndexByte(s, '('); i >= 0 {
		if j := strings.LastIndexByte(s, ')'); j > i {
			// Replace the parenthesised comm with a single placeholder
			// token so the subsequent whitespace split lines up with the
			// documented field numbers (comm is field 2; everything
			// after the closing paren is field 3 onward).
			s = s[:i] + "X" + s[j+1:]
		}
	}
	fields := strings.Fields(s)
	// pid(1) comm(2) state(3) ppid(4) pgrp(5) session(6) tty_nr(7)
	// tpgid(8) flags(9) minflt(10) cminflt(11) majflt(12) cmajflt(13)
	// utime(14) stime(15) cutime(16) cstime(17) priority(18) nice(19)
	// num_threads(20) itrealvalue(21) starttime(22) ...
	if len(fields) < 22 {
		return "", fmt.Errorf("stat has only %d fields before starttime", len(fields))
	}
	starttime := fields[21]
	if _, err := strconv.ParseUint(starttime, 10, 64); err != nil {
		return "", fmt.Errorf("starttime %q not a uint: %v", starttime, err)
	}
	return starttime, nil
}

// splitNul splits a NUL-separated argv blob into tokens, dropping empty
// trailing tokens. Pure helper — unit-tested.
func splitNul(b []byte) []string {
	out := []string{}
	start := 0
	for i, c := range b {
		if c == 0 {
			if i > start {
				out = append(out, string(b[start:i]))
			}
			start = i + 1
		}
	}
	if start < len(b) {
		out = append(out, string(b[start:]))
	}
	return out
}
