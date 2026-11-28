// Package opencodestate reads project records from the OpenCode Desktop
// app's own store so `omac serve --for-opencode-desktop` can grant every
// known project worktree to the sandbox up front.
//
// OpenCode also persists projects in storage JSON files and an
// opencode.db SQLite table under ~/.local/share/opencode, which the
// harness mounts read+write into the confined agent. Those files are
// therefore not a trustworthy source for grant decisions and are ignored.
// The Desktop store (opencode.global.dat, keys "globalSync.project" and
// "layout.page") is outside that writable tree and remains the only
// record source; folders opened in the Desktop UI can exist only there.
//
// Even for the Desktop store, Worktrees rejects records whose paths are
// in the user's home, shared temp locations, filesystem roots, or
// world-writable/sticky directories. The pseudo-project with worktree "/"
// (OpenCode's "global" record) is always skipped, as are worktrees that no
// longer exist.
package opencodestate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/TNG/oh-my-agentic-coder/internal/pathutil"
)

// projectRecord is the subset of OpenCode's project JSON we care about.
type projectRecord struct {
	ID       string `json:"id"`
	Worktree string `json:"worktree"`
}

// SkipReason tells why Worktrees rejected a recorded worktree. The
// reason is for the human behind `omac serve`: stale means the folder
// no longer exists, untrusted means the recorded path is too broad or
// sits in a location that cannot safely receive an automatic grant,
// such as the user's home, a shared temp root, or a world-writable or
// sticky directory.
type SkipReason string

const (
	SkipStale     SkipReason = "no longer exists"
	SkipUntrusted SkipReason = "untrusted for an automatic grant (in-home, shared-temp, filesystem-root, or world-writable/sticky path)"
)

// SkippedWorktree pairs a rejected worktree path with its reason.
type SkippedWorktree struct {
	Path   string
	Reason SkipReason
}

// Worktrees returns the deduplicated, existing project worktree
// directories recorded by the OpenCode Desktop app, sorted.
// Worktrees nested inside another recorded worktree are collapsed
// into the ancestor: granting the parent already covers the child, so
// emitting both would only add redundant sandbox rules.
// Missing state (OpenCode not installed / never run) yields an empty
// list, not an error. skipped receives worktrees that were recorded
// but rejected, each with its reason.
//
// Records are only read from the Desktop store, which the confined
// process cannot write. Each recorded path is still validated by
// content: paths in $HOME or its ancestors, shared temp roots,
// filesystem roots, and world-writable or sticky directories are routed
// into skipped instead of becoming grants.
func Worktrees() (worktrees []string, skipped []SkippedWorktree, err error) {
	raw := desktopWorktrees()

	home, _ := os.UserHomeDir()
	seen := map[string]bool{}
	for _, wt := range raw {
		// Skip the global pseudo-project and anything non-absolute.
		if wt == "" || wt == "/" || !filepath.IsAbs(wt) || seen[wt] {
			continue
		}
		// Reject broad or unsafe paths regardless of which record
		// named them: $HOME and its ancestors, anything inside
		// $HOME, shared temp roots, and filesystem roots.
		if isInHome(wt, home) || isSharedTempPath(wt) || isFilesystemRoot(wt) {
			skipped = append(skipped, SkippedWorktree{Path: wt, Reason: SkipUntrusted})
			continue
		}
		seen[wt] = true
		fi, serr := os.Stat(wt)
		if serr != nil || !fi.IsDir() {
			skipped = append(skipped, SkippedWorktree{Path: wt, Reason: SkipStale})
			continue
		}
		if fi.Mode().Perm()&0o002 != 0 || fi.Mode()&os.ModeSticky != 0 {
			skipped = append(skipped, SkippedWorktree{Path: wt, Reason: SkipUntrusted})
			continue
		}
		worktrees = append(worktrees, wt)
	}
	sort.Slice(skipped, func(i, j int) bool {
		return skipped[i].Path < skipped[j].Path
	})
	return collapseNested(worktrees), skipped, nil
}

// isInHome reports whether wt lies anywhere inside home: equal to it,
// an ancestor of it, or a strict descendant of it. Both the recorded
// spelling and the resolved location are checked, so a link or platform
// alias cannot hide a protected target.
func isInHome(wt, home string) bool {
	wtForms := pathForms(wt)
	for _, form := range wtForms {
		if isFilesystemRoot(form) {
			return true
		}
	}
	if home == "" {
		return false
	}
	homeForms := pathForms(home)
	for _, w := range wtForms {
		for _, h := range homeForms {
			// wt is $HOME itself or an ancestor of it.
			if isBroadWorktree(w, h) {
				return true
			}
			// wt is a descendant of home.
			if pathWithin(h, w) {
				return true
			}
		}
	}
	return false
}

// pathForms returns the cleaned spelling and, when it differs, the
// symlink-resolved location.
func pathForms(p string) []string {
	if p == "" {
		return nil
	}
	clean := filepath.Clean(p)
	forms := []string{clean}
	if resolved := pathutil.Canonical(p); resolved != "" && resolved != clean {
		forms = append(forms, resolved)
	}
	return forms
}

// pathWithin reports whether p equals root or lies below it, using a
// separator-aware comparison rather than a raw string prefix.
func pathWithin(root, p string) bool {
	if root == "" || p == "" {
		return false
	}
	root = filepath.Clean(root)
	p = filepath.Clean(p)
	return p == root || strings.HasPrefix(p, root+string(filepath.Separator))
}

// isFilesystemRoot reports whether p is a filesystem root or has no
// non-root components (e.g. "/" or a bare volume name).
func isFilesystemRoot(p string) bool {
	clean := filepath.Clean(p)
	return clean == string(filepath.Separator) || clean == filepath.Dir(clean)
}

// isSharedTempPath reports whether p is one of the machine-wide temp
// roots or lies inside one. macOS uses a per-user $TMPDIR, so it is
// still rejected: an explicit Desktop record pointing there is not a
// project the user opened. Roots and the record are compared in both
// their lexical and symlink-resolved forms so platform aliases match.
func isSharedTempPath(p string) bool {
	roots := []string{"/tmp", "/var/tmp", "/dev/shm", os.TempDir()}
	pathFormsList := pathForms(p)
	for _, root := range roots {
		if root == "" {
			continue
		}
		for _, rootForm := range pathForms(root) {
			for _, pathForm := range pathFormsList {
				if pathWithin(rootForm, pathForm) {
					return true
				}
			}
		}
	}
	return false
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
