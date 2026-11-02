package sandboxrun

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

// These fixtures live outside temp roots unless the test deliberately marks
// them writable, so refusals cannot accidentally pass for temp-directory taint.
func review446Root(t *testing.T) string {
	t.Helper()
	p, err := os.MkdirTemp(".", "review446-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(p) })
	p, err = filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func review446Script(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestDerivedGrantsWritableSupportAlias(t *testing.T) {
	home, wd := stableHome(t), t.TempDir()
	secret := filepath.Join(home, ".config", "private")
	if err := os.MkdirAll(secret, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secret, "token"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	tool := filepath.Join(wd, "prefix", "bin", "tool")
	review446Script(t, tool)
	lib := filepath.Join(wd, "prefix", "lib")
	if err := os.Symlink(secret, lib); err != nil {
		t.Fatal(err)
	}
	p := sandboxprofile.DefaultProfile()
	p.Network.Mode = sandboxprofile.ModeOpen
	g, err := ResolveGrants(p, wd, nil)
	if err != nil {
		t.Fatal(err)
	}
	dirs, err := resolveInnerBinaryDirs([]string{tool}, derivedGrantPolicyForGrants(g))
	if err == nil {
		copy := *g
		copy.ReadPaths = append(copy.ReadPaths, dirs...)
		needle := "(allow file-read* (subpath " + sbplQuote(secret) + "))"
		t.Logf("before exposes secret=%v; after exposes secret=%v; dirs=%v", strings.Contains(GenerateSBPL(g), needle), strings.Contains(GenerateSBPL(&copy), needle), dirs)
		if runtime.GOOS == "darwin" {
			argv, spawnErr := BuildChildArgv(g, []string{tool})
			t.Logf("actual BuildChildArgv err=%v; grants secret=%v", spawnErr, len(argv) > 2 && strings.Contains(argv[2], needle))
		}
		t.Fatal("a workdir lib symlink is not an explicit grant of its sensitive target")
	}
}

func TestDerivedGrantsSupportAliasReplacedAfterCheck(t *testing.T) {
	home, wd := stableHome(t), t.TempDir()
	secret := filepath.Join(home, ".config", "private")
	benign := filepath.Join(wd, "benign")
	for _, dir := range []string{secret, benign} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	tool := filepath.Join(wd, "prefix", "bin", "tool")
	review446Script(t, tool)
	lib := filepath.Join(wd, "prefix", "lib")
	if err := os.Symlink(benign, lib); err != nil {
		t.Fatal(err)
	}
	p := sandboxprofile.DefaultProfile()
	p.Network.Mode = sandboxprofile.ModeOpen
	g, err := ResolveGrants(p, wd, nil)
	if err != nil {
		t.Fatal(err)
	}
	dirs, err := resolveInnerBinaryDirs([]string{tool}, derivedGrantPolicyForGrants(g))
	if err != nil {
		t.Fatalf("initially fully covered command must be allowed: %v", err)
	}
	if err := os.Remove(lib); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, lib); err != nil {
		t.Fatal(err)
	}
	copy := *g
	copy.ReadPaths = append(copy.ReadPaths, dirs...)
	if strings.Contains(GenerateSBPL(&copy), "(allow file-read* (subpath "+sbplQuote(secret)+"))") {
		t.Fatal("validated benign support alias was re-resolved to a sensitive target during backend lowering")
	}
}

func TestDerivedGrantsWritableAncestorSymlink(t *testing.T) {
	for _, kind := range []string{"command", "interpreter", "support"} {
		t.Run(kind, func(t *testing.T) {
			root, wd := review446Root(t), t.TempDir()
			home := filepath.Join(root, "home")
			if err := os.Mkdir(home, 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("HOME", home)
			outside := filepath.Join(root, "ungranted")
			review446Script(t, filepath.Join(outside, "tool"))
			if err := os.WriteFile(filepath.Join(outside, "private-data"), []byte("secret"), 0o600); err != nil {
				t.Fatal(err)
			}
			redirect := filepath.Join(wd, "redirect")
			if err := os.Symlink(outside, redirect); err != nil {
				t.Fatal(err)
			}
			alias := filepath.Join(root, "alias")
			if err := os.Symlink(redirect, alias); err != nil {
				t.Fatal(err)
			}
			tool := filepath.Join(alias, "tool")
			if kind == "interpreter" {
				tool = filepath.Join(root, "trusted", "tool")
				review446Script(t, tool)
				if err := os.WriteFile(tool, []byte("#!"+filepath.Join(alias, "tool")+"\n"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "support" {
				tool = filepath.Join(root, "prefix", "bin", "tool")
				review446Script(t, tool)
				if err := os.Symlink(redirect, filepath.Join(root, "prefix", "lib")); err != nil {
					t.Fatal(err)
				}
			}
			g := minimalDerivedGrants(wd, home)
			dirs, err := resolveInnerBinaryDirs([]string{tool}, derivedGrantPolicyForGrants(g))
			if err == nil {
				copy := *g
				copy.ReadPaths = append(copy.ReadPaths, dirs...)
				profile := GenerateSBPL(&copy)
				bwrap, berr := BuildBwrapArgv(&copy, []string{"/bin/sh"})
				t.Logf("derived=%v; SBPL exposes private-data parent=%v; bwrap err=%v argv=%v", dirs, strings.Contains(profile, "(allow file-read* (subpath "+sbplQuote(outside)+"))"), berr, bwrap)
				if runtime.GOOS == "darwin" {
					argv, spawnErr := BuildChildArgv(g, []string{tool})
					t.Logf("actual BuildChildArgv err=%v; grants outside=%v", spawnErr, len(argv) > 2 && strings.Contains(argv[2], "(allow file-read* (subpath "+sbplQuote(outside)+"))"))
				}
				t.Fatalf("must reject %s resolution through agent-replaceable ancestor %s", kind, redirect)
			}
		})
	}
}

func TestDerivedGrantsSensitiveHomeAncestor(t *testing.T) {
	root := review446Root(t)
	home := filepath.Join(root, "home")
	review446Script(t, filepath.Join(home, ".config", "private", "tool"))
	t.Setenv("HOME", home)
	tool := filepath.Join(root, "tool")
	review446Script(t, tool)
	g := minimalDerivedGrants(t.TempDir(), home)
	dirs, err := resolveInnerBinaryDirs([]string{tool}, derivedGrantPolicyForGrants(g))
	if err == nil && slices.Contains(dirs, root) {
		t.Fatalf("derived home ancestor %s, exposing %s recursively without an explicit grant", root, filepath.Join(home, ".config"))
	}
}

func TestDerivedGrantsExplicitRootGrant(t *testing.T) {
	home := stableHome(t)
	wd := t.TempDir()
	interp := filepath.Join(home, "custom-runtime", "interp")
	review446Script(t, interp)
	p := sandboxprofile.DefaultProfile()
	p.Network.Mode = sandboxprofile.ModeOpen
	base, err := ResolveGrants(p, wd, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{filepath.Join(home, ".config", "private", "tool"), filepath.Join(wd, "tool")} {
		review446Script(t, tool)
		if filepath.Dir(tool) == wd {
			if err := os.WriteFile(tool, []byte("#!"+interp+"\n"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		g := base.withUnrestrictedFilesystem()
		if _, err := resolveInnerBinaryDirs([]string{tool}, derivedGrantPolicyForGrants(g)); err != nil {
			t.Errorf("explicit '/' (also learn mode) already grants every directory and must not refuse %s: %v", tool, err)
		}
	}
}

func TestDerivedGrantsSensitiveCaseAlias(t *testing.T) {
	home := stableHome(t)
	tool := filepath.Join(home, ".config", "private", "tool")
	review446Script(t, tool)
	alias := filepath.Join(home, ".CONFIG", "private", "tool")
	if _, err := os.Stat(alias); os.IsNotExist(err) {
		t.Skip("filesystem is case-sensitive")
	} else if err != nil {
		t.Fatal(err)
	}
	canonical, err := filepath.EvalSymlinks(alias)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("case alias resolves as %s", canonical)
	g := minimalDerivedGrants(t.TempDir(), home)
	if _, err := resolveInnerBinaryDirs([]string{tool}, derivedGrantPolicyForGrants(g)); err == nil {
		t.Fatal("control: ordinary lowercase .config must be refused")
	}
	if dirs, err := resolveInnerBinaryDirs([]string{alias}, derivedGrantPolicyForGrants(g)); err == nil {
		t.Fatalf("case alias of sensitive .config accepted: %v", dirs)
	}
}

func TestDerivedGrantsCompatibilityControls(t *testing.T) {
	home, wd := stableHome(t), t.TempDir()
	node := filepath.Join(home, ".nvm", "versions", "node", "v24", "bin", "node")
	review446Script(t, node)
	t.Setenv("PATH", filepath.Dir(node)+":/usr/bin:/bin")
	nvmTool := filepath.Join(home, "bin", "harness")
	review446Script(t, nvmTool)
	if err := os.WriteFile(nvmTool, []byte("#!/usr/bin/env node\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	pyenvTool := filepath.Join(home, ".pyenv", "versions", "3.14", "bin", "python")
	review446Script(t, pyenvTool)
	for _, d := range []string{"lib", "conf"} {
		if err := os.MkdirAll(filepath.Join(filepath.Dir(filepath.Dir(pyenvTool)), d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	brew := writeHomebrewChain(t, filepath.Join(home, "homebrew"))
	workTool := filepath.Join(wd, "tool")
	review446Script(t, workTool)
	sensitiveTool := filepath.Join(home, ".config", "private", "tool")
	review446Script(t, sensitiveTool)
	p := sandboxprofile.DefaultProfile()
	p.Network.Mode = sandboxprofile.ModeOpen
	g, err := ResolveGrants(p, wd, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		argv []string
	}{
		{"nvm shebang", []string{nvmTool}},
		{"env wrapper", []string{"env", "FOO=bar", nvmTool}},
		{"pyenv prefix", []string{pyenvTool}},
		{"homebrew chain", []string{brew}},
		{"writable workdir system shell", []string{workTool}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := resolveInnerBinaryDirs(tc.argv, derivedGrantPolicyForGrants(g)); err != nil {
				t.Fatal(err)
			}
			if runtime.GOOS == "darwin" {
				if _, err := BuildChildArgv(g, tc.argv); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
	t.Run("explicit sensitive dir grant", func(t *testing.T) {
		g2 := *g
		g2.ReadPaths = append(append([]string{}, g.ReadPaths...), filepath.Dir(sensitiveTool))
		if _, err := resolveInnerBinaryDirs([]string{sensitiveTool}, derivedGrantPolicyForGrants(&g2)); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("writable interpreter refused", func(t *testing.T) {
		interp := filepath.Join(wd, "interp")
		review446Script(t, interp)
		if err := os.WriteFile(nvmTool, []byte("#!"+interp+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := resolveInnerBinaryDirs([]string{nvmTool}, derivedGrantPolicyForGrants(g)); err == nil {
			t.Fatal("writable interpreter must not derive ungranted harness dir")
		}
	})
}

func TestDerivedGrantsRejectUnfinishedSupportResolution(t *testing.T) {
	home, wd := stableHome(t), t.TempDir()
	tool := filepath.Join(home, "runtime", "bin", "tool")
	review446Script(t, tool)
	prefix := filepath.Dir(filepath.Dir(tool))
	// A cycle used to be hidden by prefixSupportDirs' existence probe.
	if err := os.Symlink("lib", filepath.Join(prefix, "lib")); err != nil {
		t.Fatal(err)
	}
	g := minimalDerivedGrants(wd, home)
	g.ReadPaths = []string{"/bin", "/usr"}
	if _, err := resolveInnerBinaryDirs([]string{tool}, derivedGrantPolicyForGrants(g)); err == nil {
		t.Fatal("must fail closed when a support directory cannot be resolved")
	}
}

func TestDerivedGrantsBoundAncestorSymlinkHops(t *testing.T) {
	home := stableHome(t)
	root := filepath.Join(home, "runtime")
	review446Script(t, filepath.Join(root, "real", "tool"))
	// Each component has a short chain, but the complete lookup exceeds
	// the resolver's bound. An endpoint-only fallback must not admit it.
	for i := 0; i < 34; i++ {
		name := filepath.Join(root, fmt.Sprintf("link%d", i))
		target := "real"
		if i < 33 {
			target = fmt.Sprintf("link%d", i+1)
		}
		if err := os.Symlink(target, name); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	tool := filepath.Join(root, "bin", "tool")
	review446Script(t, tool)
	if err := os.Symlink("link0", filepath.Join(root, "lib")); err != nil {
		t.Fatal(err)
	}
	g := minimalDerivedGrants(t.TempDir(), home)
	g.ReadPaths = []string{"/bin", "/usr"}
	if _, err := resolveInnerBinaryDirs([]string{tool}, derivedGrantPolicyForGrants(g)); err == nil {
		t.Fatal("must not derive grants after exceeding the support symlink hop bound")
	}
}

func TestDerivedGrantsExplicitRootReadAndWrite(t *testing.T) {
	home := stableHome(t)
	tool := filepath.Join(home, "runtime", "bin", "tool")
	review446Script(t, tool)
	g := minimalDerivedGrants(t.TempDir(), home)
	g.ReadPaths = []string{"/"}
	if _, err := resolveInnerBinaryDirs([]string{tool}, derivedGrantPolicyForGrants(g)); err != nil {
		t.Fatalf("explicit root read already covers all targets: %v", err)
	}
	// Without readable coverage, root writability must taint even a
	// command outside the workdir or temp roots.
	pol := derivedGrantPolicy{writable: []string{"/"}}
	if _, err := resolveInnerBinaryDirs([]string{tool}, pol); err == nil {
		t.Fatal("root writability must taint the entire command resolution")
	}
}

func TestDerivedGrantsSensitiveHomeCaseAlias(t *testing.T) {
	home := stableHome(t)
	tool := filepath.Join(home, ".config", "private", "tool")
	review446Script(t, tool)
	aliasHome := filepath.Join(filepath.Dir(home), strings.ToUpper(filepath.Base(home)))
	fi, err := os.Stat(aliasHome)
	if os.IsNotExist(err) {
		t.Skip("filesystem is case-sensitive")
	}
	if err != nil {
		t.Fatal(err)
	}
	hi, err := os.Stat(home)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(fi, hi) {
		t.Fatal("fixture home alias does not identify the same directory")
	}
	g := minimalDerivedGrants(t.TempDir(), home)
	if _, err := resolveInnerBinaryDirs([]string{filepath.Join(aliasHome, ".config", "private", "tool")}, derivedGrantPolicyForGrants(g)); err == nil {
		t.Fatal("case alias of HOME must not hide its sensitive descendants")
	}
}

func TestDerivedGrantsPreserveCaseDistinctHomeDirs(t *testing.T) {
	home := stableHome(t)
	review446Script(t, filepath.Join(home, ".config", "private", "tool"))
	alias := filepath.Join(home, ".CONFIG", "private", "tool")
	if _, err := os.Stat(alias); err == nil {
		t.Skip("filesystem is case-insensitive")
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
	review446Script(t, alias)
	g := minimalDerivedGrants(t.TempDir(), home)
	g.ReadPaths = []string{"/bin", "/usr"}
	if _, err := resolveInnerBinaryDirs([]string{alias}, derivedGrantPolicyForGrants(g)); err != nil {
		t.Fatalf("case-distinct nonsensitive directory must remain derivable: %v", err)
	}
}

func TestDerivedGrantsLoweringKeepsValidatedTargets(t *testing.T) {
	home, wd := stableHome(t), t.TempDir()
	tool := filepath.Join(home, "runtime", "tool")
	review446Script(t, tool)
	secret := filepath.Join(home, ".config", "private")
	review446Script(t, filepath.Join(secret, "tool"))
	g := minimalDerivedGrants(wd, home)
	g.ReadPaths = []string{"/bin", "/usr"}
	dirs, err := resolveInnerBinaryDirs([]string{tool}, derivedGrantPolicyForGrants(g))
	if err != nil {
		t.Fatal(err)
	}
	g.derivedReadPaths = dirs
	runtimeDir := filepath.Dir(tool)
	needle := "(allow file-read* (subpath " + sbplQuote(runtimeDir) + "))"
	if !strings.Contains(GenerateSBPL(g), needle) {
		t.Fatal("control: admitted runtime must get a read rule")
	}
	// The agent cannot replace this untainted directory. This stronger
	// phase check nevertheless ensures lowering emits the admitted target
	// literally rather than interpreting a later host-side alias change.
	if err := os.Rename(runtimeDir, runtimeDir+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, runtimeDir); err != nil {
		t.Fatal(err)
	}
	profile := GenerateSBPL(g)
	if !strings.Contains(profile, needle) {
		t.Fatal("validated target disappeared during lowering")
	}
	if strings.Contains(profile, "(subpath "+sbplQuote(secret)+")") {
		t.Fatal("lowering followed a validated target into an unchecked directory")
	}
	argv, err := BuildBwrapArgv(g, []string{"/bin/sh"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(argv, " "), "--ro-bind "+runtimeDir+" "+runtimeDir) {
		t.Fatal("Linux lowering must keep the admitted read-only target")
	}
	if slices.Contains(argv, secret) {
		t.Fatal("Linux lowering expanded the admitted target")
	}
}

func TestDerivedGrantsSensitiveTreeTargetAncestor(t *testing.T) {
	root := review446Root(t)
	home := filepath.Join(root, "home")
	if err := os.Mkdir(home, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	tool := filepath.Join(root, "runtime", "tool")
	review446Script(t, tool)
	secret := filepath.Join(root, "runtime", "private")
	review446Script(t, filepath.Join(secret, "tool"))
	if err := os.Symlink(secret, filepath.Join(home, ".config")); err != nil {
		t.Fatal(err)
	}
	g := minimalDerivedGrants(t.TempDir(), home)
	g.ReadPaths = []string{"/bin", "/usr"}
	if _, err := resolveInnerBinaryDirs([]string{tool}, derivedGrantPolicyForGrants(g)); err == nil {
		t.Fatal("a recursive grant must not contain a sensitive tree's external target")
	}
}

func TestDerivedGrantsExplicitWritePathKeepsCommandReadable(t *testing.T) {
	home := stableHome(t)
	tool := filepath.Join(home, "writable-runtime", "bin", "tool")
	review446Script(t, tool)
	g := minimalDerivedGrants(t.TempDir(), home)
	g.ReadPaths = []string{"/bin", "/usr"}
	g.WritePaths = []string{filepath.Dir(filepath.Dir(tool))}
	dirs, err := resolveInnerBinaryDirs([]string{tool}, derivedGrantPolicyForGrants(g))
	if err != nil {
		t.Fatal(err)
	}
	g.derivedReadPaths = dirs
	if !strings.Contains(GenerateSBPL(g), "(allow file-read* (subpath "+sbplQuote(filepath.Dir(tool))+")") {
		t.Fatal("a write-only grant bounds derivation but does not already grant macOS read access")
	}
	secret := filepath.Join(home, ".config", "private")
	review446Script(t, filepath.Join(secret, "tool"))
	if err := os.Rename(filepath.Dir(tool), filepath.Dir(tool)+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Dir(tool)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(GenerateSBPL(g), "(subpath "+sbplQuote(secret)+")") {
		t.Fatal("macOS re-followed a mutable write-only path after admission")
	}
	argv, err := BuildBwrapArgv(g, []string{"/bin/sh"})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(argv, filepath.Dir(tool)) || slices.Contains(argv, secret) {
		t.Fatal("Linux added a redundant bind that can follow the replaced write-only descendant")
	}
}
