// Package echomodel is a fake model server for tests and manual debugging.
// It speaks the OpenAI chat/completions and Anthropic messages APIs and
// answers every request with a readable summary of what it received, so a
// harness can run end to end without model access and a human can see what
// the harness actually sent.
package echomodel

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Options configures a Server.
type Options struct {
	// Addr is the listen address. Only 127.0.0.1 is accepted. Empty means
	// "127.0.0.1:0", which picks a free port.
	Addr string
	// DumpDir, when set, receives one JSON file per request.
	DumpDir string
}

// Server is the echo model server. Use it as an http.Handler directly, or
// call Start to listen on loopback.
type Server struct {
	opts Options

	mu       sync.Mutex
	count    int
	requests [][]byte
	srv      *http.Server
	url      string
}

// New returns a Server that is not yet listening.
func New(opts Options) *Server {
	return &Server{opts: opts}
}

// Start listens on the configured loopback address and serves in the
// background.
func (s *Server) Start() error {
	addr := s.opts.Addr
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("echomodel: address %q: %w", addr, err)
	}
	if host != "127.0.0.1" {
		return fmt.Errorf("echomodel: address %q: only 127.0.0.1 is allowed", addr)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("echomodel: listen: %w", err)
	}
	s.mu.Lock()
	s.srv = &http.Server{Handler: s}
	s.url = "http://" + ln.Addr().String()
	srv := s.srv
	s.mu.Unlock()
	go func() { _ = srv.Serve(ln) }()
	return nil
}

// URL is the base URL of the listening server, without a path.
func (s *Server) URL() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.url
}

// Close stops the listener and drops open connections.
func (s *Server) Close() error {
	s.mu.Lock()
	srv := s.srv
	s.mu.Unlock()
	if srv == nil {
		return nil
	}
	return srv.Close()
}

// Requests returns a copy of every recorded request body, in arrival order.
func (s *Server) Requests() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([][]byte, len(s.requests))
	for i, b := range s.requests {
		out[i] = append([]byte(nil), b...)
	}
	return out
}

type apiKind int

const (
	openAI apiKind = iota
	anthropic
)

func route(path string) (apiKind, bool) {
	switch {
	case strings.HasSuffix(path, "/chat/completions"):
		return openAI, true
	case strings.HasSuffix(path, "/messages"):
		return anthropic, true
	}
	return 0, false
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	kind, ok := route(r.URL.Path)
	if !ok || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var req request
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return
	}

	headers := visibleHeaders(r.Header)
	s.mu.Lock()
	s.count++
	n := s.count
	s.requests = append(s.requests, body)
	s.mu.Unlock()

	if s.opts.DumpDir != "" {
		if err := s.dump(n, r, headers, body); err != nil {
			http.Error(w, "dump failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}

	text := describe(kind, req, headers, n)
	switch {
	case kind == openAI && req.Stream:
		writeOpenAIStream(w, req.Model, text)
	case kind == openAI:
		writeJSON(w, openAIResponse(req.Model, text))
	case req.Stream:
		writeAnthropicStream(w, req.Model, text)
	default:
		writeJSON(w, anthropicResponse(req.Model, text))
	}
}

// request holds the fields of both APIs that the echo needs.
type request struct {
	Model    string          `json:"model"`
	Stream   bool            `json:"stream"`
	System   json.RawMessage `json:"system"`
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
	Tools []struct {
		Name     string `json:"name"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	} `json:"tools"`
}

// contentText returns the text of a message content that is either a plain
// string or a list of blocks; only blocks of type "text" count.
func contentText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func (r request) systemPrompt(kind apiKind) string {
	if kind == anthropic {
		return contentText(r.System)
	}
	var parts []string
	for _, m := range r.Messages {
		if m.Role == "system" {
			parts = append(parts, contentText(m.Content))
		}
	}
	return strings.Join(parts, "\n")
}

func (r request) lastUserMessage() string {
	for i := len(r.Messages) - 1; i >= 0; i-- {
		if r.Messages[i].Role != "user" {
			continue
		}
		if t := contentText(r.Messages[i].Content); t != "" {
			return t
		}
	}
	return ""
}

func (r request) toolNames() []string {
	var names []string
	for _, t := range r.Tools {
		name := t.Function.Name
		if name == "" {
			name = t.Name
		}
		names = append(names, name)
	}
	return names
}

// omittedHeaders carry credentials and never appear in a response or dump.
var omittedHeaders = map[string]bool{
	"Authorization": true,
	"X-Api-Key":     true,
	"Cookie":        true,
	"Set-Cookie":    true,
}

func visibleHeaders(h http.Header) map[string]string {
	out := map[string]string{}
	for k, v := range h {
		if omittedHeaders[http.CanonicalHeaderKey(k)] {
			continue
		}
		out[http.CanonicalHeaderKey(k)] = strings.Join(v, ", ")
	}
	return out
}

func describe(kind apiKind, req request, headers map[string]string, n int) string {
	system := req.systemPrompt(kind)
	sum := sha256.Sum256([]byte(system))
	keys := make([]string, 0, len(headers))
	for k := range headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	fmt.Fprintf(&b, "echo model\n")
	fmt.Fprintf(&b, "request: %d\n", n)
	fmt.Fprintf(&b, "model: %s\n", req.Model)
	fmt.Fprintf(&b, "messages: %d\n", len(req.Messages))
	fmt.Fprintf(&b, "system prompt length: %d\n", len(system))
	fmt.Fprintf(&b, "system prompt sha256: %s\n", hex.EncodeToString(sum[:]))
	fmt.Fprintf(&b, "last user message: %s\n", req.lastUserMessage())
	fmt.Fprintf(&b, "tools: %s\n", strings.Join(req.toolNames(), ", "))
	fmt.Fprintf(&b, "headers:\n")
	for _, k := range keys {
		fmt.Fprintf(&b, "  %s: %s\n", strings.ToLower(k), headers[k])
	}
	return b.String()
}

func (s *Server) dump(n int, r *http.Request, headers map[string]string, body []byte) error {
	if err := os.MkdirAll(s.opts.DumpDir, 0o755); err != nil {
		return err
	}
	var payload any = string(body)
	if json.Valid(body) {
		payload = json.RawMessage(body)
	}
	data, err := json.MarshalIndent(map[string]any{
		"number":  n,
		"method":  r.Method,
		"path":    r.URL.Path,
		"headers": headers,
		"body":    payload,
	}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.opts.DumpDir, fmt.Sprintf("%06d.json", n)), data, 0o644)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func openAIResponse(model, text string) map[string]any {
	return map[string]any{
		"id": "echo", "object": "chat.completion", "created": 0, "model": model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": text},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
	}
}

func anthropicResponse(model, text string) map[string]any {
	return map[string]any{
		"id": "msg_echo", "type": "message", "role": "assistant", "model": model,
		"content":       []any{map[string]any{"type": "text", "text": text}},
		"stop_reason":   "end_turn",
		"stop_sequence": nil,
		"usage":         map[string]any{"input_tokens": 1, "output_tokens": 1},
	}
}

type sseWriter struct {
	w http.ResponseWriter
	f http.Flusher
}

func newSSE(w http.ResponseWriter) sseWriter {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	f, _ := w.(http.Flusher)
	return sseWriter{w: w, f: f}
}

// event writes one SSE message; an empty name omits the event line.
func (s sseWriter) event(name string, data any) {
	raw, ok := data.(string)
	if !ok {
		b, _ := json.Marshal(data)
		raw = string(b)
	}
	if name != "" {
		fmt.Fprintf(s.w, "event: %s\n", name)
	}
	fmt.Fprintf(s.w, "data: %s\n\n", raw)
	if s.f != nil {
		s.f.Flush()
	}
}

func writeOpenAIStream(w http.ResponseWriter, model, text string) {
	s := newSSE(w)
	chunk := func(delta map[string]any, finish any) map[string]any {
		return map[string]any{
			"id": "echo", "object": "chat.completion.chunk", "created": 0, "model": model,
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
		}
	}
	s.event("", chunk(map[string]any{"role": "assistant", "content": text}, nil))
	last := chunk(map[string]any{}, "stop")
	last["usage"] = map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}
	s.event("", last)
	s.event("", "[DONE]")
}

func writeAnthropicStream(w http.ResponseWriter, model, text string) {
	s := newSSE(w)
	start := anthropicResponse(model, "")
	start["content"] = []any{}
	start["stop_reason"] = nil
	s.event("message_start", map[string]any{"type": "message_start", "message": start})
	s.event("content_block_start", map[string]any{
		"type": "content_block_start", "index": 0,
		"content_block": map[string]any{"type": "text", "text": ""},
	})
	s.event("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": 0,
		"delta": map[string]any{"type": "text_delta", "text": text},
	})
	s.event("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	s.event("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": 1},
	})
	s.event("message_stop", map[string]any{"type": "message_stop"})
}
