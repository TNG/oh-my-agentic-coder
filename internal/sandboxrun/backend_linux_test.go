//go:build linux

package sandboxrun

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestResolveInnerBinaryDirs(t *testing.T) {
	// Empty / blank argv resolves to nothing.
	if got := resolveInnerBinaryDirs(nil); got != nil {
		t.Errorf("nil argv = %v, want nil", got)
	}
	if got := resolveInnerBinaryDirs([]string{""}); got != nil {
		t.Errorf("blank argv[0] = %v, want nil", got)
	}

	// A name not on PATH resolves to nothing rather than guessing.
	if got := resolveInnerBinaryDirs([]string{"definitely-not-a-real-binary-xyz"}); got != nil {
		t.Errorf("missing binary = %v, want nil", got)
	}

	// A shim whose symlink target lives in a different tree, with the
	// real file renamed (mimicking bun: ~/.bun/bin/opencode ->
	// ~/.bun/install/.../bin/opencode.exe). BOTH the shim dir (on PATH,
	// needed by stage2's own LookPath) and the resolved real dir must be
	// granted.
	root := t.TempDir()
	realDir := filepath.Join(root, "install", "opencode-ai", "bin")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	realBin := filepath.Join(realDir, "opencode.exe")
	if err := os.WriteFile(realBin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	shimDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(shimDir, 0o755); err != nil {
		t.Fatal(err)
	}
	shim := filepath.Join(shimDir, "opencode")
	if err := os.Symlink(realBin, shim); err != nil {
		t.Fatal(err)
	}

	// The test binary is a #!/bin/sh script, so shebang detection
	// resolves "/bin/sh" (absolute path from shebang) and grants
	// its dir + symlink-resolved dir. Compute the expected interpreter
	// dirs the same way the function does.
	interpDirs := resolveInterpreterDirs("/bin/sh")

	wantBoth := append([]string{shimDir, realDir}, interpDirs...)

	// Resolve via an absolute path to the shim (uses host PATH for sh).
	if got := resolveInnerBinaryDirs([]string{shim}); !reflect.DeepEqual(got, wantBoth) {
		t.Errorf("symlinked binary dirs = %v, want %v", got, wantBoth)
	}

	// Resolve via PATH lookup (the `which opencode` case). After
	// t.Setenv PATH=shimDir, the shebang #!/bin/sh is an absolute
	// path so the interpreter is still found via os.Stat, not PATH.
	t.Setenv("PATH", shimDir)
	wantPath := append([]string{shimDir, realDir}, interpDirs...)
	if got := resolveInnerBinaryDirs([]string{"opencode"}); !reflect.DeepEqual(got, wantPath) {
		t.Errorf("PATH-resolved binary dirs = %v, want %v", got, wantPath)
	}

	// A non-symlinked binary yields a single dir + interpreter dirs.
	plainDir := filepath.Join(root, "plain")
	if err := os.MkdirAll(plainDir, 0o755); err != nil {
		t.Fatal(err)
	}
	plainBin := filepath.Join(plainDir, "tool")
	if err := os.WriteFile(plainBin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	wantPlain := append([]string{plainDir}, interpDirs...)
	if got := resolveInnerBinaryDirs([]string{plainBin}); !reflect.DeepEqual(got, wantPlain) {
		t.Errorf("plain binary dirs = %v, want %v", got, wantPlain)
	}

	// An `env NAME=VALUE ... <cmd>` wrapper (as a sandbox profile may use to
	// set NPM_CONFIG_* before launch) must not hide the harness: the wrapped
	// command's dirs must still be granted, or a bun/npm-installed opencode
	// fails to exec inside the sandbox. Grant the shim via PATH lookup.
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+"/usr/bin:/bin")
	got := resolveInnerBinaryDirs([]string{"env", "FOO=bar", "BAR=baz", "opencode"})
	for _, want := range []string{shimDir, realDir} {
		if !slices.Contains(got, want) {
			t.Errorf("env-wrapped dirs = %v, missing wrapped-command dir %q", got, want)
		}
	}
}

func TestUnwrapEnv(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{"no wrapper", []string{"opencode", "--version"}, []string{"opencode", "--version"}},
		{"env + assignments", []string{"env", "A=1", "B=2", "opencode", "-x"}, []string{"opencode", "-x"}},
		{"env no assignments", []string{"env", "opencode"}, []string{"opencode"}},
		{"absolute env path", []string{"/usr/bin/env", "A=1", "claude"}, []string{"claude"}},
		{"nested env", []string{"env", "A=1", "env", "B=2", "codex"}, []string{"codex"}},
		{"env flag left in place", []string{"env", "-i", "opencode"}, []string{"-i", "opencode"}},
		{"bare env", []string{"env"}, []string{"env"}},
		{"empty", nil, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := unwrapEnv(tc.in); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("unwrapEnv(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestFormatUsernsDiagnosis(t *testing.T) {
	bwrapErr := errors.New("exit status 1")
	const bwrapMsg = "bwrap: setting up uid map: Permission denied"

	tests := []struct {
		name     string
		state    usernsState
		wantSubs []string // all must appear
		notSubs  []string // none may appear
	}{
		{
			name: "ubuntu apparmor restriction enabled",
			state: usernsState{
				runErr: bwrapErr, firstOutLine: bwrapMsg,
				apparmor: 1, apparmorKnown: true,
			},
			wantSubs: []string{
				bwrapMsg,
				"AppArmor is restricting unprivileged user namespaces",
				"kernel.apparmor_restrict_unprivileged_userns=1",
				"/etc/apparmor.d/bwrap",
				"apparmor_parser -r /etc/apparmor.d/bwrap",
				"sysctl -w kernel.apparmor_restrict_unprivileged_userns=0",
			},
			notSubs: []string{"unprivileged_userns_clone"},
		},
		{
			name: "apparmor knob present but disabled -> generic",
			state: usernsState{
				runErr: bwrapErr, firstOutLine: bwrapMsg,
				apparmor: 0, apparmorKnown: true,
			},
			wantSubs: []string{"unprivileged user namespaces are unavailable here"},
			notSubs:  []string{"Fix A", "unprivileged_userns_clone=0"},
		},
		{
			name: "all-or-nothing clone switch off",
			state: usernsState{
				runErr: bwrapErr, firstOutLine: bwrapMsg,
				clone: 0, cloneKnown: true,
			},
			wantSubs: []string{
				"disabled system-wide",
				"kernel.unprivileged_userns_clone=0",
				"sysctl -w kernel.unprivileged_userns_clone=1",
			},
			notSubs: []string{"AppArmor is restricting"},
		},
		{
			name: "apparmor takes precedence over clone",
			state: usernsState{
				runErr: bwrapErr, firstOutLine: bwrapMsg,
				apparmor: 1, apparmorKnown: true,
				clone: 0, cloneKnown: true,
			},
			wantSubs: []string{"AppArmor is restricting unprivileged user namespaces"},
			notSubs:  []string{"disabled system-wide"},
		},
		{
			name: "no known knobs -> generic hint",
			state: usernsState{
				runErr: bwrapErr, firstOutLine: bwrapMsg,
			},
			wantSubs: []string{
				"unprivileged user namespaces are unavailable here",
				"cat /proc/sys/kernel/apparmor_restrict_unprivileged_userns",
			},
			notSubs: []string{"Fix A", "Fix B"},
		},
		{
			name: "empty bwrap output omits the dash separator",
			state: usernsState{
				runErr: bwrapErr, firstOutLine: "",
			},
			wantSubs: []string{"bwrap is installed but not functional (unprivileged user namespaces blocked?): exit status 1"},
			notSubs:  []string{" — "},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := formatUsernsDiagnosis(tc.state)
			for _, want := range tc.wantSubs {
				if !strings.Contains(got, want) {
					t.Errorf("missing %q in:\n%s", want, got)
				}
			}
			for _, bad := range tc.notSubs {
				if strings.Contains(got, bad) {
					t.Errorf("unexpected %q in:\n%s", bad, got)
				}
			}
		})
	}
}
