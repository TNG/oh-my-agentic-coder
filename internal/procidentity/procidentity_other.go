//go:build !linux && !darwin

package procidentity

// identifyNative is the unsupported-OS fallback. On any platform without
// a native implementation (anything other than linux and darwin),
// Identify returns ErrUnsupportedOS so callers fail closed rather than
// trusting an unverifiable process.
func identifyNative(pid int) (Identity, error) {
	return Identity{}, ErrUnsupportedOS
}
