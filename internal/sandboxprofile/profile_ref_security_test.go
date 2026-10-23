package sandboxprofile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSecurityProfileRefOutsideTrustedDirRefused asserts that a --profile
// reference cannot point Resolve at an arbitrary file.
//
// Resolve treats any ref containing a path separator (or ending in .json)
// as a file path and loads it directly, with no check that the path
// resolves inside ~/.config/omac/sandbox-profiles/. A launcher config
// template built from the workdir (see internal/config's launcher-trust
// tests) can supply such a ref, so a project can point its own launch at a
// policy file it controls — including one granting "/" — via
// "--profile <workdir-relative path>".
func TestSecurityProfileRefOutsideTrustedDirRefused(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	hostile := filepath.Join(t.TempDir(), "evil.json")
	if err := os.WriteFile(hostile, []byte(`{"filesystem":{"allow":["/"]}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Control: a profile ref that names a profile in the trusted directory
	// still resolves, so a blanket "any ref with a slash errors" fix isn't
	// what's being asked for — only refs escaping the trusted directory.
	trustedDir, err := ProfilePath("control")
	if err != nil {
		t.Fatalf("ProfilePath: %v", err)
	}
	if err := WriteProfile(trustedDir, DefaultProfile()); err != nil {
		t.Fatalf("WriteProfile: %v", err)
	}
	if _, _, err := Resolve("control"); err != nil {
		t.Fatalf("control: a named profile in the trusted directory failed to resolve (%v): the fixture is broken, not the security property", err)
	}

	p, _, err := Resolve(hostile)
	if err != nil {
		// Refusing outright also satisfies the property.
		return
	}
	if len(p.Filesystem.Allow) > 0 && p.Filesystem.Allow[0] == "/" {
		t.Errorf("Resolve(%q) loaded a profile granting \"/\" from outside ~/.config/omac/sandbox-profiles/: a workdir-supplied --profile path is treated the same as a profile the user placed in the trusted directory", hostile)
	}
}

// TestSecurityProjectDirConfinesExplicitPaths asserts that WithProjectDir
// admits a project-committed profile without admitting arbitrary host files:
// the launch path passes the project's .omac directory, not WithAnyPath.
func TestSecurityProjectDirConfinesExplicitPaths(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	localDir := filepath.Join(t.TempDir(), ".omac")

	inside := filepath.Join(localDir, "sandbox.json")
	if err := os.MkdirAll(localDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inside, []byte(`{"meta":{"name":"inside"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Resolve(inside, WithProjectDir(localDir)); err != nil {
		t.Fatalf("a project-committed profile inside .omac was refused: %v", err)
	}

	// A sibling under the project root but outside .omac must be refused,
	// matching what the launch path passes.
	sibling := filepath.Join(filepath.Dir(localDir), "sandbox.json")
	if err := os.WriteFile(sibling, []byte(`{"meta":{"name":"sibling"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Resolve(sibling, WithProjectDir(localDir)); err == nil {
		t.Errorf("Resolve(%q) with WithProjectDir(%q) loaded a profile outside .omac", sibling, localDir)
	}

	outside := filepath.Join(t.TempDir(), "evil.json")
	if err := os.WriteFile(outside, []byte(`{"filesystem":{"allow":["/"]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Resolve(outside, WithProjectDir(localDir)); err == nil {
		t.Errorf("Resolve(%q) with WithProjectDir(%q) loaded a profile outside .omac", outside, localDir)
	}
}

// TestSecurityProfileValidateRejectsBlanketGrants asserts that Validate
// refuses a filesystem.allow entry naming the filesystem root (or the
// user's home directory), rather than accepting every profile that
// parses.
func TestSecurityProfileValidateRejectsBlanketGrants(t *testing.T) {
	// Control: a normal, scoped grant validates.
	scoped := DefaultProfile()
	scoped.Filesystem.Allow = []string{"~/.cache/some-tool"}
	if err := scoped.Validate(); err != nil {
		t.Fatalf("control: a scoped filesystem.allow entry was rejected (%v): the fixture is broken, not the security property", err)
	}

	blanket := DefaultProfile()
	blanket.Filesystem.Allow = []string{"/"}
	if err := blanket.Validate(); err == nil {
		t.Errorf("Validate() accepted filesystem.allow: [\"/\"]: any profile reachable via --profile can grant the whole filesystem read-write and nothing refuses it")
	}
}

// TestSecurityLearnedGrantsDoNotWidenOnReexpansion asserts that a learned
// filesystem.allow entry containing a $VAR reference cannot expand to a
// broader grant than the one the operator actually approved.
//
// ExpandPath("~/$ZZZ") with ZZZ unset joins home with "$ZZZ", os.Expand
// substitutes the empty string, and filepath.Abs then cleans the resulting
// trailing slash — so the entry silently expands to the bare home directory
// on the very next launch, even though ErrEmptyExpansion exists precisely
// to catch an all-empty expansion.
func TestSecurityLearnedGrantsDoNotWidenOnReexpansion(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("OMAC_SECURITY_TEST_UNSET_VAR", "")
	os.Unsetenv("OMAC_SECURITY_TEST_UNSET_VAR")

	// Control: a genuinely empty expansion (no home prefix involved) is
	// still caught, so the ErrEmptyExpansion path itself isn't what broke.
	if _, err := ExpandPath("$OMAC_SECURITY_TEST_UNSET_VAR"); err == nil {
		t.Fatalf("control: a bare unset-variable expansion was not rejected: the fixture is broken, not the security property")
	}

	got, err := ExpandPath("~/$OMAC_SECURITY_TEST_UNSET_VAR")
	if err == nil && got == filepath.Clean(home) {
		t.Errorf("ExpandPath(\"~/$VAR\") with an unset VAR expanded to the bare home directory %q instead of erroring: a learned grant scoped to a subdirectory silently widens to the whole home directory on the next launch", got)
	}
}

// TestSecurityConfigDirIsProtected asserts that omac's own config directory
// (~/.config/omac, holding approvals.json and sandbox-profiles/) is on the
// protected-path list every baseline enforces.
//
// The skill-approval trust model assumes this directory is unreachable from
// inside the sandbox: an agent that can write to it can forge its own
// approvals. Today nothing puts it on the list.
func TestSecurityConfigDirIsProtected(t *testing.T) {
	for name, b := range map[string]Baseline{
		"linux":  linuxBaseline(),
		"darwin": darwinBaseline(),
	} {
		t.Run(name, func(t *testing.T) {
			found := false
			for _, p := range b.ProtectedPaths {
				if p == "~/.config/omac" || p == "$XDG_CONFIG_HOME/omac" {
					found = true
				}
			}
			if !found {
				t.Errorf("%s baseline's ProtectedPaths does not include ~/.config/omac: the skill-approval store it holds is reachable and forgeable from inside the sandbox", name)
			}
		})
	}
}

// A symlinked profile file must not load: the symlink entry itself is
// replaceable inside an agent-writable directory, so it could point the
// launch somewhere else between approval and enforcement. This is the one
// leaf check Resolve does itself, so every entry point (the launcher
// config, the CLI, a direct `omac sandbox run`) shares it.
func TestSecurityResolveRefusesSymlinkedProfile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	localDir := filepath.Join(t.TempDir(), ".omac")
	if err := os.MkdirAll(localDir, 0o755); err != nil {
		t.Fatal(err)
	}
	realProfile := filepath.Join(t.TempDir(), "real.json")
	if err := os.WriteFile(realProfile, []byte(`{"meta":{"name":"real"}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("path-form ref", func(t *testing.T) {
		link := filepath.Join(localDir, "p.json")
		if err := os.Symlink(realProfile, link); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if _, _, err := Resolve(link, WithProjectDir(localDir)); err == nil {
			t.Error("Resolve loaded a symlinked profile; the symlink could be re-pointed without changing what was approved")
		}
	})

	t.Run("named ref", func(t *testing.T) {
		dir, err := ProfileDir()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		named := filepath.Join(dir, "linked.json")
		if err := os.Symlink(realProfile, named); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if _, _, err := Resolve("linked"); err == nil {
			t.Error("Resolve loaded a symlinked named profile")
		}
	})
}

// WithinDir is purely lexical: it treats a symlinked parent as inside. This
// pins the contract the containment callers rely on (they EvalSymlinks
// themselves where that matters).
func TestWithinDirLexical(t *testing.T) {
	dir := filepath.Join("/work", ".omac")
	if !WithinDir(dir, filepath.Join(dir, "p.json")) || !WithinDir(dir, dir) {
		t.Error("a path at or under dir must be inside")
	}
	if WithinDir(dir, filepath.Join("/work", ".omacX", "p.json")) {
		t.Error("a sibling sharing the prefix must not be inside")
	}
	if WithinDir("", filepath.Join(dir, "p.json")) {
		t.Error("an empty dir contains nothing")
	}
}

// CheckProfileFile is the shared leaf check: symlink refusal, existence, and
// regular-file enforcement.
func TestCheckProfileFile(t *testing.T) {
	dir := t.TempDir()
	regular := filepath.Join(dir, "p.json")
	if err := os.WriteFile(regular, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CheckProfileFile(regular, "profile"); err != nil {
		t.Fatalf("regular file refused: %v", err)
	}
	link := filepath.Join(dir, "linked.json")
	if err := os.Symlink(regular, link); err == nil {
		if err := CheckProfileFile(link, "profile"); err == nil {
			t.Error("a symlinked profile must be refused")
		}
	}
	if err := CheckProfileFile(filepath.Join(dir, "missing.json"), "profile"); err == nil {
		t.Error("a missing profile must be refused")
	}
	if err := CheckProfileFile(dir, "profile"); err == nil || !strings.Contains("x"+err.Error(), "directory") {
		t.Errorf("a directory must be refused as a profile file, got %v", err)
	}
}

// The launch path hands the child a symlink-resolved profile selection
// (config.ExplicitProfileSelection returns EvalSymlinks' output). On macOS
// the big top component of the temp/home trees it points into rewrites the
// spelling (/var -> /private/var), so the child's containment must be judged
// on the resolved form: a workdir reachable through a symlinked component
// stays a legal location, while a path that only lexically looks like it is
// inside (a symlinked intermediate directory pointing outside) is still
// refused.
func TestProjectDirAdmitsResolvedSpelling(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	realRoot := filepath.Join(t.TempDir(), "real-root")
	local := filepath.Join(realRoot, ".omac")
	if err := os.MkdirAll(local, 0o755); err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(local, "p.json")
	if err := os.WriteFile(profile, []byte(`{"meta":{"name":"p"}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// The same workdir, spelled through a symlinked ancestor — the macOS
	// /var -> /private/var shape.
	aliasRoot := filepath.Join(t.TempDir(), "alias-root")
	if err := os.Symlink(realRoot, aliasRoot); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	aliasOmac := filepath.Join(aliasRoot, ".omac")

	// The resolved spelling the parent returns must load.
	if _, _, err := Resolve(profile, WithProjectDir(aliasOmac)); err != nil {
		t.Errorf("resolved form of a project profile was refused: %v", err)
	}
	// ...and so must the lexical alias spelling it also validates.
	if _, _, err := Resolve(aliasOmac+"/p.json", WithProjectDir(aliasOmac)); err != nil {
		t.Errorf("lexical alias spelling was refused: %v", err)
	}

	// A path that lexically sits inside .omac but realpaths OUTSIDE (a
	// symlinked intermediate directory) remains refused — this is the rule
	// the selection layer enforces and the child must agree.
	tamperRoot := filepath.Join(t.TempDir(), "team-profiles")
	if err := os.MkdirAll(tamperRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tamperRoot, "evil.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	escape := filepath.Join(aliasRoot, ".omac", "escape")
	if err := os.Symlink(tamperRoot, escape); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, _, err := Resolve(escape+"/evil.json", WithProjectDir(aliasOmac)); err == nil {
		t.Error("a lexical .omac path that resolves outside .omac must be refused")
	}
}
