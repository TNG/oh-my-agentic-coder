// Package sidecarauth carries the per-spawn secret that proves a request to a
// sidecar came from the facade and not from the sandboxed agent.
package sidecarauth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
)

const (
	// EnvVar is the environment variable that hands the secret to the sidecar.
	EnvVar = "OMAC_SIDECAR_SECRET"
	// Header carries the secret on requests from the facade to the sidecar.
	Header = "X-Omac-Sidecar-Secret"
	// FacadeTokenHeader carries the agent's facade token; sidecars never see it.
	FacadeTokenHeader = "X-Omac-Facade-Token"
)

// Upstream is the address and secret of one running sidecar.
type Upstream struct {
	Port   int
	Secret string
}

// Mint returns a fresh random secret.
func Mint() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// Valid reports whether r carries exactly one Header value equal to secret.
// An empty secret never matches.
func Valid(r *http.Request, secret string) bool {
	if secret == "" {
		return false
	}
	values := r.Header.Values(Header)
	if len(values) != 1 {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(values[0]), []byte(secret)) == 1
}

// Require wraps next so that only requests carrying the secret reach it.
func Require(secret string, next http.Handler) (http.Handler, error) {
	if secret == "" {
		return nil, errors.New("sidecarauth: empty secret")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !Valid(r, secret) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	}), nil
}

// StripInbound removes both auth headers from h, including case variants and
// variants that use underscores instead of hyphens.
func StripInbound(h http.Header) {
	for key := range h {
		switch normalize(key) {
		case normalize(Header), normalize(FacadeTokenHeader):
			delete(h, key)
		}
	}
}

// Reserved reports whether name may not be used by a definition or skill for
// secrets, config or env_passthrough.
func Reserved(name string) bool {
	return strings.EqualFold(name, EnvVar)
}

func normalize(name string) string {
	return strings.ToLower(strings.ReplaceAll(name, "_", "-"))
}
