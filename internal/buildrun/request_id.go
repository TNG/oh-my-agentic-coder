package buildrun

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
	"time"
)

// NewBuildRequestID generates a short, non-secret, time-ordered id for
// one `omac build` invocation (ticket 09, spec §254). It correlates the
// build.request audit event with container-policy denials emitted by the
// container proxy so the agent receives an actionable OMAC explanation
// naming the active request rather than only a wrapped Testcontainers
// failure. Format: b<unix-seconds-hex>-<4 random hex bytes>. Non-secret
// (it appears in denial messages the agent reads); collisions are
// negligible (4 random bytes + per-second ordering).
//
// A failing crypto/rand.Read means the host entropy source is broken — a
// host-fatal condition, not a recoverable build error. We panic (the
// build command cannot proceed without a request id to correlate
// denials against); this never happens on a healthy Linux/macOS host.
//
// This is the single source of truth; internal/cli and
// internal/buildengine both call it so audit correlation stays stable
// across the build-engine extraction without duplicating the helper.
func NewBuildRequestID() string {
	var buf [4]byte
	if _, err := rand.Read(buf[:]); err != nil {
		panic(fmt.Sprintf("omac build: generate build request id: crypto/rand.Read failed: %v (host entropy source broken)", err))
	}
	return fmt.Sprintf("b%s-%s", strconv.FormatInt(time.Now().Unix(), 16), hex.EncodeToString(buf[:]))
}
