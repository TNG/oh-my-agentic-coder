package cli

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// requireLoopbackDial skips when this environment forbids connecting to
// 127.0.0.1 (some sandboxes deny loopback TCP connect).
func requireLoopbackDial(t *testing.T) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("loopback listen not permitted:", err)
	}
	defer ln.Close()
	c, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second)
	if err != nil {
		t.Skip("loopback dial not permitted in this environment:", err)
	}
	c.Close()
}

func TestControlInfoRoundTrip(t *testing.T) {
	// Isolate the well-known path under a temp TMPDIR.
	t.Setenv("TMPDIR", t.TempDir())

	if _, ok := readControlInfo(); ok {
		t.Fatal("expected no control-info before write")
	}
	if err := writeControlInfo("http://127.0.0.1:12345"); err != nil {
		t.Fatalf("writeControlInfo: %v", err)
	}
	ci, ok := readControlInfo()
	if !ok {
		t.Fatal("expected control-info after write")
	}
	if ci.ControlBase != "http://127.0.0.1:12345" {
		t.Errorf("ControlBase = %q", ci.ControlBase)
	}
	if ci.PID != os.Getpid() {
		t.Errorf("PID = %d, want %d", ci.PID, os.Getpid())
	}

	// removeControlInfo only removes our own (PID matches here).
	removeControlInfo()
	if _, ok := readControlInfo(); ok {
		t.Error("expected control-info removed")
	}
}

func TestNotifyReloadNoServe(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	ok, msg := notifyReload("/some/dir")
	if ok {
		t.Error("expected notify to fail with no serve running")
	}
	if !strings.Contains(msg, "no running omac serve") {
		t.Errorf("unexpected msg: %q", msg)
	}
}

func TestNotifyReloadHitsControlPlane(t *testing.T) {
	requireLoopbackDial(t)
	t.Setenv("TMPDIR", t.TempDir())

	var gotDir string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/__omac__/reload" || r.Method != http.MethodPost {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		// Echo back so we can assert the dir was forwarded.
		buf := make([]byte, r.ContentLength)
		r.Body.Read(buf)
		gotDir = string(buf)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"state":"active","skills":[]}`))
	}))
	defer srv.Close()

	if err := writeControlInfo(srv.URL); err != nil {
		t.Fatalf("writeControlInfo: %v", err)
	}
	ok, msg := notifyReload("/proj/x")
	if !ok {
		t.Fatalf("notify failed: %s", msg)
	}
	if !strings.Contains(gotDir, "/proj/x") {
		t.Errorf("control plane did not receive dir; body=%q", gotDir)
	}
}
