//go:build e2e && opencode_v2_repro

package e2e

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
	"github.com/TNG/oh-my-agentic-coder/internal/sandboxrun"
)

func openCodeRestrictedHome(t *testing.T) (string, string) {
	t.Helper()
	hostHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	home, err := os.MkdirTemp(hostHome, ".omac-oc2-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(home); err != nil {
			t.Error(err)
		}
	})
	workdir := filepath.Join(home, "Documents", "git", "project")
	if err := os.MkdirAll(workdir, 0o700); err != nil {
		t.Fatal(err)
	}
	return home, workdir
}

func requireOpenCodeV2Host(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "darwin" || nestedInOmacSandbox() {
		t.Skip("requires a genuine macOS host outside omac; no nested or --no-sandbox substitute")
	}
}

func TestOpenCodeRestrictedHomeIsolation(t *testing.T) {
	requireOpenCodeV2Host(t)
	home, workdir := openCodeRestrictedHome(t)
	checkOpenCodeV2Fixture(t, home)
	proveOpenCodeV2FixtureIsolation(t, home, workdir)
}

func proveOpenCodeV2FixtureIsolation(t *testing.T, home, workdir string) {
	t.Helper()
	sibling := filepath.Join(home, "sibling.txt")
	if err := os.WriteFile(sibling, []byte("outside-workdir"), 0o600); err != nil {
		t.Fatal(err)
	}
	g, err := sandboxrun.ResolveGrants(&sandboxprofile.Profile{
		Workdir: sandboxprofile.Workdir{Access: sandboxprofile.AccessReadWrite},
		Network: sandboxprofile.Network{Mode: sandboxprofile.ModeBlocked},
	}, workdir, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		argv   []string
		wantOK bool
	}{
		{"allowed-read-write", []string{"/bin/sh", "-c", `printf fixture-ok > probe && /bin/cat probe`}, true},
		{"unprotected-sibling-denied", []string{"/bin/cat", sibling}, false},
	} {
		argv, err := sandboxrun.BuildChildArgv(g, tc.argv)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		cmd.Dir = workdir
		cmd.Env = withEnv(os.Environ(), "HOME", home)
		cmd.WaitDelay = time.Second
		out, err := cmd.CombinedOutput()
		contextErr := ctx.Err()
		cancel()
		t.Logf("fixture production-sandbox probe %s: exit=%v output=%s grants=%+v", tc.name, err, out, g)
		if contextErr != nil {
			t.Fatalf("fixture probe timed out: %v\n%s", contextErr, out)
		}
		if err != nil {
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() < 0 {
				t.Fatalf("fixture probe did not execute normally: %v\n%s", err, out)
			}
		}
		if (err == nil) != tc.wantOK || (tc.wantOK && string(out) != "fixture-ok") {
			t.Fatalf("fixture premise failed for %s: %v\n%s", tc.name, err, out)
		}
	}
}

func checkOpenCodeV2Fixture(t *testing.T, home string) {
	t.Helper()
	canonical, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	base := sandboxprofile.PlatformBaseline()
	roots := append([]string{"/tmp", "/private/tmp", "/var/folders", "/private/var/folders"}, base.Read...)
	roots = append(roots, base.Write...)
	for _, raw := range roots {
		raw = strings.Replace(raw, "~/", home+"/", 1)
		root, err := sandboxprofile.ExpandPath(raw)
		if err != nil {
			if errors.Is(err, sandboxprofile.ErrEmptyExpansion) {
				continue
			}
			t.Fatal(err)
		}
		if resolved, err := filepath.EvalSymlinks(root); err == nil {
			root = resolved
		}
		if rel, err := filepath.Rel(root, canonical); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			t.Fatalf("restricted fixture %s covered by baseline/temp grant %s", canonical, root)
		}
	}
	t.Logf("restricted fixture HOME=%s canonical=%s baseline=%+v; directory location is static evidence only", home, canonical, base)
}
