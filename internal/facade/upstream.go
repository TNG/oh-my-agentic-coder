package facade

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"
)

const (
	// maxConnsPerSidecar caps the requests in flight to one sidecar. It is
	// enforced by a semaphore rather than Transport.MaxConnsPerHost, which
	// waits without a deadline.
	maxConnsPerSidecar = 32
	// upstreamQueueWait is how long a request waits for a free slot before
	// the facade answers 503 sidecar-busy.
	upstreamQueueWait = 5 * time.Second
	// maxIdleConnsPerSidecar bounds the keep-alive connections the facade
	// parks per sidecar between requests.
	maxIdleConnsPerSidecar = 8
	// upstreamIdleTimeout closes a parked upstream connection that no
	// request has used for this long.
	upstreamIdleTimeout = 30 * time.Second
)

// errSidecarBusy is returned by acquire when no slot frees up in time.
var errSidecarBusy = errors.New("sidecar busy")

// upstream is the connection pool the facade keeps for one sidecar port.
// All proxied requests to that sidecar share its Transport, so keep-alive
// connections are reused instead of opened per request, and at most
// maxConnsPerSidecar of them are in flight at once.
type upstream struct {
	transport *http.Transport
	slots     chan struct{}
}

// newUpstreamPool builds the pool for one sidecar. idle is how long a parked
// connection may sit unused before it is closed.
func newUpstreamPool(idle time.Duration) *upstream {
	return &upstream{
		slots: make(chan struct{}, maxConnsPerSidecar),
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

// acquire takes one of the sidecar's request slots. It waits at most wait
// for a free slot (errSidecarBusy) and gives up when ctx ends (ctx.Err()).
// The returned release is safe to call more than once.
func (u *upstream) acquire(ctx context.Context, wait time.Duration) (release func(), err error) {
	release = sync.OnceFunc(func() { <-u.slots })
	select {
	case u.slots <- struct{}{}:
		return release, nil
	default:
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case u.slots <- struct{}{}:
		return release, nil
	case <-timer.C:
		return nil, errSidecarBusy
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
