package netproxy

import (
	"fmt"
	"strings"
	"sync"
)

// sessionEntry is one session-scoped decision.
type sessionEntry struct {
	host    string
	scope   string // "host" | "suffix"
	allowed bool
}

// NormalizeHost is the canonical lookup key for a hostname: lowercased
// with the root-zone trailing dot stripped. Check applies it once per
// request before consulting any rule, and both decision stores apply it
// to entries on the way in, so matching never has to normalize again.
func NormalizeHost(host string) string {
	return strings.ToLower(strings.TrimSuffix(host, "."))
}

// MatchHostRule reports whether host is covered by a rule recorded for
// target under scope ("host" matches only target itself, "suffix" also
// matches any subdomain). Both stores of prompt decisions — the learned
// policy file and the in-memory session store — share this one matcher
// so their reach cannot drift apart.
//
// Both arguments must already be NormalizeHost'd: this runs once per
// stored entry per request, so it does no normalization of its own.
func MatchHostRule(host, target, scope string) bool {
	return matchHostOrSuffix(host, target, scope == "suffix")
}

// matchHostOrSuffix is the one definition of what a domain rule covers:
// an exact host, or — for a wildcard/suffix rule — the target itself and
// any subdomain. Compares the dot boundary by index rather than building
// "."+target, which would allocate per entry per request.
func matchHostOrSuffix(host, target string, wildcard bool) bool {
	if host == target {
		return true
	}
	if !wildcard {
		return false
	}
	return len(host) > len(target)+1 &&
		host[len(host)-len(target)-1] == '.' &&
		host[len(host)-len(target):] == target
}

// SessionStore is a thread-safe, in-memory store of network decisions
// scoped to one sandbox session. It shares the learned store's semantics
// — deny wins, suffix entries cover subdomains — but writes nothing to
// disk: everything dies with the process.
type SessionStore struct {
	mu      sync.RWMutex
	entries []sessionEntry
}

// NewSessionStore returns an empty session store.
func NewSessionStore() *SessionStore {
	return &SessionStore{}
}

// Lookup reports a decision for host. Deny entries win over allow
// entries; suffix entries match the host itself and any subdomain.
func (s *SessionStore) Lookup(host string) (allow bool, found bool) {
	h := NormalizeHost(host)
	s.mu.RLock()
	defer s.mu.RUnlock()
	for i := range s.entries {
		e := &s.entries[i]
		if !MatchHostRule(h, e.host, e.scope) {
			continue
		}
		if !e.allowed {
			return false, true // deny wins immediately
		}
		found = true
	}
	return found, found
}

// Record upserts a decision keyed by host+scope.
func (s *SessionStore) Record(host, scope string, allow bool) error {
	if scope != "host" && scope != "suffix" {
		return fmt.Errorf("invalid session scope %q", scope)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	h := NormalizeHost(host)
	for i := range s.entries {
		if s.entries[i].host == h && s.entries[i].scope == scope {
			s.entries[i].allowed = allow
			return nil
		}
	}
	s.entries = append(s.entries, sessionEntry{host: h, scope: scope, allowed: allow})
	return nil
}
