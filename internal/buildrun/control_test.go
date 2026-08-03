package buildrun

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// chmodInitDForCleanup restores init.d writability so t.TempDir's
// RemoveAll can unlink the always-written retire-checkstyle-twins.gradle
// (and any registry-credentials.gradle) inside it. PrepareControlState
// creates init.d read-only (0o500) to keep build code from planting an
// init script; that mode blocks RemoveAll, so every test that builds a
// leaf must register this cleanup. Safe to call with an absent/empty
// leaf (the chmod is best-effort).
func chmodInitDForCleanup(t *testing.T, leaf string) {
	t.Helper()
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(leaf, "init.d"), 0o755) })
}

func TestRenderGradleProperties_ProxyAndHeap(t *testing.T) {
	s := RenderGradleProperties(GradlePropertiesConfig{
		Proxy: ProxyEndpoint{Host: "127.0.0.1", Port: 8080}, MaxHeap: "1g",
	})
	for _, want := range []string{
		"systemProp.http.proxyHost=127.0.0.1",
		"systemProp.http.proxyPort=8080",
		"systemProp.https.proxyHost=127.0.0.1",
		"systemProp.https.proxyPort=8080",
		"systemProp.http.nonProxyHosts=localhost|127.*|[::1]",
		"systemProp.jdk.http.auth.tunneling.disabledSchemes=",
		"org.gradle.jvmargs=-Xmx1g",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("gradle.properties missing %q:\n%s", want, s)
		}
	}
}

func TestRenderGradleProperties_NoProxyOmitsProxyLines(t *testing.T) {
	s := RenderGradleProperties(GradlePropertiesConfig{MaxHeap: "512m"})
	if strings.Contains(s, "proxyHost") {
		t.Errorf("proxy lines must be absent when no proxy:\n%s", s)
	}
	if !strings.Contains(s, "org.gradle.jvmargs=-Xmx512m") {
		t.Errorf("heap line missing:\n%s", s)
	}
}

func TestPrepareControlState_WritesReadOnlyFiles(t *testing.T) {
	leaf := t.TempDir()
	chmodInitDForCleanup(t, leaf)
	paths, err := PrepareControlState(leaf, GradlePropertiesConfig{
		Proxy: ProxyEndpoint{Host: "127.0.0.1", Port: 9090}, MaxHeap: "2g",
	})
	if err != nil {
		t.Fatalf("PrepareControlState: %v", err)
	}
	// gradle.properties, the README, and the init.d control dir all exist.
	props := filepath.Join(leaf, "gradle.properties")
	readme := filepath.Join(leaf, controlStateName, "README")
	initD := filepath.Join(leaf, "init.d")
	for _, p := range []string{props, readme, initD} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("control path %s not written: %v", p, err)
		}
	}
	// init.d must be read-only to the executor (mode 0o500) so build code
	// cannot plant an init script inside it.
	if fi, err := os.Stat(initD); err == nil {
		if got := fi.Mode().Perm(); got != 0o500 {
			t.Errorf("init.d perms = %o, want 500 (read-only to executor)", got)
		}
	}
	// Returned control files: gradle.properties + README + the
	// ticket-07 retire-checkstyle-twins init script + the ticket-08
	// mockito-agent init script (both always written).
	if len(paths.Files) != 4 {
		t.Fatalf("got %d control file paths, want 4: %v", len(paths.Files), paths.Files)
	}
	// Returned control dirs: init.d (1).
	if len(paths.Dirs) != 1 || filepath.Base(paths.Dirs[0]) != "init.d" {
		t.Fatalf("got control dirs %v, want 1 entry: init.d", paths.Dirs)
	}
}

func TestPrepareControlState_InitDReadOnlyToExecutor(t *testing.T) {
	leaf := t.TempDir()
	chmodInitDForCleanup(t, leaf)
	if _, err := PrepareControlState(leaf, GradlePropertiesConfig{}); err != nil {
		t.Fatal(err)
	}
	initD := filepath.Join(leaf, "init.d")
	fi, err := os.Stat(initD)
	if err != nil {
		t.Fatalf("init.d not created: %v", err)
	}
	if fi.Mode().Perm()&0o200 != 0 {
		t.Errorf("init.d is writable by owner (mode %o); must be read-only to the executor so build code cannot plant an init script", fi.Mode().Perm())
	}
}

// TestRenderRegistryCredentialsInitScript_EmptyWhenNoRegistries asserts the
// init script is a no-op (empty) when no private registries are approved —
// the common case. The credential-lift init script must not be written.
func TestRenderRegistryCredentialsInitScript_EmptyWhenNoRegistries(t *testing.T) {
	if got := RenderRegistryCredentialsInitScript(nil); got != "" {
		t.Errorf("empty urls must yield empty script, got:\n%s", got)
	}
	if got := RenderRegistryCredentialsInitScript(map[string]string{}); got != "" {
		t.Errorf("empty urls map must yield empty script, got:\n%s", got)
	}
}

// TestRenderRegistryCredentialsInitScript_NonSecretURLsNoCredential asserts
// the init script contains the non-secret local proxy URLs but NEVER a
// credential. It maps each alias to its local loopback URL with no userinfo.
func TestRenderRegistryCredentialsInitScript_NonSecretURLsNoCredential(t *testing.T) {
	urls := map[string]string{
		"internal": "http://127.0.0.1:12345/internal/",
		"stage":    "http://127.0.0.1:12345/stage/",
	}
	s := RenderRegistryCredentialsInitScript(urls)
	for _, want := range []string{
		"allprojects",
		"maven {",
		"omac-credproxy-internal",
		"http://127.0.0.1:12345/internal/",
		"omac-credproxy-stage",
		"http://127.0.0.1:12345/stage/",
		// The credential-lift comment must state the proxy authenticates.
		"credential-lift proxy",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("init script missing %q:\n%s", want, s)
		}
	}
	// The credential must not appear. (No credential is passed into the
	// render, so this guards against a future regression that threads one.)
	// "secret"/"password"/"token" alone are banned only as VALUES — the
	// comments legitimately use the word "credential", so do NOT ban
	// that word; ban only concrete credential material.
	for _, banned := range []string{"alice", "s3cr3t", ":s3cr3t", "password=", "user:pass"} {
		if strings.Contains(s, banned) {
			t.Errorf("init script must not contain credential material %q:\n%s", banned, s)
		}
	}
	// Determinism: re-rendering yields identical output (sorted aliases).
	if s2 := RenderRegistryCredentialsInitScript(urls); s2 != s {
		t.Errorf("init script is not deterministic across renders")
	}
}

// TestPrepareControlState_WritesRegistryInitScript asserts the init.d
// script is written and granted read-only when registry proxy URLs are
// configured. The credential never appears in the file.
func TestPrepareControlState_WritesRegistryInitScript(t *testing.T) {
	leaf := t.TempDir()
	// init.d is created read-only (0o500) by PrepareControlState, which
	// blocks t.TempDir's cleanup RemoveAll. Restore writability on cleanup.
	chmodInitDForCleanup(t, leaf)
	const cred = "alice:s3cr3t"
	urls := map[string]string{
		"internal": "http://127.0.0.1:12345/internal/",
	}
	paths, err := PrepareControlState(leaf, GradlePropertiesConfig{
		RegistryProxyURLs: urls,
	})
	if err != nil {
		t.Fatalf("PrepareControlState: %v", err)
	}
	initScript := filepath.Join(leaf, "init.d", registryCredentialsInitName)
	data, err := os.ReadFile(initScript)
	if err != nil {
		t.Fatalf("registry-credentials init script not written: %v", err)
	}
	body := string(data)
	if !strings.Contains(body, "http://127.0.0.1:12345/internal/") {
		t.Errorf("init script missing the proxy URL:\n%s", body)
	}
	if strings.Contains(body, cred) || strings.Contains(body, "s3cr3t") {
		t.Errorf("credential leaked into init script:\n%s", body)
	}
	// The init script file is granted read-only: it appears in the
	// control files list (existence-filtered) AND its parent init.d dir
	// is in control dirs (read-only). Assert it appears in the returned
	// control files.
	found := false
	for _, p := range paths.Files {
		if strings.HasSuffix(p, registryCredentialsInitName) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("registry-credentials init script not in control files (read-only grant missing): %v", paths.Files)
	}
}

// TestRenderRetireCheckstyleTwinsInitScript_NonEmpty asserts the retirement
// init script is always emitted (the retirement applies to every build — it
// is a defensive no-op when no yarp3 checkstyle*Sandbox twins exist).
func TestRenderRetireCheckstyleTwinsInitScript_NonEmpty(t *testing.T) {
	s := RenderRetireCheckstyleTwinsInitScript()
	if s == "" {
		t.Fatal("retire-checkstyle-twins init script must always be non-empty (defensive no-op when no twins)")
	}
}

// TestRenderRetireCheckstyleTwinsInitScript_NeutralizesTwins asserts the
// retirement init script contains the checkstyle-twin neutralization logic:
// it runs before the project task graph, matches the yarp3
// checkstyle*Sandbox twin convention, overrides the twin's actions to a
// no-op, and is wrapped defensively so a project without the twins is
// unaffected.
func TestRenderRetireCheckstyleTwinsInitScript_NeutralizesTwins(t *testing.T) {
	s := RenderRetireCheckstyleTwinsInitScript()
	for _, want := range []string{
		// Runs before the task graph is materialized.
		"beforeProject",
		// Matches the yarp3 checkstyle*Sandbox twin convention.
		"checkstyle.*Sandbox",
		// Task configuration avoidance API (only projects with twins configure).
		"matching",
		"configureEach",
		// Overrides the twin's actions to a no-op so the canonical tasks run.
		"task.actions = []",
		// Defensive try/catch so a project without twins is unaffected.
		"catch (Exception e)",
		// Header explains WHY (ADR 0003 Revision retired guarded loopback).
		"ADR 0003 Revision",
		// Read-only contract.
		"READ-ONLY to the executor",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("retire-checkstyle-twins init script missing %q:\n%s", want, s)
		}
	}
	// The retirement log line must fire at configuration time via the
	// init-script logger, NOT via task.doFirst: a subsequent
	// task.actions = [] clears the action list, so a doFirst closure
	// registered moments before would be wiped and never log. This
	// catches the dead-doFirst ordering bug (review finding: the log
	// line is the operator-visible signal that a twin was retired).
	// Assert no EXECUTABLE doFirst call exists — the phrase may appear
	// only inside a `//` comment explaining why it is NOT used.
	execDoFirst := strings.Contains(s, "task.doFirst {")
	if execDoFirst {
		t.Errorf("retire script must not call task.doFirst { } — task.actions = [] would clear it; log at configuration time via the init-script logger instead:\n%s", s)
	}
	// The lifecycle log line must be present (configuration-time, not a
	// doFirst action) and must appear BEFORE the executable
	// task.actions = [] clear (it fires at configuration time, not after
	// the clear). Match the executable clear at statement indentation
	// (the phrase also appears in `//` comments explaining the
	// ordering, which must NOT be mistaken for the clear itself).
	logIdx := strings.Index(s, "logger.lifecycle(\"omac: retiring")
	clearIdx := strings.Index(s, "        task.actions = []")
	if logIdx < 0 {
		t.Errorf("retire script missing the configuration-time lifecycle log line:\n%s", s)
	}
	if clearIdx < 0 || (logIdx >= 0 && logIdx > clearIdx) {
		t.Errorf("retire script log line must appear before the executable task.actions = [] (it fires at config time, not after the clear):\n%s", s)
	}
	// The matching predicate must be exactly the yarp3 twin regex
	// /checkstyle.*Sandbox/ — no broader regex that could match the
	// canonical checkstyleMain/checkstyleTest tasks or unrelated tasks.
	// This guards against a future widening (e.g. /checkstyle.*/) that
	// would silently neutralize the canonical tasks the ticket preserves.
	for _, banned := range []string{
		"/checkstyle.*/",
		"/checkstyle/",
		"it.name == 'checkstyleMain'",
		"it.name == \"checkstyleMain\"",
		"it.name == 'checkstyleTest'",
		"it.name == \"checkstyleTest\"",
	} {
		if strings.Contains(s, banned) {
			t.Errorf("retire script must not contain a predicate that could match canonical/non-twin tasks (%q): %s", banned, s)
		}
	}
	// Determinism: re-rendering yields identical output.
	if s2 := RenderRetireCheckstyleTwinsInitScript(); s2 != s {
		t.Errorf("retire-checkstyle-twins init script is not deterministic across renders")
	}
}

// TestPrepareControlState_WritesRetireCheckstyleTwinsInitScript asserts the
// retirement init script is written UNCONDITIONALLY (not gated on private
// registries) and granted read-only to the executor. It must appear in the
// returned control files list so WriteDenyPaths protects it.
func TestPrepareControlState_WritesRetireCheckstyleTwinsInitScript(t *testing.T) {
	leaf := t.TempDir()
	// init.d is created read-only (0o500) by PrepareControlState, which
	// blocks t.TempDir's cleanup RemoveAll. Restore writability on cleanup.
	chmodInitDForCleanup(t, leaf)
	// No RegistryProxyURLs: the registry-credentials script is NOT
	// written, but the retire-checkstyle-twins script MUST be (it is
	// unconditional, applying to every build).
	paths, err := PrepareControlState(leaf, GradlePropertiesConfig{})
	if err != nil {
		t.Fatalf("PrepareControlState: %v", err)
	}
	initScript := filepath.Join(leaf, "init.d", retireCheckstyleTwinsInitName)
	data, err := os.ReadFile(initScript)
	if err != nil {
		t.Fatalf("retire-checkstyle-twins init script not written (it must be unconditional): %v", err)
	}
	body := string(data)
	if !strings.Contains(body, "beforeProject") {
		t.Errorf("retire-checkstyle-twins init script missing neutralization logic:\n%s", body)
	}
	// The retirement script must NOT contain any credential material
	// (it is unrelated to the credential-lift script).
	for _, banned := range []string{"alice", "s3cr3t", "password=", "user:pass"} {
		if strings.Contains(body, banned) {
			t.Errorf("retire-checkstyle-twins init script must not contain credential material %q:\n%s", banned, body)
		}
	}
	// The init script file is granted read-only: it appears in the
	// returned control files list (existence-filtered) AND its parent
	// init.d dir is in control dirs (read-only).
	found := false
	for _, p := range paths.Files {
		if strings.HasSuffix(p, retireCheckstyleTwinsInitName) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("retire-checkstyle-twins init script not in control files (read-only grant missing): %v", paths.Files)
	}
	// The registry-credentials script must NOT be present (no private
	// registries approved), confirming the retire script is written
	// independently of the registry path.
	if _, err := os.Stat(filepath.Join(leaf, "init.d", registryCredentialsInitName)); err == nil {
		t.Errorf("registry-credentials init script must NOT be written when no private registries are approved")
	}
}

// TestRenderMockitoAgentInitScript_NonEmpty asserts the mockito-agent
// init script is always emitted (the agent applies to every build — it is
// a defensive no-op when no test task uses Mockito).
func TestRenderMockitoAgentInitScript_NonEmpty(t *testing.T) {
	s := RenderMockitoAgentInitScript()
	if s == "" {
		t.Fatal("mockito-agent init script must always be non-empty (defensive no-op when no Mockito)")
	}
}

// TestRenderMockitoAgentInitScript_LocatesJarAndAddsJavaagent asserts the
// script mirrors the captured reference: it enables dynamic agent loading,
// locates the mockito-core jar on the test classpath at doFirst time, and
// adds it as a -javaagent. A missing jar is silently skipped.
func TestRenderMockitoAgentInitScript_LocatesJarAndAddsJavaagent(t *testing.T) {
	s := RenderMockitoAgentInitScript()
	for _, want := range []string{
		// Applies to every project's Test tasks.
		"allprojects",
		"tasks.withType(Test).configureEach",
		// Enables dynamic agent loading (the -javaagent attach is permitted).
		"-XX:+EnableDynamicAgentLoading",
		// Locates the mockito-core jar at doFirst time (classpath resolved).
		"doFirst",
		"mockito-core-.*\\.jar",
		// Adds the jar as a -javaagent.
		"-javaagent:",
		// Defensive skip when the jar is absent.
		"if (mockitoJar != null)",
		// Forces java.io.tmpdir to the executor's private temp, read from
		// a control-state file (preferred over the stale daemon env), with
		// a guard so a misconfigured env (empty $TMPDIR) can't blank the
		// JVM default.
		"executor-tmpdir",
		"System.getenv('TMPDIR')",
		"-Djava.io.tmpdir=",
		"if (omacTmp != null && !omacTmp.isEmpty())",
		// Read-only contract.
		"READ-ONLY to the executor",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("mockito-agent init script missing %q:\n%s", want, s)
		}
	}
	// Determinism: re-rendering yields identical output.
	if s2 := RenderMockitoAgentInitScript(); s2 != s {
		t.Errorf("mockito-agent init script is not deterministic across renders")
	}
}

// TestPrepareControlState_WritesMockitoAgentInitScript asserts the
// mockito-agent init script is written UNCONDITIONALLY (not gated on
// private registries or approved images) and granted read-only to the
// executor. It must appear in the returned control files list so
// WriteDenyPaths protects it.
func TestPrepareControlState_WritesMockitoAgentInitScript(t *testing.T) {
	leaf := t.TempDir()
	chmodInitDForCleanup(t, leaf)
	// No RegistryProxyURLs: the registry-credentials script is NOT
	// written, but the mockito-agent script MUST be (it is unconditional).
	paths, err := PrepareControlState(leaf, GradlePropertiesConfig{})
	if err != nil {
		t.Fatalf("PrepareControlState: %v", err)
	}
	initScript := filepath.Join(leaf, "init.d", mockitoAgentInitName)
	data, err := os.ReadFile(initScript)
	if err != nil {
		t.Fatalf("mockito-agent init script not written (it must be unconditional): %v", err)
	}
	body := string(data)
	if !strings.Contains(body, "mockito-core") {
		t.Errorf("mockito-agent init script missing jar-location logic:\n%s", body)
	}
	// The script must NOT contain any credential material.
	for _, banned := range []string{"alice", "s3cr3t", "password=", "user:pass"} {
		if strings.Contains(body, banned) {
			t.Errorf("mockito-agent init script must not contain credential material %q:\n%s", banned, body)
		}
	}
	// The init script file is granted read-only: it appears in the
	// returned control files list AND its parent init.d dir is in
	// control dirs (read-only).
	found := false
	for _, p := range paths.Files {
		if strings.HasSuffix(p, mockitoAgentInitName) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("mockito-agent init script not in control files (read-only grant missing): %v", paths.Files)
	}
}

// TestPrepareControlState_WritesExecutorTmpDir asserts the executor-tmpdir
// control file is written when GradlePropertiesConfig.TmpDir is set, and
// that it appears in the control files list (so the executor can read it
// for the java.io.tmpdir init-script logic). This file is the fix for the
// warm-daemon stale-TMPDIR bug: the init script reads the CURRENT run's
// temp from the file instead of the daemon's env (which holds a prior,
// deleted run's TMPDIR).
func TestPrepareControlState_WritesExecutorTmpDir(t *testing.T) {
	leaf := t.TempDir()
	chmodInitDForCleanup(t, leaf)
	wantTmp := "/tmp/omac-build-tmp/exec-42"
	paths, err := PrepareControlState(leaf, GradlePropertiesConfig{TmpDir: wantTmp})
	if err != nil {
		t.Fatalf("PrepareControlState: %v", err)
	}
	tmpFile := filepath.Join(leaf, controlStateName, executorTmpDirName)
	got, err := os.ReadFile(tmpFile)
	if err != nil {
		t.Fatalf("executor-tmpdir control file not written: %v", err)
	}
	if strings.TrimSpace(string(got)) != wantTmp {
		t.Errorf("executor-tmpdir content = %q, want %q", got, wantTmp)
	}
	// The file must be in the control files list (read-only grant for the
	// executor init script to read it).
	found := false
	for _, p := range paths.Files {
		if strings.HasSuffix(p, executorTmpDirName) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("executor-tmpdir not in control files (read-only grant missing): %v", paths.Files)
	}
}

// TestPrepareControlState_OmitsExecutorTmpDirWhenEmpty asserts the
// executor-tmpdir file is NOT written when TmpDir is empty (the
// cold-daemon/env-fallback path). The control files list still lists it
// (a missing file is harmless), but no file is written.
func TestPrepareControlState_OmitsExecutorTmpDirWhenEmpty(t *testing.T) {
	leaf := t.TempDir()
	chmodInitDForCleanup(t, leaf)
	if _, err := PrepareControlState(leaf, GradlePropertiesConfig{}); err != nil {
		t.Fatalf("PrepareControlState: %v", err)
	}
	tmpFile := filepath.Join(leaf, controlStateName, executorTmpDirName)
	if _, err := os.Stat(tmpFile); !os.IsNotExist(err) {
		t.Errorf("executor-tmpdir file should not exist when TmpDir is empty: %v", err)
	}
}
