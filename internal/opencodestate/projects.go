// Package opencodestate discovers projects authorized by the host Desktop
// store. Agent-writable project records are candidates, never authorization.
package opencodestate

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
)

// projectRecord is the subset of OpenCode's project JSON we care about.
type projectRecord struct {
	ID       string `json:"id"`
	Worktree string `json:"worktree"`
}

// SkipReason explains rejected project records or discovery sources.
type SkipReason string

const (
	SkipStale         SkipReason = "no longer exists"
	SkipUntrusted     SkipReason = "not authorized by the Desktop store; grant explicitly in filesystem.allow if intended"
	SkipInvalidSource SkipReason = "invalid, oversized, or unreadable discovery source (ignored)"
	// SkipDesktopStore: granting the folder read+write would let the agent
	// change the Desktop's own stores, which the sandbox refuses to start
	// with (for example a dotfiles project at ~/.config).
	SkipDesktopStore SkipReason = "holds OpenCode Desktop's own files, so the agent gets no access to it in this session"
)

// SkippedWorktree pairs a rejected worktree path with its reason.
type SkippedWorktree struct {
	Path   string
	Reason SkipReason
}

// storageProjectDir returns ~/.local/share/opencode/storage/project.
func storageProjectDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", "opencode", "storage", "project"), nil
}

// Worktrees returns canonical, existing Desktop project folders for the
// built-in server and the loopback server on port. Port 0 means built-in only.
// Neither a forged ID mapping nor an out-of-home record can authorize a grant.
// A folder that gives access to one of Desktop's own stores (see
// StoreRoute.CoveredBy) is skipped too: the sandbox refuses to start with a
// write grant on it, so granting it would block every start.
func Worktrees(port int) (worktrees []string, skipped []SkippedWorktree, err error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, nil, err
	}
	home, err = filepath.EvalSymlinks(home)
	if err != nil {
		return nil, nil, err
	}
	storeRoutes := desktopStoreRoutes()
	raw, ids, diagnostics := desktopProjects(port)
	skipped = append(skipped, diagnostics...)
	direct := map[string]bool{}
	for _, path := range raw {
		direct[path] = true
	}
	records, diagnostics := projectRecords()
	skipped = append(skipped, diagnostics...)
	for _, rec := range records {
		if direct[rec.Worktree] {
			continue
		}
		wt, err := filepath.EvalSymlinks(rec.Worktree)
		if ids[rec.ID] && err == nil && idMatchesWorktree(rec.ID, wt) {
			raw = append(raw, wt)
		} else if rec.Worktree != "" && rec.Worktree != "/" && filepath.IsAbs(rec.Worktree) {
			skipped = append(skipped, SkippedWorktree{Path: rec.Worktree, Reason: SkipUntrusted})
		}
	}
	seen := map[string]bool{}
	for _, path := range raw {
		if path == "" || path == "/" || !filepath.IsAbs(path) {
			continue
		}
		wt, err := filepath.EvalSymlinks(path)
		if err != nil {
			skipped = append(skipped, SkippedWorktree{Path: path, Reason: SkipStale})
			continue
		}
		// A previously writable parent could reroute a stored spelling to
		// another repository between sessions. Only grant direct paths.
		if wt != filepath.Clean(path) {
			skipped = append(skipped, SkippedWorktree{Path: path, Reason: SkipUntrusted})
			continue
		}
		if seen[wt] {
			continue
		}
		if isBroadWorktree(wt, home) {
			skipped = append(skipped, SkippedWorktree{Path: wt, Reason: SkipUntrusted})
			continue
		}
		seen[wt] = true
		if fi, serr := os.Stat(wt); serr != nil || !fi.IsDir() {
			skipped = append(skipped, SkippedWorktree{Path: wt, Reason: SkipStale})
			continue
		}
		if coversAnyStore(wt, storeRoutes) {
			skipped = append(skipped, SkippedWorktree{Path: wt, Reason: SkipDesktopStore})
			continue
		}
		worktrees = append(worktrees, wt)
	}
	worktrees = collapseNested(worktrees)
	// A record can be untrusted in one source and authorized in another.
	var rejected []SkippedWorktree
	for _, s := range skipped {
		if s.Reason == SkipInvalidSource {
			rejected = append(rejected, s)
			continue
		}
		covered := false
		canonical, _ := filepath.EvalSymlinks(s.Path)
		for _, wt := range worktrees {
			if canonical == wt || strings.HasPrefix(canonical, wt+string(filepath.Separator)) {
				covered = true
				break
			}
		}
		if !covered {
			rejected = append(rejected, s)
		}
	}
	skipped = rejected
	sort.Slice(skipped, func(i, j int) bool {
		return skipped[i].Path < skipped[j].Path
	})
	return worktrees, skipped, nil
}

// collapseNested sorts paths and drops every path that lies inside
// another path in the list (lexicographic sort puts ancestors first).
func collapseNested(paths []string) []string {
	sort.Strings(paths)
	var out []string
	for _, p := range paths {
		covered := false
		for _, kept := range out {
			if p == kept || strings.HasPrefix(p, kept+string(filepath.Separator)) {
				covered = true
				break
			}
		}
		if !covered {
			out = append(out, p)
		}
	}
	return out
}

func projectRecords() ([]projectRecord, []SkippedWorktree) {
	dir, err := storageProjectDir()
	if err != nil {
		return nil, nil
	}
	var out []projectRecord
	var diagnostics []SkippedWorktree
	f, err := os.OpenFile(dir, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	var entries []os.DirEntry
	if err == nil {
		entries, err = f.ReadDir(maxRecords + 1)
		f.Close()
		if err == io.EOF {
			err = nil
		}
	}
	if err != nil && !os.IsNotExist(err) {
		diagnostics = append(diagnostics, SkippedWorktree{Path: dir, Reason: SkipInvalidSource})
	}
	if len(entries) > maxRecords {
		entries = nil
		diagnostics = append(diagnostics, SkippedWorktree{Path: dir, Reason: SkipInvalidSource})
	}
	total := 0
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		data, rerr := readRegular(filepath.Join(dir, e.Name()), metadataMaxBytes)
		if rerr != nil {
			continue
		}
		total += len(data)
		if total > resultMaxBytes {
			out = nil
			diagnostics = append(diagnostics, SkippedWorktree{Path: dir, Reason: SkipInvalidSource})
			break
		}
		var rec projectRecord
		if json.Unmarshal(data, &rec) != nil {
			continue
		}
		out = append(out, rec)
	}
	path := filepath.Join(filepath.Dir(filepath.Dir(dir)), "opencode.db")
	rows, err := readSQLite(path, "project", `SELECT id, worktree FROM project WHERE length(CAST(id AS BLOB)) <= 4096 AND length(CAST(worktree AS BLOB)) <= 1048576 LIMIT 10001`)
	if err != nil && !os.IsNotExist(err) {
		diagnostics = append(diagnostics, SkippedWorktree{Path: path, Reason: SkipInvalidSource})
	}
	for _, row := range rows {
		out = append(out, projectRecord{ID: row[0], Worktree: row[1]})
	}
	return out, diagnostics
}

// desktopAppIDs are the OpenCode Desktop app IDs, stable first.
var desktopAppIDs = []string{"ai.opencode.desktop", "ai.opencode.desktop.beta"}

// desktopDirs lists, each once, every directory where OpenCode Desktop
// may keep its own files: for the stable and the beta app, the Electron
// userData dirs and the older XDG data dirs (see desktopStateCandidates).
// Project discovery reads opencode.global.dat from each of them, and
// DesktopStorePaths protects the stores in each of them, so the two cover
// the same places.
func desktopDirs(home string) []string {
	var dirs []string
	for _, id := range desktopAppIDs {
		dirs = append(dirs, desktopStateCandidates(home, id)...)
	}
	return uniquePaths(dirs)
}

// DesktopStorePaths returns the files where OpenCode Desktop keeps its
// project list and saved servers, in every directory from desktopDirs and
// in $XDG_STATE_HOME. Missing stores and SQLite companion files are
// included: Desktop can create them after the sandbox started.
// $XDG_STATE_HOME counts even when the home directory is unknown, so the
// result is empty only when neither is available.
func DesktopStorePaths() []string {
	var dirs []string
	if home, err := os.UserHomeDir(); err == nil {
		dirs = desktopDirs(home)
	}
	if state := absEnv("XDG_STATE_HOME"); state != "" {
		dirs = uniquePaths(append(dirs, state))
	}
	var out []string
	for _, dir := range dirs {
		for _, name := range []string{"opencode.global.dat", "drafts.sqlite", "drafts.sqlite-wal", "drafts.sqlite-shm", "drafts.sqlite-journal"} {
			out = append(out, filepath.Join(dir, name))
		}
	}
	return out
}

// absEnv returns the environment variable's value if it holds an absolute
// path, else "". The XDG Base Directory spec says to ignore relative
// values: XDG_STATE_HOME=state counts as unset.
func absEnv(name string) string {
	if v := os.Getenv(name); filepath.IsAbs(v) {
		return v
	}
	return ""
}

// uniquePaths returns paths cleaned, without duplicates, in their order.
func uniquePaths(paths []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range paths {
		p = filepath.Clean(p)
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

// DesktopStateDir returns the OpenCode Desktop app's data directory —
// the location the Desktop exposes to its child processes as
// XDG_STATE_HOME. Its opencode child keeps state in <dir>/opencode; the
// Desktop's own stores (DesktopStorePaths) sit next to it.
//
// `omac serve --for-opencode-desktop` uses this to point the inner
// opencode at the same state dir the Desktop uses when the launching
// shell didn't already set XDG_STATE_HOME (e.g. a manual run from a
// terminal). Without it the inner reads an empty state dir, provider
// credentials don't resolve, and opencode's /config/providers 500s —
// which surfaces to clients like `opencode attach` as an opaque
// "Unexpected server error".
// In the sandbox only <dir>/opencode is writable; the Desktop's stores
// are denied to the agent.
//
// It prefers an existing stable-then-beta directory (so a machine with
// only the beta installed still works, and so the actual on-disk
// location is used regardless of platform); when none exists yet it
// returns the platform-native default. Returns "" only if the home
// directory can't be resolved.
func DesktopStateDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	for _, appID := range desktopAppIDs {
		for _, dir := range desktopStateCandidates(home, appID) {
			if fi, statErr := os.Stat(dir); statErr == nil && fi.IsDir() {
				return dir
			}
		}
	}
	return defaultDesktopStateDir(home)
}

// desktopStateCandidates lists the per-user directories the OpenCode
// Desktop (an Electron app) may use for its data/state — the location it
// points opencode at via XDG_STATE_HOME (opencode then keeps its state
// under <dir>/opencode). Electron's userData default is
// ~/Library/Application Support/<id> on macOS and
// $XDG_CONFIG_HOME (~/.config)/<id> on Linux; the XDG data dir is also
// probed for older layouts. Candidates are listed for every platform
// (not branched on GOOS) so the on-disk Stat check finds the real one on
// any host and unit tests stay platform-independent. Most likely first.
// Relative XDG values are skipped, as the XDG Base Directory spec asks.
func desktopStateCandidates(home, appID string) []string {
	dirs := []string{
		filepath.Join(home, "Library", "Application Support", appID), // macOS (Electron userData)
	}
	if x := absEnv("XDG_CONFIG_HOME"); x != "" { // Linux (Electron userData)
		dirs = append(dirs, filepath.Join(x, appID))
	}
	dirs = append(dirs, filepath.Join(home, ".config", appID)) // Linux default
	if x := absEnv("XDG_DATA_HOME"); x != "" {                 // legacy/data layout
		dirs = append(dirs, filepath.Join(x, appID))
	}
	dirs = append(dirs, filepath.Join(home, ".local", "share", appID))
	return dirs
}

// defaultDesktopStateDir is the platform-native Electron userData path
// for the stable Desktop app, used only when no Desktop directory exists
// on disk yet (a degenerate case: --for-opencode-desktop with no Desktop
// installed). macOS: ~/Library/Application Support/<id>; Linux:
// $XDG_CONFIG_HOME (~/.config)/<id>.
func defaultDesktopStateDir(home string) string {
	const appID = "ai.opencode.desktop"
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Application Support", appID)
	}
	if xdg := absEnv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, appID)
	}
	return filepath.Join(home, ".config", appID)
}

// isBroadWorktree reports whether wt is too broad to grant read+write:
// $HOME itself and any ancestor of it (e.g. /home, /) are rejected.
func isBroadWorktree(wt, home string) bool {
	clean := filepath.Clean(wt)
	homeClean := filepath.Clean(home)
	for d := homeClean; ; d = filepath.Dir(d) {
		if d == clean {
			return true
		}
		if d == filepath.Dir(d) {
			return false
		}
	}
}

// unwrapValue returns the inner JSON when raw is a JSON-encoded string
// (the Desktop store's common double-encoding), otherwise raw itself.
func unwrapValue(raw json.RawMessage) []byte {
	var asString string
	if json.Unmarshal(raw, &asString) == nil {
		return []byte(asString)
	}
	return raw
}

func coversAnyStore(dir string, routes []StoreRoute) bool {
	for _, route := range routes {
		if route.CoveredBy(dir) {
			return true
		}
	}
	return false
}
