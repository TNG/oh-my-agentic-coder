package audit

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// readLines reads a JSONL file into decoded Event slices.
func readEvents(t *testing.T, path string) []Event {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	defer f.Close()
	var out []Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev Event
		if err := json.Unmarshal(line, &ev); err != nil {
			t.Fatalf("line is not valid JSON: %v\n%s", err, line)
		}
		out = append(out, ev)
	}
	return out
}

func newTestAuditor(t *testing.T, cfg Config) (Auditor, string) {
	t.Helper()
	if cfg.Path == "" {
		cfg.Path = filepath.Join(t.TempDir(), "audit.jsonl")
	}
	cfg.Enabled = true
	if cfg.Mode == "" {
		cfg.Mode = ModeStart
	}
	a, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a, cfg.Path
}

func TestEnvelopeFieldsPresent(t *testing.T) {
	a, path := newTestAuditor(t, Config{Version: "9.9.9"})
	a.Emit(SessionStart("9.9.9", "opencode", "builtin", "seatbelt"))
	_ = a.Close()

	evs := readEvents(t, path)
	if len(evs) != 1 {
		t.Fatalf("want 1 event, got %d", len(evs))
	}
	ev := evs[0]
	if ev.Ts == "" || ev.RunID == "" || ev.Type == "" || ev.Mode == "" || ev.PID == 0 {
		t.Fatalf("missing envelope field: %+v", ev)
	}
	if ev.Seq != 1 {
		t.Fatalf("want seq 1, got %d", ev.Seq)
	}
	if ev.Type != TypeSessionStart || ev.Harness != "opencode" {
		t.Fatalf("unexpected payload: %+v", ev)
	}
}

func TestSeqMonotonicSameRunID(t *testing.T) {
	a, path := newTestAuditor(t, Config{})
	for i := 0; i < 5; i++ {
		a.Emit(ControlMutation("reload", "/tmp/x", "ok"))
	}
	_ = a.Close()

	evs := readEvents(t, path)
	if len(evs) != 5 {
		t.Fatalf("want 5, got %d", len(evs))
	}
	runID := evs[0].RunID
	for i, ev := range evs {
		if ev.Seq != uint64(i+1) {
			t.Fatalf("event %d: want seq %d, got %d", i, i+1, ev.Seq)
		}
		if ev.RunID != runID {
			t.Fatalf("event %d: run_id changed within a run", i)
		}
	}
}

func TestRedactArgvSecretValue(t *testing.T) {
	secret := "super-secret-token-value"
	a, path := newTestAuditor(t, Config{SecretValues: []string{secret}})
	a.Emit(ProcessExec("myskill", "abc123token", "/work",
		[]string{"run", "--token", secret, "--flag"},
		[]string{"MY_TOKEN"}, nil))
	_ = a.Close()

	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), secret) {
		t.Fatalf("secret value leaked into log:\n%s", data)
	}
	evs := readEvents(t, path)
	found := false
	for _, a := range evs[0].Argv {
		if a == redactedMarker {
			found = true
		}
		if a == secret {
			t.Fatalf("secret value present in decoded argv")
		}
	}
	if !found {
		t.Fatalf("expected redacted marker in argv: %v", evs[0].Argv)
	}
	// Secret NAME should be present (names are safe).
	if len(evs[0].SecretNames) != 1 || evs[0].SecretNames[0] != "MY_TOKEN" {
		t.Fatalf("expected secret name present: %v", evs[0].SecretNames)
	}
}

func TestRedactNamespaceHashed(t *testing.T) {
	token := "deadbeefcafef00d1234567890abcdef"
	a, path := newTestAuditor(t, Config{})
	a.Emit(FacadeRequest("GET", "slack", token, "chat", 200, 10, 1))
	a.Emit(FacadeRequest("GET", "gh", GlobalNamespace, "repos", 200, 10, 1))
	a.Emit(FacadeRequest("GET", "flat", "", "x", 200, 10, 1))
	_ = a.Close()

	data, _ := os.ReadFile(path)
	s := string(data)
	if strings.Contains(s, token) {
		t.Fatalf("dir-token leaked into log:\n%s", s)
	}
	evs := readEvents(t, path)
	if !strings.HasPrefix(evs[0].Namespace, "ns_") {
		t.Fatalf("want hashed namespace, got %q", evs[0].Namespace)
	}
	if evs[1].Namespace != GlobalNamespace {
		t.Fatalf("global namespace should pass through, got %q", evs[1].Namespace)
	}
	if evs[2].Namespace != "" {
		t.Fatalf("flat namespace should stay empty, got %q", evs[2].Namespace)
	}
}

func TestDisabledIsNop(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	a, err := New(Config{Enabled: false, Path: path})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	a.Emit(SessionStart("1", "h", "p", "b"))
	_ = a.Close()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("disabled auditor should not create a file")
	}
}

func TestFilePermissions(t *testing.T) {
	// Nest under a subdir the sink must create, so we exercise MkdirAll's
	// 0700 (t.TempDir itself is 0755 and pre-exists).
	path := filepath.Join(t.TempDir(), "sub", "audit.jsonl")
	a, path := newTestAuditor(t, Config{Path: path})
	a.Emit(SessionStart("1", "h", "p", "b"))
	_ = a.Close()

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("want file mode 0600, got %o", fi.Mode().Perm())
	}
	di, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Fatalf("want dir mode 0700, got %o", di.Mode().Perm())
	}
}

func TestAppendAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")

	a1, _ := New(Config{Enabled: true, Path: path, Mode: ModeStart})
	a1.Emit(SessionStart("1", "h", "p", "b"))
	_ = a1.Close()

	a2, _ := New(Config{Enabled: true, Path: path, Mode: ModeStart})
	a2.Emit(SessionStart("1", "h", "p", "b"))
	_ = a2.Close()

	evs := readEvents(t, path)
	if len(evs) != 2 {
		t.Fatalf("second run should append; want 2 events, got %d", len(evs))
	}
	if evs[0].RunID == "" || evs[1].RunID == "" || evs[0].RunID == evs[1].RunID {
		t.Fatalf("runs should carry distinct run_ids: %q vs %q", evs[0].RunID, evs[1].RunID)
	}
}

func TestStrictFatalOnWriteError(t *testing.T) {
	a, path := newTestAuditor(t, Config{
		Strict: true,
		Path:   filepath.Join(t.TempDir(), "audit.jsonl"),
	})
	_ = path
	// Force a write error by closing the underlying file out from under
	// the sink, then emitting.
	au := a.(*auditor)
	_ = au.fileSink.f.Close() // subsequent writes fail

	var fatalErr error
	au.fatal = func(err error) { fatalErr = err }

	au.Emit(ControlMutation("reload", "/x", "ok"))
	if fatalErr == nil {
		t.Fatalf("strict-mode write failure should invoke fatal handler")
	}
}

func TestFailOpenWarnsOnce(t *testing.T) {
	var warnings int
	a, _ := newTestAuditor(t, Config{
		Strict: false,
		Warnf:  func(string, ...any) { warnings++ },
	})
	au := a.(*auditor)
	_ = au.fileSink.f.Close()
	au.Emit(ControlMutation("reload", "/x", "ok"))
	au.Emit(ControlMutation("reload", "/y", "ok"))
	if warnings != 1 {
		t.Fatalf("fail-open should warn exactly once, got %d", warnings)
	}
}

func TestValidJSONPerLine(t *testing.T) {
	a, path := newTestAuditor(t, Config{})
	a.Emit(NetDecision("example.com", 443, true, "host", "prompt", true))
	a.Emit(SecretInject("s", "tok", []string{"A", "B"}, []string{"C"}))
	a.Emit(RouteStateEvent("s", "tok", "broken", "boom"))
	_ = a.Close()

	f, _ := os.Open(path)
	defer f.Close()
	sc := bufio.NewScanner(f)
	n := 0
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("line %d not valid JSON: %v", n, err)
		}
		n++
	}
	if n != 3 {
		t.Fatalf("want 3 lines, got %d", n)
	}
}
