//go:build linux

package sandboxrun

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Landlock network support (TCP bind/connect rules) requires ABI v4
// (Linux >= 6.7).
const landlockNetABI = 4

// landlockRuleNetPort is LANDLOCK_RULE_NET_PORT (not yet in x/sys).
const landlockRuleNetPort = 2

// LandlockABI returns the kernel's Landlock ABI version (0 when
// Landlock is unavailable).
func LandlockABI() int {
	v, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET,
		0, 0, uintptr(unix.LANDLOCK_CREATE_RULESET_VERSION))
	if errno != 0 {
		return 0
	}
	return int(v)
}

// LandlockNetSupported reports whether TCP NetPort rules can be enforced.
func LandlockNetSupported() bool {
	return LandlockABI() >= landlockNetABI
}

// landlockNetPortAttr mirrors struct landlock_net_port_attr.
type landlockNetPortAttr struct {
	AllowedAccess uint64
	Port          uint64
}

// ApplyLandlockNet installs a Landlock ruleset that restricts TCP
// connect to connectPorts and TCP bind to bindPorts, then locks it in
// with no_new_privs + restrict_self. Irreversible for this process and
// all descendants. Empty slices mean "deny all" for that operation.
func ApplyLandlockNet(connectPorts, bindPorts []int) error {
	if !LandlockNetSupported() {
		return fmt.Errorf("landlock ABI >= %d required for network rules (kernel >= 6.7); this kernel has ABI %d",
			landlockNetABI, LandlockABI())
	}
	attr := unix.LandlockRulesetAttr{
		Access_net: unix.LANDLOCK_ACCESS_NET_BIND_TCP | unix.LANDLOCK_ACCESS_NET_CONNECT_TCP,
	}
	fdp, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET,
		uintptr(unsafe.Pointer(&attr)), unsafe.Sizeof(attr), 0)
	if errno != 0 {
		return fmt.Errorf("landlock_create_ruleset: %w", errno)
	}
	fd := int(fdp)
	defer unix.Close(fd)

	addRule := func(access uint64, port int) error {
		rule := landlockNetPortAttr{AllowedAccess: access, Port: uint64(port)}
		_, _, errno := unix.Syscall6(unix.SYS_LANDLOCK_ADD_RULE,
			uintptr(fd),
			uintptr(landlockRuleNetPort),
			uintptr(unsafe.Pointer(&rule)),
			0, 0, 0)
		if errno != 0 {
			return fmt.Errorf("landlock_add_rule(port %d): %w", port, errno)
		}
		return nil
	}
	for _, p := range connectPorts {
		if err := addRule(unix.LANDLOCK_ACCESS_NET_CONNECT_TCP, p); err != nil {
			return err
		}
	}
	for _, p := range bindPorts {
		if err := addRule(unix.LANDLOCK_ACCESS_NET_BIND_TCP, p); err != nil {
			return err
		}
	}

	// restrict_self applies to the calling thread; lock the OS thread
	// and rely on the immediately following exec to fan it out to the
	// whole (single-threaded post-exec) process image.
	runtime.LockOSThread()
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("prctl(NO_NEW_PRIVS): %w", err)
	}
	if _, _, errno := unix.Syscall(unix.SYS_LANDLOCK_RESTRICT_SELF, uintptr(fd), 0, 0); errno != 0 {
		return fmt.Errorf("landlock_restrict_self: %w", errno)
	}
	return nil
}

// ExecInner replaces the current process with the inner command
// (stage2 tail call). Landlock restrictions survive execve.
func ExecInner(argv []string) error {
	path := argv[0]
	if p, err := exec.LookPath(path); err == nil {
		path = p
	}
	return unix.Exec(path, argv, os.Environ())
}
