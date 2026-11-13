package sandboxrun

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

func TestServeAuthRuntimeRejectsChangedPolicy(t *testing.T) {
	t.Setenv("OMAC_SERVE_AUTH_TARGET", "OPENCODE_SERVER_PASSWORD")
	t.Setenv("OPENCODE_SERVER_PASSWORD", "dedicated-secret")
	t.Setenv("OMAC_CACHE_DIR", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	profiles := filepath.Join(home, ".config", "omac", "sandbox-profiles")
	if err := os.MkdirAll(profiles, 0700); err != nil {
		t.Fatal(err)
	}
	for _, deny := range []string{"OPENCODE_SERVER_PASSWORD", "OPENCODE_*", "*"} {
		t.Run(deny, func(t *testing.T) {
			path := filepath.Join(profiles, "runtime.json")
			data := fmt.Sprintf(`{"meta":{"name":"runtime"},"environment":{"allow_vars":["*"],"deny_vars":[%q]},"network":{"mode":"blocked"}}`, deny)
			if err := os.WriteFile(path, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			var stderr bytes.Buffer
			code := Run(Options{Flags: &sandboxprofile.Flags{ProfileRef: path, AllowEnv: []string{"OPENCODE_SERVER_PASSWORD"}, InnerArgv: []string{"/usr/bin/false"}}, Workdir: t.TempDir(), Stderr: &stderr})
			if code == 0 || !strings.Contains(stderr.String(), "OMAC_SERVE_PASSWORD") || !strings.Contains(stderr.String(), "deny_vars") {
				t.Fatalf("final runtime policy must reject dedicated auth before launch; exit=%d, stderr=%s", code, stderr.String())
			}
			if strings.Contains(stderr.String(), "dedicated-secret") {
				t.Error("runtime diagnostic leaked the password")
			}
		})
	}
}

func TestServeAuthFinalEnvironment(t *testing.T) {
	environ := []string{
		"OMAC_SERVE_AUTH_TARGET=OPENCODE_SERVER_PASSWORD",
		"OMAC_SERVE_PASSWORD=source-secret",
		"OPENCODE_SERVER_PASSWORD=dedicated-secret",
	}
	for _, tc := range []struct {
		name        string
		allow, deny []string
		injected    map[string]string
		wantErr     bool
	}{
		{name: "target allowed marker stripped", allow: []string{"OPENCODE_SERVER_PASSWORD"}},
		{name: "OMAC marker deny does not hide requirement", allow: []string{"OPENCODE_SERVER_PASSWORD"}, deny: []string{"OMAC_*"}},
		{name: "target not allowed", allow: []string{"HOME"}, wantErr: true},
		{name: "exact deny beats injection", allow: []string{"*"}, deny: []string{"OPENCODE_SERVER_PASSWORD"}, injected: map[string]string{"OPENCODE_SERVER_PASSWORD": "dedicated-secret"}, wantErr: true},
		{name: "prefix deny beats injection", allow: []string{"*"}, deny: []string{"OPENCODE_*"}, injected: map[string]string{"OPENCODE_SERVER_PASSWORD": "dedicated-secret"}, wantErr: true},
		{name: "wildcard deny beats injection", allow: []string{"*"}, deny: []string{"*"}, injected: map[string]string{"OPENCODE_SERVER_PASSWORD": "dedicated-secret"}, wantErr: true},
		{name: "injected replacement rejected", allow: []string{"*"}, injected: map[string]string{"OPENCODE_SERVER_PASSWORD": "replacement-secret"}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, err := filterServeAuthEnv(environ, sandboxprofile.Environment{AllowVars: tc.allow, DenyVars: tc.deny}, tc.injected)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, want error %v", err, tc.wantErr)
			}
			if err != nil {
				if strings.Contains(err.Error(), "dedicated-secret") || strings.Contains(err.Error(), "replacement-secret") {
					t.Error("filter error leaked a password")
				}
				return
			}
			if len(env) != 1 || env[0] != "OPENCODE_SERVER_PASSWORD=dedicated-secret" {
				t.Error("final environment must contain only the intended target")
			}
		})
	}
	// An unset target must not turn an explicit request into normal fallback.
	if _, err := filterServeAuthEnv(environ[:2], sandboxprofile.Environment{AllowVars: []string{"*"}}, nil); err == nil {
		t.Error("missing explicit target must fail closed")
	}
	// No marker means ordinary harness filtering, not an auth requirement.
	if env, err := filterServeAuthEnv(environ[2:], sandboxprofile.Environment{AllowVars: []string{"HOME"}}, nil); err != nil || len(env) != 0 {
		t.Fatalf("normal filtering changed: %v", err)
	}
}
