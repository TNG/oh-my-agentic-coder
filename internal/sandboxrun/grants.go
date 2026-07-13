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
	"sort"
	"strings"

	"github.com/tngtech/oh-my-agentic-coder/internal/sandboxprofile"
)

// Grants is the fully resolved, expanded, existence-filtered input to
// the platform backends.
type Grants struct {
	Workdir string // absolute; always the child's cwd

	// Path grants (expanded, absolute, existing).
	ReadPaths  []string // read-only
	WritePaths []string // write-only
	AllowPaths []string // read+write

	// ProtectedPaths are denied even under broader grants (expanded;
	// override_deny holes already punched). Not existence-filtered:
	// a missing ~/.ssh today may exist tomorrow.
	ProtectedPaths []string

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

	// Explicit (non-baseline) grants are the roots a basename-glob deny
	// scans. Computed before the baseline-merged read/write so a deny
	// like ".env" never triggers a walk of /usr or /lib. The workdir
	// grant is always an explicit root.
	denyScan, err := sandboxprofile.ExpandExisting(
		append(append(append([]string{}, p.Filesystem.Read...), p.Filesystem.Write...), p.Filesystem.Allow...),
		nil)
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
	if p.Workdir.Access != "" && p.Workdir.Access != sandboxprofile.AccessNone {
		denyScan = append(denyScan, workdir)
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
	protected = append(protected, resolveDenyPaths(
		p.Filesystem.Deny, base.WorkdirProtected, p.Filesystem.OverrideDeny,
		dedupe(denyScan), notices,
	)...)

	g := &Grants{
		Workdir:         workdir,
		ReadPaths:       dedupe(read),
		WritePaths:      dedupe(write),
		AllowPaths:      dedupe(allow),
		ProtectedPaths:  dedupe(protected),
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
const maxDenyScanEntries = 200000

// resolveDenyPaths resolves user deny entries and baseline workdir-protected
// basenames in a single filesystem walk. User deny path-form entries expand
// to explicit protected paths; basename globs and baseline basenames are
// matched together. override_deny holes (basename or absolute path) are
// punched through baseline matches.
func resolveDenyPaths(userDeny, baselineBasenames, overrideDeny, scanRoots []string, notices io.Writer) []string {
	if len(userDeny) == 0 && len(baselineBasenames) == 0 {
		return nil
	}

	var explicit []string
	var globs []string
	for _, d := range userDeny {
		if sandboxprofile.IsBasenameGlob(d) {
			globs = append(globs, d)
			continue
		}
		exp, err := sandboxprofile.ExpandPath(d)
		if err != nil {
			if notices != nil {
				fmt.Fprintf(notices, "omac sandbox: notice: skipping filesystem.deny %q (%v)\n", d, err)
			}
			continue
		}
		explicit = append(explicit, exp)
	}

	// Filter baseline basenames through overrides before walking.
	overrides := sandboxprofile.BuildOverrideLookup(overrideDeny)
	for _, b := range baselineBasenames {
		if !overrides[b] {
			globs = append(globs, b)
		}
	}

	out := explicit
	if len(globs) > 0 {
		matches := walkGlobMatches(scanRoots, globs, notices)
		// Drop baseline matches covered by an absolute-path override.
		for _, m := range matches {
			if !overrides[m] {
				out = append(out, m)
			}
		}
	}
	return out
}

// walkGlobMatches walks each root and returns every file/dir whose base
// name matches one of the globs. The walk is bounded by
// maxDenyScanEntries and never descends into matched directories
// (masking the dir is enough).
func walkGlobMatches(roots, globs []string, notices io.Writer) []string {
	seen := map[string]bool{}
	var out []string
	for _, root := range roots {
		count := 0
		stop := false
		_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil // unreadable entry: skip, don't abort the walk
			}
			count++
			if count > maxDenyScanEntries {
				if !stop && notices != nil {
					fmt.Fprintf(notices, "omac sandbox: notice: filesystem.deny scan of %s hit the %d-entry limit; some matches may be unmasked\n", root, maxDenyScanEntries)
				}
				stop = true
				return filepath.SkipAll
			}
			if path == root {
				return nil // never match the root grant itself
			}
			name := d.Name()
			for _, g := range globs {
				if ok, _ := filepath.Match(g, name); ok {
					if !seen[path] {
						seen[path] = true
						out = append(out, path)
					}
					if d.IsDir() {
						return filepath.SkipDir
					}
					return nil
				}
			}
			return nil
		})
	}
	return out
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
