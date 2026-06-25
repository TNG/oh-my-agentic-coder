package netproxy

import (
	"context"
	"fmt"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
)

// staticResolver returns fixed addresses for every host.
func staticResolver(ips ...string) func(context.Context, string) ([]netip.Addr, error) {
	var addrs []netip.Addr
	for _, s := range ips {
		addrs = append(addrs, netip.MustParseAddr(s))
	}
	return func(context.Context, string) ([]netip.Addr, error) {
		return addrs, nil
	}
}

type fakeLearned struct {
	mu      sync.Mutex
	allows  map[string]bool // host/suffix -> allow
	denies  map[string]bool
	records []string
}

func (f *fakeLearned) Lookup(host string) (bool, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	check := func(m map[string]bool) bool {
		if m[host] {
			return true
		}
		for suffix := range m {
			if len(host) > len(suffix)+1 && host[len(host)-len(suffix)-1] == '.' && host[len(host)-len(suffix):] == suffix {
				return true
			}
		}
		return false
	}
	if check(f.denies) {
		return false, true
	}
	if check(f.allows) {
		return true, true
	}
	return false, false
}

func (f *fakeLearned) Record(host, scope string, allow bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records = append(f.records, fmt.Sprintf("%s/%s/%v", host, scope, allow))
	return nil
}

type fakePrompter struct {
	mu    sync.Mutex
	calls int
	res   PromptResult
}

func (p *fakePrompter) Prompt(host string, port int) PromptResult {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	return p.res
}

func check(t *testing.T, f *Filter, host string, want Decision) Verdict {
	t.Helper()
	v, _ := f.Check(context.Background(), host, 443)
	if v.Decision != want {
		t.Errorf("Check(%s) = %v (%s), want %v", host, v.Decision, v.Reason, want)
	}
	return v
}

func TestHardDenyMetadata(t *testing.T) {
	p := &fakePrompter{res: PromptResult{Allow: true}}
	f := NewFilter(FilterConfig{
		PromptEnabled: true,
		Prompter:      p,
		Resolve:       staticResolver("93.184.216.34"),
	})
	for _, h := range []string{"169.254.169.254", "metadata.google.internal", "metadata.azure.internal"} {
		check(t, f, h, Deny)
	}
	if p.calls != 0 {
		t.Error("hard denies must never prompt")
	}
}

func TestHardDenyLinkLocalResolved(t *testing.T) {
	f := NewFilter(FilterConfig{
		AllowDomains: []string{"evil.example"},
		Resolve:      staticResolver("169.254.10.10"),
	})
	check(t, f, "evil.example", Deny)

	// IPv4-mapped IPv6 form.
	f2 := NewFilter(FilterConfig{
		AllowDomains: []string{"evil.example"},
		Resolve:      staticResolver("::ffff:169.254.10.10"),
	})
	check(t, f2, "evil.example", Deny)

	// fe80::
	f3 := NewFilter(FilterConfig{
		AllowDomains: []string{"evil.example"},
		Resolve:      staticResolver("fe80::1"),
	})
	check(t, f3, "evil.example", Deny)
}

func TestAllowlistMode(t *testing.T) {
	f := NewFilter(FilterConfig{
		AllowDomains: []string{"github.com", "*.npmjs.org"},
		Resolve:      staticResolver("93.184.216.34"),
	})
	check(t, f, "github.com", Allow)
	check(t, f, "GITHUB.COM", Allow) // case-insensitive
	check(t, f, "registry.npmjs.org", Allow)
	check(t, f, "npmjs.org", Allow) // wildcard matches suffix itself
	check(t, f, "evil.com", Deny)
	check(t, f, "notgithub.com", Deny)
	check(t, f, "github.com.evil.com", Deny)
}

func TestBlocklistMode(t *testing.T) {
	f := NewFilter(FilterConfig{
		DenyDomains: []string{"*.facebook.com"},
		Resolve:     staticResolver("93.184.216.34"),
	})
	check(t, f, "api.facebook.com", Deny)
	check(t, f, "facebook.com", Deny)
	check(t, f, "github.com", Allow) // pure blocklist: default allow
}

func TestDenyBeatsAllow(t *testing.T) {
	f := NewFilter(FilterConfig{
		AllowDomains: []string{"tracker.example"},
		DenyDomains:  []string{"tracker.example"},
		Resolve:      staticResolver("93.184.216.34"),
	})
	check(t, f, "tracker.example", Deny)
}

func TestLearnedDenyOverridesAllowlist(t *testing.T) {
	learned := &fakeLearned{denies: map[string]bool{"evil.example": true}}
	f := NewFilter(FilterConfig{
		AllowDomains: []string{"evil.example"},
		Learned:      learned,
		Resolve:      staticResolver("93.184.216.34"),
	})
	check(t, f, "evil.example", Deny)
}

func TestLearnedAllowApplies(t *testing.T) {
	learned := &fakeLearned{allows: map[string]bool{"npmjs.org": true}}
	f := NewFilter(FilterConfig{
		AllowDomains: []string{"github.com"}, // non-empty allowlist, no prompt
		Learned:      learned,
		Resolve:      staticResolver("93.184.216.34"),
	})
	check(t, f, "registry.npmjs.org", Allow) // suffix-scope learned allow
	check(t, f, "other.example", Deny)
}

func TestPromptFlow(t *testing.T) {
	p := &fakePrompter{res: PromptResult{Allow: true}}
	f := NewFilter(FilterConfig{
		PromptEnabled: true,
		Prompter:      p,
		Resolve:       staticResolver("93.184.216.34"),
	})
	check(t, f, "api.example.com", Allow)
	if p.calls != 1 {
		t.Errorf("calls = %d", p.calls)
	}
	// Allow-once does not persist: second request prompts again.
	check(t, f, "api.example.com", Allow)
	if p.calls != 2 {
		t.Errorf("allow once must re-prompt, calls = %d", p.calls)
	}
}

func TestPromptPermanentPersists(t *testing.T) {
	learned := &fakeLearned{}
	p := &fakePrompter{res: PromptResult{Allow: true, Persist: true, Scope: "suffix", Suffix: "npmjs.org"}}
	f := NewFilter(FilterConfig{
		PromptEnabled: true,
		Prompter:      p,
		Learned:       learned,
		Resolve:       staticResolver("93.184.216.34"),
	})
	check(t, f, "registry.npmjs.org", Allow)
	if len(learned.records) != 1 || learned.records[0] != "npmjs.org/suffix/true" {
		t.Errorf("records = %v", learned.records)
	}
}

func TestPromptUnavailableFallback(t *testing.T) {
	// Prompt enabled, no prompter, on_unavailable=deny (default).
	f := NewFilter(FilterConfig{
		PromptEnabled: true,
		Resolve:       staticResolver("93.184.216.34"),
	})
	check(t, f, "api.example.com", Deny)

	// on_unavailable=allow.
	f2 := NewFilter(FilterConfig{
		PromptEnabled:      true,
		OnUnavailableAllow: true,
		Resolve:            staticResolver("93.184.216.34"),
	})
	check(t, f2, "api.example.com", Allow)
}

func TestPromptCoalescing(t *testing.T) {
	const n = 5
	block := make(chan struct{})
	p := &blockingPrompter{block: block, res: PromptResult{Allow: true}}
	// coalesced counts followers that have committed to waiting on the
	// leader's in-flight prompt. Exactly one of the n goroutines becomes
	// the leader (registers inflight, calls Prompt); the other n-1 take
	// the coalescing path and bump this. Waiting for n-1 before releasing
	// the leader makes the test deterministic: every follower has parked
	// on the shared prompt — and so cannot start its own — by the time
	// the leader's Prompt is allowed to return. (The old version released
	// after merely started>=1, so a late follower could arrive after the
	// leader cleared inflight and prompt a second time. Hence the flake.)
	var coalesced atomic.Int32
	f := NewFilter(FilterConfig{
		PromptEnabled:  true,
		Prompter:       p,
		Resolve:        staticResolver("93.184.216.34"),
		onCoalesceWait: func() { coalesced.Add(1) },
	})
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, _ := f.Check(context.Background(), "same.example.com", 443)
			if v.Decision != Allow {
				t.Errorf("got %v", v)
			}
		}()
	}
	// All followers have parked on the single in-flight prompt.
	waitUntil(t, func() bool { return coalesced.Load() == n-1 })
	close(block)
	wg.Wait()
	if got := p.started.Load(); got != 1 {
		t.Errorf("prompter called %d times; want 1 (coalesced)", got)
	}
}

func TestDNSPinning(t *testing.T) {
	f := NewFilter(FilterConfig{
		AllowDomains: []string{"pinned.example"},
		Resolve:      staticResolver("93.184.216.34", "93.184.216.35"),
	})
	v, addrs := f.Check(context.Background(), "pinned.example", 443)
	if v.Decision != Allow {
		t.Fatalf("verdict %v", v)
	}
	if len(addrs) != 2 || addrs[0].String() != "93.184.216.34" {
		t.Errorf("pinned addrs = %v", addrs)
	}
}

func TestDNSFailureDenies(t *testing.T) {
	f := NewFilter(FilterConfig{
		AllowDomains: []string{"broken.example"},
		Resolve: func(context.Context, string) ([]netip.Addr, error) {
			return nil, fmt.Errorf("NXDOMAIN")
		},
	})
	check(t, f, "broken.example", Deny)
}

func TestIPLiteralTargets(t *testing.T) {
	f := NewFilter(FilterConfig{
		AllowDomains: []string{"8.8.8.8"},
		Resolve:      staticResolver(), // must not be consulted for literals
	})
	check(t, f, "8.8.8.8", Allow)
	check(t, f, "9.9.9.9", Deny)
}
