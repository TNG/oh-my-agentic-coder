package facade

// In-memory SSE test that does not need kernel socket support.
//
// Pipes a client HTTP connection (via net.Pipe) straight into the facade's
// http.Server.Handler. This proves the facade's streaming+flush path end
// to end without touching the OS network stack — useful in restricted
// execution environments where unix/loopback connect is denied.

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestFacadeSSE_InMemory exercises the SSE code path end to end without
// requiring any socket capabilities.
func TestFacadeSSE_InMemory(t *testing.T) {
	// 1. Upstream: SSE emitter with per-frame flush.
	upstream := &sseHTTPServer{framesSent: new(int32)}
	upstreamPort := upstream.startLoopback(t)
	if upstreamPort == 0 {
		t.Skip("loopback listen denied in this environment")
	}
	t.Cleanup(upstream.stop)

	// 2. Facade pointed at the upstream.
	f := New("", // socket path unused; we don't Start() the facade
		[]Route{{Mount: "echo", UpstreamPort: upstreamPort, Skill: "echo-rest"}},
		1<<20, 30*time.Second, "", "test")
	// Build the handler manually (same wiring as Facade.Start, minus the listener).
	handler := http.HandlerFunc(f.handle)

	// 3. net.Pipe gives us client/server conn halves. We run an
	//    http.Server on the server half and use an http.Client on the
	//    client half. No sockets, no OS networking.
	clientSide, serverSide := net.Pipe()
	srv := &http.Server{Handler: handler}
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		srv.Serve(singleConnListener{conn: serverSide})
	}()
	t.Cleanup(func() {
		srv.Close()
		<-serverDone
	})

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return clientSide, nil
			},
			// Allow long bodies; headers must come fast.
			ResponseHeaderTimeout: 2 * time.Second,
			DisableKeepAlives:     true,
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://x/echo/tick", nil)
	req.Header.Set("Accept", "text/event-stream")
	tStart := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %q", ct)
	}
	if ab := resp.Header.Get("X-Accel-Buffering"); ab != "no" {
		t.Fatalf("X-Accel-Buffering = %q, want no", ab)
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 4096), 1<<20)
	var (
		tickCount   int
		doneSeen    bool
		firstTickAt time.Time
		lastTickAt  time.Time
		currEvent   string
	)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			currEvent = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			switch currEvent {
			case "tick":
				tickCount++
				now := time.Now()
				if firstTickAt.IsZero() {
					firstTickAt = now
				}
				lastTickAt = now
			case "done":
				doneSeen = true
			}
		case line == "":
			currEvent = ""
		}
		if doneSeen {
			break
		}
	}
	resp.Body.Close()
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if tickCount != 5 {
		t.Fatalf("tick count = %d, want 5", tickCount)
	}
	if !doneSeen {
		t.Fatalf("done event missing")
	}
	span := lastTickAt.Sub(firstTickAt)
	total := lastTickAt.Sub(tStart)
	if span < 60*time.Millisecond {
		t.Fatalf("frames too close together (span=%s total=%s); facade may be buffering", span, total)
	}
	if atomic.LoadInt32(upstream.framesSent) != 6 {
		// 5 ticks + 1 done event.
		t.Fatalf("upstream frames sent = %d, want 6", atomic.LoadInt32(upstream.framesSent))
	}
}

// singleConnListener is a net.Listener that returns one preset connection
// and then blocks. It lets us drive an http.Server over a net.Pipe.
type singleConnListener struct {
	conn     net.Conn
	returned int32
}

func (l singleConnListener) Accept() (net.Conn, error) {
	if atomic.CompareAndSwapInt32(&l.returned, 0, 1) {
		return l.conn, nil
	}
	// Block forever so Serve() stays alive until Server.Close is called.
	c := make(chan struct{})
	<-c
	return nil, io.EOF
}

func (l singleConnListener) Close() error { return l.conn.Close() }
func (l singleConnListener) Addr() net.Addr {
	return &net.UnixAddr{Name: "inproc", Net: "unix"}
}

// sseHTTPServer is an SSE upstream bound to loopback. We try loopback
// here because the facade's reverse proxy needs a real http.Transport to
// dial the upstream; it can't use net.Pipe on that side. The skip check
// above handles the case where loopback is denied.
type sseHTTPServer struct {
	ln         net.Listener
	srv        *http.Server
	framesSent *int32
}

func (s *sseHTTPServer) startLoopback(t *testing.T) int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0
	}
	// Probe: can we dial it ourselves? If not, skip.
	c, err := net.DialTimeout("tcp", ln.Addr().String(), 200*time.Millisecond)
	if err != nil {
		ln.Close()
		return 0
	}
	c.Close()

	s.ln = ln
	mux := http.NewServeMux()
	mux.HandleFunc("/tick", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(200)
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Errorf("upstream ResponseWriter lacks Flusher")
			return
		}
		for i := 1; i <= 5; i++ {
			fmt.Fprintf(w, "event: tick\nid: %d\ndata: {\"n\":%d}\n\n", i, i)
			fl.Flush()
			atomic.AddInt32(s.framesSent, 1)
			time.Sleep(30 * time.Millisecond)
		}
		fmt.Fprint(w, "event: done\ndata: {}\n\n")
		fl.Flush()
		atomic.AddInt32(s.framesSent, 1)
	})
	s.srv = &http.Server{Handler: mux}
	go func() { _ = s.srv.Serve(ln) }()
	return ln.Addr().(*net.TCPAddr).Port
}

func (s *sseHTTPServer) stop() {
	if s.srv != nil {
		_ = s.srv.Close()
	}
	if s.ln != nil {
		_ = s.ln.Close()
	}
}
