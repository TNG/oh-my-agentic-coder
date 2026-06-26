package session

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/tngtech/oh-my-agentic-coder/internal/config"
)

func opencodeHarness(t *testing.T) config.Harness {
	t.Helper()
	h, ok := config.LookupHarness("opencode")
	if !ok {
		t.Fatal("opencode harness not registered")
	}
	return h
}

func TestListUnsupported(t *testing.T) {
	// Harness with no Session block.
	if _, err := list(config.Harness{}, "/w", nil, "", ""); !errors.Is(err, ErrUnsupported) {
		t.Errorf("nil Session: err = %v, want ErrUnsupported", err)
	}
	// Harness whose Session declares no listing strategy.
	h := config.Harness{Session: &config.HarnessSession{ListKind: config.SessionListNone}}
	if _, err := list(h, "/w", nil, "", ""); !errors.Is(err, ErrUnsupported) {
		t.Errorf("SessionListNone: err = %v, want ErrUnsupported", err)
	}
}

func TestListOpenCodeParseAndFilter(t *testing.T) {
	const wd = "/home/u/proj"
	run := func(name string, args ...string) ([]byte, error) {
		return []byte(`[
			{"id":"ses_new","title":"newest","updated":2000,"directory":"/home/u/proj"},
			{"id":"ses_old","title":"oldest","updated":1000,"directory":"/home/u/proj"},
			{"id":"ses_other","title":"elsewhere","updated":3000,"directory":"/home/u/other"}
		]`), nil
	}
	got, err := list(opencodeHarness(t), wd, run, "", "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d sessions, want 2 (filtered to workdir): %+v", len(got), got)
	}
	if got[0].ID != "ses_new" || got[1].ID != "ses_old" {
		t.Errorf("order = [%s,%s], want newest-first [ses_new,ses_old]", got[0].ID, got[1].ID)
	}
	if !got[0].When.Equal(time.UnixMilli(2000)) {
		t.Errorf("ses_new When = %v, want epoch-ms 2000", got[0].When)
	}
}

func TestListOpenCodeCLIFailureIsEmpty(t *testing.T) {
	run := func(name string, args ...string) ([]byte, error) { return nil, errors.New("not found") }
	got, err := list(opencodeHarness(t), "/w", run, "", "")
	if err != nil {
		t.Fatalf("CLI failure should not error, got %v", err)
	}
	if got != nil {
		t.Errorf("CLI failure should yield nil sessions, got %+v", got)
	}
}

func TestListOpenCodeDBMissingReturnsNil(t *testing.T) {
	// No db file -> nil (falls back to CLI, which also returns nil here).
	if got := listOpenCodeDB("/w", filepath.Join(t.TempDir(), "nope.db")); got != nil {
		t.Errorf("missing db should yield nil, got %+v", got)
	}
}

func TestListOpenCodeDBParseAndFilter(t *testing.T) {
	// Build a fake db with a session table mirroring opencode's schema and
	// verify listOpenCodeDB parses it. Requires sqlite3 on PATH; skip when
	// unavailable (best-effort, like the production path).
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 not on PATH; skipping DB parse test")
	}
	db := filepath.Join(t.TempDir(), "opencode.db")
	wd := "/home/u/proj"
	createSessionTable(t, db, []ocDBRow{
		{ID: "ses_new", Title: "newest", Updated: 2000, Directory: wd},
		{ID: "ses_old", Title: "oldest", Updated: 1000, Directory: wd},
		{ID: "ses_other", Title: "elsewhere", Updated: 3000, Directory: "/home/u/other"},
	})
	got := listOpenCodeDB(wd, db)
	if len(got) != 2 {
		t.Fatalf("got %d sessions, want 2 (filtered to workdir): %+v", len(got), got)
	}
	if got[0].ID != "ses_new" || got[1].ID != "ses_old" {
		t.Errorf("order = [%s,%s], want newest-first [ses_new,ses_old]", got[0].ID, got[1].ID)
	}
	if !got[0].When.Equal(time.UnixMilli(2000)) {
		t.Errorf("ses_new When = %v, want epoch-ms 2000", got[0].When)
	}
}

type ocDBRow struct {
	ID, Title string
	Updated   int64
	Directory string
}

func createSessionTable(t *testing.T, db string, rows []ocDBRow) {
	t.Helper()
	args := []string{db,
		"CREATE TABLE session (id TEXT PRIMARY KEY, project_id TEXT NOT NULL, slug TEXT NOT NULL, directory TEXT NOT NULL, title TEXT NOT NULL, version TEXT NOT NULL, time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL);"}
	for _, r := range rows {
		args = append(args,
			"INSERT INTO session (id, project_id, slug, directory, title, version, time_created, time_updated) VALUES ('"+
				r.ID+"','p','s','"+r.Directory+"','"+r.Title+"','v',0,"+strconv.FormatInt(r.Updated, 10)+");")
	}
	cmd := exec.Command("sqlite3", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create session table: %v\n%s", err, out)
	}
}

func TestEncodeProjectDir(t *testing.T) {
	cases := map[string]string{
		"/home/u/oh-my-agentic-coder":      "-home-u-oh-my-agentic-coder",
		"/home/u/proj/.claude/worktrees/x": "-home-u-proj--claude-worktrees-x",
	}
	for in, want := range cases {
		if got := encodeProjectDir(in); got != want {
			t.Errorf("encodeProjectDir(%q) = %q, want %q", in, got, want)
		}
	}
}

// writeJSONL writes a session fixture file and returns the workdir it claims.
func writeClaudeFixture(t *testing.T, root, workdir, id string, lines []string) {
	t.Helper()
	dir := filepath.Join(root, encodeProjectDir(workdir))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := ""
	for _, l := range lines {
		content += l + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestListClaudeTitleTimeAndCwdMatch(t *testing.T) {
	root := t.TempDir()
	const wd = "/home/u/proj"

	// Session A: has an aiTitle and timestamps; cwd matches.
	writeClaudeFixture(t, root, wd, "aaaa-1111", []string{
		`{"type":"user","cwd":"/home/u/proj","timestamp":"2026-01-01T10:00:00Z","message":{"content":"hello there"}}`,
		`{"type":"ai-title","aiTitle":"First title","sessionId":"aaaa-1111"}`,
		`{"type":"assistant","timestamp":"2026-01-01T10:05:00Z"}`,
		`{"type":"ai-title","aiTitle":"Final title","sessionId":"aaaa-1111"}`,
		`not valid json — must be skipped`,
	})
	// Session B: no aiTitle → falls back to first user message text; cwd matches.
	writeClaudeFixture(t, root, wd, "bbbb-2222", []string{
		`{"type":"user","cwd":"/home/u/proj","timestamp":"2026-01-02T09:00:00Z","message":{"content":[{"type":"text","text":"fix the bug please"}]}}`,
	})

	got := listClaude(wd, root)
	if len(got) != 2 {
		t.Fatalf("got %d sessions, want 2: %+v", len(got), got)
	}
	// B is newer (Jan 2) → first.
	if got[0].ID != "bbbb-2222" {
		t.Errorf("newest-first: got[0] = %s, want bbbb-2222", got[0].ID)
	}
	byID := map[string]Session{got[0].ID: got[0], got[1].ID: got[1]}
	if byID["aaaa-1111"].Title != "Final title" {
		t.Errorf("session A title = %q, want last aiTitle 'Final title'", byID["aaaa-1111"].Title)
	}
	if byID["bbbb-2222"].Title != "fix the bug please" {
		t.Errorf("session B title = %q, want first-user-message fallback", byID["bbbb-2222"].Title)
	}
	if !byID["aaaa-1111"].When.Equal(time.Date(2026, 1, 1, 10, 5, 0, 0, time.UTC)) {
		t.Errorf("session A When = %v, want last timestamp 10:05", byID["aaaa-1111"].When)
	}
}

func TestListClaudeCwdMismatchExcluded(t *testing.T) {
	root := t.TempDir()
	const wd = "/home/u/proj"
	// File lives under the encoded dir for wd, but its embedded cwd differs
	// (encoding collision). It MUST be excluded.
	writeClaudeFixture(t, root, wd, "cccc-3333", []string{
		`{"type":"user","cwd":"/home/u/different","timestamp":"2026-01-01T10:00:00Z","message":{"content":"x"}}`,
	})
	if got := listClaude(wd, root); len(got) != 0 {
		t.Errorf("cwd mismatch should be excluded, got %+v", got)
	}
}

func TestListClaudeMissingRootIsEmpty(t *testing.T) {
	if got := listClaude("/home/u/proj", filepath.Join(t.TempDir(), "does-not-exist")); got != nil {
		t.Errorf("missing root should yield nil, got %+v", got)
	}
	if got := listClaude("/home/u/proj", ""); got != nil {
		t.Errorf("empty root should yield nil, got %+v", got)
	}
}

func TestListClaudeMtimeFallback(t *testing.T) {
	root := t.TempDir()
	const wd = "/home/u/proj"
	// No timestamp records → When falls back to file mtime.
	writeClaudeFixture(t, root, wd, "dddd-4444", []string{
		`{"type":"user","cwd":"/home/u/proj","message":{"content":"no timestamp here"}}`,
	})
	got := listClaude(wd, root)
	if len(got) != 1 {
		t.Fatalf("got %d, want 1", len(got))
	}
	if got[0].When.IsZero() {
		t.Error("When should fall back to file mtime, got zero")
	}
}
