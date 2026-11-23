package facade

import (
	"crypto/sha256"
	"sync"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/mcp/mcppolicy"
)

const (
	mcpAllowanceTTL        = 15 * time.Second
	mcpAllowanceMaxEntries = 256
)

// mcpAllowanceKey identifies one logical tool call: the bridge retries a
// failed attempt with the same id, tool and bytes. The session id is not part
// of it, because after a gateway restart the bridge initializes a new session
// and then retries the call.
type mcpAllowanceKey struct {
	route, id, tool string
	digest          [sha256.Size]byte
}

// mcpAllowanceKeyFor returns the zero key for a call without an id, which
// is never remembered.
func mcpAllowanceKeyFor(route *Route, call mcppolicy.Call, canonical []byte) mcpAllowanceKey {
	if len(call.ID) == 0 {
		return mcpAllowanceKey{}
	}
	return mcpAllowanceKey{route: route.key(), id: string(call.ID), tool: call.Tool, digest: sha256.Sum256(canonical)}
}

// mcpAllowanceCache holds allowed tool calls for a short time so that
// transport retries of one call ask the user once. The zero value is ready.
// An allowance exists only while no attempt of the call is in flight: an
// approved call stores one only when its forward fails, and a retry takes it
// out and gives it back only when its own forward fails. So one approver
// answer lets at most one attempt reach the server at a time, and a call that
// went through needs a new answer.
type mcpAllowanceCache struct {
	mu      sync.Mutex
	until   map[mcpAllowanceKey]time.Time
	ttl     time.Duration
	now     func() time.Time
	maxSize int
}

func (c *mcpAllowanceCache) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

func (c *mcpAllowanceCache) lifetime() time.Duration {
	if c.ttl > 0 {
		return c.ttl
	}
	return mcpAllowanceTTL
}

func (c *mcpAllowanceCache) capacity() int {
	if c.maxSize > 0 {
		return c.maxSize
	}
	return mcpAllowanceMaxEntries
}

// take removes the allowance for k and returns its expiry, so exactly one
// request can use it. The caller gives it back with restore when its forward
// fails.
func (c *mcpAllowanceCache) take(k mcpAllowanceKey) (time.Time, bool) {
	if k == (mcpAllowanceKey{}) {
		return time.Time{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	exp, ok := c.until[k]
	if !ok {
		return time.Time{}, false
	}
	delete(c.until, k)
	return exp, c.clock().Before(exp)
}

// restore stores an allowance for k that expires at exp, or one full
// lifetime from now when exp is zero (a fresh approver answer). An
// allowance that has already expired is dropped.
func (c *mcpAllowanceCache) restore(k mcpAllowanceKey, exp time.Time) {
	if k == (mcpAllowanceKey{}) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.until == nil {
		c.until = map[mcpAllowanceKey]time.Time{}
	}
	now := c.clock()
	if exp.IsZero() {
		exp = now.Add(c.lifetime())
	}
	if !now.Before(exp) {
		return
	}
	if _, ok := c.until[k]; !ok && len(c.until) >= c.capacity() {
		c.evict(now)
	}
	c.until[k] = exp
}

// evict drops the expired entries, and the oldest one when none had expired.
func (c *mcpAllowanceCache) evict(now time.Time) {
	var oldest mcpAllowanceKey
	var oldestExp time.Time
	dropped := false
	for k, exp := range c.until {
		if !now.Before(exp) {
			delete(c.until, k)
			dropped = true
			continue
		}
		if oldestExp.IsZero() || exp.Before(oldestExp) {
			oldest, oldestExp = k, exp
		}
	}
	if !dropped && !oldestExp.IsZero() {
		delete(c.until, oldest)
	}
}
