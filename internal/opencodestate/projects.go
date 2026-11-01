// Package opencodestate reads project records from the local OpenCode
// installation so `omac serve --for-opencode-desktop` can grant every
// known project worktree to the sandbox up front.
//
// OpenCode persists projects in three places that all drift apart:
//   - storage JSON files under ~/.local/share/opencode/storage/project/
//   - the opencode.db SQLite `project` table
//   - the Desktop app's own store "opencode.global.dat" under the
//     ai.opencode.desktop[.beta] data dir: a JSON file in older
//     Desktop builds, rows of drafts.sqlite in Desktop 1.18+. Folders
//     opened in the Desktop UI can exist ONLY here.
//
// We read all three (opencode.db via the sqlite3 CLI when available,
// drafts.sqlite via the pure-Go driver) and merge. The pseudo-project with worktree "/"
// (OpenCode's "global" record) is always skipped, as are worktrees
// that no longer exist. Projects the Desktop store names only by ID are
// resolved and verified in project_ids.go.
package opencodestate

import (
	"database/sql"
	"encoding/json"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	_ "modernc.org/sqlite" // read-only access to the Desktop's drafts.sqlite
)

// projectRecord is the subset of OpenCode's project JSON we care about.
type projectRecord struct {
	ID       string `json:"id"`
	Worktree string `json:"worktree"`
}

// worktreeOrigin records where a harvested worktree record came from.
// Only the Desktop store lives outside the sandbox's writable tree, so
// only it is trusted for in-home paths; the storage JSON files and the
// SQLite db both live under ~/.local/share/opencode, which the harness
// mounts read+write into the confined agent.
type worktreeOrigin int

const (
	originAgentWritable worktreeOrigin = iota // storage JSON or opencode.db
	originDesktop                             // Desktop app's store (opencode.global.dat / drafts.sqlite)
	// A project the Desktop store names only by ID, whose path was looked
	// up in the agent-writable sources and then checked against the ID
	// (see idMatchesWorktree).
	originVerifiedID
)

// SkipReason tells why Worktrees rejected a recorded worktree. The
// reason is for the human behind `omac serve`: stale means the folder
// no longer exists, untrusted means the folder exists inside $HOME
// but the record that named it comes from a source the confined
// process can write, so it cannot drive a grant.
type SkipReason string

const (
	SkipStale     SkipReason = "no longer exists"
	SkipUntrusted SkipReason = "untrusted for a grant decision (record from agent-writable OpenCode state names an in-home path)"
)

// SkippedWorktree pairs a rejected worktree path with its reason.
type SkippedWorktree struct {
	Path   string
	Reason SkipReason
}

// worktreeEntry pairs a harvested worktree path with its origin so the
// filter in Worktrees can apply a stricter rule to agent-writable sources.
type worktreeEntry struct {
	path   string
	origin worktreeOrigin
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
// but rejected, each with its reason.
//
// Records from agent-writable sources (the storage JSON tree and the
// opencode.db SQLite db, both under ~/.local/share/opencode) are not
// trusted for any path inside $HOME: the confined process can write
// them, so honoring an in-home record would let it steer a later
// read+write grant at a path where the user owns every file. Such
// records are routed into skipped. Records from the Desktop app's own
// store (outside the sandbox mount), and paths verified against a
// project ID from that store, keep the looser isBroadWorktree rule so
// the Desktop workflow still resolves in-home projects.
func Worktrees() (worktrees []string, skipped []SkippedWorktree, err error) {
	storage, err := storageWorktrees()
	if err != nil {
		return nil, nil, err
	}
	raw := append(storage, dbWorktrees()...)
	raw = append(raw, desktopWorktrees()...)

	home, _ := os.UserHomeDir()
	seen := map[string]bool{}
	for _, e := range raw {
		wt := e.path
		// Skip the global pseudo-project and anything non-absolute.
		if wt == "" || wt == "/" || !filepath.IsAbs(wt) || seen[wt] {
			continue
		}
		// Reject broad paths ($HOME and its ancestors) from every
		// source, and additionally any path inside $HOME from
		// agent-writable sources.
		if home != "" && rejectsForHome(wt, home, e.origin) {
			skipped = append(skipped, SkippedWorktree{Path: wt, Reason: SkipUntrusted})
			continue
		}
		seen[wt] = true
		if fi, serr := os.Stat(wt); serr != nil || !fi.IsDir() {
			skipped = append(skipped, SkippedWorktree{Path: wt, Reason: SkipStale})
			continue
		}
		worktrees = append(worktrees, wt)
	}
	sort.Slice(skipped, func(i, j int) bool {
		return skipped[i].Path < skipped[j].Path
	})
	return collapseNested(worktrees), skipped, nil
}

// rejectsForHome reports whether wt must be rejected from the grant
// list given the user's home directory and the record's origin. The
// Desktop origin is only rejected for $HOME and its ancestors; the
// agent-writable origins are rejected for any path inside $HOME
// (ancestor, equal, or strict descendant) because the user owns every
// file there and DAC offers no protection against a forged record.
func rejectsForHome(wt, home string, origin worktreeOrigin) bool {
	if origin == originDesktop || origin == originVerifiedID {
		return isBroadWorktree(wt, home)
	}
	return isInHome(wt, home)
}

// isInHome reports whether wt lies anywhere inside home: equal to it,
// an ancestor of it, or a strict descendant of it.
func isInHome(wt, home string) bool {
	clean := filepath.Clean(wt)
	homeClean := filepath.Clean(home)
	if clean == homeClean {
		return true
	}
	// wt is an ancestor of home.
	if strings.HasPrefix(homeClean, clean+string(filepath.Separator)) {
		return true
	}
	// wt is a descendant of home.
	if strings.HasPrefix(clean, homeClean+string(filepath.Separator)) {
		return true
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

// storageWorktrees reads the JSON records under storage/project. The
// storage tree lives under ~/.local/share/opencode, which the harness
// mounts read+write into the confined agent, so records from here are
// tagged originAgentWritable.
func storageWorktrees() ([]worktreeEntry, error) {
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
	var out []worktreeEntry
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
		out = append(out, worktreeEntry{path: rec.Worktree, origin: originAgentWritable})
	}
	return out, nil
}

// desktopWorktrees reads the OpenCode Desktop app's own state. Folders
// opened in the Desktop UI may exist only here, not yet in the CLI's
// storage/db.
//
// The Desktop keeps one logical store named "opencode.global.dat" in
// one of two physical forms, depending on its version:
//   - older (Tauri) builds: a JSON file <datadir>/opencode.global.dat;
//   - Desktop 1.18+ (Electron): rows of the `state` table in
//     <datadir>/drafts.sqlite with name = 'opencode.global.dat'. The
//     legacy file is gone after the app's one-time migration.
//
// Both forms are read, in the stable and beta app data dirs. See
// globalStoreWorktrees for which keys are harvested. Projects the store
// names only by ID are resolved through verifiedProjectWorktrees.
// Best-effort: missing files or unexpected shapes yield nil.
func desktopWorktrees() []worktreeEntry {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	var out []worktreeEntry
	var ids []string
	for _, appID := range []string{"ai.opencode.desktop", "ai.opencode.desktop.beta"} {
		for _, base := range desktopDataDirs(home, appID) {
			for _, top := range []map[string]json.RawMessage{
				readGlobalStoreFile(filepath.Join(base, "opencode.global.dat")),
				readGlobalStoreSQLite(filepath.Join(base, "drafts.sqlite")),
			} {
				for _, p := range globalStoreWorktrees(top) {
					out = append(out, worktreeEntry{path: p, origin: originDesktop})
				}
				ids = append(ids, globalStoreProjectIDs(top)...)
			}
		}
	}
	for _, p := range verifiedProjectWorktrees(ids) {
		out = append(out, worktreeEntry{path: p, origin: originVerifiedID})
	}
	return out
}

// desktopDataDirs returns the platform candidates for a desktop app's
// data directory: the legacy data dirs plus every Electron userData
// candidate from desktopStateCandidates (e.g. ~/.config/<id> on Linux),
// de-duplicated.
func desktopDataDirs(home, appID string) []string {
	dirs := []string{
		filepath.Join(home, "Library", "Application Support", appID), // macOS
		filepath.Join(home, ".local", "share", appID),                // Linux
	}
	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
		dirs = append(dirs, filepath.Join(xdg, appID))
	}
	dirs = append(dirs, desktopStateCandidates(home, appID)...)
	seen := map[string]bool{}
	out := dirs[:0]
	for _, d := range dirs {
		if !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	return out
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

// desktopGlobalWorktrees extracts project directories from one legacy
// opencode.global.dat JSON file (pre-1.18 Desktop). See
// globalStoreWorktrees for the keys harvested.
func desktopGlobalWorktrees(path string) []string {
	return globalStoreWorktrees(readGlobalStoreFile(path))
}

// readGlobalStoreFile loads a legacy opencode.global.dat JSON file as
// its top-level key -> value map, or nil when it is missing or invalid.
func readGlobalStoreFile(path string) map[string]json.RawMessage {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var top map[string]json.RawMessage
	if json.Unmarshal(data, &top) != nil {
		return nil
	}
	return top
}

// desktopSQLiteWorktrees extracts project directories from the Desktop
// 1.18+ store: the `state` table of drafts.sqlite, rows with
// name = 'opencode.global.dat'. Each row's key/value pair is one
// top-level key/value of the former JSON file, so the rows are folded
// into the same map and handed to globalStoreWorktrees.
//
// The db is opened read-only through the pure-Go driver, so a running
// Desktop is not disturbed and no sqlite3 binary is needed. Any error
// (no file, locked, different schema) yields nil.
func desktopSQLiteWorktrees(path string) []string {
	return globalStoreWorktrees(readGlobalStoreSQLite(path))
}

// readGlobalStoreSQLite loads the opencode.global.dat rows of a Desktop
// 1.18+ drafts.sqlite as a top-level key -> value map, or nil on any
// error.
func readGlobalStoreSQLite(path string) map[string]json.RawMessage {
	if fi, err := os.Stat(path); err != nil || !fi.Mode().IsRegular() {
		return nil
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout=500")
	if err != nil {
		return nil
	}
	defer db.Close()
	rows, err := db.Query(`SELECT key, value FROM state WHERE name = 'opencode.global.dat'`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	top := map[string]json.RawMessage{}
	for rows.Next() {
		var key, value string
		if rows.Scan(&key, &value) != nil {
			continue
		}
		top[key] = json.RawMessage(value)
	}
	if rows.Err() != nil {
		return nil
	}
	return top
}

// serverScopeSep separates the server URL from the key name in the
// Desktop's per-server keys, e.g. "http://localhost:4095\x00layout.page".
const serverScopeSep = "\x00"

// globalStoreWorktrees extracts project directories from the Desktop's
// "opencode.global.dat" store, given as its top-level key -> value map.
// Harvested records:
//
//  1. "globalSync.project": the saved/pinned project list of the
//     built-in (local sidecar) server.
//  2. "layout.page" -> lastProjectSession: directories of open tabs of
//     the built-in server, which are NOT necessarily pinned.
//  3. "<server URL>\x00layout.page": the same open-tab record for an
//     extra server added in the Desktop (e.g. an `omac serve` sandbox
//     registered as http://localhost:4095). Desktop 1.18+ scopes it per
//     server this way.
//  4. "server" -> projects -> <server URL>: the project list per extra
//     server.
//
// Records of remote servers (3 and 4) are ignored: their paths name
// folders on another machine, so granting a same-named local folder
// would be wrong. Values are often double-encoded (a JSON string
// containing JSON); unwrapValue handles both shapes.
func globalStoreWorktrees(top map[string]json.RawMessage) []string {
	var out []string

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

	for key, raw := range top {
		if key == "layout.page" {
			out = append(out, openTabDirs(raw)...)
			continue
		}
		server, name, scoped := strings.Cut(key, serverScopeSep)
		if scoped && name == "layout.page" && isLocalServer(server) {
			out = append(out, openTabDirs(raw)...)
		}
	}

	if raw, ok := top["server"]; ok {
		var srv struct {
			Projects map[string][]projectRecord `json:"projects"`
		}
		if json.Unmarshal(unwrapValue(raw), &srv) == nil {
			for server, recs := range srv.Projects {
				if !isLocalServer(server) {
					continue
				}
				for _, rec := range recs {
					out = append(out, rec.Worktree)
				}
			}
		}
	}
	return out
}

// openTabDirs returns the directories of a layout.page value's
// lastProjectSession map (keyed by directory, each value carrying a
// "directory" field too).
func openTabDirs(raw json.RawMessage) []string {
	var page struct {
		LastProjectSession map[string]struct {
			Directory string `json:"directory"`
		} `json:"lastProjectSession"`
	}
	if json.Unmarshal(unwrapValue(raw), &page) != nil {
		return nil
	}
	var out []string
	for dir, sess := range page.LastProjectSession {
		if sess.Directory != "" {
			out = append(out, sess.Directory)
		} else {
			out = append(out, dir)
		}
	}
	return out
}

// isLocalServer reports whether a Desktop server key names a server on
// this machine: the built-in one ("local" / "sidecar") or an http(s)
// URL on a loopback host. Example: "http://localhost:4095" is local,
// "https://build-box:4096" is not.
func isLocalServer(server string) bool {
	switch server {
	case "local", "sidecar":
		return true
	}
	u, err := url.Parse(server)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	host := u.Hostname()
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
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

// dbWorktrees scrapes the project table from opencode.db via the
// sqlite3 CLI. Best-effort: a missing db or missing sqlite3 binary
// yields nil. The db is opened read-only so a live OpenCode instance
// is not disturbed. The db lives under ~/.local/share/opencode, which
// the harness mounts read+write into the confined agent, so records
// from here are tagged originAgentWritable.
func dbWorktrees() []worktreeEntry {
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
	var entries []worktreeEntry
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			entries = append(entries, worktreeEntry{path: line, origin: originAgentWritable})
		}
	}
	return entries
}
