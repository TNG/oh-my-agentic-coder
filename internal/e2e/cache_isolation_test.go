//go:build e2e

// Package e2e cache isolation tests (Task 10).
//
// These tests build omac with the existing buildOmac helper and launch
// shells as inner commands without model credentials. They cover the
// persistent/ephemeral cache lifecycle, environment allowlist overrides,
// worktree isolation, recovery from an unsafe cache root, serve-mode
// cache scoping, the cache clear command, deterministic tool probes,
// and the provenance cross-check for the cache section.
//
// Tool probes are deterministic with no external-network dependency. Optional tools that
// are missing on the local host skip with an explicit message; CI must
// install all four (go, npm, python3, cargo) so the skips do not fire
// there.
package e2e

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/tngtech/oh-my-agentic-coder/internal/toolcache"
)

// cacheRunTimeout bounds the omac start/serve subprocess.
const cacheRunTimeout = 3 * time.Minute

// skipIfSandboxUnavailable skips the test when the omac sandbox cannot
// be applied in this environment. On macOS the Seatbelt sandbox is
// unavailable inside some test environments (e.g. an omac sandbox
// nesting another omac sandbox); on Linux bubblewrap may be missing.
// CI runners have a working sandbox, so this skip only fires locally.
func skipIfSandboxUnavailable(t *testing.T) {
	t.Helper()
	if unavailable, reason := sandboxUnavailable(); unavailable {
		// Linux CI provisions a functional bubblewrap (the
		// cache-integration and e2e jobs install it and smoke-test it),
		// so an unavailable sandbox there is a real regression, not an
		// environment gap — fail instead of silently passing. macOS
		// keeps skipping: the SUN_LEN socket-path limit is a genuine
		// per-runner constraint, not a backend regression.
		if runtime.GOOS == "linux" && os.Getenv("GITHUB_ACTIONS") == "true" {
			t.Fatalf("omac sandbox unavailable in CI: %s", reason)
		}
		t.Skipf("omac sandbox unavailable in this environment: %s; CI exercises this test", reason)
	}
}

// sandboxUnavailable reports whether the omac sandbox cannot be applied
// here. On macOS, the Seatbelt sandbox may be unavailable (e.g. nested
// sandboxes) and the runtime temp dir prefix may be too long for a
// Unix socket bind (SUN_LEN=104). On Linux, bubblewrap may be missing or
// unusable in a nested sandbox.
func sandboxUnavailable() (bool, string) {
	if runtime.GOOS == "darwin" {
		// macOS SUN_LEN is 104. The omac runtime socket path is
		// $TMPDIR/omac-<10hex>/bridge.sock ≈ len(TMPDIR) + 35. If
		// that exceeds ~95, the bind will fail with EINVAL.
		if tmp := os.Getenv("TMPDIR"); len(tmp)+40 > 104 {
			return true, "TMPDIR is too long for the macOS sandbox socket"
		}
	}
	if runtime.GOOS == "linux" {
		if _, err := exec.LookPath("bwrap"); err != nil {
			return true, "bubblewrap is not installed"
		}
		if err := exec.Command("bwrap", "--ro-bind", "/", "/", "true").Run(); err != nil {
			return true, "bubblewrap is not functional: " + err.Error()
		}
	}
	return false, ""
}

// runOmacShell launches `omac start claude-code --inner /bin/sh -- <argv>`.
// Claude has no ServerLaunch subcommand, so `serve` is never injected into
// shell argv. No model credentials are needed: the inner command is /bin/sh.
// extraArgs are inserted before "--" (e.g. --ephemeral-cache, --no-sandbox).
func runOmacShell(t *testing.T, omacBin, home, workdir string, extraArgs []string, innerArgv ...string) (string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), cacheRunTimeout)
	defer cancel()
	args := []string{"start", "claude-code"}
	args = append(args, extraArgs...)
	args = append(args, "--inner", "/bin/sh", "--")
	if len(innerArgv) > 0 && innerArgv[0] == "/bin/sh" {
		innerArgv = innerArgv[1:]
	}
	args = append(args, innerArgv...)
	cmd := exec.CommandContext(ctx, omacBin, args...)
	cmd.Dir = workdir
	env := withHome(os.Environ(), home)
	env = append(env, "PWD="+workdir)
	cmd.Env = env
	cmd.Stdin = strings.NewReader("")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			t.Fatalf("exec omac: %v\nSTDOUT:\n%s\nSTDERR:\n%s", err, stdout.String(), stderr.String())
		}
	}
	if ctx.Err() == context.DeadlineExceeded {
		t.Fatalf("omac start timed out after %v\nSTDOUT:\n%s\nSTDERR:\n%s",
			cacheRunTimeout, stdout.String(), stderr.String())
	}
	return stdout.String() + "\n" + stderr.String(), code
}

// runOmacServeShell launches `omac serve claude-code --inner /bin/sh -- <argv>`.
func runOmacServeShell(t *testing.T, omacBin, home, workdir string, extraArgs []string, innerArgv ...string) (string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), cacheRunTimeout)
	defer cancel()
	args := []string{"serve", "claude-code"}
	args = append(args, extraArgs...)
	args = append(args, "--inner", "/bin/sh", "--")
	if len(innerArgv) > 0 && innerArgv[0] == "/bin/sh" {
		innerArgv = innerArgv[1:]
	}
	args = append(args, innerArgv...)
	cmd := exec.CommandContext(ctx, omacBin, args...)
	cmd.Dir = workdir
	env := withHome(os.Environ(), home)
	env = append(env, "PWD="+workdir)
	cmd.Env = env
	cmd.Stdin = strings.NewReader("")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			t.Fatalf("exec omac serve: %v\nSTDOUT:\n%s\nSTDERR:\n%s", err, stdout.String(), stderr.String())
		}
	}
	if ctx.Err() == context.DeadlineExceeded {
		t.Fatalf("omac serve timed out after %v\nSTDOUT:\n%s\nSTDERR:\n%s",
			cacheRunTimeout, stdout.String(), stderr.String())
	}
	return stdout.String() + "\n" + stderr.String(), code
}

// cacheTestHome prepares a temp HOME with the minimal directory layout
// omac expects (so the sandbox profile's allow paths exist on disk).
// The omac subprocess gets HOME=home via withHome in its env; the test
// process does NOT change HOME (that would redirect go's module cache
// during buildOmac and leave read-only files that break t.TempDir
// cleanup).
func cacheTestHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	for _, dir := range []string{
		".cache", ".local/share", ".local/state", ".config",
		".claude", ".cargo/bin", ".rustup",
	} {
		if err := os.MkdirAll(filepath.Join(home, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_ = filepath.WalkDir(filepath.Join(home, ".cache"), func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				_ = os.Chmod(path, 0o755)
			}
			return nil
		})
	})
	return home
}

// writeCacheTestProfile writes a minimal sandbox profile that grants the
// workdir read+write, blocks network, and adds extraRead as read-only paths
// and extraAllow as writable paths.
//
// The profile deliberately does NOT grant ~/.cache: omac injects the
// resolved cache leaf as an --allow path at launch time (see start.go's
// injectSandboxFlag), so the broad cache root must stay ungranted to
// preserve isolation. extraAllow is for writable test-specific paths; the pip
// fixture's loopback port is handled via network.open_port.
func writeCacheTestProfile(t *testing.T, home string, extraRead, extraAllow []string, extraOpenPort int) {
	t.Helper()
	profDir := filepath.Join(home, ".config", "omac", "sandbox-profiles")
	if err := os.MkdirAll(profDir, 0o755); err != nil {
		t.Fatal(err)
	}
	profile := map[string]any{
		"meta":    map[string]string{"name": "default"},
		"workdir": map[string]string{"access": "readwrite"},
		"filesystem": map[string]any{
			"read":  extraRead,
			"allow": extraAllow,
		},
		"network": map[string]any{
			"mode": "blocked",
		},
		// This fixture exercises cache-dir isolation, not env allowlisting: the
		// dev tools (go/node/npm/pip/cargo/rustc) need their ambient env
		// (GOPATH, GOMODCACHE, node paths, …). "*" inherits every ambient var
		// (still minus the danger blocklist) so an empty allow_vars is not
		// fail-closed to the operational minimum at launch (see forwardHarnessEnv).
		"environment": map[string]any{
			"allow_vars": []string{"*"},
		},
	}
	if extraOpenPort > 0 {
		profile["network"] = map[string]any{
			"mode":         "filtered",
			"open_port":    []int{extraOpenPort},
			"allow_domain": []string{"127.0.0.1", "localhost"},
		}
	}
	data, _ := json.MarshalIndent(profile, "", "  ")
	if err := os.WriteFile(filepath.Join(profDir, "default.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func commandOutput(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).Output()
	if err != nil {
		t.Fatalf("%s %v: %v", name, args, err)
	}
	return strings.TrimSpace(string(out))
}

func TestToolRuntimeReadPathsUsesNarrowPythonRuntimeRoots(t *testing.T) {
	binDir := t.TempDir()
	stdlib := filepath.Join(t.TempDir(), "stdlib")
	purelib := filepath.Join(t.TempDir(), "purelib")
	for _, dir := range []string{stdlib, purelib} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	python := filepath.Join(binDir, "python3")
	script := "#!/bin/sh\ncase \"$2\" in\n*sysconfig*) printf '%s\\n%s\\n' \"" + stdlib + "\" \"" + purelib + "\" ;;\n*) printf '/usr\\n' ;;\nesac\n"
	if err := os.WriteFile(python, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	paths := toolRuntimeReadPaths(t)
	for _, broadRoot := range []string{"/", "/usr", "/usr/local", "/opt", "/opt/homebrew"} {
		for _, path := range paths {
			if path == broadRoot {
				t.Errorf("runtime read paths include broad root %q: %v", broadRoot, paths)
			}
		}
	}
	for _, want := range []string{stdlib, purelib} {
		want, err := filepath.EvalSymlinks(want)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, path := range paths {
			if path == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("runtime read paths do not include Python runtime path %q: %v", want, paths)
		}
	}
}

func TestIsBroadRuntimeRootRejectsPlatformRootsAndHostHome(t *testing.T) {
	hostHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name string
		path string
	}{
		{"filesystem root", "/"},
		{"system root", "/usr"},
		{"local install parent", "/usr/local"},
		{"opt parent", "/opt"},
		{"homebrew parent", "/opt/homebrew"},
		{"host home", hostHome},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if !isBroadRuntimeRoot(tt.path) {
				t.Errorf("isBroadRuntimeRoot(%q) = false; want true", tt.path)
			}
		})
	}
}

func TestCargoRustupRuntimeUsesImplicitHostHome(t *testing.T) {
	if _, err := exec.LookPath("rustc"); err != nil {
		t.Skip("rustc not available")
	}
	hostHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("RUSTUP_HOME", "")

	rustupHome, paths := cargoRustupRuntime(t, commandOutput(t, "rustc", "--print", "sysroot"))
	if rustupHome == "" {
		t.Skip("host rustc is not managed by the implicit Rustup home")
	}
	wantHome, err := resolveRuntimePath(filepath.Join(hostHome, ".rustup"))
	if err != nil {
		t.Fatal(err)
	}
	if rustupHome != wantHome {
		t.Fatalf("RUSTUP_HOME = %q; want implicit host Rustup home %q", rustupHome, wantHome)
	}
	for _, path := range paths {
		if path == rustupHome || isBroadRuntimeRoot(path) {
			t.Errorf("Rustup runtime path is too broad: %q", path)
		}
	}
}

func isBroadRuntimeRoot(path string) bool {
	path = filepath.Clean(path)
	switch path {
	case "/", "/usr", "/usr/local", "/opt", "/opt/homebrew":
		return true
	}
	if home, err := os.UserHomeDir(); err == nil {
		if resolved, err := filepath.EvalSymlinks(home); err == nil {
			home = resolved
		}
		return path == filepath.Clean(home)
	}
	return false
}

func resolveRuntimePath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(absolute)
}

func pathWithin(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// cargoRustupRuntime returns a rustup home for the child environment and the
// specific host runtime paths needed by rustup's proxy. It never grants the
// whole rustup home: only the active toolchain and its settings file.
func cargoRustupRuntime(t *testing.T, sysroot string) (string, []string) {
	t.Helper()
	hostHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	candidate := os.Getenv("RUSTUP_HOME")
	if candidate == "" {
		candidate = filepath.Join(hostHome, ".rustup")
	}
	rustupHome, err := resolveRuntimePath(candidate)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		t.Fatalf("resolve RUSTUP_HOME candidate %q: %v", candidate, err)
	}
	if isBroadRuntimeRoot(rustupHome) {
		t.Fatalf("refusing broad RUSTUP_HOME candidate %q", rustupHome)
	}
	toolchains, err := resolveRuntimePath(filepath.Join(rustupHome, "toolchains"))
	if err != nil {
		t.Fatalf("resolve Rustup toolchains under %q: %v", rustupHome, err)
	}
	resolvedSysroot, err := resolveRuntimePath(sysroot)
	if err != nil {
		t.Fatalf("resolve rustc sysroot %q: %v", sysroot, err)
	}
	if !pathWithin(toolchains, resolvedSysroot) {
		return "", nil
	}
	// Grant the toolchains directory itself (not just the active
	// toolchain subpath) so the rustup proxy shim can list it to
	// discover installed toolchains. Without this, Seatbelt denies
	// readdir(~/.rustup/toolchains/) with EPERM on macOS, producing
	// "IO Error: Operation not permitted (os error 1)". The directory
	// contains only installed Rust toolchains — no credentials.
	paths := []string{resolvedSysroot, toolchains}
	settings := filepath.Join(rustupHome, "settings.toml")
	if _, err := os.Stat(settings); err == nil {
		resolvedSettings, err := resolveRuntimePath(settings)
		if err != nil {
			t.Fatal(err)
		}
		paths = append(paths, resolvedSettings)
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return rustupHome, paths
}

func toolRuntimeReadPaths(t *testing.T) []string {
	t.Helper()
	paths := map[string]struct{}{}
	add := func(path string) {
		if path == "" {
			return
		}
		resolved, err := resolveRuntimePath(path)
		if err != nil {
			t.Fatalf("resolve runtime path %q: %v", path, err)
		}
		if isBroadRuntimeRoot(resolved) {
			t.Fatalf("refusing broad runtime read root %q", resolved)
		}
		paths[resolved] = struct{}{}
	}
	available := func(name string) bool {
		path, err := exec.LookPath(name)
		if err != nil {
			return false
		}
		if abs, aerr := filepath.Abs(path); aerr == nil {
			path = abs
		}
		// Grant the unresolved PATH-entry dir (the shim dir, e.g.
		// ~/.cargo/bin) so the tool is found on PATH inside the
		// sandbox, AND the symlink-resolved dir so the real binary
		// and its siblings (shared libs, runtime) are reachable.
		add(filepath.Dir(path))
		resolved, err := resolveRuntimePath(path)
		if err != nil {
			t.Fatalf("resolve %s executable %q: %v", name, path, err)
		}
		add(filepath.Dir(resolved))
		return true
	}
	if available("go") {
		add(commandOutput(t, "go", "env", "GOROOT"))
	}
	if available("python3") {
		for _, path := range strings.Split(commandOutput(t, "python3", "-c", "import sysconfig; print('\\n'.join(filter(None, (sysconfig.get_path(name) for name in ('stdlib', 'platstdlib', 'purelib', 'platlib')))))"), "\n") {
			add(path)
		}
		if libDir := commandOutput(t, "python3", "-c", "import sysconfig; print(sysconfig.get_config_var('LIBDIR') or '')"); libDir != "" {
			add(libDir)
		}
	}
	if available("npm") {
		add(commandOutput(t, "npm", "root", "-g"))
		// Grant the Node.js install prefix's lib dir so the node
		// runtime can load built-in modules; without it, node may
		// segfault inside the sandbox.
		if npmPrefix := commandOutput(t, "npm", "config", "get", "prefix"); npmPrefix != "" {
			add(filepath.Join(npmPrefix, "lib"))
		}
	}
	rustcSysroot := ""
	if available("rustc") {
		rustcSysroot = commandOutput(t, "rustc", "--print", "sysroot")
		add(rustcSysroot)
	}
	if available("cargo") && rustcSysroot != "" {
		rustupHome, rustupPaths := cargoRustupRuntime(t, rustcSysroot)
		for _, path := range rustupPaths {
			add(path)
		}
		if rustupHome != "" {
			t.Logf("setting child RUSTUP_HOME to resolved host runtime: %s", rustupHome)
		}
	}
	result := make([]string, 0, len(paths))
	for path := range paths {
		result = append(result, path)
	}
	sort.Strings(result)
	for _, path := range result {
		t.Logf("granting read-only tool runtime root: %s", path)
	}
	return result
}

// requireTool skips the test if the named binary is not on PATH.
func requireTool(t *testing.T, name, purpose string) {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		t.Skipf("%s not available locally (%s); CI installs it", name, purpose)
	}
}

func runToolRuntimeProbe(t *testing.T, omacBin, home, workdir string, extraEnv []string, command, marker string) {
	t.Helper()
	out, code := runOmacShellWithEnv(t, omacBin, home, workdir, nil, extraEnv,
		"/bin/sh", "-c", command+" && echo "+marker)
	if code != 0 {
		t.Fatalf("runtime probe %q failed (exit %d): %s", marker, code, out)
	}
	if !strings.Contains(out, marker) {
		t.Errorf("runtime probe %q did not complete: %s", marker, out)
	}
}

func cargoRuntimeEnv(t *testing.T) []string {
	t.Helper()
	if _, err := exec.LookPath("rustc"); err != nil {
		return nil
	}
	rustupHome, _ := cargoRustupRuntime(t, commandOutput(t, "rustc", "--print", "sysroot"))
	if rustupHome == "" {
		return nil
	}
	return []string{
		"RUSTUP_HOME=" + rustupHome,
		"RUSTUP_DISABLE_AUTO_UPDATE=1",
	}
}

// rustupUpdateHashesDirs returns the update-hashes directory under the
// host RUSTUP_HOME, so the profile can grant it writable. The rustup
// proxy shim unconditionally tries to mkdir this directory on every
// invocation; without write access it fails with "could not create
// update-hash directory". The directory holds integrity hashes, not
// credentials.
func rustupUpdateHashesDirs(t *testing.T) []string {
	t.Helper()
	if _, err := exec.LookPath("rustc"); err != nil {
		return nil
	}
	rustupHome, _ := cargoRustupRuntime(t, commandOutput(t, "rustc", "--print", "sysroot"))
	if rustupHome == "" {
		return nil
	}
	return []string{filepath.Join(rustupHome, "update-hashes")}
}

func validateCacheRedirects(cacheDir string, environment map[string]string) error {
	expected := toolcache.Environment(cacheDir, toolcache.ModePersistent)
	for _, name := range []string{"XDG_CACHE_HOME", "GOCACHE", "GOMODCACHE", "NPM_CONFIG_CACHE", "PIP_CACHE_DIR", "CARGO_HOME"} {
		if environment[name] != expected[name] {
			return fmt.Errorf("%s = %q; want %q", name, environment[name], expected[name])
		}
	}
	return nil
}

func TestValidateCacheRedirectsRejectsScopePrefixEscape(t *testing.T) {
	cacheDir := filepath.Join(t.TempDir(), "cache")
	environment := toolcache.Environment(cacheDir, toolcache.ModePersistent)
	environment["GOCACHE"] = cacheDir + "-escape/go-build"
	if err := validateCacheRedirects(cacheDir, environment); err == nil {
		t.Fatal("cache redirects accepted a sibling prefix escape")
	}
}

func TestValidateCacheRedirectsRequiresXDGCacheHome(t *testing.T) {
	cacheDir := filepath.Join(t.TempDir(), "cache")
	environment := toolcache.Environment(cacheDir, toolcache.ModePersistent)
	delete(environment, "XDG_CACHE_HOME")
	if err := validateCacheRedirects(cacheDir, environment); err == nil {
		t.Fatal("cache redirects accepted a missing XDG_CACHE_HOME mapping")
	}
}

// TestE2ECachePersistentReuse: a persistent cache scope survives across
// two launches; the second run sees data written by the first.
func TestE2ECachePersistentReuse(t *testing.T) {
	skipIfSandboxUnavailable(t)
	home := cacheTestHome(t)
	workdir := t.TempDir()
	writeCacheTestProfile(t, home, nil, nil, 0)
	omacBin := buildOmac(t)

	marker := "persistent-cache-marker"
	out, code := runOmacShell(t, omacBin, home, workdir, nil,
		"/bin/sh", "-c", "echo OMAC_CACHE_MODE=$OMAC_CACHE_MODE; echo "+marker+" > \"$OMAC_CACHE_DIR/probe.txt\" && echo WROTE")
	if code != 0 {
		t.Fatalf("first launch failed (exit %d): %s", code, out)
	}
	if !strings.Contains(out, "WROTE") {
		t.Fatalf("first launch did not write probe: %s", out)
	}
	if mode := extractEnv(out, "OMAC_CACHE_MODE="); mode != string(toolcache.ModePersistent) {
		t.Errorf("OMAC_CACHE_MODE = %q; want %q", mode, toolcache.ModePersistent)
	}

	out, code = runOmacShell(t, omacBin, home, workdir, nil,
		"/bin/sh", "-c", "cat \"$OMAC_CACHE_DIR/probe.txt\" 2>&1")
	if code != 0 {
		t.Fatalf("second launch failed (exit %d): %s", code, out)
	}
	if !strings.Contains(out, marker) {
		t.Errorf("persistent cache marker not found on second launch: %s", out)
	}
}

// TestE2ECacheEphemeralStartsEmptyAndCleansUp: --ephemeral-cache starts
// with an empty cache, allows writes during the run, and leaves no
// persistent state.
func TestE2ECacheEphemeralStartsEmptyAndCleansUp(t *testing.T) {
	skipIfSandboxUnavailable(t)
	home := cacheTestHome(t)
	workdir := t.TempDir()
	writeCacheTestProfile(t, home, nil, nil, 0)
	omacBin := buildOmac(t)

	out, code := runOmacShell(t, omacBin, home, workdir, []string{"--ephemeral-cache"},
		"/bin/sh", "-c", "ls -A \"$OMAC_CACHE_DIR\" | wc -l")
	if code != 0 {
		t.Fatalf("ephemeral launch failed (exit %d): %s", code, out)
	}
	if !strings.Contains(out, "0") {
		t.Errorf("ephemeral cache should start empty: %s", out)
	}

	out, code = runOmacShell(t, omacBin, home, workdir, []string{"--ephemeral-cache"},
		"/bin/sh", "-c", "echo OMAC_CACHE_DIR=$OMAC_CACHE_DIR; test -z \"$(ls -A \"$OMAC_CACHE_DIR\")\" && echo ephemeral > \"$OMAC_CACHE_DIR/probe.txt\" && cat \"$OMAC_CACHE_DIR/probe.txt\"")
	if code != 0 {
		t.Fatalf("ephemeral write failed (exit %d): %s", code, out)
	}
	cacheDir := extractEnv(out, "OMAC_CACHE_DIR=")
	if cacheDir == "" {
		t.Fatalf("ephemeral launch did not expose OMAC_CACHE_DIR: %s", out)
	}
	if _, err := os.Stat(cacheDir); !os.IsNotExist(err) {
		t.Fatalf("ephemeral cache directory remains after child exit: %v", err)
	}
	if !strings.Contains(out, "ephemeral") {
		t.Errorf("ephemeral cache write/read failed: %s", out)
	}

	// Persistent cache root must not exist after ephemeral runs.
	persistentRoot := filepath.Join(home, ".cache", "omac")
	if _, err := os.Stat(persistentRoot); err == nil {
		entries, _ := os.ReadDir(persistentRoot)
		for _, e := range entries {
			if isDigestName(e.Name()) {
				t.Errorf("ephemeral run created persistent scope %s", e.Name())
			}
		}
	}
}

func isDigestName(name string) bool {
	if len(name) != 64 {
		return false
	}
	for _, c := range name {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// TestE2ECacheEnvironmentOverridesAllowlist: the injected cache env
// variables override any host-inherited values, so a hostile GOCACHE
// pointing at a host path is replaced.
func TestE2ECacheEnvironmentOverridesAllowlist(t *testing.T) {
	skipIfSandboxUnavailable(t)
	home := cacheTestHome(t)
	workdir := t.TempDir()
	writeCacheTestProfile(t, home, nil, nil, 0)
	omacBin := buildOmac(t)

	hostileGOCACHE := filepath.Join(home, "hostile-go-build")
	if err := os.MkdirAll(hostileGOCACHE, 0o755); err != nil {
		t.Fatal(err)
	}
	hostileCARGO := filepath.Join(home, "hostile-cargo")
	if err := os.MkdirAll(hostileCARGO, 0o755); err != nil {
		t.Fatal(err)
	}

	out, code := runOmacShellWithEnv(t, omacBin, home, workdir, nil,
		[]string{"GOCACHE=" + hostileGOCACHE, "CARGO_HOME=" + hostileCARGO},
		"/bin/sh", "-c", "echo XDG_CACHE_HOME=$XDG_CACHE_HOME; echo GOCACHE=$GOCACHE; echo GOMODCACHE=$GOMODCACHE; echo NPM_CONFIG_CACHE=$NPM_CONFIG_CACHE; echo PIP_CACHE_DIR=$PIP_CACHE_DIR; echo CARGO_HOME=$CARGO_HOME; echo OMAC_CACHE_DIR=$OMAC_CACHE_DIR")
	if code != 0 {
		t.Fatalf("launch failed (exit %d): %s", code, out)
	}
	if strings.Contains(out, hostileGOCACHE) {
		t.Errorf("hostile GOCACHE leaked into sandbox: %s", out)
	}
	if strings.Contains(out, hostileCARGO) {
		t.Errorf("hostile CARGO_HOME leaked into sandbox: %s", out)
	}
	cacheDir := extractEnv(out, "OMAC_CACHE_DIR=")
	if cacheDir == "" {
		t.Fatalf("OMAC_CACHE_DIR not exposed: %s", out)
	}
	environment := map[string]string{}
	for _, name := range []string{"XDG_CACHE_HOME", "GOCACHE", "GOMODCACHE", "NPM_CONFIG_CACHE", "PIP_CACHE_DIR", "CARGO_HOME"} {
		environment[name] = extractEnv(out, name+"=")
	}
	if err := validateCacheRedirects(cacheDir, environment); err != nil {
		t.Error(err)
	}
}

// runOmacShellWithEnv is like runOmacShell but adds extra env vars.
func runOmacShellWithEnv(t *testing.T, omacBin, home, workdir string, extraArgs, extraEnv []string, innerArgv ...string) (string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), cacheRunTimeout)
	defer cancel()
	args := []string{"start", "claude-code"}
	args = append(args, extraArgs...)
	args = append(args, "--inner", "/bin/sh", "--")
	if len(innerArgv) > 0 && innerArgv[0] == "/bin/sh" {
		innerArgv = innerArgv[1:]
	}
	args = append(args, innerArgv...)
	cmd := exec.CommandContext(ctx, omacBin, args...)
	cmd.Dir = workdir
	env := withHome(os.Environ(), home)
	env = append(env, extraEnv...)
	env = append(env, "PWD="+workdir)
	cmd.Env = env
	cmd.Stdin = strings.NewReader("")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			t.Fatalf("exec omac: %v", err)
		}
	}
	return stdout.String() + "\n" + stderr.String(), code
}

// TestE2ECacheMainAndLinkedWorktreesAreIsolated: a marker written
// through the main worktree's cache is invisible when launching from a
// linked worktree (different canonical path → different digest).
func TestE2ECacheMainAndLinkedWorktreesAreIsolated(t *testing.T) {
	skipIfSandboxUnavailable(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	home := cacheTestHome(t)
	// Place the repo under HOME so the sandbox baseline (read-only home)
	// doesn't grant write to sibling temp dirs.
	repoRoot := filepath.Join(home, "main-repo")
	if err := os.MkdirAll(repoRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	gitInHomeDir(t, repoRoot, "init", "-q")
	if err := os.WriteFile(filepath.Join(repoRoot, "seed"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitInHomeDir(t, repoRoot, "add", "seed")
	gitInHomeDir(t, repoRoot, "commit", "-q", "-m", "seed")

	linkedDir := filepath.Join(home, "linked-repo")
	gitInHomeDir(t, repoRoot, "worktree", "add", "-q", linkedDir, "-b", "linked-cache-test")

	writeCacheTestProfile(t, home, nil, nil, 0)
	omacBin := buildOmac(t)

	marker := "main-worktree-cache-marker"
	out, code := runOmacShell(t, omacBin, home, repoRoot, nil,
		"/bin/sh", "-c", "echo "+marker+" > \"$OMAC_CACHE_DIR/probe.txt\" && echo WROTE_MAIN")
	if code != 0 {
		t.Fatalf("main worktree launch failed (exit %d): %s", code, out)
	}

	out, code = runOmacShell(t, omacBin, home, linkedDir, nil,
		"/bin/sh", "-c", "cat \"$OMAC_CACHE_DIR/probe.txt\" 2>&1 || echo NOT_FOUND")
	if code != 0 {
		t.Fatalf("linked worktree launch failed (exit %d): %s", code, out)
	}
	if strings.Contains(out, marker) {
		t.Errorf("SECURITY: linked worktree saw main worktree's cache marker: %s", out)
	}
}

// TestE2ECachePersistentFailureSuggestsEphemeral: an unsafe symlink at
// ~/.cache/omac makes persistent launch fail (with a hint about
// --ephemeral-cache); retrying with --ephemeral-cache succeeds.
func TestE2ECachePersistentFailureSuggestsEphemeral(t *testing.T) {
	skipIfSandboxUnavailable(t)
	home := cacheTestHome(t)
	workdir := t.TempDir()
	writeCacheTestProfile(t, home, nil, nil, 0)
	omacBin := buildOmac(t)

	// Place an unsafe symlink at ~/.cache/omac.
	cacheDir := filepath.Join(home, ".cache")
	target := t.TempDir()
	if err := os.Chmod(target, 0o755); err != nil {
		t.Fatal(err)
	}
	omacCache := filepath.Join(cacheDir, "omac")
	if err := os.Symlink(target, omacCache); err != nil {
		t.Fatal(err)
	}

	out, code := runOmacShell(t, omacBin, home, workdir, nil,
		"/bin/sh", "-c", "echo SHOULD_NOT_REACH")
	if code == 0 {
		t.Fatalf("persistent launch should fail with unsafe cache root, but succeeded: %s", out)
	}
	if !strings.Contains(out, "--ephemeral-cache") {
		t.Errorf("failure output should suggest --ephemeral-cache: %s", out)
	}
	if !strings.Contains(out, omacCache) {
		t.Errorf("failure output should name the failed path %s: %s", omacCache, out)
	}

	// Retry with --ephemeral-cache: must succeed and must not create persistent state.
	out, code = runOmacShell(t, omacBin, home, workdir, []string{"--ephemeral-cache"},
		"/bin/sh", "-c", "echo REACHED_INNER")
	if code != 0 {
		t.Fatalf("ephemeral retry failed (exit %d): %s", code, out)
	}
	if !strings.Contains(out, "REACHED_INNER") {
		t.Errorf("ephemeral retry did not reach inner command: %s", out)
	}
}

// TestE2ECacheServeScopes: serve mode uses a serve-scoped persistent
// cache distinct from the workdir-scoped start cache.
func TestE2ECacheServeScopes(t *testing.T) {
	skipIfSandboxUnavailable(t)
	home := cacheTestHome(t)
	workdir := t.TempDir()
	writeCacheTestProfile(t, home, nil, nil, 0)
	omacBin := buildOmac(t)

	out, code := runOmacServeShell(t, omacBin, home, workdir, nil,
		"/bin/sh", "-c", "echo SERVE_CACHE=$OMAC_CACHE_DIR")
	if code != 0 {
		t.Fatalf("serve launch failed (exit %d): %s", code, out)
	}
	if !strings.Contains(out, "SERVE_CACHE=") {
		t.Errorf("serve did not expose OMAC_CACHE_DIR: %s", out)
	}

	// Start mode uses a workdir-scoped cache; serve uses serve-scoped.
	startOut, _ := runOmacShell(t, omacBin, home, workdir, nil,
		"/bin/sh", "-c", "echo START_CACHE=$OMAC_CACHE_DIR")
	if !strings.Contains(startOut, "START_CACHE=") {
		t.Errorf("start did not expose OMAC_CACHE_DIR: %s", startOut)
	}
	serveCache := extractEnv(out, "SERVE_CACHE=")
	startCache := extractEnv(startOut, "START_CACHE=")
	if serveCache == "" || startCache == "" {
		t.Fatalf("could not extract cache dirs: serve=%q start=%q", serveCache, startCache)
	}
	if serveCache == startCache {
		t.Errorf("serve and start caches should differ: serve=%s start=%s", serveCache, startCache)
	}
}

// TestE2ECacheActiveScopeCannotBeCleared: `omac cache clear` on an
// active workdir scope (lock held) reports "active" and leaves the
// scope directory intact.
func TestE2ECacheActiveScopeCannotBeCleared(t *testing.T) {
	home := cacheTestHome(t)
	workdir := t.TempDir()
	writeCacheTestProfile(t, home, nil, nil, 0)
	omacBin := buildOmac(t)

	// Prepare the persistent scope so the cache root + leaf exist on
	// disk. We invoke `omac provenance --json` (no sandbox needed) to
	// discover the scope path the same way the omac subprocess sees it.
	profPath := filepath.Join(home, ".config", "omac", "sandbox-profiles", "default.json")
	provCmd := exec.Command(omacBin, "provenance", "--profile", profPath, "--json")
	provCmd.Dir = workdir
	provCmd.Env = withHome(os.Environ(), home)
	provOut, err := provCmd.Output()
	if err != nil {
		t.Fatalf("omac provenance: %v\n%s", err, provOut)
	}
	var view struct {
		Cache struct {
			Path string `json:"path"`
		} `json:"cache"`
	}
	if err := json.Unmarshal(provOut, &view); err != nil {
		t.Fatalf("parse provenance JSON: %v", err)
	}
	scopeDir := view.Cache.Path
	if scopeDir == "" {
		t.Fatal("provenance cache path is empty")
	}

	// Hold the persistent scope lock open for the duration of the clear
	// attempt. PreparePersistent uses os.UserHomeDir() to compute the
	// cache root, so we must run it with HOME=home. We build and run a
	// tiny Go helper that acquires the lock and blocks until its stdin
	// is closed (so the test can release it by closing the pipe).
	helperSrc := `package main

import (
	"fmt"
	"os"

	"github.com/tngtech/oh-my-agentic-coder/internal/toolcache"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: hold-lock <workdir>")
		os.Exit(2)
	}
	scope, err := toolcache.PreparePersistent(toolcache.DomainWorkdir, os.Args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "PreparePersistent: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(scope.Dir)
	// Block until stdin is closed (parent closes the pipe to release).
	buf := make([]byte, 1)
	_, _ = os.Stdin.Read(buf)
	_ = scope.Close()
}
`
	// Place the helper inside a temp dir under the repo root so the
	// internal/toolcache import resolves against the module.
	repoRoot, _ := filepath.Abs(filepath.Join("..", ".."))
	helperDir := filepath.Join(repoRoot, ".tmp-hold-lock-"+filepath.Base(t.TempDir()))
	if err := os.MkdirAll(helperDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(helperDir) })
	if err := os.WriteFile(filepath.Join(helperDir, "main.go"), []byte(helperSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	helperPath := filepath.Join(helperDir, "hold-lock")
	buildCmd := exec.Command("go", "build", "-buildvcs=false", "-o", helperPath, ".")
	buildCmd.Dir = helperDir
	buildCmd.Env = os.Environ()
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("build hold-lock helper: %v\n%s", err, out)
	}

	// Start the helper with HOME=home so it computes the same cache root.
	holdReader, holdWriter, err := os.Pipe()
	if err != nil {
		t.Fatalf("create pipe: %v", err)
	}
	holdCmd := exec.Command(helperPath, workdir)
	holdCmd.Env = withHome(os.Environ(), home)
	holdCmd.Stdin = holdReader
	holdOut, err := holdCmd.StdoutPipe()
	if err != nil {
		t.Fatalf("capture hold-lock stdout: %v", err)
	}
	var holdErr bytes.Buffer
	holdCmd.Stderr = &holdErr
	if err := holdCmd.Start(); err != nil {
		t.Fatalf("start hold-lock helper: %v", err)
	}
	defer func() {
		holdWriter.Close()
		_ = holdCmd.Wait()
	}()
	ready, err := bufio.NewReader(holdOut).ReadString('\n')
	if err != nil {
		t.Fatalf("wait for hold-lock readiness: %v (stderr: %s)", err, holdErr.String())
	}
	if got := strings.TrimSpace(ready); got != scopeDir {
		t.Fatalf("hold-lock scope = %q; want %q (stderr: %s)", got, scopeDir, holdErr.String())
	}

	// Now clear: the scope is active (lock held) so it should report
	// "active" and leave the scope dir intact.
	clearCmd := exec.Command(omacBin, "cache", "clear")
	clearCmd.Dir = workdir
	clearCmd.Env = withHome(os.Environ(), home)
	clearOut, err := clearCmd.CombinedOutput()
	if err != nil {
		t.Logf("omac cache clear returned error (expected for active scope): %v\n%s", err, clearOut)
	}
	outStr := string(clearOut)
	if !strings.Contains(outStr, "active") {
		t.Errorf("expected 'active' status for held scope; got %s", outStr)
	}
	// The scope directory must survive the clear.
	if _, err := os.Stat(scopeDir); err != nil {
		t.Errorf("active scope directory was removed: %v (output: %s)", err, outStr)
	}
	t.Logf("omac cache clear output: %s", outStr)
}

// TestE2ECacheTools: deterministic tool probes with no external-network dependency.
// Each tool writes to its isolated cache under OMAC_CACHE_DIR. Missing
// optional tools skip locally; CI installs all four.
func TestE2ECacheTools(t *testing.T) {
	skipIfSandboxUnavailable(t)
	home := cacheTestHome(t)
	workdir := t.TempDir()
	runtimeReadPaths := toolRuntimeReadPaths(t)
	// rustup shims try to mkdir ~/.rustup/update-hashes on every
	// invocation; grant it writable so the shim doesn't fail. The
	// directory holds integrity hashes, not credentials.
	rustupAllowPaths := rustupUpdateHashesDirs(t)
	for _, d := range rustupAllowPaths {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Logf("could not pre-create %s: %v", d, err)
		} else {
			t.Logf("pre-created rustup update-hashes dir: %s", d)
		}
	}
	writeCacheTestProfile(t, home, runtimeReadPaths, rustupAllowPaths, 0)
	omacBin := buildOmac(t)

	t.Run("go", func(t *testing.T) {
		requireTool(t, "go", "GOCACHE/GOMODCACHE probe")
		runToolRuntimeProbe(t, omacBin, home, workdir, nil, "go version", "GO_RUNTIME_OK")
		// Compile a local Go package and assert GOCACHE gains entries.
		pkgDir := filepath.Join(workdir, "probe-pkg")
		if err := os.MkdirAll(pkgDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(pkgDir, "main.go"), []byte("package main\nfunc main(){}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(pkgDir, "go.mod"), []byte("module probe\n\ngo 1.21\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		out, code := runOmacShellWithEnv(t, omacBin, home, workdir, nil, []string{"PROBE_GO_DIR=" + pkgDir},
			"/bin/sh", "-c",
			"cd \"$PROBE_GO_DIR\" && go build -o /dev/null . && "+
				"echo GOCACHE=$GOCACHE && "+
				"test -d \"$GOCACHE\" && echo GOCACHE_EXISTS && "+
				"test -n \"$(ls -A \"$GOCACHE\")\" && echo GOCACHE_NONEMPTY")
		if code != 0 {
			t.Fatalf("go build probe failed (exit %d): %s", code, out)
		}
		if !strings.Contains(out, "GOCACHE_EXISTS") {
			t.Errorf("GOCACHE directory was not created: %s", out)
		}
		if !strings.Contains(out, "GOCACHE_NONEMPTY") {
			t.Errorf("GOCACHE has no entries after build: %s", out)
		}
	})

	t.Run("go module proxy", func(t *testing.T) {
		requireTool(t, "go", "GOMODCACHE probe")
		runToolRuntimeProbe(t, omacBin, home, workdir, nil, "go version", "GO_MODULE_RUNTIME_OK")
		// Generate a local file-based Go module proxy with one fixture module.
		proxyDir := filepath.Join(workdir, "goproxy")
		if err := os.MkdirAll(proxyDir, 0o755); err != nil {
			t.Fatal(err)
		}
		modName := "omac.cache/probe"
		modVersion := "v0.1.0"
		// Write the .info and .mod files; go will download from GOPROXY=file://.
		infoJSON := `{"Version":"` + modVersion + `","Time":"2026-01-01T00:00:00Z"}`
		writeProxyFile(t, proxyDir, modName, modVersion, "info", infoJSON)
		writeProxyFile(t, proxyDir, modName, modVersion, "mod", "module "+modName+"\n\ngo 1.21\n")
		writeProxyFileBytes(t, proxyDir, modName, modVersion, "zip", makeModuleZip(modName, modVersion))

		consumerDir := filepath.Join(workdir, "modconsumer")
		if err := os.MkdirAll(consumerDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(consumerDir, "go.mod"),
			[]byte("module consumer\n\ngo 1.21\n\nrequire "+modName+" "+modVersion+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(consumerDir, "main.go"),
			[]byte("package main\nimport _ \""+modName+"\"\nfunc main(){}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		out, code := runOmacShellWithEnv(t, omacBin, home, workdir, nil, []string{
			"PROBE_CONSUMER_DIR=" + consumerDir,
			"PROBE_GO_PROXY=" + proxyDir,
		},
			"/bin/sh", "-c",
			"cd \"$PROBE_CONSUMER_DIR\" && "+
				"GOPROXY=\"file://$PROBE_GO_PROXY\" GOFLAGS=-mod=mod GONOSUMDB=1 GOSUMDB=off GOPATH=\"$OMAC_CACHE_DIR/go-path\" "+
				"go mod download "+modName+" && "+
				"echo GOMODCACHE=$GOMODCACHE && "+
				"test -d \"$GOMODCACHE\" && echo GOMODCACHE_EXISTS && "+
				"test -n \"$(ls -A \"$GOMODCACHE\")\" && echo GOMODCACHE_NONEMPTY")
		if code != 0 {
			t.Fatalf("go mod download probe failed (exit %d): %s", code, out)
		}
		if !strings.Contains(out, "GOMODCACHE_NONEMPTY") {
			t.Errorf("GOMODCACHE has no entries after download: %s", out)
		}
	})

	t.Run("npm", func(t *testing.T) {
		requireTool(t, "npm", "npm cache probe")
		runToolRuntimeProbe(t, omacBin, home, workdir, nil, "npm --version", "NPM_RUNTIME_OK")
		pkgDir := filepath.Join(workdir, "npm-pkg")
		if err := os.MkdirAll(pkgDir, 0o755); err != nil {
			t.Fatal(err)
		}
		// Create a local npm package fixture.
		pkgJSON := `{"name":"omac-cache-probe","version":"0.0.1"}`
		if err := os.WriteFile(filepath.Join(pkgDir, "package.json"), []byte(pkgJSON), 0o644); err != nil {
			t.Fatal(err)
		}
		out, code := runOmacShellWithEnv(t, omacBin, home, workdir, nil, []string{"PROBE_NPM_DIR=" + pkgDir},
			"/bin/sh", "-c",
			"cd \"$PROBE_NPM_DIR\" && npm pack >/dev/null 2>&1 && npm cache add omac-cache-probe-0.0.1.tgz >/dev/null 2>&1 && echo NPM_ADDED; "+
				"echo NPM_CACHE=$NPM_CONFIG_CACHE && "+
				"test -d \"$NPM_CONFIG_CACHE\" && echo NPM_CACHE_EXISTS && "+
				"test -n \"$(ls -A \"$NPM_CONFIG_CACHE\")\" && echo NPM_CACHE_NONEMPTY")
		if code != 0 {
			t.Fatalf("npm cache probe failed (exit %d): %s", code, out)
		}
		if !strings.Contains(out, "NPM_ADDED") {
			t.Errorf("npm cache add did not succeed: %s", out)
		}
		if !strings.Contains(out, "NPM_CACHE_EXISTS") {
			t.Errorf("npm cache directory was not created: %s", out)
		}
		if !strings.Contains(out, "NPM_CACHE_NONEMPTY") {
			t.Errorf("npm cache directory is empty after cache add: %s", out)
		}
	})

	t.Run("pip", func(t *testing.T) {
		requireTool(t, "python3", "pip cache probe")
		runToolRuntimeProbe(t, omacBin, home, workdir, nil, "python3 --version", "PYTHON_RUNTIME_OK")
		// Start the loopback HTTP fixture BEFORE writing the test profile.
		fixture := newPipFixture(t)
		fixture.start()
		defer fixture.stop()

		// Write a profile that opens the fixture's port. Preserve the
		// rustup update-hashes writable grant so subsequent rustc/cargo
		// subtests (which reuse this profile) still pass: the rustup
		// proxy shim unconditionally mkdirs ~/.rustup/update-hashes on
		// every invocation and fails if the parent isn't writable.
		writeCacheTestProfile(t, home, runtimeReadPaths, rustupAllowPaths, fixture.port)

		// Generate a local source archive served from the fixture.
		archive := fixture.serveSourceArchive(t)

		out, code := runOmacShellWithEnv(t, omacBin, home, workdir, nil, []string{
			"PROBE_PIP_INDEX_URL=" + archive.url,
			"PROBE_PIP_TARGET=" + filepath.Join(workdir, "pip-install"),
		},
			"/bin/sh", "-c",
			"python3 -m pip download --no-deps --dest \"$PROBE_PIP_TARGET\" --index-url \"$PROBE_PIP_INDEX_URL\" --trusted-host 127.0.0.1 omac-cache-probe 2>&1 && "+
				"test -d \"$PIP_CACHE_DIR\" && echo PIP_CACHE_EXISTS && "+
				"test -n \"$(ls -A \"$PIP_CACHE_DIR\")\" && echo PIP_CACHE_NONEMPTY && "+
				"python3 -m pip cache purge >/dev/null 2>&1; true")
		if code != 0 {
			t.Fatalf("pip cache probe failed (exit %d): %s", code, out)
		}
		if !strings.Contains(out, "PIP_CACHE_EXISTS") {
			t.Errorf("PIP_CACHE_DIR was not created: %s", out)
		}
		if !strings.Contains(out, "PIP_CACHE_NONEMPTY") {
			t.Errorf("PIP_CACHE_DIR is empty after download: %s", out)
		}
	})

	t.Run("rustc", func(t *testing.T) {
		requireTool(t, "rustc", "Rust runtime probe")
		runToolRuntimeProbe(t, omacBin, home, workdir, cargoRuntimeEnv(t), "rustc --version", "RUSTC_RUNTIME_OK")
	})

	t.Run("cargo", func(t *testing.T) {
		requireTool(t, "cargo", "CARGO_HOME probe")
		requireTool(t, "rustc", "Cargo compiler probe")
		rustupEnv := cargoRuntimeEnv(t)
		runToolRuntimeProbe(t, omacBin, home, workdir, rustupEnv, "cargo --version", "CARGO_RUNTIME_OK")
		// Create the four Cargo config/credential files on the host with
		// distinct markers so we can assert their contents are denied
		// inside the sandbox and not copied into the isolated CARGO_HOME.
		hostCargo := filepath.Join(home, ".cargo")
		markers := make(map[string]string)
		for _, name := range []string{"config", "config.toml", "credentials", "credentials.toml"} {
			marker := "host-cargo-" + name + "-secret"
			markers[name] = marker
			if err := os.WriteFile(filepath.Join(hostCargo, name), []byte(marker), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		// Copy the rust fixture into the workdir.
		fixtureDir := filepath.Join(workdir, "rust-cache-probe")
		if err := os.MkdirAll(filepath.Join(fixtureDir, "src"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(fixtureDir, "Cargo.toml"),
			[]byte("[package]\nname = \"omac-cache-probe\"\nversion = \"0.1.0\"\nedition = \"2021\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(fixtureDir, "src", "main.rs"),
			[]byte("fn main() { println!(\"cache probe\"); }\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		out, code := runOmacShellWithEnv(t, omacBin, home, workdir, nil, append(rustupEnv,
			"PROBE_CARGO_MANIFEST="+filepath.Join(fixtureDir, "Cargo.toml"),
			"PROBE_HOST_CARGO="+hostCargo,
		),
			"/bin/sh", "-c",
			"cargo --version && echo CARGO_RUNTIME_OK && "+
				"rustc --version && echo RUSTC_RUNTIME_OK && "+
				"cargo build --manifest-path \"$PROBE_CARGO_MANIFEST\" && echo CARGO_BUILD_OK && "+
				"echo CARGO_HOME=$CARGO_HOME && "+
				"test -d \"$CARGO_HOME\" && echo CARGO_HOME_EXISTS && "+
				"echo probe > \"$CARGO_HOME/probe.txt\" && cat \"$CARGO_HOME/probe.txt\" && "+
				"for name in config config.toml credentials credentials.toml; do test ! -f \"$CARGO_HOME/$name\" || exit 1; done && "+
				"echo NO_HOST_CARGO_FILES_COPIED && "+
				"for name in config config.toml credentials credentials.toml; do if test -r \"$PROBE_HOST_CARGO/$name\"; then echo CARGO_HOST_${name}_READABLE; else echo CARGO_HOST_${name}_DENIED; fi; done")
		if code != 0 {
			t.Fatalf("cargo build probe failed (exit %d): %s", code, out)
		}
		if !strings.Contains(out, "CARGO_HOME_EXISTS") {
			t.Errorf("CARGO_HOME directory was not created: %s", out)
		}
		if !strings.Contains(out, "CARGO_BUILD_OK") {
			t.Errorf("cargo build did not complete: %s", out)
		}
		if !strings.Contains(out, "probe") {
			t.Errorf("CARGO_HOME write/read probe failed: %s", out)
		}
		for name, marker := range markers {
			if strings.Contains(out, "CARGO_HOST_"+name+"_DENIED") {
				continue // denied as expected
			}
			// On Linux, t.TempDir() is under /tmp which the baseline
			// grants writable, so ~/.cargo is readable. The isolation
			// guarantee is that the files are NOT copied into the
			// isolated CARGO_HOME (checked below), not that the host
			// files are denied when home is under a writable temp.
			if !strings.Contains(out, "CARGO_HOST_"+name+"_READABLE") {
				t.Errorf("SECURITY: ~/.cargo/%s denial/readable status not reported: %s", name, out)
			}
			if strings.Contains(out, marker) {
				t.Errorf("SECURITY: host Cargo marker for %s leaked: %s", name, out)
			}
		}
		if !strings.Contains(out, "NO_HOST_CARGO_FILES_COPIED") {
			t.Errorf("SECURITY: host cargo config/credential files were copied into isolated CARGO_HOME: %s", out)
		}
	})
}

// TestE2ECacheProvenance: `omac provenance --json` reports a cache
// object whose path/mappings match what a real shell launch observes.
// Does NOT call providerFor, runAgent, or runAuditAgent.
func TestE2ECacheProvenance(t *testing.T) {
	home := cacheTestHome(t)
	workdir := t.TempDir()
	writeCacheTestProfile(t, home, nil, nil, 0)
	omacBin := buildOmac(t)

	// Invoke provenance directly.
	profPath := filepath.Join(home, ".config", "omac", "sandbox-profiles", "default.json")
	cmd := exec.Command(omacBin, "provenance", "--profile", profPath, "--json")
	cmd.Dir = workdir
	cmd.Env = withHome(os.Environ(), home)
	provOut, err := cmd.Output()
	if err != nil {
		t.Fatalf("omac provenance: %v\n%s", err, provOut)
	}
	var view struct {
		Cache struct {
			Scope       string            `json:"scope"`
			Mode        string            `json:"mode"`
			Path        string            `json:"path"`
			Environment map[string]string `json:"environment"`
		} `json:"cache"`
	}
	if err := json.Unmarshal(provOut, &view); err != nil {
		t.Fatalf("parse provenance JSON: %v\n%s", err, provOut)
	}
	if view.Cache.Path == "" {
		t.Fatal("provenance cache path is empty")
	}
	if view.Cache.Mode != string(toolcache.ModePersistent) {
		t.Errorf("provenance cache mode = %q; want %q", view.Cache.Mode, toolcache.ModePersistent)
	}

	// Compare with a real shell launch. This requires the sandbox to
	// be applicable; skip the cross-check when the local environment
	// cannot run the omac sandbox (CI always can). Use a subtest so
	// only the cross-check is skipped, not the provenance assertions.
	t.Run("shell_cross_check", func(t *testing.T) {
		skipIfSandboxUnavailable(t)
		shellOut, code := runOmacShell(t, omacBin, home, workdir, nil,
			"/bin/sh", "-c", "echo OMAC_CACHE_DIR=$OMAC_CACHE_DIR")
		if code != 0 {
			t.Fatalf("shell launch failed (exit %d): %s", code, shellOut)
		}
		shellCacheDir := extractEnv(shellOut, "OMAC_CACHE_DIR=")
		if shellCacheDir == "" {
			t.Fatalf("shell launch did not expose OMAC_CACHE_DIR: %s", shellOut)
		}
		// On macOS, /var is a symlink to /private/var. The provenance
		// command uses the un-resolved HOME, while the sandboxed shell
		// sees the resolved path. Canonicalize both before comparing.
		provPath, _ := filepath.EvalSymlinks(view.Cache.Path)
		shellPath, _ := filepath.EvalSymlinks(shellCacheDir)
		if provPath != shellPath {
			t.Errorf("provenance cache path %q != shell OMAC_CACHE_DIR %q", view.Cache.Path, shellCacheDir)
		}
	})

	// Verify all supported cache redirects match the selected scope exactly.
	if err := validateCacheRedirects(view.Cache.Path, view.Cache.Environment); err != nil {
		t.Error(err)
	}
}

// --- helpers ---

// extractEnv pulls "KEY=VALUE" from output, returning VALUE.
func extractEnv(out, prefix string) string {
	for _, line := range strings.Split(out, "\n") {
		if i := strings.Index(line, prefix); i >= 0 {
			return strings.TrimSpace(line[i+len(prefix):])
		}
	}
	return ""
}

// gitInHomeDir runs git in dir during test setup, with a hermetic identity.
func gitInHomeDir(t *testing.T, dir string, args ...string) {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	c.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// writeProxyFile writes a Go module proxy file under proxyDir for the
// given module/version with the given extension ("info", "mod", "zip").
func writeProxyFile(t *testing.T, proxyDir, modName, modVersion, ext, content string) {
	t.Helper()
	writeProxyFileBytes(t, proxyDir, modName, modVersion, ext, []byte(content))
}

// writeProxyFileBytes is the []byte variant for binary content (zip).
func writeProxyFileBytes(t *testing.T, proxyDir, modName, modVersion, ext string, content []byte) {
	t.Helper()
	// Go encodes module paths with case-encoding: uppercase -> "!lower".
	encoded := encodeModulePath(modName)
	dir := filepath.Join(proxyDir, encoded, "@v")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, modVersion+"."+ext), content, 0o644); err != nil {
		t.Fatal(err)
	}
}

// encodeModulePath lowercases uppercase letters and prefixes them with '!'.
func encodeModulePath(mod string) string {
	var b strings.Builder
	for _, r := range mod {
		if r >= 'A' && r <= 'Z' {
			b.WriteByte('!')
			b.WriteRune(r + 32)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// makeModuleZip returns a minimal valid zip for a Go module. Go's
// moddownload requires a real zip archive with the module path prefix.
// We use archive/zip (Deflate) so the local file headers precede the
// data — a hand-rolled writer that wrote data first produced an archive
// archive/zip reads 0 files from, causing `go mod download` to fail.
func makeModuleZip(modName, modVersion string) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	entry := modName + "@" + modVersion + "/README"
	w, err := zw.Create(entry)
	if err != nil {
		panic(err)
	}
	_, _ = w.Write([]byte("omac cache probe module\n"))
	_ = zw.Close()
	return buf.Bytes()
}

// pipFixture serves a minimal source archive over loopback HTTP so pip
// can install it with --no-deps --no-build-isolation (no external fetch).
type pipFixture struct {
	archive []byte
	port    int
	srv     *httptest.Server
}

func newPipFixture(t *testing.T) *pipFixture {
	t.Helper()
	return &pipFixture{archive: omacCacheProbeWheel(t)}
}

func (p *pipFixture) start() {
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// pip only caches responses from trusted (HTTPS or explicitly
		// trusted) hosts — see pip's InsecureHTTPAdapter vs.
		// CacheControlAdapter mount in pip._internal.network.session.
		// The pip subtest passes --trusted-host 127.0.0.1 so the
		// InsecureCacheControlAdapter (which does cache) is used for
		// our loopback HTTP fixture. cachecontrol additionally requires
		// an ETag (or Date + Cache-Control: max-age) to deem a response
		// cacheable; without it pip never writes to PIP_CACHE_DIR.
		w.Header().Set("ETag", `"omac-pip-fixture"`)
		// pip --index-url points at /simple/. pip fetches the package list
		// from /simple/ (top-level) and then /simple/<name>/ (per-package).
		if r.URL.Path == "/simple/" || r.URL.Path == "/simple" {
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<!DOCTYPE html><html><body><a href="/simple/omac-cache-probe/">omac-cache-probe</a></body></html>`)
			return
		}
		if r.URL.Path == "/simple/omac-cache-probe/" {
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<!DOCTYPE html><html><body><a href="/packages/omac_cache_probe-0.0.1-py3-none-any.whl">omac-cache-probe-0.0.1</a></body></html>`)
			return
		}
		if r.URL.Path == "/packages/omac_cache_probe-0.0.1-py3-none-any.whl" {
			w.Header().Set("Content-Type", "application/zip")
			_, _ = w.Write(p.archive)
			return
		}
		http.NotFound(w, r)
	}))
	// Extract the port from the listener address.
	addr := p.srv.Listener.Addr().String()
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		fmt.Sscanf(addr[i+1:], "%d", &p.port)
	}
}

func (p *pipFixture) stop() {
	if p.srv != nil {
		p.srv.Close()
	}
}

type sourceArchive struct {
	url  string
	data []byte
}

func TestPipSourceArchiveIsZip(t *testing.T) {
	fixture := newPipFixture(t)
	fixture.start()
	defer fixture.stop()

	archive := fixture.serveSourceArchive(t)
	zr, err := zip.NewReader(bytes.NewReader(archive.data), int64(len(archive.data)))
	if err != nil {
		t.Fatalf("open source archive as zip: %v", err)
	}
	for _, f := range zr.File {
		if f.Name == "omac_cache_probe.py" {
			return
		}
	}
	t.Fatal("source archive does not contain omac_cache_probe.py")
}

func (p *pipFixture) serveSourceArchive(t *testing.T) sourceArchive {
	t.Helper()
	return sourceArchive{url: p.srv.URL + "/simple/", data: p.archive}
}

// omacCacheProbeWheel returns a minimal valid wheel for a Python package
// named omac-cache-probe. A wheel doesn't require setuptools at install
// time (unlike an sdist), so it works with --no-build-isolation even when
// the system Python lacks setuptools.
func omacCacheProbeWheel(t *testing.T) []byte {
	t.Helper()
	var data bytes.Buffer
	zw := zip.NewWriter(&data)
	distInfo := "omac_cache_probe-0.0.1.dist-info"
	files := []struct {
		name string
		body string
	}{
		{distInfo + "/METADATA", "Metadata-Version: 2.1\nName: omac-cache-probe\nVersion: 0.0.1\nSummary: cache probe\n"},
		{distInfo + "/WHEEL", "Wheel-Version: 1.0\nGenerator: omac-test\nRoot-Is-Purelib: true\nTag: py3-none-any\n"},
		{distInfo + "/RECORD", "omac_cache_probe.py,sha256=__,0\n" + distInfo + "/METADATA,sha256=__,0\n" + distInfo + "/WHEEL,sha256=__,0\n"},
		{"omac_cache_probe.py", "VALUE = 'cache-probe'\n"},
	}
	for _, f := range files {
		w, err := zw.Create(f.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(w, f.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}
