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
	if _, err := list(config.Harness{}, "/w", nil, "", "", "", "", "", ""); !errors.Is(err, ErrUnsupported) {
		t.Errorf("nil Session: err = %v, want ErrUnsupported", err)
	}
	// Harness whose Session declares no listing strategy.
	h := config.Harness{Session: &config.HarnessSession{ListKind: config.SessionListNone}}
	if _, err := list(h, "/w", nil, "", "", "", "", "", ""); !errors.Is(err, ErrUnsupported) {
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
	got, err := list(opencodeHarness(t), wd, run, "", "", "", "", "", "")
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
	got, err := list(opencodeHarness(t), "/w", run, "", "", "", "", "", "")
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

// --- Codex + Copilot session listing -----------------------------------------

func codexHarness(t *testing.T) config.Harness {
	t.Helper()
	h, ok := config.LookupHarness("codex")
	if !ok {
		t.Fatal("codex harness not registered")
	}
	return h
}

func copilotHarness(t *testing.T) config.Harness {
	t.Helper()
	h, ok := config.LookupHarness("copilot")
	if !ok {
		t.Fatal("copilot harness not registered")
	}
	return h
}

func TestListCodexDispatchesNotUnsupported(t *testing.T) {
	// Even with empty paths, codex listing must NOT return ErrUnsupported —
	// it dispatches to the codex backend (which returns nil best-effort).
	_, err := list(codexHarness(t), "/w", nil, "", "", "", "", "", "")
	if errors.Is(err, ErrUnsupported) {
		t.Errorf("codex listing returned ErrUnsupported, want dispatch (err=%v)", err)
	}
}

func TestListCopilotDispatchesNotUnsupported(t *testing.T) {
	_, err := list(copilotHarness(t), "/w", nil, "", "", "", "", "", "")
	if errors.Is(err, ErrUnsupported) {
		t.Errorf("copilot listing returned ErrUnsupported, want dispatch (err=%v)", err)
	}
}

func TestListCodexMissingStoreIsEmpty(t *testing.T) {
	// No codex session store at the given root → nil, no error.
	root := filepath.Join(t.TempDir(), "no-sessions-dir")
	got, err := list(codexHarness(t), "/w", nil, "", "", root, "", "", "")
	if err != nil {
		t.Fatalf("codex missing store: err = %v, want nil", err)
	}
	if got != nil {
		t.Errorf("codex missing store: got %+v, want nil", got)
	}
}

func TestListCopilotMissingDBIsEmpty(t *testing.T) {
	// No copilot session-store.db → nil, no error.
	dbPath := filepath.Join(t.TempDir(), "nope.db")
	got, err := list(copilotHarness(t), "/w", nil, "", "", "", dbPath, "", "")
	if err != nil {
		t.Fatalf("copilot missing db: err = %v, want nil", err)
	}
	if got != nil {
		t.Errorf("copilot missing db: got %+v, want nil", got)
	}
}

// --- Codex nested rollout files ---------------------------------------------

func writeCodexRollout(t *testing.T, root, dateDir, filename, sessionMeta string) {
	t.Helper()
	dir := filepath.Join(root, dateDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// session_meta line + a trailing junk line (must not be parsed).
	content := sessionMeta + "\n" + `{"type":"event_msg","payload":{"type":"task_started"}}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, filename), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestListCodexNestedParseAndFilter(t *testing.T) {
	root := t.TempDir()
	const wd = "/home/u/proj"
	// Two matching sessions, nested under YYYY/MM/DD/.
	writeCodexRollout(t, root, "2026/06/29",
		"rollout-2026-06-29T10-00-00-aaaa.jsonl",
		`{"timestamp":"2026-06-29T10:00:00.000Z","type":"session_meta","payload":{"session_id":"aaaa","id":"aaaa","cwd":"/home/u/proj","timestamp":"2026-06-29T10:00:00.000Z"}}`)
	writeCodexRollout(t, root, "2026/06/30",
		"rollout-2026-06-30T12-00-00-bbbb.jsonl",
		`{"timestamp":"2026-06-30T12:00:00.000Z","type":"session_meta","payload":{"session_id":"bbbb","id":"bbbb","cwd":"/home/u/proj","timestamp":"2026-06-30T12:00:00.000Z"}}`)
	// Different cwd → excluded.
	writeCodexRollout(t, root, "2026/06/30",
		"rollout-2026-06-30T13-00-00-cccc.jsonl",
		`{"timestamp":"2026-06-30T13:00:00.000Z","type":"session_meta","payload":{"session_id":"cccc","id":"cccc","cwd":"/home/u/other","timestamp":"2026-06-30T13:00:00.000Z"}}`)
	// Non-rollout file → skipped.
	writeCodexRollout(t, root, "2026/06/30",
		"other.jsonl",
		`{"type":"session_meta","payload":{"session_id":"dddd","cwd":"/home/u/proj"}}`)

	got := listCodex(wd, root)
	if len(got) != 2 {
		t.Fatalf("got %d sessions, want 2: %+v", len(got), got)
	}
	// bbbb (Jun 30) is newer than aaaa (Jun 29) → first.
	if got[0].ID != "bbbb" || got[1].ID != "aaaa" {
		t.Errorf("order = [%s,%s], want newest-first [bbbb,aaaa]", got[0].ID, got[1].ID)
	}
	if got[0].Title != "proj" {
		t.Errorf("bbbb title = %q, want 'proj' (cwd basename)", got[0].Title)
	}
	want := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	if !got[0].When.Equal(want) {
		t.Errorf("bbbb When = %v, want %v", got[0].When, want)
	}
}

func TestListCodexMissingCwdIncluded(t *testing.T) {
	// Session with no cwd field → included (don't filter out what we can't
	// classify; safer to show).
	root := t.TempDir()
	writeCodexRollout(t, root, "2026/06/30",
		"rollout-2026-06-30T12-00-00-nocwd.jsonl",
		`{"timestamp":"2026-06-30T12:00:00.000Z","type":"session_meta","payload":{"session_id":"nocwd","timestamp":"2026-06-30T12:00:00.000Z"}}`)
	got := listCodex("/home/u/proj", root)
	if len(got) != 1 {
		t.Fatalf("got %d sessions, want 1 (missing cwd included): %+v", len(got), got)
	}
	if got[0].ID != "nocwd" {
		t.Errorf("ID = %q, want 'nocwd'", got[0].ID)
	}
}

func TestListCodexGarbageFirstLineSkipped(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "2026/06/30")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// First line is not valid JSON → skipped, no panic.
	if err := os.WriteFile(filepath.Join(dir, "rollout-bad.jsonl"),
		[]byte("not json at all\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := listCodex("/w", root); got != nil {
		t.Errorf("garbage first line should yield nil, got %+v", got)
	}
}

// --- Copilot YAML fallback ---------------------------------------------------

func writeCopilotWorkspace(t *testing.T, stateDir, id, cwd, name, updated string) {
	t.Helper()
	dir := filepath.Join(stateDir, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "id: " + id + "\ncwd: " + cwd + "\nname: '" + name + "'\nupdated_at: " + updated + "\n"
	if err := os.WriteFile(filepath.Join(dir, "workspace.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestListCopilotYAMLParseAndFilter(t *testing.T) {
	stateDir := t.TempDir()
	const wd = "/home/u/proj"
	writeCopilotWorkspace(t, stateDir, "aaa-111", wd, "fix bug", "2026-06-29T10:00:00.000Z")
	writeCopilotWorkspace(t, stateDir, "bbb-222", wd, "add tests", "2026-06-30T12:00:00.000Z")
	// Different cwd → excluded.
	writeCopilotWorkspace(t, stateDir, "ccc-333", "/home/u/other", "other", "2026-06-30T13:00:00.000Z")

	got := listCopilotYAML(wd, stateDir)
	if len(got) != 2 {
		t.Fatalf("got %d sessions, want 2: %+v", len(got), got)
	}
	// bbb (Jun 30) newer than aaa (Jun 29) → first.
	if got[0].ID != "bbb-222" || got[1].ID != "aaa-111" {
		t.Errorf("order = [%s,%s], want [bbb-222,aaa-111]", got[0].ID, got[1].ID)
	}
	if got[0].Title != "add tests" {
		t.Errorf("bbb title = %q, want 'add tests'", got[0].Title)
	}
}

func TestListCopilotYAMLMissingDirIsEmpty(t *testing.T) {
	if got := listCopilotYAML("/w", filepath.Join(t.TempDir(), "nope")); got != nil {
		t.Errorf("missing state dir should yield nil, got %+v", got)
	}
}

// --- Pi session listing ------------------------------------------------------

func piHarness(t *testing.T) config.Harness {
	t.Helper()
	h, ok := config.LookupHarness("pi")
	if !ok {
		t.Fatal("pi harness not registered")
	}
	return h
}

func TestListPiDispatchesNotUnsupported(t *testing.T) {
	_, err := list(piHarness(t), "/w", nil, "", "", "", "", "", "")
	if errors.Is(err, ErrUnsupported) {
		t.Errorf("pi listing returned ErrUnsupported, want dispatch (err=%v)", err)
	}
}

func TestListPiMissingStoreIsEmpty(t *testing.T) {
	root := filepath.Join(t.TempDir(), "no-sessions-dir")
	got, err := list(piHarness(t), "/w", nil, "", "", "", "", "", root)
	if err != nil {
		t.Fatalf("pi missing store: err = %v, want nil", err)
	}
	if got != nil {
		t.Errorf("pi missing store: got %+v, want nil", got)
	}
}

func TestListPiParseAndFilter(t *testing.T) {
	root := t.TempDir()
	const wd = "/home/u/proj"

	dir := filepath.Join(root, "encoded-cwd")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	content := `{"id":"ses_aaa","cwd":"/home/u/proj","type":"user","content":"first prompt"}` + "\n" +
		`{"type":"assistant","content":"response"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "ses_aaa.jsonl"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	content2 := `{"id":"ses_bbb","cwd":"/home/u/proj","type":"user","content":"second prompt"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "ses_bbb.jsonl"), []byte(content2), 0o644); err != nil {
		t.Fatal(err)
	}

	content3 := `{"id":"ses_other","cwd":"/home/u/different","type":"user","content":"other"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "ses_other.jsonl"), []byte(content3), 0o644); err != nil {
		t.Fatal(err)
	}

	got := listPi(wd, root)
	if len(got) != 2 {
		t.Fatalf("got %d sessions, want 2 (filtered to workdir): %+v", len(got), got)
	}

	byID := map[string]Session{got[0].ID: got[0], got[1].ID: got[1]}
	if _, ok := byID["ses_aaa"]; !ok {
		t.Errorf("missing ses_aaa; got %+v", got)
	}
	if _, ok := byID["ses_bbb"]; !ok {
		t.Errorf("missing ses_bbb; got %+v", got)
	}
	if byID["ses_aaa"].Title != "first prompt" {
		t.Errorf("ses_aaa title = %q, want 'first prompt'", byID["ses_aaa"].Title)
	}
}

func TestListPiMissingCwdIncluded(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "sub")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := `{"id":"nocwd","type":"user","content":"hello"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "nocwd.jsonl"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	got := listPi("/home/u/proj", root)
	if len(got) != 1 {
		t.Fatalf("got %d sessions, want 1 (missing cwd included): %+v", len(got), got)
	}
	if got[0].ID != "nocwd" {
		t.Errorf("ID = %q, want 'nocwd'", got[0].ID)
	}
}

func TestListPiFilenameFallbackID(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "sub")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := `{"type":"user","content":"no id field here"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "fallback-id.jsonl"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	got := listPi("/w", root)
	if len(got) != 1 {
		t.Fatalf("got %d sessions, want 1: %+v", len(got), got)
	}
	if got[0].ID != "fallback-id" {
		t.Errorf("ID = %q, want 'fallback-id' (filename fallback)", got[0].ID)
	}
}

func TestListPiEmptyRootIsEmpty(t *testing.T) {
	if got := listPi("/w", ""); got != nil {
		t.Errorf("empty root should yield nil, got %+v", got)
	}
}
