package intent_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tngtech/oh-my-agentic-coder/internal/facade"
	"github.com/tngtech/oh-my-agentic-coder/internal/intent"
)

// TestIntentRoundTripOverHTTP starts a real facade on loopback,
// POSTs an intent, then looks it up via LookupOverHTTP — testing the
// cross-process wiring the sandbox child uses.
func TestIntentRoundTripOverHTTP(t *testing.T) {
	reg := intent.New(time.Minute)
	t.Cleanup(reg.Close)

	f := facade.New("", "127.0.0.1:0", nil, 1<<20, 0, "", "test")
	f.IntentRegistry = reg
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := f.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	baseURL := "http://127.0.0.1:" + strconv.Itoa(f.TCPPort()) + "/"

	// POST an intent (simulating the agent).
	postBody := bytes.NewReader([]byte(`{"target":"api.example.com","reason":"fetch release notes"}`))
	resp, err := http.Post(baseURL+"sandbox/intent", "application/json", postBody)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("POST status = %d; want 204", resp.StatusCode)
	}

	// LookupOverHTTP (simulating the netproxy prompter).
	reason, ok := intent.LookupOverHTTP(baseURL, "api.example.com")
	if !ok {
		t.Fatal("LookupOverHTTP returned false; want true")
	}
	if reason != "fetch release notes" {
		t.Errorf("reason = %q; want 'fetch release notes'", reason)
	}
}

// TestExplainMoreRoundTripOverHTTP exercises the full cross-process
// "Explain more" path: the prompter (sandbox child) marks the host via
// MarkExplainMoreOverHTTP, then the agent's out-of-band GET lookup sees the
// explain-more hint. This is the channel that replaces the HTTPS-undeliverable
// deny body.
func TestExplainMoreRoundTripOverHTTP(t *testing.T) {
	reg := intent.New(time.Minute)
	t.Cleanup(reg.Close)

	f := facade.New("", "127.0.0.1:0", nil, 1<<20, 0, "", "test")
	f.IntentRegistry = reg
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := f.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	baseURL := "http://127.0.0.1:" + strconv.Itoa(f.TCPPort()) + "/"

	// Prompter records the click over HTTP (fire-and-forget).
	intent.MarkExplainMoreOverHTTP(baseURL, "api.example.com")

	// The agent's GET lookup must now see the explain-more re-ask.
	get := func() (int, string) {
		t.Helper()
		resp, err := http.Get(baseURL + "sandbox/intent?target=api.example.com")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		buf := new(strings.Builder)
		if _, err := io.Copy(buf, resp.Body); err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, buf.String()
	}

	code, body := get()
	if code != http.StatusNotFound {
		t.Fatalf("GET status = %d; want 404 (undeclared)", code)
	}
	if !strings.Contains(body, "Explain more") {
		t.Errorf("hint should surface the explain-more re-ask: %q", body)
	}
	// One-shot: a second GET no longer carries the explain-more hint.
	if _, body2 := get(); strings.Contains(body2, "Explain more") {
		t.Errorf("explain-more hint should be consumed after one read: %q", body2)
	}
}

// TestIntentLookupMissOverHTTP verifies LookupOverHTTP returns false
// for a target with no declared intent.
func TestIntentLookupMissOverHTTP(t *testing.T) {
	reg := intent.New(time.Minute)
	t.Cleanup(reg.Close)

	f := facade.New("", "127.0.0.1:0", nil, 1<<20, 0, "", "test")
	f.IntentRegistry = reg
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := f.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	baseURL := "http://127.0.0.1:" + strconv.Itoa(f.TCPPort()) + "/"

	reason, ok := intent.LookupOverHTTP(baseURL, "unknown.example")
	if ok {
		t.Fatal("LookupOverHTTP returned true for unknown target")
	}
	if reason != "" {
		t.Errorf("reason = %q; want empty", reason)
	}
}

// TestIntentPathRoundTripOverHTTP verifies path targets work end-to-end
// (the learn-mode folder path).
func TestIntentPathRoundTripOverHTTP(t *testing.T) {
	reg := intent.New(time.Minute)
	t.Cleanup(reg.Close)

	f := facade.New("", "127.0.0.1:0", nil, 1<<20, 0, "", "test")
	f.IntentRegistry = reg
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := f.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	baseURL := "http://127.0.0.1:" + strconv.Itoa(f.TCPPort()) + "/"
	path := "/home/user/project"

	// POST a path intent.
	postBody := bytes.NewReader([]byte(`{"target":"` + path + `","reason":"read project config"}`))
	resp, err := http.Post(baseURL+"sandbox/intent", "application/json", postBody)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("POST status = %d; want 204", resp.StatusCode)
	}

	// LookupOverHTTP with the same path.
	reason, ok := intent.LookupOverHTTP(baseURL, path)
	if !ok {
		t.Fatal("LookupOverHTTP returned false for path target")
	}
	if reason != "read project config" {
		t.Errorf("reason = %q; want 'read project config'", reason)
	}
}

// TestIntentLookupEmptyBaseURL verifies LookupOverHTTP is safe when
// baseURL is empty (no facade configured — e.g. standalone sandbox run).
func TestIntentLookupEmptyBaseURL(t *testing.T) {
	reason, ok := intent.LookupOverHTTP("", "example.com")
	if ok || reason != "" {
		t.Errorf("LookupOverHTTP with empty baseURL should return false: %q %v", reason, ok)
	}
}

// TestIntentLookupUnreachableFacade verifies LookupOverHTTP returns
// false (not panics) when the facade is down.
func TestIntentLookupUnreachableFacade(t *testing.T) {
	reason, ok := intent.LookupOverHTTP("http://127.0.0.1:1/", "example.com")
	if ok || reason != "" {
		t.Errorf("LookupOverHTTP with unreachable facade should return false: %q %v", reason, ok)
	}
}

// TestIntentPOSTRejectsEmpty verifies the facade rejects empty
// target/reason over real HTTP.
func TestIntentPOSTRejectsEmpty(t *testing.T) {
	reg := intent.New(time.Minute)
	t.Cleanup(reg.Close)

	f := facade.New("", "127.0.0.1:0", nil, 1<<20, 0, "", "test")
	f.IntentRegistry = reg
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := f.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	baseURL := "http://127.0.0.1:" + strconv.Itoa(f.TCPPort()) + "/"
	resp, err := http.Post(baseURL+"sandbox/intent", "application/json",
		strings.NewReader(`{"target":"","reason":""}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("POST empty body status = %d; want 400", resp.StatusCode)
	}
}

// TestIntentSubtreeRoundTripOverHTTP declares intent for a specific file
// and looks it up via the subtree endpoint using a parent directory —
// the mismatch the folder learn-review must tolerate.
func TestIntentSubtreeRoundTripOverHTTP(t *testing.T) {
	reg := intent.New(time.Minute)
	t.Cleanup(reg.Close)

	f := facade.New("", "127.0.0.1:0", nil, 1<<20, 0, "", "test")
	f.IntentRegistry = reg
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := f.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	baseURL := "http://127.0.0.1:" + strconv.Itoa(f.TCPPort()) + "/"

	// Agent declares intent for a deep file.
	postBody := bytes.NewReader([]byte(`{"target":"/home/user/project/fixtures/big.json","reason":"load fixture data"}`))
	resp, err := http.Post(baseURL+"sandbox/intent", "application/json", postBody)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// Exact lookup of the reduced ancestor misses...
	if _, ok := intent.LookupOverHTTP(baseURL, "/home/user/project"); ok {
		t.Error("exact lookup of ancestor unexpectedly matched")
	}
	// ...but the subtree lookup finds it.
	reason, ok := intent.LookupSubtreeOverHTTP(baseURL, "/home/user/project")
	if !ok {
		t.Fatal("LookupSubtreeOverHTTP returned false for ancestor dir")
	}
	if reason != "load fixture data" {
		t.Errorf("reason = %q; want 'load fixture data'", reason)
	}
}
