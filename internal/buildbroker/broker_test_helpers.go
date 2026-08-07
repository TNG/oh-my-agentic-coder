package buildbroker

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tngtech/oh-my-agentic-coder/internal/buildengine"
)

// stubEngine is a configurable fake EngineInvoker for protocol tests.
// It records the worktree and args it was called with, writes
// scripted stdout/stderr chunks to the writers, and returns a
// scripted result. It also records whether graceful/force were closed
// and when.
type stubEngine struct {
	mu sync.Mutex

	// Inputs recorded from the broker.
	gotWorktree string
	gotArgs     []string

	// Scripted outputs. Each entry is written to the matching stream
	// in order. A chunk larger than MaxOutputFrameBytes exercises the
	// broker's chunking.
	stdoutChunks [][]byte
	stderrChunks [][]byte

	// Scripted result.
	result buildengine.Result

	// Hooks invoked when graceful/force are observed closed.
	onGraceful func()
	onForce    func()

	// BlockUntil closed lets a test hold the invoker running until a
	// cancel or disconnect is observed (so output-before-completion
	// and cancel-during-run can be exercised).
	blockUntil <-chan struct{}

	// Recorded close timing.
	gracefulClosed bool
	forceClosed    bool
}

func (s *stubEngine) invoke(worktree string, args []string, stdout, stderr io.Writer, graceful, force <-chan struct{}) buildengine.Result {
	s.mu.Lock()
	s.gotWorktree = worktree
	s.gotArgs = append([]string(nil), args...)
	s.mu.Unlock()

	// Stream stdout chunks first, then stderr chunks. Each Write goes
	// through the broker's chunked writer, which frames and flushes.
	for _, c := range s.stdoutChunks {
		_, _ = stdout.Write(c)
	}
	for _, c := range s.stderrChunks {
		_, _ = stderr.Write(c)
	}

	// Wait for a cancellation signal or the block-until channel, if
	// scripted. This is what lets a test observe output BEFORE the
	// terminal result, and observe graceful/force closing.
	if s.blockUntil != nil {
		select {
		case <-s.blockUntil:
		case <-graceful:
			s.mu.Lock()
			s.gracefulClosed = true
			if s.onGraceful != nil {
				s.onGraceful()
			}
			s.mu.Unlock()
			// Wait for force (the broker sends it after the deadline,
			// or the test sends it directly).
			select {
			case <-force:
				s.mu.Lock()
				s.forceClosed = true
				if s.onForce != nil {
					s.onForce()
				}
				s.mu.Unlock()
			case <-s.blockUntil:
			}
		case <-force:
			s.mu.Lock()
			s.forceClosed = true
			s.gracefulClosed = true // force implies graceful
			if s.onForce != nil {
				s.onForce()
			}
			s.mu.Unlock()
		}
	} else {
		// Even without a block, observe graceful/force if they close
		// before we return.
		select {
		case <-graceful:
			s.mu.Lock()
			s.gracefulClosed = true
			s.mu.Unlock()
		default:
		}
	}
	return s.result
}

// testBroker mounts a Broker on an httptest server with a stub engine
// and returns everything the test needs.
type testBroker struct {
	server *httptest.Server
	broker *Broker
	engine *stubEngine
	token  string
	authz  Authorizer
}

// newTestBroker constructs a testBroker with the given authorizer and
// stub engine. The token is a fixed test value.
func newTestBroker(t *testing.T, authz Authorizer, engine *stubEngine) *testBroker {
	t.Helper()
	token := "test-token-0123456789abcdef0123456789abcdef"
	b, err := New(Options{
		Token:         token,
		Authorizer:    authz,
		EngineInvoker: engine.invoke,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mux := http.NewServeMux()
	b.Mount(mux)
	srv := newTestServer(t, mux)
	return &testBroker{server: srv, broker: b, engine: engine, token: token, authz: authz}
}

// newTestServer wraps httptest.NewServer with cleanup. Tests that
// construct a Broker directly (without newTestBroker) use this.
func newTestServer(t *testing.T, mux *http.ServeMux) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// executePOST sends an execute request and returns the raw response
// body. The caller inspects the NDJSON stream.
func (tb *testBroker) executePOST(t *testing.T, body string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, tb.server.URL+ExecutePath, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", ContentTypeJSON)
	req.Header.Set("Authorization", "Bearer "+tb.token)
	req.Header.Set("Accept", AcceptNDJSON)
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("execute POST: %v", err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, data
}

// cancelPOST sends a cancel request and returns the response.
func (tb *testBroker) cancelPOST(t *testing.T, requestID, stage string) *http.Response {
	t.Helper()
	body := fmt.Sprintf(`{"stage":%q}`, stage)
	req, _ := http.NewRequest(http.MethodPost, tb.server.URL+CancelPathPrefix+requestID+CancelRouteSuffix, strings.NewReader(body))
	req.Header.Set("Content-Type", ContentTypeJSON)
	req.Header.Set("Authorization", "Bearer "+tb.token)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("cancel POST: %v", err)
	}
	resp.Body.Close()
	return resp
}

// frame is a decoded NDJSON frame.
type frame struct {
	Type       string `json:"type"`
	RequestID  string `json:"request_id"`
	Stream     string `json:"stream"`
	DataBase64 string `json:"data_base64"`
	Class      string `json:"class"`
	ExitCode   int    `json:"exit_code"`
	Message    string `json:"message,omitempty"`
}

// parseFrames parses the NDJSON response body into frames.
func parseFrames(t *testing.T, body []byte) []frame {
	t.Helper()
	var frames []frame
	for _, line := range bytes.Split(body, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var f frame
		if err := json.Unmarshal(line, &f); err != nil {
			t.Fatalf("parse frame %q: %v", string(line), err)
		}
		frames = append(frames, f)
	}
	return frames
}

// decodeOutput decodes a frame's data_base64.
func (f frame) decodeData(t *testing.T) []byte {
	t.Helper()
	out, err := base64.StdEncoding.DecodeString(f.DataBase64)
	if err != nil {
		t.Fatalf("decode base64: %v", err)
	}
	return out
}

// allowAllAuthorizer authorizes any worktree (canonicalizes only).
func allowAllAuthorizer() Authorizer {
	return func(clientWorktree string) (string, error) {
		return canonicalize(clientWorktree)
	}
}

// fixedAuthorizer authorizes exactly the given canonical path.
func fixedAuthorizer(canon string) Authorizer {
	return func(clientWorktree string) (string, error) {
		c, err := canonicalize(clientWorktree)
		if err != nil {
			return "", ErrUnauthorized
		}
		if c != canon {
			return "", ErrUnauthorized
		}
		return c, nil
	}
}

// successResult is a convenience for scripting the stub engine.
func successResult() buildengine.Result {
	return buildengine.Result{Class: buildengine.ClassSuccess, Exit: 0}
}

// runExecuteAndWait runs an execute POST in a goroutine and returns a
// channel that receives the response body when it completes. Used by
// cancel-during-run tests.
func (tb *testBroker) runExecuteAsync(t *testing.T, body string) <-chan asyncResult {
	t.Helper()
	ch := make(chan asyncResult, 1)
	go func() {
		resp, data := tb.executePOST(t, body)
		ch <- asyncResult{resp: resp, body: data}
		close(ch)
	}()
	return ch
}

type asyncResult struct {
	resp *http.Response
	body []byte
}
