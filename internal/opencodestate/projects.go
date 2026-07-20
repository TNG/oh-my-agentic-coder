// Package opencodestate reads project records from the local OpenCode
// installation so `omac serve --for-opencode-desktop` can grant every
// known project worktree to the sandbox up front.
//
// OpenCode persists projects in three places that all drift apart:
//   - storage JSON files under ~/.local/share/opencode/storage/project/
//   - the opencode.db SQLite `project` table
//   - the Desktop app's own store (opencode.global.dat, key
//     "globalSync.project") under the ai.opencode.desktop[.beta] data
//     dir — folders opened in the Desktop UI can exist ONLY here.
//
// We read all three (the db via the sqlite3 CLI when available — no
// driver dependency) and merge. The pseudo-project with worktree "/"
// (OpenCode's "global" record) is always skipped, as are worktrees
// that no longer exist.
package opencodestate

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// projectRecord is the subset of OpenCode's project JSON we care about.
type projectRecord struct {
	ID       string `json:"id"`
	Worktree string `json:"worktree"`
}

// storageProjectDir returns ~/.local/share/opencode/storage/project.
func storageProjectDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", "opencode", "storage", "project"), nil
}

// Worktrees returns the deduplicated, existing project worktree
// directories recorded by OpenCode (storage files + db), sorted.
// Worktrees nested inside another recorded worktree are collapsed
// into the ancestor: granting the parent already covers the child, so
// emitting both would only add redundant sandbox rules.
// Missing state (OpenCode not installed / never run) yields an empty
// list, not an error. skipped receives worktrees that were recorded
// but no longer exist.
func Worktrees() (worktrees []string, skipped []string, err error) {
	raw, err := storageWorktrees()
	if err != nil {
		return nil, nil, err
	}
	raw = append(raw, dbWorktrees()...)
	raw = append(raw, desktopWorktrees()...)

	seen := map[string]bool{}
	for _, wt := range raw {
		// Skip the global pseudo-project and anything non-absolute.
		if wt == "" || wt == "/" || !filepath.IsAbs(wt) || seen[wt] {
			continue
		}
		seen[wt] = true
		if fi, serr := os.Stat(wt); serr != nil || !fi.IsDir() {
			skipped = append(skipped, wt)
			continue
		}
		worktrees = append(worktrees, wt)
	}
	sort.Strings(skipped)
	return collapseNested(worktrees), skipped, nil
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

// storageWorktrees reads the JSON records under storage/project.
func storageWorktrees() ([]string, error) {
	dir, err := storageProjectDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		data, rerr := os.ReadFile(filepath.Join(dir, e.Name()))
		if rerr != nil {
			continue
		}
		var rec projectRecord
		if json.Unmarshal(data, &rec) != nil {
			continue
		}
		out = append(out, rec.Worktree)
	}
	return out, nil
}

// desktopWorktrees reads the OpenCode Desktop app's own state. Folders
// opened in the Desktop UI may exist only here, not yet in the CLI's
// storage/db. Two kinds of records are harvested from
// opencode.global.dat:
//   - "globalSync.project": the saved/pinned project list.
//   - "layout.page" -> lastProjectSession: the directories of currently
//     open tabs/windows, which are NOT necessarily in globalSync.project.
//
// Both the stable and beta app data dirs are consulted. Best-effort:
// missing files or unexpected shapes yield nil.
func desktopWorktrees() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	var out []string
	for _, appID := range []string{"ai.opencode.desktop", "ai.opencode.desktop.beta"} {
		for _, base := range desktopDataDirs(home, appID) {
			out = append(out, desktopGlobalWorktrees(filepath.Join(base, "opencode.global.dat"))...)
		}
	}
	return out
}

// desktopDataDirs returns the platform candidates for a desktop app's
// data directory.
func desktopDataDirs(home, appID string) []string {
	dirs := []string{
		filepath.Join(home, "Library", "Application Support", appID), // macOS
		filepath.Join(home, ".local", "share", appID),                // Linux
	}
	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
		dirs = append(dirs, filepath.Join(xdg, appID))
	}
	return dirs
}

// DesktopStateDir returns the OpenCode Desktop app's data directory —
// the location the Desktop exposes to its child processes as
// XDG_STATE_HOME so they share its auth, credentials and config.
//
// `omac serve --for-opencode-desktop` uses this to point the inner
// opencode at the same state store the Desktop uses when the launching
// shell didn't already set XDG_STATE_HOME (e.g. a manual run from a
// terminal). Without it the inner reads an empty state dir, provider
// credentials don't resolve, and opencode's /config/providers 500s —
// which surfaces to clients like `opencode attach` as an opaque
// "Unexpected server error".
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
	for _, appID := range []string{"ai.opencode.desktop", "ai.opencode.desktop.beta"} {
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
func desktopStateCandidates(home, appID string) []string {
	dirs := []string{
		filepath.Join(home, "Library", "Application Support", appID), // macOS (Electron userData)
	}
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" { // Linux (Electron userData)
		dirs = append(dirs, filepath.Join(x, appID))
	}
	dirs = append(dirs, filepath.Join(home, ".config", appID)) // Linux default
	if x := os.Getenv("XDG_DATA_HOME"); x != "" {              // legacy/data layout
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
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, appID)
	}
	return filepath.Join(home, ".config", appID)
}

// desktopGlobalWorktrees extracts project directories from one
// opencode.global.dat file: the saved project list ("globalSync.project")
// plus the directories of currently open tabs ("layout.page" ->
// lastProjectSession). The top-level values are frequently
// double-encoded (a JSON string containing JSON); unwrapValue handles
// both shapes.
func desktopGlobalWorktrees(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var top map[string]json.RawMessage
	if json.Unmarshal(data, &top) != nil {
		return nil
	}
	var out []string

	// 1. Saved/pinned projects.
	if raw, ok := top["globalSync.project"]; ok {
		var sync struct {
			Value []projectRecord `json:"value"`
		}
		if json.Unmarshal(unwrapValue(raw), &sync) == nil {
			for _, rec := range sync.Value {
				out = append(out, rec.Worktree)
			}
		}
	}

	// 2. Currently open tabs/windows. layout.page.lastProjectSession is
	//    a map keyed by directory, each value carrying a "directory"
	//    field too. Harvest the keys (the directories) directly.
	if raw, ok := top["layout.page"]; ok {
		var page struct {
			LastProjectSession map[string]struct {
				Directory string `json:"directory"`
			} `json:"lastProjectSession"`
		}
		if json.Unmarshal(unwrapValue(raw), &page) == nil {
			for dir, sess := range page.LastProjectSession {
				if sess.Directory != "" {
					out = append(out, sess.Directory)
				} else {
					out = append(out, dir)
				}
			}
		}
	}
	return out
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

// dbWorktrees scrapes the project table from opencode.db via the
// sqlite3 CLI. Best-effort: a missing db or missing sqlite3 binary
// yields nil. The db is opened read-only so a live OpenCode instance
// is not disturbed.
func dbWorktrees() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	db := filepath.Join(home, ".local", "share", "opencode", "opencode.db")
	if _, err := os.Stat(db); err != nil {
		return nil
	}
	sqlite, err := exec.LookPath("sqlite3")
	if err != nil {
		return nil
	}
	out, err := exec.Command(sqlite,
		"file:"+db+"?mode=ro", "SELECT worktree FROM project;").Output()
	if err != nil {
		return nil
	}
	var worktrees []string
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			worktrees = append(worktrees, line)
		}
	}
	return worktrees
}
