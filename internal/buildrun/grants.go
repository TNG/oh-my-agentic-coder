package buildrun

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"

	"github.com/tngtech/oh-my-agentic-coder/internal/sandboxprofile"
	"github.com/tngtech/oh-my-agentic-coder/internal/sandboxrun"
)

// BuildGrants is the executor grant set: sandboxrun.Grants plus the
// build-specific derived paths (GRADLE_USER_HOME leaf, private temp) and
// the resolved JDK / proxy posture.
type BuildGrants struct {
	*sandboxrun.Grants
	gradleUserHome string
	tmpDir         string
	// jdk is the resolved real JDK (shims bypassed) used to rewrite
	// JAVA_HOME/PATH in ChildEnv. Zero value when no JDK was resolved
	// (then the parent env passes through unchanged as a fallback).
	jdk JDKResolution
	// proxyURL is the omac filtered proxy URL the Gradle daemon is pointed
	// at via GRADLE_OPTS / gradle.properties. Empty when no proxy is in
	// use (Linux kernel-blocked build path).
	proxyURL string
	// gradleOpts is the GRADLE_OPTS value (proxy system properties) injected
	// into ChildEnv. Empty when no proxy. NEVER uses JAVA_TOOL_OPTIONS —
	// the JVM prints that env var on every launch, leaking any proxy token.
	gradleOpts string
	// maxHeap is the Gradle daemon JVM -Xmx ceiling written into the
	// OMAC-generated gradle.properties. Empty omits the line.
	maxHeap string
	// approvedImages / approvedRegistries carry the frozen-for-session
	// manifest-approved capability set. Tickets 08/09 (containers) and 06
	// (credential lift) consume them; ticket 05 only threads them through.
	approvedImages     []string
	approvedRegistries []string
	// registryProxyURLs maps each approved private registry alias to the
	// non-secret local loopback URL Gradle is pointed at via the OMAC-
	// authored init.d script (ticket 06). The credential NEVER appears
	// here — the URL is http://127.0.0.1:<port>/<alias>/. Empty when no
	// private registries are approved (the common case) or on Linux.
	registryProxyURLs map[string]string
	// containerProxyURL is the mediated Docker endpoint the executor is
	// pointed at via DOCKER_HOST (ticket 08). Empty when no container
	// proxy is in use (no approved images, or Linux kernel-blocked).
	// The URL carries NO userinfo — the proxy authenticates by ownership
	// (omac.executor=<id> label), not token.
	containerProxyURL string
	// containerProxyEnabled is true when the container proxy was started
	// (macOS with approved images). ChildEnv injects DOCKER_HOST +
	// TESTCONTAINERS_RYUK_DISABLED=true only when this is true.
	containerProxyEnabled bool
}

// GradleUserHome is the OMAC cache leaf handed to the Gradle wrapper as
// GRADLE_USER_HOME.
//
// Accessor nil-guard policy: EVERY BuildGrants accessor returns the zero
// value on a nil receiver instead of panicking — the policy is uniform so
// callers never need to memorize which accessors guard
// (TestBuildGrants_NilReceiverAccessors pins this). The embedded
// *sandboxrun.Grants is NOT guarded: touching it on nil is a caller bug,
// like any other nil struct deref.
func (b *BuildGrants) GradleUserHome() string {
	if b == nil {
		return ""
	}
	return b.gradleUserHome
}

// TmpDir is the executor's private temporary directory (exported as TMPDIR).
func (b *BuildGrants) TmpDir() string {
	if b == nil {
		return ""
	}
	return b.tmpDir
}

// JDK returns the resolved real JDK (shims bypassed). The zero value's
// empty JavaHome means resolution failed; the parent env then passes
// through unchanged as a best-effort fallback.
func (b *BuildGrants) JDK() JDKResolution {
	if b == nil {
		return JDKResolution{}
	}
	return b.jdk
}

// JDKExecutable returns the EvalSymlinks-resolved path of the resolved
// JDK's `java` binary — the executable procidentity.Verify compares the
// daemon process's resolved executable against (ticket 07, spec.md
// §238). Returns "" when no JDK was resolved (the engine treats this as
// a service failure: the daemon cannot be verified without a resolved
// JDK executable, so the ownership handshake fails closed).
//
// The path is resolved via filepath.EvalSymlinks so it matches the
// kernel-resolved path /proc/<pid>/exe (Linux) or proc_pidpath (macOS)
// reports for a process running that JDK — a symlinked JAVA_HOME would
// otherwise make the executable compare false-negative.
func (b *BuildGrants) JDKExecutable() string {
	if b == nil {
		return ""
	}
	return jdkExecutableFromResolution(b.jdk)
}

// jdkExecutableFromResolution computes the same value
// BuildGrants.JDKExecutable returns, from a raw JDKResolution. The engine
// pre-resolves the JDK executable for the ownership prepare step BEFORE
// GrantsFor (the pending DaemonRecord requires it eagerly); extracting
// the computation keeps the two call sites in lock-step so the pending
// record's jdk_executable always equals what grants.JDKExecutable()
// would later return for the verify closure.
func jdkExecutableFromResolution(jdk JDKResolution) string {
	if jdk.BinDir == "" {
		return ""
	}
	p := filepath.Join(jdk.BinDir, "java")
	if canon, err := filepath.EvalSymlinks(p); err == nil {
		return canon
	}
	return p
}

// ResolveJDKExecutable resolves the real JDK from the parent environment
// (the same ResolveJDK GrantsFor uses) and returns its `java` binary path
// — the value the ownership prepare step needs eagerly (the pending
// DaemonRecord requires a non-empty jdk_executable; GrantsFor computes
// the identical value via BuildGrants.JDKExecutable from the same
// ResolveJDK with the same env, so both sides always agree). An error
// means no real JDK is discoverable; the caller surfaces it (a build
// cannot run without a JDK, so GrantsFor would fail with the same
// resolution error).
func ResolveJDKExecutable(getenv func(string) string) (string, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	jdk, err := ResolveJDK(getenv)
	if err != nil {
		return "", err
	}
	return jdkExecutableFromResolution(jdk), nil
}

// ProxyURL returns the omac filtered proxy URL the Gradle daemon is routed
// through, or "" when no proxy is in use.
func (b *BuildGrants) ProxyURL() string {
	if b == nil {
		return ""
	}
	return b.proxyURL
}

// GradleOpts returns the GRADLE_OPTS value injected into ChildEnv, or "".
func (b *BuildGrants) GradleOpts() string {
	if b == nil {
		return ""
	}
	return b.gradleOpts
}

// ApprovedImages returns the manifest-approved container image references
// (frozen-for-session capability set). Tickets 08/09 enforce these at the
// mediated-container proxy; ticket 05 only carries them through.
func (b *BuildGrants) ApprovedImages() []string {
	if b == nil {
		return nil
	}
	return b.approvedImages
}

// ApprovedRegistries returns the manifest-approved registry aliases.
// Ticket 06 wires the credential lift; ticket 05 only carries them through.
func (b *BuildGrants) ApprovedRegistries() []string {
	if b == nil {
		return nil
	}
	return b.approvedRegistries
}

// RegistryProxyURLs returns the non-secret local loopback URL per
// approved private registry alias (ticket 06). The URL Gradle is pointed
// at via the OMAC-authored init.d script — it carries NO credential.
// Empty map when no private registries are approved (common case) or on
// Linux (the credential proxy is macOS-only in v1).
func (b *BuildGrants) RegistryProxyURLs() map[string]string {
	if b == nil {
		return nil
	}
	return b.registryProxyURLs
}

// ContainerProxyURL returns the mediated Docker endpoint the executor is
// pointed at via DOCKER_HOST (ticket 08), or "" when no container proxy is
// in use. The URL carries NO userinfo — the proxy authenticates by
// ownership (omac.executor=<id> label), not token.
func (b *BuildGrants) ContainerProxyURL() string {
	if b == nil {
		return ""
	}
	return b.containerProxyURL
}

// ContainerProxyEnabled reports whether the container proxy was started
// (macOS with approved images). ChildEnv injects DOCKER_HOST +
// TESTCONTAINERS_RYUK_DISABLED=true only when this is true.
func (b *BuildGrants) ContainerProxyEnabled() bool {
	if b == nil {
		return false
	}
	return b.containerProxyEnabled
}

// GradleLeafName is the tool leaf below the resolved OMAC cache scope.
// The spec's Gradle State section fixes GRADLE_USER_HOME=$cache/gradle.
// Exported so the CLI wiring computes the same leaf GrantsFor uses without
// re-hardcoding the literal.
const GradleLeafName = "gradle"

// preLeafLocksDir holds omac's cross-run locks taken BEFORE the Gradle
// leaf itself is touched: Gradle wrapper downloads and (in later tickets)
// mediated-container staging must not race independent `omac build`
// invocations, but the locks belong to omac, not to Gradle, so they live
// beside the leaf under the cache scope rather than inside it.
const preLeafLocksDir = ".omac-pre-leaf-locks"

// defaultMaxHeap is the Gradle daemon JVM -Xmx ceiling OMAC imposes by
// default (written into the read-only gradle.properties). Bounded so a
// runaway build cannot balloon the daemon beyond a defensible host share;
// overridable via BuildConfig.MaxHeap.
const defaultMaxHeap = "2g"

// BuildConfig bundles the per-request build configuration GrantsFor
// consumes: proxy posture, resource ceilings, JDK discovery seam. A zero
// value yields a fully kernel-blocked, default-resources build (the Linux
// posture). The CLI populates ProxyURL/ProxyPort after starting the
// netproxy server.
type BuildConfig struct {
	// ProxyURL is the omac filtered proxy URL
	// (http://omac:<token>@127.0.0.1:<port> — the token ALWAYS rides in
	// the userinfo per netproxy.Server.ProxyURL). Empty disables proxy
	// injection (kernel-blocked posture). On macOS (Shape A) the build
	// path is env-only filtered so Gradle's daemon loopback works; the
	// token authenticates the wrapper's distribution download and all
	// dependency fetches against the omac proxy (Proxy-Authorization:
	// Basic). Ticket 06 adds private-registry creds on top.
	ProxyURL string
	// ProxyPort is grants.ProxyPort (the port the SBPL allows loopback
	// egress to). 0 when no proxy.
	ProxyPort int
	// MaxHeap overrides the Gradle daemon -Xmx ceiling. Empty uses the
	// default (defaultMaxHeap).
	MaxHeap string
	// ApprovedImages is the manifest-approved container image reference
	// list (from the frozen-for-session capability set). Ticket 05 only
	// DECLARES these; tickets 08/09 enforce them at the mediated-container
	// proxy. GrantsFor does not act on them yet — they are stored on
	// BuildGrants for the container proxy to consume later.
	ApprovedImages []string
	// ApprovedRegistries is the manifest-approved registry alias list.
	// Ticket 06 wires the credential lift; ticket 05 only declares them.
	ApprovedRegistries []string
	// RegistryProxyURLs maps each approved private registry alias to the
	// non-secret local loopback URL the credential-lift proxy serves
	// (ticket 06). The URL carries NO credential; Gradle is pointed at it
	// via the OMAC-authored init.d script. Empty when no private
	// registries are approved (common case) or on Linux. GrantsFor
	// threads this into PrepareControlState so the init.d script is
	// generated; the credential itself NEVER enters BuildConfig.
	RegistryProxyURLs map[string]string
	// ContainerProxyURL is the mediated Docker endpoint the executor is
	// pointed at via DOCKER_HOST (ticket 08). Empty disables container
	// proxy injection (no approved images, or Linux kernel-blocked). The
	// URL carries NO userinfo — the proxy authenticates by ownership
	// (omac.executor=<id> label), not token. Set by the CLI after starting
	// the container proxy.
	ContainerProxyURL string
	// ContainerProxyEnabled is true when the container proxy was started
	// (macOS with approved images). ChildEnv injects DOCKER_HOST +
	// TESTCONTAINERS_RYUK_DISABLED=true only when this is true.
	ContainerProxyEnabled bool
	// DaemonOwnerMarker is the cryptographically random, unguessable
	// owner marker the host injects into the Gradle daemon JVM args
	// (ticket 07, spec.md §237). When non-empty, GrantsFor threads it
	// into GradlePropertiesConfig.DaemonOwnerMarker so
	// PrepareControlState renders -Domac.daemon.owner=<marker> into
	// org.gradle.jvmargs; the daemon-owner-handshake init script reads
	// it back and echoes it over the executor supervisor's private
	// control channel. Empty omits the property (the legacy behavior —
	// a non-omac build or a Phase-3 path that is not wiring ownership).
	// The engine (Phase 3) mints this via NewDaemonOwnerMarker BEFORE
	// calling GrantsFor and writes the pending DaemonRecord first.
	DaemonOwnerMarker DaemonOwnerMarker
	// DaemonHandshakeSock is the path of the executor supervisor's
	// private Unix socket the Gradle daemon writes its handshake to
	// (ticket 07, spec.md §237). When non-empty, GrantsFor threads it
	// into GradlePropertiesConfig.DaemonHandshakeSock so
	// PrepareControlState writes the daemon-handshake-sock control-state
	// file the init script reads at daemon startup. Empty omits the
	// file (the init script falls back to its no-op path). The engine
	// (Phase 3) derives this via DaemonHandshakeSockPath(RequestDir).
	DaemonHandshakeSock string
	// getenv is the JDK discovery seam; production passes os.Getenv, tests
	// inject a fake parent env. nil selects os.Getenv.
	getenv func(string) string
}

// SetGetenv sets the JDK discovery env seam (the engine threads its
// Options.Getenv through so GrantsFor's ResolveJDK uses the same env
// the ownership prepare's eager JDK-executable pre-resolution used —
// both read ResolveJDK(getenv), so the pending DaemonRecord's
// jdk_executable always equals grants.JDKExecutable()). A nil argument
// selects os.Getenv.
func (c *BuildConfig) SetGetenv(f func(string) string) {
	c.getenv = f
}

// envPassThrough is the fixed, harness-independent allowlist for the
// executor's environment. Nothing harness/host-specific may pass: no
// OMAC_* facade/sidecar vars, no cloud/SSH/git credentials, no HOME
// (which would expose host gradle.properties and init scripts under
// ~/.gradle). Locale vars are required so the wrapper/JDK print
// consistently; PATH and JAVA_HOME are rewritten by ResolveJDK (the
// verbatim parent values are NEVER passed — shims break under Seatbelt).
var envPassThrough = []string{
	// NOTE: PATH and JAVA_HOME are intentionally absent here — they are
	// resolved via ResolveJDK and injected from the JDKResolution, never
	// copied verbatim from the parent env (jenv shims break under
	// deny-default Seatbelt; see jdk.go).
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
//   - $cache/gradle (read+write)       — GRADLE_USER_HOME (the ONLY cache
//     path granted writable; the leaf is ensured on disk first since
//     sandboxrun existence-filters profile paths). OMAC-generated control
//     state inside the leaf (gradle.properties, .omac-control/) is granted
//     READ-ONLY via WriteDenyPaths.
//   - $cache/gradle/.omac-pre-leaf-locks — omac-owned lock staging area
//   - private temp (read+write)       — per-run TMPDIR
//   - real JDK bin + lib (read-only)  — resolved via ResolveJDK, so the
//     executor execs the real java, never a jenv/asdf shim
//   - platform read baseline (read-only) — sandboxprofile.PlatformBaseline
//     Read paths (/bin, /usr/bin, /usr/lib, /private/var/select, /etc,
//     /System, /Library, ... on macOS), the SAME baseline ResolveGrants
//     merges for `omac sandbox run`. Without these the executor under
//     deny-default Seatbelt cannot exec /bin/sh, read /usr/bin/uname, or
//     resolve /private/var/select/sh. Read-only; the WRITE set stays
//     minimal (worktree+leaf+temp only — the baseline's broad /tmp /
//     /var/folders write grants are deliberately NOT added).
//
// The platform baseline ProtectedPaths (~/.ssh, ~/.gradle, cloud creds,
// keychains) are merged into Grants.ProtectedPaths so the executor cannot
// read host secrets even though system dirs are now read-granted.
//
// The cache SCOPE dir itself is deliberately NOT granted: sibling tool
// caches laid down by `omac start`/`serve` (go, npm, pip leaves) must
// stay unwritable by the build executor.
//
// Network posture (Shape A):
//   - macOS: filtered + env-only. The Gradle daemon talks to its workers
//     over a random loopback port, which a kernel network boundary blocks;
//     env-only lets that loopback work while the omac proxy still filters
//     external egress. The proxy URL is injected via GRADLE_OPTS (NEVER
//     JAVA_TOOL_OPTIONS — the JVM prints that env var, leaking tokens).
//   - Linux: blocked + kernel. Per-request private-loopback namespace
//     keeps the executor network-isolated; Linux network validation is
//     deferred (v1 starts the loopback proxies on macOS only).
//
// cacheDir must already be the resolved OMAC cache scope dir (from
// internal/toolcache via the cli wiring); GrantsFor never invents paths.
func GrantsFor(worktree, cacheDir string, cfg BuildConfig) (*BuildGrants, error) {
	// The worktree must exist (Resolve already validated it; defensive).
	if _, err := os.Stat(worktree); err != nil {
		return nil, fmt.Errorf("worktree: %w", err)
	}
	if cacheDir == "" {
		return nil, fmt.Errorf("empty cache dir: GRADLE_USER_HOME must come from the resolved OMAC cache scope")
	}

	getenv := cfg.getenv
	if getenv == nil {
		getenv = os.Getenv
	}

	leaf := filepath.Join(cacheDir, GradleLeafName)
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

	// Resolve the real JDK (bypass jenv/asdf shims) BEFORE building the
	// grant set: the resolved bin/lib dirs must be read-granted so the
	// executor can exec and load the JVM under deny-default Seatbelt.
	jdk, jdkErr := ResolveJDK(getenv)

	// Enumerate ALL host JDK installations (unsandboxed) so Gradle's
	// toolchain auto-detection can match a pinned toolchain spec against
	// an installed JDK. /usr/libexec/java_home fails inside the sandbox
	// (LaunchServices/Spotlight enumeration broken), so the supervisor
	// enumerates here and declares the roots via
	// org.gradle.java.installations.paths (read-only control state).
	// Each JDK's bin+lib must also be read-granted so the executor can
	// exec javac from a matched toolchain.
	var installationsPaths []string
	var toolchainReadPaths []string
	if jdkErr == nil {
		installationsPaths = EnumerateHostJDKs(jdk.JavaHome)
		for _, home := range installationsPaths {
			toolchainReadPaths = append(toolchainReadPaths, jdkReadPaths(home)...)
		}
	}

	// OMAC control state: gradle.properties (proxy + jvmargs), the
	// .omac-control/ README, AND the init.d/ control directory (Gradle
	// loads init.d/*.gradle as init scripts — it must be read-only to the
	// executor so build code cannot plant an init script). All written
	// read-only to the executor.
	maxHeap := cfg.MaxHeap
	if maxHeap == "" {
		maxHeap = defaultMaxHeap
	}
	proxy := splitProxyEndpoint(cfg.ProxyURL)
	gradleProps := GradlePropertiesConfig{
		Proxy:               proxy,
		MaxHeap:             maxHeap,
		RegistryProxyURLs:   cfg.RegistryProxyURLs,
		InstallationsPaths:  installationsPaths,
		TmpDir:              tmp,
		DaemonOwnerMarker:   cfg.DaemonOwnerMarker,
		DaemonHandshakeSock: cfg.DaemonHandshakeSock,
	}
	controlPaths, err := PrepareControlState(leaf, gradleProps)
	if err != nil {
		return nil, fmt.Errorf("prepare control state: %w", err)
	}

	// Network posture: macOS env-only filtered (Shape A) so Gradle's
	// daemon loopback works; Linux kernel-blocked. Use the sandboxprofile
	// constants, not raw strings.
	networkMode := sandboxprofile.ModeBlocked
	enforcement := sandboxprofile.EnforceKernel
	if runtime.GOOS == "darwin" {
		networkMode = sandboxprofile.ModeFiltered
		enforcement = sandboxprofile.EnforceEnvOnly
	}

	readPaths := []string{}
	readPaths = append(readPaths, controlPaths.All()...)
	if jdkErr == nil {
		readPaths = append(readPaths, jdk.ReadPaths...)
		// Toolchain JDKs: each enumerated host JDK needs bin+lib
		// read-granted so the executor can exec javac from a matched
		// toolchain. (The daemon JDK's paths are already in jdk.ReadPaths;
		// toolchainReadPaths adds the OTHERS — deduped below.)
		readPaths = append(readPaths, toolchainReadPaths...)
	}
	// Platform read baseline (darwinBaseline().Read on macOS: /bin,
	// /usr/bin, /usr/lib, /private/var/select, /etc, /System, /Library,
	// ...). The build path constructs sandboxrun.Grants directly and
	// MUST merge the same baseline ResolveGrants applies to `omac sandbox
	// run`, otherwise under deny-default Seatbelt the executor cannot
	// exec /bin/sh, read /usr/bin/uname, or resolve /private/var/select/sh
	// (the sh symlink) — exactly the ticket-04 host failure
	// ("uname: command not found", "Error opening /private/var/select/sh").
	// Read-only grants; the WRITE set stays minimal (worktree+leaf+temp).
	// ExpandExisting drops absent paths and ~/$VAR entries that don't
	// resolve (e.g. $TMPDIR on Linux) with a notice.
	baseline := sandboxprofile.PlatformBaseline()
	baselineRead, err := sandboxprofile.ExpandExisting(baseline.Read, nil)
	if err != nil {
		return nil, fmt.Errorf("expand platform read baseline: %w", err)
	}
	readPaths = append(readPaths, baselineRead...)

	// Protected paths: the baseline protected set (~/.ssh, ~/.gradle via
	// the home-tree entries, cloud creds, keychains) is normally applied
	// by ResolveGrants; the build path bypasses that, so replicate it
	// here. EffectiveProtectedPaths expands ~ and drops override_deny
	// holes (none in the build path — there is no profile). These are
	// denied even under broader grants, so ~/.gradle/~/.ssh stay
	// unreadable even though /usr/local/lib etc. are now read-granted.
	protected := sandboxprofile.EffectiveProtectedPaths(baseline, nil)

	g := &sandboxrun.Grants{
		Workdir:        worktree,
		AllowPaths:     dedupePaths([]string{worktree, leaf, locksDir, tmp}),
		ReadPaths:      dedupePaths(readPaths),
		ProtectedPaths: dedupePaths(protected),
		WriteDenyPaths: dedupePaths(controlPaths.All()), // read-only control state (files + init.d)
		NetworkMode:    networkMode,
		Enforcement:    enforcement,
		ProxyPort:      cfg.ProxyPort,
	}
	if err := g.Validate(); err != nil {
		return nil, err
	}

	bg := &BuildGrants{
		Grants:                g,
		gradleUserHome:        leaf,
		tmpDir:                tmp,
		jdk:                   jdk,
		proxyURL:              cfg.ProxyURL,
		maxHeap:               maxHeap,
		approvedImages:        cfg.ApprovedImages,
		approvedRegistries:    cfg.ApprovedRegistries,
		registryProxyURLs:     cfg.RegistryProxyURLs,
		containerProxyURL:     cfg.ContainerProxyURL,
		containerProxyEnabled: cfg.ContainerProxyEnabled,
	}
	if proxy.Host != "" && proxy.Port > 0 {
		bg.gradleOpts = buildGradleOpts(proxy)
	}
	return bg, nil
}

// splitProxyEndpoint extracts host, port, and userinfo from a proxy URL of
// the form http://[user:pass@]host:port (scheme, userinfo, and IPv6
// brackets all handled by net/url.Parse, which the previous hand-rolled
// splitter did not fully cover). The userinfo carries the omac proxy
// token (http://omac:<token>@127.0.0.1:<port> per
// netproxy.Server.ProxyURL); WITHOUT it Gradle connects to the proxy but
// cannot authenticate, yielding HTTP 407 Proxy Authentication Required.
// Returns the zero value when the URL is empty, unparseable, or missing a
// positive port. Used to populate gradle.properties system properties and
// the GRADLE_OPTS proxyUser/proxyPassword system properties.
func splitProxyEndpoint(proxyURL string) ProxyEndpoint {
	if proxyURL == "" {
		return ProxyEndpoint{}
	}
	u, err := url.Parse(proxyURL)
	if err != nil {
		return ProxyEndpoint{}
	}
	host := u.Hostname()
	if host == "" {
		return ProxyEndpoint{}
	}
	portStr := u.Port()
	if portStr == "" {
		return ProxyEndpoint{}
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 {
		return ProxyEndpoint{}
	}
	ep := ProxyEndpoint{Host: host, Port: port}
	// The omac proxy ALWAYS carries a token in the userinfo
	// (netproxy.Server.ProxyURL → http://omac:<token>@127.0.0.1:<port>).
	// An empty userinfo is accepted (public/no-auth proxy); ticket-06's
	// private-registry credential proxy is the only path that omits it.
	if ui := u.User; ui != nil {
		ep.User = ui.Username()
		if pass, ok := ui.Password(); ok {
			ep.Password = pass
		}
	}
	return ep
}

// ProxyEndpoint is a resolved proxy host:port pair plus the optional
// userinfo the omac proxy validates via Proxy-Authorization (Basic
// user:token; see netproxy.Server.authorized). It replaces the
// `proxyHost string, proxyPort int` data clump threaded through
// buildGradleOpts and GradlePropertiesConfig. The zero value (empty Host,
// 0 Port) means "no proxy".
//
// The token rides in User/Password and is emitted into GRADLE_OPTS
// (https.proxyUser/proxyPassword), NEVER gradle.properties — that file is
// read-only to the executor but READABLE by build code and persists on
// disk in the cache leaf, so the token must not be written to it. GRADLE_OPTS
// is per-process env the JVM does not print (unlike JAVA_TOOL_OPTIONS).
type ProxyEndpoint struct {
	Host string
	Port int
	// User/Password carry the proxy userinfo (omac:<token>) Gradle's HTTP
	// client sends as Proxy-Authorization: Basic. Empty for a no-auth proxy.
	User     string
	Password string
}

// Valid reports whether the endpoint carries a usable host:port.
func (p ProxyEndpoint) Valid() bool { return p.Host != "" && p.Port > 0 }

// buildGradleOpts renders the GRADLE_OPTS value pointing the Gradle daemon
// (and the JVMs it spawns) at the omac filtered proxy via system
// properties. NEVER uses JAVA_TOOL_OPTIONS — the JVM prints that env var
// on every launch, leaking any proxy token to stderr (spec.md:180).
// Loopback is excluded so the daemon's worker protocol is not proxied.
//
// The proxy token rides in https.proxyUser/https.proxyPassword (and the
// http.* twins for completeness). Gradle's HTTP client sends these as
// Proxy-Authorization: Basic user:token, exactly what
// netproxy.Server.authorized validates (internal/netproxy/server.go:276).
// Without them the wrapper downloads the distribution through the proxy
// but gets HTTP 407 Proxy Authentication Required (ticket-04 host
// failure). The token is NOT written to gradle.properties — that file is
// readable by build code and persists on disk in the cache leaf, so the
// token must stay in per-process GRADLE_OPTS (which the JVM does not
// print).
func buildGradleOpts(p ProxyEndpoint) string {
	// The omac proxy ALWAYS carries a token (netproxy.Server.ProxyURL) —
	// it rides in p.User/p.Password and is emitted as the http(s).proxyUser/
	// proxyPassword properties so the wrapper's distribution download (HTTPS
	// CONNECT to services.gradle.org) AND plain-HTTP fetches authenticate.
	// The shared renderer keeps the JVM property strings identical to the
	// JAVA_TOOL_OPTIONS channel (sandboxrun.JVMProxyToolOptions).
	return sandboxrun.JVMProxySystemProperties(p.Host, p.Port, p.User, p.Password)
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
//
// PATH and JAVA_HOME come from the resolved real JDK (ResolveJDK), never
// from the parent env verbatim — jenv/asdf shims break under deny-default
// Seatbelt. When JDK resolution failed, PATH/JAVA_HOME fall back to the
// parent env verbatim (best-effort; the build will likely fail to exec
// java, which is the honest outcome).
func ChildEnv(b *BuildGrants) []string {
	injected := map[string]string{
		"GRADLE_USER_HOME": b.gradleUserHome,
		"TMPDIR":           b.tmpDir,
	}
	// JDK: rewrite PATH/JAVA_HOME to the real JDK, bypassing shims.
	if b.jdk.JavaHome != "" {
		injected["JAVA_HOME"] = b.jdk.JavaHome
		injected["PATH"] = b.jdk.Path
	} else {
		// Fallback: pass the parent env verbatim (best-effort; the build
		// will likely fail to exec java under Seatbelt — the honest
		// outcome of an unresolvable JDK).
		if v := os.Getenv("PATH"); v != "" {
			injected["PATH"] = v
		}
		if v := os.Getenv("JAVA_HOME"); v != "" {
			injected["JAVA_HOME"] = v
		}
	}
	// Proxy: GRADLE_OPTS points the Gradle daemon at the omac proxy,
	// carrying the proxy token in https.proxyUser/proxyPassword system
	// properties (the JVM does NOT print GRADLE_OPTS, so the token is
	// safe). NEVER JAVA_TOOL_OPTIONS — the JVM prints it on every launch,
	// leaking the token (spec.md:180). NO_PROXY keeps the daemon's
	// loopback worker protocol off the proxy.
	if b.gradleOpts != "" {
		injected["GRADLE_OPTS"] = b.gradleOpts
		injected["NO_PROXY"] = "localhost,127.0.0.1,::1"
	}
	// Container proxy (ticket 08): point the executor at the mediated
	// Docker endpoint via DOCKER_HOST (NEVER the raw daemon socket). The
	// URL carries NO userinfo — the proxy authenticates by ownership.
	// TESTCONTAINERS_RYUK_DISABLED=true disables the Ryuk reaper (ADR 0002
	// v1 posture); the filter also rejects Ryuk fail-closed (a client
	// could unset the env). Injected only when the container proxy is
	// enabled (macOS with approved images).
	if b.containerProxyEnabled && b.containerProxyURL != "" {
		injected["DOCKER_HOST"] = b.containerProxyURL
		injected["TESTCONTAINERS_RYUK_DISABLED"] = "true"
		tracef(os.Stderr, "ChildEnv: injected DOCKER_HOST=%s TESTCONTAINERS_RYUK_DISABLED=true (containerProxyEnabled=%v)",
			b.containerProxyURL, b.containerProxyEnabled)
	} else {
		tracef(os.Stderr, "ChildEnv: DOCKER_HOST NOT injected (containerProxyEnabled=%v containerProxyURL=%q)",
			b.containerProxyEnabled, b.containerProxyURL)
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
