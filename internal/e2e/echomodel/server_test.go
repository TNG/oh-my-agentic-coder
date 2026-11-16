package echomodel

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const secretValue = "super-secret-credential"

func startServer(t *testing.T, opts Options) *Server {
	t.Helper()
	s := New(opts)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func post(t *testing.T, url, body string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+secretValue)
	req.Header.Set("X-Api-Key", secretValue)
	req.Header.Set("Cookie", "session="+secretValue)
	req.Header.Set("X-Custom", "visible-header")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(data)
}

const openAIBody = `{"model":"gpt-x","stream":%t,"messages":[
 {"role":"system","content":"be brief"},
 {"role":"user","content":"first question"},
 {"role":"assistant","content":"first answer"},
 {"role":"user","content":[{"type":"text","text":"last question"}]}],
 "tools":[{"type":"function","function":{"name":"read_file"}},{"type":"function","function":{"name":"bash"}}]}`

const anthropicBody = `{"model":"claude-x","stream":%t,"system":[{"type":"text","text":"be brief"}],
 "messages":[
 {"role":"user","content":"first question"},
 {"role":"assistant","content":[{"type":"text","text":"first answer"}]},
 {"role":"user","content":[{"type":"text","text":"last question"}]}],
 "tools":[{"name":"read_file"},{"name":"bash"}]}`

func assertEchoText(t *testing.T, text, model string, messages int) {
	t.Helper()
	sum := sha256.Sum256([]byte("be brief"))
	for _, want := range []string{
		"model: " + model,
		fmt.Sprintf("messages: %d", messages),
		"system prompt length: 8",
		"system prompt sha256: " + hex.EncodeToString(sum[:]),
		"last user message: last question",
		"tools: read_file, bash",
		"request: 1",
		"x-custom: visible-header",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("echo text lacks %q:\n%s", want, text)
		}
	}
}

func assertNoSecrets(t *testing.T, what, s string) {
	t.Helper()
	if strings.Contains(s, secretValue) {
		t.Errorf("%s leaks credential:\n%s", what, s)
	}
	low := strings.ToLower(s)
	for _, h := range []string{"authorization", "x-api-key", "cookie"} {
		if strings.Contains(low, h) {
			t.Errorf("%s mentions header %q:\n%s", what, h, s)
		}
	}
}

func sseData(t *testing.T, body string) []string {
	t.Helper()
	var out []string
	sc := bufio.NewScanner(strings.NewReader(body))
	for sc.Scan() {
		if d, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
			out = append(out, d)
		}
	}
	return out
}

func TestOpenAINonStream(t *testing.T) {
	s := startServer(t, Options{})
	for _, path := range []string{"/v1/chat/completions", "/chat/completions"} {
		resp, body := post(t, s.URL()+path, fmt.Sprintf(openAIBody, false))
		if resp.StatusCode != 200 {
			t.Fatalf("%s: status %d: %s", path, resp.StatusCode, body)
		}
		var out struct {
			Model   string `json:"model"`
			Choices []struct {
				Message struct {
					Role    string `json:"role"`
					Content string `json:"content"`
				} `json:"message"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("%v: %s", err, body)
		}
		if len(out.Choices) != 1 || out.Choices[0].Message.Role != "assistant" || out.Choices[0].FinishReason != "stop" {
			t.Fatalf("unexpected response: %s", body)
		}
		text := out.Choices[0].Message.Content
		if path == "/v1/chat/completions" {
			assertEchoText(t, text, "gpt-x", 4)
		}
		assertNoSecrets(t, "response", body)
	}
}

func TestOpenAIStream(t *testing.T) {
	s := startServer(t, Options{})
	resp, body := post(t, s.URL()+"/v1/chat/completions", fmt.Sprintf(openAIBody, true))
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content type %q", ct)
	}
	events := sseData(t, body)
	if len(events) < 3 || events[len(events)-1] != "[DONE]" {
		t.Fatalf("events: %v", events)
	}
	var text strings.Builder
	sawFinish := false
	for _, e := range events[:len(events)-1] {
		var c struct {
			Choices []struct {
				Delta        struct{ Content string } `json:"delta"`
				FinishReason *string                  `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(e), &c); err != nil {
			t.Fatalf("%v: %s", err, e)
		}
		text.WriteString(c.Choices[0].Delta.Content)
		if c.Choices[0].FinishReason != nil && *c.Choices[0].FinishReason == "stop" {
			sawFinish = true
		}
	}
	if !sawFinish {
		t.Error("no finish_reason stop chunk")
	}
	assertEchoText(t, text.String(), "gpt-x", 4)
	assertNoSecrets(t, "response", body)
}

func TestAnthropicNonStream(t *testing.T) {
	s := startServer(t, Options{})
	for _, path := range []string{"/v1/messages", "/messages"} {
		resp, body := post(t, s.URL()+path, fmt.Sprintf(anthropicBody, false))
		if resp.StatusCode != 200 {
			t.Fatalf("%s: status %d: %s", path, resp.StatusCode, body)
		}
		var out struct {
			Type       string `json:"type"`
			Role       string `json:"role"`
			StopReason string `json:"stop_reason"`
			Content    []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("%v: %s", err, body)
		}
		if out.Type != "message" || out.Role != "assistant" || out.StopReason != "end_turn" || len(out.Content) != 1 || out.Content[0].Type != "text" {
			t.Fatalf("unexpected response: %s", body)
		}
		if path == "/v1/messages" {
			assertEchoText(t, out.Content[0].Text, "claude-x", 3)
		}
		assertNoSecrets(t, "response", body)
	}
}

func TestAnthropicStream(t *testing.T) {
	s := startServer(t, Options{})
	resp, body := post(t, s.URL()+"/v1/messages", fmt.Sprintf(anthropicBody, true))
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content type %q", ct)
	}
	var names []string
	var text strings.Builder
	sc := bufio.NewScanner(strings.NewReader(body))
	for sc.Scan() {
		line := sc.Text()
		if n, ok := strings.CutPrefix(line, "event: "); ok {
			names = append(names, n)
		}
		if d, ok := strings.CutPrefix(line, "data: "); ok {
			var e struct {
				Type  string `json:"type"`
				Delta struct {
					Text string `json:"text"`
				} `json:"delta"`
			}
			if err := json.Unmarshal([]byte(d), &e); err != nil {
				t.Fatalf("%v: %s", err, d)
			}
			if e.Type == "content_block_delta" {
				text.WriteString(e.Delta.Text)
			}
		}
	}
	want := []string{"message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("events %v, want %v", names, want)
	}
	assertEchoText(t, text.String(), "claude-x", 3)
	assertNoSecrets(t, "response", body)
}

func TestAnthropicStringSystem(t *testing.T) {
	s := startServer(t, Options{})
	_, body := post(t, s.URL()+"/v1/messages",
		`{"model":"m","system":"be brief","messages":[{"role":"user","content":"hi"}]}`)
	if !strings.Contains(body, "system prompt length: 8") {
		t.Errorf("string system not counted: %s", body)
	}
}

func TestRecordsBodiesAndNumbers(t *testing.T) {
	s := startServer(t, Options{})
	a := fmt.Sprintf(openAIBody, false)
	b := fmt.Sprintf(anthropicBody, false)
	post(t, s.URL()+"/v1/chat/completions", a)
	post(t, s.URL()+"/v1/messages", b)
	got := s.Requests()
	if len(got) != 2 || string(got[0]) != a || string(got[1]) != b {
		t.Fatalf("recorded %d bodies: %q", len(got), got)
	}
	_, body := post(t, s.URL()+"/v1/messages", b)
	if !strings.Contains(body, "request: 3") {
		t.Errorf("request counter not advanced: %s", body)
	}
}

func TestDumpOmitsCredentials(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "dump")
	s := startServer(t, Options{DumpDir: dir})
	post(t, s.URL()+"/v1/chat/completions", fmt.Sprintf(openAIBody, false))
	post(t, s.URL()+"/v1/messages", fmt.Sprintf(anthropicBody, true))

	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil || len(files) != 2 {
		t.Fatalf("dump files %v (err %v)", files, err)
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		assertNoSecrets(t, f, string(data))
		var d struct {
			Number  int               `json:"number"`
			Path    string            `json:"path"`
			Headers map[string]string `json:"headers"`
			Body    json.RawMessage   `json:"body"`
		}
		if err := json.Unmarshal(data, &d); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if d.Number == 0 || d.Path == "" || len(d.Body) == 0 || d.Headers["X-Custom"] != "visible-header" {
			t.Errorf("%s incomplete: %s", f, data)
		}
	}
}

func TestConcurrentRequests(t *testing.T) {
	dir := t.TempDir()
	s := startServer(t, Options{DumpDir: dir})
	const n = 40
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			url, body := "/v1/chat/completions", fmt.Sprintf(openAIBody, i%2 == 0)
			if i%3 == 0 {
				url, body = "/v1/messages", fmt.Sprintf(anthropicBody, i%2 == 0)
			}
			post(t, s.URL()+url, body)
			_ = s.Requests()
		}()
	}
	wg.Wait()
	if got := len(s.Requests()); got != n {
		t.Fatalf("recorded %d, want %d", got, n)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	if len(files) != n {
		t.Fatalf("dumped %d, want %d", len(files), n)
	}
}

func TestListensOnLoopbackOnly(t *testing.T) {
	s := startServer(t, Options{})
	host, _, err := net.SplitHostPort(strings.TrimPrefix(s.URL(), "http://"))
	if err != nil || host != "127.0.0.1" {
		t.Fatalf("url %q host %q err %v", s.URL(), host, err)
	}
	if err := New(Options{Addr: "0.0.0.0:0"}).Start(); err == nil {
		t.Fatal("non-loopback address accepted")
	}
}

func TestUnknownRouteAndBadBody(t *testing.T) {
	s := startServer(t, Options{})
	if resp, _ := post(t, s.URL()+"/nope", "{}"); resp.StatusCode != 404 {
		t.Errorf("unknown route: %d", resp.StatusCode)
	}
	if resp, _ := post(t, s.URL()+"/v1/messages", "not json"); resp.StatusCode != 400 {
		t.Errorf("bad body: %d", resp.StatusCode)
	}
}
