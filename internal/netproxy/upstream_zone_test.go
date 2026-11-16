package netproxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The upstream parses real CONNECTs, so raw %zones cannot pass unnoticed as
// they do with a transport sentinel or a fixture that only splits the line.
func TestUpstreamZonedAddressWire(t *testing.T) {
	for _, ip := range []string{
		"64:ff9b::808:808", // Unzoned transport control.
		"64:ff9b::808:808%0",
		"64:ff9b:1::808:808%lo0",
		"2606:4700::1111%0",
	} {
		for _, dns := range []bool{false, true} {
			for _, forward := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/dns=%v/forward=%v", ip, dns, forward), func(t *testing.T) {
					ln, err := net.Listen("tcp", "127.0.0.1:0")
					if err != nil {
						t.Fatal(err)
					}
					defer ln.Close()
					seen := make(chan string, 1)
					go func() {
						c, err := ln.Accept()
						if err != nil {
							seen <- err.Error()
							return
						}
						defer c.Close()
						c.SetDeadline(time.Now().Add(5 * time.Second))
						br := bufio.NewReader(c)
						r, err := http.ReadRequest(br)
						if err != nil {
							seen <- err.Error()
							return
						}
						if r.Method != http.MethodConnect || r.URL.Port() != "80" {
							seen <- fmt.Sprintf("unexpected CONNECT: %s %s", r.Method, r.RequestURI)
							return
						}
						seen <- r.URL.Hostname()
						io.WriteString(c, "HTTP/1.1 200 Connection Established\r\n\r\n")
						if forward {
							if _, err := http.ReadRequest(br); err == nil {
								io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 11\r\nConnection: close\r\n\r\npinned-zone")
							}
						} else {
							io.Copy(c, br)
						}
					}()
					var resolves atomic.Int32
					u := &url.URL{Scheme: "http", Host: ln.Addr().String()}
					s := startProxyWithDialer(t, FilterConfig{
						ResolveOnCheckHost: true,
						Resolve: func(context.Context, string) ([]netip.Addr, error) {
							if resolves.Add(1) > 1 {
								return pin("169.254.169.254"), nil
							}
							return pin(ip), nil
						},
					}, NewUpstreamProxyDialer(u, nil, t.Logf))
					c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", s.Port()))
					if err != nil {
						t.Fatal(err)
					}
					defer c.Close()
					c.SetDeadline(time.Now().Add(5 * time.Second))
					host := ip
					if dns {
						host = "dns64.example"
					}
					target := net.JoinHostPort(host, "80")
					start := "CONNECT " + strings.ReplaceAll(target, "%", "%25")
					if forward {
						start = "GET " + (&url.URL{Scheme: "http", Host: target, Path: "/"}).String()
					}
					fmt.Fprintf(c, "%s HTTP/1.1\r\nHost: %s\r\nProxy-Authorization: %s\r\n\r\n", start, target, basicAuth("omac", s.Token()))
					br := bufio.NewReader(c)
					resp, err := http.ReadResponse(br, nil)
					if err != nil {
						t.Fatal(err)
					}
					defer resp.Body.Close()
					select {
					case got := <-seen:
						if resp.StatusCode != http.StatusOK || got != ip {
							t.Fatalf("HTTP %d, upstream target=%q, want original %q", resp.StatusCode, got, ip)
						}
					case <-time.After(5 * time.Second):
						t.Fatalf("HTTP %d, upstream not reached", resp.StatusCode)
					}
					var body []byte
					if forward {
						body, err = io.ReadAll(resp.Body)
					} else {
						io.WriteString(c, "pinned-zone")
						body = make([]byte, len("pinned-zone"))
						_, err = io.ReadFull(br, body)
					}
					if err != nil || string(body) != "pinned-zone" {
						t.Fatalf("tunnel payload=%q, error=%v", body, err)
					}
					wantResolves := int32(0)
					if dns {
						wantResolves = 1
					}
					if resolves.Load() != wantResolves {
						t.Fatalf("resolved %d times, want %d", resolves.Load(), wantResolves)
					}
				})
			}
		}
	}
}
