package buildbroker

import (
	"io"

	"github.com/tngtech/oh-my-agentic-coder/internal/buildengine"
)

// EngineInvoker is the seam the broker uses to convert an accepted
// execute request into a build-engine invocation. The broker contains
// no build policy or execution logic; the real adapter constructs
// buildengine.Options from the authorized worktree + raw args, wires
// the snapshot provider, proxy starter, cancellation signals, stdout
// and stderr writers, and calls buildengine.Run (or buildengine.Stop
// in a later gate). Tests inject a stub to assert protocol behavior
// without real build execution.
//
// The invoker receives:
//
//   - worktree: the canonical, authorized worktree (the broker has
//     already canonicalized and authorized it).
//   - args: the raw arguments after `omac build` (the invoker does
//     NOT see "build" itself). For `omac build stop` the args carry
//     the existing stop grammar; the broker refuses stop in this gate
//     via StopRefuser before the invoker runs.
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

// StopRefuser is the seam the broker uses to refuse `omac build stop`
// in this gate. The broker carries the stop grammar through the
// execute operation (the args reach the broker unchanged) but refuses
// it before the EngineInvoker runs. A later gate replaces the refuser
// with a real stop adapter that calls buildengine.Stop.
//
// The refuser inspects the raw args and returns true if the request is
// a stop request (the first arg is "stop"). The broker then frames a
// policy_denial result with a "brokered stop is not enabled in this
// gate" diagnostic instead of invoking the engine.
//
// DefaultStopRefuser is the default implementation; tests can inject a
// different one to assert the refusal path.
type StopRefuser func(args []string) bool

// DefaultStopRefuser returns true when the raw args carry the stop
// grammar: the first arg (after `omac build`) is the literal "stop".
// This preserves the existing grammar — `omac build stop [--root <rel>]`
// — without executing it.
func DefaultStopRefuser(args []string) bool {
	return len(args) > 0 && args[0] == "stop"
}
