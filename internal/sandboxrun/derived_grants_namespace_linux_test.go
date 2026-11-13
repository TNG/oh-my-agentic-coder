//go:build linux

package sandboxrun

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// This is a real bwrap gate, not a namespace-model assertion. Keep it enabled
// on Linux CI; compilation on another OS is not evidence that these execs run.
func TestIntegrationDerivedGrantNamespaceExecution(t *testing.T) {
	if err := CheckPlatform(); err != nil {
		t.Skipf("live Linux namespace gate needs functional bwrap: %v", err)
	}
	for _, kind := range []string{"read", "write", "allow", "env", "shebang"} {
		t.Run(kind, func(t *testing.T) {
			home := stableHome(t)
			realRoot := filepath.Join(home, "runtime")
			realTool := filepath.Join(realRoot, "bin", "tool")
			review446Script(t, realTool)
			if err := os.WriteFile(realTool, []byte("#!/bin/sh\nprintf 'namespace-ok\\n'\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			alias := filepath.Join(home, "runtime-current")
			if err := os.Symlink(realRoot, alias); err != nil {
				t.Fatal(err)
			}
			tool := filepath.Join(alias, "bin", "tool")
			g := minimalDerivedGrants(t.TempDir(), home)
			for _, p := range []string{"/usr", "/bin", "/lib", "/lib64", "/etc"} {
				if _, err := os.Stat(p); err == nil {
					g.ReadPaths = append(g.ReadPaths, p)
				}
			}
			inner := []string{tool}
			required := tool
			switch kind {
			case "read":
				g.ReadPaths = append(g.ReadPaths, alias)
			case "write":
				g.WritePaths = []string{alias}
			case "allow":
				g.AllowPaths = append(g.AllowPaths, alias)
			case "env":
				inner = []string{"/usr/bin/env", "FOO=bar", tool}
			case "shebang":
				outer := filepath.Join(home, "tools", "outer")
				review446Script(t, outer)
				if err := os.WriteFile(outer, []byte("#!"+tool+"\n"), 0o755); err != nil {
					t.Fatal(err)
				}
				inner = []string{outer}
			}
			// Exercise production backend wiring as well as the shared lowerer.
			backendArgv, err := BuildChildArgv(g, inner)
			if err != nil {
				t.Fatal(err)
			}
			mounts, links := namespaceFromBwrap(backendArgv)
			if _, ok := sandboxResolveWithLinks(resolveInnerBinaryPath(inner), mounts, links); !ok {
				t.Fatal("production backend's canonical executable is not visible")
			}
			if _, ok := sandboxResolveWithLinks(required, mounts, links); !ok {
				t.Fatal("production backend omitted a required execution pathname")
			}
			planBinaryNamespace(t, g, inner)
			stage := append([]string(nil), inner...)
			stage[0] = resolveInnerBinaryPath(inner)
			argv, err := BuildBwrapArgv(g, stage)
			if err != nil {
				t.Fatal(err)
			}
			out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput()
			if err != nil || string(out) != "namespace-ok\n" {
				t.Fatalf("real %s exec: err=%v output=%q", kind, err, out)
			}
		})
	}
}

// Keep these separate from the original five cases: each exercises a distinct
// checked-plan invariant that was previously lost during Linux lowering.
func TestIntegrationDerivedGrantNamespaceExecutionAdditional(t *testing.T) {
	if err := CheckPlatform(); err != nil {
		t.Skipf("live Linux namespace gate needs functional bwrap: %v", err)
	}
	for _, kind := range []string{"symlink-target-env", "symlink-target-shebang", "root-read", "root-write", "root-allow"} {
		t.Run(kind, func(t *testing.T) {
			home := stableHome(t)
			realRoot := filepath.Join(home, "versions", "v1")
			realTool := filepath.Join(realRoot, "bin", "tool")
			review446Script(t, realTool)
			if err := os.WriteFile(realTool, []byte("#!/bin/sh\nprintf 'namespace-ok\\n'\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			g := minimalDerivedGrants(t.TempDir(), home)
			for _, p := range []string{"/usr", "/bin", "/lib", "/lib64", "/etc"} {
				if _, err := os.Stat(p); err == nil {
					g.ReadPaths = append(g.ReadPaths, p)
				}
			}
			inner := []string{realTool}
			switch kind {
			case "symlink-target-env", "symlink-target-shebang":
				current := filepath.Join(home, "current")
				if err := os.Symlink("versions/v1", current); err != nil {
					t.Fatal(err)
				}
				shim := filepath.Join(home, "shims", "tool")
				if err := os.MkdirAll(filepath.Dir(shim), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("../current/bin/tool", shim); err != nil {
					t.Fatal(err)
				}
				inner = []string{"/usr/bin/env", "FOO=bar", shim}
				if kind == "symlink-target-shebang" {
					outer := filepath.Join(home, "tools", "outer")
					review446Script(t, outer)
					if err := os.WriteFile(outer, []byte("#!"+shim+"\n"), 0o755); err != nil {
						t.Fatal(err)
					}
					inner = []string{outer}
				}
			case "root-read":
				g.ReadPaths = append(g.ReadPaths, "/")
			case "root-write":
				g.WritePaths = []string{"/"}
			case "root-allow":
				g.AllowPaths = append(g.AllowPaths, "/")
			}
			if _, err := BuildChildArgv(g, inner); err != nil {
				t.Fatalf("production backend %s plan: %v", kind, err)
			}
			planBinaryNamespace(t, g, inner)
			stage := append([]string(nil), inner...)
			stage[0] = resolveInnerBinaryPath(inner)
			argv, err := BuildBwrapArgv(g, stage)
			if err != nil {
				t.Fatal(err)
			}
			out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput()
			if err != nil || string(out) != "namespace-ok\n" {
				t.Fatalf("real %s exec: err=%v output=%q", kind, err, out)
			}
		})
	}
}
