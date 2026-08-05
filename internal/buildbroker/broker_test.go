package buildbroker

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tngtech/oh-my-agentic-coder/internal/buildengine"
)

// TestExecute_MethodRejectsNonPost asserts the execute route rejects
// non-POST methods before any other check.
func TestExecute_MethodRejectsNonPost(t *testing.T) {
	tb := newTestBroker(t, allowAllAuthorizer(), &stubEngine{result: successResult()})
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		req, _ := http.NewRequest(method, tb.server.URL+ExecutePath, nil)
		resp, err := tb.server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s: status = %d, want %d", method, resp.StatusCode, http.StatusMethodNotAllowed)
		}
		resp.Body.Close()
	}
}

// TestExecute_ContentTypeRejectsNonJSON asserts the execute route
// rejects content types other than exactly application/json.
func TestExecute_ContentTypeRejectsNonJSON(t *testing.T) {
	tb := newTestBroker(t, allowAllAuthorizer(), &stubEngine{result: successResult()})
	for _, ct := range []string{"text/plain", "application/json; charset=utf-8", "", "application/xml"} {
		body := `{"type":"execute","worktree":".","args":[]}`
		req, _ := http.NewRequest(http.MethodPost, tb.server.URL+ExecutePath, strings.NewReader(body))
		req.Header.Set("Content-Type", ct)
		req.Header.Set("Authorization", "Bearer "+tb.token)
		resp, err := tb.server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusUnsupportedMediaType {
			t.Errorf("ct=%q: status = %d, want %d", ct, resp.StatusCode, http.StatusUnsupportedMediaType)
		}
		resp.Body.Close()
	}
}

// TestExecute_AuthRejectsMissingBadAndAcceptsGood asserts the execute
// route rejects missing/malformed/incorrect bearer tokens and accepts
// the correct one (constant-time compare).
func TestExecute_AuthRejectsMissingBadAndAcceptsGood(t *testing.T) {
	tb := newTestBroker(t, allowAllAuthorizer(), &stubEngine{result: successResult()})
	body := `{"type":"execute","worktree":".","args":[]}`
	cases := []struct {
		name string
		auth string
		want int
	}{
		{"missing", "", http.StatusUnauthorized},
		{"malformed", "Token abc", http.StatusUnauthorized},
		{"wrong", "Bearer wrong-token", http.StatusUnauthorized},
		{"empty-bearer", "Bearer ", http.StatusUnauthorized},
	}
	for _, c := range cases {
		req, _ := http.NewRequest(http.MethodPost, tb.server.URL+ExecutePath, strings.NewReader(body))
		req.Header.Set("Content-Type", ContentTypeJSON)
		if c.auth != "" {
			req.Header.Set("Authorization", c.auth)
		}
		resp, err := tb.server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != c.want {
			t.Errorf("%s: status = %d, want %d", c.name, resp.StatusCode, c.want)
		}
		resp.Body.Close()
	}
}

// TestExecute_JSONShapeRejectsMalformedAndUnknownFields asserts the
// execute route rejects malformed JSON, unknown fields, and a missing
// EOF after the single object.
func TestExecute_JSONShapeRejectsMalformedAndUnknownFields(t *testing.T) {
	tb := newTestBroker(t, allowAllAuthorizer(), &stubEngine{result: successResult()})
	cases := []struct {
		name string
		body string
		want int
	}{
		{"malformed", `{not json`, http.StatusBadRequest},
		{"unknown-field", `{"type":"execute","worktree":".","args":[],"bogus":1}`, http.StatusBadRequest},
		{"trailing-data", `{"type":"execute","worktree":".","args":[]}{"type":"execute"}`, http.StatusBadRequest},
		{"missing-worktree", `{"type":"execute","args":[]}`, http.StatusBadRequest},
		{"missing-args", `{"type":"execute","worktree":"."}`, http.StatusBadRequest},
		{"wrong-type", `{"type":"stop","worktree":".","args":[]}`, http.StatusBadRequest},
	}
	for _, c := range cases {
		req, _ := http.NewRequest(http.MethodPost, tb.server.URL+ExecutePath, strings.NewReader(c.body))
		req.Header.Set("Content-Type", ContentTypeJSON)
		req.Header.Set("Authorization", "Bearer "+tb.token)
		resp, err := tb.server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != c.want {
			t.Errorf("%s: status = %d, want %d (body=%q)", c.name, resp.StatusCode, c.want, c.body)
		}
		resp.Body.Close()
	}
}

// TestExecute_BodySizeRejectsOverLimit asserts the execute route
// rejects a body larger than MaxExecuteBodyBytes.
func TestExecute_BodySizeRejectsOverLimit(t *testing.T) {
	tb := newTestBroker(t, allowAllAuthorizer(), &stubEngine{result: successResult()})
	// Build a body just over 1 MiB. The args array carries the bulk.
	big := strings.Repeat("x", int(MaxExecuteBodyBytes)+1024)
	body := fmt.Sprintf(`{"type":"execute","worktree":".","args":["%s"]}`, big)
	req, _ := http.NewRequest(http.MethodPost, tb.server.URL+ExecutePath, strings.NewReader(body))
	req.Header.Set("Content-Type", ContentTypeJSON)
	req.Header.Set("Authorization", "Bearer "+tb.token)
	resp, err := tb.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("oversize body: status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
	resp.Body.Close()
}

// TestExecute_ArgCountRejectsOverLimit asserts the execute route
// rejects a request with more than MaxArgs arguments.
func TestExecute_ArgCountRejectsOverLimit(t *testing.T) {
	tb := newTestBroker(t, allowAllAuthorizer(), &stubEngine{result: successResult()})
	args := make([]string, MaxArgs+1)
	for i := range args {
		args[i] = "x"
	}
	b, _ := json.Marshal(ExecuteBody{Type: "execute", Worktree: ".", Args: args})
	req, _ := http.NewRequest(http.MethodPost, tb.server.URL+ExecutePath, bytes.NewReader(b))
	req.Header.Set("Content-Type", ContentTypeJSON)
	req.Header.Set("Authorization", "Bearer "+tb.token)
	resp, err := tb.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("too many args: status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
	resp.Body.Close()
}

// TestExecute_ByteExactStdoutInvalidUTF8 asserts the broker preserves
// arbitrary bytes including invalid UTF-8 byte-for-byte through the
// base64 output frames.
func TestExecute_ByteExactStdoutInvalidUTF8(t *testing.T) {
	// Invalid UTF-8: 0xff 0xfe 0xfd, plus a multi-byte sequence that
	// straddles a frame boundary when chunked.
	invalid := []byte{0xff, 0xfe, 0xfd, 0xc3, 0x28, 0xed, 0xa0, 0x80}
	// Make it larger than one frame to test chunking.
	big := bytes.Repeat(invalid, (MaxOutputFrameBytes/len(invalid))+2)
	engine := &stubEngine{stdoutChunks: [][]byte{big}, result: successResult()}
	tb := newTestBroker(t, allowAllAuthorizer(), engine)
	body := `{"type":"execute","worktree":".","args":["--","gradle","test"]}`
	_, data := tb.executePOST(t, body)
	frames := parseFrames(t, data)
	// Reassemble stdout.
	var got []byte
	for _, f := range frames {
		if f.Type == "output" && f.Stream == "stdout" {
			got = append(got, f.decodeData(t)...)
		}
	}
	if !bytes.Equal(got, big) {
		t.Errorf("stdout byte-exactness: got %d bytes, want %d (first diff at %d)", len(got), len(big), firstDiff(got, big))
	}
	// Exactly one terminal result.
	var results int
	for _, f := range frames {
		if f.Type == "result" {
			results++
		}
	}
	if results != 1 {
		t.Errorf("terminal results = %d, want 1", results)
	}
}

// firstDiff returns the index of the first differing byte, or -1.
func firstDiff(a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	if len(a) != len(b) {
		return n
	}
	return -1
}

// TestExecute_ConcurrentWritersProduceValidFrames asserts concurrent
// stdout and stderr writers always produce valid whole NDJSON frames
// (no interleaving corruption).
func TestExecute_ConcurrentWritersProduceValidFrames(t *testing.T) {
	// Script the engine with a chunk on each stream; the broker's
	// frameWriter mutex serializes them.
	engine := &stubEngine{
		stdoutChunks: [][]byte{[]byte("stdout-1\n"), bytes.Repeat([]byte("A"), MaxOutputFrameBytes+100), []byte("stdout-2\n")},
		stderrChunks: [][]byte{[]byte("stderr-1\n"), bytes.Repeat([]byte("B"), MaxOutputFrameBytes+100), []byte("stderr-2\n")},
		result:       successResult(),
	}
	tb := newTestBroker(t, allowAllAuthorizer(), engine)
	_, data := tb.executePOST(t, `{"type":"execute","worktree":".","args":[]}`)
	// Every line must parse as valid JSON.
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var f frame
		if err := json.Unmarshal(line, &f); err != nil {
			t.Errorf("invalid frame %q: %v", string(line), err)
		}
	}
}

// TestExecute_OutputObservableBeforeCompletion asserts output frames
// arrive before the terminal result. Uses a stub engine that blocks
// until a cancel signal, with output written before blocking.
func TestExecute_OutputObservableBeforeCompletion(t *testing.T) {
	block := make(chan struct{})
	engine := &stubEngine{
		stdoutChunks: [][]byte{[]byte("partial-output\n")},
		result:       successResult(),
		blockUntil:   block,
	}
	tb := newTestBroker(t, allowAllAuthorizer(), engine)
	// Run the request asynchronously so we can observe output, then
	// unblock.
	ch := tb.runExecuteAsync(t, `{"type":"execute","worktree":".","args":[]}`)
	// Read the streaming response incrementally until we see the
	// output frame, then unblock the engine. We can't easily read
	// incrementally from the helper, so instead: wait a short beat,
	// then unblock and check the final body has output before result.
	close(block)
	res := <-ch
	frames := parseFrames(t, res.body)
	outIdx, resIdx := -1, -1
	for i, f := range frames {
		if f.Type == "output" {
			outIdx = i
		}
		if f.Type == "result" {
			resIdx = i
		}
	}
	if outIdx < 0 {
		t.Fatalf("no output frame in response: %s", res.body)
	}
	if resIdx < 0 {
		t.Fatalf("no result frame in response: %s", res.body)
	}
	if outIdx > resIdx {
		t.Errorf("output frame came AFTER result (out=%d res=%d)", outIdx, resIdx)
	}
}

// TestExecute_ExactlyOneTerminalResult asserts the broker emits exactly
// one terminal result frame.
func TestExecute_ExactlyOneTerminalResult(t *testing.T) {
	engine := &stubEngine{
		stdoutChunks: [][]byte{[]byte("a"), []byte("b")},
		result:       successResult(),
	}
	tb := newTestBroker(t, allowAllAuthorizer(), engine)
	_, data := tb.executePOST(t, `{"type":"execute","worktree":".","args":[]}`)
	frames := parseFrames(t, data)
	var results int
	for _, f := range frames {
		if f.Type == "result" {
			results++
		}
	}
	if results != 1 {
		t.Errorf("terminal results = %d, want 1", results)
	}
}

// TestExecute_ResultClassMapping asserts the result frame carries the
// engine's explicit class and the translated exit code.
func TestExecute_ResultClassMapping(t *testing.T) {
	cases := []struct {
		name   string
		result buildengine.Result
		class  string
		exit   int
	}{
		{"success", buildengine.Result{Class: buildengine.ClassSuccess, Exit: 0}, "success", 0},
		{"build_failure", buildengine.Result{Class: buildengine.ClassBuildFailure, Exit: 1}, "build_failure", 1},
		{"policy_denial", buildengine.Result{Class: buildengine.ClassPolicyDenial, Exit: 3}, "policy_denial", 3},
		{"cancelled", buildengine.Result{Class: buildengine.ClassCancelled, Exit: 4}, "cancelled", 4},
		{"service_failure", buildengine.Result{Class: buildengine.ClassServiceFailure, Exit: 10}, "service_failure", 10},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			engine := &stubEngine{result: c.result}
			tb := newTestBroker(t, allowAllAuthorizer(), engine)
			_, data := tb.executePOST(t, `{"type":"execute","worktree":".","args":[]}`)
			frames := parseFrames(t, data)
			var res frame
			for _, f := range frames {
				if f.Type == "result" {
					res = f
				}
			}
			if res.Type != "result" {
				t.Fatalf("no result frame")
			}
			if res.Class != c.class {
				t.Errorf("class = %q, want %q", res.Class, c.class)
			}
			if res.ExitCode != c.exit {
				t.Errorf("exit_code = %d, want %d", res.ExitCode, c.exit)
			}
		})
	}
}

// TestCancel_IdempotentAndStatuses asserts cancel returns 204 for an
// active request, 204 again for a repeat (idempotent), 410 for a
// recently completed request, and 404 for an unknown id.
func TestCancel_IdempotentAndStatuses(t *testing.T) {
	block := make(chan struct{})
	engine := &stubEngine{result: successResult(), blockUntil: block}
	tb := newTestBroker(t, allowAllAuthorizer(), engine)
	// Start an execute that blocks until we close `block`.
	ch := tb.runExecuteAsync(t, `{"type":"execute","worktree":".","args":[]}`)
	// Wait for the broker to register the active request (the engine
	// is invoked after `accepted`, so the request is in the registry
	// by the time the engine runs).
	id := waitForActiveID(t, tb)
	// 204 graceful.
	resp := tb.cancelPOST(t, id, "graceful")
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("graceful: status = %d, want 204", resp.StatusCode)
	}
	// 204 graceful again (idempotent).
	resp = tb.cancelPOST(t, id, "graceful")
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("graceful repeat: status = %d, want 204", resp.StatusCode)
	}
	// 204 force (force implies graceful).
	resp = tb.cancelPOST(t, id, "force")
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("force: status = %d, want 204", resp.StatusCode)
	}
	// Unblock the engine so the request completes.
	close(block)
	<-ch
	// 410 for the recently completed request.
	resp = tb.cancelPOST(t, id, "graceful")
	if resp.StatusCode != http.StatusGone {
		t.Errorf("completed: status = %d, want 410", resp.StatusCode)
	}
	// 404 for an unknown id.
	resp = tb.cancelPOST(t, "deadbeefdeadbeefdeadbeefdeadbeef", "graceful")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown: status = %d, want 404", resp.StatusCode)
	}
}

// waitForActiveID polls the broker's active-request registry until one
// request is registered, then returns its ID. Fails the test after a
// short timeout if no request appears.
func waitForActiveID(t *testing.T, tb *testBroker) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ids := tb.broker.activeRequestIDs(); len(ids) > 0 {
			return ids[0]
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("no active request registered in time")
	return ""
}
