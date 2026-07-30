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
