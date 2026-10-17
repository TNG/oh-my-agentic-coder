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
		// the supervisor keeps the terminal foreground and relays it to the
		// new session (sandbox.ExecDetachedSession).
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

	// Sort parents before children so nested binds layer correctly.
	ordered := make([]*mount, 0, len(mounts))
	for _, m := range mounts {
		ordered = append(ordered, m)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].path < ordered[j].path })

	// A granted symlink inside another grant already exists in the sandbox
	// as that symlink, and bwrap >= 0.12 refuses to mount onto it (#302:
	// /usr/lib64 -> lib under /usr). Drop the bind when it changes nothing.
	// Otherwise keep it and let bwrap refuse: binding the link's target
	// instead would let a link the agent planted in a writable tree choose
	// what gets mounted (e.g. workdir/lib -> /).
	kept := make([]*mount, 0, len(ordered))
	for _, m := range ordered {
		if !redundantLinkMount(m, ordered) {
			kept = append(kept, m)
		}
	}
	ordered = kept

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
	// covered; otherwise check against the ordered mount list.
	// Masks must come after the binds they shadow.
	//
	// When denial markers are prepared (see Grants.prepareMarkers), a
	// protected file is masked with a read-only marker file whose contents
	// explain the denial, and a protected dir is masked with a read-only
	// marker dir holding a single .omac-denied file. When no markers are
	// prepared, the historical /dev/null + empty-tmpfs behavior applies.
	for _, prot := range g.ProtectedPaths {
		if !rootGranted && !coveredByAny(prot, ordered) {
			continue
		}
		fi, err := os.Lstat(prot)
		if err != nil {
			continue // doesn't exist; nothing to mask
		}
		if fi.IsDir() {
			if g.markerDir != "" {
				argv = append(argv, "--ro-bind", g.markerDir, prot)
			} else {
				argv = append(argv, "--tmpfs", prot)
			}
		} else {
			if g.markerFile != "" {
				argv = append(argv, "--ro-bind", g.markerFile, prot)
			} else {
				argv = append(argv, "--ro-bind", "/dev/null", prot)
			}
		}
	}

	// --chdir only works when the workdir is actually present in the
	// namespace (root grant or covered by a bind). With
	// workdir.access=none the directory is unbound; chdir would fail
	// the whole launch, so fall back to / (matching macOS, where the
	// child simply cannot read an ungranted cwd).
	chdir := g.Workdir
	if !rootGranted && !coveredByAny(g.Workdir, ordered) && g.Workdir != "/" {
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

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// symlinkMountDests returns the destinations in a bwrap argv that are
// symlinks on the host inside an earlier grant bind. There the sandbox
// shows them as symlinks, and bwrap >= 0.12 refuses to mount onto them.
func symlinkMountDests(argv []string) []string {
	var bound []*mount
	var out []string
	for i := 0; i < len(argv) && argv[i] != "--"; i++ {
		var src, dest string
		switch argv[i] {
		case "--bind", "--ro-bind", "--bind-try", "--ro-bind-try":
			src, dest = argv[i+1], argv[i+2]
			i += 2
		case "--tmpfs":
			dest = argv[i+1]
			i++
		default:
			continue
		}
		if fi, err := os.Lstat(dest); err == nil && fi.Mode()&os.ModeSymlink != 0 &&
			(coveredByAny(dest, bound) || (len(bound) > 0 && bound[0].path == "/")) {
			out = append(out, dest)
		}
		if src == dest { // a grant; masks and tmpfs show no host content
			bound = append(bound, &mount{path: dest})
		}
	}
	return out
}

// redundantLinkMount reports whether m is a symlink inside another mount
// whose target the sandbox already shows with the same access as m.
func redundantLinkMount(m *mount, mounts []*mount) bool {
	if innermostMount(filepath.Dir(m.path), mounts) == nil {
		return false // not nested: bwrap creates a fresh dir and binds there
	}
	target := sandboxLinkTarget(m.path)
	if target == m.path || !sameRealPath(target, m.path) {
		return false
	}
	c := innermostMount(target, mounts)
	return c != nil && c.rw == m.rw
}

// sandboxLinkTarget follows the symlink at p, if any, keeping the spelling
// the sandbox sees. Mounts keep their host path there, so a relative target
// is joined to the link's own spelling rather than resolved to the host's
// real path: under a grant on the symlinked dir /alias -> /real,
// /alias/lib64 -> lib points at /alias/lib. The join is textual, so ".."
// can disagree with the kernel; callers confirm the result with
// sameRealPath. Returns p when it is not a symlink.
func sandboxLinkTarget(p string) string {
	for range maxSymlinkHops {
		target, err := os.Readlink(p)
		if err != nil {
			break
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(p), target)
		}
		p = filepath.Clean(target)
	}
	return p
}

// sameRealPath reports whether a and b resolve to the same host path.
func sameRealPath(a, b string) bool {
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && ra == rb
}

// innermostMount returns the deepest mount at or above path, or nil.
func innermostMount(path string, mounts []*mount) *mount {
	var best *mount
	for _, m := range mounts {
		if isUnder(path, m.path) && (best == nil || len(m.path) > len(best.path)) {
			best = m
		}
	}
	return best
}

// isUnder reports whether path equals root or lies below it.
func isUnder(path, root string) bool {
	return path == root || strings.HasPrefix(path, root+string(filepath.Separator))
}

// coveredByAny reports whether path lies under (or equals) any mount.
func coveredByAny(path string, mounts []*mount) bool {
	for _, m := range mounts {
		if isUnder(path, m.path) {
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
