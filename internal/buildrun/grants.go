package buildrun

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/tngtech/oh-my-agentic-coder/internal/sandboxrun"
)

// BuildGrants is the executor grant set: sandboxrun.Grants plus the
// build-specific derived paths (GRADLE_USER_HOME leaf, private temp).
type BuildGrants struct {
	*sandboxrun.Grants
	gradleUserHome string
	tmpDir         string
}

// GradleUserHome is the OMAC cache leaf handed to the Gradle wrapper as
// GRADLE_USER_HOME.
func (b *BuildGrants) GradleUserHome() string { return b.gradleUserHome }

// TmpDir is the executor's private temporary directory (exported as TMPDIR).
func (b *BuildGrants) TmpDir() string { return b.tmpDir }

// gradleLeafName is the tool leaf below the resolved OMAC cache scope.
// The spec's Gradle State section fixes GRADLE_USER_HOME=$cache/gradle.
const gradleLeafName = "gradle"

// preLeafLocksDir holds omac's cross-run locks taken BEFORE the Gradle
// leaf itself is touched: Gradle wrapper downloads and (in later tickets)
// mediated-container staging must not race independent `omac build`
// invocations, but the locks belong to omac, not to Gradle, so they live
// beside the leaf under the cache scope rather than inside it.
const preLeafLocksDir = ".omac-pre-leaf-locks"

// envPassThrough is the fixed, harness-independent allowlist for the
// executor's environment. Nothing harness/host-specific may pass: no
// OMAC_* facade/sidecar vars, no cloud/SSH/git credentials, no HOME
// (which would expose host gradle.properties and init scripts under
// ~/.gradle). PATH, JAVA_HOME and locale vars are required so the wrapper
// can discover a JDK.
var envPassThrough = []string{
	"PATH",
	"JAVA_HOME",
	"ANDROID_HOME",
	"LANG",
	"LC_ALL",
	"LC_CTYPE",
	"TERM",
	"SHELL",
	// macOS users launchd-injected JDK dir helpers; harmless elsewhere.
	"__CF_USER_TEXT_ENCODING",
}

// GrantsFor derives the executor grant set for one build request:
//
//   - worktree (read+write)           — the canonical worktree root
//   - $cache/gradle (read+write)      — GRADLE_USER_HOME (the ONLY cache
//     path granted; the leaf is ensured on disk first since sandboxrun
//     existence-filters profile paths)
//   - $cache/gradle/.omac-pre-leaf-locks — omac-owned lock staging area
//   - private temp (read+write)       — per-run TMPDIR
//
// The cache SCOPE dir itself is deliberately NOT granted: sibling tool
// caches laid down by `omac start`/`serve` (go, npm, pip leaves) must
// stay unwritable by the build executor. The executor cannot create new
// leaves at the scope level — Gradle state lives inside its own leaf per
// GRADLE_USER_HOME.
//
// Network is fully blocked (kernel enforcement): direct external egress is
// denied by default and v0 mediates no proxy endpoints. Host home, host
// ~/.gradle (covered by the platform baseline's protected paths), SSH/AWS
// state and OMAC configuration receive no grants; a host secret fixture
// outside these paths stays unreadable under (deny default).
//
// cacheDir must already be the resolved OMAC cache scope dir (from
// internal/toolcache via the cli wiring); GrantsFor never invents paths.
func GrantsFor(worktree, cacheDir string) (*BuildGrants, error) {
	// The worktree must exist (Resolve already validated it; defensive).
	if _, err := os.Stat(worktree); err != nil {
		return nil, fmt.Errorf("worktree: %w", err)
	}
	if cacheDir == "" {
		return nil, fmt.Errorf("empty cache dir: GRADLE_USER_HOME must come from the resolved OMAC cache scope")
	}

	leaf := filepath.Join(cacheDir, gradleLeafName)
	if err := ensureDir(leaf, 0o700); err != nil {
		return nil, fmt.Errorf("prepare GRADLE_USER_HOME leaf: %w", err)
	}
	locksDir := filepath.Join(leaf, preLeafLocksDir)
	if err := ensureDir(locksDir, 0o700); err != nil {
		return nil, fmt.Errorf("prepare pre-leaf lock dir: %w", err)
	}
	// Private temp lives above /tmp (under the user temp ROOT, not in a
	// shared subdir), so the executor temp itself sits beside other
	// per-user temp entries rather than inside a world-visible one. Its
	// content stays confined: the dir is 0700, the kernel grant covers
	// this exact leaf, and it is removed on exit.
	tmpRoot := os.TempDir()
	tmpParent := filepath.Join(tmpRoot, "omac-build-tmp")
	if err := ensureDir(tmpParent, 0o700); err != nil {
		return nil, fmt.Errorf("private temp root: %w", err)
	}
	tmp, err := os.MkdirTemp(tmpParent, "exec-*")
	if err != nil {
		return nil, fmt.Errorf("private temp: %w", err)
	}
	// Seatbelt rules are path-based over the real fs; /tmp vs /private/tmp
	// canonicalization is handled by sandboxrun's pathForms, but granting
	// the canonical form keeps the grant list honest.
	if canon, err := filepath.EvalSymlinks(tmp); err == nil {
		tmp = canon
	}

	// Daemon-lock staleness is a non-issue in v0 by construction (one
	// short-lived executor per request under a scoped GRADLE_USER_HOME);
	// warm-daemon reuse and any lock hygiene that comes with it is a
	// later ticket — v0 never deletes files inside the cache.

	g := &sandboxrun.Grants{
		Workdir:    worktree,
		AllowPaths: dedupePaths([]string{worktree, leaf, locksDir, tmp}),
		// ReadPaths intentionally empty beyond AllowPaths: the platform
		// backends add the wrapper's directory automatically (the inner
		// binary resolution in BuildChildArgv) and the toolchain/system
		// read baseline comes from sbpl.go's device+system rules. v0
		// grants no host tooling beyond PATH resolution.
		NetworkMode: "blocked",
		Enforcement: "kernel",
	}
	if err := g.Validate(); err != nil {
		return nil, err
	}
	return &BuildGrants{Grants: g, gradleUserHome: leaf, tmpDir: tmp}, nil
}

// CleanupTmp releases the private temp dir (safe to call with a nil receiver
// or after a failed launch).
func (b *BuildGrants) CleanupTmp() {
	if b != nil && b.tmpDir != "" {
		_ = os.RemoveAll(b.tmpDir)
		b.tmpDir = ""
	}
}

// ChildEnv renders the executor environment: nothing inherited from the
// calling harness except the fixed pass-through list, plus the injected
// Gradle/cache/redirect vars. It never contains credential values.
func ChildEnv(b *BuildGrants) []string {
	injected := map[string]string{
		"GRADLE_USER_HOME": b.gradleUserHome,
		"TMPDIR":           b.tmpDir,
	}
	environ := make([]string, 0, len(envPassThrough)+len(injected))
	for _, name := range envPassThrough {
		if v, ok := os.LookupEnv(name); ok && v != "" {
			environ = append(environ, name+"="+v)
		}
	}
	for k, v := range injected {
		environ = append(environ, k+"="+v)
	}
	return environ
}

func ensureDir(path string, perm os.FileMode) error {
	if err := os.MkdirAll(path, perm); err != nil {
		return err
	}
	return os.Chmod(path, perm)
}

func dedupePaths(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range in {
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}
