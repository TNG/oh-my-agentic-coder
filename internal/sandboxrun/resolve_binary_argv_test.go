package sandboxrun

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// writeExecutable creates an executable file at <dir>/<name> and returns its
// path. The content is irrelevant — nothing here execs it.
func writeExecutable(t *testing.T, dir, name string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// A bare argv[0] makes the macOS backend hand sandbox-exec a name rather than
// a path, and sandbox-exec's execvp then walks PATH a SECOND time — inside the
// sandbox, where a denied lookup is reported as ENOENT. That second lookup can
// disagree with the host-side one used to build the grants, and the user sees
//
//	sandbox-exec: execvp() of 'opencode' failed: No such file or directory
//
// on a machine where `which opencode` resolves. Resolving argv[0] on the host
// removes the second lookup entirely.
func TestAbsoluteInnerArgvResolvesBareCommandToAbsolutePath(t *testing.T) {
	dir := t.TempDir()
	exe := writeExecutable(t, dir, "opencode")
	t.Setenv("PATH", dir)

	got := absoluteInnerArgv([]string{"opencode", "--version"})

	want := []string{exe, "--version"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("absoluteInnerArgv = %v, want %v", got, want)
	}
}

// The rewrite must not follow symlinks. A Homebrew-style <prefix>/bin entry is
// a symlink into Cellar, and a shim install (mise/asdf) is a symlink into a
// version store; the link path is what a PATH lookup finds and what the
// harness expects to see as argv[0].
//
// It also keeps omac from defeating the profile: under a deny covering the
// harness's tree the EvalSymlinks-resolved path EXECS while the link path is
// refused, because exec of a main image never consults the file-read rules.
// Substituting the resolved path would launch a binary out of a tree the user
// explicitly denied. See lookPathAbs.
func TestAbsoluteInnerArgvKeepsTheSymlinkPathNotItsTarget(t *testing.T) {
	root := t.TempDir()
	cellarBin := filepath.Join(root, "homebrew", "Cellar", "opencode", "1.18.15", "bin")
	writeExecutable(t, cellarBin, "opencode")
	prefixBin := filepath.Join(root, "homebrew", "bin")
	if err := os.MkdirAll(prefixBin, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(prefixBin, "opencode")
	if err := os.Symlink("../Cellar/opencode/1.18.15/bin/opencode", link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", prefixBin)

	got := absoluteInnerArgv([]string{"opencode"})

	if !reflect.DeepEqual(got, []string{link}) {
		t.Errorf("absoluteInnerArgv = %v, want %v", got, []string{link})
	}
}

// A launcher profile may wrap the harness in `env NAME=VALUE ... <cmd>` to
// set NPM_CONFIG_* before launching it. Rewriting only argv[0] would resolve
// `env` and leave `env` to do its own PATH search for the harness inside the
// sandbox — the same second lookup, just moved one process along. Both
// executable tokens must be resolved.
func TestAbsoluteInnerArgvResolvesTheEnvWrappedCommandToo(t *testing.T) {
	dir := t.TempDir()
	envExe := writeExecutable(t, dir, "env")
	exe := writeExecutable(t, dir, "opencode")
	t.Setenv("PATH", dir)

	argv := []string{"env", "NPM_CONFIG_USERCONFIG=/x/npmrc", "npm_config_cache=/x/cache", "opencode", "--version"}
	got := absoluteInnerArgv(argv)

	want := []string{envExe, "NPM_CONFIG_USERCONFIG=/x/npmrc", "npm_config_cache=/x/cache", exe, "--version"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("absoluteInnerArgv = %v, want %v", got, want)
	}
}

// `env` flags are not used by omac's profiles and are deliberately left in
// place by unwrapEnv, which makes the following token a flag rather than a
// command. It must not be rewritten or otherwise disturbed.
func TestAbsoluteInnerArgvLeavesEnvFlagFormAlone(t *testing.T) {
	dir := t.TempDir()
	writeExecutable(t, dir, "opencode")
	t.Setenv("PATH", dir)

	argv := []string{"env", "-i", "opencode"}
	got := absoluteInnerArgv(argv)

	if got[1] != "-i" || got[2] != "opencode" {
		t.Errorf("absoluteInnerArgv = %v, want the flag form untouched after argv[0]", got)
	}
}

// envWrapperEnd is the index unwrapEnv slices at; the two must not drift.
func TestEnvWrapperEndMatchesUnwrapEnv(t *testing.T) {
	for _, argv := range [][]string{
		nil,
		{"opencode"},
		{"env", "A=1", "opencode", "--x"},
		{"env", "A=1", "env", "B=2", "opencode"},
		{"env", "-i", "opencode"},
		{"env"},
		{"env", "A=1"},
	} {
		want := unwrapEnv(argv)
		got := argv[envWrapperEnd(argv):]
		if !reflect.DeepEqual(got, want) {
			t.Errorf("argv %v: envWrapperEnd slice = %v, unwrapEnv = %v", argv, got, want)
		}
	}
}

// An unresolvable command must pass through untouched: the caller's error
// surface (and any launcher that resolves the command by other means) stays
// exactly as it was.
func TestAbsoluteInnerArgvLeavesUnresolvableCommandAlone(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	argv := []string{"definitely-not-installed", "--flag"}
	if got := absoluteInnerArgv(argv); !reflect.DeepEqual(got, argv) {
		t.Errorf("absoluteInnerArgv = %v, want it unchanged %v", got, argv)
	}
}

// An empty argv must not panic — Grants can carry no inner command at all.
func TestAbsoluteInnerArgvHandlesEmptyArgv(t *testing.T) {
	if got := absoluteInnerArgv(nil); len(got) != 0 {
		t.Errorf("absoluteInnerArgv(nil) = %v, want empty", got)
	}
}

// The rewrite must not mutate the caller's slice: BuildChildArgv passes the
// launch flags' InnerArgv, which the supervisor still reads afterwards (e.g.
// harnessName for the audit record).
func TestAbsoluteInnerArgvDoesNotMutateInput(t *testing.T) {
	dir := t.TempDir()
	writeExecutable(t, dir, "tool")
	t.Setenv("PATH", dir)

	argv := []string{"tool", "run"}
	absoluteInnerArgv(argv)

	if argv[0] != "tool" {
		t.Errorf("input argv mutated: argv[0] = %q, want %q", argv[0], "tool")
	}
}

// A profile deny broad enough to cover the harness's own directory defeats
// the per-launch grant, because protected denies are emitted after the read
// allows and Seatbelt is last-match-wins. The only user-visible symptom is
// an ENOENT that names neither the deny nor the directory, so the conflict
// must be reported before launch.
func TestProtectedInnerBinaryWarning(t *testing.T) {
	home := t.TempDir()
	binDir := filepath.Join(home, "homebrew", "bin")
	writeExecutable(t, binDir, "opencode")
	t.Setenv("PATH", binDir)

	warn := protectedInnerBinaryWarning([]string{"opencode"}, []string{home})
	if warn == "" {
		t.Fatal("a deny covering the harness binary dir did not warn")
	}
	if !strings.Contains(warn, home) {
		t.Errorf("warning does not name the deny %q: %s", home, warn)
	}

	// A deny elsewhere must stay quiet — this fires on every launch, so a
	// false positive trains users to ignore it.
	if w := protectedInnerBinaryWarning([]string{"opencode"}, []string{filepath.Join(home, ".ssh")}); w != "" {
		t.Errorf("unrelated deny warned: %s", w)
	}
	if w := protectedInnerBinaryWarning([]string{"opencode"}, nil); w != "" {
		t.Errorf("no denies at all warned: %s", w)
	}
}

// A command the host cannot resolve produces no grant at all and the launch
// dies inside the sandbox naming the wrong cause. Warn once, up front.
func TestUnresolvedInnerCommandWarning(t *testing.T) {
	dir := t.TempDir()
	writeExecutable(t, dir, "present")
	t.Setenv("PATH", dir)

	if w := unresolvedInnerCommandWarning([]string{"present"}); w != "" {
		t.Errorf("resolvable command warned: %q", w)
	}
	if w := unresolvedInnerCommandWarning([]string{"absent"}); w == "" {
		t.Error("unresolvable command did not warn")
	}
	if w := unresolvedInnerCommandWarning(nil); w != "" {
		t.Errorf("empty argv warned: %q", w)
	}
	// The wrapped command, not the `env` wrapper, is what must be checked.
	if w := unresolvedInnerCommandWarning([]string{"env", "FOO=1", "absent"}); w == "" {
		t.Error("env-wrapped unresolvable command did not warn")
	}
}
