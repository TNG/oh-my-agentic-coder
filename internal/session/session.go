// Package session enumerates an inner harness's prior sessions for the
// current workdir, powering `omac resume`. Each supported harness exposes
// sessions differently, so listing dispatches on the harness's
// config.SessionListKind:
//
//   - OpenCode: parse `opencode session list --format json`.
//   - Claude Code: read ~/.claude/projects/<encoded-cwd>/<session-id>.jsonl.
//
// All listing is best-effort: a missing harness CLI, a missing session store,
// or unparseable state yields an empty list (or ErrUnsupported when the
// harness declares no listing strategy), never a hard failure that would
// block the user.
package session

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/tngtech/oh-my-agentic-coder/internal/config"
	"gopkg.in/yaml.v3"
	_ "modernc.org/sqlite"
)

// Session is one resumable session, normalized across harnesses.
type Session struct {
	ID    string    // harness-native session id (opencode "ses_…", claude UUID)
	Title string    // human-readable title for the picker
	When  time.Time // last-activity time; zero when unknown
}

// ErrUnsupported is returned when the harness declares no way to list
// sessions (nil Session block or config.SessionListNone). Continue may still
// work even when listing does not.
var ErrUnsupported = errors.New("session listing not supported for this harness")

// runner runs a command and returns its stdout. Swappable in tests.
type runner func(name string, args ...string) ([]byte, error)

// execRunner is the production runner: it shells out to the named binary.
func execRunner(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).Output()
}

// List returns harness's prior sessions for workdir, most-recent first.
// workdir SHOULD be absolute; it is cleaned before comparison.
func List(h config.Harness, workdir string) ([]Session, error) {
	return list(h, workdir, execRunner, claudeProjectsRoot(h), opencodeDBPath(), codexSessionsRoot(h), copilotDBPath(h), copilotStateDir(h))
}

// list is the testable core: callers inject the command runner, the Claude
// projects root, the opencode SQLite db path, the codex sessions root, the
// copilot SQLite db path, and the copilot session-state dir (yaml fallback).
func list(h config.Harness, workdir string, run runner, claudeRoot, ocDBPath, codexRoot, copilotDB, copilotState string) ([]Session, error) {
	if h.Session == nil {
		return nil, ErrUnsupported
	}
	workdir = filepath.Clean(workdir)
	switch h.Session.ListKind {
	case config.SessionListOpenCodeCLI:
		return listOpenCode(workdir, run, ocDBPath), nil
	case config.SessionListClaudeFiles:
		return listClaude(workdir, claudeRoot), nil
	case config.SessionListCodex:
		return listCodex(workdir, codexRoot), nil
	case config.SessionListCopilot:
		return listCopilot(workdir, copilotDB, copilotState), nil
	default:
		return nil, ErrUnsupported
	}
}

// sortNewestFirst orders sessions by When descending (unknown times sink to
// the end), with ID as a stable tiebreaker.
func sortNewestFirst(s []Session) {
	sort.SliceStable(s, func(i, j int) bool {
		if s[i].When.Equal(s[j].When) {
			return s[i].ID < s[j].ID
		}
		return s[i].When.After(s[j].When)
	})
}

// --- OpenCode backend -------------------------------------------------------

// ocRecord is the subset of `opencode session list --format json` we use.
type ocRecord struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Updated   int64  `json:"updated"` // epoch milliseconds
	Directory string `json:"directory"`
}

// listOpenCode returns sessions for workdir. It prefers reading the opencode
// SQLite db directly (~6ms) when the sqlite3 CLI is available, and falls back
// to shelling out to `opencode session list` (~1s) otherwise. A missing db,
// missing sqlite3, or unparseable output yields nil (best-effort).
func listOpenCode(workdir string, run runner, ocDBPath string) []Session {
	if sessions := listOpenCodeDB(workdir, ocDBPath); sessions != nil {
		return sessions
	}
	return listOpenCodeCLI(workdir, run)
}

// opencodeDBPath returns ~/.local/share/opencode/opencode.db, or "" when no
// home dir resolves.
func opencodeDBPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".local", "share", "opencode", "opencode.db")
}

// listOpenCodeDB reads sessions directly from opencode.db via the sqlite3 CLI.
// The db is opened read-only so a live opencode instance is not disturbed.
// Returns nil when sqlite3 is missing, the db is missing, or the query fails
// (best-effort — the caller falls back to the opencode CLI).
func listOpenCodeDB(workdir, dbPath string) []Session {
	if dbPath == "" {
		return nil
	}
	if _, err := os.Stat(dbPath); err != nil {
		return nil
	}
	sqlite, err := exec.LookPath("sqlite3")
	if err != nil {
		return nil
	}
	// ponytail: workdir is cleaned by the caller; embed it literally. The
	// session table's directory column holds the absolute path opencode was
	// launched from, which is what we match against.
	q := "SELECT id, title, time_updated FROM session WHERE directory = '" +
		strings.ReplaceAll(workdir, "'", "''") +
		"' ORDER BY time_updated DESC;"
	out, err := exec.Command(sqlite, "file:"+dbPath+"?mode=ro", q).Output()
	if err != nil {
		return nil
	}
	var sessions []Session
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line == "" {
			continue
		}
		// sqlite3's default separator is '|'. Title may contain '|' but that
		// is vanishingly rare for session titles; SplitN keeps the title whole.
		parts := strings.SplitN(line, "|", 3)
		if len(parts) < 3 {
			continue
		}
		id, title, tsStr := parts[0], parts[1], parts[2]
		if id == "" {
			continue
		}
		var when time.Time
		if ms, perr := strconv.ParseInt(tsStr, 10, 64); perr == nil && ms > 0 {
			when = time.UnixMilli(ms)
		}
		sessions = append(sessions, Session{ID: id, Title: title, When: when})
	}
	// Already sorted by time_updated DESC in SQL; re-sort to be safe (also
	// sinks zero-time rows deterministically).
	sortNewestFirst(sessions)
	return sessions
}

// listOpenCodeCLI runs the opencode CLI and keeps records for workdir. A
// missing CLI, non-zero exit, or unparseable output yields nil (best-effort).
// This is the slow fallback (~1s) used when sqlite3 is not on PATH.
func listOpenCodeCLI(workdir string, run runner) []Session {
	out, err := run("opencode", "session", "list", "--format", "json")
	if err != nil {
		return nil
	}
	var recs []ocRecord
	if json.Unmarshal(out, &recs) != nil {
		return nil
	}
	var sessions []Session
	for _, r := range recs {
		if r.ID == "" || filepath.Clean(r.Directory) != workdir {
			continue
		}
		var when time.Time
		if r.Updated > 0 {
			when = time.UnixMilli(r.Updated)
		}
		sessions = append(sessions, Session{ID: r.ID, Title: r.Title, When: when})
	}
	sortNewestFirst(sessions)
	return sessions
}

// --- Claude Code backend ----------------------------------------------------

// claudeProjectsRoot returns the claude projects dir, honoring CLAUDE_HOME.
// Returns "" when no home dir resolves (best-effort: an empty root yields no sessions).
func claudeProjectsRoot(h config.Harness) string {
	home := h.ConfigHome()
	if home == "" {
		return ""
	}
	return filepath.Join(home, "projects")
}

// encodeProjectDir mirrors Claude Code's per-project directory naming: every
// non-alphanumeric rune in the absolute path becomes '-'. The encoding is
// lossy (distinct paths can collide), so callers MUST still confirm a session
// belongs to workdir via its embedded cwd — see listClaude.
func encodeProjectDir(path string) string {
	var b strings.Builder
	for _, r := range path {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}

// listClaude reads workdir's Claude session files. The encoded directory name
// is only a lookup hint; membership is decided authoritatively by each file's
// embedded cwd. Missing root/dir or unreadable files yield nil.
func listClaude(workdir, projectsRoot string) []Session {
	if projectsRoot == "" {
		return nil
	}
	dir := filepath.Join(projectsRoot, encodeProjectDir(workdir))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var sessions []Session
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".jsonl" {
			continue
		}
		path := filepath.Join(dir, e.Name())
		s, ok := parseClaudeSession(path, workdir)
		if !ok {
			continue
		}
		sessions = append(sessions, s)
	}
	sortNewestFirst(sessions)
	return sessions
}

// claudeLine is the union of fields we read from a session's JSONL records.
type claudeLine struct {
	Type      string          `json:"type"`
	AiTitle   string          `json:"aiTitle"`
	Cwd       string          `json:"cwd"`
	Timestamp string          `json:"timestamp"`
	Message   json.RawMessage `json:"message"`
}

// parseClaudeSession reads one <session-id>.jsonl file. It returns false when
// the file's cwd does not match workdir (or cannot be determined). The id is
// the filename sans extension; the title is the latest aiTitle (falling back
// to the first user message, then the id); the time is the latest record
// timestamp (falling back to file mtime). Malformed lines are skipped.
func parseClaudeSession(path, workdir string) (Session, bool) {
	f, err := os.Open(path)
	if err != nil {
		return Session{}, false
	}
	defer f.Close()

	id := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	var (
		cwd       string
		title     string
		firstUser string
		latest    time.Time
	)
	sc := bufio.NewScanner(f)
	// Session lines can be large (tool output); raise the token limit.
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec claudeLine
		if json.Unmarshal(line, &rec) != nil {
			continue
		}
		if cwd == "" && rec.Cwd != "" {
			cwd = rec.Cwd
		}
		if rec.AiTitle != "" {
			title = rec.AiTitle
		}
		if firstUser == "" && rec.Type == "user" {
			firstUser = firstUserText(rec.Message)
		}
		if rec.Timestamp != "" {
			// RFC3339Nano accepts both fractional (e.g. ...:57.378Z) and
			// whole-second timestamps; Claude emits the fractional form.
			if t, err := time.Parse(time.RFC3339Nano, rec.Timestamp); err == nil {
				latest = t
			}
		}
	}
	if filepath.Clean(cwd) != workdir {
		return Session{}, false
	}
	if title == "" {
		title = firstUser
	}
	if title == "" {
		title = id
	}
	if latest.IsZero() {
		if fi, err := os.Stat(path); err == nil {
			latest = fi.ModTime()
		}
	}
	return Session{ID: id, Title: title, When: latest}, true
}

// firstUserText extracts a short plain-text preview from a user message whose
// content is either a string or an array of content blocks ({type,text}).
// Returns "" when no text is found.
func firstUserText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var msg struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &msg) != nil {
		return ""
	}
	// content as a plain string
	var s string
	if json.Unmarshal(msg.Content, &s) == nil {
		return truncate(s)
	}
	// content as an array of blocks
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(msg.Content, &blocks) == nil {
		for _, b := range blocks {
			if b.Text != "" {
				return truncate(b.Text)
			}
		}
	}
	return ""
}

// truncate collapses whitespace and caps a preview at 80 runes.
func truncate(s string) string {
	s = strings.TrimSpace(strings.Join(strings.Fields(s), " "))
	const max = 80
	r := []rune(s)
	if len(r) > max {
		return string(r[:max-1]) + "…"
	}
	return s
}

// --- Codex backend ----------------------------------------------------------

// codexSessionsRoot returns the codex sessions dir, honoring CODEX_HOME.
// Returns "" when no home dir resolves. Listing is best-effort and returns
// nil when the directory is absent.
func codexSessionsRoot(h config.Harness) string {
	home := h.ConfigHome()
	if home == "" {
		return ""
	}
	return filepath.Join(home, "sessions")
}

// listCodex reads Codex sessions from the codex session store. Sessions are
// stored as rollout-*.jsonl files nested under codexRoot/YYYY/MM/DD/. The first
// line of each file is a session_meta JSON record carrying session_id, cwd, and
// timestamp. workdir filters by the session's cwd; sessions whose cwd doesn't
// match are excluded (missing cwd → included, safer to show than hide).
// Returns nil when the store is missing or unreadable.
func listCodex(workdir, codexRoot string) []Session {
	if codexRoot == "" {
		return nil
	}
	workdir = filepath.Clean(workdir)
	var sessions []Session
	_ = filepath.WalkDir(codexRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if !strings.HasPrefix(d.Name(), "rollout-") || !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer f.Close()
		r := bufio.NewReader(f)
		line, err := r.ReadString('\n')
		if err != nil && line == "" {
			return nil
		}
		var meta codexSessionMeta
		if err := json.Unmarshal([]byte(line), &meta); err != nil {
			return nil
		}
		if meta.Type != "session_meta" {
			return nil
		}
		id := meta.Payload.SessionID
		if id == "" {
			id = meta.Payload.ID
		}
		if id == "" {
			return nil
		}
		if meta.Payload.CWD != "" && filepath.Clean(meta.Payload.CWD) != workdir {
			return nil
		}
		when := parseCodexTimestamp(meta.Payload.Timestamp)
		if when.IsZero() {
			if info, err := d.Info(); err == nil {
				when = info.ModTime()
			}
		}
		title := id
		if meta.Payload.CWD != "" {
			title = filepath.Base(meta.Payload.CWD)
		}
		sessions = append(sessions, Session{ID: id, Title: title, When: when})
		return nil
	})
	sortNewestFirst(sessions)
	return sessions
}

// codexSessionMeta is the JSON shape of the first line of a codex rollout file.
type codexSessionMeta struct {
	Type    string `json:"type"`
	Payload struct {
		SessionID string `json:"session_id"`
		ID        string `json:"id"`
		CWD       string `json:"cwd"`
		Timestamp string `json:"timestamp"`
	} `json:"payload"`
}

// parseCodexTimestamp parses a codex ISO-8601 timestamp (e.g.
// "2026-06-30T13:34:10.222Z"). Returns zero on failure.
func parseCodexTimestamp(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// --- Copilot backend --------------------------------------------------------

// copilotDBPath returns the copilot session-store.db path, honoring COPILOT_HOME.
// Returns "" when no home dir resolves.
func copilotDBPath(h config.Harness) string {
	home := h.ConfigHome()
	if home == "" {
		return ""
	}
	return filepath.Join(home, "session-store.db")
}

// copilotStateDir returns the copilot session-state directory (per-session
// workspace.yaml files), honoring COPILOT_HOME. Returns "" when no home dir
// resolves. Used as a fallback when the SQLite db is unavailable.
func copilotStateDir(h config.Harness) string {
	home := h.ConfigHome()
	if home == "" {
		return ""
	}
	return filepath.Join(home, "session-state")
}

// listCopilot reads Copilot sessions for workdir. Primary path: query the
// session-store.db via the pure-Go modernc.org/sqlite driver (no external
// sqlite3 binary needed). Fallback: walk copilotState/<uuid>/workspace.yaml
// files when the db is missing, locked, or the schema is unrecognized.
// Returns nil when both paths yield nothing (best-effort, never errors).
func listCopilot(workdir, dbPath, copilotState string) []Session {
	if dbPath != "" {
		if s := listCopilotDB(workdir, dbPath); s != nil {
			return s
		}
	}
	if copilotState == "" {
		return nil
	}
	return listCopilotYAML(workdir, copilotState)
}

// listCopilotDB queries the copilot session-store.db. The schema (as of
// copilot CLI 1.0.x): table "sessions" with columns id, cwd, repository,
// host_type, branch, summary, created_at, updated_at. We filter by cwd and
// order by updated_at DESC. Returns nil on any error (db missing, locked,
// schema mismatch).
func listCopilotDB(workdir, dbPath string) []Session {
	if _, err := os.Stat(dbPath); err != nil {
		return nil
	}
	dsn := "file:" + dbPath + "?mode=ro&_pragma=busy_timeout=500"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil
	}
	defer db.Close()
	// Schema-gate: verify the sessions table has the expected columns.
	cols, err := db.Query("PRAGMA table_info(sessions)")
	if err != nil {
		return nil
	}
	hasID, hasCWD, hasSummary, hasUpdated := false, false, false, false
	for cols.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := cols.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			cols.Close()
			return nil
		}
		switch name {
		case "id":
			hasID = true
		case "cwd":
			hasCWD = true
		case "summary":
			hasSummary = true
		case "updated_at":
			hasUpdated = true
		}
	}
	cols.Close()
	if !hasID || !hasCWD {
		return nil
	}
	// Build query based on available columns.
	idCol := "id"
	cwdCol := "cwd"
	summaryCol := "id"
	if hasSummary {
		summaryCol = "summary"
	}
	updatedCol := ""
	if hasUpdated {
		updatedCol = "updated_at"
	}
	q := "SELECT " + idCol + ", " + summaryCol + ", COALESCE(" + cwdCol + ", '') AS cwd"
	if updatedCol != "" {
		q += ", " + updatedCol
	}
	q += " FROM sessions ORDER BY "
	if updatedCol != "" {
		q += updatedCol + " DESC"
	} else {
		q += "rowid DESC"
	}
	q += " LIMIT 100"
	rows, err := db.Query(q)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var sessions []Session
	for rows.Next() {
		var id, title, cwd string
		var updated sql.NullString
		args := []any{&id, &title, &cwd}
		if updatedCol != "" {
			args = append(args, &updated)
		}
		if err := rows.Scan(args...); err != nil {
			continue
		}
		if id == "" {
			continue
		}
		if cwd != "" && filepath.Clean(cwd) != workdir {
			continue
		}
		when := parseCodexTimestamp(updated.String)
		if title == "" {
			title = id
		}
		sessions = append(sessions, Session{ID: id, Title: title, When: when})
	}
	if len(sessions) == 0 {
		return nil
	}
	if updatedCol == "" {
		// No updated_at — already ordered by rowid DESC, but sort for consistency.
		sortNewestFirst(sessions)
	}
	return sessions
}

// listCopilotYAML walks the session-state directory reading workspace.yaml
// files (one per session). Each file contains id, cwd, name, updated_at.
// Filters by cwd == workdir.
func listCopilotYAML(workdir, stateDir string) []Session {
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		return nil
	}
	var sessions []Session
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		yamlPath := filepath.Join(stateDir, e.Name(), "workspace.yaml")
		data, err := os.ReadFile(yamlPath)
		if err != nil {
			continue
		}
		var ws copilotWorkspaceYAML
		if err := yamlUnmarshal(data, &ws); err != nil {
			continue
		}
		if ws.ID == "" {
			continue
		}
		if ws.CWD != "" && filepath.Clean(ws.CWD) != workdir {
			continue
		}
		title := ws.Name
		if title == "" {
			title = ws.ID
		}
		when := parseCodexTimestamp(ws.UpdatedAt)
		if when.IsZero() {
			if fi, err := os.Stat(yamlPath); err == nil {
				when = fi.ModTime()
			}
		}
		sessions = append(sessions, Session{ID: ws.ID, Title: title, When: when})
	}
	sortNewestFirst(sessions)
	return sessions
}

// copilotWorkspaceYAML is the minimal shape of a copilot workspace.yaml file.
type copilotWorkspaceYAML struct {
	ID        string `yaml:"id"`
	CWD       string `yaml:"cwd"`
	Name      string `yaml:"name"`
	UpdatedAt string `yaml:"updated_at"`
}

// yamlUnmarshal is a thin wrapper over yaml.v3 so callers don't import it
// directly. It mirrors json.Unmarshal's signature.
func yamlUnmarshal(data []byte, v any) error {
	return yaml.Unmarshal(data, v)
}
