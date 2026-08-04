package buildrun

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/tngtech/oh-my-agentic-coder/internal/sandboxprofile"
	"github.com/tngtech/oh-my-agentic-coder/internal/sandboxrun"
)

// TestBuildGrants_NilReceiverAccessors asserts every BuildGrants accessor
// is nil-receiver safe (returns the zero value instead of panicking). The
// nil-guard policy is uniform across accessors: any accessor that panicked
// on a nil receiver would be an inconsistency (review finding).
func TestBuildGrants_NilReceiverAccessors(t *testing.T) {
	var b *BuildGrants // nil
	fail := func(name string, fn func()) {
		t.Helper()
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("%s panicked on nil receiver: %v", name, r)
			}
		}()
		fn()
	}
	fail("GradleUserHome", func() { _ = b.GradleUserHome() })
	fail("TmpDir", func() { _ = b.TmpDir() })
	fail("JDK", func() { _ = b.JDK() })
	fail("ProxyURL", func() { _ = b.ProxyURL() })
	fail("GradleOpts", func() { _ = b.GradleOpts() })
	fail("ApprovedImages", func() { _ = b.ApprovedImages() })
	fail("ApprovedRegistries", func() { _ = b.ApprovedRegistries() })
	fail("RegistryProxyURLs", func() { _ = b.RegistryProxyURLs() })
	fail("ContainerProxyURL", func() { _ = b.ContainerProxyURL() })
	fail("ContainerProxyEnabled", func() { _ = b.ContainerProxyEnabled() })
}

func TestGrantsFor(t *testing.T) {
	wt := t.TempDir()
	canonical, err := filepath.EvalSymlinks(wt)
	if err != nil {
		t.Fatal(err)
	}
	backend := filepath.Join(wt, "backend")
	makeWrapper(t, backend)
	cacheDir := filepath.Join(t.TempDir(), "cache")

	g, err := GrantsFor(canonical, cacheDir, BuildConfig{})
	if err != nil {
		t.Fatalf("GrantsFor: %v", err)
	}
	chmodInitDForCleanup(t, filepath.Join(cacheDir, "gradle"))

	t.Run("grant set is worktree + cache leaf + private temp only", func(t *testing.T) {
		if !contains(g.AllowPaths, canonical) {
			t.Errorf("AllowPaths missing worktree %s: %v", canonical, g.AllowPaths)
		}
		// The cache SCOPE dir itself must not be rw: only the resolved
		// gradle leaf below it is granted. Sibling tool caches created by
		// `omac start`/`serve` (go/npm/pip leaves) would otherwise be
		// writable by the build executor (cache over-grant).
		if contains(g.AllowPaths, cacheDir) {
			t.Errorf("AllowPaths must not contain the cache scope dir %s: %v", cacheDir, g.AllowPaths)
		}
		wantLeaf := filepath.Join(cacheDir, "gradle")
		if !contains(g.AllowPaths, wantLeaf) {
			t.Errorf("AllowPaths missing GRADLE_USER_HOME leaf %s: %v", wantLeaf, g.AllowPaths)
		}
		// The bare cache pre-leaf lock dir must not widen the write
		// surface beyond the scoped cache dir itself.
		if !contains(g.AllowPaths, filepath.Join(cacheDir, "gradle", ".omac-pre-leaf-locks")) {
			t.Errorf(
				"AllowPaths missing pre-leaf lock dir: %v", g.AllowPaths)
		}
	})

	t.Run("sibling tool caches are not writable", func(t *testing.T) {
		// Simulate sibling tool leaves laid down by `omac start` (go,
		// npm, pip per the cache-isolation probes). Seatbelt/bwrap
		// subpath grants are prefix-based: granting the scope dir rw
		// would silently grant these too, so the scope dir must be
		// absent from every grant list entirely.
		if err := os.MkdirAll(filepath.Join(cacheDir, "go"), 0o755); err != nil {
			t.Fatal(err)
		}
		for _, sibling := range []string{"go", "npm", "pip"} {
			leaf := filepath.Join(cacheDir, sibling)
			if contains(g.AllowPaths, leaf) || contains(g.ReadPaths, leaf) || contains(g.WritePaths, leaf) {
				t.Errorf("sibling tool cache %s must not be granted: allow=%v read=%v write=%v",
					leaf, g.AllowPaths, g.ReadPaths, g.WritePaths)
			}
		}
		// The scope dir may appear only in the ancestor read rule that
		// makes descendants traversable at all (the same existence-path
		// leak the toolcache layout already relies on between scopes) —
		// never in a write rule. Subpath write rules carry the
		// "(require-not" canonicalization marker (see sbpl.go).
		sbpl := sandboxrun.GenerateSBPL(g.Grants)
		for _, line := range strings.Split(sbpl, "\n") {
			if strings.Contains(line, cacheDir) && strings.Contains(line, "require-not") {
				t.Errorf("SBPL must never make the cache scope dir writable:\n%s", line)
			}
		}
	})

	t.Run("network posture by platform (Shape A)", func(t *testing.T) {
		// macOS Shape A: env-only filtered so Gradle's daemon loopback
		// works; Linux: kernel-blocked (network isolation; proxies are
		// macOS-only in v1).
		switch runtime.GOOS {
		case "darwin":
			if g.NetworkMode != sandboxprofile.ModeFiltered {
				t.Errorf("NetworkMode = %q, want filtered (Shape A)", g.NetworkMode)
			}
			if g.Enforcement != sandboxprofile.EnforceEnvOnly {
				t.Errorf("Enforcement = %q, want env-only (Shape A)", g.Enforcement)
			}
		default:
			if g.NetworkMode != sandboxprofile.ModeBlocked {
				t.Errorf("NetworkMode = %q, want blocked", g.NetworkMode)
			}
			if g.Enforcement != sandboxprofile.EnforceKernel {
				t.Errorf("Enforcement = %q, want kernel", g.Enforcement)
			}
		}
	})

	t.Run("host home is not granted", func(t *testing.T) {
		home, err := os.UserHomeDir()
		if err != nil {
			t.Skip("no home")
		}
		if contains(g.AllowPaths, home) || contains(g.ReadPaths, home) || contains(g.WritePaths, home) {
			t.Errorf("home dir must never be granted")
		}
		hostGradle := filepath.Join(home, ".gradle")
		if contains(g.AllowPaths, hostGradle) || contains(g.ReadPaths, hostGradle) {
			t.Errorf("host ~/.gradle must never be granted: %v", g.AllowPaths)
		}
	})

	t.Run("platform read baseline is granted (darwin /usr/bin, /bin)", func(t *testing.T) {
		// Bug 2: the build path must merge sandboxprofile.PlatformBaseline
		// Read paths, otherwise deny-default Seatbelt denies /usr/bin/uname
		// and /private/var/select/sh (ticket-04 host failure). At least
		// one of /usr/bin or /bin must appear in ReadPaths on every
		// platform (both baselines list them).
		if !contains(g.ReadPaths, "/usr/bin") && !contains(g.ReadPaths, "/bin") {
			t.Errorf("ReadPaths must include a system bin dir for uname/sh: %v", g.ReadPaths)
		}
		if runtime.GOOS == "darwin" {
			// /private/var/select is the sh symlink root on macOS;
			// granting it is the specific ticket-04 fix.
			if !contains(g.ReadPaths, "/private/var/select") {
				t.Errorf("darwin ReadPaths must include /private/var/select (sh symlink): %v", g.ReadPaths)
			}
		}
	})

	t.Run("baseline protected paths remain denied", func(t *testing.T) {
		// Bug 2: merging the read baseline must NOT drop the protected
		// paths — ~/.ssh (in the baseline ProtectedPaths) stays denied
		// even though /usr/lib etc. are now read-granted. ~/.gradle is
		// NOT in the baseline ProtectedPaths; it is protected by HOME
		// being absent from the env pass-through and from ReadPaths
		// (covered by the "host home is not granted" subtest).
		home, err := os.UserHomeDir()
		if err != nil {
			t.Skip("no home")
		}
		sshDir := filepath.Join(home, ".ssh")
		if !contains(g.ProtectedPaths, sshDir) {
			t.Errorf("ProtectedPaths missing %s (must stay denied under read baseline): %v", sshDir, g.ProtectedPaths)
		}
	})

	t.Run("environment redirects gradle into cache leaf", func(t *testing.T) {
		env := ChildEnv(g)
		m := map[string]string{}
		for _, kv := range env {
			if i := strings.IndexByte(kv, '='); i > 0 {
				m[kv[:i]] = kv[i+1:]
			}
		}
		if got := m["GRADLE_USER_HOME"]; got != filepath.Join(cacheDir, "gradle") {
			t.Errorf("GRADLE_USER_HOME = %q, want %s", got, filepath.Join(cacheDir, "gradle"))
		}
		if m["HOME"] != "" {
			t.Errorf("HOME must not pass through (host gradle init scripts): got %q", m["HOME"])
		}
		for _, leaked := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "GITHUB_TOKEN", "OMAC_SOCKET", "OMAC_BASE"} {
			if _, ok := m[leaked]; ok {
				t.Errorf("env must not contain %s", leaked)
			}
		}
		if m["PATH"] == "" {
			t.Error("PATH must be present (java discovery)")
		}
		if m["TMPDIR"] != g.TmpDir() {
			t.Errorf("TMPDIR = %q, want private temp %q", m["TMPDIR"], g.TmpDir())
		}
	})

	t.Run("control state is read-only to the executor", func(t *testing.T) {
		// OMAC-generated control files (gradle.properties, .omac-control/)
		// AND the init.d/ control directory (Gradle loads init.d/*.gradle
		// as init scripts — it must be read-only so build code cannot
		// plant an init script) must be in ReadPaths + WriteDenyPaths
		// (readable, NOT writable), so build/test code cannot rewrite
		// the OMAC-imposed proxy/JVM guardrails.
		props := filepath.Join(cacheDir, "gradle", "gradle.properties")
		ctrlReadme := filepath.Join(cacheDir, "gradle", controlStateName, "README")
		initD := filepath.Join(cacheDir, "gradle", "init.d")
		for _, p := range []string{props, ctrlReadme, initD} {
			// Resolve to match the canonical form the grants carry.
			if canon, err := filepath.EvalSymlinks(p); err == nil {
				p = canon
			}
			if !contains(g.ReadPaths, p) {
				t.Errorf("control path %s must be in ReadPaths (readable): %v", p, g.ReadPaths)
			}
			if !contains(g.WriteDenyPaths, p) {
				t.Errorf("control path %s must be in WriteDenyPaths (not writable): %v", p, g.WriteDenyPaths)
			}
			if contains(g.AllowPaths, p) {
				t.Errorf("control path %s must NOT be in AllowPaths (would make it writable)", p)
			}
		}
		// The SBPL must emit a write-deny for the control files AFTER the
		// write-allows so a broader leaf write-grant cannot override it.
		sbpl := sandboxrun.GenerateSBPL(g.Grants)
		if !strings.Contains(sbpl, "(deny file-write*") {
			t.Errorf("SBPL must contain write-deny rules for control state")
		}
		// The control files must appear in a write-deny rule.
		if !strings.Contains(sbpl, props) && !strings.Contains(sbpl, filepath.ToSlash(props)) {
			t.Errorf("SBPL must deny writes to gradle.properties")
		}
		// init.d must appear in a write-deny rule too (S1).
		if !strings.Contains(sbpl, initD) && !strings.Contains(sbpl, filepath.ToSlash(initD)) {
			t.Errorf("SBPL must deny writes to init.d (S1: spec.md:164)")
		}
	})

	t.Run("sbpl denies host secret fixture", func(t *testing.T) {
		// The unit-level kernel-proof: a host secret path outside the
		// grant set must not appear in any allow rule, so (deny default)
		// covers it.
		secret := filepath.Join(t.TempDir(), "host-secret")
		if err := os.WriteFile(secret, []byte("s3cr3t"), 0o600); err != nil {
			t.Fatal(err)
		}
		sbpl := sandboxrun.GenerateSBPL(g.Grants)
		if strings.Contains(sbpl, secret) {
			t.Errorf("SBPL must not reference ungranted secret path %s", secret)
		}
		if !strings.Contains(sbpl, "(deny default)") {
			t.Error("SBPL must start from (deny default)")
		}
		// Sanity: the granted gradle leaf and worktree DO appear.
		if !strings.Contains(sbpl, canonical) {
			t.Errorf("SBPL must grant the worktree %s", canonical)
		}
	})
}

func TestGrantsForMissingWorktree(t *testing.T) {
	cacheDir := filepath.Join(t.TempDir(), "cache")
	if _, err := GrantsFor(filepath.Join(t.TempDir(), "nope"), cacheDir, BuildConfig{}); err == nil {
		t.Fatal("expected error for missing worktree")
	}
}

func TestGrantsForPreparesGradleLeaf(t *testing.T) {
	wt, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cacheDir := filepath.Join(t.TempDir(), "cache")
	g, err := GrantsFor(wt, cacheDir, BuildConfig{})
	if err != nil {
		t.Fatalf("GrantsFor: %v", err)
	}
	chmodInitDForCleanup(t, filepath.Join(cacheDir, "gradle"))
	leaf := filepath.Join(cacheDir, "gradle")
	fi, err := os.Stat(leaf)
	if err != nil {
		t.Fatalf("gradle leaf not prepared: %v", err)
	}
	if !fi.IsDir() {
		t.Errorf("gradle leaf is not a dir")
	}
	if got := fi.Mode().Perm(); got != 0o700 {
		t.Errorf("gradle leaf perms = %o, want 700", got)
	}
	if g.GradleUserHome() != leaf {
		t.Errorf("GradleUserHome = %q, want %q", g.GradleUserHome(), leaf)
	}
	if g.TmpDir() == "" || g.TmpDir() == os.TempDir() {
		t.Errorf("TmpDir must be a private dir, got %q", g.TmpDir())
	}
}

func TestGrantsForNeverDeletesInsideCache(t *testing.T) {
	// GrantsFor performs no lock hygiene: a stale-looking daemon lock
	// must survive GrantsFor untouched (post-build daemon recycling owns
	// cleanup, not GrantsFor).
	wt, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cacheDir := filepath.Join(t.TempDir(), "cache")
	lock := filepath.Join(cacheDir, "gradle", ".gradle", "daemon", "8.5", "registry.bin.lock")
	if err := os.MkdirAll(filepath.Dir(lock), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lock, []byte("lock"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := GrantsFor(wt, cacheDir, BuildConfig{}); err != nil {
		t.Fatalf("GrantsFor: %v", err)
	}
	chmodInitDForCleanup(t, filepath.Join(cacheDir, "gradle"))
	if _, err := os.Stat(lock); err != nil {
		t.Errorf("daemon lock must not be pruned by GrantsFor: %v", err)
	}
}

// TestGrantsForProxyEnv asserts the proxy is injected via GRADLE_OPTS and
// NEVER via JAVA_TOOL_OPTIONS (the JVM prints JAVA_TOOL_OPTIONS on every
// launch, leaking any proxy token — spec.md:180 forbids it).
func TestGrantsForProxyEnv(t *testing.T) {
	wt, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cacheDir := filepath.Join(t.TempDir(), "cache")
	g, err := GrantsFor(wt, cacheDir, BuildConfig{
		ProxyURL:  "http://omac:secret-token@127.0.0.1:9999",
		ProxyPort: 9999,
	})
	if err != nil {
		t.Fatalf("GrantsFor: %v", err)
	}
	chmodInitDForCleanup(t, filepath.Join(cacheDir, "gradle"))
	env := ChildEnv(g)
	m := map[string]string{}
	for _, kv := range env {
		if i := strings.IndexByte(kv, '='); i > 0 {
			m[kv[:i]] = kv[i+1:]
		}
	}
	if m["JAVA_TOOL_OPTIONS"] != "" {
		t.Errorf("JAVA_TOOL_OPTIONS must NEVER carry proxy config (JVM prints it, leaking tokens): %q", m["JAVA_TOOL_OPTIONS"])
	}
	opts := m["GRADLE_OPTS"]
	if opts == "" {
		t.Fatal("GRADLE_OPTS must be set when a proxy is configured")
	}
	for _, want := range []string{
		"-Dhttp.proxyHost=127.0.0.1",
		"-Dhttp.proxyPort=9999",
		"-Dhttps.proxyHost=127.0.0.1",
		"-Dhttps.proxyPort=9999",
		"-Dhttp.nonProxyHosts=localhost|127.*|[::1]",
		// The omac proxy ALWAYS carries a token; Gradle's HTTP client
		// sends these as Proxy-Authorization: Basic user:token, which
		// netproxy.Server.authorized validates. Without them the wrapper
		// download gets HTTP 407 (ticket-04 host failure).
		"-Dhttp.proxyUser=omac",
		"-Dhttps.proxyUser=omac",
		"-Dhttp.proxyPassword=secret-token",
		"-Dhttps.proxyPassword=secret-token",
		"-Djdk.http.auth.tunneling.disabledSchemes=",
	} {
		if !strings.Contains(opts, want) {
			t.Errorf("GRADLE_OPTS missing %q: %q", want, opts)
		}
	}
	// The proxy token rides ONLY in GRADLE_OPTS (the JVM does not print
	// GRADLE_OPTS). It must NOT appear in any OTHER env var — JAVA_TOOL_OPTIONS
	// in particular is printed by the JVM on every launch (spec.md:180).
	for k, v := range m {
		if k == "GRADLE_OPTS" {
			continue
		}
		if strings.Contains(v, "secret-token") {
			t.Errorf("proxy token leaked into env %s=%q (only GRADLE_OPTS may carry it)", k, v)
		}
	}
	// NO_PROXY keeps the daemon's loopback worker protocol off the proxy.
	if np := m["NO_PROXY"]; !strings.Contains(np, "localhost") || !strings.Contains(np, "127.0.0.1") {
		t.Errorf("NO_PROXY must exclude loopback: %q", np)
	}
}

// TestGrantsForNoProxyOmitsGradleOpts: with no proxy, GRADLE_OPTS is unset
// (the Linux kernel-blocked posture).
func TestGrantsForNoProxyOmitsGradleOpts(t *testing.T) {
	wt, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cacheDir := filepath.Join(t.TempDir(), "cache")
	g, err := GrantsFor(wt, cacheDir, BuildConfig{})
	if err != nil {
		t.Fatal(err)
	}
	chmodInitDForCleanup(t, filepath.Join(cacheDir, "gradle"))
	if g.GradleOpts() != "" {
		t.Errorf("GradleOpts must be empty with no proxy: %q", g.GradleOpts())
	}
}

// TestGrantsForContainerProxyEnv asserts ticket 08: DOCKER_HOST +
// TESTCONTAINERS_RYUK_DISABLED=true are injected into ChildEnv ONLY when
// the container proxy is enabled (macOS with approved images). The
// DOCKER_HOST URL carries NO userinfo — the proxy authenticates by
// ownership, not token.
func TestGrantsForContainerProxyEnv(t *testing.T) {
	wt, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cacheDir := filepath.Join(t.TempDir(), "cache")

	t.Run("enabled injects DOCKER_HOST and RYUK_DISABLED", func(t *testing.T) {
		g, err := GrantsFor(wt, cacheDir, BuildConfig{
			ContainerProxyURL:     "tcp://127.0.0.1:54321",
			ContainerProxyEnabled: true,
		})
		if err != nil {
			t.Fatalf("GrantsFor: %v", err)
		}
		chmodInitDForCleanup(t, filepath.Join(cacheDir, "gradle"))
		env := ChildEnv(g)
		m := childEnvMap(env)
		if m["DOCKER_HOST"] != "tcp://127.0.0.1:54321" {
			t.Errorf("DOCKER_HOST = %q, want tcp://127.0.0.1:54321", m["DOCKER_HOST"])
		}
		if m["TESTCONTAINERS_RYUK_DISABLED"] != "true" {
			t.Errorf("TESTCONTAINERS_RYUK_DISABLED = %q, want true", m["TESTCONTAINERS_RYUK_DISABLED"])
		}
		// The URL carries NO userinfo (no credential; ownership-based auth).
		if strings.Contains(m["DOCKER_HOST"], "@") {
			t.Errorf("DOCKER_HOST must not contain userinfo: %q", m["DOCKER_HOST"])
		}
	})

	t.Run("disabled omits DOCKER_HOST and RYUK_DISABLED", func(t *testing.T) {
		g, err := GrantsFor(wt, cacheDir, BuildConfig{})
		if err != nil {
			t.Fatalf("GrantsFor: %v", err)
		}
		chmodInitDForCleanup(t, filepath.Join(cacheDir, "gradle"))
		env := ChildEnv(g)
		m := childEnvMap(env)
		if _, ok := m["DOCKER_HOST"]; ok {
			t.Errorf("DOCKER_HOST must be absent when container proxy disabled: %q", m["DOCKER_HOST"])
		}
		if _, ok := m["TESTCONTAINERS_RYUK_DISABLED"]; ok {
			t.Errorf("TESTCONTAINERS_RYUK_DISABLED must be absent when container proxy disabled: %q", m["TESTCONTAINERS_RYUK_DISABLED"])
		}
	})
}

// childEnvMap parses a "key=value" child-env slice into a map. Named
// distinctly from jdk_test.go's envMap (a getenv-func builder).
func childEnvMap(env []string) map[string]string {
	m := map[string]string{}
	for _, kv := range env {
		if i := strings.IndexByte(kv, '='); i > 0 {
			m[kv[:i]] = kv[i+1:]
		}
	}
	return m
}

// TestGrantsForJDKResolution: the resolved JDK bin/lib are read-granted
// and the ChildEnv PATH/JAVA_HOME point at the real JDK, not shims.
func TestGrantsForJDKResolution(t *testing.T) {
	jdkHome := makeFakeJDK(t, filepath.Join(t.TempDir(), "real-jdk"))
	wt, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cacheDir := filepath.Join(t.TempDir(), "cache")
	g, err := GrantsFor(wt, cacheDir, BuildConfig{
		getenv: envMap(map[string]string{
			"JAVA_HOME": jdkHome,
			"PATH":      "/usr/bin:/bin",
		}),
	})
	if err != nil {
		t.Fatalf("GrantsFor: %v", err)
	}
	chmodInitDForCleanup(t, filepath.Join(cacheDir, "gradle"))
	if g.JDK().JavaHome != jdkHome {
		t.Errorf("JDK JavaHome = %q, want %q", g.JDK().JavaHome, jdkHome)
	}
	if !contains(g.ReadPaths, filepath.Join(jdkHome, "bin")) {
		t.Errorf("ReadPaths must grant the real JDK bin: %v", g.ReadPaths)
	}
	env := ChildEnv(g)
	m := map[string]string{}
	for _, kv := range env {
		if i := strings.IndexByte(kv, '='); i > 0 {
			m[kv[:i]] = kv[i+1:]
		}
	}
	if m["JAVA_HOME"] != jdkHome {
		t.Errorf("ChildEnv JAVA_HOME = %q, want %q", m["JAVA_HOME"], jdkHome)
	}
	if !strings.HasPrefix(m["PATH"], filepath.Join(jdkHome, "bin")+string(filepath.ListSeparator)) {
		t.Errorf("ChildEnv PATH = %q, want real JDK bin prepended", m["PATH"])
	}
}

// TestGrantsForResourceCeiling asserts the OMAC-generated gradle.properties
// carries the -Xmx ceiling.
func TestGrantsForResourceCeiling(t *testing.T) {
	wt, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cacheDir := filepath.Join(t.TempDir(), "cache")
	g, err := GrantsFor(wt, cacheDir, BuildConfig{MaxHeap: "1g"})
	if err != nil {
		t.Fatal(err)
	}
	chmodInitDForCleanup(t, filepath.Join(cacheDir, "gradle"))
	props, err := os.ReadFile(filepath.Join(cacheDir, "gradle", "gradle.properties"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(props), "org.gradle.jvmargs=-Xmx1g") {
		t.Errorf("gradle.properties missing -Xmx1g ceiling:\n%s", props)
	}
	_ = g
}

// TestSplitProxyEndpointParsesUserinfo asserts the proxy token (userinfo)
// is threaded through splitProxyEndpoint, not dropped. This was the
// ticket-04 root cause: the old splitter kept only host:port, so
// buildGradleOpts emitted no proxyUser/proxyPassword and the wrapper
// download got HTTP 407 Proxy Authentication Required.
func TestSplitProxyEndpointParsesUserinfo(t *testing.T) {
	ep := splitProxyEndpoint("http://omac:deadbeef@127.0.0.1:8080")
	if ep.Host != "127.0.0.1" || ep.Port != 8080 {
		t.Fatalf("host:port = %s:%d, want 127.0.0.1:8080", ep.Host, ep.Port)
	}
	if ep.User != "omac" {
		t.Errorf("User = %q, want omac (token user dropped → HTTP 407)", ep.User)
	}
	if ep.Password != "deadbeef" {
		t.Errorf("Password = %q, want deadbeef (token dropped → HTTP 407)", ep.Password)
	}
	if !ep.Valid() {
		t.Error("Valid() must be true with host:port (user/pass optional)")
	}
}

// TestSplitProxyEndpointNoUserinfo: a public/no-auth proxy (empty
// userinfo) parses to a valid endpoint with empty User/Password. The omac
// proxy ALWAYS carries a token (netproxy.Server.ProxyURL), but splitProxyEndpoint
// must not reject the no-auth shape (ticket-06 private-registry proxy).
func TestSplitProxyEndpointNoUserinfo(t *testing.T) {
	ep := splitProxyEndpoint("http://127.0.0.1:8080")
	if ep.Host != "127.0.0.1" || ep.Port != 8080 {
		t.Fatalf("host:port = %s:%d, want 127.0.0.1:8080", ep.Host, ep.Port)
	}
	if ep.User != "" || ep.Password != "" {
		t.Errorf("no-auth proxy must have empty User/Password: got %q/%q", ep.User, ep.Password)
	}
	if !ep.Valid() {
		t.Error("Valid() must be true with host:port (no-auth proxy)")
	}
}

// TestBuildGradleOptsEmitsProxyCredentials: the GRADLE_OPTS value carries
// the proxy token in https.proxyUser/https.proxyPassword (and http.* twins),
// which Gradle's HTTP client sends as Proxy-Authorization: Basic — exactly
// what netproxy.Server.authorized validates. The disabledSchemes line is
// present so HTTPS CONNECT tunnels accept Basic auth.
func TestBuildGradleOptsEmitsProxyCredentials(t *testing.T) {
	opts := buildGradleOpts(ProxyEndpoint{
		Host: "127.0.0.1", Port: 9090, User: "omac", Password: "tok-XYZ",
	})
	for _, want := range []string{
		"-Dhttp.proxyHost=127.0.0.1",
		"-Dhttp.proxyPort=9090",
		"-Dhttps.proxyHost=127.0.0.1",
		"-Dhttps.proxyPort=9090",
		"-Dhttp.proxyUser=omac",
		"-Dhttps.proxyUser=omac",
		"-Dhttp.proxyPassword=tok-XYZ",
		"-Dhttps.proxyPassword=tok-XYZ",
		"-Dhttp.nonProxyHosts=localhost|127.*|[::1]",
		"-Djdk.http.auth.tunneling.disabledSchemes=",
	} {
		if !strings.Contains(opts, want) {
			t.Errorf("GRADLE_OPTS missing %q: %q", want, opts)
		}
	}
}

// TestBuildGradleOptsNoUserOmitsCredentials: a no-auth proxy endpoint emits
// proxyHost/Port but NOT proxyUser/proxyPassword.
func TestBuildGradleOptsNoUserOmitsCredentials(t *testing.T) {
	opts := buildGradleOpts(ProxyEndpoint{Host: "127.0.0.1", Port: 9090})
	for _, bad := range []string{"proxyUser", "proxyPassword"} {
		if strings.Contains(opts, bad) {
			t.Errorf("no-auth GRADLE_OPTS must not contain %q: %q", bad, opts)
		}
	}
}

// TestGrantsForProxyTokenNotInGradleProperties: the proxy token rides in
// GRADLE_OPTS (per-process, JVM does not print it), NEVER in the
// OMAC-generated gradle.properties — that file is READABLE by build code
// and persists on disk in the cache leaf, so writing the token there would
// leak it across builds and to any build script that reads the file.
func TestGrantsForProxyTokenNotInGradleProperties(t *testing.T) {
	wt, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cacheDir := filepath.Join(t.TempDir(), "cache")
	_, err = GrantsFor(wt, cacheDir, BuildConfig{
		ProxyURL:  "http://omac:secret-token@127.0.0.1:9999",
		ProxyPort: 9999,
	})
	if err != nil {
		t.Fatal(err)
	}
	chmodInitDForCleanup(t, filepath.Join(cacheDir, "gradle"))
	props, err := os.ReadFile(filepath.Join(cacheDir, "gradle", "gradle.properties"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(props), "secret-token") {
		t.Errorf("gradle.properties must NOT contain the proxy token (readable by build code, persists on disk):\n%s", props)
	}
	if strings.Contains(string(props), "proxyUser") || strings.Contains(string(props), "proxyPassword") {
		t.Errorf("gradle.properties must NOT carry proxy credentials (token belongs in GRADLE_OPTS):\n%s", props)
	}
}
