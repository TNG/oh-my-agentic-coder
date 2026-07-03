// Package audit implements omac's security audit trail: a single,
// append-only, structured (JSON Lines) record of every security-relevant
// action omac performs — commands it spawns, network decisions it makes,
// facade requests it proxies, control-plane mutations, secret injection,
// and session lifecycle.
//
// Design highlights (see openspec/changes/add-audit-trail/design.md):
//
//   - One JSON object per line, sharing a fixed envelope (ts, run_id, seq,
//     type, mode, pid) so events are greppable, streamable, and
//     gap-detectable within a run.
//   - Redaction happens at the single Emit boundary: secret values and
//     secret namespace tokens are never written verbatim, even if a call
//     site passes them by mistake.
//   - The default log lives at a persistent, central, host-level path that
//     survives restarts (NOT the ephemeral per-run runtime dir).
//   - Failure mode is selectable: fail-open by default (a write error
//     becomes one stderr warning), or fail-closed under strict mode (a
//     write failure aborts the run via a registered fatal handler).
package audit

import (
	"sync/atomic"
	"time"
)

// Mode identifies which omac entrypoint produced the events.
type Mode string

const (
	ModeStart Mode = "start"
	ModeServe Mode = "serve"
)

// Event types. Dotted namespaces group related actions.
const (
	TypeSessionStart    = "session.start"
	TypeSessionStop     = "session.stop"
	TypeProcessExec     = "process.exec"
	TypeProcessExit     = "process.exit"
	TypeNetDecision     = "net.decision"
	TypeFacadeRequest   = "facade.request"
	TypeControlMutation = "control.mutation"
	TypeSecretInject    = "secret.inject"
	TypeRouteState      = "route.state"
)

// Event is one audit record. The envelope fields (Ts, RunID, Seq, Type,
// Mode, PID) are stamped by the Auditor at Emit time; callers fill only
// Type and the type-specific payload fields. Sensitive fields (Argv,
// Namespace) are passed through the redactor before serialization.
//
// A single flat struct with omitempty keeps each line compact and avoids a
// map[string]any (which would defeat the redactor's field-awareness).
type Event struct {
	// --- envelope (stamped by the Auditor) ---
	Ts    string `json:"ts"`
	RunID string `json:"run_id"`
	Seq   uint64 `json:"seq"`
	Type  string `json:"type"`
	Mode  Mode   `json:"mode"`
	PID   int    `json:"pid"`

	// --- common optional fields ---
	Skill     string `json:"skill,omitempty"`
	Namespace string `json:"namespace,omitempty"` // redacted (hashed) unless "" or "__global__"
	Detail    string `json:"detail,omitempty"`

	// --- session.start / session.stop ---
	Version        string `json:"version,omitempty"`
	Harness        string `json:"harness,omitempty"`
	SandboxProfile string `json:"sandbox_profile,omitempty"`
	SandboxBackend string `json:"sandbox_backend,omitempty"`
	ExitCode       *int   `json:"exit_code,omitempty"`

	// --- process.exec / process.exit ---
	Argv        []string `json:"argv,omitempty"` // secret values redacted
	Cwd         string   `json:"cwd,omitempty"`
	SecretNames []string `json:"secret_names,omitempty"` // names only, never values
	ConfigNames []string `json:"config_names,omitempty"`
	Sandboxed   *bool    `json:"sandboxed,omitempty"`
	DurationMS  int64    `json:"duration_ms,omitempty"`

	// --- net.decision ---
	Host      string `json:"host,omitempty"`
	Port      int    `json:"port,omitempty"`
	Allow     *bool  `json:"allow,omitempty"`
	Scope     string `json:"scope,omitempty"`  // once|host|suffix
	Source    string `json:"source,omitempty"` // prompt|learned|allowlist|blocklist|timeout|unavailable
	Persisted *bool  `json:"persisted,omitempty"`

	// --- facade.request ---
	Method         string `json:"method,omitempty"`
	Mount          string `json:"mount,omitempty"`
	Path           string `json:"path,omitempty"`
	UpstreamStatus int    `json:"upstream_status,omitempty"`
	BytesOut       int64  `json:"bytes_out,omitempty"`

	// --- control.mutation ---
	Action string `json:"action,omitempty"` // activate|deactivate|reload|reload-global
	Dir    string `json:"dir,omitempty"`
	Result string `json:"result,omitempty"` // ok|error summary

	// --- route.state ---
	State string `json:"state,omitempty"` // pending-credentials|broken
}

// bptr / iptr / sptr are small helpers for the pointer-typed optional
// fields (so "false"/"0" are distinguishable from "absent").
func bptr(b bool) *bool { return &b }
func iptr(i int) *int   { return &i }

// Auditor records audit events. It is safe for concurrent use. Callers
// hold a non-nil Auditor (see Nop) and call Emit unconditionally.
type Auditor interface {
	// Emit stamps the envelope, redacts sensitive fields, and writes the
	// event. It never panics and (in fail-open mode) never returns an
	// error to the caller.
	Emit(ev Event)
	// Close flushes and releases sinks. Safe to call more than once.
	Close() error
}

// seqCounter is a process-global monotonic sequence source shared by every
// sink of a single Auditor so gaps are detectable within a run.
type seqCounter struct{ n atomic.Uint64 }

func (s *seqCounter) next() uint64 { return s.n.Add(1) }

// nowRFC3339Nano is swappable in tests.
var nowRFC3339Nano = func() string { return time.Now().UTC().Format(time.RFC3339Nano) }
