// Package buildrun implements the `omac build` adapter layer: it resolves
// a build request (repository-owned Gradle wrapper under a root inside the
// canonical worktree), derives the executor grant set (worktree + resolved
// OMAC cache leaf + private temp, following GRADLE_USER_HOME from the
// existing cache-scope machinery), builds the sandboxed child argv via
// sandboxrun, and runs one restricted executor process per request with
// streaming output and staged cancellation.
package buildrun

import (
	"errors"
	"fmt"
	"strings"
)

// Exit codes for `omac build`. 0 and any other build exit code pass through
// from the build tool verbatim; 3, 4 and 10 are reserved by omac. The
// mapping from build-exit codes onto those reserved values is done by
// ExitCode().
const (
	// ExitPolicyDenied is returned when OMAC policy rejects the request
	// before any build code runs (adapter unsupported, wrapper/root
	// resolution failure, worktree escape).
	ExitPolicyDenied = 3
	// ExitCancelled is returned when the caller cancelled the build
	// (SIGINT/SIGTERM to omac) or the build's own exit code mappable to a
	// signal kill is known to follow a cancellation.
	ExitCancelled = 4
	// ExitServiceFailure is returned for OMAC infrastructure failures
	// (sandbox unavailable, exec error) with an omac-prefixed diagnostic.
	//
	// 10, not 1: Gradle's canonical build-failure rc IS 1, so a service
	// failure was indistinguishable from a plain build failure by exit
	// code. All omac-reserved build codes are command-local (the cli.go
	// global table only constrains other subcommands); the build
	// reservations are chosen to avoid 0/1 (Gradle success/failure), 2
	// (Gradle CLI misuse), 3/4 (already reserved here), and the shell
	// 126/127/128+n signal conventions.
	ExitServiceFailure = 10
	// CancelledMarker is printed to stderr before a cancelled build
	// returns ExitCancelled, so rc==4 + marker is distinguishable from a
	// raw `gradle exit 4` (which never prints the omac-prefixed marker).
	CancelledMarker = "omac build: cancelled"
)

// AdapterGradle is the required literal adapter token after `--`.
const AdapterGradle = "gradle"

// errUsage marks a request/grammar error: the caller can fix and retry.
// Distinct from policy denials (which exit 3 in the CLI): usage errors are
// still policy denials per the exit-code contract — no build code ran.
var errRequest = errors.New("build request rejected")

// RequestError describes a rejected build request. CLI maps it to
// ExitPolicyDenied with a structured message.
type RequestError struct {
	msg string
}

func (e *RequestError) Error() string { return e.msg }
func (e *RequestError) Is(target error) bool {
	return target == errRequest
}

// Request is a parsed `omac build` invocation.
type Request struct {
	// Root is the raw --root value ("." when omitted).
	Root string
	// Args are the adapter arguments passed through unchanged.
	Args []string
}

// ParseArgs parses `omac build` arguments:
//
//	omac build [--root <rel>] -- gradle <args...>
//
// The adapter token after `--` is required and must be the literal "gradle"
// (the Maven seam: any other token yields "unsupported adapter"). Everything
// after the adapter token passes through to the build tool unchanged.
func ParseArgs(args []string) (Request, error) {
	r := Request{Root: "."}
	// Find the `--` separator: flags must precede it, everything after is
	// the adapter token + pass-through args.
	sep := -1
	for i, a := range args {
		if a == "--" {
			sep = i
			break
		}
	}
	if sep < 0 {
		return Request{}, &RequestError{msg: "missing `-- gradle <args...>` separator (usage: omac build [--root <rel>] -- gradle <args...>)"}
	}
	flags, rest := args[:sep], args[sep+1:]
	for i := 0; i < len(flags); i++ {
		a := flags[i]
		switch {
		case a == "--root":
			if i+1 >= len(flags) {
				return Request{}, &RequestError{msg: "--root requires a value"}
			}
			r.Root = flags[i+1]
			i++
		case strings.HasPrefix(a, "--root="):
			r.Root = strings.TrimPrefix(a, "--root=")
		default:
			return Request{}, &RequestError{msg: fmt.Sprintf("unknown flag %q (usage: omac build [--root <rel>] -- gradle <args...>)", a)}
		}
	}
	if r.Root == "" {
		return Request{}, &RequestError{msg: "--root must not be empty"}
	}
	if len(rest) == 0 {
		return Request{}, &RequestError{msg: "missing adapter token after `--` (want `-- gradle <args...>`)"}
	}
	if rest[0] != AdapterGradle {
		return Request{}, &RequestError{msg: fmt.Sprintf("unsupported adapter %q: v0 supports the literal adapter token %q only", rest[0], AdapterGradle)}
	}
	r.Args = rest[1:]
	return r, nil
}

// ExitCode maps a build executor outcome onto the documented exit-code
// contract:
//
//	0                  build success
//	gradle's code      build failure (wrapper's own exit code, incl. 128+n
//	                   when the build itself was killed by a signal)
//	3                  policy denial (rejected before any build code ran;
//	                   asserted by the CLI, which never reaches here)
//	4                  cancellation (SIGINT/SIGTERM honored during the build;
//	                   CancelledMarker precedes it on stderr)
//	10                 service failure (sandbox unavailable, exec error)
//
// The cancelled flag distinguishes "the build died of a signal we sent
// because the caller cancelled" (-> 4) from "the build died of a signal on
// its own" (-> 128+n pass-through): without it both would read 130. A raw
// gradle exit code of 3 or 4 passes through unchanged — the policy-denial/
// cancellation stderr markers (printed only by the omac paths) are what
// disambiguate, matching how `omac sandbox run` passes codes through.
func ExitCode(buildExit int, cancelled bool, err error) int {
	switch {
	case err != nil:
		return ExitServiceFailure
	case cancelled:
		return ExitCancelled
	default:
		return buildExit
	}
}
