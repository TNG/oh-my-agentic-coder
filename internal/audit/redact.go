package audit

import (
	"crypto/sha256"
	"encoding/hex"
)

// redactedMarker replaces any argv element that matches a known secret.
const redactedMarker = "<redacted>"

// GlobalNamespace mirrors facade.GlobalNamespace. It is duplicated here to
// avoid importing the facade package from audit (which would create an
// import cycle once the facade depends on audit). Kept in sync by a test.
const GlobalNamespace = "__global__"

// redactor scrubs sensitive material at the single Emit boundary. It holds
// the set of known secret *values* so that even an argv a caller forgot to
// tag cannot leak a secret. The always-safe path remains passing secret
// NAMES (Event.SecretNames), never values.
type redactor struct {
	secretValues map[string]struct{}
}

// newRedactor builds a redactor from the known secret values. Empty and
// very short values are ignored: matching a 1-2 char "secret" against argv
// would redact innocuous tokens ("-", "0") and leak nothing useful.
func newRedactor(secretValues []string) *redactor {
	m := make(map[string]struct{}, len(secretValues))
	for _, v := range secretValues {
		if len(v) < 3 {
			continue
		}
		m[v] = struct{}{}
	}
	return &redactor{secretValues: m}
}

// apply returns a copy of ev with sensitive fields scrubbed. It never
// mutates the caller's slices.
func (r *redactor) apply(ev Event) Event {
	if len(ev.Argv) > 0 {
		ev.Argv = r.redactArgv(ev.Argv)
	}
	ev.Namespace = redactNamespace(ev.Namespace)
	return ev
}

// redactArgv copies argv, replacing any element equal to a known secret
// value with the redacted marker.
func (r *redactor) redactArgv(argv []string) []string {
	out := make([]string, len(argv))
	for i, a := range argv {
		if _, ok := r.secretValues[a]; ok {
			out[i] = redactedMarker
			continue
		}
		out[i] = a
	}
	return out
}

// redactNamespace hashes a per-directory namespace token so a secret
// bearer token never appears in the log. The non-secret sentinels — the
// empty (flat) namespace and the reserved GlobalNamespace — pass through
// unchanged, since they carry no secret and are useful for correlation.
func redactNamespace(ns string) string {
	if ns == "" || ns == GlobalNamespace {
		return ns
	}
	sum := sha256.Sum256([]byte(ns))
	return "ns_" + hex.EncodeToString(sum[:6]) // 12 hex chars
}
