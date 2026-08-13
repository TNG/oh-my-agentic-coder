//go:build darwin

package sandboxrun

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sandbox-exec is handed argv after `--` verbatim, so a bare argv[0] leaves it
// to execvp to walk PATH a second time INSIDE the sandbox — where a denied
// lookup surfaces as ENOENT and the user is told
//
//	sandbox-exec: execvp() of 'opencode' failed: No such file or directory
//
// on a machine where `which opencode` resolves fine. The grants are computed
// from a HOST-side lookup; handing sandbox-exec the path that lookup produced
// is what keeps the two from disagreeing.
func TestBuildChildArgvPassesTheInnerBinaryAsAnAbsolutePath(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "opencode")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	argv, err := BuildChildArgv(&Grants{Workdir: dir}, []string{"opencode", "--version"})
	if err != nil {
		t.Fatal(err)
	}

	// argv is: sandbox-exec -p <profile> -- <inner...>
	const innerAt = 4
	if len(argv) != innerAt+2 {
		t.Fatalf("argv = %v, want %d elements", argv, innerAt+2)
	}
	if argv[innerAt] != exe {
		t.Errorf("inner argv[0] = %q, want the absolute path %q", argv[innerAt], exe)
	}
	if argv[innerAt+1] != "--version" {
		t.Errorf("inner args not preserved: %v", argv[innerAt:])
	}
}

// The rewrite must not silently drop a command the host cannot resolve: argv
// stays as the caller wrote it so the failure mode is unchanged (and the
// supervisor's warning, not this function, is what explains it).
func TestBuildChildArgvKeepsAnUnresolvableInnerCommand(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	argv, err := BuildChildArgv(&Grants{Workdir: t.TempDir()}, []string{"definitely-not-installed"})
	if err != nil {
		t.Fatal(err)
	}
	if got := argv[len(argv)-1]; got != "definitely-not-installed" {
		t.Errorf("inner argv[0] = %q, want it unchanged", got)
	}
}

// Regression guard for the reported failure: a Homebrew install at a custom
// $HOME-rooted prefix (/Users/<u>/homebrew, not /opt/homebrew) whose bin entry
// is a relative symlink into Cellar. The darwin baseline hard-codes
// /opt/homebrew and /usr/local, so nothing but the per-launch resolution
// covers this layout — both the PATH-entry dir and the Cellar real dir must be
// granted read AND map-executable, and the emitted argv must be absolute.
func TestBuildChildArgvHomeRootedHomebrewPrefix(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cellarBin := filepath.Join(home, "homebrew", "Cellar", "opencode", "1.18.15", "bin")
	if err := os.MkdirAll(cellarBin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cellarBin, "opencode"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	prefixBin := filepath.Join(home, "homebrew", "bin")
	if err := os.MkdirAll(prefixBin, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(prefixBin, "opencode")
	if err := os.Symlink("../Cellar/opencode/1.18.15/bin/opencode", link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", prefixBin)

	argv, err := BuildChildArgv(&Grants{Workdir: home}, []string{"opencode"})
	if err != nil {
		t.Fatal(err)
	}
	profile := argv[2]

	// Assert the canonical form: the resolver feeds GenerateSBPL the
	// symlink-resolved real dir, so on a host whose temp root sits behind a
	// symlink (/var -> /private/var) only that form is emitted for it.
	for _, dir := range []string{prefixBin, cellarBin} {
		form := canonicalPath(dir)
		if form == "" {
			form = dir
		}
		for _, op := range []string{"file-read*", "file-map-executable"} {
			want := "(allow " + op + " (subpath " + sbplQuote(form) + "))"
			if !strings.Contains(profile, want) {
				t.Errorf("profile missing %s", want)
			}
		}
	}
	if got := argv[len(argv)-1]; got != link {
		t.Errorf("inner argv[0] = %q, want the absolute PATH-entry path %q", got, link)
	}
}
