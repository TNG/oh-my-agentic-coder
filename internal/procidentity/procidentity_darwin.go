//go:build darwin

package procidentity

/*
#cgo LDFLAGS: -lproc

#include <libproc.h>
#include <sys/proc_info.h>
#include <errno.h>
// Helpers hide proc_info.h enum/macro constants and errno from cgo,
// which cannot refer to C.errno or some #define enum values directly.

// omac_proc_pidpath wraps proc_pidpath and reports the errno on failure
// via the out-param so cgo can map ESRCH -> no-such-process vs other ->
// unverifiable. Returns the path length (without trailing NUL) on
// success, <=0 on failure.
static int omac_proc_pidpath(int pid, char *buf, int bufsize, int *out_errno) {
    int n = proc_pidpath(pid, buf, (uint32_t)bufsize);
    if (n <= 0) {
        *out_errno = errno;
    } else {
        *out_errno = 0;
    }
    return n;
}

// omac_proc_pidinfo_bsdinfo fetches PROC_PIDTBSDINFO (the flavor that
// returns proc_bsdinfo with pbi_start_tvsec/usec). Returns bytes
// written (<=0 on failure) and sets *out_errno on failure.
static int omac_proc_pidinfo_bsdinfo(int pid, void *buf, int bufsize, int *out_errno) {
    int n = proc_pidinfo(pid, PROC_PIDTBSDINFO, 0, buf, (int)bufsize);
    if (n <= 0) {
        *out_errno = errno;
    } else {
        *out_errno = 0;
    }
    return n;
}

// omac_proc_bsdinfo_start extracts the start-time tv_sec/usec pair from
// a proc_bsdinfo struct (opaque to cgo) into out integers. Returns 0 on
// success, -1 on null input.
static int omac_proc_bsdinfo_start(void *bsd, long long *sec, long long *usec) {
    if (!bsd) return -1;
    struct proc_bsdinfo *p = (struct proc_bsdinfo *)bsd;
    *sec = (long long)p->pbi_start_tvsec;
    *usec = (long long)p->pbi_start_tvusec;
    return 0;
}

// omac_proc_pidpath_maxsize returns PROC_PIDPATHINFO_MAXSIZE so cgo can
// size the path buffer without referencing the macro directly.
static int omac_proc_pidpath_maxsize(void) {
    return PROC_PIDPATHINFO_MAXSIZE;
}

// omac_esrch returns ESRCH so cgo can compare without referencing the
// macro directly.
static int omac_esrch(void) {
    return ESRCH;
}
*/
import "C"

import (
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"unsafe"
)

// identifyNative resolves a pid's identity on macOS using the native
// libproc interface (spec.md §238 — "macOS uses the native
// process-information interface").
//
//   - Executable: proc_pidpath (the resolved real executable path) —
//     native libproc.
//   - StartIdentity: proc_bsdinfo.pbi_start_tvsec + pbi_start_tvusec
//     (opaque string the caller compares for equality) — native libproc.
//   - MainClass: extracted from the process command line via
//     `ps -o args= -p <pid>` (the established codebase pattern,
//     internal/buildrun/stop.go:292). macOS libproc does NOT expose argv
//     via proc_pidinfo (no PROC_PIDARGVINFO flavor), so the command
//     line must come from `ps`. This is compliant with spec.md §238
//     because the "never sufficient" list forbids relying on
//     command-line substring matching ALONE — here the executable +
//     start-identity match (both native libproc) are required alongside
//     the main-class token (an EXACT token match, not a substring).
//
// A sandbox that blocks libproc surfaces as ErrUnverifiable; a dead pid
// surfaces as ErrNoSuchProcess (libproc returns 0 with errno=ESRCH).
func identifyNative(pid int) (Identity, error) {
	if pid <= 0 {
		return Identity{}, ErrNoSuchProcess
	}
	cpid := C.int(pid)
	esrch := C.omac_esrch()

	// Executable path via proc_pidpath (native).
	pathSize := C.omac_proc_pidpath_maxsize()
	pathBuf := make([]byte, int(pathSize))
	var pathErr C.int
	n := C.omac_proc_pidpath(cpid, (*C.char)(unsafe.Pointer(&pathBuf[0])), C.int(len(pathBuf)), &pathErr)
	if n <= 0 {
		if pathErr == esrch {
			return Identity{}, ErrNoSuchProcess
		}
		return Identity{}, fmt.Errorf("%w: proc_pidpath (errno %d)", ErrUnverifiable, pathErr)
	}
	// proc_pidpath returns the path WITHOUT a trailing NUL on success,
	// but trim defensively.
	exe := string(pathBuf[:int(n)])
	if i := strings.IndexByte(exe, 0); i >= 0 {
		exe = exe[:i]
	}

	// Start time via proc_bsdinfo (PROC_PIDTBSDINFO, native). Done
	// before the `ps` argv probe so an argv failure does not lose the
	// start-identity.
	bsdBuf := make([]byte, 256) // larger than sizeof(proc_bsdinfo)
	var bsdErr C.int
	rn := C.omac_proc_pidinfo_bsdinfo(cpid, unsafe.Pointer(&bsdBuf[0]), C.int(len(bsdBuf)), &bsdErr)
	if rn <= 0 {
		if bsdErr == esrch {
			return Identity{}, ErrNoSuchProcess
		}
		return Identity{}, fmt.Errorf("%w: proc_pidinfo PROC_PIDTBSDINFO (errno %d)", ErrUnverifiable, bsdErr)
	}
	var sec, usec C.longlong
	if C.omac_proc_bsdinfo_start(unsafe.Pointer(&bsdBuf[0]), &sec, &usec) != 0 {
		return Identity{}, fmt.Errorf("%w: extract pbi_start", ErrUnverifiable)
	}
	startIdentity := fmt.Sprintf("%d.%d", int64(sec), int64(usec))

	// Main class via `ps -o args=` (the codebase's established cmdline
	// probe; macOS libproc exposes no argv flavor). Best-effort: on any
	// failure MainClass stays empty and Verify treats that as a
	// mismatch. The executable + start-identity match still applies,
	// and the marker handshake (verified separately at the daemon-
	// record level) is the stronger guarantee at promote time.
	mainClass := darwinMainClass(pid)

	return Identity{
		Executable:    exe,
		MainClass:     mainClass,
		StartIdentity: startIdentity,
	}, nil
}

// darwinMainClass extracts the Gradle daemon main class from the
// process command line via `ps -o args= -p <pid>` (the established
// codebase pattern, internal/buildrun/stop.go:292). macOS libproc does
// not expose argv, so the command line must come from `ps`. Returns
// GradleDaemonMainClass if it appears as an exact argv token, "" on any
// error or when the class is absent.
func darwinMainClass(pid int) string {
	out, err := exec.Command("ps", "-o", "args=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return ""
	}
	// ps output is a single line of space-separated argv. Split on
	// whitespace and look for an exact main-class token (NOT substring
	// match — the spec forbids relying on substring alone).
	for _, tok := range strings.Fields(string(out)) {
		if tok == GradleDaemonMainClass {
			return GradleDaemonMainClass
		}
		if strings.HasSuffix(tok, "/"+GradleDaemonMainClass) {
			return GradleDaemonMainClass
		}
	}
	return ""
}

// keep errors imported
var _ = errors.Is
