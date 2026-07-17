// Package intent holds an in-memory, TTL-evicted registry of agent-declared
// intents: why the agent wants to reach a given host or path. The agent
// writes intents via the facade POST /sandbox/intent endpoint; the network
// popup and learn-mode review read them in-process to show the user the
// agent's reason before deciding access.
package intent

import (
	"net"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// DefaultTTL is the session-scoped lifetime the CLI gives a Registry.
// Intents are advisory and short-lived: long enough to bridge a declare →
// request round-trip, short enough that a stale reason never outlives its
// task.
const DefaultTTL = 10 * time.Minute

const (
	// maxReasonLen bounds a recorded reason. The reason is agent-supplied
	// and surfaced verbatim in a fixed-size dialog; a runaway reason is
	// truncated rather than allowed to blow up the popup or the map.
	maxReasonLen = 500
	// maxEntries caps the registry. Intents are advisory and TTL-evicted,
	// but a misbehaving agent could spam distinct targets within one TTL
	// window; when full, recording a new target evicts the oldest.
	maxEntries = 512
)

// Entry is one recorded intent.
type Entry struct {
	Target string // host (lowercased) or absolute path (cleaned)
	Reason string
	Time   time.Time
}

// Registry is a thread-safe, TTL-evicted intent store. A nil *Registry
// is safe to call: Record is a no-op, all lookups return false.
//
// Trust boundary: the registry has no authentication of its own. It is
// reachable only through the facade, which binds a unix socket
// (file-permission gated) and 127.0.0.1 loopback — the same posture as
// the sibling /sandbox/denied endpoint. The recorded reason is
// agent-supplied and strictly advisory: it is shown to the user before a
// decision but never auto-grants access. A same-user local process could
// plant or read intents, but such a process already sits outside the
// sandbox and holds broader capabilities than the agent, so this is
// accepted rather than defended with a token (which the agent, being the
// legitimate writer, could not be constrained by anyway).
type Registry struct {
	mu      sync.Mutex
	entries map[string]Entry
	// explainMore tracks hosts for which the user clicked "Explain more" in
	// the network popup, keyed by normalized host → time requested. It is the
	// carrier for the "Explain more" signal on the GET /sandbox/intent
	// channel: an HTTPS/CONNECT denial discards the deny body, so the click is
	// surfaced here instead, where the agent's out-of-band lookup can reach
	// it. Consumed on read (one-shot) so a stale flag never drives a retry
	// loop after the user later declines for real.
	explainMore map[string]time.Time
	ttl         time.Duration
	stop        chan struct{}
	stopped     sync.Once
}

// New builds a Registry with the given TTL. Starts a background
// sweeper that evicts expired entries every ttl/2. Call Close to stop
// the sweeper.
func New(ttl time.Duration) *Registry {
	r := &Registry{
		entries:     map[string]Entry{},
		explainMore: map[string]time.Time{},
		ttl:         ttl,
		stop:        make(chan struct{}),
	}
	go r.sweep()
	return r
}

// Close stops the background sweeper. Safe to call multiple times.
// Records and lookups still work after Close (the map stays usable).
func (r *Registry) Close() {
	if r == nil {
		return
	}
	r.stopped.Do(func() { close(r.stop) })
}

// Record stores (or overwrites) an intent for target. target is
// normalized: hosts are lowercased, paths are cleaned+absolutized.
// Empty reason is ignored (no entry written).
func (r *Registry) Record(target, reason string) {
	if r == nil {
		return
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return
	}
	if len(reason) > maxReasonLen {
		reason = reason[:maxReasonLen]
	}
	target = normalize(target)
	if target == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.entries[target]; !exists && len(r.entries) >= maxEntries {
		r.evictOldest()
	}
	r.entries[target] = Entry{Target: target, Reason: reason, Time: time.Now()}
}

// evictOldest removes the entry with the earliest Time. Caller holds mu.
func (r *Registry) evictOldest() {
	var oldestKey string
	var oldest time.Time
	for k, e := range r.entries {
		if oldestKey == "" || e.Time.Before(oldest) {
			oldestKey, oldest = k, e.Time
		}
	}
	if oldestKey != "" {
		delete(r.entries, oldestKey)
	}
}

// Lookup returns the entry for target (normalized the same way as
// Record). Returns ok=false if missing or expired (expired entries are
// lazily deleted).
func (r *Registry) Lookup(target string) (Entry, bool) {
	if r == nil {
		return Entry{}, false
	}
	target = normalize(target)
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[target]
	if !ok {
		return Entry{}, false
	}
	if r.expired(e) {
		delete(r.entries, target)
		return Entry{}, false
	}
	return e, true
}

// LookupHost lowercases host and delegates to Lookup. Convenience for
// the netproxy layer which deals in hosts.
func (r *Registry) LookupHost(host string) (Entry, bool) {
	if r == nil {
		return Entry{}, false
	}
	return r.Lookup(strings.ToLower(host))
}

// MarkExplainMore records that the user clicked "Explain more" for target
// (a host) in the network popup. The flag is TTL-bounded and, like Record,
// is a no-op on a nil registry. It is set by the popup path (via the facade
// POST /sandbox/intent/explain endpoint) so the agent's out-of-band GET
// lookup can learn the user wants a fuller reason — the one signal an
// HTTPS/CONNECT denial cannot deliver in-band.
func (r *Registry) MarkExplainMore(target string) {
	if r == nil {
		return
	}
	target = normalize(target)
	if target == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.explainMore[target]; !exists && len(r.explainMore) >= maxEntries {
		r.evictOldestExplainMore()
	}
	r.explainMore[target] = time.Now()
}

// ConsumeExplainMore reports whether the user clicked "Explain more" for
// target and clears the flag (one-shot). Returns false if unset or expired.
// Consuming on read keeps the signal from outliving the click: after the
// agent reads it, re-declares, and retries, a subsequent real denial reverts
// to the "user declined — do not retry" hint rather than looping.
func (r *Registry) ConsumeExplainMore(target string) bool {
	if r == nil {
		return false
	}
	target = normalize(target)
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.explainMore[target]
	if !ok {
		return false
	}
	delete(r.explainMore, target)
	return !r.expiredTime(t)
}

// evictOldestExplainMore removes the earliest explain-more flag. Caller
// holds mu. Bounds a pathological flood even though the flag is user-gated.
func (r *Registry) evictOldestExplainMore() {
	var oldestKey string
	var oldest time.Time
	for k, t := range r.explainMore {
		if oldestKey == "" || t.Before(oldest) {
			oldestKey, oldest = k, t
		}
	}
	if oldestKey != "" {
		delete(r.explainMore, oldestKey)
	}
}

// LookupSubtree returns the live path intents related to dir: those
// whose target equals dir, lies under dir, or is an ancestor of dir.
// Host intents are ignored. It exists for the folder learn-review, where
// the offered candidate is a reduced ancestor of the specific paths the
// agent declared — an exact Lookup would miss them. Results are sorted by
// target for determinism.
func (r *Registry) LookupSubtree(dir string) []Entry {
	if r == nil {
		return nil
	}
	dir = normalize(dir)
	if !filepath.IsAbs(dir) {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Entry
	for k, e := range r.entries {
		if r.expired(e) {
			delete(r.entries, k)
			continue
		}
		if filepath.IsAbs(e.Target) && pathRelated(dir, e.Target) {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Target < out[j].Target })
	return out
}

// expired reports whether e is past the TTL. Caller holds mu.
func (r *Registry) expired(e Entry) bool {
	return r.expiredTime(e.Time)
}

// expiredTime reports whether a timestamp is past the TTL. Caller holds mu.
func (r *Registry) expiredTime(t time.Time) bool {
	return r.ttl > 0 && time.Since(t) > r.ttl
}

// pathRelated reports whether a and b are equal or one contains the other.
func pathRelated(a, b string) bool {
	sep := string(filepath.Separator)
	return a == b || strings.HasPrefix(a, b+sep) || strings.HasPrefix(b, a+sep)
}

// normalize maps a declared target to its canonical key so a lookup by
// bare host (network popup) or by path (folder review) matches what the
// agent recorded, tolerating common variations:
//
//   - URL form ("https://api.example.com/x") → the lowercased hostname.
//   - host:port ("api.example.com:443")      → the lowercased host.
//   - path form (has a separator, ~ or is absolute) → cleaned absolute path.
//   - anything else                          → lowercased as a bare host.
func normalize(target string) string {
	target = strings.TrimSpace(target)
	if target == "" {
		return ""
	}
	if strings.Contains(target, "://") {
		if u, err := url.Parse(target); err == nil && u.Hostname() != "" {
			return strings.ToLower(u.Hostname())
		}
	}
	if strings.ContainsRune(target, filepath.Separator) ||
		strings.HasPrefix(target, "~") ||
		filepath.IsAbs(target) {
		abs, err := filepath.Abs(filepath.Clean(expandTilde(target)))
		if err != nil {
			return ""
		}
		return abs
	}
	if host, _, err := net.SplitHostPort(target); err == nil && host != "" {
		return strings.ToLower(host)
	}
	return strings.ToLower(target)
}

// expandTilde replaces a leading ~ with HOME so filepath.Abs can resolve.
func expandTilde(p string) string {
	if strings.HasPrefix(p, "~") {
		if home, err := userHome(); err == nil {
			if p == "~" {
				return home
			}
			if strings.HasPrefix(p, "~/") || strings.HasPrefix(p, "~\\") {
				return filepath.Join(home, p[2:])
			}
		}
	}
	return p
}

// userHome is a seam for tests (they set HOME via t.Setenv).
func userHome() (string, error) {
	return osUserHome()
}

func (r *Registry) sweep() {
	if r == nil || r.ttl <= 0 {
		return
	}
	interval := r.ttl / 2
	if interval < time.Second {
		interval = time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-r.stop:
			return
		case <-t.C:
			r.evictExpired()
		}
	}
}

func (r *Registry) evictExpired() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, e := range r.entries {
		if r.expired(e) {
			delete(r.entries, k)
		}
	}
	for k, t := range r.explainMore {
		if r.expiredTime(t) {
			delete(r.explainMore, k)
		}
	}
}
