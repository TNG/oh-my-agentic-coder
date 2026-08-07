package netprompt

import (
	"fmt"
	"strings"
	"sync"
)

// sessionEntry is one session-scoped decision.
type sessionEntry struct {
	Host     string
	Scope    string // "host" | "suffix"
	Decision string // "allow" | "deny"
}

// SessionStore is a thread-safe, in-memory store of per-session network
// decisions. It mirrors LearnedPolicy's matching semantics but writes
// nothing to disk: everything dies with the process.
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
	s.mu.RLock()
	defer s.mu.RUnlock()
	h := strings.ToLower(host)
	var match *sessionEntry
	for i := range s.entries {
		e := &s.entries[i]
		if !sessionEntryMatches(e, h) {
			continue
		}
		if e.Decision == "deny" {
			return false, true // deny wins immediately
		}
		match = e
	}
	if match != nil {
		return match.Decision == "allow", true
	}
	return false, false
}

func sessionEntryMatches(e *sessionEntry, host string) bool {
	target := strings.ToLower(e.Host)
	if e.Scope == "suffix" {
		return host == target || strings.HasSuffix(host, "."+target)
	}
	return host == target
}

// Record upserts a decision keyed by host+scope.
func (s *SessionStore) Record(host, scope string, allow bool) error {
	if scope != "host" && scope != "suffix" {
		return fmt.Errorf("invalid session scope %q", scope)
	}
	decision := "deny"
	if allow {
		decision = "allow"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	h := strings.ToLower(host)
	for i := range s.entries {
		if strings.ToLower(s.entries[i].Host) == h && s.entries[i].Scope == scope {
			s.entries[i].Decision = decision
			return nil
		}
	}
	s.entries = append(s.entries, sessionEntry{Host: h, Scope: scope, Decision: decision})
	return nil
}
