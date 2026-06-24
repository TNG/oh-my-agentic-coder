//go:build linux

package sandboxrun

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
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

	wantBoth := []string{shimDir, realDir}

	// Resolve via an absolute path to the shim.
	if got := resolveInnerBinaryDirs([]string{shim}); !reflect.DeepEqual(got, wantBoth) {
		t.Errorf("symlinked binary dirs = %v, want %v", got, wantBoth)
	}

	// Resolve via PATH lookup (the `which opencode` case).
	t.Setenv("PATH", shimDir)
	if got := resolveInnerBinaryDirs([]string{"opencode"}); !reflect.DeepEqual(got, wantBoth) {
		t.Errorf("PATH-resolved binary dirs = %v, want %v", got, wantBoth)
	}

	// A non-symlinked binary yields a single dir (no duplicate grant).
	plainDir := filepath.Join(root, "plain")
	if err := os.MkdirAll(plainDir, 0o755); err != nil {
		t.Fatal(err)
	}
	plainBin := filepath.Join(plainDir, "tool")
	if err := os.WriteFile(plainBin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := resolveInnerBinaryDirs([]string{plainBin}); !reflect.DeepEqual(got, []string{plainDir}) {
		t.Errorf("plain binary dirs = %v, want %v", got, []string{plainDir})
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
