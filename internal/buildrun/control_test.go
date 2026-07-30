package buildrun

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
	// Returned control files: gradle.properties + README (2).
	if len(paths.Files) != 2 {
		t.Fatalf("got %d control file paths, want 2: %v", len(paths.Files), paths.Files)
	}
	// Returned control dirs: init.d (1).
	if len(paths.Dirs) != 1 || filepath.Base(paths.Dirs[0]) != "init.d" {
		t.Fatalf("got control dirs %v, want 1 entry: init.d", paths.Dirs)
	}
}

func TestPrepareControlState_InitDReadOnlyToExecutor(t *testing.T) {
	leaf := t.TempDir()
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
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(leaf, "init.d"), 0o755) })
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
