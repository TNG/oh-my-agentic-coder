package sandboxrun

import (
	"strings"
	"testing"
)

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
		"-Dhttps.nonProxyHosts=localhost|127.*|[::1]",
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
