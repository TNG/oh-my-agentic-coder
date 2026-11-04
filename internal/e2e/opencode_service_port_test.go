//go:build e2e || e2e_fast

package e2e

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestE2EOpenCodeV2RefusesTakenServicePort drives the real `omac start` with a
// fake OpenCode v2 binary and a pinned service port that is not usable (#345).
// omac must refuse before launching the inner command, exit non-zero, and say
// which port and why. Before the fix, OpenCode itself failed ~15-20 s later
// with "Managed service port … in use".
//
// The refusal happens before any sandbox is applied, so this needs no
// OpenCode install, no model and no working sandbox backend: model-free
// e2e_fast slice. OMAC_TEST_OPENCODE_SERVICE_PORT forces the port pick; the
// forced port goes through the same checks as a picked one.
func TestE2EOpenCodeV2RefusesTakenServicePort(t *testing.T) {
	if nestedInOmacSandbox() {
		t.Skip("nested runs use --no-sandbox, which has no service pin")
	}
	omacBin := buildOmac(t)

	cases := []struct {
		name string
		// setup prepares the scratch HOME and returns the port to force and the expected message fragments.
		setup func(t *testing.T, home string) (int, []string)
	}{
		{"port held by another process", func(t *testing.T, home string) (int, []string) {
			l, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = l.Close() })
			port := l.Addr().(*net.TCPAddr).Port
			return port, []string{fmt.Sprintf("port %d", port), "already in use by another process", "run the command again"}
		}},
		{"port registered by the host OpenCode service", func(t *testing.T, home string) (int, []string) {
			l, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			port := l.Addr().(*net.TCPAddr).Port
			_ = l.Close()
			reg := filepath.Join(home, ".local", "state", "opencode", "service.json")
			if err := os.MkdirAll(filepath.Dir(reg), 0o700); err != nil {
				t.Fatal(err)
			}
			body := fmt.Sprintf(`{"url":"http://127.0.0.1:%d","pid":4242,"password":"x"}`, port)
			if err := os.WriteFile(reg, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			return port, []string{fmt.Sprintf("port %d", port), "registered by the host OpenCode shared service", "pid 4242"}
		}},
		{"OpenCode's default service port", func(t *testing.T, home string) (int, []string) {
			return 49374, []string{"port 49374", "default shared-service port"}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Short base: on Linux the facade socket lives under XDG_STATE_HOME and
			// t.TempDir() (named after this test) exceeds the unix-socket path limit.
			base, err := os.MkdirTemp("", "ocp")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(base) })
			home := filepath.Join(base, "home")
			work := filepath.Join(base, "work")
			bin := filepath.Join(base, "bin")
			for _, d := range []string{home, work, bin} {
				if err := os.MkdirAll(d, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			// Fake OpenCode v2: answers --version, records any real launch.
			marker := filepath.Join(base, "inner-ran")
			fake := filepath.Join(bin, "opencode")
			script := "#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo 2.0.26; exit 0; fi\ntouch " + strconv.Quote(marker) + "\n"
			if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}

			port, want := tc.setup(t, home)

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			cmd := exec.CommandContext(ctx, omacBin, "start", "opencode", "--inner", fake, "--", "api", "get", "/api/info")
			cmd.Dir = work
			env := os.Environ()
			for k, v := range map[string]string{
				"HOME":                            home,
				"XDG_CONFIG_HOME":                 filepath.Join(home, ".config"),
				"XDG_DATA_HOME":                   filepath.Join(home, ".local", "share"),
				"XDG_STATE_HOME":                  filepath.Join(home, ".local", "state"),
				"XDG_CACHE_HOME":                  filepath.Join(home, ".cache"),
				"OPENCODE_CONFIG_DIR":             "",
				"OMAC_TEST_OPENCODE_SERVICE_PORT": strconv.Itoa(port),
			} {
				env = withEnv(env, k, v)
			}
			cmd.Env = env
			out, err := cmd.CombinedOutput()

			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 7 {
				t.Fatalf("omac start: want exit 7 (refused), got %v\n%s", err, out)
			}
			for _, w := range want {
				if !strings.Contains(string(out), w) {
					t.Errorf("output lacks %q:\n%s", w, out)
				}
			}
			if _, err := os.Stat(marker); err == nil {
				t.Error("the inner command ran although the service port was refused")
			}
		})
	}
}
