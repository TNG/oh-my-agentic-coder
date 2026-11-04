//go:build !linux

package sandbox

// detachedSessionGroups is Linux-only: only the bwrap backend detaches the
// child from the terminal session.
func detachedSessionGroups(_ int) []int { return nil }
