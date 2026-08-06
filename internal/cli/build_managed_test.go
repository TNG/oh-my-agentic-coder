package cli

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tngtech/oh-my-agentic-coder/internal/buildbroker"
	"github.com/tngtech/oh-my-agentic-coder/internal/buildengine"
)

// TestDecideManagedMode_DirectWhenAllAbsent asserts direct host
// execution is selected only when the required marker AND all managed
// session variables are absent.
func TestDecideManagedMode_DirectWhenAllAbsent(t *testing.T) {
	clearBrokerEnv(t)
	mode, _ := decideManagedMode()
	if mode != managedModeDirect {
		t.Errorf("mode = %v, want direct", mode)
	}
}

// TestDecideManagedMode_ManagedWhenTupleComplete asserts managed mode
// is selected when all three (REQUIRED + CONTROL_BASE + TOKEN) are set.
func TestDecideManagedMode_ManagedWhenTupleComplete(t *testing.T) {
	clearBrokerEnv(t)
	t.Setenv(envBuildBrokerRequired, "1")
	t.Setenv(envControlBase, "http://127.0.0.1:12345")
	t.Setenv(envBuildToken, "abc")
	mode, ep := decideManagedMode()
	if mode != managedModeManaged {
		t.Errorf("mode = %v, want managed", mode)
	}
	if ep.Base != "http://127.0.0.1:12345" {
		t.Errorf("base = %q", ep.Base)
	}
	if ep.Token != "abc" {
		t.Errorf("token = %q", ep.Token)
	}
}

// TestDecideManagedMode_FailClosedWhenRequiredButMissingBase asserts
// the required marker with a missing base/token fails closed.
func TestDecideManagedMode_FailClosedWhenRequiredButMissingBase(t *testing.T) {
	clearBrokerEnv(t)
	t.Setenv(envBuildBrokerRequired, "1")
	t.Setenv(envBuildToken, "abc")
	// base missing
	mode, _ := decideManagedMode()
	if mode != managedModeFailClosed {
		t.Errorf("mode = %v, want failClosed (required set, base missing)", mode)
	}
}

// TestDecideManagedMode_FailClosedWhenRequiredButMissingToken asserts
// the required marker with a missing token fails closed.
func TestDecideManagedMode_FailClosedWhenRequiredButMissingToken(t *testing.T) {
	clearBrokerEnv(t)
	t.Setenv(envBuildBrokerRequired, "1")
	t.Setenv(envControlBase, "http://127.0.0.1:12345")
	// token missing
	mode, _ := decideManagedMode()
	if mode != managedModeFailClosed {
		t.Errorf("mode = %v, want failClosed (required set, token missing)", mode)
	}
}

// TestDecideManagedMode_PartialEnvBlocksDirect asserts any partial OMAC
// session env (without the complete broker tuple) blocks direct
// execution and fails closed.
func TestDecideManagedMode_PartialEnvBlocksDirect(t *testing.T) {
	cases := map[string]string{
		envOmacSocket:  "/tmp/omac.sock",
		envOmacBase:    "http://127.0.0.1:9999",
		envControlBase: "http://127.0.0.1:9999",
		envBuildToken:  "abc",
	}
	for name, val := range cases {
		t.Run(name, func(t *testing.T) {
			clearBrokerEnv(t)
			t.Setenv(name, val)
			// No required marker, but partial env present.
			mode, _ := decideManagedMode()
			if mode != managedModeFailClosed {
				t.Errorf("partial env %q: mode = %v, want failClosed", name, mode)
			}
		})
	}
}

// clearBrokerEnv clears all broker/OMAC session env vars for the test.
func clearBrokerEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{envBuildBrokerRequired, envControlBase, envBuildToken, envOmacSocket, envOmacBase} {
		t.Setenv(k, "")
	}
}

// TestRunBuild_FailClosedExit10 asserts runBuild exits 10 with the
// restart/upgrade diagnostic when the broker tuple is incomplete.
func TestRunBuild_FailClosedExit10(t *testing.T) {
	clearBrokerEnv(t)
	t.Setenv(envBuildBrokerRequired, "1")
	// base and token missing
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	var stderr bytes.Buffer
	env := &Env{Version: "test", Workdir: t.TempDir(), Stdout: &os.File{}, Stderr: &os.File{}}
	// Use byte buffers via a temp file replacement: Env.Stderr is
	// *os.File, so use a temp file we read back.
	stderrFile := newCapture(t)
	defer stderrFile.Close()
	env.Stderr = stderrFile
	code := runBuild([]string{"--root", ".", "--", "gradle", "test"}, env)
	if code != 10 {
		t.Errorf("code = %d, want 10", code)
	}
	_ = stderrFile.Sync()
	out, _ := os.ReadFile(stderrFile.Name())
	if !strings.Contains(string(out), "broker environment is incomplete") {
		t.Errorf("stderr missing diagnostic: %q", string(out))
	}
	_ = stderr
}

// TestRunBuild_HelpWithoutBroker asserts `omac build --help` renders
// locally without a broker (no env vars needed).
func TestRunBuild_HelpWithoutBroker(t *testing.T) {
	clearBrokerEnv(t)
	// Even with the required marker set (incomplete tuple), --help
	// must render locally.
	t.Setenv(envBuildBrokerRequired, "1")
	stderrFile := newCapture(t)
	defer stderrFile.Close()
	env := &Env{Version: "test", Workdir: t.TempDir(), Stdout: newDevNull(t), Stderr: stderrFile}
	code := runBuild([]string{"--help"}, env)
	if code != ExitOK {
		t.Errorf("code = %d, want %d", code, ExitOK)
	}
	_ = stderrFile.Sync()
	out, _ := os.ReadFile(stderrFile.Name())
	if !strings.Contains(string(out), "omac build") {
		t.Errorf("help did not render: %q", string(out))
	}
}

// TestRunBuild_StopHelpWithoutBroker asserts `omac build stop --help`
// renders locally without a broker.
func TestRunBuild_StopHelpWithoutBroker(t *testing.T) {
	clearBrokerEnv(t)
	t.Setenv(envBuildBrokerRequired, "1")
	stderrFile := newCapture(t)
	defer stderrFile.Close()
	env := &Env{Version: "test", Workdir: t.TempDir(), Stdout: newDevNull(t), Stderr: stderrFile}
	code := runBuild([]string{"stop", "--help"}, env)
	if code != ExitOK {
		t.Errorf("code = %d, want %d", code, ExitOK)
	}
	_ = stderrFile.Sync()
	out, _ := os.ReadFile(stderrFile.Name())
	if !strings.Contains(string(out), "omac build stop") {
		t.Errorf("stop help did not render: %q", string(out))
	}
}

// TestRunBuildManaged_EndToEndWithFakeBroker asserts the managed CLI
// client streams output and translates the result class to the exit
// code, against a fake broker mounted on an httptest server.
func TestRunBuildManaged_EndToEndWithFakeBroker(t *testing.T) {
	// Fake broker: accept, stream one stdout + one stderr frame, then
	// a success result.
	engine := &fakeEngine{stdout: []byte("hello\n"), stderr: []byte("warn\n"), result: buildengine.Result{Class: buildengine.ClassSuccess, Exit: 0}}
	b, err := buildbroker.New(buildbroker.Options{
		Token:         "tok",
		Authorizer:    func(string) (string, error) { return "/", nil },
		EngineInvoker: engine.invoke,
	})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	b.Mount(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	// Set the managed env.
	clearBrokerEnv(t)
	t.Setenv(envBuildBrokerRequired, "1")
	t.Setenv(envControlBase, srv.URL)
	t.Setenv(envBuildToken, "tok")
	var stdout, stderr bytes.Buffer
	stdoutW, releaseStdout := stdoutFile(t, &stdout)
	stderrW, releaseStderr := stderrFile(t, &stderr)
	env := &Env{Version: "test", Workdir: t.TempDir(), Stdout: stdoutW, Stderr: stderrW}
	code := runBuild([]string{"--root", ".", "--", "gradle", "test"}, env)
	releaseStdout()
	releaseStderr()
	if code != 0 {
		t.Errorf("code = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "hello") {
		t.Errorf("stdout = %q, want hello", stdout.String())
	}
	if !strings.Contains(stderr.String(), "warn") {
		t.Errorf("stderr = %q, want warn", stderr.String())
	}
}

// TestRunBuildManaged_ServiceFailurePrintsMessage asserts the broker's
// terminal result-frame Message reaches stderr (the CLI previously
// dropped it, so a brokered service_failure exited 10 with zero
// diagnostic — the broker DOES sanitize and send it; the client just
// never printed it).
func TestRunBuildManaged_ServiceFailurePrintsMessage(t *testing.T) {
	engine := &fakeEngine{result: buildengine.Result{
		Class: buildengine.ClassServiceFailure,
		Exit:  10,
		Err:   errors.New("prepare daemon ownership: write pending daemon record"),
	}}
	b, _ := buildbroker.New(buildbroker.Options{
		Token: "tok", Authorizer: func(string) (string, error) { return "/", nil }, EngineInvoker: engine.invoke,
	})
	mux := http.NewServeMux()
	b.Mount(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	clearBrokerEnv(t)
	t.Setenv(envBuildBrokerRequired, "1")
	t.Setenv(envControlBase, srv.URL)
	t.Setenv(envBuildToken, "tok")
	var stdout, stderr bytes.Buffer
	stdoutW, releaseStdout := stdoutFile(t, &stdout)
	stderrW, releaseStderr := stderrFile(t, &stderr)
	env := &Env{Version: "test", Workdir: t.TempDir(), Stdout: stdoutW, Stderr: stderrW}
	code := runBuild([]string{"--root", ".", "--", "gradle", "test"}, env)
	releaseStdout()
	releaseStderr()
	if code != 10 {
		t.Errorf("code = %d, want 10", code)
	}
	want := "prepare daemon ownership"
	if !strings.Contains(stderr.String(), want) {
		t.Errorf("stderr must carry the broker's result message %q; got %q", want, stderr.String())
	}
	if !strings.HasPrefix(strings.TrimSpace(stderr.String()), "omac build:") {
		t.Errorf("stderr must carry the omac build: prefix; got %q", stderr.String())
	}
}

// TestRunBuildManaged_BuildFailureExitCode asserts a build_failure
// result frame translates to the wrapper's exit code.
func TestRunBuildManaged_BuildFailureExitCode(t *testing.T) {
	engine := &fakeEngine{result: buildengine.Result{Class: buildengine.ClassBuildFailure, Exit: 42}}
	b, _ := buildbroker.New(buildbroker.Options{
		Token: "tok", Authorizer: func(string) (string, error) { return "/", nil }, EngineInvoker: engine.invoke,
	})
	mux := http.NewServeMux()
	b.Mount(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	clearBrokerEnv(t)
	t.Setenv(envBuildBrokerRequired, "1")
	t.Setenv(envControlBase, srv.URL)
	t.Setenv(envBuildToken, "tok")
	var stdout, stderr bytes.Buffer
	stdoutW, releaseStdout := stdoutFile(t, &stdout)
	stderrW, releaseStderr := stderrFile(t, &stderr)
	env := &Env{Version: "test", Workdir: t.TempDir(), Stdout: stdoutW, Stderr: stderrW}
	code := runBuild([]string{"--root", ".", "--", "gradle", "test"}, env)
	releaseStdout()
	releaseStderr()
	if code != 42 {
		t.Errorf("code = %d, want 42 (raw wrapper exit)", code)
	}
}

// TestRunBuildManaged_BrokerUnreachableExits10 asserts an unreachable
// broker exits 10.
func TestRunBuildManaged_BrokerUnreachableExits10(t *testing.T) {
	clearBrokerEnv(t)
	t.Setenv(envBuildBrokerRequired, "1")
	t.Setenv(envControlBase, "http://127.0.0.1:1") // port 1: unreachable
	t.Setenv(envBuildToken, "tok")
	// Use a short-timeout client by patching? The production client
	// has no timeout; we rely on the connection refusing fast on
	// 127.0.0.1:1. Give the test a deadline.
	done := make(chan int, 1)
	go func() {
		stderrFile := newCapture(t)
		defer stderrFile.Close()
		env := &Env{Version: "test", Workdir: t.TempDir(), Stdout: newDevNull(t), Stderr: stderrFile}
		done <- runBuild([]string{"--root", ".", "--", "gradle", "test"}, env)
	}()
	select {
	case code := <-done:
		if code != 10 {
			t.Errorf("code = %d, want 10", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("managed build did not return within 10s")
	}
}

// fakeEngine is a minimal stub for the managed CLI tests; it uses the
// buildbroker stub pattern but lives in cli to exercise the client.
type fakeEngine struct {
	stdout []byte
	stderr []byte
	result buildengine.Result
}

func (f *fakeEngine) invoke(worktree string, args []string, stdout, stderr io.Writer, graceful, force <-chan struct{}) buildengine.Result {
	stdout.Write(f.stdout)
	stderr.Write(f.stderr)
	return f.result
}

// stdoutFile/stderrFile return an *os.File that writes to the buffer
// and a release func the test calls to close the write end and wait for
// the drain goroutine before reading the buffer. The cli Env uses
// *os.File for Stdout/Stderr; tests that want to capture into a
// bytes.Buffer use a pipe with a goroutine that copies the read end
// into the buffer.
func stdoutFile(t *testing.T, buf *bytes.Buffer) (*os.File, func()) {
	t.Helper()
	return newCapturePipe(t, buf)
}

func stderrFile(t *testing.T, buf *bytes.Buffer) (*os.File, func()) {
	t.Helper()
	return newCapturePipe(t, buf)
}

// newCapturePipe returns a pipe whose write end is the *os.File the
// CLI writes to, and whose read end is drained into buf by a goroutine.
// The returned release func closes the write end and waits for the
// goroutine; the test must call it before reading buf.
func newCapturePipe(t *testing.T, buf *bytes.Buffer) (*os.File, func()) {
	t.Helper()
	r, w, _ := os.Pipe()
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(buf, r)
		r.Close()
		close(done)
	}()
	t.Cleanup(func() {
		w.Close()
		<-done
	})
	return w, func() {
		w.Close()
		<-done
	}
}
