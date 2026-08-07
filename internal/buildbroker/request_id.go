package buildbroker

import (
	"crypto/rand"
	"encoding/hex"
)

// mintRequestID generates a 128-bit random request ID as a hex string.
// It is returned in the accepted frame and used in the cancel route.
// A crypto/rand failure is extremely unlikely; we surface it to the
// caller (the broker rejects the request as a 503 before `accepted`).
func mintRequestID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
