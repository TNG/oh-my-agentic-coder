package sandboxrun

import (
	"strings"
	"testing"

	"github.com/tngtech/oh-my-agentic-coder/internal/sandboxprofile"
)

// TestProxyInjectorsCoverProfileTools guards against drift: every family
// accepted by the profile validator must have an injector, and vice versa.
func TestProxyInjectorsCoverProfileTools(t *testing.T) {
	for _, tool := range sandboxprofile.ProxyInjectionTools() {
		if _, ok := proxyInjectors[tool]; !ok {
			t.Errorf("profile accepts %q but no injector is registered", tool)
		}
	}
	for tool := range proxyInjectors {
		if !contains(sandboxprofile.ProxyInjectionTools(), tool) {
			t.Errorf("injector registered for %q but profile validation rejects it", tool)
		}
	}
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func TestProxyInjectionEnv(t *testing.T) {
	env, err := ProxyInjectionEnv(
		[]string{sandboxprofile.ProxyInjectJVM, sandboxprofile.ProxyInjectNode},
		"http://omac:sekret@127.0.0.1:40981",
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(env["JAVA_TOOL_OPTIONS"], "-Dhttps.proxyHost=127.0.0.1") {
		t.Errorf("JAVA_TOOL_OPTIONS not set for jvm family: %q", env["JAVA_TOOL_OPTIONS"])
	}
	if env["NODE_USE_ENV_PROXY"] != "1" {
		t.Errorf("NODE_USE_ENV_PROXY = %q, want 1", env["NODE_USE_ENV_PROXY"])
	}
}

func TestProxyInjectionEnv_UnknownFamily(t *testing.T) {
	if _, err := ProxyInjectionEnv([]string{"python"}, "http://127.0.0.1:8080"); err == nil {
		t.Error("expected error for unknown family, got nil")
	}
}

func TestJVMProxyToolOptions(t *testing.T) {
	got, err := JVMProxyToolOptions("http://omac:sekret@127.0.0.1:40981")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{
		"-Dhttp.proxyHost=127.0.0.1",
		"-Dhttp.proxyPort=40981",
		"-Dhttps.proxyHost=127.0.0.1",
		"-Dhttps.proxyPort=40981",
		"-Dhttps.proxyUser=omac",
		"-Dhttps.proxyPassword=sekret",
		"-Dhttp.proxyUser=omac",
		"-Dhttp.proxyPassword=sekret",
		"-Dhttp.nonProxyHosts=localhost|127.*|[::1]",
		"-Djdk.http.auth.tunneling.disabledSchemes=",
	}
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("JAVA_TOOL_OPTIONS missing %q\n---\n%s", w, got)
		}
	}
}

func TestJVMProxyToolOptions_NoCredentials(t *testing.T) {
	got, err := JVMProxyToolOptions("http://127.0.0.1:8080")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(got, "proxyUser") || strings.Contains(got, "proxyPassword") {
		t.Errorf("expected no auth options without credentials, got:\n%s", got)
	}
	if !strings.Contains(got, "-Dhttps.proxyHost=127.0.0.1") {
		t.Errorf("expected proxyHost option, got:\n%s", got)
	}
}

func TestJVMProxyToolOptions_Invalid(t *testing.T) {
	for _, bad := range []string{"://nope", "http://noport", "not a url with spaces"} {
		if _, err := JVMProxyToolOptions(bad); err == nil {
			t.Errorf("expected error for %q, got nil", bad)
		}
	}
}
