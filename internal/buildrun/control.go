package buildrun

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

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
//
// The ticket-06 credential-lift init script
// (init.d/registry-credentials.gradle) is likewise existence-filtered: it is
// written only when private registries are approved, so a fresh leaf or a
// no-private-registry build reports it absent.
//
// The ticket-07 checkstyle-twin retirement init script
// (init.d/retire-checkstyle-twins.gradle) is UNCONDITIONALLY written by
// PrepareControlState, so it is always present after PrepareControlState and
// always appears in the control files list. It is a defensive no-op when no
// yarp3 checkstyle*Sandbox twin tasks exist in a project.
var controlFiles = []string{
	"gradle.properties",                                             // OMAC-generated: proxy + jvmargs + resource ceiling
	filepath.Join(controlStateName, "README"),                       // explains the read-only contract
	filepath.Join(controlStateName, buildmanifest.ApprovalFilename), // ticket 05: per-developer approval record
	filepath.Join(controlStateName, buildmanifest.ActiveFilename),   // ticket 05: frozen-for-session active record
	filepath.Join("init.d", registryCredentialsInitName),            // ticket 06: credential-lift init script (when private registries approved)
	filepath.Join("init.d", retireCheckstyleTwinsInitName),          // ticket 07: checkstyle twin retirement (always written)
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
	// RegistryProxyURLs maps each approved private registry alias to the
	// non-secret local loopback URL the credential-lift proxy serves
	// (ticket 06). Empty/nil disables the registry-credentials init.d
	// script. The credential itself NEVER appears here — the URLs are
	// http://127.0.0.1:<port>/<alias>/ with no userinfo.
	RegistryProxyURLs map[string]string
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

// registryCredentialsInitName is the OMAC-authored init script Gradle
// loads at daemon startup to point private registries at the credential-
// lift proxy. It lives in <leaf>/init.d/ (read-only control state).
const registryCredentialsInitName = "registry-credentials.gradle"

// RenderRegistryCredentialsInitScript renders the OMAC-authored Gradle
// init script that points each approved private registry alias at its
// non-secret local loopback URL (the credential-lift proxy, ticket 06).
// Gradle loads it from <leaf>/init.d/registry-credentials.gradle at
// daemon startup; the credential NEVER appears in it — the URLs are
// http://127.0.0.1:<port>/<alias>/ with no userinfo.
//
// The script injects one maven repository per alias at the local proxy
// URL into every project's `repositories { }` block (via `allprojects`),
// so Gradle resolves private dependencies through the credential-lift
// proxy. The developer's build.gradle still declares the upstream
// registry; the injected local mirror is additive (Gradle merges
// repositories by URL). No credentials are configured on the injected
// repository — the proxy authenticates upstream.
//
// Pure string — unit-testable. Returns "" when urls is empty.
func RenderRegistryCredentialsInitScript(urls map[string]string) string {
	if len(urls) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("// OMAC-generated credential-lift init script (ticket 06).\n")
	b.WriteString("// Points each approved private registry alias at the host-side\n")
	b.WriteString("// credential-lift proxy. The credential NEVER appears here;\n")
	b.WriteString("// Gradle sees only the non-secret local loopback URL.\n")
	b.WriteString("// This file is READ-ONLY to the executor (do not edit).\n\n")
	b.WriteString("allprojects {\n")
	b.WriteString("  repositories {\n")
	// Emit in a deterministic order (alias-sorted) so the digest is stable.
	aliases := make([]string, 0, len(urls))
	for a := range urls {
		aliases = append(aliases, a)
	}
	sort.Strings(aliases)
	for _, a := range aliases {
		b.WriteString(fmt.Sprintf("    maven {\n"))
		b.WriteString(fmt.Sprintf("      name = 'omac-credproxy-%s'\n", a))
		b.WriteString(fmt.Sprintf("      url = '%s'\n", urls[a]))
		b.WriteString("      // No credentials here: the credential-lift proxy\n")
		b.WriteString("      // authenticates upstream host-side.\n")
		b.WriteString("    }\n")
	}
	b.WriteString("  }\n")
	b.WriteString("}\n")
	return b.String()
}

// retireCheckstyleTwinsInitName is the OMAC-authored init script Gradle
// loads at daemon startup to neutralize yarp3's machine-local
// checkstyle*Sandbox twin tasks. It lives in <leaf>/init.d/ (read-only
// control state) and is written UNCONDITIONALLY by PrepareControlState —
// the retirement applies to every build (the script is a defensive no-op
// when no twin tasks exist in a project).
const retireCheckstyleTwinsInitName = "retire-checkstyle-twins.gradle"

// RenderRetireCheckstyleTwinsInitScript renders the OMAC-authored Gradle
// init script that retires yarp3's machine-local checkstyle*Sandbox twin
// tasks (ticket 07). Historically yarp3 needed the twins AND a host init
// script because guarded loopback was the goal; ADR 0003 Revision killed
// guarded loopback, so the twins and host init script are no longer
// needed. The JVM build executor's macOS Shape A (env-only network,
// filesystem confinement only) and Linux private-loopback (kernel
// boundary) postures make the canonical checkstyleMain/checkstyleTest
// tasks — which run their Checkstyle analysis through the Gradle Worker
// API process isolation — run unchanged on both platforms.
//
// The script runs BEFORE project task-graph evaluation (via the Gradle
// `beforeProject` hook, which fires during project configuration before
// the task graph is materialized), and for each project neutralizes any
// task whose name matches the yarp3 `checkstyle*Sandbox` twin convention
// by overriding its actions to a no-op. The retirement log line fires at
// configuration time via the init-script `logger` (NOT via `task.doFirst`,
// which the subsequent `task.actions = []` would clear). It uses Gradle's
// task configuration avoidance API (tasks.matching { … }.configureEach {
// … }) so projects without the twins are not configured. The whole hook
// is wrapped in try/catch so a project that fails to configure for
// unrelated reasons is unaffected. The canonical checkstyleMain /
// checkstyleTest tasks are left untouched — they are what actually runs.
//
// Pure string — unit-testable. Always returns a non-empty script (the
// retirement applies to every build; it is a defensive no-op when no
// twins exist).
func RenderRetireCheckstyleTwinsInitScript() string {
	var b strings.Builder
	b.WriteString("// OMAC-generated checkstyle twin retirement init script (ticket 07).\n")
	b.WriteString("// yarp3 historically needed machine-local checkstyle*Sandbox twin\n")
	b.WriteString("// tasks AND a host init script because guarded loopback was the\n")
	b.WriteString("// goal. ADR 0003 Revision retired guarded loopback: the JVM build\n")
	b.WriteString("// executor's macOS Shape A (env-only network, filesystem\n")
	b.WriteString("// confinement only) and Linux private-loopback (kernel boundary)\n")
	b.WriteString("// postures make the canonical checkstyleMain/checkstyleTest tasks\n")
	b.WriteString("// run unchanged through the Gradle Worker API process isolation,\n")
	b.WriteString("// so the twins and host init script are no longer needed. This\n")
	b.WriteString("// script neutralizes any stale machine-local checkstyle*Sandbox\n")
	b.WriteString("// twin so the canonical tasks are what actually runs. It is a\n")
	b.WriteString("// defensive no-op when no twins exist in a project.\n")
	b.WriteString("// This file is READ-ONLY to the executor (do not edit).\n\n")
	b.WriteString("allprojects {\n")
	b.WriteString("  // beforeProject fires during project configuration, BEFORE the\n")
	b.WriteString("  // task graph is materialized, so the twins are neutralized in\n")
	b.WriteString("  // time for the canonical checkstyleMain/checkstyleTest to be the\n")
	b.WriteString("  // only checkstyle tasks that run.\n")
	b.WriteString("  beforeProject { project ->\n")
	b.WriteString("    try {\n")
	b.WriteString("      // Task configuration avoidance: only projects that actually\n")
	b.WriteString("      // declare a checkstyle*Sandbox twin configure it. Projects\n")
	b.WriteString("      // without the twins are unaffected (defensive no-op).\n")
	b.WriteString("      project.tasks.matching { it.name ==~ /checkstyle.*Sandbox/ }.configureEach { task ->\n")
	b.WriteString("        // Log at configuration time (NOT via task.doFirst): a\n")
	b.WriteString("        // subsequent task.actions = [] would clear a doFirst action\n")
	b.WriteString("        // registered here, so the log line must NOT ride on the\n")
	b.WriteString("        // task's action list. The init-script logger is in scope.\n")
	b.WriteString("        logger.lifecycle(\"omac: retiring yarp3 checkstyle twin task {} — canonical checkstyleMain/checkstyleTest run unchanged through the Gradle Worker API (ADR 0003 Revision)\", task.path)\n")
	b.WriteString("        // Replace the twin's actions with a no-op so it cannot run\n")
	b.WriteString("        // the machine-local Checkstyle it was wired for. Setting\n")
	b.WriteString("        // actions = [] AFTER the log line above is correct: the\n")
	b.WriteString("        // log already fired at configuration time, not execution.\n")
	b.WriteString("        task.actions = []\n")
	b.WriteString("      }\n")
	b.WriteString("    } catch (Exception e) {\n")
	b.WriteString("      // A project that fails to configure for unrelated reasons\n")
	b.WriteString("      // must not be broken by the retirement hook.\n")
	b.WriteString("      project.logger.debug(\"omac: checkstyle twin retirement skipped for {}: {}\", project.path, e.message)\n")
	b.WriteString("    }\n")
	b.WriteString("  }\n")
	b.WriteString("}\n")
	return b.String()
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
	// init.d must exist (created read-only by the loop below); create it
	// writable ONCE here so both the conditional registry-credentials
	// script (ticket 06) and the unconditional retire-checkstyle-twins
	// script (ticket 07) can be written, then the loop below locks it to
	// 0o500. Creating it once avoids a redundant idempotent ensureDir on
	// the retire path (the retire script is always written, so this call
	// is the sole creator; the registry path no longer re-creates it).
	if err := ensureDir(filepath.Join(leaf, "init.d"), 0o700); err != nil {
		return ControlPaths{}, fmt.Errorf("prepare init.d for control scripts: %w", err)
	}
	// Ticket 06: write the credential-lift init script BEFORE the init.d
	// control directory is locked read-only (0o500) below. The script
	// carries only non-secret local URLs; the credential NEVER appears in
	// it. It is written only when private registries are approved
	// (RegistryProxyURLs non-empty); a no-op otherwise.
	regInit := RenderRegistryCredentialsInitScript(cfg.RegistryProxyURLs)
	if regInit != "" {
		regInitPath := filepath.Join(leaf, "init.d", registryCredentialsInitName)
		if err := os.WriteFile(regInitPath, []byte(regInit), 0o644); err != nil {
			return ControlPaths{}, fmt.Errorf("write registry-credentials init script: %w", err)
		}
	}
	// Ticket 07: write the checkstyle-twin retirement init script
	// UNCONDITIONALLY (the retirement applies to every build — it is a
	// defensive no-op when no yarp3 checkstyle*Sandbox twins exist).
	// Written BEFORE the init.d control directory is locked read-only
	// (0o500) below, same pattern as the registry script. The script is
	// read-only to the executor: it appears in controlFiles and is
	// granted read access + a write-deny.
	retireInitPath := filepath.Join(leaf, "init.d", retireCheckstyleTwinsInitName)
	if err := os.WriteFile(retireInitPath, []byte(RenderRetireCheckstyleTwinsInitScript()), 0o644); err != nil {
		return ControlPaths{}, fmt.Errorf("write retire-checkstyle-twins init script: %w", err)
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
