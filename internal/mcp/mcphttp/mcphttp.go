// Package mcphttp is the client side of the MCP Streamable HTTP transport
// (revision 2025-06-18): it carries JSON-RPC messages to a server reached by
// URL and hands every message the server sends back to a callback. The
// gateway and the tool probe use it where they would otherwise start a
// stdio server.
package mcphttp

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
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	sessionHeader  = "Mcp-Session-Id"
	versionHeader  = "MCP-Protocol-Version"
	maxMessageSize = 16 << 20
	headerTimeout  = 60 * time.Second
)

// ErrSessionExpired is returned when the server no longer knows the session.
var ErrSessionExpired = errors.New("MCP server session expired")

// Config describes one server.
type Config struct {
	URL string
	// Headers are sent with every request. The caller has checked them.
	Headers map[string]string
	// Bearer, when not empty, is sent as "Authorization: Bearer <value>".
	Bearer string
	// AllowPrivateNetwork lets the client connect to private addresses. Loopback
	// is allowed only when the URL names it, and nothing else is ever a private
	// address by accident of DNS.
	AllowPrivateNetwork bool
	// Log receives diagnostics, never header values.
	Log io.Writer
}

// Transport sends messages to one server session.
type Transport struct {
	cfg       Config
	client    *http.Client
	onMessage func([]byte)
	onClose   func()

	mu        sync.Mutex
	sessionID string
	version   string
	closed    bool
	getOnce   sync.Once
	ctx       context.Context
	cancel    context.CancelFunc
}

// Validate checks a URL as the definition may hold it: https, or http for a
// loopback host only, with no user info, query secrets or fragment.
func Validate(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("server.url %q is not a valid URL", raw)
	}
	if u.User != nil {
		return errors.New("server.url must not contain user info; credentials go in a declared secret")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return errors.New("server.url must not contain a query or fragment; a token in a URL would be stored in the definition")
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !IsLoopbackHost(u.Hostname()) {
			return errors.New("server.url uses http, which is allowed only for a loopback host (localhost, 127.0.0.1, ::1); use https")
		}
	default:
		return fmt.Errorf("server.url scheme %q must be https (or http for a loopback host)", u.Scheme)
	}
	return nil
}

// IsLoopbackHost reports whether host names the local machine.
func IsLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// New returns a transport. onMessage receives each JSON-RPC message the server
// sends, from any goroutine. onClose is called once when the server session
// ends without Close being called.
func New(cfg Config, onMessage func([]byte), onClose func()) (*Transport, error) {
	if err := Validate(cfg.URL); err != nil {
		return nil, err
	}
	u, _ := url.Parse(cfg.URL)
	ctx, cancel := context.WithCancel(context.Background())
	t := &Transport{cfg: cfg, onMessage: onMessage, onClose: onClose, ctx: ctx, cancel: cancel}
	if t.cfg.Log == nil {
		t.cfg.Log = io.Discard
	}
	t.client = &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 15 * time.Second, Control: addressGuard(IsLoopbackHost(u.Hostname()), cfg.AllowPrivateNetwork)}).DialContext,
			ResponseHeaderTimeout: headerTimeout,
			TLSHandshakeTimeout:   15 * time.Second,
		},
		// A redirect would carry the credentials to another host.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return t, nil
}

// addressGuard refuses a connection to an address the user did not name:
// loopback only when the URL names a loopback host, private and link-local
// ranges unless allowed. It sees the resolved address, so a hostname that
// resolves to an internal address is refused. With an HTTP proxy configured in
// the environment the proxy resolves the name, and the guard sees the proxy.
func addressGuard(loopbackOK, privateOK bool) func(network, address string, c syscall.RawConn) error {
	return func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		ip := net.ParseIP(host)
		if ip == nil {
			return fmt.Errorf("refusing to connect to %q: not an IP address", host)
		}
		switch {
		case ip.IsLoopback():
			if !loopbackOK {
				return fmt.Errorf("refusing to connect to loopback address %s: the URL does not name a loopback host", ip)
			}
		case isPrivate(ip):
			if !privateOK {
				return fmt.Errorf("refusing to connect to private address %s: set server.allow_private_network to allow it", ip)
			}
		}
		return nil
	}
}

func isPrivate(ip net.IP) bool {
	if ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() || ip.IsInterfaceLocalMulticast() {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		// Carrier-grade NAT, 100.64.0.0/10.
		return v4[0] == 100 && v4[1]&0xc0 == 64
	}
	return false
}

// Send posts one JSON-RPC message. A request's reply, and any messages the
// server sends before it, arrive through onMessage. Send returns once the
// server has accepted the message, not when the reply is complete.
func (t *Transport) Send(msg []byte) error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return errors.New("session closed")
	}
	t.mu.Unlock()

	var head struct {
		Method string          `json:"method"`
		ID     json.RawMessage `json:"id"`
	}
	_ = json.Unmarshal(msg, &head)

	req, err := http.NewRequestWithContext(t.ctx, http.MethodPost, t.cfg.URL, bytes.NewReader(msg))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	t.setHeaders(req, head.Method == "initialize")
	resp, err := t.client.Do(req)
	if err != nil {
		return fmt.Errorf("MCP server request failed: %w", err)
	}
	switch {
	case resp.StatusCode == http.StatusNotFound && t.hasSession():
		resp.Body.Close()
		t.end()
		return ErrSessionExpired
	case resp.StatusCode >= 300:
		resp.Body.Close()
		return fmt.Errorf("MCP server answered HTTP %d", resp.StatusCode)
	}
	if head.Method == "initialize" {
		if id := resp.Header.Get(sessionHeader); id != "" {
			t.mu.Lock()
			t.sessionID = id
			t.mu.Unlock()
		}
	}
	if resp.StatusCode == http.StatusAccepted || resp.ContentLength == 0 {
		resp.Body.Close()
		t.afterAccepted(head.Method)
		return nil
	}
	go t.read(resp, head.Method)
	return nil
}

func (t *Transport) afterAccepted(method string) {
	if method == "notifications/initialized" {
		t.getOnce.Do(func() { go t.listen() })
	}
}

func (t *Transport) hasSession() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sessionID != ""
}

func (t *Transport) setHeaders(req *http.Request, initializing bool) {
	for k, v := range t.cfg.Headers {
		req.Header.Set(k, v)
	}
	if t.cfg.Bearer != "" {
		req.Header.Set("Authorization", "Bearer "+t.cfg.Bearer)
	}
	t.mu.Lock()
	if t.sessionID != "" {
		req.Header.Set(sessionHeader, t.sessionID)
	}
	if t.version != "" && !initializing {
		req.Header.Set(versionHeader, t.version)
	}
	t.mu.Unlock()
}

// read delivers the messages of a POST response: one JSON message or an SSE stream.
func (t *Transport) read(resp *http.Response, method string) {
	defer resp.Body.Close()
	mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	switch mt {
	case "text/event-stream":
		t.readEvents(resp.Body, method)
	default:
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxMessageSize+1))
		if err != nil || len(body) > maxMessageSize {
			fmt.Fprintln(t.cfg.Log, "mcp http: reply unreadable or too large")
			return
		}
		t.deliver(bytes.TrimSpace(body), method)
	}
}

func (t *Transport) readEvents(r io.Reader, method string) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), maxMessageSize)
	var data []byte
	flush := func() {
		if len(data) > 0 {
			t.deliver(bytes.TrimSpace(data), method)
		}
		data = nil
	}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, "data:"):
			if len(data) > 0 {
				data = append(data, '\n')
			}
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " ")...)
		}
	}
	flush()
}

// deliver passes a message on and keeps the negotiated protocol version.
func (t *Transport) deliver(msg []byte, method string) {
	if len(msg) == 0 {
		return
	}
	if method == "initialize" {
		var r struct {
			Result struct {
				ProtocolVersion string `json:"protocolVersion"`
			} `json:"result"`
		}
		if json.Unmarshal(msg, &r) == nil && r.Result.ProtocolVersion != "" {
			t.mu.Lock()
			t.version = r.Result.ProtocolVersion
			t.mu.Unlock()
		}
	}
	t.onMessage(msg)
}

// listen opens the GET stream for messages the server sends on its own. A
// server that does not offer one answers 405, which is fine.
func (t *Transport) listen() {
	req, err := http.NewRequestWithContext(t.ctx, http.MethodGet, t.cfg.URL, nil)
	if err != nil {
		return
	}
	req.Header.Set("Accept", "text/event-stream")
	t.setHeaders(req, false)
	resp, err := t.client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return
	}
	t.readEvents(resp.Body, "")
}

// end closes the transport because the server session is gone.
func (t *Transport) end() {
	t.mu.Lock()
	already := t.closed
	t.closed = true
	t.mu.Unlock()
	t.cancel()
	if !already && t.onClose != nil {
		t.onClose()
	}
}

// Close ends the session at the server (best effort) and stops all requests.
func (t *Transport) Close() {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	t.closed = true
	id := t.sessionID
	t.mu.Unlock()
	if id != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if req, err := http.NewRequestWithContext(ctx, http.MethodDelete, t.cfg.URL, nil); err == nil {
			t.setHeaders(req, false)
			if resp, err := t.client.Do(req); err == nil {
				resp.Body.Close()
			}
		}
	}
	t.cancel()
}
