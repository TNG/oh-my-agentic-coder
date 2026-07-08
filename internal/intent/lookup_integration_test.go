package intent_test

import (
	"bytes"
	"context"
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
