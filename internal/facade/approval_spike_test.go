package facade

// Characterization of the facade as the approval gate (#428) will find it:
// what the sidecar receives today, whether a request held before forwarding
// still streams, and whether a client disconnect is visible while held.

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func spikeFacade(t *testing.T, upstream http.Handler) (*Facade, *httptest.Server) {
	t.Helper()
	up := httptest.NewServer(upstream)
	t.Cleanup(up.Close)
	port := up.Listener.Addr().(*net.TCPAddr).Port
	f := New("", "", []Route{{Mount: "echo", UpstreamPort: port, Skill: "echo-rest"}},
		1<<20, 30*time.Second, "", "test")
	return f, up
}

// What path and headers reach the sidecar for hostile spellings.
func TestSpike_UpstreamSeesPathAndOverrideHeaders(t *testing.T) {
	type seen struct{ path, raw, override string }
	got := make(chan seen, 1)
	f, _ := spikeFacade(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- seen{r.URL.Path, r.URL.EscapedPath(), r.Header.Get("X-HTTP-Method-Override")}
	}))
	for _, target := range []string{
		"/echo/public/../admin/x",
		"/echo//admin/x",
		"/echo/%2e%2e/admin/x",
		"/echo/a%2fb",
		"/echo/a%5cb",
	} {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		req.Header.Set("X-HTTP-Method-Override", "DELETE")
		f.handle(httptest.NewRecorder(), req)
		select {
		case s := <-got:
			t.Logf("%-28s -> upstream path=%q escaped=%q override=%q", target, s.path, s.raw, s.override)
			if s.override != "DELETE" {
				t.Errorf("override header no longer forwarded for %s: %q", target, s.override)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s: upstream not reached", target)
		}
	}
}

func serve(t *testing.T, h http.Handler) string {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String()
}

// A request that is held for a while before it is forwarded still streams
// its response frame by frame.
func TestSpike_HeldRequestStillStreamsSSE(t *testing.T) {
	f, _ := spikeFacade(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for i := 0; i < 3; i++ {
			io.WriteString(w, "data: x\n\n")
			fl.Flush()
			time.Sleep(150 * time.Millisecond)
		}
	}))
	const hold = 400 * time.Millisecond
	addr := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(hold) // stands in for the approval dialog
		f.handle(w, r)
	}))
	start := time.Now()
	resp, err := http.Get("http://" + addr + "/echo/tick")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if d := time.Since(start); d < hold {
		t.Fatalf("headers after %v, held %v", d, hold)
	}
	var at []time.Duration
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "data:") {
			at = append(at, time.Since(start))
		}
	}
	if len(at) != 3 {
		t.Fatalf("frames = %d", len(at))
	}
	t.Logf("frame arrival: %v", at)
	if at[2]-at[0] < 200*time.Millisecond {
		t.Errorf("frames were buffered: %v", at)
	}
}

// A chunked request body reaches the sidecar while the client is still
// sending, i.e. the facade does not buffer it.
func TestSpike_ChunkedUploadIsNotBuffered(t *testing.T) {
	firstSeen := make(chan struct{})
	f, _ := spikeFacade(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 5)
		io.ReadFull(r.Body, buf)
		close(firstSeen)
		io.Copy(io.Discard, r.Body)
	}))
	addr := serve(t, http.HandlerFunc(f.handle))
	pr, pw := io.Pipe()
	req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/echo/upload", pr)
	req.ContentLength = -1 // chunked
	done := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
		}
		done <- err
	}()
	pw.Write([]byte("first"))
	select {
	case <-firstSeen:
	case <-time.After(2 * time.Second):
		t.Fatal("first chunk did not reach the sidecar before the upload finished: body is buffered")
	}
	pw.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// Whether the request context ends when the client goes away while the
// handler is held, for a GET and for a POST whose body was not read.
func TestSpike_ClientDisconnectWhileHeld(t *testing.T) {
	var mu sync.Mutex
	ended := map[string]time.Duration{}
	addr := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		select {
		case <-r.Context().Done():
			mu.Lock()
			ended[r.Method] = time.Since(start)
			mu.Unlock()
		case <-time.After(1500 * time.Millisecond):
		}
	}))
	for _, c := range []struct{ method, body string }{
		{"GET", ""},
		{"POST", "Content-Length: 1000\r\n"},
	} {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		io.WriteString(conn, c.method+" /x HTTP/1.1\r\nHost: x\r\n"+c.body+"\r\n")
		if c.method == "POST" {
			io.WriteString(conn, "partial") // body never completes
		}
		time.Sleep(200 * time.Millisecond)
		conn.Close()
		time.Sleep(1700 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, m := range []string{"GET", "POST"} {
		if d, ok := ended[m]; ok {
			t.Logf("%s: context ended %v after the handler started (client left at 200ms)", m, d)
		} else {
			t.Logf("%s: context did NOT end; handler held to its timeout", m)
		}
	}
	if _, ok := ended["GET"]; !ok {
		t.Error("a held GET does not see the client disconnect")
	}
	_ = context.Background
}
