//go:build vuln

package facade

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// The Upgrade path.
//
// Sidecar ports are bound on loopback and kept out of the sandbox's network
// grants: the facade is meant to be the only way in, which is what lets a
// sidecar hold a skill's API token in its environment. Everything the facade
// promises about that front door — the request body limit, speaking HTTP to
// the sidecar rather than arbitrary bytes — is enforced on the ordinary
// proxy path.
//
// A request carrying "Connection: upgrade" and any non-empty "Upgrade"
// header takes a different path instead, which dials the sidecar, forwards
// the request verbatim, hijacks the client connection and splices the two
// together. The switch is driven purely by two request headers the caller
// chooses, so a caller opts out of the ordinary path simply by asking to.
//
// These tests need real loopback listeners (the facade binds one, the fake
// sidecar another) and therefore cannot run in an environment that forbids
// binding. Run the suite on a normal host or in the e2e container.

// requireLoopbackListener fails loudly when the environment cannot bind, so
// a missing capability never reads as a passing security test.
func requireLoopbackListener(t *testing.T) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("cannot bind a loopback listener (%v): this test needs one; run the security suite outside a sandbox that forbids it", err)
	}
	ln.Close()
}

// fakeSidecar stands in for a skill's sidecar process: it records every byte
// the facade sends it and replies with a fixed, raw response.
type fakeSidecar struct {
	port  int
	reply string

	mu       sync.Mutex
	received bytes.Buffer
}

// startSidecar listens on loopback. reply is written to each accepted
// connection before reading begins; empty means answer nothing at all.
func startSidecar(t *testing.T, reply string) *fakeSidecar {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("sidecar listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	s := &fakeSidecar{port: ln.Addr().(*net.TCPAddr).Port, reply: reply}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				if s.reply != "" {
					_, _ = io.WriteString(c, s.reply)
				}
				_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
				buf := make([]byte, 4096)
				for {
					n, err := c.Read(buf)
					if n > 0 {
						s.mu.Lock()
						s.received.Write(buf[:n])
						s.mu.Unlock()
					}
					if err != nil {
						return
					}
				}
			}(c)
		}
	}()
	return s
}

func (s *fakeSidecar) saw() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.received.String()
}

// startFacade runs a facade on loopback TCP with one route to the sidecar.
func startFacade(t *testing.T, s *fakeSidecar, maxBody int64) int {
	t.Helper()
	f := New("", "127.0.0.1:0", []Route{{
		Mount:        "skill",
		UpstreamPort: s.port,
		State:        RouteReady,
	}}, maxBody, time.Minute, "", "test")
	if err := f.Start(context.Background()); err != nil {
		t.Fatalf("facade start: %v", err)
	}
	t.Cleanup(func() { f.Close() })
	return f.TCPPort()
}

// dialFacade opens a raw connection and writes req verbatim. Raw rather than
// net/http because these tests send requests a client library would refuse
// to construct.
func dialFacade(t *testing.T, port int, req string) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("dial facade: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	if _, err := io.WriteString(c, req); err != nil {
		t.Fatalf("write request: %v", err)
	}
	return c
}

// TestSecurityUpgradeProxyEnforcesBodyLimit asserts that the request body
// limit holds whether or not the caller asks for an upgrade.
//
// The limit is the facade's only backstop against a caller pushing unbounded
// data into a sidecar, and against the facade itself buffering it. If two
// request headers turn it off, it protects only callers who were not trying
// to get around it.
func TestSecurityUpgradeProxyEnforcesBodyLimit(t *testing.T) {
	requireLoopbackListener(t)

	const limit = 100
	const bodyLen = 64 * 1024
	body := strings.Repeat("A", bodyLen)

	request := func(extraHeaders string) string {
		return "POST /skill/ingest HTTP/1.1\r\n" +
			"Host: facade\r\n" +
			fmt.Sprintf("Content-Length: %d\r\n", bodyLen) +
			extraHeaders +
			"\r\n" + body
	}

	// Control: without the upgrade headers the limit holds. It is the same
	// request, the same body, the same route — the only difference below is
	// two headers, which is exactly the point being made.
	t.Run("plain request is limited", func(t *testing.T) {
		s := startSidecar(t, "HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n")
		port := startFacade(t, s, limit)
		c := dialFacade(t, port, request(""))
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, _ = io.ReadAll(c)
		time.Sleep(100 * time.Millisecond)

		if got := strings.Count(s.saw(), "A"); got > limit {
			t.Fatalf("the sidecar received %d body bytes through the ordinary path with a %d-byte limit: the limit is not enforced anywhere, so this test cannot show it being bypassed", got, limit)
		}
	})

	s := startSidecar(t, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
	port := startFacade(t, s, limit)
	c := dialFacade(t, port, request("Connection: upgrade\r\nUpgrade: websocket\r\n"))
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _ = io.ReadAll(c)
	time.Sleep(200 * time.Millisecond)

	if got := strings.Count(s.saw(), "A"); got > limit {
		t.Errorf("the sidecar received %d body bytes despite a %d-byte limit: adding \"Connection: upgrade\" and \"Upgrade: websocket\" to any request switches the limit off", got, limit)
	}
}

// TestSecurityUpgradeProxyRequiresSwitchingProtocols asserts that the facade
// only starts splicing raw bytes once the sidecar has actually agreed to an
// upgrade by answering 101.
//
// Without that check the facade is a plain TCP tunnel to a sidecar port on
// request: the caller asks for an upgrade, the sidecar declines or says
// nothing, and the bytes flow anyway. Sidecar ports are withheld from the
// sandbox precisely so that only well-formed HTTP reaches them through the
// facade, and any sidecar speaking a non-HTTP protocol is then addressable
// directly at the byte level.
func TestSecurityUpgradeProxyRequiresSwitchingProtocols(t *testing.T) {
	requireLoopbackListener(t)

	const probe = "PING\r\nnot-http\r\n"

	upgradeRequest := "GET /skill/socket HTTP/1.1\r\n" +
		"Host: facade\r\n" +
		"Connection: upgrade\r\n" +
		"Upgrade: websocket\r\n" +
		"\r\n"

	// Control: a sidecar that does agree to the upgrade still gets its bytes
	// spliced. Proves the harness can drive a working upgrade, so the
	// failure below is the missing check and not a broken fixture.
	t.Run("accepted upgrade still splices", func(t *testing.T) {
		s := startSidecar(t, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		port := startFacade(t, s, 0)
		c := dialFacade(t, port, upgradeRequest)
		time.Sleep(100 * time.Millisecond)
		if _, err := io.WriteString(c, probe); err != nil {
			t.Fatalf("write post-handshake bytes: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
		if !strings.Contains(s.saw(), probe) {
			t.Fatal("an accepted upgrade did not splice: the fixture cannot drive an upgrade at all, so the assertion below proves nothing")
		}
	})

	// The sidecar refuses the upgrade with an ordinary response.
	s := startSidecar(t, "HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n")
	port := startFacade(t, s, 0)
	c := dialFacade(t, port, upgradeRequest)
	time.Sleep(100 * time.Millisecond)
	if _, err := io.WriteString(c, probe); err != nil {
		t.Fatalf("write post-handshake bytes: %v", err)
	}
	time.Sleep(200 * time.Millisecond)

	if strings.Contains(s.saw(), probe) {
		t.Errorf("raw bytes %q reached the sidecar although it never answered 101: the facade is an on-demand TCP tunnel to a port the sandbox is not allowed to reach", probe)
	}
}
