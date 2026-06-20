package sandboxrun

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/tngtech/oh-my-agentic-coder/internal/sandboxprofile"
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
//   - --die-with-parent (NOT --new-session, see below)
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
		// Deliberately NOT --new-session. setsid(2) detaches the inner
		// process tree from the controlling terminal, which stops the
		// kernel from delivering SIGWINCH on window resize — so an inner
		// TUI (e.g. opencode) renders once and never reflows. --new-session
		// exists only to block TIOCSTI terminal-input injection; that
		// vector is gated off by the kernel on Linux >= 6.2 via the
		// dev.tty.legacy_tiocsti sysctl (default 0). We keep the real
		// terminal so resize works; filesystem/network/PID isolation is
		// unaffected (it comes from the binds, Landlock, and --unshare-*).
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

	// Sort parents before children so nested binds layer correctly.
	ordered := make([]*mount, 0, len(mounts))
	for _, m := range mounts {
		ordered = append(ordered, m)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].path < ordered[j].path })

	tmpGranted := rootGranted
	for _, m := range ordered {
		if m.path == "/tmp" || strings.HasPrefix(m.path, "/tmp/") {
			tmpGranted = true
		}
		if rootGranted {
			continue // everything is already bound rw via "/"
		}
		flag := "--ro-bind"
		if m.rw {
			flag = "--bind"
		}
		argv = append(argv, flag, m.path, m.path)
	}
	if !tmpGranted {
		argv = append(argv, "--tmpfs", "/tmp")
	}

	// Protected-path masking: only needed where a granted tree would
	// otherwise expose the protected path. Everything else is already
	// absent (unbound). Masks must come after the binds they shadow.
	for _, prot := range g.ProtectedPaths {
		if !coveredByAny(prot, ordered) {
			continue
		}
		fi, err := os.Lstat(prot)
		if err != nil {
			continue // doesn't exist; nothing to mask
		}
		if fi.IsDir() {
			argv = append(argv, "--tmpfs", prot)
		} else {
			argv = append(argv, "--ro-bind", "/dev/null", prot)
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
