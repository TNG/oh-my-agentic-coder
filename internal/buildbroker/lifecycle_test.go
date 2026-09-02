package buildbroker

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tngtech/oh-my-agentic-coder/internal/buildengine"
)

// TestExecute_DisconnectTriggersGracefulCancel asserts that an
// execute-connection disconnect delivers graceful cancellation to the
// engine invoker (the registry does not leak the request).
func TestExecute_DisconnectTriggersGracefulCancel(t *testing.T) {
	block := make(chan struct{})
	engine := &stubEngine{result: successResult(), blockUntil: block}
	tb := newTestBroker(t, allowAllAuthorizer(), engine)
	body := `{"type":"execute","worktree":".","args":[]}`
	req, _ := http.NewRequest(http.MethodPost, tb.server.URL+ExecutePath, strings.NewReader(body))
	req.Header.Set("Content-Type", ContentTypeJSON)
	req.Header.Set("Authorization", "Bearer "+tb.token)
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = waitForActiveID(t, tb)
	resp.Body.Close() // disconnect
	deadline := time.Now().Add(ForceDeadlineSeconds*time.Second + 5*time.Second)
	for time.Now().Before(deadline) {
		engine.mu.Lock()
		g := engine.gracefulClosed
		engine.mu.Unlock()
		if g {
			close(block)
			break
		}
		time.Sleep(time.Millisecond)
	}
	engine.mu.Lock()
	if !engine.gracefulClosed {
		t.Errorf("disconnect did not deliver graceful cancellation")
	}
	engine.mu.Unlock()
}

// TestExecute_PanicRecoveryEmitsServiceFailure asserts a panic in the
// engine invoker is recovered and framed as a sanitized
// service_failure (the parent does not terminate).
func TestExecute_PanicRecoveryEmitsServiceFailure(t *testing.T) {
	panicInvoker := func(worktree string, args []string, stdout, stderr io.Writer, graceful, force <-chan struct{}) buildengine.Result {
		panic("boom")
	}
	b, err := New(Options{
		Token:         "test-token-0123456789abcdef0123456789abcdef",
		Authorizer:    allowAllAuthorizer(),
		EngineInvoker: panicInvoker,
	})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	b.Mount(mux)
	srv := newTestServer(t, mux)
	tb := &testBroker{server: srv, broker: b, token: "test-token-0123456789abcdef0123456789abcdef"}
	_, data := tb.executePOST(t, `{"type":"execute","worktree":".","args":[]}`)
	frames := parseFrames(t, data)
	var res frame
	for _, f := range frames {
		if f.Type == "result" {
			res = f
		}
	}
	if res.Type != "result" {
		t.Fatalf("no result frame after panic: %s", data)
	}
	if res.Class != "service_failure" {
		t.Errorf("class = %q, want service_failure", res.Class)
	}
	if res.ExitCode != 10 {
		t.Errorf("exit_code = %d, want 10", res.ExitCode)
	}
	if strings.Contains(res.Message, "boom") {
		t.Errorf("panic message leaked unsanitized: %q", res.Message)
	}
}

// TestExecute_GracefulThenForce asserts a graceful cancel followed by
// a force cancel both reach the engine invoker (force implies
// graceful).
func TestExecute_GracefulThenForce(t *testing.T) {
	block := make(chan struct{})
	engine := &stubEngine{result: successResult(), blockUntil: block}
	tb := newTestBroker(t, allowAllAuthorizer(), engine)
	ch := tb.runExecuteAsync(t, `{"type":"execute","worktree":".","args":[]}`)
	id := waitForActiveID(t, tb)
	tb.cancelPOST(t, id, "graceful")
	waitForGraceful(t, engine)
	tb.cancelPOST(t, id, "force")
	close(block)
	<-ch
	engine.mu.Lock()
	if !engine.forceClosed {
		t.Errorf("force not delivered")
	}
	engine.mu.Unlock()
}

func waitForGraceful(t *testing.T, engine *stubEngine) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		engine.mu.Lock()
		g := engine.gracefulClosed
		engine.mu.Unlock()
		if g {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("graceful not observed in time")
}

// TestExecute_StopDispatchedToInvoker asserts the broker dispatches
// `omac build stop` to the EngineInvoker (ticket 07 Phase 4: the broker
// no longer refuses stop; it routes `args[0]=="stop"` to the invoker,
// which the production wiring dispatches to buildengine.StopBrokered).
// The test injects a capture invoker that records the args it received
// and asserts the stop grammar reaches it verbatim. A genuine
// authorization denial (an unauthorized worktree) still 403s before the
// invoker runs (TestExecute_UnauthorizedWorktreeRejectedBeforeBuild).
func TestExecute_StopDispatchedToInvoker(t *testing.T) {
	var got struct {
		worktree string
		args     []string
		mu       sync.Mutex
	}
	captureInvoker := func(worktree string, args []string, stdout, stderr io.Writer, graceful, force <-chan struct{}) buildengine.Result {
		got.mu.Lock()
		got.worktree = worktree
		got.args = append([]string(nil), args...)
		got.mu.Unlock()
		return successResult()
	}
	b, err := New(Options{Token: "test-token-0123456789abcdef0123456789abcdef", Authorizer: allowAllAuthorizer(), EngineInvoker: captureInvoker})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	b.Mount(mux)
	srv := newTestServer(t, mux)
	tb := &testBroker{server: srv, broker: b, token: "test-token-0123456789abcdef0123456789abcdef"}
	body := `{"type":"execute","worktree":".","args":["stop","--root","backend"]}`
	_, data := tb.executePOST(t, body)
	frames := parseFrames(t, data)
	var res frame
	for _, f := range frames {
		if f.Type == "result" {
			res = f
		}
	}
	if res.Type != "result" {
		t.Fatalf("no result frame: %s", data)
	}
	if res.Class != "success" {
		t.Errorf("stop result class = %q, want success", res.Class)
	}
	got.mu.Lock()
	wt := got.worktree
	args := got.args
	got.mu.Unlock()
	if wt == "" {
		t.Fatalf("stop was not dispatched to the invoker (no worktree recorded)")
	}
	if len(args) < 1 || args[0] != "stop" {
		t.Errorf("invoker args = %v, want first arg \"stop\"", args)
	}
	// The stop grammar is carried through verbatim so the production
	// invoker can strip args[0] and dispatch args[1:] to
	// buildengine.StopBrokered (cli/build_broker_wiring.go).
	wantArgs := []string{"stop", "--root", "backend"}
	if len(args) != len(wantArgs) {
		t.Errorf("invoker args len = %d, want %d (%v)", len(args), len(wantArgs), args)
	} else {
		for i := range wantArgs {
			if args[i] != wantArgs[i] {
				t.Errorf("invoker args[%d] = %q, want %q", i, args[i], wantArgs[i])
			}
		}
	}
}

// TestExecute_UnauthorizedWorktreeRejectedBeforeBuild asserts an
// unauthorized worktree is rejected with 403 before the engine runs.
func TestExecute_UnauthorizedWorktreeRejectedBeforeBuild(t *testing.T) {
	engine := &stubEngine{result: successResult()}
	tb := newTestBroker(t, fixedAuthorizer("/nonexistent/worktree"), engine)
	body := `{"type":"execute","worktree":".","args":[]}`
	req, _ := http.NewRequest(http.MethodPost, tb.server.URL+ExecutePath, strings.NewReader(body))
	req.Header.Set("Content-Type", ContentTypeJSON)
	req.Header.Set("Authorization", "Bearer "+tb.token)
	resp, err := tb.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("unauthorized worktree: status = %d, want 403", resp.StatusCode)
	}
	resp.Body.Close()
	engine.mu.Lock()
	wt := engine.gotWorktree
	engine.mu.Unlock()
	if wt != "" {
		t.Errorf("engine ran for an unauthorized worktree (gotWorktree=%q)", wt)
	}
}

// TestShutdown_RejectsNewAndDrains asserts parent shutdown rejects
// new requests and drains the active one (graceful then force).
func TestShutdown_RejectsNewAndDrains(t *testing.T) {
	block := make(chan struct{})
	engine := &stubEngine{result: successResult(), blockUntil: block}
	tb := newTestBroker(t, allowAllAuthorizer(), engine)
	ch := tb.runExecuteAsync(t, `{"type":"execute","worktree":".","args":[]}`)
	_ = waitForActiveID(t, tb)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		tb.broker.Shutdown()
	}()
	waitForGraceful(t, engine)
	body := `{"type":"execute","worktree":".","args":[]}`
	req, _ := http.NewRequest(http.MethodPost, tb.server.URL+ExecutePath, strings.NewReader(body))
	req.Header.Set("Content-Type", ContentTypeJSON)
	req.Header.Set("Authorization", "Bearer "+tb.token)
	resp, err := tb.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("new request during shutdown: status = %d, want 503", resp.StatusCode)
	}
	resp.Body.Close()
	close(block)
	<-ch
	wg.Wait()
}

// TestExecute_ClientInputCannotSelectBuildState asserts the client
// cannot select a wrapper, manifest, proxy, credential, image, cache,
// env, or audit ID via the execute body: the broker rejects unknown
// fields before the engine runs. Structural: ExecuteBody has only
// Type, Worktree, Args.
func TestExecute_ClientInputCannotSelectBuildState(t *testing.T) {
	var got struct {
		worktree string
		args     []string
	}
	captureInvoker := func(worktree string, args []string, stdout, stderr io.Writer, graceful, force <-chan struct{}) buildengine.Result {
		got.worktree = worktree
		got.args = append([]string(nil), args...)
		return successResult()
	}
	b, err := New(Options{Token: "test-token-0123456789abcdef0123456789abcdef", Authorizer: allowAllAuthorizer(), EngineInvoker: captureInvoker})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	b.Mount(mux)
	srv := newTestServer(t, mux)
	tb := &testBroker{server: srv, broker: b, token: "test-token-0123456789abcdef0123456789abcdef"}
	body := `{"type":"execute","worktree":".","args":["test"],"wrapper":"./evil-wrapper"}`
	req, _ := http.NewRequest(http.MethodPost, tb.server.URL+ExecutePath, strings.NewReader(body))
	req.Header.Set("Content-Type", ContentTypeJSON)
	req.Header.Set("Authorization", "Bearer "+tb.token)
	resp, err := tb.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("unknown field: status = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()
	if got.worktree != "" {
		t.Errorf("engine ran with injected state (worktree=%q args=%v)", got.worktree, got.args)
	}
}
