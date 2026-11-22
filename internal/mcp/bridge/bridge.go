// Package bridge relays a harness's stdio MCP connection to the omac facade's
// Streamable HTTP mount for one registered MCP server.
package bridge

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/mcp/mcppolicy"
	"github.com/TNG/oh-my-agentic-coder/internal/mcp/mcpreg"
)

const (
	jsonRPCInternalError = -32603
	deleteTimeout        = 5 * time.Second
	maxLineBytes         = 16 << 20
)

// A gateway that is down is retried for sidecarDownRetry, sleeping
// sidecarDownPause between attempts. Variables so tests can shorten them.
var (
	sidecarDownRetry = 10 * time.Second
	sidecarDownPause = 250 * time.Millisecond
)

// httpError is a non-2xx answer from the facade or the gateway behind it.
type httpError struct {
	status int
	reason string
	detail string
}

func (e *httpError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.status, e.detail) }

// neverReached reports whether err proves the request did not reach the MCP
// server, so sending it again cannot run anything twice: the sidecar is down
// (503) or nothing listens at the facade.
func neverReached(err error) bool {
	var he *httpError
	if errors.As(err, &he) {
		return he.status == http.StatusServiceUnavailable && he.reason == "sidecar-down"
	}
	return errors.Is(err, syscall.ECONNREFUSED)
}

func sessionGone(err error) bool {
	var he *httpError
	return errors.As(err, &he) && he.status == http.StatusNotFound && he.reason == mcppolicy.ReasonUnknownSession
}

type message struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

func (m message) isRequest() bool  { return m.Method != "" && hasID(m.ID) }
func (m message) isResponse() bool { return m.Method == "" && hasID(m.ID) }

func hasID(id json.RawMessage) bool { return len(id) > 0 && string(id) != "null" }

type bridge struct {
	client   *http.Client
	endpoint string
	token    string
	out      io.Writer
	logf     func(format string, args ...any)

	outMu sync.Mutex

	mu       sync.Mutex
	session  string
	protocol string
	initLine []byte

	// reconnectMu serializes reconnects so concurrent failures open one session.
	reconnectMu sync.Mutex
	reinits     int

	streamMu     sync.Mutex
	streamCancel context.CancelFunc
	streams      sync.WaitGroup
}

// Run relays newline-delimited JSON-RPC from in to the facade mount for the
// named server and writes everything the server sends to out. It returns
// after in reaches EOF and the session has been closed.
func Run(ctx context.Context, name string, in io.Reader, out io.Writer, getenv func(string) string) error {
	b, err := newBridge(name, out, getenv)
	if err != nil {
		return err
	}
	return b.run(ctx, in)
}

func newBridge(name string, out io.Writer, getenv func(string) string) (*bridge, error) {
	transport := &http.Transport{Proxy: nil}
	b := &bridge{out: out, logf: func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, "omac mcp bridge: "+format+"\n", args...)
	}}
	path := "/" + mcpreg.FacadeNamespace + "/" + url.PathEscape(name) + "/mcp"
	if base, ok := tcpBase(getenv("OMAC_BASE")); ok {
		b.endpoint = strings.TrimRight(base, "/") + path
		b.token = getenv("OMAC_FACADE_TOKEN")
	} else if socket := getenv("OMAC_SOCKET"); socket != "" {
		transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		}
		b.endpoint = "http://omac" + path
	} else {
		return nil, errors.New("neither OMAC_BASE nor OMAC_SOCKET is set")
	}
	b.client = &http.Client{Transport: transport}
	return b, nil
}

func tcpBase(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", false
	}
	return raw, true
}

func (b *bridge) run(ctx context.Context, in io.Reader) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	lines := readLines(in)
	var inflight sync.WaitGroup
	initialized := false

loop:
	for {
		var line []byte
		select {
		case l, ok := <-lines:
			if !ok {
				break loop
			}
			line = l
		case <-ctx.Done():
			break loop
		}
		var msg message
		if err := json.Unmarshal(line, &msg); err != nil {
			b.logf("dropping invalid JSON line: %v", err)
			continue
		}
		if !initialized && msg.Method == "initialize" {
			b.mu.Lock()
			b.initLine = line
			b.mu.Unlock()
			if b.send(ctx, line, msg) {
				initialized = true
				b.startStream(ctx)
			}
			continue
		}
		inflight.Add(1)
		go func() {
			defer inflight.Done()
			b.send(ctx, line, msg)
		}()
	}

	inflight.Wait()
	b.terminate()
	cancel()
	b.streams.Wait()
	return nil
}

// startStream opens the GET stream for the current session, replacing the
// previous one.
func (b *bridge) startStream(ctx context.Context) {
	b.streamMu.Lock()
	defer b.streamMu.Unlock()
	if b.streamCancel != nil {
		b.streamCancel()
	}
	streamCtx, cancel := context.WithCancel(ctx)
	b.streamCancel = cancel
	b.streams.Add(1)
	go func() {
		defer b.streams.Done()
		b.listen(streamCtx)
	}()
}

func readLines(in io.Reader) <-chan []byte {
	lines := make(chan []byte)
	go func() {
		defer close(lines)
		r := bufio.NewReaderSize(in, 64<<10)
		for {
			line, err := r.ReadBytes('\n')
			if len(line) > maxLineBytes {
				line = nil
			}
			if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 {
				lines <- trimmed
			}
			if err != nil {
				return
			}
		}
	}()
	return lines
}

func (b *bridge) newRequest(ctx context.Context, method string, body []byte, accept string) (*http.Request, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, b.endpoint, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "")
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if b.token != "" {
		req.Header.Set("X-Omac-Facade-Token", b.token)
	}
	b.mu.Lock()
	session, protocol := b.session, b.protocol
	b.mu.Unlock()
	if session != "" {
		req.Header.Set("Mcp-Session-Id", session)
	}
	if protocol != "" {
		req.Header.Set("MCP-Protocol-Version", protocol)
	}
	return req, nil
}

// send posts one client message and relays the reply. It reports whether the
// exchange succeeded; a failed request is answered with a JSON-RPC error.
func (b *bridge) send(ctx context.Context, line []byte, msg message) bool {
	err := b.deliver(ctx, line, msg)
	if err == nil {
		return true
	}
	b.logf("%s failed: %v", describe(msg), err)
	if msg.isRequest() {
		b.writeError(msg.ID, err)
	}
	return false
}

// deliver posts the message, waiting out a gateway restart. When the gateway
// has lost the session (it was restarted) the bridge opens a new one and sends
// the message once more. Only failures that prove the message never reached
// the server are retried; a dropped response is not.
func (b *bridge) deliver(ctx context.Context, line []byte, msg message) error {
	session := b.currentSession()
	err := b.postWhenUp(ctx, line, msg)
	if sessionGone(err) && msg.Method != "initialize" && b.hasInitialize() {
		if rerr := b.reconnect(ctx, session); rerr != nil {
			return fmt.Errorf("reconnect after gateway restart: %w", rerr)
		}
		err = b.postWhenUp(ctx, line, msg)
	}
	return err
}

// postWhenUp posts the message and retries while the gateway is down.
func (b *bridge) postWhenUp(ctx context.Context, line []byte, msg message) error {
	deadline := time.Now().Add(sidecarDownRetry)
	for {
		err := b.post(ctx, line, msg, false)
		if err == nil || !neverReached(err) || !time.Now().Add(sidecarDownPause).Before(deadline) {
			return err
		}
		select {
		case <-time.After(sidecarDownPause):
		case <-ctx.Done():
			return err
		}
	}
}

func (b *bridge) currentSession() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.session
}

func (b *bridge) hasInitialize() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.initLine != nil
}

// reconnect opens a new session by replaying the client's initialize request
// under an internal id and swallowing its answer, then announces
// notifications/initialized and reopens the GET stream. It does nothing when
// another request already replaced the session that failed.
func (b *bridge) reconnect(ctx context.Context, failed string) error {
	b.reconnectMu.Lock()
	defer b.reconnectMu.Unlock()
	if b.currentSession() != failed {
		return nil
	}
	b.mu.Lock()
	cached := b.initLine
	b.reinits++
	id := fmt.Sprintf("omac-bridge-reinit-%d", b.reinits)
	b.mu.Unlock()

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(cached, &fields); err != nil {
		return err
	}
	fields["id"], _ = json.Marshal(id)
	line, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	if err := b.postFresh(ctx, line, message{ID: fields["id"], Method: "initialize"}); err != nil {
		return err
	}
	initialized := []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if err := b.post(ctx, initialized, message{Method: "notifications/initialized"}, false); err != nil {
		return err
	}
	b.startStream(ctx)
	return nil
}

func (b *bridge) postFresh(ctx context.Context, line []byte, msg message) error {
	deadline := time.Now().Add(sidecarDownRetry)
	for {
		err := b.post(ctx, line, msg, true)
		if err == nil || !neverReached(err) || !time.Now().Add(sidecarDownPause).Before(deadline) {
			return err
		}
		select {
		case <-time.After(sidecarDownPause):
		case <-ctx.Done():
			return err
		}
	}
}

func describe(msg message) string {
	if msg.Method != "" {
		return msg.Method
	}
	return "response"
}

// post sends one message. With fresh set the request carries no session and
// the answer is not written to the client: it replays an initialize.
func (b *bridge) post(ctx context.Context, line []byte, msg message, fresh bool) error {
	req, err := b.newRequest(ctx, http.MethodPost, line, "application/json, text/event-stream")
	if err != nil {
		return err
	}
	if fresh {
		req.Header.Del("Mcp-Session-Id")
		req.Header.Del("MCP-Protocol-Version")
	}
	emit := b.writeLine
	if fresh {
		emit = func([]byte) error { return nil }
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusAccepted {
		return nil
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return &httpError{status: resp.StatusCode, reason: resp.Header.Get("X-Omac-Reason"), detail: strings.TrimSpace(string(detail))}
	}
	if msg.Method == "initialize" {
		if id := resp.Header.Get("Mcp-Session-Id"); id != "" {
			b.mu.Lock()
			b.session = id
			b.mu.Unlock()
		}
	}

	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if mediaType == "text/event-stream" {
		answered, err := b.relayEvents(resp.Body, msg, emit)
		if err == nil && !answered && msg.isRequest() {
			err = errors.New("event stream ended without a response")
		}
		return err
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(body)) == 0 {
		if msg.isRequest() {
			return errors.New("empty response body")
		}
		return nil
	}
	b.observe(msg, body)
	return emit(body)
}

// relayEvents writes each SSE data event as one line and stops once the
// response to req has been written. It reports whether that response arrived.
func (b *bridge) relayEvents(body io.Reader, req message, emit func([]byte) error) (bool, error) {
	var answered bool
	err := readEvents(body, func(data []byte) bool {
		b.observe(req, data)
		if err := emit(data); err != nil {
			b.logf("write to stdout: %v", err)
		}
		if req.isRequest() {
			var m message
			if json.Unmarshal(data, &m) == nil && m.isResponse() && bytes.Equal(m.ID, req.ID) {
				answered = true
				return false
			}
		}
		return true
	})
	return answered, err
}

// observe records the negotiated protocol version from an initialize result.
func (b *bridge) observe(req message, data []byte) {
	if req.Method != "initialize" {
		return
	}
	var m struct {
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"result"`
	}
	if json.Unmarshal(data, &m) != nil || m.Result.ProtocolVersion == "" {
		return
	}
	b.mu.Lock()
	b.protocol = m.Result.ProtocolVersion
	b.mu.Unlock()
}

// listen forwards server-initiated messages from the GET stream until it ends.
func (b *bridge) listen(ctx context.Context) {
	req, err := b.newRequest(ctx, http.MethodGet, nil, "text/event-stream")
	if err != nil {
		b.logf("GET stream: %v", err)
		return
	}
	resp, err := b.client.Do(req)
	if err != nil {
		if ctx.Err() == nil {
			b.logf("GET stream: %v", err)
		}
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return
	}
	err = readEvents(resp.Body, func(data []byte) bool {
		if err := b.writeLine(data); err != nil {
			b.logf("write to stdout: %v", err)
		}
		return true
	})
	if err != nil && ctx.Err() == nil {
		b.logf("GET stream: %v", err)
	}
}

func (b *bridge) terminate() {
	b.mu.Lock()
	session := b.session
	b.mu.Unlock()
	if session == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), deleteTimeout)
	defer cancel()
	req, err := b.newRequest(ctx, http.MethodDelete, nil, "")
	if err != nil {
		b.logf("DELETE: %v", err)
		return
	}
	resp, err := b.client.Do(req)
	if err != nil {
		b.logf("DELETE: %v", err)
		return
	}
	resp.Body.Close()
}

func (b *bridge) writeError(id json.RawMessage, cause error) {
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": jsonRPCInternalError, "message": "omac mcp bridge: " + cause.Error()},
	})
	if err != nil {
		b.logf("encode error response: %v", err)
		return
	}
	if err := b.writeLine(body); err != nil {
		b.logf("write to stdout: %v", err)
	}
}

// writeLine writes one protocol message as a single line.
func (b *bridge) writeLine(data []byte) error {
	var compact bytes.Buffer
	if err := json.Compact(&compact, bytes.TrimSpace(data)); err != nil {
		b.logf("dropping non-JSON message: %v", err)
		return nil
	}
	compact.WriteByte('\n')
	b.outMu.Lock()
	defer b.outMu.Unlock()
	_, err := b.out.Write(compact.Bytes())
	return err
}

// readEvents calls handle with the data of each SSE event until handle returns
// false or the stream ends.
func readEvents(r io.Reader, handle func(data []byte) bool) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64<<10), maxLineBytes)
	var data []string
	flush := func() bool {
		if len(data) == 0 {
			return true
		}
		payload := strings.Join(data, "\n")
		data = nil
		return handle([]byte(payload))
	}
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			if !flush() {
				return nil
			}
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	flush()
	return nil
}
