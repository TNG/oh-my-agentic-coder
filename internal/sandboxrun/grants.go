// Package sandboxrun implements `omac sandbox run`: it resolves a
// sandboxprofile into a concrete grant set, starts the filtering
// proxy, and launches the inner command under the platform kernel
// sandbox (Seatbelt via sandbox-exec on macOS, bubblewrap + Landlock
// on Linux).
package sandboxrun

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/TNG/oh-my-agentic-coder/internal/sandboxdeny"
	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

// Grants is the fully resolved, expanded, existence-filtered input to
// the platform backends.
type Grants struct {
	Workdir string // absolute; always the child's cwd
	HomeDir string // literal directory access for macOS home resolution; not recursive

	// Path grants (expanded, absolute, existing).
	ReadPaths  []string // read-only
	WritePaths []string // write-only
	AllowPaths []string // read+write

	// ProtectedPaths are denied even under broader grants (expanded;
	// override_deny holes already punched). Not existence-filtered:
	// a missing ~/.ssh today may exist tomorrow.
	ProtectedPaths []string

	// NameDenyRoots are granted folders with too many entries for the
	// launch-time scan (maxDenyScanEntries). Instead of listing their
	// secret files, the macOS profile hides every file or folder below
	// them whose name matches one of NameDenyGlobs (see sbplNameDenies).
	// Always empty on Linux, where such a folder still refuses launch.
	NameDenyRoots []string
	NameDenyGlobs []string

	// Network.
	NetworkMode     string // filtered|blocked|open
	ProxyPort       int    // 0 when no proxy is running
	ListenPorts     []int
	AllowTCPConnect []int
	OpenPorts       []int

	// UnixSockets lists socket files granted via --allow-file that
	// need explicit AF_UNIX connect allowance on macOS.
	UnixSockets []string

	// UnixSocketDirs (from --allow-unix-dir) allow AF_UNIX connect to any
	// socket under each dir (subpath rule), for dynamic socket names.
	UnixSocketDirs []string

	Enforcement string // kernel|env-only

	// DenialText holds the configurable marker-file content written over
	// protected files (Linux bwrap fast path). When empty, bwrap falls
	// back to /dev/null (opaque ENOENT/EACCES, the historical behavior).
	// Held as plain prose: prepareMarkers runs it through
	// sandboxdeny.Inert before it becomes a bind source, so a masked
	// shell config cannot execute it.
	DenialText string

	// DenialDirName is the filename of the notice placed inside a masked
	// protected directory (from the profile's denial.marker_dir_name).
	// Empty falls back to markerDirFileName.
	DenialDirName string

	// markerFile and markerDir are the bind sources used to mask
	// protected files and directories with an explanatory denial marker.
	// They are populated by prepareMarkers and consumed by BuildBwrapArgv;
	// empty means "fall back to /dev/null + tmpfs".
	markerFile string
	markerDir  string
}

// markerDirFileName is the single file placed inside a masked protected
// directory; the agent reads it to learn the directory is intentionally
// restricted.
const markerDirFileName = ".omac-denied"

// prepareMarkers creates the bind sources that mask protected paths with
// an explanatory denial marker: a read-only file bound over protected
// files, and a directory holding a single .omac-denied file bound over
// protected directories. Both carry DenialText.
//
// The text is written in its inert (comment-prefixed) form: the baseline
// protected set includes shell configs, which exist to be executed, so a
// marker of plain prose bound over ~/.profile is sourced by every login
// shell inside the sandbox (#213). Neutralizing here — at the boundary
// where bytes enter the sandbox — covers profile-supplied
// denial.marker_file text too, so no profile can inject commands into
// the process omac is confining.
//
// It sets g.markerFile and g.markerDir and returns a cleanup that removes
// the backing temp dir. The caller MUST defer cleanup until after the
// sandbox process has exited: bwrap reads bind sources at launch, not at
// argv-construction time, so deleting them earlier would leave dangling
// --ro-bind sources and abort the launch.
//
// A no-op cleanup is returned when there is no denial text or nothing to
// protect; BuildBwrapArgv then falls back to /dev/null + tmpfs.
func (g *Grants) prepareMarkers() (func(), error) {
	noop := func() {}
	if strings.TrimSpace(g.DenialText) == "" || len(g.ProtectedPaths) == 0 {
		return noop, nil
	}
	dir, err := os.MkdirTemp("", "omac-markers-*")
	if err != nil {
		return noop, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	text := []byte(sandboxdeny.Inert(g.DenialText))
	markerFile := filepath.Join(dir, "file")
	if err := os.WriteFile(markerFile, text, 0o444); err != nil {
		cleanup()
		return noop, err
	}
	markerDir := filepath.Join(dir, "dir")
	if err := os.Mkdir(markerDir, 0o755); err != nil {
		cleanup()
		return noop, err
	}
	dirFileName := g.DenialDirName
	if dirFileName == "" {
		dirFileName = markerDirFileName
	}
	markerDirFile := filepath.Join(markerDir, dirFileName)
	if !strings.HasPrefix(filepath.Clean(markerDirFile), markerDir+string(filepath.Separator)) {
		cleanup()
		return noop, fmt.Errorf("grants: denial dir name %q escapes marker directory", dirFileName)
	}
	if err := os.WriteFile(markerDirFile, text, 0o444); err != nil {
		cleanup()
		return noop, err
	}
	g.markerFile = markerFile
	g.markerDir = markerDir
	return cleanup, nil
}

// ResolveGrants merges the profile, the platform baseline, and the
// workdir access level into a Grants value. notices receives skip
// notices for nonexistent paths.
func ResolveGrants(p *sandboxprofile.Profile, workdir string, notices io.Writer) (*Grants, error) {
	base := sandboxprofile.PlatformBaseline()

	read, err := sandboxprofile.ExpandExisting(append(append([]string{}, base.Read...), p.Filesystem.Read...), notices)
	if err != nil {
		return nil, err
	}
	write, err := sandboxprofile.ExpandExisting(append(append([]string{}, base.Write...), p.Filesystem.Write...), notices)
	if err != nil {
		return nil, err
	}
	allow, err := sandboxprofile.ExpandExisting(p.Filesystem.Allow, notices)
	if err != nil {
		return nil, err
	}

	// Expand but don't existence-filter: the daemon may create the dir
	// after launch. Also appended to allow so the socket files can be opened.
	var unixDirs []string
	for _, raw := range p.Filesystem.AllowUnixDir {
		abs, expErr := sandboxprofile.ExpandPath(raw)
		if expErr != nil {
			if errors.Is(expErr, sandboxprofile.ErrEmptyExpansion) {
				continue
			}
			return nil, fmt.Errorf("allow_unix_dir %q: %w", raw, expErr)
		}
		unixDirs = append(unixDirs, abs)
	}
	allow = append(allow, unixDirs...)

	denyScan, err := denyScanRoots(p, workdir)
	if err != nil {
		return nil, err
	}

	// Workdir grant.
	switch p.Workdir.Access {
	case sandboxprofile.AccessRead:
		read = append(read, workdir)
	case sandboxprofile.AccessWrite:
		write = append(write, workdir)
	case sandboxprofile.AccessReadWrite:
		allow = append(allow, workdir)
	}

	// A linked worktree's .git file points at an admin dir under a shared
	// common dir OUTSIDE the workdir — grant those subdirs so git add/commit
	// work. No-op for plain clones/submodules (no commondir).
	var worktreeDeny []string
	if p.Workdir.Access != "" && p.Workdir.Access != sandboxprofile.AccessNone {
		if wr, wa, wd, ok := gitWorktreeGrants(workdir, p.Workdir.Access); ok {
			// Existence-filter silently: a missing packed-refs is normal here.
			wr, err = sandboxprofile.ExpandExisting(wr, nil)
			if err != nil {
				return nil, err
			}
			wa, err = sandboxprofile.ExpandExisting(wa, nil)
			if err != nil {
				return nil, err
			}
			read = append(read, wr...)
			allow = append(allow, wa...)
			worktreeDeny = wd
		}
	}

	protected := sandboxprofile.EffectiveProtectedPaths(base, p.Filesystem.OverrideDeny)
	protected = append(protected, worktreeDeny...)

	// User deny entries and baseline workdir-protected basenames share
	// a single walk over the granted (non-baseline) scan roots. Path-form
	// deny entries expand to explicit paths; basename globs and baseline
	// basenames are matched together in one pass.
	denyResolved, byName, err := resolveDenyPaths(
		p.Filesystem.Deny, base.WorkdirProtected, p.Filesystem.OverrideDeny,
		dedupe(denyScan), protected, notices, nil, denyScanConcurrency,
	)
	if err != nil {
		return nil, err
	}
	protected = append(protected, denyResolved...)
	if notices != nil && len(byName.roots) > 0 {
		fmt.Fprintf(notices, "omac sandbox: notice: these folders have more than %d entries, so secret files (.env, ...) in them are hidden by name instead of being listed: %s\n",
			denyScanLimit.Load(), strings.Join(byName.roots, ", "))
	}

	g := &Grants{
		Workdir:         workdir,
		ReadPaths:       dedupe(read),
		WritePaths:      dedupe(write),
		AllowPaths:      dedupe(allow),
		ProtectedPaths:  dedupe(protected),
		NameDenyRoots:   byName.roots,
		NameDenyGlobs:   byName.globs,
		NetworkMode:     p.Network.EffectiveMode(),
		ListenPorts:     dedupeInts(p.Network.ListenPort),
		AllowTCPConnect: dedupeInts(p.Network.AllowTCPConnect),
		OpenPorts:       dedupeInts(p.Network.OpenPort),
		UnixSocketDirs:  dedupe(unixDirs),
		Enforcement:     p.Network.EffectiveEnforcement(),
	}

	// Unix sockets: any allow-granted path that is a socket file gets
	// the macOS AF_UNIX carve-out.
	for _, path := range g.AllowPaths {
		if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSocket != 0 {
			g.UnixSockets = append(g.UnixSockets, path)
		}
	}
	return g, nil
}

// gitWorktreeGrants returns the extra grants a linked git worktree needs.
// ok=false unless workdir is a linked worktree (.git is a file pointing at an
// admin dir under a shared common dir).
//
// objects/refs/logs and the per-worktree admin dir are granted at the workdir's
// access level; config/info/packed-refs/hooks stay read-only — readable (so git
// reads config and RUNS host commit hooks) but never writable, blocking the
// #30 persistence vector (planting a hook or mutating core.hooksPath).
//
// The common-dir ROOT is never granted: git can't create <common>/packed-refs.lock,
// so a non-fatal EPERM prints on ref updates (loose refs still write, commit
// succeeds). Granting the root writable would let a lock-rename defeat the
// read-only config guard.
func gitWorktreeGrants(workdir, access string) (readAdds, allowAdds, denyAdds []string, ok bool) {
	common, admin, ok := resolveWorktreeCommonDir(workdir)
	if !ok {
		return nil, nil, nil, false
	}
	// SECURITY: grant paths derive from in-workdir files (.git, <admin>/commondir)
	// a prior sandboxed session could have tampered with, and backends
	// symlink-canonicalize every grant into a kernel rule. Admit only entries
	// physically inside the common dir so a planted symlink (e.g. `objects` ->
	// ~/.ssh) cannot widen a grant to an out-of-tree path.
	root, err := filepath.EvalSymlinks(common)
	if err != nil {
		return nil, nil, nil, false
	}
	contained := func(paths ...string) []string {
		out := make([]string, 0, len(paths))
		for _, p := range paths {
			if pathWithinRoot(p, root) {
				out = append(out, p)
			}
		}
		return out
	}
	writeSubdirs := contained(
		filepath.Join(common, "objects"),
		filepath.Join(common, "refs"),
		filepath.Join(common, "logs"),
		admin, // per-worktree index, HEAD, ORIG_HEAD, COMMIT_EDITMSG, logs
	)
	readOnly := contained(
		filepath.Join(common, "config"),
		filepath.Join(common, "info"),
		filepath.Join(common, "packed-refs"),
		// hooks must stay in contained(): a read grant on a planted `hooks ->
		// secret` symlink would otherwise widen to the target.
		filepath.Join(common, "hooks"),
	)

	switch access {
	case sandboxprofile.AccessReadWrite, sandboxprofile.AccessWrite:
		// Write subdirs get read+write even for AccessWrite: git must read
		// the objects/refs it commits.
		return readOnly, writeSubdirs, denyAdds, true
	case sandboxprofile.AccessRead:
		// Read-only workdir: git status/log/diff still read the common dir.
		return append(writeSubdirs, readOnly...), nil, denyAdds, true
	default:
		return nil, nil, nil, false
	}
}

// resolveWorktreeCommonDir inspects workdir/.git. For a linked worktree it is
// a "gitdir: <admin>" file; <admin>/commondir points at the shared common dir.
// Returns (common, admin, true) only for a linked worktree — plain clones and
// non-repo workdirs yield ok=false.
func resolveWorktreeCommonDir(workdir string) (common, admin string, ok bool) {
	dotgit := filepath.Join(workdir, ".git")
	fi, err := os.Lstat(dotgit)
	if err != nil || fi.IsDir() {
		return "", "", false
	}
	admin, err = readGitdirPointer(dotgit)
	if err != nil {
		return "", "", false
	}
	// Back-pointer check: <admin>/gitdir must point back at this workdir's
	// .git file. Without this, any workdir that names another worktree's
	// admin dir in its .git file gets that repo's grants with no proof of
	// ownership.
	if !worktreeBackPointerValid(admin, dotgit) {
		return "", "", false
	}
	data, err := os.ReadFile(filepath.Join(admin, "commondir"))
	if err != nil {
		return "", "", false
	}
	rel := strings.TrimSpace(string(data))
	if rel == "" {
		return "", "", false
	}
	common = rel
	if !filepath.IsAbs(common) {
		common = filepath.Join(admin, common)
	}
	common = filepath.Clean(common)
	// git invariant: admin dir is <common>/worktrees/<name>. Enforce it so a
	// crafted commondir can't point the grant root elsewhere.
	if filepath.Base(filepath.Dir(admin)) != "worktrees" || filepath.Dir(filepath.Dir(admin)) != common {
		return "", "", false
	}
	return common, admin, true
}

// worktreeBackPointerValid reads <admin>/gitdir and checks that it resolves
// to the same path as dotgit. Both sides are symlink-resolved so that
// git's canonical (EvalSymlinks) path matches dotgit's possibly-unresolved
// form (e.g. /var vs /private/var on macOS). Falls back to filepath.Clean
// when EvalSymlinks fails. Returns false when the file is absent or the
// paths don't match.
func worktreeBackPointerValid(admin, dotgit string) bool {
	data, err := os.ReadFile(filepath.Join(admin, "gitdir"))
	if err != nil {
		return false
	}
	ptr := strings.TrimSpace(string(data))
	if ptr == "" {
		return false
	}
	if !filepath.IsAbs(ptr) {
		ptr = filepath.Join(admin, ptr)
	}
	ptr = filepath.Clean(ptr)
	canonical := func(p string) string {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return r
		}
		return p
	}
	return canonical(ptr) == canonical(dotgit)
}

// pathWithinRoot reports whether path, symlinks resolved, is root or inside it.
// An unresolvable path is treated as not contained (grant dropped).
func pathWithinRoot(path, root string) bool {
	rp, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	return rp == root || strings.HasPrefix(rp, root+string(filepath.Separator))
}

// readGitdirPointer parses a linked worktree's .git file ("gitdir: <path>")
// and returns the absolute admin-dir path. A relative pointer is resolved
// against the .git file's directory (the workdir).
func readGitdirPointer(dotgit string) (string, error) {
	data, err := os.ReadFile(dotgit)
	if err != nil {
		return "", err
	}
	line := strings.TrimSpace(string(data))
	const prefix = "gitdir:"
	if !strings.HasPrefix(line, prefix) {
		return "", fmt.Errorf("%s: missing %q pointer", dotgit, prefix)
	}
	p := strings.TrimSpace(strings.TrimPrefix(line, prefix))
	if p == "" {
		return "", fmt.Errorf("%s: empty gitdir pointer", dotgit)
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(filepath.Dir(dotgit), p)
	}
	return filepath.Clean(p), nil
}

// maxDenyScanEntries bounds the basename-glob walk so a deny like
// ".env" over a huge granted tree cannot stall launch. Reaching the cap
// stops the walk for that root (already-found matches are still masked).
// 1,000,000 entries take about 0.2-0.5 s to walk (4-core Linux, warm
// cache).
const maxDenyScanEntries = 1_000_000

// denyScanLimit is the cap in effect; tests lower it (setDenyScanLimit).
// Atomic because the mid-session watch walks in its own goroutine.
var denyScanLimit atomic.Int64

func init() { denyScanLimit.Store(maxDenyScanEntries) }

// denyScanSkipDirNames are directory basenames the deny walk does not
// descend into. They are dependency, build and cache trees: large enough
// to dominate a scan, and not where users keep dotenv secrets. Matching
// on the basename (not a path) prunes at any depth. A skipped subtree
// means a matching file inside it is left unmasked, which keeps the
// workdir-and-grants scan bounded on toolchain and home roots.
//
// PONYTAIL: a huge tree whose name is not listed still fails closed at
// maxDenyScanEntries. Add the name here (or narrow the grant) rather
// than growing this into a path-aware configuration.
var denyScanSkipDirNames = map[string]bool{
	// dependency / VCS trees
	"node_modules": true,
	".git":         true,
	".hg":          true,
	".svn":         true,
	"vendor":       true,
	// language build caches
	".venv":       true,
	"venv":        true,
	"__pycache__": true,
	"target":      true,
	".gradle":     true,
	".m2":         true,
	".cargo":      true,
	".rustup":     true,
	".nvm":        true,
	".npm":        true,
	".yarn":       true,
	".pnpm-store": true,
	// generic build outputs and caches
	"dist":   true,
	"build":  true,
	"out":    true,
	".cache": true,
	// macOS user library: app support, caches, containers
	"Library": true,
}

// denyScanRoots returns the roots a basename-glob deny scans: the
// explicit (non-baseline) grants plus the workdir when granted.
// Baseline grants are excluded so a deny like ".env" never walks /usr
// or /lib.
func denyScanRoots(p *sandboxprofile.Profile, workdir string) ([]string, error) {
	roots, err := sandboxprofile.ExpandExisting(
		append(append(append([]string{}, p.Filesystem.Read...), p.Filesystem.Write...), p.Filesystem.Allow...),
		nil)
	if err != nil {
		return nil, err
	}
	if p.Workdir.Access != "" && p.Workdir.Access != sandboxprofile.AccessNone {
		roots = append(roots, workdir)
	}
	return dedupe(roots), nil
}

// resolveDenyPaths resolves user deny entries and baseline workdir-protected
// basenames in a single filesystem walk. User deny path-form entries expand
// to explicit protected paths; basename globs and baseline basenames are
// matched together. override_deny holes (basename or absolute path) are
// punched through baseline matches. protectedDirs are paths already denied
// by an ancestor rule: the walk prunes them (a match inside is masked by the
// ancestor anyway).
//
// A root the walk could not finish (it hit the entry cap) is returned in
// byName when the sandbox can hide files there by name (macOS, see
// denyByName): the profile then covers every match, listed or not.
// Otherwise an incomplete scan is an error so the caller fails closed
// instead of proceeding with a partial protected set.
//
// A non-nil cache carries state between passes (see denyScanCache): with
// one, out lists only the matches from directories actually re-read this
// pass — every match of an unchanged directory was reported by an earlier
// pass, which is what the mid-session watch's new-match detection needs.
// Launch passes nil and gets the complete set, as before.
//
// concurrency bounds how many folders the walk reads in parallel; launch
// passes denyScanConcurrency, the watch's throttled full pass passes less.
func resolveDenyPaths(userDeny, baselineBasenames, overrideDeny, scanRoots, protectedDirs []string, notices io.Writer, cache *denyScanCache, concurrency int) (out []string, byName nameDeny, err error) {
	if len(userDeny) == 0 && len(baselineBasenames) == 0 {
		return nil, nameDeny{}, nil
	}

	explicit := pathFormDenies(userDeny, notices)
	var globs []string
	for _, d := range userDeny {
		if sandboxprofile.IsBasenameGlob(d) {
			globs = append(globs, d)
		}
	}

	// Filter baseline basenames through overrides before walking.
	overrides := sandboxprofile.BuildOverrideLookup(overrideDeny)
	for _, b := range baselineBasenames {
		if !overrides[b] {
			globs = append(globs, b)
		}
	}

	out = explicit
	if len(globs) > 0 {
		matches, incomplete := walkGlobMatches(scanRoots, globs, protectedDirs, cache, concurrency)
		for _, root := range incomplete {
			if ok, reason := canDenyByName(root, globs, overrideDeny); !ok {
				return nil, nameDeny{}, fmt.Errorf("sandbox grants: filesystem.deny scan of %s hit the %d-entry limit; refusing to launch with an incomplete protected set%s", root, denyScanLimit.Load(), reason)
			}
		}
		if len(incomplete) > 0 {
			byName = nameDeny{roots: incomplete, globs: globs}
		}
		// Drop baseline matches covered by an absolute-path override.
		for _, m := range matches {
			if !overrides[m] {
				out = append(out, m)
			}
		}
	}
	return out, byName, nil
}

// nameDeny hides, below each of roots, every file or folder whose name
// matches one of globs, without listing them first.
type nameDeny struct {
	roots, globs []string
}

// denyByName reports whether the sandbox can hide files by name below a
// folder: Seatbelt on macOS matches paths with regular expressions. The
// Linux backends can only hide paths that exist at launch, so there a
// folder too large to scan still refuses launch. Tests on Linux switch it
// on to cover the shared code.
var denyByName atomic.Bool

func init() { denyByName.Store(runtime.GOOS == "darwin") }

// canDenyByName reports whether root can be covered by a name rule; if
// not, reason (possibly empty) is appended to the refusal. An absolute
// override_deny path inside root would be hidden by the rule although
// the user lifted it, and a path or pattern the rule cannot spell is not
// covered, so those setups keep refusing launch, as before name rules.
func canDenyByName(root string, globs, overrideDeny []string) (ok bool, reason string) {
	if !denyByName.Load() {
		return false, ""
	}
	for _, o := range overrideDeny {
		exp, err := sandboxprofile.ExpandPath(o)
		if err != nil || !filepath.IsAbs(exp) || !insideFolder(exp, root) {
			continue
		}
		for _, g := range globs {
			if ok, _ := filepath.Match(g, filepath.Base(exp)); ok {
				return false, fmt.Sprintf(" (override_deny %q is inside it)", o)
			}
		}
	}
	for _, fp := range pathForms(root) {
		if _, err := sbplNameRegex(fp, globs); err != nil {
			return false, fmt.Sprintf(" (%v)", err)
		}
	}
	return true, ""
}

// insideFolder reports whether p is below root, comparing the written
// and symlink-resolved spellings of both.
func insideFolder(p, root string) bool {
	for _, pf := range pathForms(p) {
		for _, rf := range pathForms(root) {
			if strings.HasPrefix(pf, strings.TrimSuffix(rf, string(filepath.Separator))+string(filepath.Separator)) {
				return true
			}
		}
	}
	return false
}

// pathFormDenies expands the path-form (non basename-glob) entries of a
// filesystem.deny list to absolute paths. Glob entries are skipped —
// they need a tree walk (walkGlobMatches). Shared by ResolveGrants (the
// child's real enforcement) and NewProtectedPathSet (the facade-side
// coarse checker) so the two agree on what counts as an explicitly
// denied path.
func pathFormDenies(deny []string, notices io.Writer) []string {
	var out []string
	for _, d := range deny {
		if sandboxprofile.IsBasenameGlob(d) {
			continue
		}
		exp, err := sandboxprofile.ExpandPath(d)
		if err != nil {
			if notices != nil {
				fmt.Fprintf(notices, "omac sandbox: notice: skipping filesystem.deny %q (%v)\n", d, err)
			}
			continue
		}
		out = append(out, exp)
	}
	return out
}

// denyScanConcurrency bounds how many folders walkGlobMatches reads in
// parallel, across all roots. The walks are I/O-bound (readdir/lstat), so
// overlapping them across the many granted roots — every project folder under
// --for-opencode-desktop, plus large cache/toolchain dirs like
// ~/Library/Caches, ~/go and ~/.cargo — cuts launch time substantially
// without changing the result set. Clamped to a sensible range.
var denyScanConcurrency = clampInt(runtime.NumCPU(), 4, 16)

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// walkGlobMatches walks each root and returns every file/dir whose base
// name matches one of the globs. Each root walk is bounded by
// maxDenyScanEntries and never descends into a matched directory
// (masking the dir is enough). Roots and the folders inside them are read
// concurrently (bounded by the concurrency argument); the result set is
// order-independent — callers dedupe+sort — so concurrency doesn't
// affect the resolved grants.
//
// A non-nil cache makes the walk incremental (see denyScanCache): only
// directories whose fingerprint changed since the previous pass are
// read, and the returned matches are those found in re-read directories
// plus — without a cache — every match, as before.
//
// If a root hits the entry cap its walk is incomplete: files the
// configuration promises are blocked may never be visited and would be
// left unmasked. Such roots are returned in incomplete (their matches up
// to the cap are still in matches), and resolveDenyPaths decides between
// refusing launch and hiding files there by name.
//
// protectedDirs are already-denied paths the walk prunes without
// descending (any match inside is masked by the ancestor rule), and
// denyScanSkipDirNames prunes large dependency/cache trees.
func walkGlobMatches(roots, globs, protectedDirs []string, cache *denyScanCache, concurrency int) (matches, incomplete []string) {
	if len(globs) == 0 || len(roots) == 0 {
		return nil, nil
	}
	protected := map[string]bool{}
	for _, p := range protectedDirs {
		for _, fp := range pathForms(p) {
			protected[fp] = true
		}
	}
	type rootResult struct {
		matches []string
		hitCap  bool
	}
	results := make([]rootResult, len(roots))
	readDirs := make(chan struct{}, clampInt(concurrency, 1, denyScanConcurrency))
	var wg sync.WaitGroup
	for i, root := range roots {
		i, root := i, root
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i].matches, results[i].hitCap = walkOneDenyRoot(root, globs, protected, readDirs, cache, concurrency)
		}()
	}
	wg.Wait()

	// Merge deterministically in root order; dedupe paths matched under
	// overlapping roots.
	seen := map[string]bool{}
	for i, root := range roots {
		if results[i].hitCap {
			incomplete = append(incomplete, root)
		}
		for _, m := range results[i].matches {
			if !seen[m] {
				seen[m] = true
				matches = append(matches, m)
			}
		}
	}
	return matches, incomplete
}

// walkOneDenyRoot walks a single root and returns the paths whose base
// name matches a glob, plus whether the entry cap was reached. Its
// workers read folders concurrently, each read holding a slot in readDirs
// (shared across roots); with a cold cache this is 2-5x faster.
// protected holds already-denied paths to prune (layer 1); a directory
// whose basename is in denyScanSkipDirNames is pruned too (layer 2), but
// only when it is not itself a glob match — a matched dir is masked.
//
// A non-nil cache makes the walk incremental: what is skipped, what a
// pass returns and how the cap is charged is the denyScanCache contract.
//
// When root is itself a symlink, the kernel backends resolve it and mount
// the real tree, but filepath.WalkDir lstat's the root and so would not
// descend a symlink root. We resolve the root with EvalSymlinks first
// and walk the real path; on resolution failure (broken link, permission)
// we fall back to walking the granted spelling, matching the pre-fix
// behavior for an unresolvable root so the change is strictly an
// improvement. Each match is re-grafted onto the granted spelling of the
// root (filepath.Join of the suffix under the resolved root), because
// that is the spelling every consumer uses: bwrap's coveredByAny and the
// marker bind destination, Seatbelt subpath rules (which canonicalize via
// pathForms) and, most importantly, the facade-side ProtectedPathSet —
// the mid-session watch stores these strings and IsProtected matches
// lexically against the alias spelling the agent actually queries. On
// macOS a plain /var-rooted grant would otherwise emit /private/var
// spellings the consumers compare against /var. Interior symlinks are
// still not followed: a folder listing reports a link as a link, and any
// real-tree location the agent can reach belongs to some granted root
// whose own scan emits its spelling.
func walkOneDenyRoot(root string, globs []string, protected map[string]bool, readDirs chan struct{}, cache *denyScanCache, concurrency int) (matches []string, hitCap bool) {
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		resolved = root
	}
	rootSep := resolved + string(filepath.Separator)
	limit := denyScanLimit.Load()

	var rc *denyScanRootState
	if cache != nil {
		rc = cache.state(root)
	}

	// read lists one folder; full reports that the cap was reached.
	var count atomic.Int64
	count.Add(1) // the root itself, as filepath.WalkDir counted it
	read := func(dir string) (subdirs, found []string, full bool) {
		readDirs <- struct{}{}
		defer func() { <-readDirs }()

		// Incremental fast path: a directory whose fingerprint is
		// unchanged since the last pass cannot have gained, lost or
		// renamed entries, so its cached subdirectory list and the
		// absence of new matches stay valid. One lstat replaces
		// open+readdir per unchanged directory, which is what keeps
		// the periodic re-walk affordable on granted cache trees.
		var fp dirFingerprint
		var isDir, haveFP bool
		if rc != nil {
			rc.lstats.Add(1)
			info, lerr := os.Lstat(dir)
			if lerr == nil {
				fp, isDir, haveFP = fingerprintOf(info), info.IsDir(), true
				if e, ok := rc.unchanged(dir, fp, isDir); ok {
					rc.skips.Add(1)
					if !e.isDir {
						return nil, nil, false
					}
					if count.Add(e.count) > limit {
						return nil, nil, true
					}
					return e.subdirs, nil, false
				}
			} else {
				// Gone or unreadable since it was discovered: skip, and
				// drop the cached subtree so a later pass re-reads it.
				rc.forget(dir)
			}
		}

		if rc != nil {
			rc.readdirs.Add(1)
		}
		entries, err := readDir(dir) // unreadable folder: skip, don't abort the walk
		for _, d := range entries {
			if count.Add(1) > limit {
				return subdirs, found, true
			}
			path := filepath.Join(dir, d.Name())
			if d.IsDir() && protected[path] {
				continue
			}
			matched := false
			for _, g := range globs {
				if ok, _ := filepath.Match(g, d.Name()); ok {
					matched = true
					break
				}
			}
			if matched {
				// A matched folder is masked whole, not descended.
				found = append(found, filepath.Join(root, strings.TrimPrefix(path, rootSep)))
				continue
			}
			if d.IsDir() && !denyScanSkipDirNames[d.Name()] {
				subdirs = append(subdirs, path)
			}
		}
		// A failed read of a directory is never recorded: caching the
		// empty state under the current fingerprint would hide the whole
		// subtree from later passes, while the stale entry's old
		// fingerprint no longer matches and the next pass re-reads — like
		// the uncached walk. A granted file root's failed read is the
		// expected ENOTDIR and records the empty state that retires it.
		if rc != nil && haveFP {
			switch {
			case isDir && err == nil:
				rc.record(dir, fp, true, int64(len(entries)), subdirs)
			case !isDir && dir == resolved && errors.Is(err, syscall.ENOTDIR):
				rc.record(dir, fp, false, 0, nil)
			}
		}
		return subdirs, found, false
	}

	// A fixed set of workers takes folders from a shared stack.
	var (
		mu    sync.Mutex
		cond  = sync.NewCond(&mu)
		stack = []string{resolved}
		busy  int
		wg    sync.WaitGroup
	)
	for range clampInt(concurrency, 1, denyScanConcurrency) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mu.Lock()
			defer mu.Unlock()
			for {
				for len(stack) == 0 && busy > 0 && !hitCap {
					cond.Wait()
				}
				if len(stack) == 0 || hitCap {
					cond.Broadcast()
					return
				}
				dir := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				busy++
				mu.Unlock()
				subdirs, found, full := read(dir)
				mu.Lock()
				busy--
				stack = append(stack, subdirs...)
				matches = append(matches, found...)
				hitCap = hitCap || full
				cond.Broadcast()
			}
		}()
	}
	wg.Wait()
	return matches, hitCap
}

// readDirHook, when non-nil, replaces os.ReadDir in the deny-scan walk.
// Tests use it to inject transient read failures.
var readDirHook atomic.Pointer[func(string) ([]os.DirEntry, error)]

func readDir(dir string) ([]os.DirEntry, error) {
	if f := readDirHook.Load(); f != nil {
		return (*f)(dir)
	}
	return os.ReadDir(dir)
}

func dedupe(in []string) []string {
	seen := make(map[string]bool, len(in))
	var out []string
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func dedupeInts(in []int) []int {
	seen := make(map[int]bool, len(in))
	var out []int
	for _, n := range in {
		if seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	sort.Ints(out)
	return out
}

// withUnrestrictedFilesystem returns a copy of the grants with the
// filesystem opened up (learn mode): the root directory becomes a
// read+write grant and the protected-path denials are dropped.
// Network and env restrictions are untouched.
func (g *Grants) withUnrestrictedFilesystem() *Grants {
	out := *g
	out.AllowPaths = append(append([]string{}, g.AllowPaths...), "/")
	out.ProtectedPaths = nil
	out.NameDenyRoots, out.NameDenyGlobs = nil, nil
	return &out
}

// Validate sanity-checks the grant set before launch.
func (g *Grants) Validate() error {
	if g.Workdir == "" {
		return fmt.Errorf("sandbox grants: empty workdir")
	}
	switch g.NetworkMode {
	case sandboxprofile.ModeFiltered, sandboxprofile.ModeBlocked, sandboxprofile.ModeOpen:
	default:
		return fmt.Errorf("sandbox grants: invalid network mode %q", g.NetworkMode)
	}
	return nil
}
