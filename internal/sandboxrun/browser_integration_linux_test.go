//go:build linux

package sandboxrun

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

// findChromiumForTest locates a Chromium/Chrome binary for hermetic sandbox
// browser tests. Prefers a Playwright cache install (a real ELF binary)
// over PATH names, because distro "chromium" is often a snap wrapper that
// cannot run inside bwrap. Returns "" when nothing usable is found.
//
// The result is always symlink-resolved: callers grant filepath.Dir() of it,
// and a distro package typically leaves only a /usr/bin symlink pointing at
// the real tree elsewhere (Google's .deb: /usr/bin/google-chrome-stable ->
// /opt/google/chrome/...). Granting the symlink's own dir would grant /usr,
// which the baseline already covers, while the target tree stays unmounted
// and the symlink dangles inside the sandbox.
func findChromiumForTest() string {
	home, err := os.UserHomeDir()
	if err == nil {
		matches, _ := filepath.Glob(filepath.Join(home, ".cache", "ms-playwright", "chromium-*", "chrome-linux", "chrome"))
		if len(matches) > 0 {
			best := matches[0]
			for _, m := range matches[1:] {
				if m > best {
					best = m
				}
			}
			if real, rerr := filepath.EvalSymlinks(best); rerr == nil {
				return real
			}
			return best
		}
	}
	for _, name := range []string{"google-chrome-stable", "google-chrome", "chromium", "chromium-browser"} {
		p, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		real, err := filepath.EvalSymlinks(p)
		if err != nil {
			continue
		}
		// Snap wrappers fail inside bwrap ("timeout waiting for snap system
		// profiles"); skip them so the test skips cleanly instead of failing.
		// Checked after resolution too: on Ubuntu /usr/bin/chromium is a
		// symlink into /snap/bin.
		if strings.Contains(p, "/snap/") || strings.Contains(real, "/snap/") {
			continue
		}
		return real
	}
	return ""
}

// TestIntegrationBrowserHeadlessLocalPage runs a real Chromium inside
// bwrap+Landlock against a host-side httptest server. It proves the
// browser grant set (binary read + open_port) is sufficient for
// a headless local-page fetch — no internet, no npm, no Playwright runner.
//
// Skips when no Chromium binary is available (CI default runners do not
// install one). Does NOT use skipOrFailCI: a missing browser is expected
// outside dedicated jobs.
func TestIntegrationBrowserHeadlessLocalPage(t *testing.T) {
	requireBwrap(t)
	if !LandlockNetSupported() {
		t.Skipf("Landlock ABI %d < 4", LandlockABI())
	}
	chrome := findChromiumForTest()
	if chrome == "" {
		t.Skip("chromium not installed (PATH or ~/.cache/ms-playwright)")
	}
	chromeDir := filepath.Dir(chrome)

	const marker = "omac-browser-probe-ok"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, "<html><body>%s</body></html>", marker)
	}))
	defer srv.Close()
	port := srv.Listener.Addr().(*net.TCPAddr).Port
	url := fmt.Sprintf("http://127.0.0.1:%d/", port)

	// Build omac with the real host env before any HOME override.
	omac := filepath.Join(t.TempDir(), "omac")
	build := exec.Command("go", "build", "-o", omac, "github.com/TNG/oh-my-agentic-coder/cmd/omac")
	build.Env = os.Environ()
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build omac: %v\n%s", err, out)
	}

	wd := t.TempDir()
	userData := filepath.Join(wd, "chrome-profile")
	if err := os.MkdirAll(userData, 0o755); err != nil {
		t.Fatal(err)
	}

	p := sandboxprofile.DefaultProfile()
	p.Network.OpenPort = []int{port}
	p.Filesystem.Read = append(p.Filesystem.Read, chromeDir)
	g, err := ResolveGrants(p, wd, nil)
	if err != nil {
		t.Fatal(err)
	}
	g.ReadPaths = append(g.ReadPaths, filepath.Dir(omac))

	inner := []string{
		chrome,
		"--headless=new",
		"--disable-gpu",
		"--no-first-run",
		"--disable-component-update",
		"--disable-background-networking",
		"--user-data-dir=" + userData,
		"--dump-dom",
		url,
	}
	stage2 := append([]string{omac, "sandbox", "stage2"}, Stage2Args(g)...)
	argvTail := append(append([]string{}, stage2...), append([]string{"--"}, inner...)...)
	argv, err := BuildBwrapArgv(g, argvTail)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("chromium inside sandbox failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), marker) {
		t.Errorf("marker %q not found in chromium output:\n%s", marker, out)
	}
}
