//go:build e2e

// Package e2e end-to-end build brokered canary (issue #207).
//
// TestE2EJvmBuild drives the full omac JVM-build loop exactly as an
// agent does: an `omac start claude-code --inner /bin/sh` sandbox
// session (the claude-code binary is never launched into a model; the
// harness is used only for the --inner seam), a brokered `omac build`
// request submitted by the inner shell to the parent's host build
// broker, the restricted executor, and a REAL Gradle wrapper from the
// committed synthetic fixture. The canary is model-free: no
// SKAINET_* secrets, no model call, no diff review.
//
// It asserts on the filesystem artifacts Gradle writes (test-results
// XML), on the approval-gate negative path, and on the cold-cache
// pre-seed. The canary must be a LOUD failure on regression — a
// daemon-recycle revert, an image-allowlist removal, or a
// container-proxy cleanup regression must fail this test, never skip
// it (AGENTS.md's "missing toolchain = broken image, not a skip"
// rule).
//
// The fixture is `testdata/jvm-fixture/` — a minimal synthetic Gradle
// project (wrapper, Groovy DSL, JUnit 5 + Mockito + Testcontainers).
// It is deliberately NOT a copy of any client repo: its unit leg
// (GreetingServiceTest) covers the plain Mockito path, its IT leg
// (PostgresIT) covers the mediated container-access path, and its
// committed .omac/build.yaml declares the postgres:16-alpine image.
// No Spring: the property canaried is cold-daemon-per-build (listener
// re-registration + init.d/ re-read), not any framework class.
//
// Legs (the executor env is hermetic — buildrun.envPassThrough — so
// leg selection is by WHICH Gradle task runs, read by the TEST
// process, never by an env var smuggled into the executor):
//
//   - unit leg (default): runs `gradle test` — GreetingServiceTest
//     only. No Colima, no daemon, no container data path. The
//     container proxy is still STARTED host-side (the approved images
//     come from the frozen snapshot), but no container request is
//     made.
//   - IT leg (E2E_JVM_BUILD_IT=1): runs `gradle integrationTest` —
//     PostgresIT through the mediated container proxy. Requires a
//     reachable Docker/Colima daemon. The parent's container proxy
//     resolves its upstream from os.UserHomeDir() (the parent's HOME
//     — the test's temp HOME), so the test stages the daemon socket
//     at HOME/.colima/default/docker.sock (symlink from DOCKER_HOST or
//     the real user's socket).
//
// Approval-gate negative path: with the pre-seeded approval removed,
// the parent freezes NO snapshot (freezeSnapshotFromDurableApproval
// leaves the store empty), the broker's ParentSnapshotProvider returns
// ErrNoSnapshot, and the engine maps it to a service failure — exit
// 10 with the "no parent capability snapshot for this worktree (run
// `omac build approve` and restart the omac parent)" diagnostic. (The
// issue sketch says exit 3 + "manifest approval required" — that is
// the DIRECT-host GateError shape; the brokered path's unapproved
// worktree is a service failure by design: the parent must be
// restarted after approval, so a "do not retry" build-unavailable is
// the accurate contract. The canary asserts the actual brokered
// contract.)
//
// In a NESTED omac sandbox (E2E_NESTED / OMAC_SOCKET — see
// nestedInOmacSandbox in fixtures.go), the parent runs with
// --no-sandbox, which gives an EMPTY cache scope
// (prepareLaunchCache noSandbox → nil). A brokered build is then
// structurally impossible: brokerEngineInvoker fails CLOSED with exit
// 10 + errBrokeredBuildRequiresCacheRoot. The nested branch asserts
// that loud failure (the canary never silently skips) and documents
// the exposure — mirroring sandboxActive := !forceNoSandbox in the
// audit test.
package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/tngtech/oh-my-agentic-coder/internal/buildcontrol"
	"github.com/tngtech/oh-my-agentic-coder/internal/buildmanifest"
)

// buildRunTimeout bounds one `omac start` subprocess (the whole loop:
// sandbox launch, brokered build, Gradle). A cold Gradle build with a
// cold daemon can take several minutes; 30m matches the e2e-local.sh
// go-test timeout.
const buildRunTimeout = 30 * time.Minute

// jvmFixtureDir returns the repo's committed Gradle fixture directory.
func jvmFixtureDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join("..", "..", "internal", "e2e", "testdata", "jvm-fixture")
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

// copyJvmFixture copies the committed fixture into workdir as
// workdir/jvm-fixture (the --root the canary builds with) AND places
// the fixture's committed .omac/ (build manifest) at the WORKTREE root
// — the parent loads the manifest from the canonical worktree root,
// not from --root (buildmanifest.Load(resolved.Worktree) in the
// engine; freezeSnapshotFromDurableApproval uses the same root), so
// the approval pre-seed and the parent's frozen snapshot both key off
// the worktree-root manifest. Returns the fixture root.
func copyJvmFixture(t *testing.T, workdir string) string {
	t.Helper()
	src := jvmFixtureDir(t)
	dst := filepath.Join(workdir, "jvm-fixture")
	cmd := exec.Command("cp", "-R", src, dst)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("copy jvm fixture: %v\n%s", err, out)
	}
	// The fixture's .omac/build.yaml is the manifest of record; the
	// parent reads it at the worktree root.
	omacDst := filepath.Join(workdir, ".omac")
	if err := os.MkdirAll(omacDst, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(src, ".omac", "build.yaml"))
	if err != nil {
		t.Fatalf("read fixture manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(omacDst, "build.yaml"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	return dst
}

// preSeedBuildApproval writes the durable BuildControl approval record
// for the fixture worktree — the TTY-less pre-seed recipe from
// internal/cli/serve_snapshot_test.go:56-66, exported-API only (e2e
// cannot import internal/cli).
//
// The parent reads this at launch (startSnapshotProvider →
// freezeSnapshotFromDurableApproval) and freezes the in-memory
// capability snapshot for the session. Without it, a brokered build
// fails with "no parent capability snapshot" (build unavailable until
// approve + restart).
//
// cacheDir is the resolved cache scope dir UNDER THE TEST'S TEMP HOME
// (the parent prepares it under its own HOME, which runInnerBuild sets
// to the temp home); canon is the canonical worktree root (the parent
// loads the manifest from there, and copyJvmFixture placed the
// fixture's committed .omac/build.yaml at that root).
func preSeedBuildApproval(t *testing.T, cacheDir, canon string) {
	t.Helper()
	// The manifest of record is at the WORKTREE root (what the parent
	// loads). Load from there so the digest matches exactly what
	// freezeSnapshotFromDurableApproval computes.
	manifest, err := buildmanifest.Load(canon)
	if err != nil {
		t.Fatalf("load fixture manifest: %v", err)
	}
	digest := buildmanifest.Digest(manifest)
	caps := manifest.CapabilitySet(buildmanifest.HostPolicy{})
	leaf := filepath.Join(cacheDir, "gradle")
	root := buildcontrol.CacheRootFromCacheDir(cacheDir)
	if root == "" {
		t.Fatal("pre-seed approval: empty cache root")
	}
	loc := buildmanifest.NewBuildControlLocation(root, canon)
	if err := buildmanifest.ApproveAt(leaf, loc, digest, caps); err != nil {
		t.Fatalf("pre-seed approval: %v", err)
	}
	t.Logf("pre-seeded approval for %s (digest %.8s, images %v)", canon, digest, caps.Images)
}

// removeApproval deletes the durable approval record for canon under
// the build-control root of cacheDir.
func removeApproval(t *testing.T, cacheDir, canon string) {
	t.Helper()
	root := buildcontrol.CacheRootFromCacheDir(cacheDir)
	if root == "" {
		t.Fatal("remove approval: empty cache root")
	}
	hash := sha256.Sum256([]byte(canon))
	path := filepath.Join(root, "build-control", "approvals", hex.EncodeToString(hash[:])+".json")
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove approval file %s: %v", path, err)
	}
	t.Logf("removed approval %s", path)
}

// canonicalWorktree resolves the canonical (symlink-resolved) worktree
// path — the form buildControlApprovalLocation uses in internal/cli.
func canonicalWorktreeForTest(t *testing.T, workdir string) string {
	t.Helper()
	abs, err := filepath.Abs(workdir)
	if err != nil {
		t.Fatal(err)
	}
	canon, err := filepath.EvalSymlinks(abs)
	if err != nil {
		t.Fatal(err)
	}
	return canon
}

// shortCacheHome creates a SHORT-path HOME for the parent's build
// broker. The daemon-handshake socket lives at
// <cacheRoot>/build-control/requests/<id>/daemon.sock
// (<cacheRoot> = dir(cacheScopeDir) = <home>/.cache/omac); on macOS a
// t.TempDir() under /var/folders/... pushes that past the 104-byte
// SUN_LEN boundary (bind: invalid argument). A short /tmp-rooted home
// keeps the socket path legal. The parent needs a real HOME (the
// cache-scope + build-control machinery reads os.UserHomeDir()), so
// this builds the full layout under /tmp with a per-test unique leaf.
func shortCacheHome(t *testing.T) string {
	t.Helper()
	unique := filepath.Join(os.TempDir(), fmt.Sprintf("omac-e2e-%d-%d", os.Getpid(), time.Now().UnixNano()%1e6))
	for _, d := range []string{".cache", ".local/share", ".local/state", ".config", ".claude", ".cargo/bin", ".rustup"} {
		if err := os.MkdirAll(filepath.Join(unique, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { os.RemoveAll(unique) })
	return unique
}

// tempHomeSharedCacheScopeDir computes the persistent shared cache
// scope dir under a given HOME without touching the test process's
// HOME: the default cache scope is global → DomainShared, whose
// identity is the constant "v1:shared" and Dir is
// <home>/.cache/omac/<sha256("v1:shared")>.
func tempHomeSharedCacheScopeDir(home string) string {
	sum := sha256.Sum256([]byte("v1:shared"))
	return filepath.Join(home, ".cache", "omac", hex.EncodeToString(sum[:]))
}

// stageColimaSocket exposes a reachable Docker/Colima daemon socket at
// the TEST's temp HOME/.colima/default/docker.sock so the parent's
// container proxy (which resolves upstream from os.UserHomeDir() at
// startup — the parent's HOME is the temp home) can reach it.
//
// The IT leg's CI workflow starts Colima and exports DOCKER_HOST; the
// test reads that (or the well-known socket of the REAL user HOME)
// and symlinks it into the temp home.
func stageColimaSocket(t *testing.T, home string) {
	t.Helper()
	if runtime.GOOS != "darwin" {
		return
	}
	colimaDir := filepath.Join(home, ".colima", "default")
	if err := os.MkdirAll(colimaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sockPath := filepath.Join(colimaDir, "docker.sock")
	if fi, err := os.Stat(sockPath); err == nil && fi.Mode()&os.ModeSocket != 0 {
		return
	}
	var candidates []string
	if v := os.Getenv("DOCKER_HOST"); v != "" {
		p := strings.TrimPrefix(strings.TrimPrefix(v, "unix://"), "unix:")
		if p != "" && p != v {
			candidates = append(candidates, p)
		}
	}
	if real := os.Getenv("HOME"); real != "" {
		candidates = append(candidates, filepath.Join(real, ".colima", "default", "docker.sock"))
	}
	for _, src := range candidates {
		if src == "" {
			continue
		}
		if fi, err := os.Stat(src); err == nil && fi.Mode()&os.ModeSocket != 0 {
			if err := os.Symlink(src, sockPath); err != nil {
				t.Fatalf("symlink colima socket %s -> %s: %v", sockPath, src, err)
			}
			t.Logf("staged colima socket %s -> %s", sockPath, src)
			return
		}
	}
	t.Logf("no docker socket found; the container proxy will still start but container requests will fail (IT leg needs Colima)")
}

// runInnerBuild launches `omac start claude-code --inner /bin/sh --`
// with the inner shell invoking the brokered `./omac build --root
// jvm-fixture -- gradle <task>` against the broker the parent injects
// into the sandbox env. `omacBin` is the outer omac binary (also
// copied into the workdir as `innerOmac` — the sandbox grants the
// workdir read+write, so it is executable there). Returns combined
// output + exit code.
//
// The parent runs with HOME=home (the temp cache-test home), so the
// cache scope + build-control root resolve under it — matching where
// preSeedBuildApproval wrote the record. In a nested omac sandbox the
// parent needs --no-sandbox (macOS denies nested sandbox_apply);
// cache-isolation tests pass it via extraArgs, and this helper adds it
// when nestedInOmacSandbox().
func runInnerBuild(t *testing.T, omacBin, home, workdir, innerOmac, task string) (string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), buildRunTimeout)
	defer cancel()
	args := []string{"start", "claude-code"}
	if nestedInOmacSandbox() {
		args = append(args, "--no-sandbox")
	}
	// --inner /bin/sh tells the parent to exec /bin/sh as the sandboxed
	// inner command; everything after -- is its argv. The leading
	// /bin/sh is NOT repeated (the parent prepends the resolved inner
	// command to innerArgs).
	args = append(args, "--inner", "/bin/sh", "--")
	cmdLine := fmt.Sprintf("%s build --root jvm-fixture -- gradle %s", innerOmac, task)
	args = append(args, "-c", cmdLine)
	cmd := exec.CommandContext(ctx, omacBin, args...)
	cmd.Dir = workdir
	env := withHome(os.Environ(), home)
	env = append(env, "PWD="+workdir)
	cmd.Env = env
	cmd.Stdin = strings.NewReader("")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			t.Fatalf("exec omac start: %v\nSTDOUT:\n%s\nSTDERR:\n%s", err, stdout.String(), stderr.String())
		}
	}
	if ctx.Err() == context.DeadlineExceeded {
		t.Fatalf("omac start (build loop) timed out after %v\nSTDOUT:\n%s\nSTDERR:\n%s",
			buildRunTimeout, stdout.String(), stderr.String())
	}
	return stdout.String() + "\n" + stderr.String(), code
}

// jvmResultXML returns the standard Gradle test-results XML for one
// class under build/test-results/<task>/TEST-<class>.xml.
func jvmResultXML(fixtureRoot, task, class string) string {
	return filepath.Join(fixtureRoot, "build", "test-results", task, "TEST-"+class+".xml")
}

// assertResultXML asserts a Gradle JUnit XML report exists with
// failures="0" errors="0" and logs the class it covered.
func assertResultXML(t *testing.T, path, class string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read test results XML %s: %v", path, err)
	}
	s := string(data)
	for _, want := range []string{`failures="0"`, `errors="0"`} {
		if !strings.Contains(s, want) {
			t.Errorf("%s: missing %s in report (%s):\n%s", path, want, class, s)
		}
	}
	t.Logf("asserted %s green (%s)", class, path)
}

// jvmGradleDistWarm reports whether the shared cache scope holds a
// usable Gradle distribution for the fixture wrapper
// (GRADLE_USER_HOME=<cacheScope>/gradle → dists under
// <cacheScope>/gradle/wrapper/dists with at least one unpacked
// version dir).
func jvmGradleDistWarm(cacheScopeDir string) bool {
	dists := filepath.Join(cacheScopeDir, "gradle", "wrapper", "dists")
	entries, err := os.ReadDir(dists)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() {
			return true
		}
	}
	return false
}

// ensureGradleDistWarm pre-seeds the Gradle distribution into the
// shared cache scope if absent (host-side ./gradlew --version under
// the fixture) and FAILS LOUDLY (never skips) if it still cannot be
// made warm — the AGENTS.md "missing toolchain = broken image, not a
// skip" rule for the build canary.
func ensureGradleDistWarm(t *testing.T, fixtureRoot, cacheScopeDir string) {
	t.Helper()
	if jvmGradleDistWarm(cacheScopeDir) {
		return
	}
	t.Logf("cold cache: Gradle dist absent; seeding host-side once")
	seedGradleDist(t, fixtureRoot, cacheScopeDir)
	if !jvmGradleDistWarm(cacheScopeDir) {
		t.Fatalf("gradle dist still not warm after host-side pre-seed; the fixture wrapper cannot run")
	}
	t.Logf("gradle dist warm at %s", filepath.Join(cacheScopeDir, "gradle", "wrapper", "dists"))
}

// seedGradleDist runs the fixture's real wrapper host-side once to
// warm the shared cache scope (GRADLE_USER_HOME=<cacheScope>/gradle).
// A seed failure is fatal (missing toolchain = broken image, not a
// skip).
func seedGradleDist(t *testing.T, fixtureRoot, cacheScopeDir string) {
	t.Helper()
	wrapper := filepath.Join(fixtureRoot, "gradlew")
	if _, err := os.Stat(wrapper); err != nil {
		t.Fatalf("fixture gradlew missing: %v", err)
	}
	cmd := exec.Command(wrapper, "--version")
	cmd.Dir = fixtureRoot
	cmd.Env = append(os.Environ(),
		"GRADLE_USER_HOME="+filepath.Join(cacheScopeDir, "gradle"),
		"GRADLE_OPTS=-Dorg.gradle.jvmargs=-Xmx512m",
	)
	out, err := cmd.CombinedOutput()
	t.Logf("gradlew --version pre-seed: err=%v\n%s", err, string(out))
	if err != nil {
		t.Fatalf("cold-cache pre-seed gradlew --version failed: %v\n%s", err, out)
	}
	dists := filepath.Join(cacheScopeDir, "gradle", "wrapper", "dists")
	if _, err := os.Stat(dists); err != nil {
		t.Fatalf("gradle dists still absent after pre-seed: %v", err)
	}
	t.Logf("gradle dist pre-seeded at %s", dists)
}

// writeJvmBuildProfile writes a sandbox profile for the build canary.
// Unlike writeCacheTestProfile (network blocked), a brokered JVM build
// REQUIRES loopback TCP: the sandboxed `omac build` POSTs to the
// parent's loopback build broker (OMAC_CONTROL_BASE), and the parent
// whitelists that ephemeral port into the sandbox argv via
// --open-port. On macOS the Seatbelt generator emits `(deny network*)`
// under network.mode "blocked" and ignores open-port exceptions there
// (sbpl.go: the open-port loop only runs in the filtered branch), so a
// blocked profile makes every brokered build die with `connect:
// operation not permitted` before the request ever reaches the engine —
// masking even the approval-gate diagnostic the negative subtest
// asserts. Filtered mode honors the injected --open-port and additionally
// whitelists every loopback connection (the Gradle daemon binds
// ephemeral loopback ports for its worker protocol).
//
// proxy_injection ["jvm"] makes the supervisor point every JVM at the
// omac filtering proxy via JAVA_TOOL_OPTIONS — the sanctioned path for
// Maven-central resolution under a filtered sandbox (allow_domain reads
// as a proxy-egress allowlist, and the JVM ignores HTTP(S)_PROXY).
//
// The Colima socket the IT leg stages under ~/.colima is granted here
// (the parent's container proxy reads it); the parent connects to the
// daemon directly (the proxy is a parent-side process), and the
// sandboxed Gradle reaches it via the proxy's loopback port.
//
// jvmReadPaths MUST include the real JDK home: under the default
// Seatbelt deny-policy the sandboxed /bin/sh runs `gradlew`, whose
// launcher JVM immediately reads <jdk>/lib/security/java.security —
// without a grant the wrapper dies with java.lang.InternalError
// "Error loading java.security file" before it can do anything
// (enzyme-cold flats, dist not yet installed). The production buildrun
// engine's JDK read-grants apply only to the SEPARATE style executor
// sandbox (its own buildrun.BuildGrants), not to this outer shell's
// Seatbelt profile, so the canary must grant the JDK here (mirroring
// how toolRuntimeReadPaths grants go/python/node for the cache tests).
func writeJvmBuildProfile(t *testing.T, home string) {
	t.Helper()
	profDir := filepath.Join(home, ".config", "omac", "sandbox-profiles")
	if err := os.MkdirAll(profDir, 0o755); err != nil {
		t.Fatal(err)
	}
	read := []string{"~/.colima"}
	read = append(read, jvmReadPaths(t)...)
	profile := map[string]any{
		"meta":    map[string]string{"name": "default"},
		"workdir": map[string]string{"access": "readwrite"},
		"filesystem": map[string]any{
			"read":  read,
			"allow": nil,
		},
		// The canary asserts a LOUD regression on daemon-recycle /
		// image-allowlist / container cleanup — never a skip. The
		// executor env is hermetic, so proxy_injection only routes the
		// sandbox-side wrapper JVMs (the gradlew launcher); the build
		// executor itself runs unsandboxed on the host.
		"network": map[string]any{
			"mode":            "filtered",
			"allow_domain":    []string{"127.0.0.1", "localhost", "repo.maven.apache.org", "services.gradle.org", "plugins.gradle.org"},
			"proxy_injection": []string{"jvm"},
		},
		// Same rationale as writeCacheTestProfile: the dev tools need
		// their ambient env (JDK paths, GRADLE_USER_HOME redirect, the
		// broker env the parent injects), so inherit every ambient var
		// minus the danger blocklist.
		"environment": map[string]any{
			"allow_vars": []string{"*"},
		},
	}
	data, err := json.MarshalIndent(profile, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profDir, "default.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// jvmReadPaths resolves the test-runner's JDK home (JAVA_HOME, else the
// java on PATH) and returns the read-only grant dirs the sandboxed
// /bin/sh needs for gradlew to start the wrapper JVM. Mirrors
// buildrun.jdkReadPaths (bin + the existing lib/libexec/lib64 install
// dirs) but computed by the test from the host env, because the
// profile is written before `omac start` runs and exists outside the
// build engine's own executor-scoped grant set. Fails loudly when no
// JDK is discoverable — on CI setup-java always sets JAVA_HOME; a
// missing JDK is a broken image, not a skip.
func jvmReadPaths(t *testing.T) []string {
	t.Helper()
	home := os.Getenv("JAVA_HOME")
	if home == "" {
		javaBin, err := exec.LookPath("java")
		if err != nil {
			t.Fatalf("no JDK discoverable: JAVA_HOME unset and java not on PATH")
		}
		resolved, err := filepath.EvalSymlinks(javaBin)
		if err != nil {
			t.Fatalf("resolve java %q: %v", javaBin, err)
		}
		// Strip /bin/java to the install home.
		home = filepath.Dir(filepath.Dir(resolved))
	}
	if fi, err := os.Stat(filepath.Join(home, "bin", "java")); err != nil || fi.IsDir() {
		t.Fatalf("resolved JDK home %q lacks bin/java (stat: %v)", home, err)
	}
	// Grant the install home itself: JDKs differ in layout (Java 8 keeps
	// java.security + lib/ at the root; Java 9+ splits into conf/,
	// jmods/, lib/; Homebrew nests the real home under libexec/). Granting
	// each subdir separately fragments across vendors; every entry in a
	// JDK home is a read-only runtime asset, and the home is a leaf
	// install tree (never a broad root like /usr), so the read grant
	// stays bounded to this one JDK.
	t.Logf("granting sandbox JDK read home: %s", home)
	return []string{home}
}

// TestE2EJvmBuild is the build brokered canary.
func TestE2EJvmBuild(t *testing.T) {
	skipIfSandboxUnavailable(t)

	// The parent's build broker creates the daemon-handshake socket
	// under <home>/.cache/omac/build-control/requests/<id>/daemon.sock;
	// on macOS that must stay under SUN_LEN (104), so the parent runs
	// with a SHORT /tmp-rooted HOME (never the deep t.TempDir()).
	home := shortCacheHome(t)
	workdir := t.TempDir()
	writeJvmBuildProfile(t, home)

	outerBin := buildOmac(t)

	// The inner omac binary is copied into the workdir (the sandbox
	// grants the workdir read+write, so it is executable inside).
	innerBin := filepath.Join(workdir, "omac")
	if data, err := os.ReadFile(outerBin); err != nil {
		t.Fatal(err)
	} else if err := os.WriteFile(innerBin, data, 0o755); err != nil {
		t.Fatal(err)
	}

	fixtureRoot := copyJvmFixture(t, workdir)
	canon := canonicalWorktreeForTest(t, workdir)
	// The parent runs with HOME=home (runInnerBuild sets it), so the
	// cache scope resolves under the temp home.
	cacheDir := tempHomeSharedCacheScopeDir(home)
	_ = fixtureRoot // the fixture root's manifest is copied to the worktree root by copyJvmFixture

	itLeg := os.Getenv("E2E_JVM_BUILD_IT") == "1"
	nested := nestedInOmacSandbox()

	if nested {
		// Nested omac sandbox: the parent runs --no-sandbox → empty
		// cache scope → brokered builds fail CLOSED before the
		// snapshot provider ever runs (exit 10,
		// errBrokeredBuildRequiresCacheRoot). Neither the approval
		// negative path nor the positive loop is reachable. Assert the
		// loud failure and document the exposure instead of silently
		// skipping (issue #66 spirit).
		t.Run("nested-exposure", func(t *testing.T) {
			preSeedBuildApproval(t, cacheDir, canon)
			out, code := runInnerBuild(t, outerBin, home, workdir, innerBin, "test")
			if code != 10 {
				t.Fatalf("nested brokered build: exit = %d, want 10 (empty cache scope)\n%s", code, out)
			}
			if !strings.Contains(out, "build-control cache root") {
				t.Errorf("nested brokered build: missing 'build-control cache root' diagnostic:\n%s", out)
			}
			t.Logf("nested run documented: the full loop cannot execute in a nested omac sandbox (empty cache scope → exit 10); unit+IT legs run on host/CI")
		})
		return
	}

	t.Run("approval-gate-negative", func(t *testing.T) {
		// A fresh parent per subtest (each runInnerBuild launches a new
		// omac start), so the approval state is read fresh at launch.
		preSeedBuildApproval(t, cacheDir, canon)
		removeApproval(t, cacheDir, canon)
		out, code := runInnerBuild(t, outerBin, home, workdir, innerBin, "test")
		if code != 10 {
			t.Fatalf("without approval: exit = %d, want 10 (service failure: no parent snapshot)\n%s", code, out)
		}
		if !strings.Contains(out, "no parent capability snapshot") {
			t.Errorf("without approval: missing 'no parent capability snapshot' diagnostic:\n%s", out)
		}
		if !strings.Contains(out, "omac build approve") {
			t.Errorf("without approval: missing 'omac build approve' hint:\n%s", out)
		}
	})

	// --- Unit leg (default): the full brokered loop through gradle test.
	t.Run("unit-leg-loop", func(t *testing.T) {
		preSeedBuildApproval(t, cacheDir, canon)

		// Cold-cache pre-seed with loud failure: if the Gradle dist is
		// absent from the shared cache scope, seed it host-side once
		// (./gradlew --version under the fixture); if it still cannot
		// be made warm, t.Fatalf.
		ensureGradleDistWarm(t, fixtureRoot, cacheDir)

		out, code := runInnerBuild(t, outerBin, home, workdir, innerBin, "test")
		if code != 0 {
			t.Fatalf("unit leg build failed (exit %d):\n%s", code, out)
		}
		assertResultXML(t, jvmResultXML(fixtureRoot, "test", "com.omac.fixture.GreetingServiceTest"), "GreetingServiceTest")
		// The IT class must NOT run under `gradle test` (it is excluded
		// by the task's include filter in build.gradle).
		postgresXML := jvmResultXML(fixtureRoot, "test", "com.omac.fixture.PostgresIT")
		if _, err := os.Stat(postgresXML); !os.IsNotExist(err) {
			t.Errorf("unit leg must not run PostgresIT (integrationTest is the IT leg): %s exists", postgresXML)
		}
	})

	// --- IT leg (E2E_JVM_BUILD_IT=1): PostgresIT through the proxy.
	t.Run("it-leg-loop", func(t *testing.T) {
		if !itLeg {
			t.Skip("E2E_JVM_BUILD_IT not set; IT leg needs a reachable Docker/Colima daemon (unit leg only)")
		}
		if runtime.GOOS != "darwin" {
			t.Skip("container proxy is macOS-only in v1 (Linux executor is kernel-blocked; the IT leg runs on the macos-15-intel CI leg)")
		}
		preSeedBuildApproval(t, cacheDir, canon)
		stageColimaSocket(t, home)

		// The cold cache is warm from the unit leg (same cache scope);
		// still seed if a fresh run skipped the unit leg.
		ensureGradleDistWarm(t, fixtureRoot, cacheDir)
		out, code := runInnerBuild(t, outerBin, home, workdir, innerBin, "integrationTest")
		if code != 0 {
			t.Fatalf("IT leg build failed (exit %d):\n%s", code, out)
		}
		assertResultXML(t, jvmResultXML(fixtureRoot, "integrationTest", "com.omac.fixture.PostgresIT"), "PostgresIT")

		// Container-cleanup assertion (ADR 0002): the container proxy's
		// lifecycle cleanup removes executor-owned containers + the
		// executor-owned internal network when the parent tears down
		// (runInnerBuild has returned, so the parent's defer chain ran).
		// Assert via the real Docker CLI that nothing labeled
		// omac.executor=<id> remains.
		execID := containerExecutorID(canon)
		outBytes, err := dockerListOwned(t, execID, "containers")
		if err != nil {
			t.Fatalf("docker ps (cleanup assert): %v", err)
		}
		if strings.TrimSpace(outBytes) != "" {
			t.Errorf("executor-owned containers remain after teardown (label=%s):\n%s", "omac.executor="+execID, outBytes)
		} else {
			t.Logf("container cleanup asserted: no executor-owned containers remain")
		}
		netOut, err := dockerListOwned(t, execID, "networks")
		if err != nil {
			t.Fatalf("docker network ls (cleanup assert): %v", err)
		}
		if strings.TrimSpace(netOut) != "" {
			t.Errorf("executor-owned networks remain after teardown (label=%s):\n%s", "omac.executor="+execID, netOut)
		} else {
			t.Logf("network cleanup asserted: no executor-owned networks remain")
		}
	})
}

// dockerListOwned lists Docker resources (containers|networks) labeled
// omac.executor=<execID> via the real Docker CLI. Works against the
// runner's DOCKER_HOST (IT leg CI: Colima).
func dockerListOwned(t *testing.T, execID, kind string) (string, error) {
	t.Helper()
	var cmd *exec.Cmd
	switch kind {
	case "containers":
		cmd = exec.Command("docker", "ps", "-a",
			"--filter", "label=omac.executor="+execID,
			"--format", "{{.ID}} {{.Image}}")
	case "networks":
		cmd = exec.Command("docker", "network", "ls",
			"--filter", "label=omac.executor="+execID,
			"--format", "{{.ID}} {{.Name}}")
	default:
		t.Fatalf("dockerListOwned: unknown kind %q", kind)
	}
	out, err := cmd.Output()
	return string(out), err
}

// containerExecutorID mirrors internal/cli's containerExecutorID (a
// stable per-worktree executor ownership label value derived from the
// canonical worktree base name). The container proxy labels
// executor-owned resources omac.executor=<id>; the cleanup assertion
// filters on it.
func containerExecutorID(canonWorktree string) string {
	if canonWorktree == "" {
		return "omac-exec"
	}
	base := filepath.Base(canonWorktree)
	if base == "" || base == "." || base == string(filepath.Separator) {
		return "omac-exec"
	}
	return "omac-" + base
}
