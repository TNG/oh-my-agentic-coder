package netprompt

import (
	"context"
	"net/netip"
	"strings"
	"testing"

	"github.com/tngtech/oh-my-agentic-coder/internal/netprompt/origin"
	"github.com/tngtech/oh-my-agentic-coder/internal/netproxy"
)

type fakeResolver struct {
	o  origin.Origin
	ok bool
}

func (f fakeResolver) Resolve(_, _ netip.AddrPort) (origin.Origin, bool) { return f.o, f.ok }

func ctxWithSource(t *testing.T) context.Context {
	t.Helper()
	src := netip.MustParseAddrPort("127.0.0.1:54321")
	proxy := netip.MustParseAddrPort("127.0.0.1:443")
	return netproxy.WithClientSource(context.Background(), src, proxy)
}

func TestResolveOriginRendersLine(t *testing.T) {
	p := &Prompter{origin: fakeResolver{o: origin.Origin{PID: 4242, Name: "opencode"}, ok: true}}
	got := p.resolveOrigin(ctxWithSource(t))
	if got != "opencode (host pid 4242)" {
		t.Errorf("origin line = %q", got)
	}
}

func TestResolveOriginOmittedWhenNoResolver(t *testing.T) {
	p := &Prompter{}
	if got := p.resolveOrigin(ctxWithSource(t)); got != "" {
		t.Errorf("no resolver should omit, got %q", got)
	}
}

func TestResolveOriginOmittedWhenNoClientSource(t *testing.T) {
	p := &Prompter{origin: fakeResolver{o: origin.Origin{PID: 1, Name: "x"}, ok: true}}
	if got := p.resolveOrigin(context.Background()); got != "" {
		t.Errorf("no ClientSource should omit, got %q", got)
	}
}

func TestResolveOriginOmittedWhenResolveFails(t *testing.T) {
	p := &Prompter{origin: fakeResolver{ok: false}}
	if got := p.resolveOrigin(ctxWithSource(t)); got != "" {
		t.Errorf("failed resolve should omit, got %q", got)
	}
}

func TestPromptTextOriginOrdering(t *testing.T) {
	got := promptText("raw.githubusercontent.com", 443, "", "grammar", "opencode (host pid 42)")
	if !strings.Contains(got, "Origin: opencode (host pid 42)\n") {
		t.Errorf("missing origin line: %q", got)
	}
	// Origin precedes cause precedes intent.
	oi, ci, ii := strings.Index(got, "Origin:"), strings.Index(got, "Likely cause:"), strings.Index(got, "Agent intent:")
	if !(oi < ci && ci < ii) {
		t.Errorf("expected Origin < cause < intent, got %d %d %d in %q", oi, ci, ii, got)
	}
}

func TestPromptTextOriginOmittedWhenEmpty(t *testing.T) {
	if strings.Contains(promptText("api.example.com", 443, "", "", ""), "Origin:") {
		t.Error("empty origin must omit the line")
	}
}
