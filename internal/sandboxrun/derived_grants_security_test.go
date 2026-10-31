package sandboxrun

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

// requireSandboxBackend skips on platforms without the sandbox CLI of
// the built-in backend. No build tags, so this file compiles everywhere.
func requireSandboxBackend(t *testing.T) {
	t.Helper()
	switch runtime.GOOS {
	case "linux":
		if _, err := exec.LookPath("bwrap"); err != nil {
			t.Skip("bubblewrap (bwrap) not available on PATH")
		}
	case "darwin":
		if _, err := exec.LookPath("sandbox-exec"); err != nil {
			t.Skip("sandbox-exec not available on PATH")
		}
	default:
		t.Skipf("the built-in sandbox is unsupported on %s", runtime.GOOS)
	}
}

// minimalDerivedGrants is the smallest grant set a spawned child gets:
// the workdir is the writable surface, everything else stays closed.
// Open networking with env-only enforcement keeps the spawn argv free of
// kernel-gate machinery, so the assertions see the derived mounts only.
func minimalDerivedGrants(wd, home string) *Grants {
	return &Grants{
		Workdir:     wd,
		HomeDir:     home,
		AllowPaths:  []string{wd},
		NetworkMode: sandboxprofile.ModeOpen,
		Enforcement: sandboxprofile.EnforceEnvOnly,
	}
}

// writeExecScript writes content and marks it executable, so command
// resolution accepts it as an inner command or shebang interpreter.
func writeExecScript(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

// stableHome stages HOME below the package directory: outside the
// writable surface, outside every temp root, and outside the real home.
// A home under TMPDIR (what t.TempDir gives) would itself count as
// agent-writable surface under the derived-grant taint policy, so a
// boundary refusal could then fire for the taint reason instead.
func stableHome(t *testing.T) string {
	t.Helper()
	home, err := os.MkdirTemp(".", "omac-security-home-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	if home, err = filepath.Abs(home); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	return home
}

// configRootDir returns a fresh directory below the (staged) home's
// config root: dotfile config trees must never be granted silently.
func configRootDir(t *testing.T, home string) string {
	t.Helper()
	base := filepath.Join(home, ".config")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(base, "omac-security-")
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// stableToolTree builds an install tree below the package directory,
// the way a user-managed tool runtime lives on a real host: shim links,
// install dirs, an interpreter dir. It sits outside the writable
// surface, outside every temp root, and outside the staged home, so the
// confined process can read it but never modify it.
func stableToolTree(t *testing.T) (links, installs, runtimeDir string) {
	t.Helper()
	root, err := os.MkdirTemp(".", "omac-security-ctrl-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if root, err = filepath.Abs(root); err != nil {
		t.Fatal(err)
	}
	links = filepath.Join(root, "links")
	installs = filepath.Join(root, "installs", "v1")
	runtimeDir = filepath.Join(root, "runtime")
	for _, dir := range []string{links, installs, runtimeDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return links, installs, runtimeDir
}

// requireDerivedBind asserts dir was derived into the spawn argv as a
// read-only grant. On Linux that is a --ro-bind pair; on macOS the
// directory appears in the generated Seatbelt profile text.
func requireDerivedBind(t *testing.T, argv []string, dir string) {
	t.Helper()
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, dir) {
		t.Errorf("the interpreter directory %q was not derived into the spawn argv: %s", dir, joined)
	}
	if runtime.GOOS == "linux" && !strings.Contains(joined, "--ro-bind "+dir+" "+dir) {
		t.Errorf("the interpreter directory %q is not a read-only mount in the spawn argv: %s", dir, joined)
	}
}

// TestSecurityDerivedGrantsRefuseAgentWritableInnerCommand guards the
// derived-grant policy at the spawn boundary: an inner command the
// confined process can rewrite (it lives in the workdir) must not
// extend the next sandbox's read surface via its shebang, here into
// the user's config root. The spawn must fail closed and name the
// directory it refused to derive. The control case runs first: an
// identical spawn whose inner command lives in untainted content still
// derives its interpreter dir, so the refusal can only come from the
// agent-writability of the inner command, never from the environment
// or a blanket suppression of shebang derivation.
func TestSecurityDerivedGrantsRefuseAgentWritableInnerCommand(t *testing.T) {
	requireSandboxBackend(t)

	home := stableHome(t)
	wd := t.TempDir()
	_, installs, runtimeDir := stableToolTree(t)
	g := minimalDerivedGrants(wd, home)

	t.Run("untainted command still derives its interpreter dir", func(t *testing.T) {
		interp := filepath.Join(runtimeDir, "interp")
		writeExecScript(t, interp, "#!/bin/sh\n")
		benign := filepath.Join(installs, "tool")
		writeExecScript(t, benign, "#!"+interp+"\n")
		argv, err := BuildChildArgv(g, []string{benign})
		if err != nil {
			t.Fatalf("an inner command outside the writable surface must keep deriving, got: %v", err)
		}
		requireDerivedBind(t, argv, runtimeDir)
	})

	t.Run("workdir command with a shebang into the config root is refused", func(t *testing.T) {
		secretDir := configRootDir(t, home)
		interp := filepath.Join(secretDir, "interp")
		writeExecScript(t, interp, "#!/bin/sh\n")
		tool := filepath.Join(wd, "tool")
		writeExecScript(t, tool, "#!"+interp+"\n")
		argv, err := BuildChildArgv(g, []string{tool})
		if err == nil {
			t.Fatalf("spawn argv was built for an inner command planted in the workdir; expected a refusal that names %q", secretDir)
		}
		if argv != nil {
			t.Errorf("the spawn must fail closed, not return a partial grant set: %v", argv)
		}
		if !strings.Contains(err.Error(), secretDir) {
			t.Errorf("refusal does not name the derived target directory %q: %v", secretDir, err)
		}
	})
}

// TestSecuritySymlinkChainGrantBounded guards symlink-chain derivations:
// a chain that starts in the writable surface is refused even when it
// ends in trusted content; a chain entirely outside the writable
// surface keeps resolving the way version-manager shims do; and no
// derivation may cross into the home/config boundary without an
// explicit user grant.
func TestSecuritySymlinkChainGrantBounded(t *testing.T) {
	requireSandboxBackend(t)

	home := stableHome(t)
	wd := t.TempDir()
	links, installs, runtimeDir := stableToolTree(t)
	g := minimalDerivedGrants(wd, home)

	t.Run("chain outside the writable surface still resolves", func(t *testing.T) {
		interp := filepath.Join(runtimeDir, "interp")
		writeExecScript(t, interp, "#!/bin/sh\n")
		real := filepath.Join(installs, "real")
		writeExecScript(t, real, "#!"+interp+"\n")
		shim := filepath.Join(links, "tool")
		if err := os.Symlink(real, shim); err != nil {
			t.Fatal(err)
		}
		argv, err := BuildChildArgv(g, []string{shim})
		if err != nil {
			t.Fatalf("a chain entirely outside the writable surface must keep resolving, got: %v", err)
		}
		requireDerivedBind(t, argv, runtimeDir)
	})

	t.Run("writable first hop refused despite trusted target", func(t *testing.T) {
		target := filepath.Join(installs, "real-a")
		writeExecScript(t, target, "#!/bin/sh\n")
		hop := filepath.Join(wd, "hop")
		if err := os.Symlink(target, hop); err != nil {
			t.Fatal(err)
		}
		if _, err := BuildChildArgv(g, []string{hop}); err == nil {
			t.Errorf("spawn argv was built for a chain whose first hop (%q) lies in the writable workdir; expected a refusal", hop)
		}
	})

	t.Run("home boundary needs an explicit grant", func(t *testing.T) {
		secretDir := configRootDir(t, home)
		target := filepath.Join(secretDir, "tool")
		writeExecScript(t, target, "#!/bin/sh\n")
		hop := filepath.Join(links, "home-hop")
		if err := os.Symlink(target, hop); err != nil {
			t.Fatal(err)
		}
		if _, err := BuildChildArgv(g, []string{hop}); err == nil {
			t.Errorf("spawn argv was built while deriving a grant below the user's config root %q; expected a refusal", secretDir)
		}
	})
}
