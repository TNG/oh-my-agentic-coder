package sandboxrun

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

// BuildBwrapArgv constructs the bubblewrap invocation for the grant
// set. Pure argv construction — unit-testable on every platform.
//
// Layout (spec: sandbox-process-isolation "Linux enforcement via
// bubblewrap"):
//   - --ro-bind for read grants, --bind for allow/write grants
//   - system baseline as --ro-bind
//   - fresh --proc /proc and --dev /dev, --tmpfs /tmp unless granted
//   - --unshare-pid --unshare-ipc --unshare-uts (NOT --unshare-net)
//   - --die-with-parent --new-session
//   - protected paths inside granted trees masked with --tmpfs (dirs)
//     or --ro-bind /dev/null (files), honoring override_deny
//   - bind destinations that exist on the host as a symlink shadowed
//     by an earlier mount are rebound at their symlink-resolved path
//     (see resolveSymlinkMountDests)
//
// stage2Argv is the command bwrap execs; the caller passes the
// re-exec'd `omac sandbox stage2 ...` argv (which applies Landlock net
// rules and then execs the inner command).
func BuildBwrapArgv(g *Grants, stage2Argv []string) ([]string, error) {
	if len(stage2Argv) == 0 {
		return nil, fmt.Errorf("bwrap: empty stage2 argv")
	}
	argv := []string{
		"bwrap",
		"--die-with-parent",
		// --new-session creates a new terminal session (setsid), preventing
		// TIOCSTI keystroke injection from confined code into the host shell.
		// The child no longer receives kernel-delivered SIGWINCH on resize;
		// the supervisor relays it explicitly via kill(-pgid, SIGWINCH).
		"--new-session",
		"--unshare-pid",
		"--unshare-ipc",
		"--unshare-uts",
	}
	// Learn mode grants "/" read+write: bind the whole tree first (the
	// fresh /proc and /dev below still shadow the host's).
	rootGranted := false
	for _, p := range g.AllowPaths {
		if p == "/" {
			rootGranted = true
		}
	}
	if rootGranted {
		argv = append(argv, "--bind", "/", "/")
	}
	argv = append(argv,
		"--proc", "/proc",
		"--dev", "/dev",
	)

	// Deduplicate: allow (rw) wins over read (ro) for the same path.
	mounts := map[string]*mount{}
	add := func(p string, rw bool) {
		if p == "/proc" || p == "/dev" || p == "/" {
			return
		}
		if m, ok := mounts[p]; ok {
			m.rw = m.rw || rw
			return
		}
		mounts[p] = &mount{path: p, rw: rw}
	}
	for _, p := range g.ReadPaths {
		add(p, false)
	}
	for _, p := range g.WritePaths {
		add(p, true)
	}
	for _, p := range g.AllowPaths {
		add(p, true)
	}
	// Unix-socket dirs are the one grant ResolveGrants doesn't
	// existence-filter (the daemon may mint the dir after launch). bwrap
	// --bind aborts the launch on a missing source, so bind them with the
	// -try variant. AF_UNIX connect isn't mediated by Landlock, so once
	// the dir exists at launch the shared-inode bind is all Linux needs.
	for _, d := range g.UnixSocketDirs {
		if m, ok := mounts[d]; ok {
			m.try = true
		}
	}

	// Capture the pre-rewrite spellings: a symlinked mount path that
	// resolveSymlinkMountDests rewrites still covers everything under it
	// (the spelling resolves through the covering ancestor to the
	// rebound target), so both spellings count for reachability checks.
	orderedPre := sortedMounts(mounts)

	// Rebind shadowed symlink destinations at their resolved paths
	// before any argv is emitted (see resolveSymlinkMountDests).
	resolveSymlinkMountDests(mounts, rootGranted)

	// Sort parents before children so nested binds layer correctly.
	ordered := sortedMounts(mounts)
	// Reachability = covered by any bind in either spelling.
	coverage := append(append([]*mount{}, orderedPre...), ordered...)

	// tmpGranted is true only when bare /tmp itself is bound — a scoped
	// subpath like /tmp/omac-sandbox-tmp-x does not expose the whole shared
	// host temp dir, so it must not suppress the private --tmpfs /tmp.
	tmpGranted := rootGranted
	// Separate /tmp subpath mounts so we can emit them AFTER --tmpfs /tmp.
	var tmpSubpathMounts []*mount
	for _, m := range ordered {
		if m.path == "/tmp" && (!m.try || exists(m.path)) {
			tmpGranted = true
		}
		if rootGranted {
			continue // everything already bound rw via "/"
		}
		// Defer /tmp subpath binds: they must layer over --tmpfs /tmp.
		if strings.HasPrefix(m.path, "/tmp/") {
			tmpSubpathMounts = append(tmpSubpathMounts, m)
			continue
		}
		flag := "--ro-bind"
		if m.rw {
			flag = "--bind"
		}
		if m.try {
			flag += "-try"
		}
		argv = append(argv, flag, m.path, m.path)
	}
	if !tmpGranted {
		argv = append(argv, "--tmpfs", "/tmp")
	}
	// Emit /tmp subpath binds after the tmpfs so they shadow it correctly.
	for _, m := range tmpSubpathMounts {
		flag := "--ro-bind"
		if m.rw {
			flag = "--bind"
		}
		if m.try {
			flag += "-try"
		}
		argv = append(argv, flag, m.path, m.path)
	}

	// Protected-path masking: when rootGranted every protected path is
	// covered; otherwise check against the mount list (both spellings,
	// see `coverage` above). Masks must come after the binds they shadow.
	//
	// When denial markers are prepared (see Grants.prepareMarkers), a
	// protected file is masked with a read-only marker file whose contents
	// explain the denial, and a protected dir is masked with a read-only
	// marker dir holding a single .omac-denied file. When no markers are
	// prepared, the historical /dev/null + empty-tmpfs behavior applies.
	for _, prot := range g.ProtectedPaths {
		if !rootGranted && !coveredByAny(prot, coverage) {
			continue
		}
		target := prot
		fi, err := os.Lstat(prot)
		if err != nil {
			continue // doesn't exist; nothing to mask
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			// A protected path that is itself a symlink cannot be masked
			// at its own spelling: inside the covering bind the spelling
			// exists as that symlink, and bwrap refuses to mount on it
			// (same refusal resolveSymlinkMountDests avoids for grants).
			// Mask the resolved target instead. Reads through the spelling
			// land on the target, so the deny holds; masking the target is
			// strictly what the profile asked for, since it is the secret
			// the symlink spelling was denied over.
			resolved, rerr := filepath.EvalSymlinks(prot)
			if rerr != nil {
				continue // broken link: the spelling reads as ENOENT already
			}
			target = resolved
			if fi, err = os.Lstat(target); err != nil {
				continue
			}
		}
		if fi.IsDir() {
			if g.markerDir != "" {
				argv = append(argv, "--ro-bind", g.markerDir, target)
			} else {
				argv = append(argv, "--tmpfs", target)
			}
		} else {
			if g.markerFile != "" {
				argv = append(argv, "--ro-bind", g.markerFile, target)
			} else {
				argv = append(argv, "--ro-bind", "/dev/null", target)
			}
		}
	}

	// --chdir only works when the workdir is actually present in the
	// namespace (root grant or covered by a bind). With
	// workdir.access=none the directory is unbound; chdir would fail
	// the whole launch, so fall back to / (matching macOS, where the
	// child simply cannot read an ungranted cwd).
	chdir := g.Workdir
	if !rootGranted && !coveredByAny(g.Workdir, coverage) && g.Workdir != "/" {
		chdir = "/"
	}
	argv = append(argv, "--chdir", chdir, "--")
	argv = append(argv, stage2Argv...)
	return argv, nil
}

// mount is one bwrap bind entry.
type mount struct {
	path string
	rw   bool
	try  bool // use the -try variant: skip silently if the source is absent
}

// sortedMounts returns the map's mounts ordered parents before children,
// which lexicographic path order guarantees.
func sortedMounts(mounts map[string]*mount) []*mount {
	out := make([]*mount, 0, len(mounts))
	for _, m := range mounts {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out
}

// resolveSymlinkMountDests rewrites every mount whose destination exists on
// the host as a symlink that an earlier mount shadows, binding its
// symlink-resolved target instead.
//
// Why this is needed: bubblewrap creates a bind destination inside the
// sandbox when no earlier mount covers it, so a symlink destination whose
// ancestor is not yet mounted is harmless (it becomes a fresh directory
// holding the target's content). But once an ancestor is mounted, the
// destination exists in the sandbox as that symlink, and bwrap refuses to
// mount on it ("Can't mount on symlink destination") to keep sandboxed code
// from steering mount layout through symlinks. That is the default layout on
// merged-/usr distributions (issue #302: /usr/lib64 -> lib while the
// baseline binds /usr read-only), and it also crashes profile-granted paths
// the same way.
//
// Mounts whose destination is a symlink without a covering ancestor are left
// alone, deliberately: the fresh-directory behavior preserves the spelling
// itself, which binaries can depend on.
func resolveSymlinkMountDests(mounts map[string]*mount, rootGranted bool) {
	shadowed := func(p string) bool {
		if rootGranted {
			return true
		}
		for q := range mounts {
			if q != p && strings.HasPrefix(p, q+string(filepath.Separator)) {
				return true
			}
		}
		return false
	}
	for changed := true; changed; {
		changed = false
		for p, m := range mounts {
			fi, err := os.Lstat(p)
			if err != nil || fi.Mode()&os.ModeSymlink == 0 || !shadowed(p) {
				continue
			}
			// Unresolvable (broken) links stay as-is: their --bind would
			// fail on the missing source either way, which is the
			// documented abort-on-missing-source behavior.
			resolved, rerr := filepath.EvalSymlinks(p)
			if rerr != nil || resolved == p {
				continue
			}
			if existing, ok := mounts[resolved]; ok {
				existing.rw = existing.rw || m.rw
			} else {
				cp := *m
				cp.path = resolved
				mounts[resolved] = &cp
			}
			delete(mounts, p)
			changed = true
		}
	}
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// coveredByAny reports whether path lies under (or equals) any mount.
func coveredByAny(path string, mounts []*mount) bool {
	for _, m := range mounts {
		if path == m.path || strings.HasPrefix(path, m.path+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// Stage2Args serializes the network rules for the stage2 re-exec.
// Format: repeated --connect-tcp N / --bind-tcp N flags, then -- and
// the inner argv.
func Stage2Args(g *Grants) []string {
	var args []string
	if g.NetworkMode == sandboxprofile.ModeFiltered && g.Enforcement == sandboxprofile.EnforceKernel {
		connect := map[int]bool{}
		bind := map[int]bool{}
		if g.ProxyPort > 0 {
			connect[g.ProxyPort] = true
		}
		for _, p := range g.AllowTCPConnect {
			connect[p] = true
		}
		for _, p := range g.OpenPorts {
			connect[p] = true
			bind[p] = true
		}
		for _, p := range g.ListenPorts {
			bind[p] = true
		}
		for _, p := range sortedKeys(connect) {
			args = append(args, "--connect-tcp", strconv.Itoa(p))
		}
		for _, p := range sortedKeys(bind) {
			args = append(args, "--bind-tcp", strconv.Itoa(p))
		}
		args = append(args, "--enforce")
	} else if g.NetworkMode == sandboxprofile.ModeBlocked {
		args = append(args, "--enforce") // no ports at all = full TCP block
	}
	// open mode / env-only: no --enforce, stage2 just execs.
	return args
}

func sortedKeys(m map[int]bool) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}
