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
	filepath.Join("init.d", mockitoAgentInitName),                   // ticket 08: mockito -javaagent (always written)
	filepath.Join("init.d", daemonOwnerHandshakeInitName),           // ticket 07: daemon-owner handshake (always written; no-op when no marker)
	filepath.Join(controlStateName, executorTmpDirName),             // current run's executor temp (read by the mockito-agent init script)
	filepath.Join(controlStateName, daemonHandshakeSockName),        // ticket 07: daemon-handshake socket path (read by the daemon-owner-handshake init script; when set)
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
	// InstallationsPaths is the list of host JDK install roots (parents
	// of bin/) Gradle should know about for toolchain auto-detection.
	// Written to gradle.properties as
	// org.gradle.java.installations.paths so Gradle matches a pinned
	// toolchain spec against installed JDKs WITHOUT calling
	// /usr/libexec/java_home inside the sandbox (which fails — the
	// sandbox breaks java_home's LaunchServices/Spotlight enumeration
	// even though the binary runs and the directories are readable).
	// The supervisor enumerates these unsandboxed (EnumerateHostJDKs).
	// Empty/nil omits the line (Gradle falls back to its own detection).
	InstallationsPaths []string
	// TmpDir is the executor's private temporary directory (the only
	// writable temp leaf under the sandbox). Written to a control-state
	// file (<leaf>/.omac-control/executor-tmpdir) so the mockito-agent
	// init script can read the CURRENT run's temp from the file instead
	// of the Gradle DAEMON's env (a warm daemon retains a prior run's
	// TMPDIR, which has been deleted on exit — reading the env would
	// point the test worker's java.io.tmpdir at a non-existent dir).
	// Empty omits the file (the init script falls back to the env).
	TmpDir string
	// DaemonOwnerMarker is the cryptographically random, unguessable
	// owner marker the host injects into the Gradle daemon JVM args
	// (ticket 07, spec.md §237). When non-empty, RenderGradleProperties
	// appends `-Domac.daemon.owner=<marker>` to the
	// org.gradle.jvmargs line so the Gradle daemon carries it as a
	// system property; the daemon-owner-handshake init script reads it
	// back and echoes it over the executor supervisor's private control
	// channel (see RenderDaemonOwnerHandshakeInitScript). The marker is
	// NOT a credential (it is an ownership claim, not a secret) but it
	// MUST be unguessable so a stale or PID-recycled process cannot
	// spoof it. Empty omits the system property (a non-omac build
	// reusing the leaf, or a warm daemon from before omac — the
	// handshake init script is a no-op then). Minted by
	// NewDaemonOwnerMarker and written into the pending DaemonRecord
	// by the engine (Phase 3); Phase 2 only exposes the injection.
	DaemonOwnerMarker DaemonOwnerMarker
	// DaemonHandshakeSock is the path of the executor supervisor's
	// private Unix socket the Gradle daemon writes its handshake to
	// (ticket 07, spec.md §237). When non-empty, PrepareControlState
	// writes it to a control-state file
	// (<leaf>/.omac-control/daemon-handshake-sock) that the
	// daemon-owner-handshake init script reads at daemon startup, so
	// the socket path reaches the daemon via a control-state FILE
	// rather than an additional JVM arg (consistent with the mockito
	// init script's executor-tmpdir pattern). Empty omits the file
	// (the init script falls back to no socket → no-op, e.g. a non-omac
	// build or a Phase-2-only render without the engine wiring).
	DaemonHandshakeSock string
}

// RenderGradleProperties renders the OMAC-generated gradle.properties
// content. Pure string — unit-testable.
func RenderGradleProperties(cfg GradlePropertiesConfig) string {
	var b strings.Builder
	if cfg.Proxy.Valid() {
		fmt.Fprintf(&b, "systemProp.http.proxyHost=%s\n", cfg.Proxy.Host)
		fmt.Fprintf(&b, "systemProp.http.proxyPort=%d\n", cfg.Proxy.Port)
		fmt.Fprintf(&b, "systemProp.https.proxyHost=%s\n", cfg.Proxy.Host)
		fmt.Fprintf(&b, "systemProp.https.proxyPort=%d\n", cfg.Proxy.Port)
		// Loopback must NOT be proxied: the Gradle daemon talks to its
		// workers over a random loopback port.
		b.WriteString("systemProp.http.nonProxyHosts=localhost|127.*|[::1]\n")
		// Java 8u111+ disables Basic auth on HTTPS CONNECT tunnels by
		// default; re-enable so the proxy token is accepted (public
		// resolution in this ticket carries no token; ticket 06 adds it).
		b.WriteString("systemProp.jdk.http.auth.tunneling.disabledSchemes=\n")
	}
	if cfg.MaxHeap != "" {
		fmt.Fprintf(&b, "org.gradle.jvmargs=-Xmx%s", cfg.MaxHeap)
		// Ticket 07: append the daemon-owner marker as a JVM system
		// property so the Gradle daemon carries it. The
		// daemon-owner-handshake init script reads it back at daemon
		// startup and echoes it over the executor supervisor's private
		// control channel. Deterministic order: MaxHeap first, then the
		// marker (stable across renders so the file digest is stable).
		// The marker is NOT a credential — it is an ownership claim
		// (see DaemonOwnerMarker); it MUST be unguessable so a stale
		// or PID-recycled process cannot spoof it. Empty marker omits
		// the property (a non-omac build or a Phase-2-only render).
		if cfg.DaemonOwnerMarker != "" {
			fmt.Fprintf(&b, " -Domac.daemon.owner=%s", cfg.DaemonOwnerMarker)
		}
		b.WriteString("\n")
	} else if cfg.DaemonOwnerMarker != "" {
		// Marker only (no MaxHeap): emit the jvmargs line with just the
		// -Domac.daemon.owner system property.
		fmt.Fprintf(&b, "org.gradle.jvmargs=-Domac.daemon.owner=%s\n", cfg.DaemonOwnerMarker)
	}
	// Host JDK install roots for toolchain auto-detection. Gradle's
	// /usr/libexec/java_home -V call fails inside the sandbox (the
	// binary runs but finds nothing — LaunchServices/Spotlight
	// enumeration is broken by the sandbox). The supervisor enumerates
	// these unsandboxed and declares them here so Gradle matches a
	// pinned toolchain spec against installed JDKs without calling
	// java_home at all.
	if len(cfg.InstallationsPaths) > 0 {
		b.WriteString("org.gradle.java.installations.paths=" + strings.Join(cfg.InstallationsPaths, ",") + "\n")
	}
	return b.String()
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
		b.WriteString("    maven {\n")
		b.WriteString("      name = 'omac-credproxy-" + a + "'\n")
		b.WriteString("      url = '" + urls[a] + "'\n")
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

// mockitoAgentInitName is the OMAC-authored init script Gradle loads at
// daemon startup to add mockito-core as a -javaagent on test tasks. It
// lives in <leaf>/init.d/ (read-only control state) and is written
// UNCONDITIONALLY by PrepareControlState — the agent applies to every
// build (it is a defensive no-op when no test task uses Mockito).
const mockitoAgentInitName = "mockito-agent.gradle"

// executorTmpDirName is the control-state file holding the CURRENT
// run's executor private temp path. The mockito-agent init script reads
// this to set java.io.tmpdir on the test worker, instead of the Gradle
// daemon's env TMPDIR (stale on a warm daemon — see GradlePropertiesConfig.TmpDir).
const executorTmpDirName = "executor-tmpdir"

// daemonHandshakeSockName is the control-state file holding the path
// of the executor supervisor's private Unix socket the Gradle daemon
// writes its handshake to (ticket 07, spec.md §237). The
// daemon-owner-handshake init script reads this at daemon startup to
// learn where to send its {"pid","marker"} JSON, instead of receiving
// the socket path via an additional JVM arg — consistent with the
// mockito init script's executor-tmpdir pattern (control-state FILE
// preferred over a threaded JVM arg). Written by PrepareControlState
// when GradlePropertiesConfig.DaemonHandshakeSock is set; the init
// script is a no-op when the file is absent (a non-omac build reusing
// the leaf, or a warm daemon from before omac).
const daemonHandshakeSockName = "daemon-handshake-sock"

// RenderMockitoAgentInitScript renders the OMAC-authored Gradle init
// script that loads mockito-core as a -javaagent on test tasks (ticket 08,
// REPORT.md item 4 / spec.md:168). Mockito's inline mock-maker cannot
// self-attach its ByteBuddy agent under the JVM build executor's
// restrictions; the trusted generated config reproduces the host
// ~/.gradle/init.gradle workaround without importing host Gradle state.
//
// The script mirrors the captured reference at
// .scratch/jvm-build-executor/02-testcontainers-capture/gradle-home/init.d/
// mockito-agent.gradle: it configures every Test task to enable dynamic
// agent loading AND to locate the mockito-core jar on the test runtime
// classpath, adding it as -javaagent. The jar is located via the test
// task's classpath (a test-runtime entry) at doFirst time, so it resolves
// after dependency resolution; a missing jar is silently skipped (a
// project without Mockito is unaffected). The script is read-only control
// state: build code cannot relax it.
//
// Pure string — unit-testable. Always returns a non-empty script (the
// agent applies to every build; it is a defensive no-op when no test task
// uses Mockito or the jar is absent).
//
// The script also forces java.io.tmpdir to the executor's private temp
// ($TMPDIR, injected via ChildEnv in grants.go). Without this the JVM
// resolves java.io.tmpdir to the macOS default /var/folders/.../T/, which
// the sandbox deliberately does NOT grant writable (the private temp is
// the only writable temp leaf). Tooling that writes its temp under
// java.io.tmpdir — e.g. the embedded Kafka broker's
// TestUtils.tempDirectory() log dir (spring-kafka-test's
// GlobalEmbeddedKafkaTestExecutionListener) — would otherwise hit EPERM
// and fail silently, leaving dependent config (spring.embedded.kafka.brokers)
// unset. Aligning java.io.tmpdir with the sandbox-granted temp fixes this
// and any other tool that assumes java.io.tmpdir == $TMPDIR.
func RenderMockitoAgentInitScript() string {
	var b strings.Builder
	b.WriteString("// OMAC-generated mockito-agent init script (ticket 08).\n")
	b.WriteString("// Mockito's inline mock-maker cannot self-attach its ByteBuddy agent\n")
	b.WriteString("// under the JVM build executor's restrictions; this trusted generated\n")
	b.WriteString("// config reproduces the host ~/.gradle/init.gradle workaround without\n")
	b.WriteString("// importing host Gradle state. Loads mockito-core as a -javaagent on\n")
	b.WriteString("// test tasks. Defensive no-op when no test task uses Mockito or the\n")
	b.WriteString("// jar is absent from the test runtime classpath.\n")
	b.WriteString("// This file is READ-ONLY to the executor (do not edit).\n\n")
	b.WriteString("allprojects {\n")
	b.WriteString("  tasks.withType(Test).configureEach {\n")
	b.WriteString("    // Enable dynamic agent loading so the -javaagent attach is permitted.\n")
	b.WriteString("    jvmArgs '-XX:+EnableDynamicAgentLoading'\n")
	b.WriteString("    doFirst {\n")
	b.WriteString("      // Force java.io.tmpdir to the executor's private temp. The JVM\n")
	b.WriteString("      // otherwise defaults to the macOS /var/folders/.../T/ leaf,\n")
	b.WriteString("      // which the sandbox does NOT grant writable — only the private\n")
	b.WriteString("      // temp is writable. Tooling that writes its temp under\n")
	b.WriteString("      // java.io.tmpdir (e.g. the embedded Kafka broker log dir via\n")
	b.WriteString("      // TestUtils.tempDirectory) would otherwise hit EPERM and fail\n")
	b.WriteString("      // silently.\n")
	b.WriteString("      //\n")
	b.WriteString("      // Source priority: a control-state FILE (written fresh each\n")
	b.WriteString("      // build by the supervisor) is preferred over the daemon env\n")
	b.WriteString("      // TMPDIR. A warm Gradle daemon retains a PRIOR run's TMPDIR,\n")
	b.WriteString("      // which has been deleted on exit — reading the env would point\n")
	b.WriteString("      // the test worker at a non-existent dir. The file is at\n")
	b.WriteString("      // <gradleUserHomeDir>/.omac-control/executor-tmpdir. Fall back\n")
	b.WriteString("      // to the env only when the file is absent (cold-daemon path\n")
	b.WriteString("      // or a non-omac build reusing the init script).\n")
	b.WriteString("      //\n")
	b.WriteString("      // This runs in doFirst (execution time) NOT at configuration\n")
	b.WriteString("      // time: a warm daemon caches the configureEach closure from\n")
	b.WriteString("      // the first build that evaluated it, so a config-time jvmArgs\n")
	b.WriteString("      // would bake in the FIRST run's tmpdir. doFirst re-evaluates\n")
	b.WriteString("      // on every build, reading the file fresh each time.\n")
	b.WriteString("      def omacTmp = null\n")
	b.WriteString("      try {\n")
	b.WriteString("        def tmpFile = new File(gradle.gradleUserHomeDir, '.omac-control/executor-tmpdir')\n")
	b.WriteString("        if (tmpFile.isFile()) {\n")
	b.WriteString("          omacTmp = tmpFile.text.trim()\n")
	b.WriteString("        }\n")
	b.WriteString("      } catch (Exception ignored) {}\n")
	b.WriteString("      if (omacTmp == null || omacTmp.isEmpty()) {\n")
	b.WriteString("        omacTmp = System.getenv('TMPDIR')\n")
	b.WriteString("      }\n")
	b.WriteString("      if (omacTmp != null && !omacTmp.isEmpty()) {\n")
	b.WriteString("        jvmArgs \"-Djava.io.tmpdir=${omacTmp}\"\n")
	b.WriteString("      }\n")
	b.WriteString("      // Locate the mockito-core jar on the test runtime classpath. The\n")
	b.WriteString("      // classpath is resolved by doFirst time, so the jar is present\n")
	b.WriteString("      // here iff the project depends on mockito-core.\n")
	b.WriteString("      def mockitoJar = classpath.find { it.name ==~ /mockito-core-.*\\.jar/ }\n")
	b.WriteString("      if (mockitoJar != null) {\n")
	b.WriteString("        jvmArgs \"-javaagent:${mockitoJar.absolutePath}\"\n")
	b.WriteString("      }\n")
	b.WriteString("    }\n")
	b.WriteString("  }\n")
	b.WriteString("}\n")
	return b.String()
}

// daemonOwnerHandshakeInitName is the OMAC-authored init script Gradle
// loads at daemon startup to send its PID + the owner marker back to
// the executor supervisor's private control channel BEFORE project
// configuration proceeds (ticket 07, spec.md §237). It lives in
// <leaf>/init.d/ (read-only control state) and is written
// UNCONDITIONALLY by PrepareControlState — the handshake applies to
// every OMAC-owned daemon, and the script is a defensive no-op when
// the -Domac.daemon.owner system property is absent (a non-omac build
// reusing the leaf, or a warm daemon from before omac that predates
// the marker injection). The host's DaemonHandshakeChannel awaits the
// handshake and blocks the wrapper from proceeding until the daemon
// has registered; if the host fails to verify the daemon (marker
// mismatch or procidentity mismatch) it does NOT acknowledge, the
// init script throws a GradleException, and the build fails closed.
const daemonOwnerHandshakeInitName = "daemon-owner-handshake.gradle"

// RenderDaemonOwnerHandshakeInitScript renders the OMAC-authored Gradle
// init script that performs the pending-to-active daemon ownership
// handshake (ticket 07, spec.md §237). At daemon startup, BEFORE
// project configuration proceeds, the script:
//
//  1. Reads the -Domac.daemon.owner=<marker> system property set by
//     the host in gradle.properties org.gradle.jvmargs. If absent, the
//     daemon is not OMAC-owned → the script is a no-op (a non-omac
//     build reusing the leaf, or a warm daemon from before omac).
//  2. Reads the executor supervisor's private control-channel socket
//     path from the control-state file
//     <gradleUserHomeDir>/.omac-control/daemon-handshake-sock
//     (written by PrepareControlState). Reading from a control-state
//     FILE (rather than a second JVM arg) is consistent with the
//     mockito init script's executor-tmpdir pattern.
//  3. Opens the Unix socket, sends a single line
//     `{"pid":<pid>,"marker":"<marker>"}` (the daemon's PID via the
//     portable Java 8+ ManagementFactory.getRuntimeMXBean().getName()
//     split("@")[0] — avoids the Java 9+ ProcessHandle API because
//     Gradle 8+ requires Java 8+ but daemons run on the configured
//     toolchain, which may be Java 8), then blocks on a single-byte
//     ack from the host.
//  4. Waits for the host's acknowledgement (a single byte "1") before
//     returning. The ack is a single byte, NOT a line: the host writes
//     one byte and the script reads one byte, so no line terminator
//     convention is needed. If the host closes without acking or the
//     socket breaks, the script throws a GradleException so the
//     wrapper cannot proceed unverified (fail closed). A bounded
//     30s timeout prevents a hung host from deadlocking Gradle
//     forever — the script throws after the timeout.
//
// The script is wrapped in try/catch so a project that fails for
// unrelated reasons is not broken by the handshake; the
// GradleException is re-thrown ONLY for handshake failures (marker
// missing after the system property was non-empty, socket connect
// failure, read timeout, or host close without ack). The script is a
// defensive no-op when the system property is absent.
//
// Pure string — unit-testable. Always returns a non-empty script (the
// handshake applies to every OMAC-owned build; it is a defensive
// no-op when no marker property is present, like the
// retire-checkstyle-twins script is a defensive no-op when no twins
// exist).
func RenderDaemonOwnerHandshakeInitScript() string {
	var b strings.Builder
	b.WriteString("// OMAC-generated daemon-owner handshake init script (ticket 07).\n")
	b.WriteString("// Makes a newly-started OMAC-owned Gradle daemon send its PID and\n")
	b.WriteString("// the owner marker back to the executor supervisor's private control\n")
	b.WriteString("// channel BEFORE project configuration proceeds. The host verifies\n")
	b.WriteString("// the process (procidentity) and atomically promotes the pending\n")
	b.WriteString("// ownership record to active before acknowledging; the wrapper CANNOT\n")
	b.WriteString("// continue without that acknowledgement. Fail closed: a marker\n")
	b.WriteString("// mismatch, a procidentity mismatch, or a host close without ack\n")
	b.WriteString("// throws a GradleException so the build fails rather than proceed\n")
	b.WriteString("// unverified. Defensive no-op when -Domac.daemon.owner is absent (a\n")
	b.WriteString("// non-omac build reusing the leaf, or a warm daemon from before\n")
	b.WriteString("// omac that predates the marker injection).\n")
	b.WriteString("// This file is READ-ONLY to the executor (do not edit).\n\n")
	b.WriteString("import groovy.json.JsonOutput\n")
	b.WriteString("import java.lang.management.ManagementFactory\n")
	b.WriteString("\n")
	b.WriteString("// The marker the host injected into org.gradle.jvmargs as\n")
	b.WriteString("// -Domac.daemon.owner=<marker>. Absent => this daemon is not\n")
	b.WriteString("// OMAC-owned (a non-omac build reusing the leaf, or a warm daemon\n")
	b.WriteString("// from before omac). The script is a defensive no-op then.\n")
	b.WriteString("def omacMarker = System.getProperty('omac.daemon.owner')\n")
	b.WriteString("if (omacMarker == null || omacMarker.isEmpty()) {\n")
	b.WriteString("  return\n")
	b.WriteString("}\n")
	b.WriteString("\n")
	b.WriteString("// The executor supervisor's private Unix socket path, written by\n")
	b.WriteString("// the host to a control-state file so the socket path reaches the\n")
	b.WriteString("// daemon via a FILE (consistent with the executor-tmpdir pattern)\n")
	b.WriteString("// rather than a second JVM arg. Absent => no channel wired (a\n")
	b.WriteString("// Phase-2-only render or a non-omac build); the script is a no-op.\n")
	b.WriteString("def sockPath = null\n")
	b.WriteString("try {\n")
	b.WriteString("  def sockFile = new File(gradle.gradleUserHomeDir, '.omac-control/daemon-handshake-sock')\n")
	b.WriteString("  if (sockFile.isFile()) {\n")
	b.WriteString("    sockPath = sockFile.text.trim()\n")
	b.WriteString("  }\n")
	b.WriteString("} catch (Exception ignored) {}\n")
	b.WriteString("if (sockPath == null || sockPath.isEmpty()) {\n")
	b.WriteString("  return\n")
	b.WriteString("}\n")
	b.WriteString("\n")
	b.WriteString("// The daemon's PID. ManagementFactory.getRuntimeMXBean().getName()\n")
	b.WriteString("// returns \"<pid>@<hostname>\" on every JVM since Java 1.8 (the\n")
	b.WriteString("// classic portable PID extraction); avoids the Java 9+\n")
	b.WriteString("// ProcessHandle API because Gradle 8+ requires Java 8+ but\n")
	b.WriteString("// daemons run on the configured toolchain, which may be Java 8\n")
	b.WriteString("// (ProcessHandle is Java 9+).\n")
	// toInteger() coerces the split's String to an Integer so
	// JsonOutput.toJson emits {"pid":12345,...} — UNQUOTED. The Go side
	// (DaemonHandshakePID.PID) is an int; a quoted pid would make
	// json.Unmarshal fail the handshake (string → int mismatch).
	b.WriteString("def pid = ManagementFactory.getRuntimeMXBean().getName().split(\"@\")[0].toInteger()\n")
	b.WriteString("\n")
	b.WriteString("// Send {\"pid\":<pid>,\"marker\":\"<marker>\"} as a single line, then\n")
	b.WriteString("// block on a one-byte ack. The ack is a single byte \"1\", NOT a\n")
	b.WriteString("// line — the host writes one byte, the script reads one byte, so\n")
	b.WriteString("// no line terminator convention is needed. A 30s bounded timeout\n")
	b.WriteString("// prevents a hung host from deadlocking Gradle forever; the\n")
	b.WriteString("// script throws a GradleException after the timeout (fail closed).\n")
	b.WriteString("//\n")
	b.WriteString("// The read timeout is implemented via a CountDownLatch + a worker\n")
	b.WriteString("// thread because SocketChannel.socket().setSoTimeout is a NO-OP on a\n")
	b.WriteString("// blocking channel (the JVM documents this). The worker reads the\n")
	b.WriteString("// one-byte ack; the main thread awaits the latch with the 30s bound\n")
	b.WriteString("// and throws a GradleException on timeout so the build fails closed.\n")
	b.WriteString("try {\n")
	b.WriteString("  // Open the Unix-domain socket via java.net.UnixDomainSocketAddress\n")
	b.WriteString("  // (Java 16+). The PID extraction above is portable back to Java 8,\n")
	b.WriteString("  // but Unix-domain socket client support requires Java 16+. The\n")
	b.WriteString("  // omac host resolves the daemon JDK and the handshake requires a\n")
	b.WriteString("  // JDK new enough to support it; a Java 8 daemon fails closed here\n")
	b.WriteString("  // (the catch maps any Exception to a GradleException so the build\n")
	b.WriteString("  // fails rather than proceeds unverified).\n")
	b.WriteString("  def addr = java.net.UnixDomainSocketAddress.of(sockPath)\n")
	b.WriteString("  def sock = java.nio.channels.SocketChannel.open(addr)\n")
	b.WriteString("  try {\n")
	b.WriteString("    def out = new java.io.OutputStreamWriter(java.nio.channels.Channels.newOutputStream(sock), 'UTF-8')\n")
	b.WriteString("    def payload = JsonOutput.toJson([pid: pid, marker: omacMarker]) + \"\\n\"\n")
	b.WriteString("    out.write(payload)\n")
	b.WriteString("    out.flush()\n")
	b.WriteString("    // Read the one-byte ack on a worker thread so the main thread\n")
	b.WriteString("    // can bound the wait. read() returns -1 on EOF (host closed\n")
	b.WriteString("    // without acking) → the worker records -1, the main thread sees\n")
	b.WriteString("    // it and throws to fail closed. A 30s timeout on the latch also\n")
	b.WriteString("    // throws, so a hung host cannot deadlock Gradle forever.\n")
	b.WriteString("    def latch = new java.util.concurrent.CountDownLatch(1)\n")
	b.WriteString("    def ackHolder = new java.util.concurrent.atomic.AtomicInteger(-1)\n")
	b.WriteString("    def readErr = new java.util.concurrent.atomic.AtomicReference(null)\n")
	b.WriteString("    def worker = Thread.start {\n")
	b.WriteString("      try {\n")
	b.WriteString("        def inp = java.nio.channels.Channels.newInputStream(sock)\n")
	b.WriteString("        ackHolder.set(inp.read())\n")
	b.WriteString("      } catch (Exception e) {\n")
	b.WriteString("        readErr.set(e)\n")
	b.WriteString("      } finally {\n")
	b.WriteString("        latch.countDown()\n")
	b.WriteString("      }\n")
	b.WriteString("    }\n")
	b.WriteString("    if (!latch.await(30000, java.util.concurrent.TimeUnit.MILLISECONDS)) {\n")
	b.WriteString("      worker.interrupt()\n")
	b.WriteString("      throw new GradleException(\"omac: daemon handshake timed out waiting for host ack (30s)\")\n")
	b.WriteString("    }\n")
	b.WriteString("    if (readErr.get() != null) {\n")
	b.WriteString("      throw new GradleException(\"omac: daemon handshake read failed: \" + readErr.get().message, readErr.get())\n")
	b.WriteString("    }\n")
	b.WriteString("    int ack = ackHolder.get()\n")
	b.WriteString("    if (ack != ((int) '1')) {\n")
	b.WriteString("      throw new GradleException(\"omac: daemon handshake failed — host did not acknowledge (ack=\" + ack + \")\")\n")
	b.WriteString("    }\n")
	b.WriteString("  } finally {\n")
	b.WriteString("    sock.close()\n")
	b.WriteString("  }\n")
	b.WriteString("} catch (GradleException e) {\n")
	b.WriteString("  throw e\n")
	b.WriteString("} catch (Exception e) {\n")
	b.WriteString("  // Socket connect failure, read failure, or host close without\n")
	b.WriteString("  // ack → fail closed so the wrapper cannot proceed unverified.\n")
	b.WriteString("  throw new GradleException(\"omac: daemon handshake failed: \" + e.message, e)\n")
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
	// Ticket 08: write the mockito-agent init script UNCONDITIONALLY (the
	// agent applies to every build — it is a defensive no-op when no test
	// task uses Mockito or the jar is absent). Written BEFORE the init.d
	// control directory is locked read-only (0o500) below, same pattern
	// as the registry/retire scripts. The script is read-only to the
	// executor: it appears in controlFiles and is granted read access +
	// a write-deny.
	mockitoInitPath := filepath.Join(leaf, "init.d", mockitoAgentInitName)
	if err := os.WriteFile(mockitoInitPath, []byte(RenderMockitoAgentInitScript()), 0o644); err != nil {
		return ControlPaths{}, fmt.Errorf("write mockito-agent init script: %w", err)
	}
	// Ticket 07: write the daemon-owner handshake init script
	// UNCONDITIONALLY (the handshake applies to every OMAC-owned build —
	// it is a defensive no-op when the -Domac.daemon.owner system
	// property is absent, like the retire-checkstyle-twins script is a
	// defensive no-op when no twins exist). Written BEFORE the init.d
	// control directory is locked read-only (0o500) below, same pattern
	// as the registry/retire/mockito scripts. The script is read-only to
	// the executor: it appears in controlFiles and is granted read
	// access + a write-deny.
	handshakeInitPath := filepath.Join(leaf, "init.d", daemonOwnerHandshakeInitName)
	if err := os.WriteFile(handshakeInitPath, []byte(RenderDaemonOwnerHandshakeInitScript()), 0o644); err != nil {
		return ControlPaths{}, fmt.Errorf("write daemon-owner-handshake init script: %w", err)
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
	// Executor temp file: the mockito-agent init script reads this to
	// set java.io.tmpdir on the test worker. Reading from a file (not
	// the daemon env) is REQUIRED because a warm Gradle daemon retains
	// a prior run's TMPDIR, which has been deleted on exit. The file is
	// regenerated each build with the current run's temp. Best-effort:
	// a write failure degrades to the init script's env fallback (the
	// cold-daemon path still works), so it does not fail the build.
	if cfg.TmpDir != "" {
		tmpFile := filepath.Join(ctrlDir, executorTmpDirName)
		if err := os.WriteFile(tmpFile, []byte(cfg.TmpDir), 0o644); err != nil {
			return ControlPaths{}, fmt.Errorf("write executor-tmpdir control file: %w", err)
		}
	}
	// Ticket 07: write the daemon-handshake socket-path control file
	// when the executor supervisor's private Unix socket path is known.
	// The daemon-owner-handshake init script reads this at daemon
	// startup to learn where to send its {"pid","marker"} JSON. Best-
	// effort write failure degrades to the init script's no-op path
	// (no socket file → the daemon does not register → the host's
	// AwaitHandshake times out → the build fails closed), but a write
	// failure here is surfaced because it means the host's trusted
	// control state could not be written, not just a graceful
	// degradation. Empty DaemonHandshakeSock omits the file (the init
	// script falls back to its no-op path).
	if cfg.DaemonHandshakeSock != "" {
		sockFile := filepath.Join(ctrlDir, daemonHandshakeSockName)
		if err := os.WriteFile(sockFile, []byte(cfg.DaemonHandshakeSock), 0o644); err != nil {
			return ControlPaths{}, fmt.Errorf("write daemon-handshake-sock control file: %w", err)
		}
	} else {
		// No socket wired (non-owner-wrapped build / Phase-2-only render).
		// Remove a stale daemon-handshake-sock file from a previous owner
		// build: the host socket died with that build, so a daemon that
		// inherits -Domac.daemon.owner from gradle.properties but reads a
		// dead sock path would fail closed against it. Removing the file
		// restores the init script's designed no-op path (issue #206).
		if err := os.Remove(filepath.Join(ctrlDir, daemonHandshakeSockName)); err != nil && !os.IsNotExist(err) {
			return ControlPaths{}, fmt.Errorf("remove stale daemon-handshake-sock control file: %w", err)
		}
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
