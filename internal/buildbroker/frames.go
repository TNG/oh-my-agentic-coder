package buildbroker

import (
	"encoding/json"
	"io"
)

// frameType is the discriminator each NDJSON frame and each request
// body carries in its "type" field. The execute request body and the
// accepted/output/result response frames all use this. The cancel
// request body does NOT carry a type — it carries a "stage" field
// instead (see CancelBody) — because the cancel route is fixed by the
// path and the body is a single short object.
type frameType string

const (
	// frameTypeExecute is the request body type for an execute
	// request: {"type":"execute","worktree":"...","args":[...]}.
	frameTypeExecute frameType = "execute"

	// frameTypeAccepted is the first response frame, sent once the
	// broker has validated transport, token, bounds, and worktree
	// authorization and has registered the request:
	// {"type":"accepted","request_id":"..."}.
	frameTypeAccepted frameType = "accepted"

	// frameTypeOutput is a streamed output frame:
	// {"type":"output","stream":"stdout|stderr","data_base64":"..."}.
	// data_base64 carries up to MaxOutputFrameBytes of raw bytes
	// before base64. Output is base64-encoded so arbitrary bytes and
	// invalid UTF-8 preserve the direct-writer contract.
	frameTypeOutput frameType = "output"

	// frameTypeResult is the terminal result frame, emitted exactly
	// once after cleanup whenever the response remains writable:
	// {"type":"result","class":"success|build_failure|policy_denial|cancelled|service_failure","exit_code":N}.
	// A disconnected client is the only case in which the broker may
	// be unable to deliver it.
	frameTypeResult frameType = "result"
)

// outputStream is the stream discriminator on an output frame.
type outputStream string

const (
	streamStdout outputStream = "stdout"
	streamStderr outputStream = "stderr"
)

// ExecuteBody is the decoded execute request body. The handler decodes
// exactly one of these and requires EOF after it; trailing objects or
// bytes are rejected. Worktree is the client worktree candidate; the
// broker canonicalizes and authorizes it before any build code runs.
// Args are the raw arguments after `omac build` (the engine does NOT
// see "build" itself). For `omac build stop` the args carry the
// existing stop grammar (e.g. ["stop","--root","backend"]).
//
// Unknown JSON fields are rejected: the decoder uses
// json.Decoder.DisallowUnknownFields. An empty worktree or a missing
// "args" field is rejected. Args is bounded by MaxArgs.
type ExecuteBody struct {
	Type     string   `json:"type"`
	Worktree string   `json:"worktree"`
	Args     []string `json:"args"`
}

// CancelBody is the decoded cancel request body. Stage is "graceful" or
// "force". Both are idempotent; force implies graceful if the graceful
// request was lost or raced, then closes the force signal.
//
// Unknown JSON fields are rejected. An empty or unrecognized stage is
// rejected. The body is bounded by MaxCancelBodyBytes.
type CancelBody struct {
	Stage string `json:"stage"`
}

// cancelStage is the parsed stage.
type cancelStage int

const (
	cancelStageNone cancelStage = iota
	cancelStageGraceful
	cancelStageForce
)

// acceptedFrame is the first response frame.
type acceptedFrame struct {
	Type      string `json:"type"`
	RequestID string `json:"request_id"`
}

// outputFrame is a streamed output frame. Data is base64-encoded raw
// bytes (up to MaxOutputFrameBytes before encoding).
type outputFrame struct {
	Type       string `json:"type"`
	Stream     string `json:"stream"`
	DataBase64 string `json:"data_base64"`
}

// resultFrame is the terminal result frame. Class is the explicit
// buildengine.ResultClass; ExitCode is the translated CLI exit code.
// The broker emits exactly one of these per accepted request whenever
// the response remains writable.
type resultFrame struct {
	Type     string `json:"type"`
	Class    string `json:"class"`
	ExitCode int    `json:"exit_code"`
	// Message is an optional sanitized diagnostic for non-success
	// classes (omitted on success / build_failure with a clean
	// pass-through code). It is sanitized: credentials, keychain
	// values, the raw Docker endpoint, and host-only paths never
	// appear here. The CLI prints it omac-prefixed on stderr.
	Message string `json:"message,omitempty"`
}

// encodeFrame writes one NDJSON frame followed by a newline, then
// flushes. It is the single serialized frame writer concurrent stream
// writers submit through — concurrent stdout/stderr writers always
// produce valid whole NDJSON frames because every frame goes through
// this one function under the writer's lock.
func encodeFrame(w io.Writer, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		return err
	}
	_, err = w.Write([]byte("\n"))
	return err
}
