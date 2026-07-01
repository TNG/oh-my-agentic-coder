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

	// Linked-worktree grant. A linked worktree's .git is a *file* pointing
	// at an admin dir under the shared common dir, which sits OUTSIDE the
	// workdir — so git add/commit (which write the object store, refs,
	// reflogs and the per-worktree index/HEAD) get EPERM from the sandbox.
	// Restore plain-clone parity — where .git lives inside the granted
	// workdir — by granting the commit-relevant common-dir subdirs at the
	// workdir's access level, read-only on config/info, and a hard deny on
	// hooks/. No-op for a plain clone or a non-worktree workdir (including a
	// submodule, whose admin dir has no commondir).
	var worktreeDeny []string
	if p.Workdir.Access != "" && p.Workdir.Access != sandboxprofile.AccessNone {
		if wr, wa, wd, ok := gitWorktreeGrants(workdir, p.Workdir.Access); ok {
			// Existence-filter silently: these are omac-derived internal
			// paths (a missing packed-refs is normal), not user grants, so
			// a "skipping nonexistent path" notice would only confuse.
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

	// User deny entries carve holes out of the granted trees. Path-form
	// entries expand to an explicit path; basename globs (e.g. ".env")
	// are matched against the files inside the granted (non-baseline)
	// trees so the same deny covers the cwd and any explicit grant.
	protected = append(protected, resolveUserDeny(p.Filesystem.Deny, dedupe(denyScan), notices)...)

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

// gitWorktreeGrants returns the extra grants a linked git worktree needs
// so git operations work inside the sandbox. It returns ok=false unless
// workdir is a linked worktree (its .git is a file pointing at an admin
// dir under a shared common dir). The commit-write subdirs (objects,
// refs, logs, packed-refs, and this worktree's admin dir) are granted at
// the workdir's access level; config/info stay read-only (blocking
// core.hooksPath / credential.helper mutation); hooks/ is denied (a
// writable hooks dir would run un-sandboxed on the host's next commit).
func gitWorktreeGrants(workdir, access string) (readAdds, allowAdds, denyAdds []string, ok bool) {
	common, admin, ok := resolveWorktreeCommonDir(workdir)
	if !ok {
		return nil, nil, nil, false
	}
	// SECURITY: these grant paths are derived from in-workdir file content
	// (.git, <admin>/commondir) that a prior sandboxed session could have
	// tampered with, and both backends symlink-canonicalize every grant
	// into a kernel rule. Resolve the common dir and admit only entries
	// that physically live inside it, so a planted symlink (e.g. a crafted
	// `objects` -> ~/.ssh) cannot widen a grant to an out-of-tree path and
	// punch through the protected-path denials.
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
		// packed-refs holds only ref->SHA mappings (no hook/exec/credential
		// vector), and auto-gc / git pack-refs rewrite it, so it must be
		// writable — same trust level as refs/, which is already rw.
		filepath.Join(common, "packed-refs"),
		admin, // the per-worktree index, HEAD, ORIG_HEAD, COMMIT_EDITMSG, logs
	)
	readOnly := contained(
		filepath.Join(common, "config"),
		filepath.Join(common, "info"),
	)
	denyAdds = []string{filepath.Join(common, "hooks")}

	switch access {
	case sandboxprofile.AccessReadWrite, sandboxprofile.AccessWrite:
		// The write subdirs are granted read+write even when the workdir
		// itself is write-only (AccessWrite): git must read the objects and
		// refs it commits, so read-only here would break commit.
		return readOnly, writeSubdirs, denyAdds, true
	case sandboxprofile.AccessRead:
		// Read-only workdir: git status/log/diff still read the common
		// dir, but nothing under it may be written.
		return append(writeSubdirs, readOnly...), nil, denyAdds, true
	default:
		return nil, nil, nil, false
	}
}

// resolveWorktreeCommonDir inspects workdir/.git. For a linked worktree
// it is a regular file "gitdir: <admin>"; the admin dir's commondir file
// (a path relative to the admin dir, as git writes it) points at the
// shared common dir. Returns (common, admin, true) only for a linked
// worktree — a plain clone (.git is a directory) or a non-repo workdir
// yields ok=false.
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
	// git invariant: the admin dir is exactly <common>/worktrees/<name>.
	// Enforce it so a crafted commondir cannot point the grant root at an
	// unrelated location (e.g. an absolute commondir aimed elsewhere).
	if filepath.Base(filepath.Dir(admin)) != "worktrees" || filepath.Dir(filepath.Dir(admin)) != common {
		return "", "", false
	}
	return common, admin, true
}

// pathWithinRoot reports whether path, with all symlinks resolved, is root
// itself or lies inside it. A path that cannot be resolved (missing, or a
// dangling/looping symlink) is treated as NOT contained: the grant is
// dropped rather than risk canonicalizing to an out-of-tree location.
func pathWithinRoot(path, root string) bool {
	rp, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	return rp == root || strings.HasPrefix(rp, root+string(filepath.Separator))
}

// readGitdirPointer parses a linked worktree's .git file ("gitdir: <path>")
// and returns the absolute admin-dir path it points at. A relative pointer
// is resolved against the .git file's directory (the workdir).
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

// resolveUserDeny turns filesystem.deny entries into concrete protected
// paths. Path-form entries (with a separator, ~ or $VAR) expand to a
// single explicit path. Basename globs (e.g. ".env", "*.key") are
// matched against the files found by walking scanRoots — the explicit
// (non-baseline) granted trees plus the workdir — so one deny covers
// the cwd and every directory the user granted. Baseline system trees
// (e.g. /usr) are never scanned because they are not in scanRoots.
func resolveUserDeny(deny, scanRoots []string, notices io.Writer) []string {
	if len(deny) == 0 {
		return nil
	}
	var explicit []string
	var globs []string
	for _, d := range deny {
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

	out := explicit
	if len(globs) > 0 {
		out = append(out, walkGlobMatches(scanRoots, globs, notices)...)
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
