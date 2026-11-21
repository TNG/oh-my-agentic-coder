package facade

import (
	"net"
	"net/http"
	"time"
)

const (
	// maxIdleConnsPerSidecar bounds the keep-alive connections the facade
	// parks per sidecar between requests.
	maxIdleConnsPerSidecar = 8
	// upstreamIdleTimeout closes a parked upstream connection that no
	// request has used for this long.
	upstreamIdleTimeout = 30 * time.Second
)

// upstream is the connection pool the facade keeps for one sidecar port.
// All proxied requests to that sidecar share its Transport, so keep-alive
// connections are reused instead of opened per request.
type upstream struct {
	transport *http.Transport
}

// newUpstreamPool builds the pool for one sidecar. idle is how long a parked
// connection may sit unused before it is closed.
func newUpstreamPool(idle time.Duration) *upstream {
	return &upstream{
		transport: &http.Transport{
			DialContext:           (&net.Dialer{Timeout: 2 * time.Second}).DialContext,
			DisableCompression:    true,
			ResponseHeaderTimeout: 30 * time.Second,
			MaxIdleConns:          maxIdleConnsPerSidecar,
			MaxIdleConnsPerHost:   maxIdleConnsPerSidecar,
			IdleConnTimeout:       idle,
		},
	}
}
