// Package procidentity verifies the OS-level identity of a process for
// the build-control daemon ownership handshake (ticket 07, spec.md §238).
//
// A process qualifies as a leaf's Gradle daemon ONLY if ALL of:
//
//   - its resolved executable is the JDK binary the host resolved for the
//     build (expectedJDKExecutable),
//   - its main class is Gradle's daemon bootstrap
//     (org.gradle.launcher.daemon.bootstrap.GradleDaemon),
//   - its OS start identity (Linux /proc/<pid>/stat field 22 `starttime`
//     in clock ticks since boot; macOS `proc_bsdinfo` start time) is
//     unchanged from when the daemon was promoted to active.
//
// PID alone, command-line substring matching, and heuristic
// `registry.bin` parsing are NEVER sufficient (spec.md §238 — explicit
// "never sufficient" list). This package owns the PROCESS-identity half
// of the verification; the owner MARKER (the unguessable value the host
// injects into Gradle daemon JVM args and the daemon echoes back over
// the private control channel) is verified separately at the
// daemon-record level by internal/buildcontrol.
//
// The package exposes two top-level functions, Identify and Verify, that
// delegate to platform-specific native implementations
// (procidentity_linux.go, procidentity_darwin.go). Tests inject a fake
// by swapping the package-level `identify` / `verify` function vars —
// the same seam style used by internal/buildrun.StopDaemonOptions.Cmdline
// (stop.go:57).
package procidentity

import (
	"errors"
)

// Identity is the OS-level identity of a process: the resolved
// executable path, the Gradle daemon main class if extractable, and the
// OS start identity (an opaque string the caller compares for equality
// across calls — Linux starttime in clock ticks, macOS start time).
//
// StartIdentity is platform-opaque: callers MUST compare it only for
// string equality against a previously recorded value, never parse it.
// Empty StartIdentity means the platform could not extract it (the
// caller treats this as ErrUnverifiable rather than trusting an empty
// match).
type Identity struct {
	// Executable is the resolved real executable path of the process
	// (Linux: /proc/<pid>/exe symlink target; macOS: proc_pidpath).
	Executable string

	// MainClass is the Gradle main class extracted from the process
	// command line if present and recognised
	// (org.gradle.launcher.daemon.bootstrap.GradleDaemon); empty if the
	// command line could not be read or did not contain a recognisable
	// Gradle daemon main class.
	MainClass string

	// StartIdentity is the OS start identity (Linux /proc/<pid>/stat
	// field 22 `starttime`; macOS proc_bsdinfo start time). Opaque
	// string compared only for equality.
	StartIdentity string
}

// GradleDaemonMainClass is the well-known Gradle daemon bootstrap main
// class. A process qualifies as the leaf's Gradle daemon only if its
// command line contains this class (spec.md §238 — "main class is
// Gradle's daemon bootstrap").
const GradleDaemonMainClass = "org.gradle.launcher.daemon.bootstrap.GradleDaemon"

// Sentinel errors. Callers MUST use errors.Is, not ==, so platform
// implementations can wrap them with low-level detail.
var (
	// ErrUnsupportedOS is returned by Identify/Verify on any OS without
	// a native implementation (anything other than linux and darwin).
	ErrUnsupportedOS = errors.New("procidentity: unsupported OS")

	// ErrNoSuchProcess is returned when the pid is not a live process.
	ErrNoSuchProcess = errors.New("procidentity: no such process")

	// ErrUnverifiable is returned when the platform cannot extract one
	// of the required identity fields (e.g. a sandbox blocks /proc on
	// Linux or libproc on macOS). The caller treats this as "leave the
	// record but block the leaf / fail closed" (spec.md §239).
	ErrUnverifiable = errors.New("procidentity: process identity unverifiable")
)

// Identify resolves the identity of pid. Returns:
//
//   - ErrNoSuchProcess if the pid is not alive,
//   - ErrUnsupportedOS on non-linux/darwin,
//   - ErrUnverifiable if the platform cannot extract one of the
//     required fields (e.g. a sandbox blocks /proc or libproc),
//   - a non-nil Identity on success.
//
// Identify is the low-level primitive; callers that want the boolean
// "is this the leaf's Gradle daemon" verdict should use Verify.
//
// Tests inject a fake by swapping the package-level `identify` var.
func Identify(pid int) (Identity, error) {
	return identify(pid)
}

// Verify reports whether pid is a Gradle daemon running the expected JDK
// executable with an unchanged OS start identity. expectedStart is the
// StartIdentity recorded when the daemon was promoted to active; the
// empty string means "no prior identity recorded, verify process is
// alive + executable + main class only" (used at promote time, when the
// host has just verified the marker handshake and wants to capture the
// start identity for future comparisons).
//
// Returns (true, identity, nil) when the process is verified,
// (false, identity, nil) when the process is alive but does not match
// (executable mismatch, main class missing, or — when expectedStart is
// non-empty — start identity changed / PID reused), and (false,
// zero-Identity, err) for ErrNoSuchProcess, ErrUnsupportedOS, or
// ErrUnverifiable.
//
// When the platform returns ErrUnverifiable, Verify propagates it
// unchanged so the caller can apply the fail-closed policy (block the
// leaf, keep the record) rather than treating unverifiable as a
// mismatch that would retire the record.
//
// Tests inject a fake by swapping the package-level `verify` var.
func Verify(pid int, expectedJDKExecutable, expectedStart string) (bool, Identity, error) {
	return verify(pid, expectedJDKExecutable, expectedStart)
}

// identify / verify are the platform-specific implementations. They are
// package-level vars (not unexported functions) so tests can swap them
// without spawning real processes. The same seam style as
// buildrun.StopDaemonOptions.Cmdline.
var (
	identify = identifyNative
	verify   = verifyNative
)

// verifyNative is the default Verify implementation: it calls Identify
// and applies the match rules. Platform code never overrides `verify`
// directly — it overrides `identify`. (Keeping `verify` overridable
// lets tests for the build-control reconciliation stub the whole verdict
// without re-implementing the match rules.)
func verifyNative(pid int, expectedJDKExecutable, expectedStart string) (bool, Identity, error) {
	id, err := identify(pid)
	if err != nil {
		return false, Identity{}, err
	}
	if id.Executable != expectedJDKExecutable {
		return false, id, nil
	}
	if id.MainClass != GradleDaemonMainClass {
		return false, id, nil
	}
	if expectedStart != "" && id.StartIdentity != expectedStart {
		// PID reuse: the recorded start identity no longer matches.
		return false, id, nil
	}
	return true, id, nil
}
