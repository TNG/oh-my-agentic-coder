package facade

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

const (
	opText  = 0x1
	opPing  = 0x9
	opPong  = 0xA
	opClose = 0x8
)

func wsAccept(key string) string {
	sum := sha1.Sum([]byte(key + wsGUID))
	return base64.StdEncoding.EncodeToString(sum[:])
}

func writeFrame(w io.Writer, opcode byte, payload []byte, masked bool) error {
	hdr := []byte{0x80 | opcode}
	maskBit := byte(0)
	if masked {
		maskBit = 0x80
	}
	switch n := len(payload); {
	case n < 126:
		hdr = append(hdr, maskBit|byte(n))
	case n < 1<<16:
		hdr = append(hdr, maskBit|126, byte(n>>8), byte(n))
	default:
		hdr = append(hdr, maskBit|127)
		hdr = binary.BigEndian.AppendUint64(hdr, uint64(n))
	}
	body := payload
	if masked {
		key := []byte{1, 2, 3, 4}
		hdr = append(hdr, key...)
		body = make([]byte, len(payload))
		for i := range payload {
			body[i] = payload[i] ^ key[i%4]
		}
	}
	_, err := w.Write(append(hdr, body...))
	return err
}

func readFrame(r *bufio.Reader) (opcode byte, payload []byte, err error) {
	var h [2]byte
	if _, err = io.ReadFull(r, h[:]); err != nil {
		return
	}
	opcode = h[0] & 0x0f
	masked := h[1]&0x80 != 0
	n := uint64(h[1] & 0x7f)
	switch n {
	case 126:
		var b [2]byte
		if _, err = io.ReadFull(r, b[:]); err != nil {
			return
		}
		n = uint64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err = io.ReadFull(r, b[:]); err != nil {
			return
		}
		n = binary.BigEndian.Uint64(b[:])
	}
	var key [4]byte
	if masked {
		if _, err = io.ReadFull(r, key[:]); err != nil {
			return
		}
	}
	payload = make([]byte, n)
	if _, err = io.ReadFull(r, payload); err != nil {
		return
	}
	if masked {
		for i := range payload {
			payload[i] ^= key[i%4]
		}
	}
	return
}

type connEnd struct {
	at  time.Time
	err error
}

// wsUpstream answers the WebSocket handshake with 101 and then echoes text
// frames and answers pings. In raw mode it echoes bytes instead of frames.
type wsUpstream struct {
	port  int
	raw   bool
	ended chan connEnd
}

func startWSUpstream(t *testing.T, raw bool) *wsUpstream {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("loopback listen not possible: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	u := &wsUpstream{port: ln.Addr().(*net.TCPAddr).Port, raw: raw, ended: make(chan connEnd, 4)}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go u.serve(c)
		}
	}()
	return u
}

func (u *wsUpstream) serve(c net.Conn) {
	defer c.Close()
	br := bufio.NewReader(c)
	req, err := http.ReadRequest(br)
	if err != nil {
		u.ended <- connEnd{time.Now(), err}
		return
	}
	fmt.Fprintf(c, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n",
		wsAccept(req.Header.Get("Sec-WebSocket-Key")))
	var end error
	if u.raw {
		_, end = io.Copy(c, br)
	} else {
		for end == nil {
			var op byte
			var p []byte
			op, p, end = readFrame(br)
			if end != nil {
				break
			}
			switch op {
			case opText:
				end = writeFrame(c, opText, p, false)
			case opPing:
				end = writeFrame(c, opPong, p, false)
			case opClose:
				_ = writeFrame(c, opClose, p, false)
				end = errors.New("close frame")
			}
		}
	}
	if end == nil {
		end = io.EOF
	}
	u.ended <- connEnd{time.Now(), end}
}

type scenario struct {
	transport string // direct, tcp, unix
	idle      time.Duration
	behavior  string // none, message, ping, rawbyte
	window    time.Duration
	maxBody   int64
	routeIdle time.Duration
}

type outcome struct {
	alive       bool
	clientAfter time.Duration
	clientErr   string
	upAfter     time.Duration
	upErr       string
}

func (o outcome) String() string {
	if o.alive {
		return "alive at window end"
	}
	return fmt.Sprintf("client end %.1fs (%s), upstream end %.1fs (%s)",
		o.clientAfter.Seconds(), o.clientErr, o.upAfter.Seconds(), o.upErr)
}

func startFacadeFor(t *testing.T, s scenario, upPort int) (network, addr string) {
	t.Helper()
	routes := []Route{{Mount: "skill", UpstreamPort: upPort, State: RouteReady, IdleTimeout: s.routeIdle}}
	switch s.transport {
	case "direct":
		return "tcp", fmt.Sprintf("127.0.0.1:%d", upPort)
	case "unix":
		dir, err := os.MkdirTemp("", "ws")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.RemoveAll(dir) })
		sock := filepath.Join(dir, "f.sock")
		f := New(sock, "", routes, s.maxBody, s.idle, "", "test")
		if err := f.Start(context.Background()); err != nil {
			t.Skipf("unix listen not possible: %v", err)
		}
		t.Cleanup(func() { f.Close() })
		return "unix", sock
	default:
		f := New("", "127.0.0.1:0", routes, s.maxBody, s.idle, "", "test")
		if err := f.Start(context.Background()); err != nil {
			t.Skipf("tcp listen not possible: %v", err)
		}
		t.Cleanup(func() { f.Close() })
		return "tcp", fmt.Sprintf("127.0.0.1:%d", f.TCPPort())
	}
}

func runScenario(t *testing.T, s scenario) outcome {
	t.Helper()
	up := startWSUpstream(t, s.behavior == "rawbyte")
	network, addr := startFacadeFor(t, s, up.port)

	t0 := time.Now()
	c, err := net.Dial(network, addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	key := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef"))
	path := "/skill/ws"
	if s.transport == "direct" {
		path = "/ws"
	}
	fmt.Fprintf(c, "GET %s HTTP/1.1\r\nHost: x\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n", path, key)
	br := bufio.NewReader(c)
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("handshake read: %v", err)
	}
	if resp.StatusCode != 101 || resp.Header.Get("Sec-WebSocket-Accept") != wsAccept(key) {
		t.Fatalf("handshake failed: status %d accept %q", resp.StatusCode, resp.Header.Get("Sec-WebSocket-Accept"))
	}
	_ = c.SetReadDeadline(time.Time{})

	readEnd := make(chan connEnd, 1)
	go func() {
		for {
			var err error
			if s.behavior == "rawbyte" {
				_, err = br.ReadByte()
			} else {
				_, _, err = readFrame(br)
			}
			if err != nil {
				readEnd <- connEnd{time.Now(), err}
				return
			}
		}
	}()

	var interval time.Duration
	switch s.behavior {
	case "message", "rawbyte":
		interval = 500 * time.Millisecond
	case "ping":
		interval = time.Second
	}
	var tick <-chan time.Time
	if interval > 0 {
		tk := time.NewTicker(interval)
		defer tk.Stop()
		tick = tk.C
	}
	window := time.After(s.window)
	var res outcome
	var clientEnd connEnd
loop:
	for {
		select {
		case clientEnd = <-readEnd:
			break loop
		case <-window:
			res.alive = true
			return res
		case <-tick:
			var werr error
			switch s.behavior {
			case "message":
				werr = writeFrame(c, opText, []byte("hello"), true)
			case "ping":
				werr = writeFrame(c, opPing, []byte("p"), true)
			case "rawbyte":
				_, werr = c.Write([]byte{'x'})
			}
			if werr != nil {
				clientEnd = connEnd{time.Now(), werr}
				break loop
			}
		}
	}
	res.clientAfter = clientEnd.at.Sub(t0)
	res.clientErr = clientEnd.err.Error()
	select {
	case e := <-up.ended:
		res.upAfter = e.at.Sub(t0)
		res.upErr = e.err.Error()
	case <-time.After(3 * time.Second):
		res.upErr = "upstream did not see the connection end within 3s"
	}
	return res
}

func windowFor(idle time.Duration) time.Duration { return idle + 5*time.Second }

func requireIdleClose(t *testing.T, s scenario, got outcome) {
	t.Helper()
	if got.alive {
		t.Errorf("silent connection still open after %v with idle timeout %v", s.window, s.idle)
		return
	}
	if d := got.clientAfter - s.idle; d < -500*time.Millisecond || d > time.Second {
		t.Errorf("silent connection ended at %.1fs, want about %v", got.clientAfter.Seconds(), s.idle)
	}
}

func requireStaysOpen(t *testing.T, s scenario, got outcome) {
	t.Helper()
	if !got.alive {
		t.Errorf("busy (%s) connection was cut: %s; idle timeout %v, window %v", s.behavior, got, s.idle, s.window)
	}
}

// TestWebSocketIdleControlDirectBusy is the control: the same client talking
// straight to the echo upstream, no facade, must stay open for 20 s.
func TestWebSocketIdleControlDirectBusy(t *testing.T) {
	if testing.Short() {
		t.Skip("20 s")
	}
	t.Parallel()
	for _, b := range []string{"message", "ping", "rawbyte"} {
		t.Run(b, func(t *testing.T) {
			t.Parallel()
			s := scenario{transport: "direct", behavior: b, window: 20 * time.Second}
			got := runScenario(t, s)
			t.Logf("direct %s: %s", b, got)
			requireStaysOpen(t, s, got)
		})
	}
}

// TestWebSocketIdleSilentConnectionCloses: a connection that carries no data
// is closed by the facade after about idle_timeout.
func TestWebSocketIdleSilentConnectionCloses(t *testing.T) {
	t.Parallel()
	for _, tr := range []string{"tcp", "unix"} {
		for _, idle := range []time.Duration{2 * time.Second, 6 * time.Second, 20 * time.Second} {
			if testing.Short() && idle > 2*time.Second {
				continue
			}
			t.Run(fmt.Sprintf("%s/idle%v", tr, idle), func(t *testing.T) {
				t.Parallel()
				s := scenario{transport: tr, idle: idle, behavior: "none", window: windowFor(idle)}
				got := runScenario(t, s)
				t.Logf("%s idle=%v none: %s", tr, idle, got)
				requireIdleClose(t, s, got)
			})
		}
	}
}

// TestWebSocketIdleBusyConnectionStaysOpen: a connection that keeps
// exchanging messages (or pings) must outlive idle_timeout.
func TestWebSocketIdleBusyConnectionStaysOpen(t *testing.T) {
	t.Parallel()
	for _, tr := range []string{"tcp", "unix"} {
		for _, idle := range []time.Duration{2 * time.Second, 6 * time.Second, 20 * time.Second} {
			if testing.Short() && idle > 2*time.Second {
				continue
			}
			for _, b := range []string{"message", "ping", "rawbyte"} {
				t.Run(fmt.Sprintf("%s/idle%v/%s", tr, idle, b), func(t *testing.T) {
					t.Parallel()
					s := scenario{transport: tr, idle: idle, behavior: b, window: windowFor(idle)}
					got := runScenario(t, s)
					t.Logf("%s idle=%v %s: %s", tr, idle, b, got)
					requireStaysOpen(t, s, got)
				})
			}
		}
	}
}

// TestWebSocketIdleRootCauseCandidates changes one thing at a time to rule
// out other reasons for a busy connection to end.
func TestWebSocketIdleRootCauseCandidates(t *testing.T) {
	t.Parallel()
	t.Run("body limit of 1 byte does not end a busy connection", func(t *testing.T) {
		t.Parallel()
		s := scenario{transport: "tcp", idle: time.Hour, behavior: "message", window: 6 * time.Second, maxBody: 1}
		got := runScenario(t, s)
		t.Logf("%s", got)
		requireStaysOpen(t, s, got)
	})
	t.Run("connection outlives the 2 s dial context", func(t *testing.T) {
		t.Parallel()
		s := scenario{transport: "tcp", idle: time.Hour, behavior: "message", window: 6 * time.Second}
		got := runScenario(t, s)
		t.Logf("%s", got)
		requireStaysOpen(t, s, got)
	})
}

// TestWebSocketIdleRouteTimeoutOverridesFacade: a skill's own
// limits.idle_timeout_secs applies to its upgraded connections.
func TestWebSocketIdleRouteTimeoutOverridesFacade(t *testing.T) {
	t.Parallel()
	s := scenario{transport: "tcp", idle: time.Hour, routeIdle: 2 * time.Second, behavior: "none", window: 7 * time.Second}
	got := runScenario(t, s)
	t.Logf("%s", got)
	requireIdleClose(t, scenario{idle: 2 * time.Second, window: s.window}, got)
}

func TestIdleGuardNeverExtendsClosedConnection(t *testing.T) {
	a, b := net.Pipe()
	g := &idleGuard{idle: time.Hour, conns: []net.Conn{a}}
	guarded := g.wrap(a)
	a.Close()
	b.Close()
	if _, err := guarded.Write([]byte("x")); err == nil {
		t.Fatal("write on a closed connection succeeded")
	}
	if err := a.SetDeadline(time.Now().Add(time.Hour)); err == nil {
		t.Fatal("a closed connection accepted a new deadline")
	}
}

func TestIdleGuardConcurrentReadWriteAndNoGoroutineLeak(t *testing.T) {
	before := runtime.NumGoroutine()
	for i := 0; i < 20; i++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Skipf("tcp listen not possible: %v", err)
		}
		go func() {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close()
			_, _ = io.Copy(c, c)
		}()
		raw, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		g := &idleGuard{idle: time.Minute, conns: []net.Conn{raw}}
		c := g.wrap(raw)
		payload := make([]byte, 64<<10)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if _, err := c.Write(payload); err != nil {
					t.Errorf("write: %v", err)
					return
				}
			}
		}()
		go func() {
			defer wg.Done()
			if _, err := io.CopyN(io.Discard, c, 50*int64(len(payload))); err != nil {
				t.Errorf("read: %v", err)
			}
		}()
		wg.Wait()
		c.Close()
		ln.Close()
	}
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before {
		t.Fatalf("goroutines: %d before, %d after", before, after)
	}
}

// BenchmarkSplice1GiB copies 1 GiB through a client-to-upstream pair over
// loopback, either straight (io.Copy can splice between TCP sockets) or through
// the deadline-refreshing wrapper (it cannot).
func BenchmarkSplice1GiB(b *testing.B) {
	const total = int64(1 << 30)
	for _, wrapped := range []bool{false, true} {
		name := "plain"
		if wrapped {
			name = "wrapped"
		}
		b.Run(name, func(b *testing.B) {
			b.SetBytes(total)
			for i := 0; i < b.N; i++ {
				spliceOnce(b, total, wrapped)
			}
		})
	}
}

func spliceOnce(b *testing.B, total int64, wrapped bool) {
	b.Helper()
	pair := func() (net.Conn, net.Conn) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			b.Fatal(err)
		}
		defer ln.Close()
		accepted := make(chan net.Conn, 1)
		go func() { c, _ := ln.Accept(); accepted <- c }()
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			b.Fatal(err)
		}
		return c, <-accepted
	}
	srcW, srcR := pair() // source writes into srcW, facade reads srcR
	dstW, dstR := pair() // facade writes dstW, sink reads dstR
	defer srcW.Close()
	defer srcR.Close()
	defer dstW.Close()
	defer dstR.Close()

	var in, out net.Conn = srcR, dstW
	if wrapped {
		g := &idleGuard{idle: time.Minute, conns: []net.Conn{srcR, dstW}}
		in, out = g.wrap(srcR), g.wrap(dstW)
	}
	go func() {
		_, _ = io.CopyN(srcW, zeroReader{}, total)
		srcW.Close()
	}()
	go func() {
		_, _ = io.Copy(out, in)
		out.Close()
	}()
	n, err := io.Copy(io.Discard, dstR)
	if err != nil || n != total {
		b.Fatalf("sink got %d of %d bytes: %v", n, total, err)
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }
