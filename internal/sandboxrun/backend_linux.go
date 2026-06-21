//go:build linux

package sandboxrun

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	if out, err := smoke.CombinedOutput(); err != nil {
		return fmt.Errorf("bwrap is installed but not functional (user namespaces disabled?): %v — %s", err, firstLine(out))
	}
	return nil
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

// resolveInnerBinaryDir resolves the inner command's executable on the
// host PATH and returns the directory holding its real (symlink-resolved)
// file, or "" when the command cannot be found or resolved. It runs on
// the supervisor (outside the namespace), so the lookup sees the user's
// real PATH — the same resolution `which opencode` performs. Directories
// already covered by the baseline (e.g. /usr/bin) are harmless: the
// dedupe in BuildBwrapArgv collapses the duplicate grant.
func resolveInnerBinaryDir(innerArgv []string) string {
	if len(innerArgv) == 0 || innerArgv[0] == "" {
		return ""
	}
	// LookPath handles both bare PATH names and explicit (absolute or
	// relative) paths, returning an error when nothing resolves.
	resolved, err := exec.LookPath(innerArgv[0])
	if err != nil {
		return ""
	}
	if abs, aerr := filepath.Abs(resolved); aerr == nil {
		resolved = abs
	}
	if real, rerr := filepath.EvalSymlinks(resolved); rerr == nil {
		resolved = real
	}
	return filepath.Dir(resolved)
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
	// read-only explicitly; the dedupe in BuildBwrapArgv collapses it
	// when an existing grant already covers it.
	gz := *g
	gz.ReadPaths = append(append([]string{}, g.ReadPaths...), self)

	// The inner harness binary (e.g. opencode / claude) must also be
	// reachable inside the namespace. Harnesses are frequently installed
	// outside the baseline trees — version managers like mise, asdf, nvm,
	// or volta put them under ~/.local/share/<mgr>/installs/... — so a
	// plain ~/.local/bin grant is not enough. Resolve the binary on the
	// host PATH (the same lookup `which opencode` performs), follow
	// symlinks to the real file, and grant its containing directory
	// read-only so sibling files (shared libs, node runtime, shims) are
	// reachable too. dedupe in BuildBwrapArgv collapses it when an
	// existing grant already covers it.
	if dir := resolveInnerBinaryDir(innerArgv); dir != "" {
		gz.ReadPaths = append(gz.ReadPaths, dir)
	}

	stage2 := []string{self, "sandbox", "stage2"}
	stage2 = append(stage2, Stage2Args(&gz)...)
	stage2 = append(stage2, "--")
	stage2 = append(stage2, innerArgv...)
	return BuildBwrapArgv(&gz, stage2)
}
