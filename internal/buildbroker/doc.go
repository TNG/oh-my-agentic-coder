// Package buildbroker owns the host build broker: protocol decoding,
// framing, authentication, input bounds, active-worktree authorization,
// conversion of protocol requests into build-engine invocations, an
// in-memory active-request registry used only for cancellation and
// drain, byte-preserving bounded stdout/stderr streaming, graceful and
// forced cancellation delivery, disconnect and write-failure handling,
// terminal result framing, and broker shutdown/draining.
//
// The broker contains NO build policy or execution logic. The build
// engine (internal/buildengine) owns orchestration; the broker only
// converts wire requests into engine invocations and frames the
// outcomes. Control-plane wiring constructs and mounts the broker; it
// does not contain build policy either.
//
// # Endpoints
//
// The broker mounts two routes on the parent's loopback control
// listener (build endpoints are registered ONLY on a loopback
// listener; a non-loopback configuration disables them and managed
// build fails closed):
//
//	POST /__omac__/build/v1                     (execute)
//	POST /__omac__/build/v1/<request_id>/cancel (cancel)
//
// Both endpoints require an Authorization: Bearer <token> header; the
// token is compared in constant time. Method, content type,
// authentication, body size, JSON shape, unknown-field rejection,
// worktree authorization, and broker-shutdown rejection happen before
// the execute handler sends 200. Both endpoints decode exactly one
// JSON object and require EOF after it; trailing objects or bytes are
// rejected.
//
// # Execute
//
// The finite JSON body carries the client worktree candidate and the
// raw arguments after `omac build`:
//
//	{"type":"execute","worktree":"/canonical/worktree","args":["--root","backend","--","gradle","test"]}
//
// `omac build stop` reuses the execute operation with its existing
// grammar (ticket 07, Phase 4: the broker no longer refuses stop; it
// routes `args[0]=="stop"` to buildengine.StopBrokered, the distinct
// brokered-stop engine op that uses verified daemon control via
// procidentity + the host-only ownership records — NOT the repo
// wrapper):
//
//	{"type":"execute","worktree":"/canonical/worktree","args":["stop","--root","backend"]}
//
// The execute body is fully decoded before the response starts; the
// handler does not depend on Go HTTP/1 full-duplex request-body
// behavior. The response is HTTP/1.1 chunked NDJSON; the handler
// flushes each complete frame:
//
//	{"type":"accepted","request_id":"<128-bit-random-id>"}
//	{"type":"output","stream":"stdout","data_base64":"..."}
//	{"type":"output","stream":"stderr","data_base64":"..."}
//	{"type":"result","class":"success","exit_code":0}
//
// Output is base64-encoded so arbitrary bytes and invalid UTF-8
// preserve the current direct-writer contract. Each stream preserves
// byte order; cross-stream ordering is best-effort. Concurrent stream
// writers submit complete frames through one serialized frame writer.
// Each output frame contains at most MaxOutputFrameBytes of raw bytes
// before base64.
//
// After acceptance, CLI grammar errors, path/wrapper denials, manifest
// denials, queue timeouts, launch failures, cancellation, build exits,
// and cleanup outcomes use output plus a terminal result frame. Exactly
// one terminal result is emitted whenever the response remains
// writable; a disconnected client is the only case in which the broker
// may be unable to deliver it.
//
// # Cancellation
//
// Cancellation is a one-shot POST rather than client frames on the
// execute connection:
//
//	POST /__omac__/build/v1/<request_id>/cancel
//	{"stage":"graceful"}   // or {"stage":"force"}
//
// The first client signal requests graceful cancellation; the second
// requests force. Both operations are idempotent. Force implies
// graceful if the graceful request was lost or raced, then closes the
// force signal. Unknown IDs return 404; completed IDs return 410 for a
// short bounded tombstone lifetime. A successful cancellation
// (including an idempotent repeat) returns 204.
//
// After acceptance, execute-connection disconnect, response write
// failure, or client disappearance triggers graceful cancellation
// followed by the existing forced deadline.
//
// # Bounds
//
// The execute body is limited to MaxExecuteBodyBytes via
// http.MaxBytesReader. A cancellation body is limited to
// MaxCancelBodyBytes. At most MaxArgs arguments are accepted. The
// active-request registry and completed-ID tombstones are bounded.
// Unknown JSON fields, methods, content types, or operation types are
// rejected. Body and argument limits are local control-plane DoS
// bounds, not build-policy limits.
//
// # Lifecycle
//
// Parent shutdown is explicit and precedes HTTP server close: stop
// accepting builds, gracefully cancel queued and active requests,
// force after the bounded deadline, wait for engine cleanup, then
// close the control listener. Fatal strict-audit paths call the same
// shutdown path before any os.Exit. Per-request panic recovery runs
// cleanup and emits a sanitized service-failure result when the stream
// remains writable. The broker never terminates the parent process on
// a per-request panic.
//
// # Security
//
// The build token authorizes a client to request a constrained build;
// it does not authorize host paths or capabilities. The token,
// keychain value, raw Docker endpoint, and host environment never
// appear in executor env/args/control-files/output/audit — the broker
// does not carry any of them across the engine seam. Client input
// cannot select a wrapper, manifest content, proxy endpoint,
// credential, image policy, cache path, environment, or audit ID; the
// host owns all of those.
package buildbroker
