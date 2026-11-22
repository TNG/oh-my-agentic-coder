package facade

import (
	"crypto/sha256"
	"net/http"
	"sync"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/mcp/mcppolicy"
)

const (
	mcpAllowanceTTL        = 15 * time.Second
	mcpAllowanceMaxEntries = 256
)

// mcpAllowanceKey identifies one logical tool call: the bridge retries a
// failed attempt with the same session, id, tool and bytes.
type mcpAllowanceKey struct {
	route, session, id, tool string
	digest                   [sha256.Size]byte
}

// mcpAllowanceKeyFor returns the zero key for a call without an id, which
// is never remembered.
func mcpAllowanceKeyFor(r *http.Request, route *Route, call mcppolicy.Call, canonical []byte) mcpAllowanceKey {
	if len(call.ID) == 0 {
		return mcpAllowanceKey{}
	}
	return mcpAllowanceKey{
		route: route.key(), session: r.Header.Get("Mcp-Session-Id"), id: string(call.ID), tool: call.Tool,
		digest: sha256.Sum256(canonical),
	}
}

// mcpAllowanceCache holds allowed tool calls for a short time so that
// transport retries of one call ask the user once. The zero value is ready.
// An allowance is not consumed by use. Concurrent identical calls that
// arrive before the first answer each ask; every allowance still comes from
// an approver answer.
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

func (c *mcpAllowanceCache) has(k mcpAllowanceKey) bool {
	if k == (mcpAllowanceKey{}) {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	exp, ok := c.until[k]
	return ok && c.clock().Before(exp)
}

func (c *mcpAllowanceCache) remember(k mcpAllowanceKey) {
	if k == (mcpAllowanceKey{}) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.until == nil {
		c.until = map[mcpAllowanceKey]time.Time{}
	}
	now := c.clock()
	if _, ok := c.until[k]; !ok && len(c.until) >= c.capacity() {
		c.evict(now)
	}
	c.until[k] = now.Add(c.lifetime())
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
