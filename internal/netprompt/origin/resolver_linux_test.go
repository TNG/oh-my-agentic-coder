//go:build linux

package origin

import (
	"net"
	"net/netip"
	"os"
	"strings"
	"testing"
)

// TestResolveRealSocket dials a loopback listener within this process and
// asserts the resolver attributes the connection to this very PID via the full
// 4-tuple — a real-socket check that a /proc/net/tcp fixture cannot give.
func TestResolveRealSocket(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	server, err := ln.Accept()
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	defer server.Close()

	// From the proxy's vantage the accepted conn.RemoteAddr is the client's
	// source; conn.LocalAddr is the proxy endpoint.
	src := server.RemoteAddr().(*net.TCPAddr).AddrPort()
	proxy := server.LocalAddr().(*net.TCPAddr).AddrPort()

	o, ok := procResolver{}.Resolve(src, proxy)
	if !ok {
		t.Fatal("expected to resolve the loopback connection")
	}
	if o.PID != os.Getpid() {
		t.Errorf("PID = %d, want this process %d", o.PID, os.Getpid())
	}
	// Name is comm — verify it matches /proc/self/comm (proving we read comm,
	// not cmdline).
	want, _ := os.ReadFile("/proc/self/comm")
	if o.Name != strings.TrimSpace(string(want)) {
		t.Errorf("Name = %q, want comm %q", o.Name, strings.TrimSpace(string(want)))
	}
}

func TestResolveUnknownTupleMisses(t *testing.T) {
	// A src/proxy pair that is not an open connection must not resolve.
	src := netip.MustParseAddrPort("127.0.0.1:1")
	proxy := netip.MustParseAddrPort("127.0.0.1:2")
	if _, ok := (procResolver{}).Resolve(src, proxy); ok {
		t.Error("nonexistent tuple should miss")
	}
}

func TestHexEndpoint(t *testing.T) {
	got, ok := hexEndpoint(netip.MustParseAddrPort("127.0.0.1:443"))
	if !ok || got != "0100007F:01BB" {
		t.Errorf("hexEndpoint(127.0.0.1:443) = %q,%v; want 0100007F:01BB,true", got, ok)
	}
	if _, ok := hexEndpoint(netip.MustParseAddrPort("[::1]:443")); ok {
		t.Error("IPv6 should report ok=false (loopback proxy traffic is IPv4)")
	}
}
