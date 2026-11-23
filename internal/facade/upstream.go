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
	// slowSlotWait is how long a request may wait for a slot before the
	// facade logs it.
	slowSlotWait = time.Second
	// maxIdleConnsPerSidecar bounds the keep-alive connections the facade
	// parks per sidecar between requests.
	maxIdleConnsPerSidecar = 8
	// upstreamIdleTimeout closes a parked upstream connection that no
	// request has used for this long.
	upstreamIdleTimeout = 30 * time.Second
)

// errSidecarBusy is returned by acquire when no slot frees up in time.
var errSidecarBusy = errors.New("sidecar busy")

// newUpstreamTransport builds the Transport that all proxied requests
// share. It pools keep-alive connections per sidecar port.
func newUpstreamTransport() *http.Transport {
	return &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 2 * time.Second}).DialContext,
		DisableCompression:    true,
		ResponseHeaderTimeout: 30 * time.Second,
		MaxIdleConnsPerHost:   maxIdleConnsPerSidecar,
		IdleConnTimeout:       upstreamIdleTimeout,
	}
}

// sidecarSlots bounds the requests in flight to one sidecar.
type sidecarSlots chan struct{}

func newSidecarSlots() sidecarSlots {
	return make(sidecarSlots, maxConnsPerSidecar)
}

// acquire takes one of the sidecar's request slots. It waits at most wait
// for a free slot (errSidecarBusy) and gives up when ctx ends (ctx.Err()).
// The returned release is safe to call more than once.
func (s sidecarSlots) acquire(ctx context.Context, wait time.Duration) (release func(), err error) {
	release = sync.OnceFunc(func() { <-s })
	select {
	case s <- struct{}{}:
		return release, nil
	default:
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case s <- struct{}{}:
		return release, nil
	case <-timer.C:
		return nil, errSidecarBusy
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
