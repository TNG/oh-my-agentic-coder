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
// We read all three with bounded JSON and pure-Go SQLite readers and merge.
// The pseudo-project with worktree "/"
// (OpenCode's "global" record) is always skipped, as are worktrees
// that no longer exist. Projects the Desktop store names only by ID are
// resolved and verified in project_ids.go.
package opencodestate

import (
	"encoding/json"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
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
	SkipUntrusted SkipReason = "not granted: the sandbox can edit OpenCode's own project list, so an in-home folder is only granted when Desktop's store verifies it; to grant it anyway, pass --allow <dir>"
	// SkipInvalidSource uses Path for the source, not a worktree. No record
	// contents or underlying parser/database error are included.
	SkipInvalidSource SkipReason = "invalid, oversized, or unreadable discovery source (ignored)"
)

// SkippedWorktree pairs a rejected worktree or discovery source with its reason.
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
// but rejected, each with its reason. Invalid sources are reported with
// SkipInvalidSource without discarding grants from independent valid sources.
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
//
// Worktrees reads the projects of every loopback server in the Desktop
// store. Use WorktreesForPort to limit that to the server omac runs.
func Worktrees() (worktrees []string, skipped []SkippedWorktree, err error) {
	return WorktreesForPort(0)
}

// WorktreesForPort is Worktrees for a launch whose server listens on
// servePort. A loopback server of the Desktop store (for example
// "http://localhost:3000" or a second omac instance) contributes
// projects only if it uses that port. The built-in Desktop server
// ("local" / "sidecar") always counts, because the folders a user picks
// in the Desktop are recorded there first. servePort <= 0 accepts every
// loopback server.
func WorktreesForPort(servePort int) (worktrees []string, skipped []SkippedWorktree, err error) {
	scope := serverScope{port: servePort}
	records, diagnostics, err := readProjectSources()
	if err != nil {
		return nil, nil, err
	}
	var raw []worktreeEntry
	for _, rec := range records {
		raw = append(raw, worktreeEntry{path: rec.Worktree, origin: originAgentWritable})
	}
	desktop, desktopDiagnostics := desktopWorktreesFromRecords(records, scope)
	raw = append(raw, desktop...)
	skipped = append(diagnostics, desktopDiagnostics...)

	home, _ := os.UserHomeDir()
	if canonical, herr := filepath.EvalSymlinks(home); herr == nil {
		home = canonical
	}
	seen := map[string]bool{}
	for _, e := range raw {
		wt := e.path
		// Skip the global pseudo-project and anything non-absolute.
		if wt == "" || wt == "/" || !filepath.IsAbs(wt) {
			continue
		}
		wt, serr := canonicalWorktree(wt)
		if serr != nil {
			skipped = append(skipped, SkippedWorktree{Path: e.path, Reason: SkipStale})
			continue
		}
		if seen[wt] {
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
	// Appending a separator to root would make the prefix "//".
	if clean == string(filepath.Separator) || homeClean == string(filepath.Separator) {
		return true
	}
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
// names only by ID are resolved through verifiedProjectRecords.
// Best-effort: missing files or unexpected shapes yield nil.
func desktopWorktreesFromRecords(records []projectRecord, scope serverScope) ([]worktreeEntry, []SkippedWorktree) {
	var out []worktreeEntry
	var ids []string
	var diagnostics []SkippedWorktree
	for _, base := range desktopStoreDirs() {
		for _, name := range []string{"opencode.global.dat", "drafts.sqlite"} {
			path := filepath.Join(base, name)
			var top map[string]json.RawMessage
			var err error
			if name == "drafts.sqlite" {
				top, err = readDesktopSQLite(path)
			} else {
				top, err = readDesktopFile(path)
			}
			diagnostics = append(diagnostics, sourceDiagnostic(path, err)...)
			if err != nil {
				continue
			}
			for _, p := range globalStoreWorktrees(top, scope) {
				out = append(out, worktreeEntry{path: p, origin: originDesktop})
			}
			ids = append(ids, globalStoreProjectIDs(top, scope)...)
		}
	}
	for _, p := range verifiedProjectRecords(ids, records) {
		out = append(out, worktreeEntry{path: p, origin: originVerifiedID})
	}
	return out, diagnostics
}

// desktopStoreDirs is the single enumeration used for reads and protection.
// Keep literal absolute spellings: callers must not expand runtime metacharacters.
func desktopStoreDirs() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	var dirs []string
	for _, appID := range []string{"ai.opencode.desktop", "ai.opencode.desktop.beta"} {
		dirs = append(dirs, desktopDataDirs(home, appID)...)
	}
	if state := os.Getenv("XDG_STATE_HOME"); state != "" {
		dirs = append(dirs, state)
	}
	var out []string
	seen := map[string]bool{}
	for _, dir := range dirs {
		absolute, err := filepath.Abs(dir)
		if err == nil && !seen[absolute] {
			seen[absolute] = true
			out = append(out, absolute)
		}
	}
	return out
}

// DesktopStorePaths returns all literal absolute paths consulted for Desktop
// discovery, including absent stores and SQLite companions that need protection.
func DesktopStorePaths() []string {
	var paths []string
	for _, base := range desktopStoreDirs() {
		for _, name := range []string{"drafts.sqlite", "drafts.sqlite-wal", "drafts.sqlite-shm", "drafts.sqlite-journal", "opencode.global.dat"} {
			paths = append(paths, filepath.Join(base, name))
		}
	}
	return paths
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

// readGlobalStoreFile loads a legacy opencode.global.dat JSON file
// (pre-1.18 Desktop) as its top-level key -> value map, or nil when it
// is missing or invalid.
func readGlobalStoreFile(path string) map[string]json.RawMessage {
	top, _ := readDesktopFile(path)
	return top
}

// readGlobalStoreSQLite loads the Desktop 1.18+ store: the `state`
// table of drafts.sqlite, rows with name = 'opencode.global.dat'. Each
// row's key/value pair is one top-level key/value of the former JSON
// file, so the rows are folded into the same map readGlobalStoreFile
// returns.
//
// The db is opened read-only through the pure-Go driver, so a running
// Desktop is not disturbed and no sqlite3 binary is needed. Any error
// (no file, locked, different schema) yields nil.
func readGlobalStoreSQLite(path string) map[string]json.RawMessage {
	top, _ := readDesktopSQLite(path)
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
func globalStoreWorktrees(top map[string]json.RawMessage, scope serverScope) []string {
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
		if scoped && name == "layout.page" && scope.includes(server) {
			out = append(out, openTabDirs(raw)...)
		}
	}

	if raw, ok := top["server"]; ok {
		var srv struct {
			Projects map[string][]projectRecord `json:"projects"`
		}
		if json.Unmarshal(unwrapValue(raw), &srv) == nil {
			for server, recs := range srv.Projects {
				if !scope.includes(server) {
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

// serverScope selects which Desktop server keys contribute projects.
type serverScope struct {
	port int // port of the server omac runs; <= 0 accepts any loopback port
}

// includes reports whether a Desktop server key names a server whose
// projects may be granted: the built-in one ("local" / "sidecar"), or an
// http(s) URL on a loopback host that uses the scope's port. Examples
// with port 4095: "http://localhost:4095" is included,
// "http://localhost:3000" and "https://build-box:4096" are not.
func (sc serverScope) includes(server string) bool {
	switch server {
	case "local", "sidecar":
		return true
	}
	u, err := url.Parse(server)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	host := u.Hostname()
	if host != "localhost" {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return false
		}
	}
	if sc.port <= 0 {
		return true
	}
	return urlPort(u) == sc.port
}

// urlPort returns the explicit port of u, or the scheme default.
func urlPort(u *url.URL) int {
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil {
			return 0
		}
		return n
	}
	if u.Scheme == "https" {
		return 443
	}
	return 80
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
