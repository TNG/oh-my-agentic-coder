package facade

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/sectest"
)

func TestLimitedBody(t *testing.T) {
	cases := []struct {
		name    string
		body    io.Reader
		limit   int64
		want    string
		wantErr error
	}{
		{"empty", strings.NewReader(""), 4, "", nil},
		{"below limit", strings.NewReader("abc"), 4, "abc", nil},
		{"exactly the limit", strings.NewReader("abcd"), 4, "abcd", nil},
		{"one over", strings.NewReader("abcde"), 4, "abcd", errBodyTooLarge},
		{"far over", strings.NewReader(strings.Repeat("x", 100)), 4, "xxxx", errBodyTooLarge},
		{"exactly the limit, one byte per read", iotest.OneByteReader(strings.NewReader("abcd")), 4, "abcd", nil},
		{"one over, one byte per read", iotest.OneByteReader(strings.NewReader("abcde")), 4, "abcd", errBodyTooLarge},
		{"exactly the limit, data and EOF together", iotest.DataErrReader(strings.NewReader("abcd")), 4, "abcd", nil},
		{"one over, data and EOF together", iotest.DataErrReader(strings.NewReader("abcde")), 4, "abcd", errBodyTooLarge},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := &limitedBody{ReadCloser: io.NopCloser(c.body), n: c.limit}
			got, err := io.ReadAll(b)
			if string(got) != c.want {
				t.Errorf("read %q, want %q", got, c.want)
			}
			if !errors.Is(err, c.wantErr) {
				t.Errorf("error = %v, want %v", err, c.wantErr)
			}
		})
	}
}

type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

func TestLimitedBodyReadsAtMostOneByteBeyondLimit(t *testing.T) {
	src := &countingReader{r: strings.NewReader(strings.Repeat("x", 1000))}
	_, err := io.ReadAll(&limitedBody{ReadCloser: io.NopCloser(src), n: 10})
	if !errors.Is(err, errBodyTooLarge) {
		t.Fatalf("error = %v, want errBodyTooLarge", err)
	}
	if src.n > 11 {
		t.Errorf("read %d bytes from the source, want at most 11", src.n)
	}
}

func TestLimitedBodyNeverReturnsZeroWithoutError(t *testing.T) {
	b := &limitedBody{ReadCloser: io.NopCloser(strings.NewReader("abcd")), n: 4}
	for i := 0; i < 10; i++ {
		n, err := b.Read(make([]byte, 8))
		if n == 0 && err == nil {
			t.Fatal("Read returned (0, nil)")
		}
		if err != nil {
			return
		}
	}
	t.Fatal("no terminal error after 10 reads")
}

// bodyDrainingSidecarPort starts a sidecar that reads each request body to the
// end and returns its port.
func bodyDrainingSidecarPort(t *testing.T) int {
	t.Helper()
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
		}
	}))
	t.Cleanup(sidecar.Close)
	return sidecar.Listener.Addr().(*net.TCPAddr).Port
}

func bodyOfSize(n int64) []byte { return bytes.Repeat([]byte("x"), int(n)) }

// TestFacadeBodyLimitIsInclusive: a body of exactly the limit passes, one
// byte more is refused with 413, whether it announces its length or is chunked,
// on the plain path and on the WebSocket upgrade path.
func TestFacadeBodyLimitIsInclusive(t *testing.T) {
	sectest.RequireLoopbackListener(t)
	const limit = 4096

	t.Run("plain", func(t *testing.T) {
		port := bodyDrainingSidecarPort(t)
		fport := startFacade(t, &fakeSidecar{port: port}, limit)

		for _, chunked := range []bool{false, true} {
			for _, c := range []struct {
				size int64
				want int
			}{{limit, 200}, {limit + 1, 413}} {
				name := fmt.Sprintf("chunked=%v/size=%d", chunked, c.size)
				t.Run(name, func(t *testing.T) {
					var body io.Reader = bytes.NewReader(bodyOfSize(c.size))
					if chunked {
						body = io.MultiReader(body)
					}
					resp, err := http.Post(fmt.Sprintf("http://127.0.0.1:%d/skill/x", fport), "application/octet-stream", body)
					if err != nil {
						t.Fatal(err)
					}
					resp.Body.Close()
					if resp.StatusCode != c.want {
						t.Errorf("status %d, want %d", resp.StatusCode, c.want)
					}
				})
			}
		}
	})

	t.Run("upgrade", func(t *testing.T) {
		for _, chunked := range []bool{false, true} {
			for _, c := range []struct {
				size int64
				want int
			}{{limit, 101}, {limit + 1, 413}} {
				name := fmt.Sprintf("chunked=%v/size=%d", chunked, c.size)
				t.Run(name, func(t *testing.T) {
					s := startSidecar(t, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
					fport := startFacade(t, s, limit)
					head := "POST /skill/ws HTTP/1.1\r\nHost: facade\r\nConnection: upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n"
					var req string
					if chunked {
						req = head + "Transfer-Encoding: chunked\r\n\r\n" + fmt.Sprintf("%x\r\n", c.size) + string(bodyOfSize(c.size)) + "\r\n0\r\n\r\n"
					} else {
						req = head + fmt.Sprintf("Content-Length: %d\r\n\r\n", c.size) + string(bodyOfSize(c.size))
					}
					conn := dialFacade(t, fport, req)
					_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
					resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
					if err != nil {
						t.Fatalf("read response: %v", err)
					}
					if resp.StatusCode != c.want {
						msg, _ := io.ReadAll(resp.Body)
						t.Errorf("status %d, want %d (%s)", resp.StatusCode, c.want, strings.TrimSpace(string(msg)))
					}
				})
			}
		}
	})
}
