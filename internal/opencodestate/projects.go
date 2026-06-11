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

// desktopWorktrees reads the OpenCode Desktop app's own project store
// (key "globalSync.project" in opencode.global.dat). Folders opened in
// the Desktop UI may exist only here, not yet in the CLI's storage/db.
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

// desktopGlobalWorktrees extracts project worktrees from one
// opencode.global.dat file. The file is JSON; the "globalSync.project"
// value is itself a JSON-encoded string of {"value":[{"worktree":...}]}.
func desktopGlobalWorktrees(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var top map[string]json.RawMessage
	if json.Unmarshal(data, &top) != nil {
		return nil
	}
	rawProj, ok := top["globalSync.project"]
	if !ok {
		return nil
	}
	// The value may be double-encoded (a JSON string containing JSON)
	// or a plain object; handle both.
	var inner []byte = rawProj
	var asString string
	if json.Unmarshal(rawProj, &asString) == nil {
		inner = []byte(asString)
	}
	var sync struct {
		Value []projectRecord `json:"value"`
	}
	if json.Unmarshal(inner, &sync) != nil {
		return nil
	}
	var out []string
	for _, rec := range sync.Value {
		out = append(out, rec.Worktree)
	}
	return out
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
