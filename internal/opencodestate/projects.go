// Package opencodestate reads project records from the local OpenCode
// installation so `omac serve --for-opencode-desktop` can grant every
// known project worktree to the sandbox up front.
//
// OpenCode persists projects twice (storage JSON files and a SQLite
// db), and the two can drift: recently opened projects may exist only
// in the db. We read both — the storage files as plain JSON, and the
// db via the sqlite3 CLI when available (no driver dependency) — and
// merge. The pseudo-project with worktree "/" (OpenCode's "global"
// record) is always skipped, as are worktrees that no longer exist.
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
