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
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/tngtech/oh-my-agentic-coder/internal/config"
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
	return list(h, workdir, execRunner, claudeProjectsRoot())
}

// list is the testable core: callers inject the command runner and the Claude
// projects root.
func list(h config.Harness, workdir string, run runner, claudeRoot string) ([]Session, error) {
	if h.Session == nil {
		return nil, ErrUnsupported
	}
	workdir = filepath.Clean(workdir)
	switch h.Session.ListKind {
	case config.SessionListOpenCodeCLI:
		return listOpenCode(workdir, run), nil
	case config.SessionListClaudeFiles:
		return listClaude(workdir, claudeRoot), nil
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

// listOpenCode runs the opencode CLI and keeps records for workdir. A missing
// CLI, non-zero exit, or unparseable output yields nil (best-effort).
func listOpenCode(workdir string, run runner) []Session {
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

// claudeProjectsRoot returns ~/.claude/projects, or "" when no home dir
// resolves (best-effort: an empty root yields no sessions).
func claudeProjectsRoot() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".claude", "projects")
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
