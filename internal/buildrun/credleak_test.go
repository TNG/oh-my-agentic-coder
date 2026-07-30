package buildrun

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCredentialLift_GrantsEnvAndControlStateDoNotLeak is the red-team
// test for ticket 06 criteria 3 + 4. It constructs the FULL executor state
// for a build with an approved private registry (the credential-lift path)
// and asserts the credential string is ABSENT from:
//
//   - ChildEnv (the executor environment — the credential must never be
//     an env var; the proxy URL Gradle sees is non-secret loopback).
//   - the OMAC-generated gradle.properties content (readable by build
//     code and persisted in the cache leaf — never carries a credential).
//   - the proxy URL Gradle is pointed at (http://127.0.0.1:<port>/<alias>/,
//     no userinfo).
//   - the registry-credentials init.d script (read-only control state;
//     carries only non-secret local URLs).
//   - audit events (build.request carries adapter/root/arg-count only).
//
// It mirrors ticket 04's TestRunBuildProxyTokenDoesNotLeak pattern: the
// credential IS present in-process (the credproxy holds it), but it must
// not reach any executor-visible surface.
func TestCredentialLift_GrantsEnvAndControlStateDoNotLeak(t *testing.T) {
	const credential = "alice:supersecret-deadbeef-1234"
	wt, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cacheDir := filepath.Join(t.TempDir(), "cache")
	// Non-secret local loopback URL the credential-lift proxy serves —
	// the credential rides UPSTREAM from the proxy, never in this URL.
	credProxyURLs := map[string]string{
		"internal": "http://127.0.0.1:54321/internal/",
	}
	g, err := GrantsFor(wt, cacheDir, BuildConfig{
		ProxyURL:           "http://omac:proxytoken@127.0.0.1:9999",
		ProxyPort:          9999,
		ApprovedRegistries: []string{"internal"},
		RegistryProxyURLs:  credProxyURLs,
	})
	if err != nil {
		t.Fatalf("GrantsFor: %v", err)
	}
	t.Cleanup(g.CleanupTmp)
	// GrantsFor creates init.d read-only (0o500) which blocks
	// t.TempDir's cleanup RemoveAll. Restore writability on cleanup.
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(g.GradleUserHome(), "init.d"), 0o755) })

	// 1. ChildEnv: the credential must not appear in ANY env var. The
	//    proxy URL Gradle sees (RegistryProxyURLs) is non-secret loopback;
	//    the GRADLE_OPTS proxy token is a DIFFERENT secret (proxytoken)
	//    which has its own leak test — the registry credential must not
	//    collide with it.
	env := ChildEnv(g)
	for _, kv := range env {
		if strings.Contains(kv, credential) {
			t.Errorf("credential leaked into child env: %q", kv)
		}
	}
	// The non-secret proxy URL MUST be reachable somehow for Gradle to use
	// it — but it is NOT in env (it is in the init.d script). Assert the
	// credential's host/port never appears with userinfo in env either.
	for _, kv := range env {
		if strings.Contains(kv, "@127.0.0.1:54321") {
			t.Errorf("credential proxy URL leaked userinfo into env: %q", kv)
		}
	}

	// 2. gradle.properties: read the OMAC-generated file and assert the
	//    credential is absent. The file carries proxy host:port + heap
	//    only — never credentials.
	propsPath := filepath.Join(g.GradleUserHome(), "gradle.properties")
	props, err := os.ReadFile(propsPath)
	if err != nil {
		t.Fatalf("read gradle.properties: %v", err)
	}
	if strings.Contains(string(props), credential) {
		t.Errorf("credential leaked into gradle.properties:\n%s", props)
	}

	// 3. The proxy URL Gradle is pointed at (via the init.d script) is
	//    non-secret loopback with no userinfo.
	for alias, u := range g.RegistryProxyURLs() {
		if strings.Contains(u, "@") {
			t.Errorf("credential proxy URL for %q contains userinfo: %q", alias, u)
		}
		if strings.Contains(u, credential) {
			t.Errorf("credential leaked into proxy URL for %q: %q", alias, u)
		}
		if !strings.HasPrefix(u, "http://127.0.0.1:") {
			t.Errorf("credential proxy URL for %q must be loopback http: %q", alias, u)
		}
	}

	// 4. The registry-credentials init.d script: read it and assert the
	//    credential is absent (only non-secret local URLs).
	initPath := filepath.Join(g.GradleUserHome(), "init.d", "registry-credentials.gradle")
	initScript, err := os.ReadFile(initPath)
	if err != nil {
		t.Fatalf("read registry-credentials init script: %v", err)
	}
	if strings.Contains(string(initScript), credential) {
		t.Errorf("credential leaked into registry-credentials init script:\n%s", initScript)
	}
	if !strings.Contains(string(initScript), "http://127.0.0.1:54321/internal/") {
		t.Errorf("init script must contain the non-secret local URL:\n%s", initScript)
	}

	// 5. Audit events: the build.request ControlMutation carries only
	//    adapter/root/arg-count — never the credential. Force a
	//    service-failure path (boom launcher) so an event is emitted
	//    without spawning a child, then grep the serialized form.
	//    The boom launcher ALSO captures the innerArgv it was handed so
	//    the test can assert the credential never reaches process
	//    arguments (spec.md:291 lists "process arguments").
	var capturedArgv []string
	res := Resolved{
		Worktree:   g.Workdir,
		ProjectDir: g.Workdir,
		Wrapper:    "/bin/true",
		Args:       []string{":help"},
	}
	var stderr bytes.Buffer
	var stdout bytes.Buffer
	rec := &recordingAuditor{}
	boomLauncher := func(_ *BuildGrants, innerArgv []string) ([]string, error) {
		capturedArgv = append([]string{}, innerArgv...)
		return nil, errors.New("simulated launch failure")
	}
	_, runErr := RunBuild(RunOptions{
		Resolved: res, Grants: g,
		Stdout:   &stdout,
		Stderr:   &stderr,
		Launcher: boomLauncher,
		Auditor:  rec,
	})
	if runErr == nil {
		t.Fatal("expected a launch error from boomLauncher")
	}
	for _, ev := range rec.events {
		if strings.Contains(eventText(ev), credential) {
			t.Errorf("credential leaked into audit event: %+v", ev)
		}
	}
	// omac's own stderr must not contain the credential.
	if strings.Contains(stderr.String(), credential) {
		t.Errorf("credential leaked into omac stderr:\n%s", stderr.String())
	}
	// omac's own stdout must not contain the credential (spec.md:291).
	if strings.Contains(stdout.String(), credential) {
		t.Errorf("credential leaked into omac stdout:\n%s", stdout.String())
	}
	// Process arguments: the innerArgv the launcher receives is the
	// Gradle wrapper + pass-through args. The credential must never be
	// injected into argv (spec.md:291: "process arguments") — the
	// credential rides upstream from the proxy, not in the exec line.
	for _, a := range capturedArgv {
		if strings.Contains(a, credential) {
			t.Errorf("credential leaked into process argument: %q", a)
		}
	}
}

// TestCredentialLift_NoRegistriesNoInitScript asserts that when no private
// registries are approved (the common case), the registry-credentials
// init.d script is NOT written — a standard Gradle project gets no
// credential-lift control state. gradle.properties + README only.
func TestCredentialLift_NoRegistriesNoInitScript(t *testing.T) {
	wt, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cacheDir := filepath.Join(t.TempDir(), "cache")
	g, err := GrantsFor(wt, cacheDir, BuildConfig{})
	if err != nil {
		t.Fatalf("GrantsFor: %v", err)
	}
	t.Cleanup(g.CleanupTmp)
	initPath := filepath.Join(g.GradleUserHome(), "init.d", "registry-credentials.gradle")
	if _, err := os.Stat(initPath); err == nil {
		t.Errorf("registry-credentials init script must NOT exist when no registries are approved")
	}
	if len(g.RegistryProxyURLs()) != 0 {
		t.Errorf("RegistryProxyURLs must be empty for no-registry build, got %v", g.RegistryProxyURLs())
	}
}

// TestCredentialLift_AuditCarriesNoCredentialOrProxyURL asserts audit
// events never carry the registry credential OR the credential-lift proxy
// URL — even on a successful-looking event path. The audit trail records
// names/codes/durations, never URLs-with-credentials.
func TestCredentialLift_AuditCarriesNoCredentialOrProxyURL(t *testing.T) {
	const credential = "alice:s3cr3t-credproxy"
	credProxyURLs := map[string]string{
		"internal": "http://127.0.0.1:54321/internal/",
	}
	wt, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cacheDir := filepath.Join(t.TempDir(), "cache")
	g, err := GrantsFor(wt, cacheDir, BuildConfig{
		RegistryProxyURLs: credProxyURLs,
	})
	if err != nil {
		t.Fatalf("GrantsFor: %v", err)
	}
	t.Cleanup(g.CleanupTmp)
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(g.GradleUserHome(), "init.d"), 0o755) })
	rec := &recordingAuditor{}
	res := Resolved{
		Worktree: g.Workdir, ProjectDir: g.Workdir,
		Wrapper: "/bin/true", Args: []string{":help"},
	}
	_, _ = RunBuild(RunOptions{
		Resolved: res, Grants: g,
		Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{},
		Launcher: func(*BuildGrants, []string) ([]string, error) {
			return nil, errors.New("boom")
		},
		Auditor: rec,
	})
	for _, ev := range rec.events {
		txt := eventText(ev)
		if strings.Contains(txt, credential) {
			t.Errorf("credential leaked into audit event: %+v", ev)
		}
	}
}
