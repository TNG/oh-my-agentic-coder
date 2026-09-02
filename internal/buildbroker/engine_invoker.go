package buildbroker

import (
	"io"

	"github.com/tngtech/oh-my-agentic-coder/internal/buildengine"
)

// EngineInvoker is the seam the broker uses to convert an accepted
// execute request into a build-engine invocation. The broker contains
// no build policy or execution logic; the real adapter inspects the
// raw args, dispatches `omac build stop` to buildengine.StopBrokered
// (the distinct brokered-stop engine op, ticket 07) and every other
// invocation to buildengine.Run, constructs the engine Options from
// the authorized worktree + cache scope + auditor + snapshot provider,
// wires the broker's graceful/force cancellation signals to the
// engine, and returns the engine's Result. Tests inject a stub to
// assert protocol behavior without real build execution.
//
// The invoker receives:
//
//   - worktree: the canonical, authorized worktree (the broker has
//     already canonicalized and authorized it).
//   - args: the raw arguments after `omac build` (the invoker does
//     NOT see "build" itself). For `omac build stop` the first arg
//     is the literal "stop"; the invoker strips it and dispatches the
//     remaining args to buildengine.StopBrokered (ticket 07, Phase 4 —
//     the broker no longer refuses stop; it routes it to the verified
//     daemon-control engine op). For an ordinary build the invoker
//     passes the args verbatim to buildengine.Run.
//   - stdout/stderr: byte-preserving writers. The broker wraps them
//     so each write is chunked into MaxOutputFrameBytes-sized output
//     frames and submitted through one serialized frame writer.
//   - graceful/force: the cancellation signals. The broker closes
//     graceful on a graceful cancel or execute-connection disconnect;
//     it closes force on a force cancel or after ForceDeadline during
//     parent shutdown.
//
// The invoker returns the engine's Result. The broker frames it as the
// terminal result; a panic in the invoker is recovered by the broker
// and framed as a sanitized service_failure.
type EngineInvoker func(worktree string, args []string, stdout, stderr io.Writer, graceful, force <-chan struct{}) buildengine.Result
