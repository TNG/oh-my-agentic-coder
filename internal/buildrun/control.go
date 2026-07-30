package buildrun

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/tngtech/oh-my-agentic-coder/internal/buildmanifest"
)

// Control state: OMAC-generated files under the GRADLE_USER_HOME leaf that
// must be READ-ONLY to the executor. Gradle may read them (so its build
// honors the OMAC-configured proxy / JVM args / init scripts) but must
// never replace or tamper with them — that would let build or test code
// rewrite the OMAC-imposed guardrails.
//
// Normal Gradle state (wrapper dists, dependency caches, daemon registry,
// build cache) lives elsewhere in the leaf and stays writable.

// controlStateName is the OMAC control root inside the leaf. Everything
// under here is OMAC-owned and read-only to the executor.
const controlStateName = ".omac-control"

// controlFiles lists the OMAC-generated control files (relative to the
// leaf) that GrantsFor makes read-only. Gradle reads them; the executor
// cannot write them.
//
// The manifest-approval + active-manifest records (ticket 05) live under
// .omac-control/ alongside the README; they are OMAC-owned, read-only to the
// executor, and store the per-developer approval + frozen-for-session state.
// They are created on demand by internal/buildmanifest (StoreApproval /
// StoreActive) and may be ABSENT on a fresh leaf; resolveControlPaths only
// canonicalizes paths that exist, so their absence does not break the grant
// set (existence-filtered by sandboxrun).
var controlFiles = []string{
	"gradle.properties",                                             // OMAC-generated: proxy + jvmargs + resource ceiling
	filepath.Join(controlStateName, "README"),                       // explains the read-only contract
	filepath.Join(controlStateName, buildmanifest.ApprovalFilename), // ticket 05: per-developer approval record
	filepath.Join(controlStateName, buildmanifest.ActiveFilename),   // ticket 05: frozen-for-session active record
}

// controlDirs lists OMAC-owned control directories (relative to the leaf)
// that must be READ-ONLY to the executor. Gradle loads control state from
// these (init.d/*.gradle are init scripts Gradle runs at daemon startup),
// so a build that could create or overwrite a file here could plant its
// own init script and relax the OMAC-imposed guardrails. GrantsFor grants
// them read access + a write-deny; PrepareControlState creates them (owned
// by omac, mode 0o500) so the executor cannot create them either.
var controlDirs = []string{
	"init.d", // Gradle init-script directory: loaded as control state
}

// GradlePropertiesConfig is the set of OMAC-imposed Gradle settings written
// to <leaf>/gradle.properties. Build/test code cannot override these
// because the file is read-only to the executor.
type GradlePropertiesConfig struct {
	// Proxy wires Gradle's daemon at the omac filtered proxy (zero value
	// disables the proxy lines). Injected via system properties so
	// every JVM the Gradle daemon spawns (workers, test executors) honors
	// them without per-invocation GRADLE_OPTS.
	Proxy ProxyEndpoint
	// MaxHeap is the Gradle daemon JVM -Xmx ceiling (e.g. "1g"). Empty
	// omits the line (host default applies).
	MaxHeap string
}

// RenderGradleProperties renders the OMAC-generated gradle.properties
// content. Pure string — unit-testable.
func RenderGradleProperties(cfg GradlePropertiesConfig) string {
	var b string
	if cfg.Proxy.Valid() {
		b += fmt.Sprintf("systemProp.http.proxyHost=%s\n", cfg.Proxy.Host)
		b += fmt.Sprintf("systemProp.http.proxyPort=%d\n", cfg.Proxy.Port)
		b += fmt.Sprintf("systemProp.https.proxyHost=%s\n", cfg.Proxy.Host)
		b += fmt.Sprintf("systemProp.https.proxyPort=%d\n", cfg.Proxy.Port)
		// Loopback must NOT be proxied: the Gradle daemon talks to its
		// workers over a random loopback port.
		b += "systemProp.http.nonProxyHosts=localhost|127.*|[::1]\n"
		// Java 8u111+ disables Basic auth on HTTPS CONNECT tunnels by
		// default; re-enable so the proxy token is accepted (public
		// resolution in this ticket carries no token; ticket 06 adds it).
		b += "systemProp.jdk.http.auth.tunneling.disabledSchemes=\n"
	}
	if cfg.MaxHeap != "" {
		b += fmt.Sprintf("org.gradle.jvmargs=-Xmx%s\n", cfg.MaxHeap)
	}
	return b
}

// controlStateReadme is the explanatory text placed at
// <leaf>/.omac-control/README so a build that tries to overwrite an
// OMAC control file gets a legible denial rather than an opaque EPERM.
const controlStateReadme = `This directory and the files it documents are OMAC control state.
They are READ-ONLY to the build executor on purpose: OMAC owns Gradle's
init scripts (init.d/*.gradle), the user-level gradle.properties, and
the OMAC-generated control configuration under .omac-control/, setting
the proxy, JVM args, and resource ceilings here so build or test code
cannot relax them.

A write to any of these surfaces to the build as an EPERM (denied by the
OMAC sandbox) because they are granted read-only. Do NOT try to write,
replace, or create files here — that is rejected by the sandbox, not by
Gradle, and the rejection is enforced at the kernel level.

To change Gradle build behavior, use the supported alternatives:
  - project-level build.gradle / settings.gradle in the worktree
    (checked in, fully writable, the normal Gradle configuration surface)
  - project-level gradle.properties at <root>/gradle.properties (not the
    user-level one OMAC generates here)
  - the OMAC build manifest at .omac/build.yaml for non-standard
    capabilities (containers, resource requests) — approved once, then
    frozen for the session

OMAC regenerates these control files on each 'omac build'.
`

// PrepareControlState writes the OMAC control files under the leaf,
// creates the OMAC-owned control directories (init.d), and returns the
// paths that must be granted READ-ONLY (ReadPaths + WriteDenyPaths) to
// the executor — both the control files and the control directories.
// The leaf dir must already exist (GrantsFor ensures it).
//
// The control directories are created OMAC-owned (mode 0o500: readable
// + executable, NOT writable) so the executor cannot create them if
// absent and cannot plant a file inside them. Gradle loads init.d/*.gradle
// as init scripts, so the directory itself must be read-only to the
// executor — otherwise build code could create <leaf>/init.d/evil.gradle
// and have Gradle run it at daemon startup.
//
// Files already present are overwritten with the current OMAC config so a
// stale config from a prior run never survives — but the executor itself
// can never write them (they are read-only under the sandbox), so the
// only writer is OMAC running unsandboxed here.
func PrepareControlState(leaf string, cfg GradlePropertiesConfig) (ControlPaths, error) {
	ctrlDir := filepath.Join(leaf, controlStateName)
	if err := ensureDir(ctrlDir, 0o700); err != nil {
		return ControlPaths{}, fmt.Errorf("prepare control state dir: %w", err)
	}
	// OMAC-owned control directories (init.d): create them read-only to
	// the executor so Gradle can read init scripts from them but build
	// code cannot plant one. 0o500 = r-x for owner (omac): readable +
	// traversable, not writable.
	for _, rel := range controlDirs {
		if err := ensureDir(filepath.Join(leaf, rel), 0o500); err != nil {
			return ControlPaths{}, fmt.Errorf("prepare control dir %s: %w", rel, err)
		}
	}
	readme := filepath.Join(ctrlDir, "README")
	if err := os.WriteFile(readme, []byte(controlStateReadme), 0o644); err != nil {
		return ControlPaths{}, fmt.Errorf("write control README: %w", err)
	}
	propsPath := filepath.Join(leaf, "gradle.properties")
	if err := os.WriteFile(propsPath, []byte(RenderGradleProperties(cfg)), 0o644); err != nil {
		return ControlPaths{}, fmt.Errorf("write gradle.properties: %w", err)
	}
	return resolveControlPaths(leaf), nil
}

// ControlPaths holds the leaf-relative OMAC control paths that must be
// granted READ-ONLY (ReadPaths + WriteDenyPaths) to the executor: both
// the control files and the control directories (init.d).
type ControlPaths struct {
	// Files are the control files (gradle.properties, .omac-control/README).
	Files []string
	// Dirs are the OMAC-owned control directories (init.d) that Gradle
	// loads control state from. The directory itself is read-only to the
	// executor so it cannot plant a file inside.
	Dirs []string
}

// All returns Files and Dirs concatenated (ReadPaths + WriteDenyPaths
// treat them identically: readable, not writable).
func (c ControlPaths) All() []string {
	return append(append([]string{}, c.Files...), c.Dirs...)
}

// resolveControlPaths returns the canonical (symlink-resolved) control
// paths for the leaf WITHOUT writing them. Used by PrepareControlState
// (after writing) and by GrantsFor (via PrepareControlState) so the
// control files AND the init.d control directory are granted read-only.
//
// Manifest approval / active records (ticket 05) live under .omac-control/
// but are created on demand by internal/buildmanifest (StoreApproval /
// StoreActive), NOT by PrepareControlState. They are included only when
// they actually exist on disk — a fresh leaf without a manifest therefore
// reports only gradle.properties + README (2 files), and a leaf with an
// approved manifest reports 4. This keeps the grant set honest: paths in
// ReadPaths / WriteDenyPaths should exist (sandboxrun existence-filters
// them anyway, and a phantom path in the test-asserted count is noise).
func resolveControlPaths(leaf string) ControlPaths {
	canonical := func(rel string) string {
		p := filepath.Join(leaf, rel)
		if canon, err := filepath.EvalSymlinks(p); err == nil {
			p = canon
		}
		return p
	}
	// exists reports whether the file at rel exists (regular file). The
	// manifest records may be absent; gradle.properties / README / init.d
	// are always present after PrepareControlState.
	exists := func(rel string) bool {
		_, err := os.Stat(filepath.Join(leaf, rel))
		return err == nil
	}
	var files []string
	for _, rel := range controlFiles {
		if !exists(rel) {
			continue
		}
		files = append(files, canonical(rel))
	}
	var dirs []string
	for _, rel := range controlDirs {
		dirs = append(dirs, canonical(rel))
	}
	return ControlPaths{Files: files, Dirs: dirs}
}
