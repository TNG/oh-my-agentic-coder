package intent_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/facade"
	"github.com/TNG/oh-my-agentic-coder/internal/intent"
)

const testFacadeToken = "test-facade-token"

// startGateArmedFacade returns the baseURL of a facade on loopback with the
// TCP auth gate armed and OMAC_FACADE_TOKEN set, mirroring production wiring:
// runServe/runLaunch mint a token at startup and the sandbox child receives
// it via the env var the intent lookups authenticate with.
func startGateArmedFacade(t *testing.T, reg *intent.Registry) string {
	t.Helper()
	f := facade.New("", "127.0.0.1:0", nil, 1<<20, 0, "", "test")
	f.IntentRegistry = reg
	f.FacadeToken = testFacadeToken
	t.Setenv("OMAC_FACADE_TOKEN", testFacadeToken)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := f.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })

	baseURL := "http://127.0.0.1:" + strconv.Itoa(f.TCPPort()) + "/"

	// Guard the arming itself: a tokenless request must be rejected, so a
	// regression that drops the header from the intent lookups (or from this
	// helper) shows up as a failed round trip below rather than silently
	// passing through a gateless facade.
	resp, err := http.Get(baseURL + "sandbox/intent?target=gate-probe.example")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("tokenless TCP request status = %d; want 401 (gate not armed)", resp.StatusCode)
	}
	return baseURL
}

// agentDo makes an HTTP request the way the sandboxed agent does, carrying
// the facade token from $OMAC_FACADE_TOKEN as the brief instructs.
func agentDo(t *testing.T, method, url string, body io.Reader) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Omac-Facade-Token", os.Getenv("OMAC_FACADE_TOKEN"))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func postIntent(t *testing.T, baseURL, target, reason string) {
	t.Helper()
	body := bytes.NewReader([]byte(`{"target":"` + target + `","reason":"` + reason + `"}`))
	resp := agentDo(t, http.MethodPost, baseURL+"sandbox/intent", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("POST status = %d; want 204", resp.StatusCode)
	}
}

// TestIntentRoundTripOverHTTP starts a real facade on loopback with the gate
// armed, POSTs an intent, then looks it up via LookupOverHTTP. The lookup
// must authenticate like the sandbox child does in production; with the gate
// armed this fails if the header is ever dropped.
func TestIntentRoundTripOverHTTP(t *testing.T) {
	reg := intent.New(time.Minute)
	t.Cleanup(reg.Close)
	baseURL := startGateArmedFacade(t, reg)

	postIntent(t, baseURL, "api.example.com", "fetch release notes")

	reason, ok := intent.LookupOverHTTP(baseURL, "api.example.com")
	if !ok {
		t.Fatal("LookupOverHTTP returned false; want true")
	}
	if reason != "fetch release notes" {
		t.Errorf("reason = %q; want 'fetch release notes'", reason)
	}
}

// TestExplainMoreRoundTripOverHTTP exercises the full cross-process
// "Explain more" path with the gate armed: the prompter (sandbox child) marks
// the host via MarkExplainMoreOverHTTP, then the agent's out-of-band GET
// lookup sees the explain-more hint. MarkExplainMoreOverHTTP must
// authenticate or the click is silently lost (it is fire-and-forget).
func TestExplainMoreRoundTripOverHTTP(t *testing.T) {
	reg := intent.New(time.Minute)
	t.Cleanup(reg.Close)
	baseURL := startGateArmedFacade(t, reg)

	// Prompter records the click over HTTP (fire-and-forget).
	intent.MarkExplainMoreOverHTTP(baseURL, "api.example.com")

	// The agent's GET lookup must now see the explain-more re-ask.
	get := func() (int, string) {
		t.Helper()
		resp := agentDo(t, http.MethodGet, baseURL+"sandbox/intent?target=api.example.com", nil)
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
// for a target with no declared intent, with the gate armed.
func TestIntentLookupMissOverHTTP(t *testing.T) {
	reg := intent.New(time.Minute)
	t.Cleanup(reg.Close)
	baseURL := startGateArmedFacade(t, reg)

	reason, ok := intent.LookupOverHTTP(baseURL, "unknown.example")
	if ok {
		t.Fatal("LookupOverHTTP returned true for unknown target")
	}
	if reason != "" {
		t.Errorf("reason = %q; want empty", reason)
	}
}

// TestIntentPathRoundTripOverHTTP verifies path targets work end-to-end
// (the learn-mode folder path), with the gate armed.
func TestIntentPathRoundTripOverHTTP(t *testing.T) {
	reg := intent.New(time.Minute)
	t.Cleanup(reg.Close)
	baseURL := startGateArmedFacade(t, reg)
	path := "/home/user/project"

	postIntent(t, baseURL, path, "read project config")

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
// false (not panics) when the facade is down — including with a token in
// the env, as the sandbox child has in production.
func TestIntentLookupUnreachableFacade(t *testing.T) {
	t.Setenv("OMAC_FACADE_TOKEN", testFacadeToken)
	reason, ok := intent.LookupOverHTTP("http://127.0.0.1:1/", "example.com")
	if ok || reason != "" {
		t.Errorf("LookupOverHTTP with unreachable facade should return false: %q %v", reason, ok)
	}
}

// TestIntentPOSTRejectsEmpty verifies the facade rejects empty
// target/reason over real HTTP, with the gate armed.
func TestIntentPOSTRejectsEmpty(t *testing.T) {
	reg := intent.New(time.Minute)
	t.Cleanup(reg.Close)
	baseURL := startGateArmedFacade(t, reg)

	resp := agentDo(t, http.MethodPost, baseURL+"sandbox/intent",
		strings.NewReader(`{"target":"","reason":""}`))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("POST empty body status = %d; want 400", resp.StatusCode)
	}
}

// TestIntentSubtreeRoundTripOverHTTP declares intent for a specific file
// and looks it up via the subtree endpoint using a parent directory —
// the mismatch the folder learn-review must tolerate — with the gate armed.
func TestIntentSubtreeRoundTripOverHTTP(t *testing.T) {
	reg := intent.New(time.Minute)
	t.Cleanup(reg.Close)
	baseURL := startGateArmedFacade(t, reg)

	// Agent declares intent for a deep file.
	postIntent(t, baseURL, "/home/user/project/fixtures/big.json", "load fixture data")

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

// TestIntentLookupGatelessFacade verifies lookups still work when no facade
// token is minted and none is in the env: the header is omitted and the
// facade serves tokenless requests.
func TestIntentLookupGatelessFacade(t *testing.T) {
	reg := intent.New(time.Minute)
	t.Cleanup(reg.Close)

	f := facade.New("", "127.0.0.1:0", nil, 1<<20, 0, "", "test")
	f.IntentRegistry = reg
	t.Setenv("OMAC_FACADE_TOKEN", "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := f.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	baseURL := "http://127.0.0.1:" + strconv.Itoa(f.TCPPort()) + "/"

	postIntent(t, baseURL, "api.example.com", "fetch release notes")

	reason, ok := intent.LookupOverHTTP(baseURL, "api.example.com")
	if !ok || reason != "fetch release notes" {
		t.Errorf("gateless lookup = (%q, %v); want ('fetch release notes', true)", reason, ok)
	}
}
