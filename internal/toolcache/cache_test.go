package toolcache

import (
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDescribePersistentCanonicalAliasesShareIdentity(t *testing.T) {
	for _, test := range []struct {
		name      string
		aliasPath func(t *testing.T, worktree string) string
	}{
		{
			name: "direct symlink",
			aliasPath: func(t *testing.T, worktree string) string {
				t.Helper()
				alias := filepath.Join(t.TempDir(), "worktree-alias")
				if err := os.Symlink(worktree, alias); err != nil {
					t.Fatal(err)
				}
				return alias
			},
		},
		{
			name: "symlink with cleanable path",
			aliasPath: func(t *testing.T, worktree string) string {
				t.Helper()
				alias := filepath.Join(t.TempDir(), "worktree-alias")
				if err := os.Symlink(worktree, alias); err != nil {
					t.Fatal(err)
				}
				return alias + string(os.PathSeparator) + "."
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			worktree := t.TempDir()
			canonical, err := DescribePersistent(DomainWorkdir, worktree)
			if err != nil {
				t.Fatal(err)
			}
			aliased, err := DescribePersistent(DomainWorkdir, test.aliasPath(t, worktree))
			if err != nil {
				t.Fatal(err)
			}

			if canonical.CanonicalPath != aliased.CanonicalPath {
				t.Errorf("canonical paths differ: %q != %q", canonical.CanonicalPath, aliased.CanonicalPath)
			}
			if canonical.Identity != aliased.Identity {
				t.Errorf("identities differ: %q != %q", canonical.Identity, aliased.Identity)
			}
			if canonical.Digest != aliased.Digest {
				t.Errorf("digests differ: %q != %q", canonical.Digest, aliased.Digest)
			}
		})
	}
}

func TestDescribePersistentMainAndLinkedWorktreesDiffer(t *testing.T) {
	for _, test := range []struct {
		name   string
		domain Domain
	}{
		{name: "workdir domain", domain: DomainWorkdir},
		{name: "serve domain", domain: DomainServe},
	} {
		t.Run(test.name, func(t *testing.T) {
			mainDir := t.TempDir()
			git(t, mainDir, "init", "-q")
			if err := os.WriteFile(filepath.Join(mainDir, "tracked.txt"), []byte("tracked\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			git(t, mainDir, "add", "tracked.txt")
			git(t, mainDir, "-c", "user.name=Omac", "-c", "user.email=omac@example.invalid", "commit", "-qm", "initial")

			linkedDir := filepath.Join(t.TempDir(), "linked")
			git(t, mainDir, "worktree", "add", "-q", linkedDir, "-b", "linked-cache-test")

			main, err := DescribePersistent(test.domain, mainDir)
			if err != nil {
				t.Fatal(err)
			}
			linked, err := DescribePersistent(test.domain, linkedDir)
			if err != nil {
				t.Fatal(err)
			}

			if main.CanonicalPath == linked.CanonicalPath {
				t.Errorf("canonical paths match: %q", main.CanonicalPath)
			}
			if main.Identity == linked.Identity {
				t.Errorf("identities match: %q", main.Identity)
			}
		})
	}
}

func TestDescribePersistentDomainsDiffer(t *testing.T) {
	path := t.TempDir()
	for _, test := range []struct {
		name         string
		firstDomain  Domain
		secondDomain Domain
	}{
		{name: "workdir then serve", firstDomain: DomainWorkdir, secondDomain: DomainServe},
		{name: "serve then workdir", firstDomain: DomainServe, secondDomain: DomainWorkdir},
	} {
		t.Run(test.name, func(t *testing.T) {
			first, err := DescribePersistent(test.firstDomain, path)
			if err != nil {
				t.Fatal(err)
			}
			second, err := DescribePersistent(test.secondDomain, path)
			if err != nil {
				t.Fatal(err)
			}

			if first.Identity == second.Identity {
				t.Errorf("identities match: %q", first.Identity)
			}
			if first.Digest == second.Digest {
				t.Errorf("digests match: %q", first.Digest)
			}
		})
	}
}

func TestDescribePersistentUsesFullSHA256(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	for _, test := range []struct {
		name   string
		domain Domain
	}{
		{name: "workdir domain", domain: DomainWorkdir},
		{name: "serve domain", domain: DomainServe},
	} {
		t.Run(test.name, func(t *testing.T) {
			scope, err := DescribePersistent(test.domain, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}

			wantDigest := sha256.Sum256([]byte(scope.Identity))
			if scope.Digest != hex.EncodeToString(wantDigest[:]) {
				t.Errorf("digest = %q, want full SHA-256 digest", scope.Digest)
			}
			if len(scope.Digest) != 64 {
				t.Errorf("digest length = %d, want 64", len(scope.Digest))
			}
			leaf := filepath.Base(scope.Dir)
			if leaf != scope.Digest {
				t.Errorf("cache leaf = %q, want digest %q", leaf, scope.Digest)
			}
			if len(leaf) != 64 {
				t.Errorf("cache leaf length = %d, want 64", len(leaf))
			}
			if _, err := hex.DecodeString(leaf); err != nil {
				t.Errorf("cache leaf = %q, want hexadecimal: %v", leaf, err)
			}
			if parent := filepath.Dir(scope.Dir); parent != filepath.Join(home, ".cache", "omac") {
				t.Errorf("cache parent = %q, want %q", parent, filepath.Join(home, ".cache", "omac"))
			}
		})
	}
}

func TestEnvironment(t *testing.T) {
	for _, test := range []struct {
		name string
		mode Mode
	}{
		{name: "persistent", mode: ModePersistent},
		{name: "ephemeral", mode: ModeEphemeral},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "cache")
			want := map[string]string{
				"XDG_CACHE_HOME":     filepath.Join(dir, "xdg"),
				"GOCACHE":            filepath.Join(dir, "go-build"),
				"GOMODCACHE":         filepath.Join(dir, "go-mod"),
				"NPM_CONFIG_CACHE":   filepath.Join(dir, "npm"),
				"PIP_CACHE_DIR":      filepath.Join(dir, "pip"),
				"CARGO_HOME":         filepath.Join(dir, "cargo"),
				"OMAC_CACHE_DIR":     dir,
				"OMAC_XDG_CACHE_DIR": dir,
				"OMAC_CACHE_MODE":    string(test.mode),
			}

			if got := Environment(dir, test.mode); !maps.Equal(got, want) {
				t.Errorf("Environment() = %#v, want %#v", got, want)
			}
		})
	}
}

// TestEnvironmentSplitSeparatesXDGFromBuildCaches asserts that the harness
// XDG cache and the tool build caches resolve under their own scope
// directories, so a harness's plugins persist across workdirs while build
// artifacts stay isolated per workdir.
func TestEnvironmentSplitSeparatesXDGFromBuildCaches(t *testing.T) {
	buildDir := filepath.Join(t.TempDir(), "build")
	xdgDir := filepath.Join(t.TempDir(), "xdg-scope")
	env := EnvironmentSplit(buildDir, xdgDir, ModePersistent)
	if got, want := env["XDG_CACHE_HOME"], filepath.Join(xdgDir, "xdg"); got != want {
		t.Errorf("XDG_CACHE_HOME = %q; want %q", got, want)
	}
	if got, want := env["OMAC_XDG_CACHE_DIR"], xdgDir; got != want {
		t.Errorf("OMAC_XDG_CACHE_DIR = %q; want %q", got, want)
	}
	for _, name := range []string{"GOCACHE", "GOMODCACHE", "NPM_CONFIG_CACHE", "PIP_CACHE_DIR", "CARGO_HOME"} {
		if !strings.HasPrefix(env[name], buildDir) {
			t.Errorf("%s = %q; want it under build scope %q", name, env[name], buildDir)
		}
	}
	if env["OMAC_CACHE_DIR"] != buildDir {
		t.Errorf("OMAC_CACHE_DIR = %q; want %q", env["OMAC_CACHE_DIR"], buildDir)
	}
}

// TestDescribeHarnessIsWorkdirIndependent asserts a harness scope is keyed by
// name only (workdir-independent) and distinct from a workdir scope.
func TestDescribeHarnessIsWorkdirIndependent(t *testing.T) {
	a, _ := DescribeHarness("opencode")
	if a.Domain != DomainHarness {
		t.Errorf("domain = %q, want %q", a.Domain, DomainHarness)
	}
	other, _ := DescribeHarness("claude-code")
	wd, _ := DescribePersistent(DomainWorkdir, t.TempDir())
	if a.Digest == other.Digest || a.Digest == wd.Digest {
		t.Errorf("harness digest %q collides with another harness/workdir scope", a.Digest)
	}
	if _, err := DescribeHarness(""); err == nil {
		t.Error("DescribeHarness accepted an empty harness name")
	}
}

func TestPreparePersistentPermissionsAndSafety(t *testing.T) {
	for _, test := range []struct {
		name    string
		prepare func(t *testing.T, root, leaf string)
	}{
		{
			name: "new directories",
			prepare: func(t *testing.T, root, leaf string) {
				t.Helper()
			},
		},
		{
			name: "pre-existing permissive directories",
			prepare: func(t *testing.T, root, leaf string) {
				t.Helper()
				if err := os.MkdirAll(leaf, 0o777); err != nil {
					t.Fatal(err)
				}
				for _, path := range []string{root, leaf} {
					if err := os.Chmod(path, 0o777); err != nil {
						t.Fatal(err)
					}
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			worktree := t.TempDir()
			described, err := DescribePersistent(DomainWorkdir, worktree)
			if err != nil {
				t.Fatal(err)
			}
			root := filepath.Join(home, ".cache", "omac")
			test.prepare(t, root, described.Dir)

			scope, err := PreparePersistent(DomainWorkdir, worktree)
			if err != nil {
				t.Fatal(err)
			}
			if err := scope.Close(); err != nil {
				t.Fatal(err)
			}

			for _, path := range []string{root, scope.Dir} {
				assertPrivateDir(t, path)
			}
		})
	}

	for _, test := range []struct {
		name     string
		location string
		kind     string
	}{
		{name: "persistent root symlink", location: "root", kind: "symlink"},
		{name: "persistent root regular file", location: "root", kind: "file"},
		{name: "scope leaf symlink", location: "leaf", kind: "symlink"},
		{name: "scope leaf regular file", location: "leaf", kind: "file"},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			worktree := t.TempDir()
			unsafe, err := DescribePersistent(DomainWorkdir, worktree)
			if err != nil {
				t.Fatal(err)
			}
			root := filepath.Join(home, ".cache", "omac")
			unsafePath := root
			if test.location == "leaf" {
				unsafePath = unsafe.Dir
			}
			if err := os.MkdirAll(filepath.Dir(unsafePath), 0o700); err != nil {
				t.Fatal(err)
			}

			var target string
			switch test.kind {
			case "symlink":
				target = t.TempDir()
				if err := os.Chmod(target, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, unsafePath); err != nil {
					t.Fatal(err)
				}
			case "file":
				if err := os.WriteFile(unsafePath, []byte("unsafe"), 0o600); err != nil {
					t.Fatal(err)
				}
			default:
				t.Fatalf("unknown unsafe path kind %q", test.kind)
			}

			if _, err := PreparePersistent(DomainWorkdir, worktree); err == nil {
				t.Fatal("PreparePersistent() succeeded for unsafe cache path")
			}
			if target != "" {
				info, err := os.Stat(target)
				if err != nil {
					t.Fatal(err)
				}
				if got := info.Mode().Perm(); got != 0o755 {
					t.Errorf("symlink target permissions = %#o, want 0755", got)
				}
			}
		})
	}
}

func TestPrepareEphemeralBypassesPersistentState(t *testing.T) {
	for _, test := range []struct {
		name    string
		prepare func(t *testing.T, dir string)
	}{
		{
			name: "new cache",
			prepare: func(t *testing.T, dir string) {
				t.Helper()
			},
		},
		{
			name: "pre-existing permissive cache",
			prepare: func(t *testing.T, dir string) {
				t.Helper()
				if err := os.Mkdir(dir, 0o777); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(dir, 0o777); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			sandboxTmp := t.TempDir()
			dir := filepath.Join(sandboxTmp, "cache")
			test.prepare(t, dir)

			scope, err := PrepareEphemeral(sandboxTmp)
			if err != nil {
				t.Fatal(err)
			}
			if err := scope.Close(); err != nil {
				t.Fatal(err)
			}

			if scope.Mode != ModeEphemeral {
				t.Errorf("mode = %q, want %q", scope.Mode, ModeEphemeral)
			}
			if scope.Dir != dir {
				t.Errorf("dir = %q, want %q", scope.Dir, dir)
			}
			assertPrivateDir(t, scope.Dir)
			if _, err := os.Stat(filepath.Join(home, ".cache", "omac")); !os.IsNotExist(err) {
				t.Errorf("persistent cache root exists or stat failed: %v", err)
			}
		})
	}
}

func assertPrivateDir(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Errorf("%s permissions = %#o, want 0700", path, got)
	}
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
}
