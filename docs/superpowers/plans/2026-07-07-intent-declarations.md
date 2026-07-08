# Intent declarations for sandbox prompts — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Surface the agent's declared intent (why it needs access, what it expects to find) in network permission popups and session-end folder reviews, via a new in-supervisor intent registry and facade endpoint.

**Architecture:** One new in-memory `intent.Registry` (process-local, TTL-evicted) wired through three surfaces: a `POST /sandbox/intent` facade endpoint (agent writes intents), an enriched network popup (user reads intents + "Explain more" button denies with a marker pointing the agent back to the endpoint), and learn-mode folder review (intents shown next to candidates). The sandbox brief instructs the agent to pre-declare.

**Tech Stack:** Go 1.22+, standard library only. Existing patterns: `netproxy.Filter`, `netprompt.Prompter`, `facade.Facade`, `sandboxdeny.Text`.

**Spec:** `docs/superpowers/specs/2026-07-07-intent-declarations-design.md`

---

## File Structure

**Create:**
- `internal/intent/registry.go` — thread-safe intent store (in-memory, TTL-evicted).
- `internal/intent/registry_test.go` — unit tests.

**Modify:**
- `internal/facade/facade.go` — add `IntentRegistry` field + `POST /sandbox/intent` route.
- `internal/facade/sandbox_denied_test.go` (or new `intent_endpoint_test.go`) — endpoint tests.
- `internal/netprompt/prompt.go` — `lookupIntent` field, `promptText` gains intent + urlPath, 7th option "Explain more", new `tokenNeedsIntent`.
- `internal/netprompt/prompt_test.go` — update `promptText` signature usages, add intent + needs_intent tests.
- `internal/netproxy/filter.go` — `PromptResult.NeedsIntent`, verdict reason `prompt:needs_intent`.
- `internal/netproxy/filter_test.go` — needs_intent verdict test.
- `internal/netproxy/server.go` — `denyBody` branches on needs_intent.
- `internal/netproxy/server_test.go` — needs_intent deny body test.
- `internal/sandboxdeny/deny.go` — append intent hint to `Default().MarkerFile` + `FacadeNote`.
- `internal/sandboxdeny/deny_test.go` — assert hint present.
- `internal/sandboxrun/run.go` — construct registry, wire into facade + prompter + learnRecorder.
- `internal/sandboxrun/learn.go` — `learnRecorder.intentReg`, `candidatesWithIntent`, `OfferLearnedFolders` prints intents.
- `internal/sandboxrun/learn_test.go` — extend `testRecorder` signature, add intent-printing test.
- `internal/sandboxbrief/brief.md` — new bullet + extend Network bullet.

---

### Task 1: Intent registry package

**Files:**
- Create: `internal/intent/registry.go`
- Create: `internal/intent/registry_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/intent/registry_test.go`:

```go
package intent

import (
	"path/filepath"
	"testing"
	"time"
)

func TestRegistryRecordLookup(t *testing.T) {
	r := New(time.Minute, nil)
	r.Record("example.com", "fetch release notes")
	e, ok := r.Lookup("example.com")
	if !ok {
		t.Fatal("entry not found")
	}
	if e.Reason != "fetch release notes" {
		t.Errorf("reason = %q", e.Reason)
	}
}

func TestRegistryHostLowercase(t *testing.T) {
	r := New(time.Minute, nil)
	r.Record("EXAMPLE.com", "x")
	if _, ok := r.Lookup("example.com"); !ok {
		t.Error("lowercase lookup failed")
	}
	if _, ok := r.LookupHost("EXAMPLE.COM"); !ok {
		t.Error("LookupHost should lowercase")
	}
}

func TestRegistryPathNormalization(t *testing.T) {
	r := New(time.Minute, nil)
	abs, _ := filepath.Abs("/tmp/./foo")
	r.Record("/tmp/./foo", "read config")
	e, ok := r.Lookup(abs)
	if !ok {
		t.Fatalf("normalized lookup failed; want %q", abs)
	}
	if e.Reason != "read config" {
		t.Errorf("reason = %q", e.Reason)
	}
}

func TestRegistryTTLExpiry(t *testing.T) {
	r := New(20*time.Millisecond, nil)
	r.Record("ephemeral.example", "short-lived")
	if _, ok := r.Lookup("ephemeral.example"); !ok {
		t.Fatal("entry missing before TTL")
	}
	time.Sleep(40 * time.Millisecond)
	if _, ok := r.Lookup("ephemeral.example"); ok {
		t.Error("entry survived past TTL")
	}
}

func TestRegistryOverwrite(t *testing.T) {
	r := New(time.Minute, nil)
	r.Record("dup.example", "first")
	r.Record("dup.example", "second")
	e, ok := r.Lookup("dup.example")
	if !ok || e.Reason != "second" {
		t.Errorf("overwrite failed: %+v", e)
	}
}

func TestRegistryNilSafe(t *testing.T) {
	var r *Registry
	r.Record("anything", "no-op")
	if _, ok := r.Lookup("anything"); ok {
		t.Error("nil registry should return false")
	}
	if _, ok := r.LookupHost("anything"); ok {
		t.Error("nil registry LookupHost should return false")
	}
}

func TestRegistryEmptyReasonIgnored(t *testing.T) {
	r := New(time.Minute, nil)
	r.Record("empty.example", "")
	if _, ok := r.Lookup("empty.example"); ok {
		t.Error("empty reason should not record")
	}
}

func TestRegistryCloseStopsSweeper(t *testing.T) {
	r := New(time.Minute, nil)
	r.Close()
	// No panic; sweeper stopped. Record after close is still safe
	// (map stays usable; sweeper just no longer runs).
	r.Record("after.example", "x")
	if _, ok := r.Lookup("after.example"); !ok {
		t.Error("record after close should still work")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/intent/ -run TestRegistry -v`
Expected: FAIL (package does not exist / types undefined).

- [ ] **Step 3: Write minimal implementation**

Create `internal/intent/registry.go`:

```go
// Package intent holds an in-memory, TTL-evicted registry of agent-declared
// intents: why the agent wants to reach a given host or path. The agent
// writes intents via the facade POST /sandbox/intent endpoint; the network
// popup and learn-mode review read them in-process to show the user the
// agent's reason before deciding access.
package intent

import (
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Entry is one recorded intent.
type Entry struct {
	Target string    // host (lowercased) or absolute path (cleaned)
	Reason string
	Time   time.Time
}

// Registry is a thread-safe, TTL-evicted intent store. A nil *Registry
// is safe to call: Record is a no-op, all lookups return false.
type Registry struct {
	mu      sync.Mutex
	entries map[string]Entry
	ttl     time.Duration
	logf    func(string, ...any)
	stop    chan struct{}
	stopped sync.Once
}

// New builds a Registry with the given TTL. logf receives debug lines
// (nil = discard). Starts a background sweeper that evicts expired
// entries every ttl/2. Call Close to stop the sweeper.
func New(ttl time.Duration, logf func(string, ...any)) *Registry {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	r := &Registry{
		entries: map[string]Entry{},
		ttl:     ttl,
		logf:    logf,
		stop:    make(chan struct{}),
	}
	go r.sweep()
	return r
}

// Close stops the background sweeper. Safe to call multiple times.
// Records and lookups still work after Close (the map stays usable).
func (r *Registry) Close() {
	if r == nil {
		return
	}
	r.stopped.Do(func() { close(r.stop) })
}

// Record stores (or overwrites) an intent for target. target is
// normalized: hosts are lowercased, paths are cleaned+absolutized.
// Empty reason is ignored (no entry written).
func (r *Registry) Record(target, reason string) {
	if r == nil {
		return
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return
	}
	target = normalize(target)
	if target == "" {
		return
	}
	r.mu.Lock()
	r.entries[target] = Entry{Target: target, Reason: reason, Time: time.Now()}
	r.mu.Unlock()
}

// Lookup returns the entry for target (normalized the same way as
// Record). Returns ok=false if missing or expired (expired entries are
// lazily deleted).
func (r *Registry) Lookup(target string) (Entry, bool) {
	if r == nil {
		return Entry{}, false
	}
	target = normalize(target)
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[target]
	if !ok {
		return Entry{}, false
	}
	if r.ttl > 0 && time.Since(e.Time) > r.ttl {
		delete(r.entries, target)
		return Entry{}, false
	}
	return e, true
}

// LookupHost lowercases host and delegates to Lookup. Convenience for
// the netproxy layer which deals in hosts.
func (r *Registry) LookupHost(host string) (Entry, bool) {
	if r == nil {
		return Entry{}, false
	}
	return r.Lookup(strings.ToLower(host))
}

// normalize lowercases hosts; cleans + absolutizes paths. A target
// containing a path separator (or starting with / or ~) is treated as
// a path; everything else as a host.
func normalize(target string) string {
	target = strings.TrimSpace(target)
	if target == "" {
		return ""
	}
	if strings.ContainsRune(target, filepath.Separator) ||
		strings.HasPrefix(target, "~") ||
		filepath.IsAbs(target) {
		abs, err := filepath.Abs(filepath.Clean(expandTilde(target)))
		if err != nil {
			return ""
		}
		return abs
	}
	return strings.ToLower(target)
}

// expandTilde replaces a leading ~ with HOME so filepath.Abs can resolve.
func expandTilde(p string) string {
	if strings.HasPrefix(p, "~") {
		if home, err := userHome(); err == nil {
			if p == "~" {
				return home
			}
			if strings.HasPrefix(p, "~/") || strings.HasPrefix(p, "~\\") {
				return filepath.Join(home, p[2:])
			}
		}
	}
	return p
}

// userHome is a seam for tests (they set HOME via t.Setenv).
func userHome() (string, error) {
	return osUserHome()
}

func (r *Registry) sweep() {
	if r == nil || r.ttl <= 0 {
		return
	}
	interval := r.ttl / 2
	if interval < time.Second {
		interval = time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-r.stop:
			return
		case <-t.C:
			r.evictExpired()
		}
	}
}

func (r *Registry) evictExpired() {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	for k, e := range r.entries {
		if r.ttl > 0 && now.Sub(e.Time) > r.ttl {
			delete(r.entries, k)
		}
	}
}
```

Also add a tiny helper file `internal/intent/home.go`:

```go
package intent

import "os"

func osUserHome() (string, error) { return os.UserHomeDir() }
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/intent/ -run TestRegistry -v`
Expected: PASS for all registry tests.

- [ ] **Step 5: Run gofmt + go vet**

Run: `gofmt -w internal/intent/ && go vet ./internal/intent/`
Expected: clean.

- [ ] **Step 6: Commit**

```bash
git add internal/intent/
git commit -s -m "feat(intent): add in-memory intent registry

Thread-safe, TTL-evicted store for agent-declared access intents.
Host targets lowercased; path targets cleaned+absolutized. Nil-safe.

Part of issue #41."
```

---

### Task 2: Facade endpoint POST /sandbox/intent

**Files:**
- Modify: `internal/facade/facade.go` (Facade struct field, handle route, handler)
- Create: `internal/facade/intent_endpoint_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/facade/intent_endpoint_test.go`:

```go
package facade

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tngtech/oh-my-agentic-coder/internal/intent"
)

func TestIntentEndpointRecords(t *testing.T) {
	reg := intent.New(time.Minute, nil)
	t.Cleanup(reg.Close)
	f := &Facade{IntentRegistry: reg}

	body := bytes.NewReader([]byte(`{"target":"example.com","reason":"fetch release notes"}`))
	req := httptest.NewRequest(http.MethodPost, "/sandbox/intent", body)
	w := httptest.NewRecorder()
	f.handleSandboxIntent(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d; want 204", w.Code)
	}
	e, ok := reg.Lookup("example.com")
	if !ok || e.Reason != "fetch release notes" {
		t.Errorf("registry = %+v", e)
	}
}

func TestIntentEndpointRejectsEmptyBody(t *testing.T) {
	reg := intent.New(time.Minute, nil)
	t.Cleanup(reg.Close)
	f := &Facade{IntentRegistry: reg}

	req := httptest.NewRequest(http.MethodPost, "/sandbox/intent", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	f.handleSandboxIntent(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "target") || !strings.Contains(w.Body.String(), "reason") {
		t.Errorf("body should name required fields: %q", w.Body.String())
	}
}

func TestIntentEndpointNilRegistry(t *testing.T) {
	f := &Facade{}
	req := httptest.NewRequest(http.MethodPost, "/sandbox/intent",
		bytes.NewReader([]byte(`{"target":"x","reason":"y"}`)))
	w := httptest.NewRecorder()
	f.handleSandboxIntent(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d; want 503", w.Code)
	}
}

func TestIntentEndpointGetNotAllowed(t *testing.T) {
	f := &Facade{IntentRegistry: intent.New(time.Minute, nil)}
	req := httptest.NewRequest(http.MethodGet, "/sandbox/intent", nil)
	w := httptest.NewRecorder()
	f.handleSandboxIntent(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d; want 405", w.Code)
	}
}

func TestIntentEndpointMalformedJSON(t *testing.T) {
	reg := intent.New(time.Minute, nil)
	t.Cleanup(reg.Close)
	f := &Facade{IntentRegistry: reg}
	req := httptest.NewRequest(http.MethodPost, "/sandbox/intent",
		strings.NewReader(`{not json`))
	w := httptest.NewRecorder()
	f.handleSandboxIntent(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400", w.Code)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/facade/ -run TestIntentEndpoint -v`
Expected: FAIL (`IntentRegistry` field undefined, `handleSandboxIntent` undefined).

- [ ] **Step 3: Add the Facade field and route**

Edit `internal/facade/facade.go`:

In the `Facade` struct (after `DenialNote string` around line 138), add:

```go
	// IntentRegistry records agent-declared access intents from
	// POST /sandbox/intent. nil disables the endpoint (returns 503).
	IntentRegistry *intent.Registry
```

Add the import `"github.com/tngtech/oh-my-agentic-coder/internal/intent"` to the import block.

In `handle` (around line 342, after the `/sandbox/denied` block), add:

```go
	if r.URL.Path == "/sandbox/intent" {
		f.handleSandboxIntent(w, r)
		return
	}
```

- [ ] **Step 4: Add the handler**

Append to `internal/facade/facade.go` (after `handleSandboxDenied`):

```go
// handleSandboxIntent records an agent-declared intent: why the agent
// wants to reach a host or path. The registry is read in-process by
// the network popup and learn-mode review; there is no GET endpoint.
func (f *Facade) handleSandboxIntent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "omac: method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if f.IntentRegistry == nil {
		w.Header().Set("X-Omac-Reason", "intent-endpoint-disabled")
		http.Error(w, "omac: intent registry not configured", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		Target string `json:"target"`
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "omac: malformed JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(body.Target) == "" || strings.TrimSpace(body.Reason) == "" {
		http.Error(w, "omac: both \"target\" and \"reason\" are required", http.StatusBadRequest)
		return
	}
	f.IntentRegistry.Record(body.Target, body.Reason)
	w.WriteHeader(http.StatusNoContent)
}
```

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./internal/facade/ -run TestIntentEndpoint -v`
Expected: PASS.

- [ ] **Step 6: Run full facade package tests + vet**

Run: `go test ./internal/facade/ -v && go vet ./internal/facade/`
Expected: all pass.

- [ ] **Step 7: Commit**

```bash
git add internal/facade/
git commit -s -m "feat(facade): add POST /sandbox/intent endpoint

Records agent-declared access intents into the in-process registry.
204 on success, 400 on empty/malformed body, 503 when registry not
configured, 405 on non-POST.

Part of issue #41."
```

---

### Task 3: Sandbox brief update

**Files:**
- Modify: `internal/sandboxbrief/brief.md`

- [ ] **Step 1: Extend the Network bullet**

Edit `internal/sandboxbrief/brief.md`. Find the line beginning `- **Network:**` and append after "the answer is remembered.":

```
 If you declared an intent, the user sees it; if not, the dialog says so and offers an "Explain more" button that denies and asks you to elaborate.
```

- [ ] **Step 2: Add the Intent bullet**

After the `- **Capabilities:**` bullet (the last bullet in the file), append:

```
- **Intent:** before contacting a new external host, or before accessing a path outside your granted directories, declare why: `POST $OMAC_BASE/sandbox/intent` with JSON `{"target":"<host or absolute path>","reason":"<one sentence: why you need it and what you expect to find>"}`. The user sees your reason in the approval popup (network) or the session-end review (folders). A request with no intent on file still works — it just pops up a less informative dialog, and the user can click "Explain more" to send you back for a reason before deciding.
```

- [ ] **Step 3: Verify brief embeds unchanged**

Run: `go test ./internal/sandboxbrief/ -v 2>&1 | head -20` (if a test exists that embeds the brief; if not, `go build ./...`).

Expected: brief still compiles into whatever embed consumes it.

- [ ] **Step 4: Commit**

```bash
git add internal/sandboxbrief/brief.md
git commit -s -m "docs(brief): instruct agent to pre-declare access intent

Adds Intent bullet telling the agent to POST /sandbox/intent before
contacting a new host or touching a path outside granted dirs. Extends
Network bullet to mention the Explain-more button.

Part of issue #41."
```

---

### Task 4: Network popup enrichment — promptText + option labels

**Files:**
- Modify: `internal/netprompt/prompt.go`
- Modify: `internal/netprompt/prompt_test.go`

- [ ] **Step 1: Write the failing tests**

Edit `internal/netprompt/prompt_test.go`. Replace `TestPromptTextParity` (lines 92-102) with:

```go
func TestPromptTextParity(t *testing.T) {
	got := promptText("api.example.com", 443, "", "")
	want := "The sandboxed process is trying to reach:\n\n    api.example.com:443\n\nAgent intent: (not declared)\n\nHow should omac handle this destination?"
	if got != want {
		t.Errorf("promptText (no intent) = %q", got)
	}
}

func TestPromptTextWithURLAndIntent(t *testing.T) {
	got := promptText("api.example.com", 443, "/v1/releases", "fetch release notes")
	want := "The sandboxed process is trying to reach:\n\n    https://api.example.com:443/v1/releases\n\nAgent intent: \"fetch release notes\"\n\nHow should omac handle this destination?"
	if got != want {
		t.Errorf("promptText (with intent) = %q", got)
	}
}

func TestPromptTextIntentOnly(t *testing.T) {
	got := promptText("api.example.com", 443, "", "verify the version")
	if !strings.Contains(got, "api.example.com:443") {
		t.Errorf("missing host:port: %q", got)
	}
	if !strings.Contains(got, `"verify the version"`) {
		t.Errorf("missing intent: %q", got)
	}
	if strings.Contains(got, "https://") {
		t.Errorf("should not show https when no path: %q", got)
	}
}
```

Update `TestOptionLabelsExactAndDefault` (lines 72-90) — the expected list gains a 7th entry:

```go
func TestOptionLabelsExactAndDefault(t *testing.T) {
	opts := optionLabels("example.com")
	want := []string{
		"Allow once",
		"Allow permanently (this host)",
		"Allow permanently (*.example.com)",
		"Deny once",
		"Deny permanently (this host)",
		"Deny permanently (*.example.com)",
		"Explain more",
	}
	if len(opts) != len(want) {
		t.Fatalf("got %d options", len(opts))
	}
	for i := range want {
		if opts[i] != want[i] {
			t.Errorf("option[%d] = %q, want %q", i, opts[i], want[i])
		}
	}
}
```

Add to `TestLabelTokenRoundTrip` (around line 31) a case for Explain more:

```go
		"Explain more": tokenNeedsIntent,
```

Add to `TestTokenToResult` (around line 48) a case for needs_intent:

```go
	r = tokenToResult(tokenNeedsIntent, host, suffix)
	if r.Allow || r.Persist || !r.NeedsIntent {
		t.Errorf("needs_intent: %+v", r)
	}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/netprompt/ -v`
Expected: FAIL (signature mismatch, `tokenNeedsIntent` undefined, `NeedsIntent` field missing).

- [ ] **Step 3: Update promptText + option labels**

Edit `internal/netprompt/prompt.go`:

Add `tokenNeedsIntent` to the const block (after `tokenDenyPermanentSuffix`, around line 22):

```go
	tokenNeedsIntent           = "needs_intent"
```

Update `optionLabels` (around line 27) to add the 7th option:

```go
func optionLabels(suffix string) []string {
	return []string{
		"Allow once",
		"Allow permanently (this host)",
		fmt.Sprintf("Allow permanently (*.%s)", suffix),
		"Deny once",
		"Deny permanently (this host)",
		fmt.Sprintf("Deny permanently (*.%s)", suffix),
		"Explain more",
	}
}
```

Update `labelToToken` (around line 39) — add the case before the `default`:

```go
	case "Explain more":
		return tokenNeedsIntent
```

Update `tokenToResult` (around line 57) — add the case before `default`:

```go
	case tokenNeedsIntent:
		return netproxy.PromptResult{Allow: false, NeedsIntent: true}
```

Update `promptText` (around line 89) — new signature with `urlPath` and `intent`:

```go
// promptText is the dialog body. urlPath is the request path when known
// (forward HTTP only; CONNECT can't see it). intent is the agent-declared
// reason; empty means "not declared".
func promptText(host string, port int, urlPath, intent string) string {
	target := fmt.Sprintf("%s:%d", host, port)
	if urlPath != "" {
		target = fmt.Sprintf("https://%s:%d%s", host, port, urlPath)
	}
	intentLine := "Agent intent: (not declared)"
	if intent != "" {
		intentLine = fmt.Sprintf("Agent intent: %q", intent)
	}
	return fmt.Sprintf("The sandboxed process is trying to reach:\n\n    %s\n\n%s\n\nHow should omac handle this destination?", target, intentLine)
}
```

- [ ] **Step 4: Update the Prompter struct + NewPrompter signature**

Edit `internal/netprompt/prompt.go`. Add a field to `Prompter` (around line 110):

```go
type Prompter struct {
	timeout      time.Duration
	backends     []dialogBackend
	notify       func(host string, port int)
	logf         func(format string, args ...any)
	lookupIntent func(host string) (string, bool)
}
```

Update `NewPrompter` to accept an intent lookup func (around line 119). Change the signature to:

```go
func NewPrompter(timeoutSecs int, logf func(string, ...any), lookupIntent func(host string) (string, bool)) (*Prompter, bool) {
```

In the body, after `p := &Prompter{...}`, set the field:

```go
	p.lookupIntent = lookupIntent
```

(nil is fine; `Prompt` treats nil as "no registry".)

- [ ] **Step 5: Update Prompt to pass intent + urlPath into promptText**

Edit `Prompt` (around line 149). The current call is `backend.show(ctx, host, port, suffix)`. We need the intent before calling `show`. Add before the suffix line:

```go
	intent := ""
	if p.lookupIntent != nil {
		if e, ok := p.lookupIntent(host); ok {
			intent = e
		}
	}
```

The backends' `show` signatures need updating to accept `urlPath` and `intent`. Update the `dialogBackend` interface (around line 99):

```go
type dialogBackend interface {
	name() string
	available() bool
	show(ctx context.Context, host string, port int, suffix, urlPath, intent string) (string, error)
}
```

Update the three implementations (`osascriptBackend.show`, `zenityBackend.show`, `kdialogBackend.show`) to accept `urlPath, intent string` and pass them to `promptText`:

- `osascriptBackend.show`: change signature to `func (osascriptBackend) show(ctx context.Context, host string, port int, suffix, urlPath, intent string) (string, error)` and replace `promptText(host, port)` with `promptText(host, port, urlPath, intent)`.
- `zenityBackend.show`: same signature change; replace `promptText(host, port)` with `promptText(host, port, urlPath, intent)`.
- `kdialogBackend.show`: same; and it currently builds its own text inline — replace that with `promptText(host, port, urlPath, intent)`.

Update the `backend.show` call in `Prompt` (around line 166):

```go
	label, err := backend.show(ctx, host, port, suffix, "", intent)
```

(The `""` is urlPath — the prompter does not see the URL path because CONNECT tunnels don't expose it. Forward-HTTP path enrichment is a future enhancement; the popup today always shows host:port for the target line, and the intent when present. The `urlPath` parameter is wired through so a future change can plumb the forward-HTTP path without touching every backend signature again.)

- [ ] **Step 6: Run tests to verify they pass**

Run: `go test ./internal/netprompt/ -v`
Expected: PASS.

- [ ] **Step 7: Fix any callers of NewPrompter**

The only production caller is `internal/sandboxrun/run.go` line 143. Update it to pass `nil` for now (Task 9 wires the real registry):

```go
		np, available := netprompt.NewPrompter(p.Network.PromptTimeoutSecs(), logf, nil)
```

Run: `go build ./...`
Expected: compiles.

- [ ] **Step 8: Commit**

```bash
git add internal/netprompt/ internal/sandboxrun/run.go
git commit -s -m "feat(netprompt): enrich popup with intent + Explain-more option

promptText gains urlPath and intent params. optionLabels gains a 7th
entry 'Explain more' → tokenNeedsIntent → PromptResult.NeedsIntent.
NewPrompter accepts a lookupIntent func (nil = no registry).

Part of issue #41."
```

---

### Task 5: PromptResult.NeedsIntent + filter verdict reason

**Files:**
- Modify: `internal/netproxy/filter.go`
- Modify: `internal/netproxy/filter_test.go`

- [ ] **Step 1: Write the failing test**

Edit `internal/netproxy/filter_test.go`. Add:

```go
func TestPromptNeedsIntentVerdictReason(t *testing.T) {
	p := &fakePrompter{res: PromptResult{NeedsIntent: true}}
	f := NewFilter(FilterConfig{
		PromptEnabled: true,
		Prompter:      p,
		Resolve:       staticResolver("93.184.216.34"),
	})
	v, _ := f.Check(context.Background(), "api.example.com", 443)
	if v.Decision != Deny {
		t.Fatalf("decision = %v; want Deny", v.Decision)
	}
	if v.Reason != "prompt:needs_intent" {
		t.Errorf("reason = %q; want prompt:needs_intent", v.Reason)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/netproxy/ -run TestPromptNeedsIntent -v`
Expected: FAIL (`NeedsIntent` field undefined; reason is `prompt:deny`).

- [ ] **Step 3: Add NeedsIntent to PromptResult**

Edit `internal/netproxy/filter.go`. In `PromptResult` (around line 55), add:

```go
	// NeedsIntent signals that the user clicked "Explain more" — the
	// request is denied with a marker pointing the agent at the intent
	// endpoint. Never persisted.
	NeedsIntent bool
```

- [ ] **Step 4: Update defaultDecision to set the verdict reason**

Edit `defaultDecision` (around line 228). Change the deny branch at the end:

```go
	if res.Allow {
		return Verdict{Allow, "prompt:allow"}, true
	}
	if res.NeedsIntent {
		return Verdict{Deny, "prompt:needs_intent"}, true
	}
	return Verdict{Deny, "prompt:deny"}, true
```

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./internal/netproxy/ -run TestPromptNeedsIntent -v`
Expected: PASS.

- [ ] **Step 6: Run full netproxy package tests**

Run: `go test ./internal/netproxy/ -v`
Expected: all pass.

- [ ] **Step 7: Commit**

```bash
git add internal/netproxy/filter.go internal/netproxy/filter_test.go
git commit -s -m "feat(netproxy): NeedsIntent field + prompt:needs_intent reason

PromptResult gains NeedsIntent (Explain-more signal). defaultDecision
returns verdict reason 'prompt:needs_intent' so the deny body can
point the agent at /sandbox/intent.

Part of issue #41."
```

---

### Task 6: Proxy deny body for needs_intent

**Files:**
- Modify: `internal/netproxy/server.go`
- Modify: `internal/netproxy/server_test.go`

- [ ] **Step 1: Write the failing test**

Edit `internal/netproxy/server_test.go`. Add (anywhere in the file):

```go
func TestDenyBodyNeedsIntent(t *testing.T) {
	body := denyBody("example.com", "prompt:needs_intent")
	if !strings.Contains(body, "DENIED") {
		t.Errorf("missing DENIED: %q", body)
	}
	if !strings.Contains(body, "/sandbox/intent") {
		t.Errorf("missing intent hint: %q", body)
	}
	if !strings.Contains(body, "example.com") {
		t.Errorf("missing host: %q", body)
	}
}

func TestDenyBodyRegularDeny(t *testing.T) {
	body := denyBody("example.com", "prompt:deny")
	if !strings.Contains(body, "DENIED BY THE SANDBOX") {
		t.Errorf("regular deny lost its wording: %q", body)
	}
	if strings.Contains(body, "/sandbox/intent") {
		t.Errorf("regular deny should not mention intent: %q", body)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/netproxy/ -run TestDenyBody -v`
Expected: FAIL (deny body doesn't branch on needs_intent).

- [ ] **Step 3: Update denyBody to branch on reason**

Edit `internal/netproxy/server.go`. Replace `denyBody` (lines 32-45) with:

```go
// denyBody renders the body for a filtered denial. It explicitly
// attributes the denial to the sandbox network policy and points at
// the knobs that change it. When reason indicates the user clicked
// "Explain more", the body directs the agent to declare or refine an
// intent via POST /sandbox/intent and retry.
func denyBody(host, reason string) string {
	if strings.Contains(reason, "needs_intent") {
		return fmt.Sprintf(`omac sandbox: access to %q was DENIED — the user asked for more explanation.

Declare or refine your intent via:
  POST $OMAC_BASE/sandbox/intent  {"target":%q,"reason":"..."}
then retry the request.
`, host, host)
	}
	return fmt.Sprintf(`omac sandbox: access to %q was DENIED BY THE SANDBOX network policy (%s).

This response comes from the omac sandbox proxy, not from %s.
The destination was never contacted.

To allow this host, either:
  - answer "Allow" in the network prompt (if enabled),
  - add it to network.allow_domain in your sandbox profile
    (~/.config/omac/sandbox-profiles/<profile>.json),
  - or remove a matching deny entry from network.deny_domain or the
    <profile>.pages.json learned-policy file.
`, host, reason, host)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/netproxy/ -run TestDenyBody -v`
Expected: PASS.

- [ ] **Step 5: Run full netproxy package tests**

Run: `go test ./internal/netproxy/ -v`
Expected: all pass.

- [ ] **Step 6: Commit**

```bash
git add internal/netproxy/server.go internal/netproxy/server_test.go
git commit -s -m "feat(netproxy): deny body points agent at /sandbox/intent on Explain-more

denyBody branches on reason: needs_intent → body directs the agent to
declare/refine intent and retry; other denies keep the existing
wording.

Part of issue #41."
```

---

### Task 7: Sandbox deny text gains intent hint

**Files:**
- Modify: `internal/sandboxdeny/deny.go`
- Modify: `internal/sandboxdeny/deny_test.go`

- [ ] **Step 1: Write the failing test**

Edit `internal/sandboxdeny/deny_test.go`. Add to `TestDefaultHasSentinel` (or as a new test):

```go
func TestDefaultMentionsIntent(t *testing.T) {
	d := Default()
	if !strings.Contains(d.MarkerFile, "/sandbox/intent") {
		t.Errorf("MarkerFile should mention /sandbox/intent: %q", d.MarkerFile)
	}
	if !strings.Contains(d.FacadeNote, "/sandbox/intent") {
		t.Errorf("FacadeNote should mention /sandbox/intent: %q", d.FacadeNote)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/sandboxdeny/ -run TestDefaultMentionsIntent -v`
Expected: FAIL.

- [ ] **Step 3: Append the intent hint to Default()**

Edit `internal/sandboxdeny/deny.go`. Replace `Default()` (lines 29-40) with:

```go
func Default() Text {
	return Text{
		MarkerFile: `X-Omac-Sandbox: denied
This path is protected by the omac sandbox policy and is intentionally
restricted. It is not missing and not a bug. Do not request access unless
the task explicitly requires this path and cannot proceed otherwise.

If you need this path for your task, declare why first:
  POST $OMAC_BASE/sandbox/intent  {"target":"<absolute path>","reason":"..."}
The user will see your reason when reviewing access.
`,
		MarkerDirName: ".omac-denied",
		FacadeNote: "Intentionally restricted by sandbox policy. Not missing, not a bug. " +
			"Escalate to the user only if the task cannot proceed without this path. " +
			"To declare why you need a path, POST $OMAC_BASE/sandbox/intent " +
			`{"target":"<absolute path>","reason":"..."}` +
			" — the user sees your reason when reviewing access.",
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/sandboxdeny/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/sandboxdeny/
git commit -s -m "feat(sandboxdeny): point agent at /sandbox/intent in deny text

Default MarkerFile and FacadeNote gain a trailing hint telling the
agent to declare an intent before touching a protected path. Works
whether or not the agent ever calls the endpoint.

Part of issue #41."
```

---

### Task 8: Learn-mode enrichment

**Files:**
- Modify: `internal/sandboxrun/learn.go`
- Modify: `internal/sandboxrun/learn_test.go`

- [ ] **Step 1: Write the failing test**

Edit `internal/sandboxrun/learn_test.go`. Update `testRecorder` (lines 27-39) to accept an optional registry. Replace the function with:

```go
func testRecorder(home string, g *Grants, reg *intent.Registry) *learnRecorder {
	r := &learnRecorder{
		seen:      map[string]bool{},
		stop:      make(chan struct{}),
		protected: g.ProtectedPaths,
		home:      home,
		intentReg: reg,
	}
	r.excluded = append(r.excluded, g.ReadPaths...)
	r.excluded = append(r.excluded, g.WritePaths...)
	r.excluded = append(r.excluded, g.AllowPaths...)
	r.excluded = append(r.excluded, g.Workdir, "/tmp-test")
	return r
}
```

Add the import `"github.com/tngtech/oh-my-agentic-coder/internal/intent"` and `"time"`.

Update existing callers in the test file: `testRecorder(home, learnGrants(t, home))` becomes `testRecorder(home, learnGrants(t, home), nil)`.

Add a new test:

```go
func TestOfferLearnedFoldersPrintsIntent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := t.TempDir()
	profilePath := filepath.Join(dir, "default.json")
	if err := sandboxprofile.WriteProfile(profilePath, &sandboxprofile.Profile{}); err != nil {
		t.Fatal(err)
	}
	newProj := filepath.Join(home, "newproj")
	reg := intent.New(time.Minute, nil)
	t.Cleanup(reg.Close)
	reg.Record(newProj, "read project config")

	var out strings.Builder
	if err := OfferLearnedFolders(profilePath, []string{newProj}, strings.NewReader("n\n"), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `agent said: "read project config"`) {
		t.Errorf("missing intent line: %q", out.String())
	}
}

func TestOfferLearnedFoldersNoIntentLabel(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := t.TempDir()
	profilePath := filepath.Join(dir, "default.json")
	if err := sandboxprofile.WriteProfile(profilePath, &sandboxprofile.Profile{}); err != nil {
		t.Fatal(err)
	}
	newProj := filepath.Join(home, "newproj")
	reg := intent.New(time.Minute, nil)
	t.Cleanup(reg.Close)

	var out strings.Builder
	if err := OfferLearnedFolders(profilePath, []string{newProj}, strings.NewReader("n\n"), &out, reg); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "(no intent declared)") {
		t.Errorf("missing no-intent label: %q", out.String())
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/sandboxrun/ -run TestOfferLearned -v`
Expected: FAIL (`intentReg` field undefined, `OfferLearnedFolders` signature mismatch).

- [ ] **Step 3: Add intentReg to learnRecorder**

Edit `internal/sandboxrun/learn.go`. Add a field to `learnRecorder` (around line 25):

```go
type learnRecorder struct {
	mu      sync.Mutex
	seen    map[string]bool
	stop    chan struct{}
	stopped sync.WaitGroup

	excluded   []string
	protected  []string
	home       string
	intentReg  *intent.Registry
}
```

Add the import `"github.com/tngtech/oh-my-agentic-coder/internal/intent"`.

Update `newLearnRecorder` (around line 37) to accept the registry:

```go
func newLearnRecorder(g *Grants, reg *intent.Registry) *learnRecorder {
	home, _ := os.UserHomeDir()
	r := &learnRecorder{
		seen:      map[string]bool{},
		stop:      make(chan struct{}),
		protected: g.ProtectedPaths,
		home:      home,
		intentReg: reg,
	}
	r.excluded = append(r.excluded, g.ReadPaths...)
	r.excluded = append(r.excluded, g.WritePaths...)
	r.excluded = append(r.excluded, g.AllowPaths...)
	r.excluded = append(r.excluded, g.Workdir)
	r.excluded = append(r.excluded,
		os.TempDir(), "/tmp", "/private/tmp", "/private/var/folders", "/var/folders")
	if home != "" {
		r.excluded = append(r.excluded,
			filepath.Join(home, ".local", "state", "omac"),
			filepath.Join(home, ".config", "omac"))
	}
	return r
}
```

- [ ] **Step 4: Update OfferLearnedFolders signature + body**

Edit `OfferLearnedFolders` (around line 192). Change the signature to accept a variadic / optional registry. To keep the existing call site in `run.go` working, use an optional trailing param:

```go
// OfferLearnedFolders presents the candidates on the terminal and asks
// whether to append them to the profile's filesystem.allow list. It
// rewrites the profile pretty-printed on confirmation. in/out default
// to the controlling terminal so the prompt works after a TUI session.
// reg, when non-nil, supplies agent-declared intents shown next to each
// candidate.
func OfferLearnedFolders(profilePath string, candidates []string, in io.Reader, out io.Writer, reg ...*intent.Registry) error {
	if len(candidates) == 0 {
		fmt.Fprintln(out, "omac sandbox: learn mode: no new folders observed")
		return nil
	}
	var registry *intent.Registry
	if len(reg) > 0 {
		registry = reg[0]
	}
	fmt.Fprintln(out, "\nomac sandbox: learn mode observed these folders outside the current profile:")
	for _, c := range candidates {
		intentLine := "(no intent declared)"
		if registry != nil {
			if e, ok := registry.Lookup(c); ok {
				intentLine = fmt.Sprintf("agent said: %q", e.Reason)
			}
		}
		fmt.Fprintf(out, "  %-40s — %s\n", c, intentLine)
	}
	fmt.Fprintf(out, "Add them to filesystem.allow in %s? [y/N] ", profilePath)
	reader := bufio.NewReader(in)
	answer, _ := reader.ReadString('\n')
	answer = strings.ToLower(strings.TrimSpace(answer))
	if answer != "y" && answer != "yes" {
		fmt.Fprintln(out, "omac sandbox: profile unchanged")
		return nil
	}
	data, err := os.ReadFile(profilePath)
	if err != nil {
		return fmt.Errorf("learn mode: read profile: %w", err)
	}
	profile, err := sandboxprofile.Parse(data)
	if err != nil {
		return fmt.Errorf("learn mode: %w", err)
	}
	existing := map[string]bool{}
	for _, a := range profile.Filesystem.Allow {
		existing[a] = true
	}
	added := 0
	for _, c := range candidates {
		entry := abbreviateHome(c)
		if existing[entry] {
			continue
		}
		profile.Filesystem.Allow = append(profile.Filesystem.Allow, entry)
		added++
	}
	if added == 0 {
		fmt.Fprintln(out, "omac sandbox: all folders already present; profile unchanged")
		return nil
	}
	if err := sandboxprofile.WriteProfile(profilePath, profile); err != nil {
		return fmt.Errorf("learn mode: write profile: %w", err)
	}
	fmt.Fprintf(out, "omac sandbox: added %d folder(s) to %s\n", added, profilePath)
	return nil
}
```

- [ ] **Step 5: Update run.go to pass the registry**

Edit `internal/sandboxrun/run.go`. In `Run`, after the `recorder = newLearnRecorder(grants)` line (around line 71), update the call. But the registry doesn't exist yet in `run.go` (Task 9 creates it). For now, pass `nil` so the build works:

```go
		recorder = newLearnRecorder(grants, nil)
```

And in the `OfferLearnedFolders` call (around line 120), leave it unchanged — the variadic param means it defaults to no registry:

```go
		if oerr := OfferLearnedFolders(profilePath, candidates, os.Stdin, stderr); oerr != nil {
```

(Task 9 will thread the real registry through both call sites.)

- [ ] **Step 6: Run tests to verify they pass**

Run: `go test ./internal/sandboxrun/ -v`
Expected: all pass.

- [ ] **Step 7: Run gofmt + vet**

Run: `gofmt -w internal/sandboxrun/ && go vet ./internal/sandboxrun/`
Expected: clean.

- [ ] **Step 8: Commit**

```bash
git add internal/sandboxrun/learn.go internal/sandboxrun/learn_test.go internal/sandboxrun/run.go
git commit -s -m "feat(learn): show agent intents next to learned folders

OfferLearnedFolders takes an optional intent.Registry; each candidate
prints 'agent said: \"...\"' or '(no intent declared)'. learnRecorder
gains an intentReg field wired through newLearnRecorder.

Part of issue #41."
```

---

### Task 9: Wire the registry through run.go

**Files:**
- Modify: `internal/sandboxrun/run.go`

- [ ] **Step 1: Construct the registry in Run**

Edit `internal/sandboxrun/run.go`. Add the import `"time"` and `"github.com/tngtech/oh-my-agentic-coder/internal/intent"`.

After `grants.DenialText = resolvedDenialText(merged.Denial)` (around line 62), add:

```go
	// Intent registry: in-memory, session-scoped. The agent writes via
	// POST $OMAC_BASE/sandbox/intent; the popup and learn-mode review
	// read it in-process. 10 min TTL — long enough to cover a prompt
	// round-trip, short enough to avoid stale intents lingering.
	intentReg := intent.New(10*time.Minute, logf)
	defer intentReg.Close()
```

- [ ] **Step 2: Pass the registry to the proxy/prompter**

The proxy is built in `buildProxy` (around line 130). Update `buildProxy` to accept the registry and thread it into `NewPrompter`:

Change the signature (around line 130):

```go
func buildProxy(p *sandboxprofile.Profile, profilePath string, stderr io.Writer, logf func(string, ...any), intentReg *intent.Registry) (*netproxy.Server, error) {
```

Update the `NewPrompter` call (around line 143). `lookupIntent` expects `(string, bool)` but `LookupHost` returns `(Entry, bool)`, so wrap it:

```go
		np, available := netprompt.NewPrompter(p.Network.PromptTimeoutSecs(), logf, func(host string) (string, bool) {
			if intentReg == nil {
				return "", false
			}
			e, ok := intentReg.LookupHost(host)
			return e.Reason, ok
		})
```

Update the call site in `Run` (around line 88):

```go
		proxy, err = buildProxy(merged, profilePath, diag.Writer(), logf, intentReg)
```

- [ ] **Step 3: Pass the registry to the learnRecorder**

Update the `newLearnRecorder` call (around line 71):

```go
		recorder = newLearnRecorder(grants, intentReg)
```

Update the `OfferLearnedFolders` call (around line 120):

```go
		if oerr := OfferLearnedFolders(profilePath, candidates, os.Stdin, stderr, intentReg); oerr != nil {
```

- [ ] **Step 4: Build and run all tests**

Run: `go build ./... && go test ./internal/sandboxrun/ ./internal/netprompt/ ./internal/netproxy/ ./internal/facade/ ./internal/intent/ ./internal/sandboxdeny/ -v`
Expected: all pass.

- [ ] **Step 5: Commit**

```bash
git add internal/sandboxrun/run.go
git commit -s -m "feat(sandboxrun): wire intent registry through supervisor

One registry per session, 10 min TTL. Passed to buildProxy (→
NewPrompter's lookupIntent) and to newLearnRecorder + OfferLearnedFolders.

Part of issue #41."
```

---

### Task 10: Wire the registry into the facade

The facade is constructed outside `run.go` — find where `Facade` is built and add `IntentRegistry`.

- [ ] **Step 1: Find the facade construction site**

Run: `rg "facade.New\(|&facade.Facade\{" --type go -n`

Expected: shows the construction sites. The main one is likely in `internal/cli/start.go` or `internal/cli/serve.go`.

- [ ] **Step 2: Thread the registry from Run to the facade**

This depends on where the facade is built. If the facade is built inside `Run` (it may be — check), add `IntentRegistry: intentReg` to the `Facade` struct literal. If it's built outside `Run` (in `start.go`/`serve.go`), the registry needs to be returned from `Run` or built alongside it.

Check the actual construction site and wire `IntentRegistry: intentReg` into the `Facade` literal. If the facade is built in a different function than `Run`, the simplest path is to construct the registry in the same function and pass it to both `Run` and the facade.

- [ ] **Step 3: Build and test**

Run: `go build ./... && go test ./...`
Expected: all pass.

- [ ] **Step 4: Commit**

```bash
git add -A
git commit -s -m "feat(facade): wire IntentRegistry into facade construction

The facade constructed alongside Run gets the session's intent registry
so POST /sandbox/intent records intents the popup can read.

Part of issue #41."
```

---

### Task 11: Final verification

- [ ] **Step 1: Run the full test suite**

Run: `go test ./...`
Expected: all pass.

- [ ] **Step 2: Run gofmt + vet on everything touched**

Run: `gofmt -l internal/intent/ internal/facade/ internal/netprompt/ internal/netproxy/ internal/sandboxdeny/ internal/sandboxrun/ internal/sandboxbrief/`
Expected: no output (all formatted).

Run: `go vet ./...`
Expected: clean.

- [ ] **Step 3: Verify the brief renders**

Run: `go build ./...`
Expected: compiles (the brief is embedded).

- [ ] **Step 4: Create the PR branch and push**

```bash
git checkout -b feat/intent-declarations
git push -u origin feat/intent-declarations
```

- [ ] **Step 5: Open the PR**

```bash
gh pr create --title "feat(sandbox): intent declarations for web/folder prompts" --body "Closes #41.

Surfaces the agent's declared intent (why it needs access, what it expects to find) in network permission popups and session-end folder reviews.

- New \`internal/intent\` package: in-memory, TTL-evicted registry.
- New facade endpoint \`POST /sandbox/intent\` for agents to declare intents.
- Network popup shows URL path + agent intent; new 'Explain more' button denies with a marker pointing the agent at the intent endpoint.
- Sandbox deny text and learn-mode folder review surface declared intents.
- Sandbox brief instructs the agent to pre-declare before contacting new hosts or touching paths outside granted dirs.

See \`docs/superpowers/specs/2026-07-07-intent-declarations-design.md\` for the full design."
```

---

## Self-Review

**Spec coverage:**
- §1 Intent registry → Task 1 ✓
- §2 Facade endpoint → Task 2 ✓
- §3 Brief update → Task 3 ✓
- §4 Network popup enrichment → Tasks 4, 5, 6 ✓
- §5 Folder deny text + learn-mode enrichment → Tasks 7, 8 ✓
- Wiring → Tasks 9, 10 ✓
- Testing (all 7 test groups from spec) → embedded in Tasks 1, 2, 4, 5, 6, 7, 8 ✓

**Placeholder scan:** Task 10 step 2 says "find the facade construction site" — this is a deliberate research step, not a placeholder. The rest has concrete code.

**Type consistency:** `lookupIntent func(host string) (string, bool)` in Task 4 matches `intentReg.LookupHost` in Task 9 (`func (r *Registry) LookupHost(host string) (Entry, bool)` — note: returns `Entry`, not `string`. Need to reconcile).

**Fix:** In Task 4 step 4, `lookupIntent` returns `(string, bool)`. In Task 9, we pass `intentReg.LookupHost` which returns `(Entry, bool)`. Reconcile by wrapping: in `run.go`, pass a closure:

```go
		np, available := netprompt.NewPrompter(p.Network.PromptTimeoutSecs(), logf, func(host string) (string, bool) {
			if intentReg == nil {
				return "", false
			}
			e, ok := intentReg.LookupHost(host)
			return e.Reason, ok
		})
```

This is now fixed in Task 9 step 2's code block above.
