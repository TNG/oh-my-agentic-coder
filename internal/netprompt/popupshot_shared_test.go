package netprompt

import (
	"os"
	"path/filepath"
	"testing"
)

// Representative network prompt for the screenshot tests. Every optional line
// is present (Origin + Likely cause + Agent intent), and the cause is long
// enough to expose wrapping, truncation, and the fixed dialog height — the
// things promptText's golden string tests cannot see. Kept in one place so the
// per-platform render tests (popupshot_linux_test.go / _darwin_test.go) agree
// on what is drawn.
const (
	shotHost   = "raw.githubusercontent.com"
	shotPort   = 443
	shotTitle  = "omac: network access"
	shotIntent = "" // renders "(not declared)"
	shotCause  = "Syntax-highlighting grammar/query (tree-sitter) for the code you are viewing"
	shotOrigin = "opencode"
)

// shotPath returns where a backend's PNG should be written: OMAC_POPUP_SHOT_DIR
// when set (CI uploads it as an artifact), else a per-test temp dir.
func shotPath(t *testing.T, backend string) string {
	t.Helper()
	dir := os.Getenv("OMAC_POPUP_SHOT_DIR")
	if dir == "" {
		dir = t.TempDir()
	}
	return filepath.Join(dir, "network-prompt-"+backend+".png")
}

// assertShot fails when the capture produced no usable file.
func assertShot(t *testing.T, path string) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("screenshot not written: %v", err)
	}
	if fi.Size() == 0 {
		t.Fatalf("screenshot is empty: %s", path)
	}
	t.Logf("wrote popup screenshot: %s (%d bytes)", path, fi.Size())
}
