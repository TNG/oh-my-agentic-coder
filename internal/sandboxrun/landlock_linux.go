//go:build linux

package sandboxrun

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

// landlockFsWriteAccess is the set of filesystem rights the FS ruleset
// handles. Rights NOT in this set (read, execute, traverse) stay
// unrestricted, so read-only grants remain readable while every
// write-related operation requires an explicit allow rule on an
// ancestor directory. REFER must be handled so that link(2)/rename(2)
// across directories are denied when the source directory lacks the
// right — this is what blocks giving a read-only file a new name in a
// writable directory and writing through it.
var landlockFsWriteAccess = uint64(
	unix.LANDLOCK_ACCESS_FS_WRITE_FILE |
		unix.LANDLOCK_ACCESS_FS_TRUNCATE |
		unix.LANDLOCK_ACCESS_FS_REMOVE_FILE |
		unix.LANDLOCK_ACCESS_FS_REMOVE_DIR |
		unix.LANDLOCK_ACCESS_FS_REFER |
		unix.LANDLOCK_ACCESS_FS_MAKE_CHAR |
		unix.LANDLOCK_ACCESS_FS_MAKE_DIR |
		unix.LANDLOCK_ACCESS_FS_MAKE_REG |
		unix.LANDLOCK_ACCESS_FS_MAKE_SOCK |
		unix.LANDLOCK_ACCESS_FS_MAKE_FIFO |
		unix.LANDLOCK_ACCESS_FS_MAKE_BLOCK |
		unix.LANDLOCK_ACCESS_FS_MAKE_SYM)

// ApplyLandlockFS installs a Landlock filesystem ruleset that permits
// write-related operations only beneath the given read-write roots.
// Every other path — including read-only grants — is denied write
// access, so a hardlink from a read-only file into a writable directory
// cannot mutate the original: the source directory lacks REFER, so
// link(2) itself is denied. Read, execute, and traverse are not
// handled and remain unrestricted. Stacks on top of any existing
// Landlock ruleset (Landlock intersections rulesets).
//
// Paths that do not exist are skipped: bwrap uses --bind-try for
// not-yet-created dirs (e.g. unix-socket dirs), so they are absent from
// the namespace and need no rule.
func ApplyLandlockFS(readWriteRoots []string) error {
	abi := LandlockABI()
	if abi < landlockNetABI {
		return fmt.Errorf("landlock ABI >= %d required for filesystem ruleset; this kernel has ABI %d",
			landlockNetABI, abi)
	}
	attr := unix.LandlockRulesetAttr{
		Access_fs: landlockFsWriteAccess,
	}
	fdp, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET,
		uintptr(unsafe.Pointer(&attr)), unsafe.Sizeof(attr), 0)
	if errno != 0 {
		return fmt.Errorf("landlock_create_ruleset(fs): %w", errno)
	}
	fd := int(fdp)
	defer unix.Close(fd)

	for _, root := range readWriteRoots {
		root = filepath.Clean(root)
		f, err := os.Open(root)
		if err != nil {
			continue // not in the namespace (bwrap --bind-try skipped it)
		}
		pathBeneath := unix.LandlockPathBeneathAttr{
			Allowed_access: landlockFsWriteAccess,
			Parent_fd:      int32(f.Fd()),
		}
		_, _, errno := unix.Syscall6(unix.SYS_LANDLOCK_ADD_RULE,
			uintptr(fd),
			uintptr(unix.LANDLOCK_RULE_PATH_BENEATH),
			uintptr(unsafe.Pointer(&pathBeneath)),
			0, 0, 0)
		f.Close()
		if errno != 0 {
			return fmt.Errorf("landlock_add_rule(fs %q): %w", root, errno)
		}
	}

	runtime.LockOSThread()
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("prctl(NO_NEW_PRIVS): %w", err)
	}
	if _, _, errno := unix.Syscall(unix.SYS_LANDLOCK_RESTRICT_SELF, uintptr(fd), 0, 0); errno != 0 {
		return fmt.Errorf("landlock_restrict_self(fs): %w", errno)
	}
	return nil
}

// ExecInner replaces the current process with the inner command
// (stage2 tail call). Landlock restrictions survive execve.
//
// This runs inside the bwrap mount namespace, so PATH and the visible
// filesystem are the sandboxed ones: a binary that exists on the host
// but whose directory was not bound into the namespace is simply absent
// here. The bare execve error for that case is "no such file or
// directory", which gives the user no idea which file or how to fix it,
// so wrap it with the resolved path, the likely cause, and the grant
// flag that unblocks it.
func ExecInner(argv []string) error {
	requested := argv[0]
	path := requested
	if p, err := exec.LookPath(path); err == nil {
		path = p
	}
	err := unix.Exec(path, argv, os.Environ())
	// unix.Exec only returns on failure.
	if errors.Is(err, unix.ENOENT) || errors.Is(err, exec.ErrNotFound) {
		return fmt.Errorf(
			"cannot run %q inside the sandbox: %q not found in the sandbox filesystem.\n"+
				"This usually means the harness binary lives in a directory that was not\n"+
				"mounted into the sandbox (e.g. a version-manager path like\n"+
				"~/.local/share/mise/installs/..., ~/.asdf/, ~/.nvm/, or a custom prefix).\n"+
				"Grant read access to its directory, e.g.:\n"+
				"    omac sandbox run --read \"$(dirname \"$(command -v %s)\")\" -- ...\n"+
				"or add that directory to filesystem.read in your sandbox profile.\n"+
				"underlying error: %w",
			requested, path, requested, err)
	}
	return fmt.Errorf("exec %q (resolved to %q): %w", requested, path, err)
}
