//go:build linux

package sandboxrun

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/tngtech/oh-my-agentic-coder/internal/sandboxprofile"
)

// CheckPlatform verifies the Linux sandbox prerequisites: bwrap must
// be installed AND functional (unprivileged user namespaces can be
// disabled by distro policy, in containers, or via sysctl, in which
// case bwrap exists but cannot sandbox anything).
func CheckPlatform() error {
	if _, err := exec.LookPath("bwrap"); err != nil {
		return fmt.Errorf("bubblewrap (bwrap) not found on PATH — install it with your package manager (e.g. apt install bubblewrap / dnf install bubblewrap): %w", err)
	}
	smoke := exec.Command("bwrap", "--ro-bind", "/", "/", "true")
	out, err := smoke.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s", usernsDiagnosis(err, firstLine(out)))
	}
	return nil
}

// procUint reads a sysctl-style /proc file expected to hold a single
// integer and returns (value, true) on success. Missing files or
// unparseable contents yield (0, false) — the feature is simply absent.
func procUint(path string) (int, bool) {
	data, rerr := os.ReadFile(path)
	if rerr != nil {
		return 0, false
	}
	n, perr := strconv.Atoi(strings.TrimSpace(string(data)))
	if perr != nil {
		return 0, false
	}
	return n, true
}

// usernsDiagnosis turns a failed bwrap smoke test into an actionable
// message, reading the live /proc knobs to pick the right cause/fix.
func usernsDiagnosis(runErr error, firstOutLine string) string {
	apparmor, apparmorKnown := procUint("/proc/sys/kernel/apparmor_restrict_unprivileged_userns")
	clone, cloneKnown := procUint("/proc/sys/kernel/unprivileged_userns_clone")
	return formatUsernsDiagnosis(usernsState{
		runErr:        runErr,
		firstOutLine:  firstOutLine,
		apparmor:      apparmor,
		apparmorKnown: apparmorKnown,
		clone:         clone,
		cloneKnown:    cloneKnown,
	})
}

// usernsState carries the inputs to the diagnosis so the message
// formatting is pure and unit-testable without touching real /proc.
type usernsState struct {
	runErr        error
	firstOutLine  string
	apparmor      int  // value of apparmor_restrict_unprivileged_userns
	apparmorKnown bool // whether that proc file existed/parsed
	clone         int  // value of unprivileged_userns_clone
	cloneKnown    bool // whether that proc file existed/parsed
}

func formatUsernsDiagnosis(s usernsState) string {
	var b strings.Builder
	fmt.Fprintf(&b, "bwrap is installed but not functional (unprivileged user namespaces blocked?): %v", s.runErr)
	if s.firstOutLine != "" {
		fmt.Fprintf(&b, " — %s", s.firstOutLine)
	}

	switch {
	// Ubuntu 23.10+/24.04: AppArmor restricts unprivileged userns for
	// any unconfined binary (bwrap has no shipped profile) unless its
	// profile carries `userns create`. Enabled by default on 24.04.
	case s.apparmorKnown && s.apparmor == 1:
		b.WriteString(
			"\n\nCause: AppArmor is restricting unprivileged user namespaces " +
				"(kernel.apparmor_restrict_unprivileged_userns=1), the default on Ubuntu 24.04+.\n" +
				"bwrap has no AppArmor profile, so it is denied the user namespace it needs.\n" +
				"Fix A (preferred — keeps the protection for every other program): grant just bwrap\n" +
				"the permission. Create /etc/apparmor.d/bwrap with:\n" +
				"    abi <abi/4.0>,\n" +
				"    /usr/bin/bwrap flags=(unconfined) {\n" +
				"      userns,\n" +
				"    }\n" +
				"then load it:\n" +
				"    sudo apparmor_parser -r /etc/apparmor.d/bwrap\n" +
				"Fix B (system-wide, weaker — reverts to pre-23.10 behaviour):\n" +
				"    sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=0\n" +
				"  make it persist across reboots:\n" +
				"    echo 'kernel.apparmor_restrict_unprivileged_userns=0' | sudo tee /etc/sysctl.d/60-apparmor-namespace.conf")

	// Distro kernels with the all-or-nothing switch (Debian/older
	// Ubuntu/Arch): unprivileged userns disabled entirely.
	case s.cloneKnown && s.clone == 0:
		b.WriteString(
			"\n\nCause: unprivileged user namespaces are disabled system-wide " +
				"(kernel.unprivileged_userns_clone=0).\n" +
				"Fix: sudo sysctl -w kernel.unprivileged_userns_clone=1\n" +
				"  persist: echo 'kernel.unprivileged_userns_clone=1' | sudo tee /etc/sysctl.d/60-userns.conf")

	// No recognised knob: likely a container without userns, a hardening
	// sysctl, or seccomp policy. Keep the generic hint.
	default:
		b.WriteString(
			"\n\nThis usually means unprivileged user namespaces are unavailable here " +
				"(restricted by AppArmor/sysctl, or running in a container that disallows them).\n" +
				"On Ubuntu 24.04+ check: cat /proc/sys/kernel/apparmor_restrict_unprivileged_userns")
	}
	return b.String()
}

// kernelVersionString returns the running kernel version string from
// /proc/version (e.g. "6.1.0-28-amd64"). Returns "unknown" on failure.
func kernelVersionString() string {
	data, err := os.ReadFile("/proc/version")
	if err != nil {
		return "unknown"
	}
	// Format: "Linux version 6.19.11-200.fc43.x86_64 (builder@...) ..."
	fields := strings.Fields(string(data))
	if len(fields) >= 3 {
		return fields[2]
	}
	return strings.TrimSpace(string(data))
}

// resolveInnerBinaryDirs resolves the inner command's executable on the
// host PATH and returns the directories that must be granted for it to be
// reachable inside the namespace:
//
//   - the directory of the PATH entry itself, which is frequently a symlink
//     or shim (e.g. ~/.bun/bin/opencode, a mise/asdf shim). stage2 re-runs
//     LookPath inside the namespace, so this dir must be mounted and on PATH
//     or the lookup fails even when the real binary is present elsewhere.
//   - the directory of its symlink-resolved real file (e.g.
//     ~/.bun/install/.../opencode-ai/bin/opencode.exe), so the link target
//     and its sibling files (shared libs, node runtime) are reachable.
//
// Granting only the resolved dir (the historical behavior) left the shim on
// PATH unmounted, breaking version-manager installs like bun where the
// PATH entry and the real binary live in different trees. It runs on the
// supervisor (outside the namespace), so the lookup sees the user's real
// PATH — the same resolution `which opencode` performs. Directories already
// covered by the baseline (e.g. /usr/bin) are harmless: bwrap re-binding a
// path that a parent bind already covers is idempotent. (BuildBwrapArgv's
// dedupe only drops exact-duplicate path strings, not child-of-parent
// overlaps.) Returns nil when the command cannot be found or resolved.
func resolveInnerBinaryDirs(innerArgv []string) []string {
	if len(innerArgv) == 0 || innerArgv[0] == "" {
		return nil
	}
	// LookPath handles both bare PATH names and explicit (absolute or
	// relative) paths, returning an error when nothing resolves.
	resolved, err := exec.LookPath(innerArgv[0])
	if err != nil {
		return nil
	}
	if abs, aerr := filepath.Abs(resolved); aerr == nil {
		resolved = abs
	}
	dirs := []string{filepath.Dir(resolved)}
	if real, rerr := filepath.EvalSymlinks(resolved); rerr == nil {
		if d := filepath.Dir(real); d != dirs[0] {
			dirs = append(dirs, d)
		}
	}
	return dirs
}

func firstLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}

// BuildChildArgv wraps the inner command in bwrap + the stage2
// re-exec. self is the path to the running omac binary.
func BuildChildArgv(g *Grants, innerArgv []string) ([]string, error) {
	if err := CheckPlatform(); err != nil {
		return nil, err
	}
	// Both filtered (kernel enforcement) and blocked mode apply a
	// Landlock TCP ruleset in stage2; gate on ABI v4 up front so the
	// failure is a clear pre-launch error, not a stage2 crash.
	needsLandlock := (g.NetworkMode == sandboxprofile.ModeFiltered && g.Enforcement == sandboxprofile.EnforceKernel) ||
		g.NetworkMode == sandboxprofile.ModeBlocked
	if needsLandlock && !LandlockNetSupported() {
		return nil, fmt.Errorf(
			"kernel-enforced network filtering needs Landlock ABI >= 4 (Linux >= 6.7, e.g. Ubuntu 24.04 LTS, Fedora 40+);\n"+
				"this kernel has ABI %d (%s).\n"+
				"Fix A: upgrade to a kernel >= 6.7.\n"+
				"Fix B: set enforcement to env-only in ~/.config/omac/sandbox-profiles/default.json:\n"+
				"  {\"network\": {\"enforcement\": \"env-only\"}}\n"+
				"(env-only: filtering via the omac proxy, not the kernel — advisory only)",
			LandlockABI(), kernelVersionString())
	}
	self, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve omac executable: %w", err)
	}
	// Resolve symlinks so the bind target is the real file (a symlink
	// bind would dangle if its target dir is not in the namespace).
	if resolved, rerr := filepath.EvalSymlinks(self); rerr == nil {
		self = resolved
	}
	// The omac binary itself must exist inside the mount namespace for
	// bwrap to exec stage2. It commonly lives outside the granted
	// trees (~/go/bin, ~/.local/bin, /opt/omac, ...), so grant it
	// read-only explicitly. If a baseline grant already covers it, the
	// redundant bind is harmless (bwrap re-binding under a parent bind is
	// idempotent; BuildBwrapArgv's dedupe drops only exact-duplicate paths).
	gz := *g
	gz.ReadPaths = append(append([]string{}, g.ReadPaths...), self)

	// The inner harness binary (e.g. opencode / claude) must also be
	// reachable inside the namespace. Harnesses are frequently installed
	// outside the baseline trees — version managers like mise, asdf, nvm,
	// or volta put them under ~/.local/share/<mgr>/installs/..., and bun
	// installs a ~/.bun/bin shim pointing into ~/.bun/install/... — so a
	// plain ~/.local/bin grant is not enough. Resolve the binary on the
	// host PATH (the same lookup `which opencode` performs) and grant both
	// the PATH-entry (shim) directory and the symlink-resolved real
	// directory read-only, so the shim is found on PATH and its target plus
	// sibling files (shared libs, node runtime) are reachable too. Redundant
	// grants are harmless (bwrap re-binding under a parent bind is idempotent;
	// BuildBwrapArgv's dedupe drops only exact-duplicate paths, not overlaps).
	gz.ReadPaths = append(gz.ReadPaths, resolveInnerBinaryDirs(innerArgv)...)

	stage2 := []string{self, "sandbox", "stage2"}
	stage2 = append(stage2, Stage2Args(&gz)...)
	stage2 = append(stage2, "--")
	stage2 = append(stage2, innerArgv...)
	return BuildBwrapArgv(&gz, stage2)
}
