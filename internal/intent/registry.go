// Package intent holds an in-memory, TTL-evicted registry of agent-declared
// intents: why the agent wants to reach a given host or path. The agent
// writes intents via the facade POST /sandbox/intent endpoint; the network
// popup and learn-mode review read them in-process to show the user the
// agent's reason before deciding access.
package intent

import (
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Entry is one recorded intent.
type Entry struct {
	Target string // host (lowercased) or absolute path (cleaned)
	Reason string
	Time   time.Time
}

// Registry is a thread-safe, TTL-evicted intent store. A nil *Registry
// is safe to call: Record is a no-op, all lookups return false.
type Registry struct {
	mu      sync.Mutex
	entries map[string]Entry
	ttl     time.Duration
	stop    chan struct{}
	stopped sync.Once
}

// New builds a Registry with the given TTL. Starts a background
// sweeper that evicts expired entries every ttl/2. Call Close to stop
// the sweeper.
func New(ttl time.Duration) *Registry {
	r := &Registry{
		entries: map[string]Entry{},
		ttl:     ttl,
		stop:    make(chan struct{}),
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
	target = normalize(target)
	if target == "" {
		return
	}
	r.mu.Lock()
	r.entries[target] = Entry{Target: target, Reason: reason, Time: time.Now()}
	r.mu.Unlock()
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
	if r.ttl > 0 && time.Since(e.Time) > r.ttl {
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

// normalize lowercases hosts; cleans + absolutizes paths. A target
// containing a path separator (or starting with / or ~) is treated as
// a path; everything else as a host.
func normalize(target string) string {
	target = strings.TrimSpace(target)
	if target == "" {
		return ""
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
	now := time.Now()
	for k, e := range r.entries {
		if r.ttl > 0 && now.Sub(e.Time) > r.ttl {
			delete(r.entries, k)
		}
	}
}
