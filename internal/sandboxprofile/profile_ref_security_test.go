package sandboxprofile

import (
	"os"
	"path/filepath"
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
