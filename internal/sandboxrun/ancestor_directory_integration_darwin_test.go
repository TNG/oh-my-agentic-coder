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

// Programs work inside the project while no directory above it is listable.
func TestIntegrationAncestorDirectoriesNotListable(t *testing.T) {
	for _, layout := range []string{"inside-home", "outside-home", "symlinked-ancestor"} {
		t.Run(layout, func(t *testing.T) {
			testIntegrationAncestorDirectoriesNotListable(t, layout)
		})
	}
}

func testIntegrationAncestorDirectoriesNotListable(t *testing.T, layout string) {
	t.Helper()
	if os.Getenv("OMAC_SOCKET") != "" || os.Getenv("E2E_NESTED") == "1" {
		t.Skip("requires a macOS host outside omac; nested Seatbelt is not reproduction evidence")
	}
	hostHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	// Not in a temp directory: the baseline grants /tmp and /var/folders,
	// so a denied listing there would prove nothing.
	root, err := os.MkdirTemp(hostHome, ".omac-oc2-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Error(err)
		}
	})
	home := filepath.Join(root, "home")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	workdirOnDisk := filepath.Join(root, "a", "b", "project")
	if layout == "inside-home" {
		workdirOnDisk = filepath.Join(home, "Documents", "git", "project")
	}
	if err := os.MkdirAll(workdirOnDisk, 0o700); err != nil {
		t.Fatal(err)
	}
	workdir := workdirOnDisk
	if layout == "symlinked-ancestor" {
		if err := os.Symlink(filepath.Join(root, "a"), filepath.Join(root, "link")); err != nil {
			t.Fatal(err)
		}
		workdir = filepath.Join(root, "link", "b", "project")
	}
	if err := os.Mkdir(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(home, "ungranted"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(home, "sibling.txt"), filepath.Join(home, ".ssh", "key"), filepath.Join(home, "ungranted", "file")} {
		if err := os.WriteFile(path, []byte("outside-workdir"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(home, "sibling.txt"), filepath.Join(workdirOnDisk, "escape")); err != nil {
		t.Fatal(err)
	}
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	canonicalWorkdir, err := filepath.EvalSymlinks(workdir)
	if err != nil {
		t.Fatal(err)
	}
	// Every directory between the project and the fixture root gets a
	// foreign file, so each one has an entry name to hide and content to deny.
	var between []string
	for dir := filepath.Dir(canonicalWorkdir); ; dir = filepath.Dir(dir) {
		between = append(between, dir)
		if err := os.WriteFile(filepath.Join(dir, "foreign.txt"), []byte("outside-workdir"), 0o600); err != nil {
			t.Fatal(err)
		}
		if dir == canonicalRoot {
			break
		}
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
	roots := append([]string{"/tmp", "/private/tmp", "/var/folders", "/private/var/folders"}, read...)
	roots = append(roots, write...)
	for _, root := range roots {
		for _, form := range pathForms(root) {
			if rel, err := filepath.Rel(form, canonicalRoot); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				t.Fatalf("fixture %s is covered by baseline/temp grant %s", canonicalRoot, form)
			}
		}
	}
	t.Logf("HOME=%s workdir=%s", home, workdir)
	type probe struct {
		name   string
		argv   []string
		wantOK bool
		want   string // exact trimmed output, when set
	}
	cases := []probe{
		{name: "chdir-home", argv: []string{"/bin/sh", "-c", `cd "$HOME"`}, wantOK: true},
		// getcwd needs a data read on the directory or, as fallback, on its
		// parent to look up its name. HOME has neither, so only the shell
		// builtin, which trusts $PWD, works there.
		{name: "getcwd-home", argv: []string{"/bin/sh", "-c", `cd "$HOME" && /bin/pwd -P`}, wantOK: false},
		{name: "getcwd-home-builtin", argv: []string{"/bin/sh", "-c", `cd "$HOME" && pwd`}, wantOK: true, want: home},
		{name: "getcwd", argv: []string{"/bin/pwd", "-P"}, wantOK: true, want: canonicalWorkdir},
		{name: "getcwd-logical", argv: []string{"/bin/sh", "-c", `/bin/pwd -L`}, wantOK: true},
		{name: "realpath", argv: []string{"/bin/realpath", "."}, wantOK: true, want: canonicalWorkdir},
		{name: "realpath-workdir-path", argv: []string{"/bin/realpath", workdir}, wantOK: true, want: canonicalWorkdir},
		{name: "zsh-start", argv: []string{"/bin/zsh", "-c", `pwd -P`}, wantOK: true, want: canonicalWorkdir},
		{name: "bash-login-start", argv: []string{"/bin/bash", "-lc", `pwd -P`}, wantOK: true, want: canonicalWorkdir},
		{name: "workdir-read-write", argv: []string{"/bin/sh", "-c", `printf inside-workdir > probe && /bin/cat probe`}, wantOK: true, want: "inside-workdir"},
		{name: "list-workdir", argv: []string{"/bin/ls", "."}, wantOK: true},
		{name: "list-home", argv: []string{"/bin/ls", home}, wantOK: false},
		{name: "sibling-file", argv: []string{"/bin/cat", filepath.Join(home, "sibling.txt")}, wantOK: false},
		{name: "write-sibling", argv: []string{"/bin/sh", "-c", `printf x > "$HOME/new-file"`}, wantOK: false},
		{name: "protected", argv: []string{"/bin/ls", filepath.Join(home, ".ssh")}, wantOK: false},
		{name: "ungranted-directory", argv: []string{"/bin/ls", filepath.Join(home, "ungranted")}, wantOK: false},
		{name: "symlink-escape", argv: []string{"/bin/cat", filepath.Join(workdir, "escape")}, wantOK: false},
	}
	for _, dir := range between {
		rel, err := filepath.Rel(canonicalRoot, dir)
		if err != nil {
			t.Fatal(err)
		}
		cases = append(cases,
			probe{name: "list-ancestor-" + rel, argv: []string{"/bin/ls", dir}, wantOK: false},
			probe{name: "foreign-file-" + rel, argv: []string{"/bin/cat", filepath.Join(dir, "foreign.txt")}, wantOK: false},
			probe{name: "write-ancestor-" + rel, argv: []string{"/bin/sh", "-c", `printf x > "$0/new-file"`, dir}, wantOK: false},
		)
	}
	// OpenCode is a Bun-compiled binary; BUN_BE_BUN=1 makes it act as the
	// bun CLI, so its embedded runtime can run a script.
	if bun := bunBinary(); bun != "" {
		script := `const fs = require("fs"); console.log(fs.realpathSync(".")); console.log(fs.realpathSync.native(process.cwd()))`
		cases = append(cases,
			probe{name: "bun-realpath", argv: []string{"/usr/bin/env", "BUN_BE_BUN=1", bun, "-e", script}, wantOK: true, want: canonicalWorkdir + "\n" + canonicalWorkdir},
			probe{name: "bun-list-parent", argv: []string{"/usr/bin/env", "BUN_BE_BUN=1", bun, "-e", `require("fs").readdirSync("..")`}, wantOK: false},
		)
	} else {
		t.Log("bun or opencode not on PATH: Bun realpath cases skipped")
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			full, err := BuildChildArgv(g, tc.argv)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, full[0], full[1:]...)
			cmd.Dir = workdir
			for _, kv := range os.Environ() {
				if !strings.HasPrefix(kv, "HOME=") && !strings.HasPrefix(kv, "PWD=") {
					cmd.Env = append(cmd.Env, kv)
				}
			}
			cmd.Env = append(cmd.Env, "HOME="+home, "PWD="+workdir)
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
			t.Logf("argv=%q exit=%d stdout/stderr:\n%s\nSBPL:\n%s", tc.argv, code, out, full[2])
			if ctx.Err() != nil {
				t.Fatalf("probe timed out: %v", ctx.Err())
			}
			if code < 0 {
				t.Fatalf("signal termination is not a permission-denial result: exit=%d\n%s", code, out)
			}
			if (code == 0) != tc.wantOK {
				t.Errorf("exit=%d wantOK=%v\nstdout/stderr:\n%s", code, tc.wantOK, out)
			}
			if tc.wantOK && tc.want != "" && strings.TrimSpace(string(out)) != tc.want {
				t.Errorf("output %q, want %q", out, tc.want)
			}
			// Any non-zero exit is not enough: a wrong path fails too.
			if !tc.wantOK && !strings.Contains(strings.ToLower(string(out)), "operation not permitted") {
				t.Errorf("failed for another reason than a sandbox denial: %q", out)
			}
			if !tc.wantOK && strings.Contains(string(out), "outside-workdir") {
				t.Errorf("content outside the workdir leaked: %q", out)
			}
			if strings.Contains(tc.name, "list-") && (strings.Contains(string(out), "foreign.txt") || strings.Contains(string(out), "sibling.txt")) {
				t.Errorf("entry names leaked: %q", out)
			}
		})
	}
}

// bunBinary returns the canonical path of bun, or of OpenCode as a
// Bun-compiled stand-in, or "" when neither is installed.
func bunBinary() string {
	for _, name := range []string{"bun", "opencode"} {
		if p, err := exec.LookPath(name); err == nil {
			if resolved, err := filepath.EvalSymlinks(p); err == nil {
				return resolved
			}
		}
	}
	return ""
}
