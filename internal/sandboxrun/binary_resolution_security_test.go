//go:build vuln

package sandboxrun

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestSecurityBinaryResolutionNeverGrantsRootOrHome asserts that resolving a
// script's shebang interpreter can never widen a grant to "/" or the user's
// home directory.
//
// A shebang line of exactly "#!/" parses to an interpreter of "/". LookPath
// on it fails (not a bare name, not executable), but the absolute-path
// fallback in resolveInterpreterDirs then calls os.Stat("/") — which
// succeeds — and returns filepath.Dir("/") == "/" as a grant.
func TestSecurityBinaryResolutionNeverGrantsRootOrHome(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "payload")
	if err := os.WriteFile(script, []byte("#!/\necho hi\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Control: a script with a real, resolvable interpreter grants that
	// interpreter's directory, so a fix that suppresses every shebang
	// grant would not make this test pass for the wrong reason.
	realInterp := filepath.Join(dir, "bin", "runner")
	if err := os.MkdirAll(filepath.Dir(realInterp), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(realInterp, []byte("\x7fELF"), 0o755); err != nil {
		t.Fatal(err)
	}
	normal := filepath.Join(dir, "normal")
	if err := os.WriteFile(normal, []byte("#!"+realInterp+"\necho hi\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	got := resolveCommandBinaryDirs([]string{normal})
	if !slices.Contains(got, filepath.Dir(realInterp)) {
		t.Fatalf("control: a resolvable shebang interpreter's directory was not granted (%v): the fixture is broken, not the security property", got)
	}

	got = resolveCommandBinaryDirs([]string{script})
	if slices.Contains(got, "/") {
		t.Errorf("resolveCommandBinaryDirs(%q) granted \"/\": a shebang line of \"#!/\" parses to interpreter \"/\", and the absolute-path LookPath fallback stats it successfully and grants its directory", script)
	}
	home, err := os.UserHomeDir()
	if err == nil && home != "" && slices.Contains(got, home) {
		t.Errorf("resolveCommandBinaryDirs(%q) granted the user's home directory %q", script, home)
	}
}
