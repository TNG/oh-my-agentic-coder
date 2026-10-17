package herdr

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// mapGetenv adapts a map to the getenv signature Detect takes.
func mapGetenv(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestDetect(t *testing.T) {
	ok := func(t *testing.T, env map[string]string) Detected {
		t.Helper()
		det, found := Detect(mapGetenv(env))
		if !found {
			t.Fatalf("Detect(%q) = not ok, want ok", env)
		}
		return det
	}
	notOK := func(t *testing.T, env map[string]string) {
		t.Helper()
		if det, found := Detect(mapGetenv(env)); found {
			t.Fatalf("Detect(%q) = %+v, want not ok", env, det)
		}
	}

	full := map[string]string{
		"HERDR_ENV":      "1",
		"HERDR_PANE_ID":  "pane-7:0",
		"HERDR_BIN_PATH": "/usr/local/bin/herdr",
	}
	if det := ok(t, full); det.PaneID != "pane-7:0" || det.BinPath != "/usr/local/bin/herdr" {
		t.Fatalf("Detect fields = %+v", det)
	}

	// HERDR_ENV must be exactly "1": herdr sets it to mark pane processes.
	notOK(t, map[string]string{"HERDR_PANE_ID": "p", "HERDR_BIN_PATH": "/bin/herdr"})
	notOK(t, map[string]string{"HERDR_ENV": "yes", "HERDR_PANE_ID": "p", "HERDR_BIN_PATH": "/bin/herdr"})

	// Pane id: empty, flag-like, control characters, and overlong values
	// are all rejected so the id can never be confused with CLI flags.
	notOK(t, map[string]string{"HERDR_ENV": "1", "HERDR_PANE_ID": "", "HERDR_BIN_PATH": "/bin/herdr"})
	notOK(t, map[string]string{"HERDR_ENV": "1", "HERDR_PANE_ID": "-x", "HERDR_BIN_PATH": "/bin/herdr"})
	notOK(t, map[string]string{"HERDR_ENV": "1", "HERDR_PANE_ID": "bad\x01id", "HERDR_BIN_PATH": "/bin/herdr"})
	notOK(t, map[string]string{"HERDR_ENV": "1", "HERDR_PANE_ID": strings.Repeat("p", 129), "HERDR_BIN_PATH": "/bin/herdr"})

	// Bin path: only an absolute path is trusted; a relative value could
	// point the exec at an attacker-chosen binary via PATH lookup.
	notOK(t, map[string]string{"HERDR_ENV": "1", "HERDR_PANE_ID": "p", "HERDR_BIN_PATH": ""})
	notOK(t, map[string]string{"HERDR_ENV": "1", "HERDR_PANE_ID": "p", "HERDR_BIN_PATH": "bin/herdr"})
}

func TestValidSessionID(t *testing.T) {
	valid := []string{
		"a", "abc123", "ses_1a2b3c", "0e4d5f6a", "x.y-z_w",
	}
	for _, id := range valid {
		if !ValidSessionID(id) {
			t.Errorf("ValidSessionID(%q) = false, want true", id)
		}
	}
	invalid := []string{
		"",                        // empty
		"-lead", ".lead", "_lead", // leading punctuation reads as flag
		"has space", "has/slash", "it's", // herdr forbids these in resume argv
		"sidechain-abc",          // claude sub-agent session, not resumable
		strings.Repeat("a", 257), // absurd length
	}
	for _, id := range invalid {
		if ValidSessionID(id) {
			t.Errorf("ValidSessionID(%q) = true, want false", id)
		}
	}
}

func TestIDsFromJSONL(t *testing.T) {
	cases := map[string]string{
		"ses_abc.jsonl":      "ses_abc",
		"nested/dir/x.jsonl": "x",
		"notes.txt":          "",
		"noext":              "",
		".jsonl":             "",
	}
	for entry, want := range cases {
		if got := IDsFromJSONL(entry); got != want {
			t.Errorf("IDsFromJSONL(%q) = %q, want %q", entry, got, want)
		}
	}
}

func TestFileProbeFileChange(t *testing.T) {
	dir := t.TempDir()
	store := filepath.Join(dir, "opencode.db")
	if err := os.WriteFile(store, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := NewFileProbe(store, store+"-wal")

	// First call is the baseline: whatever exists now is not a change.
	if changed, ids := p.Probe(); changed || len(ids) != 0 {
		t.Fatalf("baseline Probe = (%v, %q), want (false, [])", changed, ids)
	}

	// A size change in the db counts as activity.
	if err := os.WriteFile(store, []byte("xyz"), 0o644); err != nil {
		t.Fatal(err)
	}
	if changed, _ := p.Probe(); !changed {
		t.Fatal("Probe after db change = false, want true")
	}

	// A new sidecar file counts as activity.
	wal := store + "-wal"
	if err := os.WriteFile(wal, []byte("w"), 0o644); err != nil {
		t.Fatal(err)
	}
	if changed, _ := p.Probe(); !changed {
		t.Fatal("Probe after wal appears = false, want true")
	}

	// So does a file vanishing.
	if err := os.Remove(wal); err != nil {
		t.Fatal(err)
	}
	if changed, _ := p.Probe(); !changed {
		t.Fatal("Probe after wal vanishes = false, want true")
	}

	// And a quiet store reports no change, no matter how often asked.
	for i := 0; i < 3; i++ {
		if changed, ids := p.Probe(); changed || len(ids) != 0 {
			t.Fatalf("quiet Probe = (%v, %q), want (false, [])", changed, ids)
		}
	}
}

func TestFileProbeDirIDs(t *testing.T) {
	dir := t.TempDir()
	p := NewFileProbe(dir)
	p.DirIDs(dir, IDsFromJSONL)
	if changed, ids := p.Probe(); changed || len(ids) != 0 {
		t.Fatalf("baseline Probe = (%v, %q), want (false, [])", changed, ids)
	}

	// A new .jsonl session file surfaces its id.
	if err := os.WriteFile(filepath.Join(dir, "ses_new.jsonl"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, ids := p.Probe()
	if !changed || len(ids) != 1 || ids[0] != "ses_new" {
		t.Fatalf("Probe after new session file = (%v, %q), want (true, [ses_new])", changed, ids)
	}

	// A non-session file is activity, but not an id.
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, ids = p.Probe()
	if !changed || len(ids) != 0 {
		t.Fatalf("Probe after non-session file = (%v, %q), want (true, [])", changed, ids)
	}

	// A claude sidechain file yields an id the bridge must reject later
	// (ValidSessionID), but the probe itself stays a dumb reporter.
	if err := os.WriteFile(filepath.Join(dir, "sidechain-x.jsonl"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, ids = p.Probe()
	if len(ids) != 1 || ids[0] != "sidechain-x" {
		t.Fatalf("Probe ids = %q, want [sidechain-x]", ids)
	}
	if ValidSessionID(ids[0]) {
		t.Fatal("ValidSessionID(sidechain-x) = true, want false")
	}
}

// fakeProbe is a scriptable Probe for bridge tests.
type fakeProbe struct {
	changed bool
	ids     []string
}

func (f *fakeProbe) Probe() (bool, []string) { return f.changed, f.ids }

// fakeRunner records herdr CLI calls instead of spawning processes.
type fakeRunner struct {
	mu    sync.Mutex
	calls [][]string
	err   error
}

func (f *fakeRunner) run(_ context.Context, argv []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string(nil), argv...))
	return f.err
}

func (f *fakeRunner) recorded() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]string(nil), f.calls...)
}

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }

// newTestBridge builds a Bridge with an injected runner and clock, then
// replicates Start's initialization without the goroutine so tests drive
// ticks synchronously.
func newTestBridge(t *testing.T, o Options) (*Bridge, *fakeRunner, *fakeClock, *fakeProbe) {
	t.Helper()
	probe := &fakeProbe{}
	if o.Probe == nil {
		o.Probe = probe
	}
	clock := &fakeClock{now: time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)}
	r := &fakeRunner{}
	o.Runner = r.run
	o.Now = clock.Now
	b := New(Detected{PaneID: "pane-1", BinPath: "/usr/bin/herdr"}, o)
	b.opts.Probe.Probe() // baseline, as Start does
	b.state = StateWorking
	b.lastChange = clock.now
	return b, r, clock, probe
}

func hasPair(argv []string, flag, val string) bool {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == flag && argv[i+1] == val {
			return true
		}
	}
	return false
}

func lastCall(t *testing.T, r *fakeRunner) []string {
	t.Helper()
	calls := r.recorded()
	if len(calls) == 0 {
		t.Fatal("no herdr calls recorded")
	}
	return calls[len(calls)-1]
}

func TestBridgeFirstReportClaimsPane(t *testing.T) {
	b, r, _, _ := newTestBridge(t, Options{})
	b.tick()

	calls := r.recorded()
	if len(calls) != 1 {
		t.Fatalf("got %d calls after first tick, want 1", len(calls))
	}
	argv := calls[0]
	if argv[0] != "/usr/bin/herdr" || argv[1] != "pane" || argv[2] != "report-agent" || argv[3] != "pane-1" {
		t.Fatalf("argv head = %q, want herdr pane report-agent pane-1", argv[:4])
	}
	if !hasPair(argv, "--source", "omac") || !hasPair(argv, "--agent", "omac") {
		t.Fatalf("argv %q missing --source/--agent omac", argv)
	}
	if !hasPair(argv, "--state", StateWorking) {
		t.Fatalf("argv %q: first report must claim the pane as working", argv)
	}
	for _, a := range argv {
		if a == "--" || a == "--agent-session-id" {
			t.Fatalf("argv %q: no session id yet, resume args must not be attached", argv)
		}
	}
}

func TestBridgeAttachesSessionIDAndResume(t *testing.T) {
	b, r, _, _ := newTestBridge(t, Options{
		SessionID:  func() string { return "ses_abc123" },
		ResumeArgv: func(id string) []string { return []string{"omac", "continue", "-s", id} },
	})
	b.tick()

	argv := lastCall(t, r)
	if !hasPair(argv, "--agent-session-id", "ses_abc123") {
		t.Fatalf("argv %q missing --agent-session-id ses_abc123", argv)
	}
	sep := -1
	for i, a := range argv {
		if a == "--" {
			sep = i
			break
		}
	}
	if sep < 0 {
		t.Fatalf("argv %q missing -- before resume command", argv)
	}
	want := []string{"omac", "continue", "-s", "ses_abc123"}
	if len(argv[sep+1:]) != len(want) {
		t.Fatalf("resume argv = %q, want %q", argv[sep+1:], want)
	}
	for i := range want {
		if argv[sep+1+i] != want[i] {
			t.Fatalf("resume argv = %q, want %q", argv[sep+1:], want)
		}
	}
}

func TestBridgeSkipsUnchangedReports(t *testing.T) {
	// A long quiet threshold so the clock advances below stay within
	// "working" and only the coalescing rule is under test.
	b, r, clock, _ := newTestBridge(t, Options{Quiet: time.Hour})
	b.tick()
	clock.now = clock.now.Add(time.Minute)
	b.tick()
	clock.now = clock.now.Add(time.Minute)
	b.tick() // still working, still no store change: nothing new to say
	if calls := r.recorded(); len(calls) != 1 {
		t.Fatalf("got %d calls for an unchanged session, want 1", len(calls))
	}
}

func TestBridgeWorkingToIdleAndBack(t *testing.T) {
	b, r, clock, probe := newTestBridge(t, Options{Quiet: 2 * time.Second})
	b.tick()
	if !hasPair(lastCall(t, r), "--state", StateWorking) {
		t.Fatal("first report must be working")
	}

	// Quiet store past the threshold: the harness is back at its prompt.
	clock.now = clock.now.Add(3 * time.Second)
	b.tick()
	if !hasPair(lastCall(t, r), "--state", StateIdle) {
		t.Fatal("quiet session must be reported idle")
	}

	// Staying idle sends nothing new.
	clock.now = clock.now.Add(time.Minute)
	b.tick()
	if calls := r.recorded(); len(calls) != 2 {
		t.Fatalf("got %d calls while idle, want 2", len(calls))
	}

	// A store write flips the state back to working.
	probe.changed = true
	b.tick()
	if !hasPair(lastCall(t, r), "--state", StateWorking) {
		t.Fatal("store change must be reported working")
	}
}

func TestBridgeFailSoftOnRunnerError(t *testing.T) {
	b, r, clock, _ := newTestBridge(t, Options{})
	r.err = context.DeadlineExceeded
	b.tick() // must not panic, must not retry the identical report
	clock.now = clock.now.Add(time.Minute)
	b.tick()
	if calls := r.recorded(); len(calls) != 1 {
		t.Fatalf("got %d calls after runner failure, want 1 (no retry spam)", len(calls))
	}

	// A state change still goes out, error or not.
	clock.now = clock.now.Add(3 * time.Hour)
	b.tick()
	if !hasPair(lastCall(t, r), "--state", StateIdle) {
		t.Fatal("state change after failures must still report")
	}
}

func TestNextSeqMonotonic(t *testing.T) {
	b, _, clock, _ := newTestBridge(t, Options{})
	s1 := b.nextSeq()
	// A rewound clock must not make herdr drop subsequent reports.
	clock.now = clock.now.Add(-24 * time.Hour)
	s2 := b.nextSeq()
	if s2 <= s1 {
		t.Fatalf("seq after clock rewind = %d, want > %d", s2, s1)
	}
	// Same-instant calls still tick upward.
	s3 := b.nextSeq()
	if s3 != s2+1 {
		t.Fatalf("seq at same instant = %d, want %d", s3, s2+1)
	}
}

func TestCloseReleasesPaneExactlyOnce(t *testing.T) {
	calls := make(chan []string, 64)
	probe := &fakeProbe{}
	b := New(Detected{PaneID: "pane-1", BinPath: "/usr/bin/herdr"}, Options{
		Probe: probe,
		Runner: func(_ context.Context, argv []string) error {
			calls <- append([]string(nil), argv...)
			return nil
		},
		PollInterval: time.Hour, // no report ticks during the test
	})
	b.Start()
	b.Close()
	// Idempotent: a second Close must not hang or re-release.
	b.Close()

	releases, reports := 0, 0
	for {
		select {
		case argv := <-calls:
			if len(argv) > 3 && argv[3] == "pane-1" && argv[2] == "release-agent" {
				releases++
				if !hasPair(argv, "--source", "omac") || !hasPair(argv, "--agent", "omac") {
					t.Fatalf("release argv %q missing source/agent", argv)
				}
			} else {
				reports++
			}
			continue
		default:
		}
		break
	}
	if releases != 1 {
		t.Fatalf("got %d release-agent calls, want exactly 1", releases)
	}
	if reports != 0 {
		t.Fatalf("got %d report calls, want 0", reports)
	}
}

func TestCloseWithoutStartIsNoop(t *testing.T) {
	var ran bool
	b := New(Detected{PaneID: "pane-1", BinPath: "/usr/bin/herdr"}, Options{
		Probe: &fakeProbe{},
		Runner: func(_ context.Context, _ []string) error {
			ran = true
			return nil
		},
	})
	// Start no-ops (Probe nil in the bridge itself) …
	b2 := New(Detected{PaneID: "pane-1", BinPath: "/usr/bin/herdr"}, Options{})
	b2.Close() // … so Close must be a no-op too, not a deadlock.
	b.Close()
	if ran {
		t.Fatal("Close on a never-started bridge must not call herdr")
	}
}
