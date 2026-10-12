//go:build darwin

package sandboxrun

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestIntegrationNameRuleHidesSecretFiles runs real sandbox-exec with a
// name rule on the workdir, as launch adds for a folder too large to
// scan: secret files at any depth (also inside node_modules, and ones
// created during the session) are hidden, other files are not.
func TestIntegrationNameRuleHidesSecretFiles(t *testing.T) {
	g := testGrants(t)
	g.NameDenyRoots = []string{g.Workdir}
	g.NameDenyGlobs = []string{".env", ".envrc"}
	write := func(rel string) {
		t.Helper()
		p := filepath.Join(g.Workdir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("SECRET\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, rel := range []string{".env", "a/b/.env", "a/.env/inner", "node_modules/pkg/.envrc", ".env.example", "x.env", "plain.txt"} {
		write(rel)
	}
	read := func(rel string) bool {
		_, code := runSandboxed(t, g, "/bin/cat", filepath.Join(g.Workdir, rel))
		return code == 0
	}
	// .ENV opens .env on a case-insensitive disk (the macOS default).
	for _, rel := range []string{".env", ".ENV", "a/b/.env", "a/.env/inner", "node_modules/pkg/.envrc"} {
		if read(rel) {
			t.Errorf("%s must be hidden", rel)
		}
	}
	for _, rel := range []string{".env.example", "x.env", "plain.txt"} {
		if !read(rel) {
			t.Errorf("%s must stay readable", rel)
		}
	}
	dir := filepath.Join(g.Workdir, "new")
	out, _ := runSandboxed(t, g, "/bin/sh", "-c",
		"mkdir -p "+dir+" && echo x > "+dir+"/ok.txt && echo ok-written; echo x > "+dir+"/.env && echo env-written")
	if !strings.Contains(out, "ok-written") || strings.Contains(out, "env-written") {
		t.Errorf("in a new folder, ok.txt must be writable and .env not, got %q", out)
	}
}
