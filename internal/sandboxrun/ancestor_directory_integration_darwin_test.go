//go:build darwin

package sandboxrun

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

func TestIntegrationAncestorDirectoryData(t *testing.T) {
	if os.Getenv("OMAC_SOCKET") != "" || os.Getenv("E2E_NESTED") == "1" {
		t.Skip("requires a macOS host outside omac; nested Seatbelt is not reproduction evidence")
	}
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
	if err := os.Mkdir(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(home, "sibling.txt"), filepath.Join(home, ".ssh", "key")} {
		if err := os.WriteFile(path, []byte("outside-workdir"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(home, "sibling.txt"), filepath.Join(workdir, "escape")); err != nil {
		t.Fatal(err)
	}
	base := sandboxprofile.PlatformBaseline()
	read, err := sandboxprofile.ExpandExisting(base.Read, nil)
	if err != nil {
		t.Fatal(err)
	}
	write, err := sandboxprofile.ExpandExisting(base.Write, nil)
	if err != nil {
		t.Fatal(err)
	}
	g := &Grants{
		Workdir: workdir, ReadPaths: read, WritePaths: write,
		AllowPaths: []string{workdir}, ProtectedPaths: []string{filepath.Join(home, ".ssh")},
		NetworkMode: sandboxprofile.ModeBlocked,
	}
	canonical, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	roots := append([]string{"/tmp", "/private/tmp", "/var/folders", "/private/var/folders"}, read...)
	roots = append(roots, write...)
	for _, root := range roots {
		for _, form := range pathForms(root) {
			if rel, err := filepath.Rel(form, canonical); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				t.Fatalf("fixture %s is covered by baseline/temp grant %s", canonical, form)
			}
		}
	}
	t.Logf("HOME=%s canonical=%s workdir=%s grants=%+v\nSBPL:\n%s", home, canonical, workdir, g, GenerateSBPL(g))
	cases := []struct {
		name   string
		argv   []string
		wantOK bool
	}{
		{"chdir", []string{"/bin/sh", "-c", `cd "$HOME"`}, true},
		{"getcwd", []string{"/bin/sh", "-c", `cd "$HOME" && /bin/pwd -P`}, true},
		{"list", []string{"/bin/ls", home}, true},
		{"sibling-file", []string{"/bin/cat", filepath.Join(home, "sibling.txt")}, false},
		{"write-sibling", []string{"/bin/sh", "-c", `printf x > "$HOME/new-file"`}, false},
		{"protected", []string{"/bin/ls", filepath.Join(home, ".ssh")}, false},
		{"symlink-escape", []string{"/bin/cat", filepath.Join(workdir, "escape")}, false},
		{"workdir-read-write", []string{"/bin/sh", "-c", `printf inside-workdir > probe && /bin/cat probe`}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			full, err := BuildChildArgv(g, tc.argv)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, full[0], full[1:]...)
			cmd.Dir = workdir
			for _, kv := range os.Environ() {
				if !strings.HasPrefix(kv, "HOME=") {
					cmd.Env = append(cmd.Env, kv)
				}
			}
			cmd.Env = append(cmd.Env, "HOME="+home)
			cmd.WaitDelay = time.Second
			out, err := cmd.CombinedOutput()
			code := 0
			if err != nil {
				if ee, ok := err.(*exec.ExitError); ok {
					code = ee.ExitCode()
				} else {
					t.Fatalf("exec: %v\nstdout/stderr:\n%s", err, out)
				}
			}
			t.Logf("argv=%q exit=%d stdout/stderr:\n%s", tc.argv, code, out)
			if ctx.Err() != nil {
				t.Fatalf("probe timed out: %v", ctx.Err())
			}
			if code < 0 {
				t.Fatalf("signal termination is not a permission-denial result: exit=%d\n%s", code, out)
			}
			if (code == 0) != tc.wantOK {
				t.Errorf("exit=%d wantOK=%v\nstdout/stderr:\n%s", code, tc.wantOK, out)
			}
			if tc.name == "workdir-read-write" && string(out) != "inside-workdir" {
				t.Errorf("allowed read/write did not round-trip: %q", out)
			}
		})
	}
}
