package netproxy

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
)

// countingPrompter counts interactive prompts and always answers
// "allow once" (Persist:false, so it is never cached as a learned rule).
type countingPrompter struct{ n atomic.Int32 }

func (p *countingPrompter) Prompt(host string, port int) PromptResult {
	p.n.Add(1)
	return PromptResult{Allow: true, Persist: false}
}

// TestDirectPathRunsFilterOnce proves the filter decision pipeline
// (DNS resolve + interactive prompt + audit log via f.log) runs exactly
// ONCE per client connection on the direct dial path.
//
// The PR #112 refactor has the server call filter.Check (discarding the
// resolved addrs) and then directDialer.DialTunnel call filter.Check a
// second time to obtain them. Because Check is not side-effect free, on
// the default (direct) path that means: double DNS resolution, double
// audit log lines, and — for an "allow once" verdict — a double prompt.
func TestDirectPathRunsFilterOnce(t *testing.T) {
	// Real upstream so the CONNECT tunnel establishes end to end.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port

	var resolves, logLines atomic.Int32
	prompter := &countingPrompter{}
	s := startProxy(t, FilterConfig{
		PromptEnabled: true,
		Prompter:      prompter,
		Resolve: func(_ context.Context, _ string) ([]netip.Addr, error) {
			resolves.Add(1)
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		},
		Logf: func(string, ...any) { logLines.Add(1) },
	})

	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", s.Port()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	auth := basicAuth("omac", s.Token())
	// Non-loopback hostname (loopback CONNECTs are refused before the filter).
	fmt.Fprintf(conn, "CONNECT app.internal:%d HTTP/1.1\r\nHost: app.internal:%d\r\nProxy-Authorization: %s\r\n\r\n", port, port, auth)

	br := bufio.NewReader(conn)
	status, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read status line: %v", err)
	}
	if !strings.Contains(status, "200") {
		t.Fatalf("expected 200 Connection Established, got %q", status)
	}

	if got := resolves.Load(); got != 1 {
		t.Errorf("DNS resolve ran %d times, want 1 (>1 == duplicated Filter.Check on the direct path)", got)
	}
	if got := prompter.n.Load(); got != 1 {
		t.Errorf("interactive prompt shown %d times, want 1 (>1 == allow-once double-prompt regression)", got)
	}
	if got := logLines.Load(); got != 1 {
		t.Errorf("audit log wrote %d decision lines, want 1 (>1 == duplicated audit entries)", got)
	}
}
