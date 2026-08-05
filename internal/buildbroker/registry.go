package buildbroker

import (
	"sync"
	"time"
)

// activeRequest is the broker's in-memory record of one accepted
// request that has not yet completed. The registry is used ONLY for
// cancellation and drain — it does not track build state, output, or
// results (those live on the execute connection's goroutine). The
// registry is bounded by MaxActiveRequests; a new request that would
// exceed it is rejected as a policy denial before `accepted`.
type activeRequest struct {
	id string
	// graceful and force are the cancellation signals the broker
	// closes to deliver graceful and forced cancellation to the
	// engine invoker. The broker creates them here, passes them to
	// the invoker, and closes them on a cancel POST or parent
	// shutdown. They are buffered so a close is observable even if
	// the invoker never reads them (the invoker selects on them
	// alongside other channels; a closed channel is immediately
	// ready).
	graceful chan struct{}
	force    chan struct{}
	// done is closed by the execute goroutine when the invoker has
	// returned and the terminal result has been framed (or the
	// response is no longer writable). The registry removes the
	// request on done; a late cancel that races done sees the
	// tombstone instead.
	done chan struct{}
}

// registry is the broker's in-memory active-request registry plus the
// completed-ID tombstone map. It is bounded and thread-safe.
//
// Active requests are keyed by request ID. A cancel POST looks up the
// ID here; a hit closes the graceful/force signal, a miss consults the
// tombstone map (410 for a recent completion, 404 for an unknown ID).
//
// Tombstones are bounded by MaxTombstones and evicted by TTL
// (TombstoneTTL). The eviction is lazy: a stale tombstone is evicted
// on the next lookup that touches it, and a full map is evicted
// oldest-first on insert.
type registry struct {
	mu        sync.Mutex
	active    map[string]*activeRequest
	order     []string // FIFO of active IDs, for bounded eviction
	tomb      map[string]time.Time
	tombOrder []string
}

func newRegistry() *registry {
	return &registry{
		active:    map[string]*activeRequest{},
		tomb:      map[string]time.Time{},
		tombOrder: nil,
	}
}

// register adds an active request. It returns false if the registry is
// full (MaxActiveRequests) — the caller rejects the request as a
// policy denial before sending `accepted`. The caller has already
// generated the request ID and validated it is unique (not in active
// or tombstone).
func (r *registry) register(req *activeRequest) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.active) >= MaxActiveRequests {
		return false
	}
	r.active[req.id] = req
	r.order = append(r.order, req.id)
	return true
}

// activeIDs returns a snapshot of the active request IDs. Test-only
// seam: tests need the request ID to issue a cancel against a blocking
// request. The registry does not expose this to production code.
func (r *registry) activeIDs() []string {
	r.mu.Lock()
	out := make([]string, 0, len(r.active))
	for id := range r.active {
		out = append(out, id)
	}
	r.mu.Unlock()
	return out
}

// complete removes the active request and adds a tombstone. The
// execute goroutine calls this after the terminal result has been
// framed (or the response is no longer writable). idempotent: a
// second complete for the same ID is a no-op.
func (r *registry) complete(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.active[id]; !ok {
		return
	}
	delete(r.active, id)
	for i, x := range r.order {
		if x == id {
			r.order = append(r.order[:i], r.order[i+1:]...)
			break
		}
	}
	r.addTombstoneLocked(id)
}

// addTombstoneLocked adds a tombstone, evicting oldest-first when the
// tombstone map is full. Caller holds r.mu.
func (r *registry) addTombstoneLocked(id string) {
	if _, exists := r.tomb[id]; exists {
		return
	}
	r.evictStaleTombstonesLocked()
	if len(r.tomb) >= MaxTombstones && len(r.tombOrder) > 0 {
		oldest := r.tombOrder[0]
		delete(r.tomb, oldest)
		r.tombOrder = r.tombOrder[1:]
	}
	r.tomb[id] = time.Now()
	r.tombOrder = append(r.tombOrder, id)
}

// evictStaleTombstonesLocked removes tombstones older than
// TombstoneTTL. Caller holds r.mu.
func (r *registry) evictStaleTombstonesLocked() {
	now := time.Now()
	cutoff := now.Add(-TombstoneTTL * time.Second)
	kept := r.tombOrder[:0]
	for _, id := range r.tombOrder {
		if r.tomb[id].Before(cutoff) {
			delete(r.tomb, id)
			continue
		}
		kept = append(kept, id)
	}
	r.tombOrder = kept
}

// tombstoneStatus returns the cancel-route status for an id that is
// not active: 410 (gone) for a recent completion, 404 (not found) for
// an unknown or expired ID. The caller has already consulted lookup
// and got nil.
func (r *registry) tombstoneStatus(id string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ts, ok := r.tomb[id]; ok {
		if time.Since(ts) < TombstoneTTL*time.Second {
			return 410
		}
		delete(r.tomb, id)
	}
	return 404
}

// drainForShutdown closes graceful on every active request, then after
// forceDeadline closes force on every still-active request. It blocks
// until every active request has completed (its done channel is
// closed) or forceDeadline elapses. Used by parent shutdown; the
// broker stops accepting new requests before calling this.
func (r *registry) drainForShutdown(forceDeadline time.Duration) {
	r.mu.Lock()
	ids := make([]string, 0, len(r.active))
	reqs := make([]*activeRequest, 0, len(r.active))
	for id, req := range r.active {
		ids = append(ids, id)
		reqs = append(reqs, req)
	}
	r.mu.Unlock()
	// Stage 1: graceful on every active request.
	for _, req := range reqs {
		closeOnce(req.graceful)
	}
	// Wait for completions or the force deadline.
	deadline := time.After(forceDeadline)
	for _, req := range reqs {
		select {
		case <-req.done:
		case <-deadline:
			goto force
		}
	}
	return
force:
	// Stage 2: force on every still-active request.
	for _, req := range reqs {
		closeOnce(req.force)
	}
	// Wait (without bound) for each remaining request's done so the
	// engine's forced-cancel cleanup completes before the broker
	// returns. The engine's own forced-cancel path is bounded; this
	// wait is bounded by that path, not by another broker timer.
	for _, req := range reqs {
		<-req.done
	}
	_ = ids
}

// cancel delivers a cancellation stage to an active request. It is
// idempotent: a second graceful or force is a no-op. force implies
// graceful: a force cancel closes graceful first (in case the graceful
// request was lost or raced), then closes force. Returns true if the
// request was active (the caller returns 204), false if it was not
// (the caller consults the tombstone map).
func (r *registry) cancel(id string, stage cancelStage) bool {
	r.mu.Lock()
	req, ok := r.active[id]
	r.mu.Unlock()
	if !ok {
		return false
	}
	if stage == cancelStageForce {
		closeOnce(req.graceful)
		closeOnce(req.force)
	} else {
		closeOnce(req.graceful)
	}
	return true
}

// closeOnce closes a channel guarded by a recover so a double-close
// (idempotent cancel) does not panic.
func closeOnce(ch chan struct{}) {
	defer func() { _ = recover() }()
	close(ch)
}
