package buildrun

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// DaemonOwnerMarker is the cryptographically random, unguessable value the
// host injects into the OMAC-controlled Gradle daemon JVM args so the
// daemon can echo it back over the executor supervisor's private control
// channel and the host can prove the daemon that registered is the one
// the host started (ticket 07, spec.md §237).
//
// The marker is NOT a credential: it is an ownership claim, not a
// secret. It appears in the read-only gradle.properties (org.gradle.
// jvmargs), the pending DaemonRecord (buildcontrol.DaemonRecord.Marker),
// and the daemon-handshake JSON over the private Unix socket. It does
// not gate access to anything; its only purpose is unguessability — a
// stale or PID-recycled process cannot spoof it and get itself
// acknowledged as the leaf's owner. A crypto/rand failure is extremely
// unlikely; NewDaemonOwnerMarker surfaces it to the caller (the engine
// rejects the build as a service failure before launching the wrapper).
//
// It is a plain string (not a typed wrapper) so it flows through
// GradlePropertiesConfig.DaemonOwnerMarker, buildcontrol.DaemonRecord.
// Marker, and the handshake JSON without per-field accessors. The
// unguessability is enforced at mint time (NewDaemonOwnerMarker); the
// match is a constant-time compare (buildrun/daemon_handshake.go).
type DaemonOwnerMarker = string

// daemonOwnerMarkerBytes is the entropy length of a fresh marker
// (32 random bytes → 64 hex chars). 256 bits of entropy makes brute-
// forcing the marker to spoof an acknowledgement infeasible within the
// handshake's bounded timeout, and matches the codebase's token style
// (buildbroker.mintRequestID uses 128 bits; the marker doubles that
// because it guards a long-lived ownership claim, not a single request).
const daemonOwnerMarkerBytes = 32

// NewDaemonOwnerMarker returns a freshly minted, cryptographically
// random daemon-owner marker. The marker is hex-encoded for safe
// inclusion in a JVM system property
// (-Domac.daemon.owner=<marker>), the handshake JSON, and the daemon
// record's JSON schema. Returns an error only on a crypto/rand read
// failure (treated as a service failure — the build is rejected
// before the wrapper launches).
//
// The caller writes this marker into:
//   - GradlePropertiesConfig.DaemonOwnerMarker (rendered into
//     gradle.properties org.gradle.jvmargs as
//     -Domac.daemon.owner=<marker>), so the Gradle daemon carries it;
//   - buildcontrol.DaemonRecord.Marker (the pending ownership record
//     written before wrapper launch), so reconciliation and stop see
//     the same value;
//   - the expectedMarker argument of DaemonHandshakeChannel.
//     AwaitHandshake, which compares it (constant-time) against the
//     marker the daemon sends back over the private control channel.
func NewDaemonOwnerMarker() (DaemonOwnerMarker, error) {
	var b [daemonOwnerMarkerBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("buildrun: mint daemon owner marker: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}
